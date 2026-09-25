"""Colab 24/7 Rotator Supervisor for Sever_provider.

Manages rolling allocation of Google Colab T4 GPU notebook instances across a pool
of Google accounts, automating the 3-hour lifecycle:
  - 3 hours active run duration (RUN_DURATION = 10800s)
  - Pre-warm next successor worker at 2h 35m (PREWARM_THRESHOLD = 9300s) for zero-downtime transition
  - 16 hours cooldown (COOLDOWN_DURATION = 57600s) per retired account recorded in SQLite
  - Automatic injection of combined Gemma-4 32K context + In-Memory Piper/Khmer TTS worker
  - Out-of-band proxy support via Cloudflare WARP SOCKS5 proxy

Usage:
  python scripts/colab_rotator.py --server-url ws://127.0.0.1:18080/api/llm/ws/worker
"""
import argparse
import asyncio
import base64
import json
import logging
import os
import signal
import sqlite3
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path
from typing import Optional

REPO_ROOT = Path(__file__).resolve().parent.parent
DEFAULT_WORKER_SCRIPT = REPO_ROOT / "colab" / "combined_worker.py"

LOG = logging.getLogger("colab_rotator")

# Constants
RUN_DURATION = 3 * 3600             # 3 hours (10,800s)
PREWARM_THRESHOLD = 155 * 60         # 2h 35m (9,300s)
COOLDOWN_DURATION = 16 * 3600        # 16 hours (57,600s)
CHECK_INTERVAL = 30                 # Check every 30s


class AccountState:
    READY = "READY"
    ACTIVE = "ACTIVE"
    PREWARMING = "PREWARMING"
    COOLDOWN = "COOLDOWN"


class SQLiteCooldownStore:
    def __init__(self, db_path: str):
        self.db_path = db_path
        self._ensure_table()

    def _get_conn(self):
        conn = sqlite3.connect(self.db_path, timeout=10.0)
        conn.execute("PRAGMA journal_mode=WAL")
        return conn

    def _ensure_table(self):
        with self._get_conn() as conn:
            conn.execute("""
                CREATE TABLE IF NOT EXISTS account_cooldowns (
                    identity_id TEXT PRIMARY KEY,
                    state TEXT NOT NULL DEFAULT 'READY',
                    cooldown_until DATETIME,
                    last_used_at DATETIME,
                    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
                )
            """)

    def get_all_cooldowns(self) -> dict[str, dict]:
        results = {}
        with self._get_conn() as conn:
            cursor = conn.execute("SELECT identity_id, state, cooldown_until, last_used_at FROM account_cooldowns")
            for row in cursor.fetchall():
                email, state, cd_str, last_str = row
                cd_val = 0.0
                last_val = 0.0
                if cd_str:
                    try:
                        cd_val = float(cd_str)
                    except ValueError:
                        pass
                if last_str:
                    try:
                        last_val = float(last_str)
                    except ValueError:
                        pass
                results[email] = {
                    "state": state,
                    "cooldown_until": cd_val,
                    "last_used_at": last_val
                }
        return results

    def set_cooldown(self, email: str, cooldown_until: float, last_used_at: float, state: str = "COOLDOWN"):
        with self._get_conn() as conn:
            conn.execute("""
                INSERT INTO account_cooldowns (identity_id, state, cooldown_until, last_used_at, updated_at)
                VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
                ON CONFLICT(identity_id) DO UPDATE SET
                    state = excluded.state,
                    cooldown_until = excluded.cooldown_until,
                    last_used_at = excluded.last_used_at,
                    updated_at = CURRENT_TIMESTAMP
            """, (email, state, str(cooldown_until), str(last_used_at)))


