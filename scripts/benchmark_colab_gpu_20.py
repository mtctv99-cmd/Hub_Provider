import concurrent.futures
import io
import json
import statistics
import sys
import threading
import time
import urllib.request
import wave
from pathlib import Path

SERVER_URL = "http://192.168.1.117:18080"
API_KEY = "sk-sp-unlimited-master-key"
ENDPOINT = f"{SERVER_URL}/v1/audio/speech"

prompts = [
    # Tiếng Việt (ngochuyennew) - 7 requests
    ("ngochuyennew", "Xin chào, chào mừng bạn đến với hệ thống trợ lý ảo."),
    ("ngochuyennew", "Đơn hàng của bạn đang được xử lý nhanh chóng trên máy chủ."),
    ("ngochuyennew", "Cảm ơn quý khách đã lựa chọn dịch vụ chuyển văn bản thành giọng nói."),
    ("ngochuyennew", "Hệ thống đang hoạt động ổn định và có hiệu năng xử lý cao."),
    ("ngochuyennew", "Chất lượng âm thanh chuẩn hai mươi hai ki lô héc rất tự nhiên."),
    ("ngochuyennew", "Kiểm tra độ trễ phản hồi của cụm card màn hình chuyên dụng."),
    ("ngochuyennew", "Hoàn tất đo lường tải song song hai mươi luồng âm thanh liên tục."),

    # Tiếng Thái (th_TH-mms_female-medium) - 7 requests
    ("th_TH-mms_female-medium", "สวัสดี ยินดีต้อนรับสู่ระบบสังเคราะห์เสียงอัจฉริยะ"),
    ("th_TH-mms_female-medium", "คำสั่งซื้อของคุณกำลังได้รับการประมวลผลอย่างรวดเร็ว"),
    ("th_TH-mms_female-medium", "ขอบคุณที่เลือกใช้บริการคุณภาพสูงของเราในวันนี้"),
    ("th_TH-mms_female-medium", "ระบบประมวลผลกำลังทำงานได้อย่างมีเสถียรภาพและแม่นยำ"),
    ("th_TH-mms_female-medium", "คุณภาพเสียงระดับมาตรฐานยี่สิบสองกิโลเฮิรตซ์คมชัด"),
    ("th_TH-mms_female-medium", "ทดสอบความเร็วในการตอบสนองของระบบปัญญาประดิษฐ์"),
    ("th_TH-mms_female-medium", "การทดสอบการทำงานพร้อมกันยี่สิบงานเสร็จสมบูรณ์เรียบร้อย"),

    # Tiếng Khmer (khmer_tts_v3) - 6 requests
    ("khmer_tts_v3", "សួស្តី សូមស្វាគមន៍មកកាន់ប្រព័ន្ធបម្លែងសំឡេងឆ្លាតវៃ"),
    ("khmer_tts_v3", "ការបញ្ជាទិញរបស់អ្នកកំពុងត្រូវបានដំណើរការយ៉ាងរហ័ស"),
    ("khmer_tts_v3", "សូមអរគុណសម្រាប់ការជ្រើសរើសប្រើប្រាស់សេវាកម្មរបស់យើង"),
    ("khmer_tts_v3", "ប្រព័ន្ធកំពុងដំណើរការយ៉ាងរលូននិងមានស្ថេរភាពខ្ពស់"),
    ("khmer_tts_v3", "គុណភាពសំឡេងច្បាស់ល្អ គ្មានការរំខានឬដាច់សំឡេង"),
    ("khmer_tts_v3", "ការធ្វើតេស្តល្បឿនឆ្លើយតបសំឡេងនៅលើម៉ាស៊ីនបម្រើ"),
]

assert len(prompts) == 20, f"Expected 20 prompts, got {len(prompts)}"

out_dir = Path("data/benchmark_results")
out_dir.mkdir(parents=True, exist_ok=True)

barrier = threading.Barrier(20)

