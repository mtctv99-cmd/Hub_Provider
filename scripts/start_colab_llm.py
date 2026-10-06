#!/usr/bin/env python3
"""Start real Colab T4 GPU Worker for Gemma-4 and connect to Sever_provider."""

import argparse
import asyncio
import base64
import json
import logging
import os
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path

from google.oauth2.credentials import Credentials
from google.auth.transport.requests import Request, AuthorizedSession
from colab_cli.client import Client, Prod, Accelerator, Variant
from colab_cli.runtime import ColabRuntime

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("colab_llm_launcher")

REPO_ROOT = Path(__file__).resolve().parent.parent
WORKER_SCRIPT = REPO_ROOT / "colab" / "remote_worker.py"
TOKEN_DIR = Path(os.environ.get("COLAB_TOKEN_DIR", Path.home() / ".config" / "colab-cli"))


def check_warp_proxy():
    try:
        r = subprocess.run(["warp-cli", "status"], capture_output=True, text=True, timeout=3)
        if "Connected" in r.stdout:
            logger.info("WARP proxy active, routing Colab requests through socks5://127.0.0.1:40000")
            return {
                "http": "socks5://127.0.0.1:40000",
                "https": "socks5://127.0.0.1:40000",
            }
    except Exception:
        pass
    return None


def get_available_accounts() -> list[str]:
    if not TOKEN_DIR.exists():
        return []
    emails = []
    for f in TOKEN_DIR.glob("token_*.json"):
        if "lock" in f.name:
            continue
        name = f.stem.removeprefix("token_")
        try:
            decoded = base64.urlsafe_b64decode(name + "==").decode()
            if "@" in decoded:
                emails.append(decoded)
                continue
        except Exception:
            pass
        if "_at_" in name:
            local, domain = name.split("_at_", 1)
            emails.append(local + "@" + domain.replace("_", "."))
    return sorted(list(set(emails)))


def get_token_path(email: str) -> Path:
    safe = base64.urlsafe_b64encode(email.encode()).decode().rstrip("=")
    p1 = TOKEN_DIR / f"token_{safe}.json"
    if p1.is_file():
        return p1
    old_safe = email.replace("@", "_at_").replace(".", "_")
    p2 = TOKEN_DIR / f"token_{old_safe}.json"
    if p2.is_file():
        return p2
    return TOKEN_DIR / "token.json"


def get_session(email: str, proxies=None) -> AuthorizedSession:
    p = get_token_path(email)
    if not p.is_file():
        raise FileNotFoundError(f"Token file not found for {email} at {p}")
    with open(p, "r", encoding="utf-8") as f:
        data = json.load(f)
    creds = Credentials.from_authorized_user_info(data)
    if creds.expired and creds.refresh_token:
        creds.refresh(Request())
    sess = AuthorizedSession(creds)
    if proxies:
        sess.proxies.update(proxies)
    return sess