class ColabRotator:
    def __init__(
        self,
        token_dir: Path,
        db_store: SQLiteCooldownStore,
        server_url: str,
        auth_token: str,
        worker_script: Path = DEFAULT_WORKER_SCRIPT,
        run_duration: float = RUN_DURATION,
        prewarm_threshold: float = PREWARM_THRESHOLD,
        cooldown_duration: float = COOLDOWN_DURATION,
    ):
        self.token_dir = token_dir
        self.db = db_store
        self.server_url = server_url
        self.auth_token = auth_token
        self.worker_script = worker_script
        self.run_duration = run_duration
        self.prewarm_threshold = prewarm_threshold
        self.cooldown_duration = cooldown_duration

        self._active_sessions: dict[str, dict] = {}
        self._starting_workers: set[str] = set()
        self._prewarmed_for: set[str] = set()
        self._lock = asyncio.Lock()

    def get_available_accounts(self) -> list[str]:
        if not self.token_dir.is_dir():
            return []
        emails = []
        for f in self.token_dir.iterdir():
            if f.name.startswith("token_") and f.name.endswith(".json") and "lock" not in f.name:
                name = f.name.removeprefix("token_").removesuffix(".json")
                try:
                    decoded = base64.urlsafe_b64decode(name + "==").decode()
                    if "@" in decoded:
                        emails.append(decoded)
                        continue
                except Exception:
                    pass
                if "_at_" in name:
                    local, domain = name.split("_at_", 1)
                    email = local + "@" + domain.replace("_", ".")
                    if "@" in email:
                        emails.append(email)
        return sorted(emails)

    def _email_to_token_path(self, email: str) -> Optional[Path]:
        for f in self.token_dir.iterdir():
            if f.name.startswith("token_") and f.name.endswith(".json"):
                if email.replace("@", "_at_").replace(".", "_") in f.name:
                    return f
                name = f.name.removeprefix("token_").removesuffix(".json")
                try:
                    if base64.urlsafe_b64decode(name + "==").decode() == email:
                        return f
                except Exception:
                    pass
        fallback = self.token_dir / "token.json"
        return fallback if fallback.is_file() else None

    def get_eligible_account(self, exclude_emails: Optional[set[str]] = None) -> Optional[str]:
        exclude = set(exclude_emails or [])
        accounts = self.get_available_accounts()
        if not accounts:
            return None

        cooldowns = self.db.get_all_cooldowns()
        now = time.time()
        candidates = []

        for email in accounts:
            if email in exclude or email in self._active_sessions or email in self._starting_workers:
                continue

            info = cooldowns.get(email, {})
            cooldown_until = info.get("cooldown_until", 0.0)
            last_used_at = info.get("last_used_at", 0.0)

            if now < cooldown_until:
                continue  # Still in cooldown

            candidates.append((last_used_at, email))

        if not candidates:
            return None

        # Sort by Least Recently Used
        candidates.sort(key=lambda x: x[0])
        return candidates[0][1]

    def _check_warp_proxy(self) -> Optional[dict]:
        try:
            r = subprocess.run(["warp-cli", "status"], capture_output=True, text=True, timeout=3)
            if "Connected" in r.stdout:
                LOG.info("WARP proxy detected. Routing Colab requests through socks5://127.0.0.1:40000")
                return {
                    "http": "socks5://127.0.0.1:40000",
                    "https": "socks5://127.0.0.1:40000",
                }
        except Exception:
            pass
        return None

    async def start_colab_worker(self, email: str) -> bool:
        if email in self._active_sessions:
            return True
        if email in self._starting_workers:
            return True
        self._starting_workers.add(email)

        try:
            try:
                from colab_cli.client import Client, Prod, Accelerator, Variant
                from colab_cli.runtime import ColabRuntime
                from google.oauth2.credentials import Credentials
                from google.auth.transport.requests import Request, AuthorizedSession
            except ImportError:
                LOG.error("colab-cli or google-auth not installed in current environment.")
                return False

            token_path = self._email_to_token_path(email)
            if not token_path or not token_path.is_file():
                LOG.error("No token found for %s", email)
                return False

            with token_path.open(encoding="utf-8") as f:
                token_data = json.load(f)

            creds = Credentials.from_authorized_user_info(token_data)
            if not creds.valid:
                if creds.expired and creds.refresh_token:
                    await asyncio.to_thread(creds.refresh, Request())

            warp_proxies = await asyncio.to_thread(self._check_warp_proxy)
            session = AuthorizedSession(creds)
            if warp_proxies:
                session.proxies.update(warp_proxies)

            client = Client(Prod(), session)
            notebook_hash = uuid.uuid4()
            LOG.info("[%s] Requesting T4 GPU session from Google Colab API...", email)

            result = await asyncio.to_thread(
                client.assign,
                notebook_hash,
                variant=Variant.GPU,
                accelerator=Accelerator.T4
            )

            url = result.runtime_proxy_info.url
            token = result.runtime_proxy_info.token
            endpoint = result.endpoint
            LOG.info("[%s] Runtime allocated: %s", email, endpoint)

            runtime = ColabRuntime(url, token)
            worker_id = f"colab-t4-{uuid.uuid4().hex[:6]}"

            with self.worker_script.open("r", encoding="utf-8") as f:
                script_code = f.read()

            injected_code = f"""# === Injected by Sever_provider Rotator ===
import sys
sys.argv = [
    'combined-worker',
    '--server', '{self.server_url}',
    '--token', '{self.auth_token}',
    '--worker-id', '{worker_id}',
    '--context-size', '32768',
    '--flash-attn',
    '--tts-concurrency', '20',
    '--tts-gpu',
]
# ==========================================
""" + script_code

            stop_event = threading.Event()
            ready_event = threading.Event()

            def run_worker():
                try:
                    def output_hook(out):
                        if out.get("output_type") == "stream":
                            text = out.get("text", "").strip()
                            if text:
                                LOG.info("[%s] Colab: %s", email, text[:150])
                                if "connected" in text.lower() or "loaded" in text.lower():
                                    ready_event.set()
                        elif out.get("output_type") == "error":
                            tb = "".join(out.get("traceback", []))
                            LOG.error("[%s] Colab error: %s", email, tb[:200])

                    runtime.execute_code(injected_code, timeout=600, output_hook=output_hook)
                except Exception as exc:
                    if not stop_event.is_set():
                        LOG.error("[%s] Worker execution error: %s", email, exc)
                finally:
                    stop_event.set()
                    self._active_sessions.pop(email, None)
                    self._starting_workers.discard(email)

            def keep_alive():
                fails = 0
                while not stop_event.is_set():
                    if stop_event.wait(120):
                        break
                    try:
                        client.keep_alive_assignment(endpoint)
                        fails = 0
                    except Exception as exc:
                        fails += 1
                        LOG.warning("[%s] Keepalive error (%d/3): %s", email, fails, exc)
                        if fails >= 3:
                            LOG.error("[%s] Keepalive failed 3x; stopping session", email)
                            stop_event.set()
                            break

            worker_thread = threading.Thread(target=run_worker, daemon=True)
            ka_thread = threading.Thread(target=keep_alive, daemon=True)
            worker_thread.start()
            ka_thread.start()

            LOG.info("[%s] Waiting for Colab model to be ready (up to 300s)...", email)
            is_ready = await asyncio.to_thread(ready_event.wait, timeout=300.0)

            if not is_ready:
                LOG.warning("[%s] Colab worker did not report ready within timeout", email)
                stop_event.set()
                return False

            self._active_sessions[email] = {
                "client": client,
                "runtime": runtime,
                "endpoint": endpoint,
                "worker_id": worker_id,
                "stop_event": stop_event,
                "worker_thread": worker_thread,
                "ka_thread": ka_thread,
                "start_time": time.time(),
            }
            LOG.info("[%s] Colab worker successfully online and registered (Worker ID: %s)", email, worker_id)
            return True

        except Exception as exc:
            LOG.error("[%s] Failed to launch worker: %s", email, exc)
            return False
        finally:
            self._starting_workers.discard(email)

    async def stop_colab_worker(self, email: str):
        info = self._active_sessions.pop(email, None)
        if not info:
            return
        LOG.info("[%s] Stopping Colab worker...", email)
        info["stop_event"].set()
        try:
            info["runtime"].stop()
        except Exception:
            pass
        try:
            await asyncio.to_thread(info["client"].unassign, info["endpoint"])
        except Exception:
            pass
        for tname in ("worker_thread", "ka_thread"):
            t = info.get(tname)
            if t and t.is_alive():
                t.join(timeout=5.0)
        LOG.info("[%s] Colab worker stopped.", email)

    async def tick(self):
        async with self._lock:
            now = time.time()

            # 1. Bootstrap if no workers active
            if not self._active_sessions and not self._starting_workers:
                next_acc = self.get_eligible_account()
                if next_acc:
                    LOG.info("[Rotator] No active Colab workers. Bootstrapping with: %s", next_acc)
                    asyncio.create_task(self._launch(next_acc))
                else:
                    LOG.warning("[Rotator] No eligible accounts available (all in cooldown).")
                return

            for email, info in list(self._active_sessions.items()):
                uptime = now - info.get("start_time", now)

                # 2. Pre-warm next successor at 2h 35m
                if uptime >= self.prewarm_threshold and email not in self._prewarmed_for:
                    self._prewarmed_for.add(email)
                    next_acc = self.get_eligible_account(exclude_emails={email})
                    if next_acc:
                        LOG.info(
                            "[Rotator] Account %s uptime %.1fm >= %.1fm threshold. Pre-warming successor: %s",
                            email, uptime / 60, self.prewarm_threshold / 60, next_acc
                        )
                        asyncio.create_task(self._launch(next_acc))
                    else:
                        LOG.warning("[Rotator] Account %s reached pre-warm, but no eligible successor available.", email)

                # 3. Retire at 3h and enter 16h cooldown
                if uptime >= self.run_duration:
                    LOG.info("[Rotator] Account %s reached 3h limit. Retiring and entering 16h cooldown...", email)
                    self._prewarmed_for.discard(email)
                    await self.stop_colab_worker(email)
                    cd_until = now + self.cooldown_duration
                    self.db.set_cooldown(email, cooldown_until=cd_until, last_used_at=now, state="COOLDOWN")

    async def _launch(self, email: str):
        success = await self.start_colab_worker(email)
        now = time.time()
        if success:
            self.db.set_cooldown(email, cooldown_until=0.0, last_used_at=now, state="ACTIVE")
        else:
            # 1h penalty cooldown on allocation failure
            self.db.set_cooldown(email, cooldown_until=now + 3600, last_used_at=now, state="COOLDOWN")

    def status_summary(self) -> list[dict]:
        all_accounts = self.get_available_accounts()
        cooldowns = self.db.get_all_cooldowns()
        now = time.time()
        res = []
        for email in all_accounts:
            sess = self._active_sessions.get(email)
            cd = cooldowns.get(email, {})
            cd_until = cd.get("cooldown_until", 0.0)
            last_used = cd.get("last_used_at", 0.0)

            if sess:
                uptime = now - sess.get("start_time", now)
                res.append({
                    "email": email,
                    "state": AccountState.ACTIVE,
                    "uptime_seconds": round(uptime, 1),
                    "remaining_seconds": max(0.0, round(self.run_duration - uptime, 1)),
                    "worker_id": sess.get("worker_id"),
                })
            elif email in self._starting_workers:
                res.append({
                    "email": email,
                    "state": AccountState.PREWARMING,
                    "uptime_seconds": 0.0,
                    "remaining_seconds": self.run_duration,
                })
            elif now < cd_until:
                rem = round(cd_until - now, 1)
                res.append({
                    "email": email,
                    "state": AccountState.COOLDOWN,
                    "cooldown_remaining_seconds": rem,
                    "cooldown_remaining_hours": round(rem / 3600, 2),
                    "last_used_at": last_used,
                })
            else:
                res.append({
                    "email": email,
                    "state": AccountState.READY,
                    "last_used_at": last_used,
                })
        return res


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server-url", default="ws://127.0.0.1:18080/api/llm/ws/worker")
    parser.add_argument("--token", default=os.environ.get("WORKER_AUTH_TOKEN") or "omni-secret-key-123")
    parser.add_argument("--token-dir", type=Path, default=Path(os.path.expanduser("~/.config/colab-cli")))
    parser.add_argument("--db-path", default="data/local-smoke.db")
    parser.add_argument("--status", action="store_true", help="Print account states and exit")
    parser.add_argument("--interval", type=int, default=CHECK_INTERVAL)
    args = parser.parse_args()

    logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")

    db_store = SQLiteCooldownStore(args.db_path)
    rotator = ColabRotator(
        token_dir=args.token_dir,
        db_store=db_store,
        server_url=args.server_url,
        auth_token=args.token,
    )

    if args.status:
        statuses = rotator.status_summary()
        print(f"Total Google Colab accounts found: {len(statuses)}")
        for s in statuses:
            print(f" - {s['email']}: {s['state']} (Uptime: {s.get('uptime_seconds', 0)}s, Cooldown rem: {s.get('cooldown_remaining_hours', 0)}h)")
        return

    LOG.info("Starting Colab 24/7 Rotator Supervisor (Interval: %ds)...", args.interval)
    stop = asyncio.Event()

    loop = asyncio.get_running_loop()
    for sig in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(sig, stop.set)
        except NotImplementedError:
            pass

    while not stop.is_set():
        try:
            await rotator.tick()
        except Exception as exc:
            LOG.error("Error during rotator tick: %s", exc)

        try:
            await asyncio.wait_for(stop.wait(), timeout=args.interval)
        except asyncio.TimeoutError:
            pass

    LOG.info("Shutting down Colab Rotator...")
    for email in list(rotator._active_sessions.keys()):
        await rotator.stop_colab_worker(email)
    LOG.info("Colab Rotator stopped cleanly.")


if __name__ == "__main__":
    asyncio.run(main())
