import urllib.request
import json
import time

API_KEY = "omni-secret-key-123"
BASE_URL = "http://127.0.0.1:18080/v1/chat/completions"
MODELS = ["deepseek-chat", "gemini-3.8-flash", "gpt-5.6", "deepseek-reasoner"]

def call_model(model, prompt):
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}]
    }
    req = urllib.request.Request(
        BASE_URL,
        data=json.dumps(payload).encode("utf-8"),
        headers={"Authorization": f"Bearer {API_KEY}", "Content-Type": "application/json"}
    )
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            data = json.loads(resp.read().decode())
            dur = round(time.time() - t0, 2)
            content = data["choices"][0]["message"]["content"].strip()
            return {"status": "OK", "duration": dur, "content": content}
    except Exception as e:
        dur = round(time.time() - t0, 2)
        return {"status": f"ERR: {e}", "duration": dur, "content": ""}

tests = [
    {
        "title": "Test 1 - Câu đố mẹo (Logic)",
        "prompt": "Một người nông dân có 17 con cừu, tất cả trừ 9 con chạy mất. Hỏi người đó còn lại bao nhiêu con cừu? Trả lời thật ngắn gọn 1 câu."
    },
    {
        "title": "Test 2 - Viết code Python (Coding)",
        "prompt": "Viết hàm Python kiểm tra số nguyên tố is_prime(n) tối ưu O(sqrt(n)). Chỉ đưa ra mã nguồn code hoàn chỉnh, không giải thích dài dòng."
    },
    {
        "title": "Test 3 - Viết văn bản (Vietnamese Writing)",
        "prompt": "Viết 1 đoạn văn ngắn 3 câu miêu tả hương vị đặc trưng của món Phở Bò Hà Nội."
    }
]

for t in tests:
    print("=" * 60)
    print(f"[*] {t['title']}")
    print(f"Prompt: {t['prompt']}")
    print("-" * 60)
    for m in MODELS:
        res = call_model(m, t["prompt"])
        print(f"--> [{m}] (Thời gian: {res['duration']}s | Status: {res['status']})")
        print(f"{res['content'][:400]}")
        print()
