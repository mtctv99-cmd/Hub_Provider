import base64
import concurrent.futures
import io
import json
import os
import subprocess
import sys
import threading
import time
import urllib.request
import wave
from pathlib import Path

def log(msg):
    print(msg, flush=True)

log("=== BẮT ĐẦU CHẠY THẬT VÀ KIỂM CHỨNG TOÀN DIỆN ===")
python_bin = sys.executable
server_port = 18088
server_url = f"http://127.0.0.1:{server_port}"
ws_url = f"ws://127.0.0.1:{server_port}/api/llm/ws/worker"
worker_token = "scale-test-secret-token"

# 1. Khởi động server Go mới trên port 18088
env = os.environ.copy()
env["PORT"] = str(server_port)
env["SERVER_PORT"] = str(server_port)
env["DB_PATH"] = "data/real_e2e_test.db"
env["COLAB_WORKER_KEY"] = worker_token
env["WORKER_AUTH_TOKEN"] = worker_token
env["PIPER_MODELS_DIR"] = "models"
env["PIPER_BIN"] = r"C:\Users\MaiThanhChu\AppData\Local\hermes\hermes-agent\venv\Scripts\piper.EXE"
env["MASTER_KEY"] = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

exe_path = str((Path(__file__).resolve().parent.parent / "sever-provider-new.exe"))
log(f"1. Khởi động {exe_path} trên cổng {server_port}...")
server_proc = subprocess.Popen([exe_path, "-config", "config"], env=env)

w1_proc = None
w2_stop = threading.Event()
w2_thread = None