def send_request(idx, voice, text):
    payload = json.dumps({
        "model": f"piper:{voice}",
        "input": text,
        "voice": voice
    }).encode("utf-8")

    req = urllib.request.Request(
        ENDPOINT,
        data=payload,
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {API_KEY}"
        }
    )

    # Synchronize all threads before firing
    barrier.wait()
    t0 = time.perf_counter()

    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            data = resp.read()
            dur_ms = (time.perf_counter() - t0) * 1000.0

            audio_sec = 0.0
            framerate = 0
            channels = 0
            try:
                with wave.open(io.BytesIO(data)) as w:
                    framerate = w.getframerate()
                    frames = w.getnframes()
                    channels = w.getnchannels()
                    audio_sec = frames / float(framerate)
            except Exception as e:
                pass

            out_file = out_dir / f"req_{idx:02d}_{voice}.wav"
            out_file.write_bytes(data)

            return {
                "idx": idx,
                "voice": voice,
                "status": resp.status,
                "bytes": len(data),
                "audio_sec": round(audio_sec, 2),
                "framerate": framerate,
                "channels": channels,
                "latency_ms": round(dur_ms, 2),
                "error": None
            }
    except Exception as exc:
        dur_ms = (time.perf_counter() - t0) * 1000.0
        return {
            "idx": idx,
            "voice": voice,
            "status": getattr(exc, "code", -1),
            "bytes": 0,
            "audio_sec": 0.0,
            "framerate": 0,
            "channels": 0,
            "latency_ms": round(dur_ms, 2),
            "error": str(exc)
        }

print("=" * 80)
print(f"BẮT ĐẦU BENCHMARK 20 REQUEST SONG SONG (CONCURRENT)")
print(f"Server Target: {ENDPOINT}")
print(f"Số lượng requests: {len(prompts)} (7 VN, 7 Thai, 6 Khmer)")
print("=" * 80)

t_batch_start = time.perf_counter()
with concurrent.futures.ThreadPoolExecutor(max_workers=20) as executor:
    futures = [executor.submit(send_request, i + 1, v, t) for i, (v, t) in enumerate(prompts)]
    results = [f.result() for f in concurrent.futures.as_completed(futures)]
t_batch_total_ms = (time.perf_counter() - t_batch_start) * 1000.0

results.sort(key=lambda r: r["idx"])

print(f"\n{'STT':<4} | {'Ngôn ngữ / Voice':<26} | {'HTTP':<5} | {'Bytes':<8} | {'Audio (s)':<10} | {'Latency (ms)':<12} | {'Trạng thái'}")
print("-" * 88)

success_count = 0
latencies = []
for r in results:
    status_str = "OK" if r["error"] is None and r["status"] == 200 else f"FAIL ({r['error']})"
    if r["status"] == 200:
        success_count += 1
        latencies.append(r["latency_ms"])
    print(f"{r['idx']:<4} | {r['voice']:<26} | {r['status']:<5} | {r['bytes']:<8} | {r['audio_sec']:<10} | {r['latency_ms']:<12.2f} | {status_str}")

min_lat = min(latencies) if latencies else 0.0
max_lat = max(latencies) if latencies else 0.0
avg_lat = statistics.mean(latencies) if latencies else 0.0
p50_lat = statistics.median(latencies) if latencies else 0.0

print("-" * 88)
print(f"Tổng số request:       {len(results)}")
print(f"Thành công:            {success_count}/{len(results)} ({success_count/len(results)*100:.1f}%)")
print(f"Min Latency:           {min_lat:.2f} ms")
print(f"Max Latency:           {max_lat:.2f} ms")
print(f"Avg Latency:           {avg_lat:.2f} ms")
print(f"Median Latency:        {p50_lat:.2f} ms")
print(f"Tổng thời gian batch:  {t_batch_total_ms:.2f} ms")
print("=" * 88)

summary = {
    "total_requests": len(results),
    "successful": success_count,
    "min_latency_ms": round(min_lat, 2),
    "max_latency_ms": round(max_lat, 2),
    "avg_latency_ms": round(avg_lat, 2),
    "median_latency_ms": round(p50_lat, 2),
    "total_batch_time_ms": round(t_batch_total_ms, 2)
}

(out_dir / "benchmark_summary.json").write_text(json.dumps(summary, indent=2), encoding="utf-8")
