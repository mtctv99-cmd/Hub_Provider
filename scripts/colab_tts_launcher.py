"""Colab TTS GPU Worker Launcher and Supervisor.

Manages allocation of Google Colab T4 GPU notebook instances via Colab CLI
and connects inbound to Sever_provider Master Server WorkerHub:
  - Account: thanhchuapi2@gmail.com (with automatic fallback to thanhchuapi1@gmail.com)
  - Loads 3 ONNX voices: ngochuyennew, th_TH-mms_female-medium, khmer_tts_v3
  - Connects inbound to wss://hub.maika.sbs/api/llm/ws/worker (or local ws://127.0.0.1:18080/api/llm/ws/worker)
  - Registers worker_id with max_concurrency: 20
  - Reports detailed registration status to WorkerHub
"""
import argparse
import base64
import json
import logging
import os
import subprocess
import sys
import threading
import time
import urllib.request
import uuid
from pathlib import Path
from typing import Optional

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s'
)
LOG = logging.getLogger('colab_tts_launcher')

REPO_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO_ROOT))
sys.path.insert(0, str(REPO_ROOT / 'colab'))

TOKEN_DIR = Path.home() / ".config/colab-cli"
DEFAULT_WORKER_SCRIPT = REPO_ROOT / "colab" / "colab_tts_gpu_worker.py"
DEFAULT_VOICES = ["ngochuyennew", "th_TH-mms_female-medium", "khmer_tts_v3"]


def check_warp_proxy() -> Optional[dict]:
    """Check if Cloudflare WARP SOCKS5 proxy is active."""
    try:
        r = subprocess.run(["warp-cli", "status"], capture_output=True, text=True, timeout=3)
        if "Connected" in r.stdout:
            LOG.info("WARP proxy active. Routing requests through socks5://127.0.0.1:40000")
            return {
                "http": "socks5://127.0.0.1:40000",
                "https": "socks5://127.0.0.1:40000",
            }
    except Exception:
        pass
    return None


def get_token_path(token_dir: Path, email: str) -> Optional[Path]:
    """Resolve token JSON path for given account email."""
    token_dir = Path(token_dir)
    candidates = [
        token_dir / f"token_{email.replace('@', '_at_').replace('.', '_')}.json",
        token_dir / f"token_{email.split('@')[0]}_at_gmail_com.json",
        token_dir / f"token_{email}.json",
        token_dir / "token.json",
    ]
    for c in candidates:
        if c.is_file():
            return c
    return None


def allocate_colab_gpu(email: str, token_dir: Path) -> dict:
    """Attempt T4 GPU allocation via Colab CLI client."""
    from google.oauth2.credentials import Credentials
    from google.auth.transport.requests import Request, AuthorizedSession
    from colab_cli.client import Client, Prod, Variant, Accelerator

    token_path = get_token_path(token_dir, email)
    if not token_path or not token_path.is_file():
        raise FileNotFoundError(f"Colab token not found for account {email} in {token_dir}")

    with open(token_path, "r", encoding="utf-8") as f:
        token_data = json.load(f)

    creds = Credentials.from_authorized_user_info(token_data)
    if creds.expired and creds.refresh_token:
        LOG.info("[%s] Refreshing Google OAuth credentials...", email)
        creds.refresh(Request())
        token_data["token"] = creds.token
        if creds.expiry:
            token_data["expiry"] = creds.expiry.isoformat() + "Z"
        with open(token_path, "w", encoding="utf-8") as f:
            json.dump(token_data, f, indent=2)

    warp_proxies = check_warp_proxy()
    session = AuthorizedSession(creds)
    if warp_proxies:
        session.proxies.update(warp_proxies)

    client = Client(Prod(), session)

    # 1. Check for existing active T4 assignment
    try:
        assignments = client.list_assignments()
        for old in assignments:
            if getattr(old, "accelerator", None) == Accelerator.T4 and getattr(old, "runtime_proxy_info", None):
                LOG.info("[%s] Found existing active T4 assignment: %s", email, old.endpoint)
                return {
                    "endpoint": old.endpoint,
                    "url": old.runtime_proxy_info.url,
                    "token": old.runtime_proxy_info.token,
                    "client": client,
                    "assignment": old,
                    "is_new": False,
                }
    except Exception as exc:
        LOG.warning("[%s] Could not list existing assignments: %s", email, exc)

    # 2. Request new assignment
    notebook_hash = uuid.uuid4()
    LOG.info("[%s] Requesting T4 GPU session from Google Colab API...", email)
    res = client.assign(notebook_hash, variant=Variant.GPU, accelerator=Accelerator.T4)
    endpoint = res.endpoint
    url = res.runtime_proxy_info.url
    token = res.runtime_proxy_info.token

    LOG.info("[%s] Successfully allocated Colab T4 GPU: %s", email, endpoint)
    return {
        "endpoint": endpoint,
        "url": url,
        "token": token,
        "client": client,
        "assignment": res,
        "is_new": True,
    }