try:
    # Chờ server ready
    ready = False
    for _ in range(30):
        time.sleep(0.5)
        try:
            with urllib.request.urlopen(f"{server_url}/ready", timeout=2) as r:
                if r.status == 200:
                    ready = True
                    break
        except Exception:
            pass

    if not ready:
        raise RuntimeError(f"Server không khởi động được trên port {server_port}")
    log(f"-> Server đã READY trên cổng {server_port}!")

    # 2. Khởi động Worker #1: Local in-memory TTS (10 concurrency, 3 voices: Việt, Thái, Khmer)
    log("2. Kết nối Worker #1 (Local In-Memory TTS, 10 slot concurrency)...")
    w1_proc = subprocess.Popen([
        python_bin, "scripts/local_tts_worker.py",
        "--server", ws_url,
        "--worker-id", "tts-cluster-node-1",
        "--token", worker_token,
        "--concurrency", "10"
    ])

    # 3. Khởi động Worker #2: Thread WebSocket giả lập Node GPU / Colab ngoài
    log("3. Kết nối Worker #2 (Node 2: model động 'custom-deepseek-colab')...")
    def run_worker_2():
        import websocket
        try:
            ws = websocket.create_connection(f"{ws_url}?token={worker_token}", timeout=10)
            # Dùng cùng WorkerID gốc để kiểm chứng cơ chế tự động gán suffix chống đá nhau
            reg = {
                "worker_id": "tts-cluster-node-1",
                "model": "custom-deepseek-colab",
                "models": ["custom-deepseek-colab", "piper:voice-node2"],
                "capabilities": ["chat", "streaming", "audio_speech"],
                "max_concurrency": 8,
                "token": worker_token
            }
            ws.send(json.dumps(reg))
            ack = json.loads(ws.recv())
            log(f"-> Worker #2 Đăng ký thành công: {ack.get('worker_id')}")

            ws.settimeout(1.0)
            while not w2_stop.is_set():
                try:
                    raw = ws.recv()
                except websocket.WebSocketTimeoutException:
                    continue
                if not raw:
                    break
                msg = json.loads(raw)
                if msg.get("type") == "ping":
                    ws.send(json.dumps({"type": "pong"}))
                elif msg.get("type") == "chat_request":
                    rid = msg["request_id"]
                    # Gửi phản hồi terminal job_result
                    ws.send(json.dumps({
                        "type": "job_result",
                        "request_id": rid,
                        "text": "Xin chào từ Worker #2 (Node GPU Colab bên ngoài)!",
                        "done": True,
                        "usage": {"prompt_tokens": 5, "completion_tokens": 12, "total_tokens": 17}
                    }))
            ws.close()
        except Exception as e:
            log(f"Worker #2 error: {e}")

    w2_thread = threading.Thread(target=run_worker_2, daemon=True)
    w2_thread.start()

    # Chờ 4.5 giây cho Worker nạp model và kết nối
    time.sleep(4.5)

    # 4. Kiểm tra danh sách Worker đang kết nối trên Server
    log("\n4. Kiểm tra danh sách Worker & Model Snapshot từ Server:")
    with urllib.request.urlopen(f"{server_url}/v1/models") as r:
        models_data = json.loads(r.read())
        served = [m["id"] for m in models_data.get("data", [])]
        avail = [m["id"] for m in models_data.get("data", []) if m.get("available")]
        log(f"-> Các model server phục vụ ({len(served)}): {served}")
        log(f"-> Các model sẵn sàng phục vụ ({len(avail)}): {avail}")
        assert "piper:ngochuyennew" in avail, "Thiếu ngochuyennew"
        assert "piper:th_TH-mms_female-medium" in avail, "Thiếu Thái nữ"
        assert "piper:khmer_tts_v3" in avail, "Thiếu Khmer"
        assert "custom-deepseek-colab" in avail, "Worker #2 dynamic model chưa sẵn sàng!"

    # 5. TEST THẬT 1: Tổng hợp giọng Việt ngochuyennew
    log("\n5. TEST THẬT 1: Tổng hợp tiếng Việt (ngochuyennew) qua In-Memory Worker...")
    t0 = time.monotonic()
    req = urllib.request.Request(f"{server_url}/v1/audio/speech",
        data=json.dumps({"model": "piper:ngochuyennew", "input": "Kiểm tra hệ thống giọng nói Việt Nam siêu tốc.", "voice": "ngochuyennew"}).encode("utf-8"),
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        audio = resp.read()
        dt = (time.monotonic() - t0) * 1000
        with wave.open(io.BytesIO(audio)) as w:
            log(f"-> THÀNH CÔNG: HTTP {resp.status}, {len(audio)} bytes, {dt:.1f}ms, sample_rate={w.getframerate()}, frames={w.getnframes()}")
            Path("data/real_run_vietnamese.wav").write_bytes(audio)

    # 6. TEST THẬT 2: Tổng hợp giọng Thái nữ
    log("\n6. TEST THẬT 2: Tổng hợp tiếng Thái (th_TH-mms_female-medium) qua In-Memory Worker...")
    t0 = time.monotonic()
    req = urllib.request.Request(f"{server_url}/v1/audio/speech",
        data=json.dumps({"model": "piper:th_TH-mms_female-medium", "input": "สวัสดี ระบบทำงานได้ดีมาก", "voice": "th_TH-mms_female-medium"}).encode("utf-8"),
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        audio = resp.read()
        dt = (time.monotonic() - t0) * 1000
        with wave.open(io.BytesIO(audio)) as w:
            log(f"-> THÀNH CÔNG: HTTP {resp.status}, {len(audio)} bytes, {dt:.1f}ms, sample_rate={w.getframerate()}, frames={w.getnframes()}")
            Path("data/real_run_thai.wav").write_bytes(audio)

    # 7. TEST THẬT 3: Tổng hợp tiếng Campuchia (khmer_tts_v3)
    log("\n7. TEST THẬT 3: Tổng hợp tiếng Campuchia (khmer_tts_v3) qua In-Memory Worker...")
    t0 = time.monotonic()
    req = urllib.request.Request(f"{server_url}/v1/audio/speech",
        data=json.dumps({"model": "piper:khmer_tts_v3", "input": "សួស្តី នេះជាការសាកល្បង", "voice": "khmer_tts_v3"}).encode("utf-8"),
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        audio = resp.read()
        dt = (time.monotonic() - t0) * 1000
        with wave.open(io.BytesIO(audio)) as w:
            log(f"-> THÀNH CÔNG: HTTP {resp.status}, {len(audio)} bytes, {dt:.1f}ms, sample_rate={w.getframerate()}, frames={w.getnframes()}")
            Path("data/real_run_khmer.wav").write_bytes(audio)

    # 8. TEST THẬT 4: Chat với Model động từ Worker #2 (Dynamic Model Routing)
    log("\n8. TEST THẬT 4: Chat với dynamic model 'custom-deepseek-colab' từ Worker #2...")
    t0 = time.monotonic()
    req = urllib.request.Request(f"{server_url}/v1/chat/completions",
        data=json.dumps({"model": "custom-deepseek-colab", "messages": [{"role": "user", "content": "Alo worker node 2"}]}).encode("utf-8"),
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        res_json = json.loads(resp.read())
        dt = (time.monotonic() - t0) * 1000
        log(f"-> THÀNH CÔNG: HTTP {resp.status}, {dt:.1f}ms, nội dung: {res_json['choices'][0]['message']['content']}")

    # 9. TEST THẬT 5: Bắn 10 request song song để kiểm tra trần 10 slot concurrency của Worker TTS
    log("\n9. TEST THẬT 5: Kiểm tra tải song song 10 request TTS cùng lúc...")
    def send_one(i):
        v = ["ngochuyennew", "th_TH-mms_female-medium", "khmer_tts_v3"][i % 3]
        text = f"Câu thử tải số {i}" if v == "ngochuyennew" else ("สวัสดี" if "th_" in v else "សួស្តី")
        t_start = time.monotonic()
        r = urllib.request.Request(f"{server_url}/v1/audio/speech",
            data=json.dumps({"model": f"piper:{v}", "input": text, "voice": v}).encode("utf-8"),
            headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(r, timeout=15) as re:
            data = re.read()
            return i, v, len(data), (time.monotonic() - t_start) * 1000

    t_all = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=10) as ex:
        futures = [ex.submit(send_one, i) for i in range(10)]
        results = [f.result() for f in concurrent.futures.as_completed(futures)]
    total_time = (time.monotonic() - t_all) * 1000
    log(f"-> 10 request TTS song song hoàn thành trong {total_time:.1f}ms!")
    for i, v, size, dur in sorted(results, key=lambda x: x[0]):
        log(f"   [Job #{i}] {v:24s} -> {size} bytes trong {dur:.1f}ms")

    log("\n=== TẤT CẢ 5 BÀI TEST THẬT ĐỀU ĐẠT 100% ===")

finally:
    log("\nDọn dẹp các tiến trình thử nghiệm...")
    w2_stop.set()
    if w1_proc:
        try: w1_proc.terminate(); w1_proc.wait(timeout=2)
        except Exception: pass
    try: server_proc.terminate(); server_proc.wait(timeout=2)
    except Exception: pass
