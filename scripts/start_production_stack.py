import os
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent

env = os.environ.copy()
env["PORT"] = "18080"
env["SERVER_PORT"] = "18080"
env["DB_PATH"] = "data/local-smoke.db"
env["COLAB_WORKER_KEY"] = "omni-secret-key-123"
env["WORKER_AUTH_TOKEN"] = "omni-secret-key-123"
env["PIPER_MODELS_DIR"] = "models"
env["PIPER_BIN"] = r"C:\Users\MaiThanhChu\AppData\Local\hermes\hermes-agent\venv\Scripts\piper.EXE"
env["MASTER_KEY"] = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

exe_path = str(REPO_ROOT / "sever-provider-new.exe")
python_bin = sys.executable

print(f"1. Khởi động server Go: {exe_path} trên cổng 18080...")
server_proc = subprocess.Popen([exe_path, "-config", "config"], env=env)

# Chờ server ready
ready = False
for _ in range(25):
    time.sleep(0.5)
    try:
        with urllib.request.urlopen("http://127.0.0.1:18080/ready", timeout=2) as r:
            if r.status == 200:
                ready = True
                break
    except Exception:
        pass

if not ready:
    print("LỖI: Server không khởi động được!")
    sys.exit(1)

print("-> Server 18080 đã sẵn sàng!")

print("2. Khởi động In-Memory TTS Worker (20 Concurrency)...")
tts_proc = subprocess.Popen([
    python_bin, "scripts/local_tts_worker.py",
    "--server", "ws://127.0.0.1:18080/api/llm/ws/worker",
    "--worker-id", "tts-worker-cluster",
    "--token", "omni-secret-key-123",
    "--concurrency", "20",
    "--threads-per-voice", "3",
    "--queue-size", "32"
])

print("3. Khởi động Colab T4 GPU Bridge Worker (Gemma-4)...")
colab_proc = subprocess.Popen([
    python_bin, "scripts/colab_bridge_worker.py",
    "--server", "ws://127.0.0.1:18080/api/llm/ws/worker",
    "--worker-id", "colab-t4-live-bridge",
    "--token", "omni-secret-key-123"
])

# Chờ workers kết nối
print("4. Đang kết nối các worker vào Server...")
for _ in range(30):
    time.sleep(0.5)
    try:
        with urllib.request.urlopen("http://127.0.0.1:18080/v1/models") as r:
            data = r.read().decode("utf-8")
            if "piper:khmer_tts_v3" in data and "gemma-4-colab" in data:
                print("-> Toàn bộ Worker TTS (3 giọng) và Colab Gemma-4 đã kết nối thành công!")
                break
    except Exception:
        pass

print(f"\nHệ thống đang chạy ngầm:")
print(f"Server PID: {server_proc.pid}")
print(f"TTS Worker PID: {tts_proc.pid}")
print(f"Colab Bridge PID: {colab_proc.pid}")
print(f"Địa chỉ web UI: http://127.0.0.1:18080/web/#accounts")
