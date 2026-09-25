import urllib.request
import json
import time

print("=" * 60)
print("KIỂM THỬ THỰC TẾ 4 DỊCH VỤ CỐT LÕI TRÊN PORT 18080")
print("=" * 60)

# 1. Gemini
print("\n[1/4] Kiểm thử Gemini (gemini-3.8-flash)...")
t0 = time.time()
req_gemini = urllib.request.Request(
    "http://127.0.0.1:18080/v1/chat/completions",
    data=json.dumps({
        "model": "gemini-3.8-flash",
        "messages": [{"role": "user", "content": "1+1=?"}],
        "max_tokens": 50
    }).encode(),
    headers={"Content-Type": "application/json"}
)
try:
    with urllib.request.urlopen(req_gemini, timeout=30) as r:
        res = json.loads(r.read().decode())
        dt = (time.time() - t0) * 1000
        content = res["choices"][0]["message"]["content"]
        print(f" -> Thành công ({dt:.1f}ms): {content.strip()}")
except Exception as e:
    print(f" -> Lỗi: {e}")

# 2. Copilot (gpt-5.6)
print("\n[2/4] Kiểm thử Copilot (gpt-5.6)...")
t0 = time.time()
req_copilot = urllib.request.Request(
    "http://127.0.0.1:18080/v1/chat/completions",
    data=json.dumps({
        "model": "gpt-5.6",
        "messages": [{"role": "user", "content": "Thủ đô của Việt Nam là gì? Trả lời ngắn."}],
        "max_tokens": 50
    }).encode(),
    headers={"Content-Type": "application/json"}
)
try:
    with urllib.request.urlopen(req_copilot, timeout=30) as r:
        res = json.loads(r.read().decode())
        dt = (time.time() - t0) * 1000
        content = res["choices"][0]["message"]["content"]
        print(f" -> Thành công ({dt:.1f}ms): {content.strip()}")
except Exception as e:
    print(f" -> Lỗi: {e}")

# 3. TTS 3 giọng (Việt, Thái, Khmer)
print("\n[3/4] Kiểm thử In-Memory TTS (3 giọng)...")
voices = [
    ("piper:ngochuyennew", "Xin chào, đây là giọng đọc tiếng Việt."),
    ("piper:th_TH-mms_female-medium", "สวัสดีค่ะ นี่คือเสียงภาษาไทย"),
    ("piper:khmer_tts_v3", "សួស្តី នេះគឺជាសំឡេងភាសាខ្មែរ")
]
for model, text in voices:
    t0 = time.time()
    req_tts = urllib.request.Request(
        "http://127.0.0.1:18080/v1/audio/speech",
        data=json.dumps({
            "model": model,
            "input": text,
            "voice": model.split(":")[-1]
        }).encode(),
        headers={"Content-Type": "application/json"}
    )
    try:
        with urllib.request.urlopen(req_tts, timeout=30) as r:
            audio = r.read()
            dt = (time.time() - t0) * 1000
            print(f" -> {model} thành công ({dt:.1f}ms, {len(audio)} bytes audio)")
    except Exception as e:
        print(f" -> {model} lỗi: {e}")

# 4. Gemma-4 Colab
print("\n[4/4] Kiểm thử Colab T4 GPU (gemma-4-colab)...")
t0 = time.time()
req_gemma = urllib.request.Request(
    "http://127.0.0.1:18080/v1/chat/completions",
    data=json.dumps({
        "model": "gemma-4-colab",
        "messages": [
            {"role": "system", "content": "You are a helpful assistant."},
            {"role": "user", "content": "Who are you? Answer in 5 words."}
        ],
        "max_tokens": 50
    }).encode(),
    headers={"Content-Type": "application/json"}
)
try:
    with urllib.request.urlopen(req_gemma, timeout=60) as r:
        res = json.loads(r.read().decode())
        dt = (time.time() - t0) * 1000
        content = res["choices"][0]["message"]["content"]
        print(f" -> Thành công ({dt:.1f}ms): {content.strip()}")
except Exception as e:
    print(f" -> Lỗi: {e}")

print("\n" + "=" * 60)
print("HOÀN TẤT KIỂM THỬ THỰC TẾ TRÊN TOÀN BỘ 4 HỆ THỐNG!")
print("=" * 60)
