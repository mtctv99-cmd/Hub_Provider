import concurrent.futures
import io
import json
import math
import os
import subprocess
import sys
import time
import urllib.request
import wave
from pathlib import Path

def log(msg):
    print(msg, flush=True)

log("=================================================================")
log("  STRESS TEST THỰC TẾ: BẮN ĐỒNG THỜI 20 REQUEST TTS QUA 3 GIỌNG")
log("=================================================================")

python_bin = sys.executable
server_port = 18090
server_url = f"http://127.0.0.1:{server_port}"
ws_url = f"ws://127.0.0.1:{server_port}/api/llm/ws/worker"
worker_token = "stress-test-secret-token"

out_dir = Path("data/stress_test_20")
out_dir.mkdir(parents=True, exist_ok=True)

# 1. Khởi động server Go mới trên port 18090
env = os.environ.copy()
env["PORT"] = str(server_port)
env["SERVER_PORT"] = str(server_port)
env["DB_PATH"] = "data/stress_test.db"
env["COLAB_WORKER_KEY"] = worker_token
env["WORKER_AUTH_TOKEN"] = worker_token
env["PIPER_MODELS_DIR"] = "models"
env["PIPER_BIN"] = r"C:\Users\MaiThanhChu\AppData\Local\hermes\hermes-agent\venv\Scripts\piper.EXE"
env["MASTER_KEY"] = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

exe_path = str((Path(__file__).resolve().parent.parent / "sever-provider-new.exe"))
log(f"[1/4] Khởi động Server {exe_path} trên cổng {server_port}...")
server_proc = subprocess.Popen([exe_path, "-config", "config"], env=env)

w_proc = None

