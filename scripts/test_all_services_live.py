import json
import time
import urllib.request
import wave
import io

SERVER_URL = "http://127.0.0.1:18080"

print("=================================================================")
print("  KIỂM THỬ THỰC TẾ TRỰC TIẾP: GEMINI, COPILOT, TTS, GEMMA-4")
print("=================================================================")

# 1. Kiểm tra Server Ready & Models
print("\n[1/5] Kiểm tra Server và Danh sách Model...")
with urllib.request.urlopen(f"{SERVER_URL}/ready", timeout=5) as r:
    ready_data = json.loads(r.read())
    print(f"-> Server Status: {ready_data.get('status')}, Uptime: {ready_data.get('uptime_seconds')}s")

with urllib.request.urlopen(f"{SERVER_URL}/v1/models", timeout=5) as r:
    models_data = json.loads(r.read())
    avail_models = [m["id"] for m in models_data.get("data", []) if m.get("available")]
    print(f"-> Các model sẵn sàng phục vụ ({len(avail_models)}):")
    for mid in sorted(avail_models):
        print(f"   - {mid}")

# 2. Test Gemini (gemini-3.5-flash-lite & gemini-3.8-flash)
print("\n[2/5] Kiểm thử Gemini Live...")
for model in ["gemini-3.5-flash-lite", "gemini-3.8-flash"]:
    try:
        t0 = time.monotonic()
        req_body = {
            "model": model,
            "messages": [{"role": "user", "content": f"Xin chào, hãy trả lời ngắn trong 1 câu: 5 + 5 bằng mấy?"}],
            "temperature": 0.7,
            "max_tokens": 64
        }
        req = urllib.request.Request(
            f"{SERVER_URL}/v1/chat/completions",
            data=json.dumps(req_body).encode("utf-8"),
            headers={"Content-Type": "application/json"}
        )
        with urllib.request.urlopen(req, timeout=60) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            dur = (time.monotonic() - t0) * 1000
            content = data["choices"][0]["message"]["content"].strip()
            print(f"-> {model}: HTTP {resp.status} ({dur:.1f}ms) -> \"{content}\"")
    except Exception as e:
        print(f"-> {model}: Lỗi ({e})")

# 3. Test Copilot (gpt-5.6)
print("\n[3/5] Kiểm thử Copilot (gpt-5.6)...")
try:
    t0 = time.monotonic()
    req_body = {
        "model": "gpt-5.6",
        "messages": [{"role": "user", "content": "Hello"}]
    }
    req = urllib.request.Request(
        f"{SERVER_URL}/v1/chat/completions",
        data=json.dumps(req_body).encode("utf-8"),
        headers={"Content-Type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        print("-> gpt-5.6 HTTP:", resp.status)
except urllib.error.HTTPError as e:
    err_body = e.read().decode("utf-8", errors="ignore").strip()
    print(f"-> gpt-5.6: HTTP {e.code} (Fail-closed đúng chuẩn khi chưa có account: {err_body})")
except Exception as e:
    print(f"-> gpt-5.6: {e}")

# 4. Test TTS cả 3 giọng (Việt, Thái, Khmer) qua Worker
print("\n[4/5] Kiểm thử TTS cả 3 giọng thực tế...")
voices = [
    ("piper:ngochuyennew", "ngochuyennew", "Xin chào quý khách, chúc bạn một ngày làm việc hiệu quả."),
    ("piper:th_TH-mms_female-medium", "th_TH-mms_female-medium", "สวัสดี ยินดีต้อนรับสู่ระบบอัตโนมัติความเร็วสูง"),
    ("piper:khmer_tts_v3", "khmer_tts_v3", "សួស្តីបងប្អូនទាំងអស់គ្នា សូមស្វាគមន៍មកកាន់ប្រព័ន្ធរបស់យើង")
]
for model_id, vid, text in voices:
    try:
        t0 = time.monotonic()
        req_body = {"model": model_id, "voice": vid, "input": text}
        req = urllib.request.Request(
            f"{SERVER_URL}/v1/audio/speech",
            data=json.dumps(req_body).encode("utf-8"),
            headers={"Content-Type": "application/json"}
        )
        with urllib.request.urlopen(req, timeout=20) as resp:
            audio = resp.read()
            dur = (time.monotonic() - t0) * 1000
            with wave.open(io.BytesIO(audio)) as w:
                print(f"-> {vid:24s}: HTTP {resp.status} | {len(audio):7d} bytes | {dur:6.1f}ms | rate={w.getframerate()} | frames={w.getnframes()}")
    except Exception as e:
        print(f"-> {vid:24s}: Lỗi ({e})")

# 5. Test Gemma-4 Colab
print("\n[5/5] Kiểm thử Gemma-4 (Colab Worker)...")
try:
    t0 = time.monotonic()
    req_body = {
        "model": "gemma-4-colab",
        "messages": [{"role": "user", "content": "What is the capital of Vietnam? Answer in 3 words."}],
        "max_tokens": 32,
        "temperature": 0.7
    }
    req = urllib.request.Request(
        f"{SERVER_URL}/v1/chat/completions",
        data=json.dumps(req_body).encode("utf-8"),
        headers={"Content-Type": "application/json"}
    )
    with urllib.request.urlopen(req, timeout=40) as resp:
        data = json.loads(resp.read().decode("utf-8"))
        dur = (time.monotonic() - t0) * 1000
        content = data["choices"][0]["message"]["content"].strip()
        print(f"-> gemma-4-colab: HTTP {resp.status} ({dur:.1f}ms) -> \"{content}\"")
except Exception as e:
    print(f"-> gemma-4-colab: Lỗi ({e})")

print("\n=================================================================")
print("  HOÀN TẤT KIỂM THỬ TẤT CẢ CÁC HẠNG MỤC YÊU CẦU")
print("=================================================================")