def start_worker(email: str, server_url: str, token: str):
    proxies = check_warp_proxy()
    logger.info("[%s] Authenticating with Colab API...", email)
    session = get_session(email, proxies=proxies)
    client = Client(Prod, session=session)

    # 1. Clean up old assignments to ensure fresh responsive notebook
    assignments = client.list_assignments()
    for a in assignments:
        try:
            logger.info("[%s] Unassigning stale notebook: %s", email, a.endpoint)
            client.unassign(a.endpoint)
        except Exception:
            pass

    logger.info("[%s] Requesting fresh T4 GPU session from Google Colab API...", email)
    active_assignment = client.assign(uuid.uuid4(), variant=Variant.GPU, accelerator=Accelerator.T4)
    logger.info("[%s] Session created: %s", email, active_assignment.endpoint)

    url = active_assignment.runtime_proxy_info.url
    proxy_token = active_assignment.runtime_proxy_info.token
    endpoint = active_assignment.endpoint

    runtime = ColabRuntime(url, proxy_token)
    worker_id = f"colab-t4-{uuid.uuid4().hex[:6]}"

    if not WORKER_SCRIPT.is_file():
        logger.error("Worker script not found at %s", WORKER_SCRIPT)
        sys.exit(1)

    with open(WORKER_SCRIPT, "r", encoding="utf-8") as f:
        script_code = f.read()

    hf_token = os.environ.get("HF_TOKEN", "")
    injected_code = f"""# === Injected by Sever_provider Launcher ===
import os
import sys
HF_TOKEN = {hf_token!r}
if HF_TOKEN:
    os.environ["HF_TOKEN"] = HF_TOKEN
os.environ["HF_HUB_ENABLE_HF_TRANSFER"] = "1"
sys.argv = ['remote_worker',
    '--server-url', '{server_url}',
    '--worker-id', '{worker_id}',
    '--api-key', '{token}',
    '--repo-id', 'unsloth/Qwen3-VL-4B-Instruct-GGUF',
    '--filename', 'Qwen3-VL-4B-Instruct-Q4_K_M.gguf',
    '--mmproj', 'mmproj-F16.gguf',
    '--n-ctx', '32768',
    '--flash-attn']
# ============================================
""" + script_code

    stop_event = threading.Event()
    ready_event = threading.Event()

    def run_worker():
        try:
            def output_hook(out):
                if out.get("output_type") == "stream":
                    text = out.get("text", "").strip()
                    if text:
                        logger.info("[%s] Colab: %s", email, text[:200])
                        if "Connected to Master" in text or "Model loaded successfully" in text:
                            ready_event.set()
                elif out.get("output_type") == "error":
                    tb = "".join(out.get("traceback", []))
                    logger.error("[%s] Colab error: %s", email, tb[:300])

            runtime.execute_code(injected_code, timeout=600, output_hook=output_hook)
        except Exception as e:
            if not stop_event.is_set():
                logger.error("[%s] Worker execution error: %s", email, e)
        finally:
            stop_event.set()

    def keep_alive():
        fails = 0
        while not stop_event.is_set():
            if stop_event.wait(120):
                break
            try:
                client.keep_alive_assignment(endpoint)
                fails = 0
            except Exception as e:
                fails += 1
                logger.warning("[%s] Keep-alive error (%d/3): %s", email, fails, e)
                if fails >= 3:
                    logger.error("[%s] Keep-alive failed 3x, terminating", email)
                    stop_event.set()
                    break

    worker_thread = threading.Thread(target=run_worker, daemon=True)
    ka_thread = threading.Thread(target=keep_alive, daemon=True)
    worker_thread.start()
    ka_thread.start()

    logger.info("[%s] Waiting for Colab worker to signal readiness (downloading model + warming up GPU)...", email)
    while not stop_event.is_set():
        if ready_event.wait(5.0):
            logger.info("🚀 Colab Gemma-4 worker online and registered successfully!")
            break
        logger.info("[%s] Still waiting for Colab model...", email)

    # Keep main thread alive while worker runs
    try:
        while not stop_event.is_set():
            time.sleep(10)
    except KeyboardInterrupt:
        logger.info("Stopping worker on interrupt...")
        stop_event.set()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--email", default=None, help="Google Colab account email (defaults to first available)")
    parser.add_argument("--server-url", default="https://hub.maika.sbs", help="Master server URL (default: https://hub.maika.sbs)")
    parser.add_argument("--token", default="omni-secret-key-123", help="Master worker token")
    args = parser.parse_args()

    accounts = get_available_accounts()
    if not accounts:
        logger.error("No Colab accounts found in %s", TOKEN_DIR)
        sys.exit(1)

    target_email = args.email or accounts[0]
    if target_email not in accounts:
        logger.error("Account %s not in available accounts: %s", target_email, accounts)
        sys.exit(1)

    logger.info("Target Account: %s", target_email)
    logger.info("Target Server: %s", args.server_url)
    start_worker(target_email, args.server_url, args.token)


if __name__ == "__main__":
    main()