try:
    ready = False
    for _ in range(25):
        time.sleep(0.4)
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

    # 2. Khởi động Worker TTS 30 slot concurrency
    log(f"[2/4] Khởi động In-Memory TTS Worker (30 Concurrency, 3 threads/voice, Queue 32)...")
    w_proc = subprocess.Popen([
        python_bin, "scripts/local_tts_worker.py",
        "--server", ws_url,
        "--worker-id", "stress-node-20c",
        "--token", worker_token,
        "--concurrency", "30",
        "--threads-per-voice", "3",
        "--queue-size", "32"
    ])

    # Chờ worker nạp 3 model vào RAM và kết nối
    log("-> Chờ Worker nạp xong 3 model vào RAM và kết nối WebSocket...")
    ready_models = False
    for _ in range(30):
        time.sleep(0.5)
        try:
            with urllib.request.urlopen(f"{server_url}/v1/models") as r:
                models_data = json.loads(r.read())
                avail = [m["id"] for m in models_data.get("data", []) if m.get("available")]
                if "piper:khmer_tts_v3" in avail and "piper:ngochuyennew" in avail and "piper:th_TH-mms_female-medium" in avail:
                    log(f"-> Toàn bộ 3 giọng đã sẵn sàng trên WorkerHub: {avail}")
                    ready_models = True
                    break
        except Exception:
            pass

    if not ready_models:
        raise RuntimeError("Worker TTS không kết nối được sau 15 giây")

    # 3. Chuẩn bị 20 câu thoại thực tế dài ngắn khác nhau cho 3 thứ tiếng
    prompts = [
        ("ngochuyennew", "Xin chào, chào mừng bạn đến với hệ thống."),
        ("th_TH-mms_female-medium", "สวัสดี ยินดีต้อนรับสู่ระบบของเรา"),
        ("khmer_tts_v3", "សួស្តី សូមស្វាគមន៍មកកាន់ប្រព័ន្ធរបស់យើង"),
        ("ngochuyennew", "Đơn hàng của bạn đang được xử lý nhanh chóng."),
        ("th_TH-mms_female-medium", "คำสั่งซื้อของคุณกำลังดำเนินการอย่างรวดเร็ว"),
        ("khmer_tts_v3", "ការបញ្ជាទិញរបស់អ្នកកំពុងត្រូវបានដំណើរការ"),
        ("ngochuyennew", "Cảm ơn bạn đã lựa chọn dịch vụ của chúng tôi."),
        ("th_TH-mms_female-medium", "ขอบคุณที่เลือกใช้บริการของเราในวันนี้"),
        ("khmer_tts_v3", "សូមអរគុណសម្រាប់ការប្រើប្រាស់សេវាកម្ម"),
        ("ngochuyennew", "Hệ thống đang hoạt động ổn định và chính xác."),
        ("th_TH-mms_female-medium", "ระบบทำงานได้อย่างมีประสิทธิภาพมาก"),
        ("khmer_tts_v3", "ប្រព័ន្ធដំណើរការបានយ៉ាងរលូន"),
        ("ngochuyennew", "Chất lượng âm thanh chuẩn hai mươi hai ki lô héc."),
        ("th_TH-mms_female-medium", "คุณภาพเสียงระดับมาตรฐานยี่สิบสองกิโลเฮิรตซ์"),
        ("khmer_tts_v3", "គុណភាពសំឡេងច្បាស់ល្អ គ្មានការរំខាន"),
        ("ngochuyennew", "Kiểm tra độ trễ của từng luồng âm thanh."),
        ("th_TH-mms_female-medium", "ทดสอบความเร็วในการตอบสนองของเสียง"),
        ("khmer_tts_v3", "ការធ្វើតេស្តល្បឿនឆ្លើយតបសំឡេង"),
        ("ngochuyennew", "Hoàn tất kiểm tra tải nặng hai mươi tác vụ."),
        ("th_TH-mms_female-medium", "การทดสอบโหลดเสร็จสมบูรณ์เรียบร้อยแล้ว"),
    ]

    log(f"\n[3/4] BẮT ĐẦU BẮN 20 REQUEST CÙNG 1 MILLISECOND (ThreadPoolExecutor max_workers=20)...")

    def send_task(idx, voice, text):
        t0 = time.monotonic()
        req_data = json.dumps({"model": f"piper:{voice}", "input": text, "voice": voice}).encode("utf-8")
        req = urllib.request.Request(f"{server_url}/v1/audio/speech", data=req_data, headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                data = resp.read()
                dur_ms = (time.monotonic() - t0) * 1000.0
                content_type = resp.headers.get("Content-Type", "")
                # Parse WAV file
                with wave.open(io.BytesIO(data)) as w:
                    framerate = w.getframerate()
                    frames = w.getnframes()
                    audio_seconds = frames / float(framerate)
                # Lưu file ra đĩa
                out_path = out_dir / f"req_{idx:02d}_{voice}.wav"
                out_path.write_bytes(data)
                return {
                    "idx": idx,
                    "voice": voice,
                    "status": resp.status,
                    "bytes": len(data),
                    "framerate": framerate,
                    "audio_sec": audio_seconds,
                    "latency_ms": dur_ms,
                    "file": str(out_path),
                    "error": None,
                }
        except Exception as exc:
            dur_ms = (time.monotonic() - t0) * 1000.0
            return {
                "idx": idx,
                "voice": voice,
                "status": -1,
                "bytes": 0,
                "framerate": 0,
                "audio_sec": 0,
                "latency_ms": dur_ms,
                "file": "",
                "error": str(exc),
            }

    batch_start = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=20) as executor:
        futures = [executor.submit(send_task, i, v, t) for i, (v, t) in enumerate(prompts)]
        results = [f.result() for f in concurrent.futures.as_completed(futures)]
    total_batch_time_ms = (time.monotonic() - batch_start) * 1000.0

    # 4. Tổng hợp và phân tích kết quả
    results.sort(key=lambda x: x["idx"])
    log(f"\n[4/4] BÁO CÁO CHI TIẾT 20 REQUEST THỰC TẾ:")
    log("-" * 95)
    log(f"{'STT':<4} | {'Giọng':<24} | {'HTTP':<5} | {'Bytes':<8} | {'Audio Dài':<10} | {'Độ trễ':<10} | {'Trạng thái'}")
    log("-" * 95)

    success_count = 0
    latencies = []
    total_audio_sec = 0.0

    for r in results:
        if r["error"] is None and r["status"] == 200:
            success_count += 1
            latencies.append(r["latency_ms"])
            total_audio_sec += r["audio_sec"]
            status_str = "OK (RIFF/WAVE)"
        else:
            status_str = f"LỖI: {r['error']}"
        log(f"#{r['idx']:02d} | {r['voice']:<24} | {r['status']:<5} | {r['bytes']:<8} | {r['audio_sec']:>6.2f}s    | {r['latency_ms']:>7.1f}ms | {status_str}")

    log("-" * 95)

    # Thống kê phân vị
    latencies.sort()
    n = len(latencies)
    avg_ms = sum(latencies) / n if n > 0 else 0
    min_ms = latencies[0] if n > 0 else 0
    max_ms = latencies[-1] if n > 0 else 0
    p50_ms = latencies[int(n * 0.5)] if n > 0 else 0
    p95_ms = latencies[min(n - 1, int(n * 0.95))] if n > 0 else 0

    log(f"\n====================== THỐNG KÊ TỔNG THỂ ======================")
    log(f"-> Tổng số request bắn cùng lúc:    20")
    log(f"-> Thành công 100%:                  {success_count}/20 request")
    log(f"-> Thất bại / Lỗi / Rớt job:         {20 - success_count}")
    log(f"-> Tổng thời gian gánh cả 20 job:   {total_batch_time_ms:.1f} ms ({total_batch_time_ms/1000.0:.2f} giây)")
    log(f"-> Tổng thời lượng âm thanh sinh ra: {total_audio_sec:.2f} giây audio thật")
    log(f"-> Thông lượng sinh audio:           {total_audio_sec / (total_batch_time_ms/1000.0):.1f}x thời gian thực")
    log(f"-----------------------------------------------------------------")
    log(f"-> Độ trễ Nhanh nhất (Min):          {min_ms:.1f} ms")
    log(f"-> Độ trễ Trung bình (Avg):          {avg_ms:.1f} ms")
    log(f"-> Độ trễ Trung vị (P50):            {p50_ms:.1f} ms")
    log(f"-> Độ trễ Chậm nhất (P95):           {p95_ms:.1f} ms")
    log(f"-> Độ trễ Cao nhất (Max):            {max_ms:.1f} ms")
    log(f"=================================================================")

finally:
    log("\nDọn dẹp các tiến trình thử nghiệm...")
    if w_proc:
        try: w_proc.terminate(); w_proc.wait(timeout=2)
        except Exception: pass
    try: server_proc.terminate(); server_proc.wait(timeout=2)
    except Exception: pass
    # Xóa DB tạm
    for p in Path("data").glob("stress_test.db*"):
        try: p.unlink()
        except Exception: pass