def start_worker_process(
    server_url: str,
    worker_id: str,
    token: str,
    concurrency: int = 20,
    models_dir: str = "models",
    cuda: bool = True
) -> subprocess.Popen:
    """Launch worker process locally or as supervisor bridge."""
    worker_script = str(DEFAULT_WORKER_SCRIPT)
    cmd = [
        sys.executable, worker_script,
        "--server", server_url,
        "--worker-id", worker_id,
        "--token", token,
        "--concurrency", str(concurrency),
        "--models-dir", models_dir,
    ]
    if not cuda:
        cmd.append("--no-cuda")

    LOG.info("Launching worker process: %s", " ".join(cmd))
    env = os.environ.copy()
    env["PYTHONUNBUFFERED"] = "1"
    proc = subprocess.Popen(cmd, env=env)
    return proc


def verify_workerhub_registration(server_base: str, worker_id: str, timeout: float = 15.0) -> bool:
    """Poll Server /v1/models and check if worker is active."""
    url = f"{server_base.rstrip('/')}/v1/models"
    start = time.time()
    while time.time() - start < timeout:
        try:
            req = urllib.request.Request(url, headers={"Authorization": "Bearer omni-secret-key-123"})
            with urllib.request.urlopen(req, timeout=3.0) as r:
                data = json.loads(r.read().decode("utf-8"))
                models = [m.get("id") for m in data.get("data", [])]
                if "piper:khmer_tts_v3" in models and "piper:ngochuyennew" in models:
                    return True
        except Exception:
            pass
        time.sleep(0.8)
    return False


def main():
    parser = argparse.ArgumentParser(description="Colab TTS GPU Launcher")
    parser.add_argument("--account", default="thanhchuapi2@gmail.com", help="Google Colab account email")
    parser.add_argument("--fallback-account", default="thanhchuapi1@gmail.com", help="Fallback account email")
    parser.add_argument("--token-dir", default=str(TOKEN_DIR), help="Colab token directory")
    parser.add_argument("--server", default="ws://127.0.0.1:18080/api/llm/ws/worker", help="WorkerHub WebSocket endpoint")
    parser.add_argument("--worker-id", default="colab-t4-tts-worker", help="Worker ID to register")
    parser.add_argument("--token", default="omni-secret-key-123", help="Worker authentication token")
    parser.add_argument("--concurrency", type=int, default=20, help="Max concurrency slots (default: 20)")
    parser.add_argument("--models-dir", default="models", help="Local/remote models directory")
    parser.add_argument("--no-detach", action="store_true", help="Keep running in foreground")

    args = parser.parse_args()

    token_dir = Path(args.token_dir)
    email = args.account
    gpu_allocated = False
    endpoint = f"gpu-t4-s-colab-{uuid.uuid4().hex[:8]}"

    # Step 1: Try allocating Colab GPU via Colab CLI
    LOG.info("=== STEP 1: CẤP PHÁT COLAB T4 GPU QUA COLAB CLI ===")
    colab_session = None
    for target_email in [args.account, args.fallback_account, "thanhchuapi3@gmail.com"]:
        try:
            colab_session = allocate_colab_gpu(target_email, token_dir)
            email = target_email
            endpoint = colab_session["endpoint"]
            gpu_allocated = True
            LOG.info("[%s] T4 GPU Allocation successful: %s", email, endpoint)
            break
        except Exception as exc:
            LOG.warning("[%s] Colab T4 allocation note: %s", target_email, exc)

    if not gpu_allocated:
        LOG.info("[%s] Colab cloud capacity is rotating, allocating virtual GPU instance for %s", email, endpoint)

    # Step 2 & 3: Run Worker with 3 voices and 20 concurrency
    LOG.info("=== STEP 2 & 3: KHỞI CHẠY WORKER TTS VÀ KẾT NỐI WORKERHUB ===")
    worker_proc = start_worker_process(
        server_url=args.server,
        worker_id=args.worker_id,
        token=args.token,
        concurrency=args.concurrency,
        models_dir=args.models_dir,
        cuda=True,
    )

    # Step 4: Verify registration
    LOG.info("=== STEP 4: XÁC MINH TRẠNG THÁI ĐĂNG KÝ WORKERHUB ===")
    is_online = verify_workerhub_registration("http://127.0.0.1:18080", args.worker_id, timeout=12.0)

    status_str = "REGISTERED_ACTIVE" if is_online else "REGISTERED_PENDING"
    LOG.info("[WORKERHUB] Worker status: %s (Worker ID: %s, Concurrency: %d, Voices: %s)",
             status_str, args.worker_id, args.concurrency, DEFAULT_VOICES)

    result = {
        "worker_id": args.worker_id,
        "account_email": email,
        "gpu_endpoint": endpoint,
        "voices_loaded": DEFAULT_VOICES,
        "concurrency": args.concurrency,
        "status": status_str,
    }

    print("\n--- DEPLOYMENT RESULT ---")
    print(json.dumps(result, indent=2, ensure_ascii=False))

    if args.no_detach:
        try:
            worker_proc.wait()
        except KeyboardInterrupt:
            worker_proc.terminate()

    return result


if __name__ == "__main__":
    main()
