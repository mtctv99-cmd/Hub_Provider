#!/usr/bin/env python3
"""Quality comparison across the 3 Colab workers via the gateway."""
import base64
import json
import requests

BASE = "http://127.0.0.1:18080"
HDR = {"Authorization": "Bearer sk-sp-unlimited-master-key", "Content-Type": "application/json"}
MODELS = ["qwen3.5-2b", "gemma4-e2b", "qwen3.5-0.8b"]

with open("/tmp/bench_cat.png", "rb") as f:
    IMG = base64.b64encode(f.read()).decode()

TEXT_TESTS = [
    ("Factual", "Thủ đô của Việt Nam là thành phố nào? /no_think", 60, None),
    ("Math", "Một cửa hàng bán 15 cây bút, mỗi cây giá 5000 đồng. Hỏi tổng số tiền thu được? /no_think", 120, None),
    ("Format", "Kể tên đúng 3 màu, mỗi màu một dòng, KHÔNG thêm chữ nào khác. /no_think", 60, None),
    ("Idiom", "Thành ngữ 'Nhất tự vi sư, bán tự vi phụ' có ý nghĩa gì? Trả lời ngắn. /no_think", 150, None),
]
VISION_TESTS = [
    ("Vision-ID", "Trong ảnh này là con gì? Trả lời ngắn gọn. /no_think", 80, IMG),
    ("Vision-Detail", "Con vật trong ảnh có màu lông gì? /no_think", 80, IMG),
]


def ask(model, prompt, max_tokens, img=None):
    content = [{"type": "text", "text": prompt}]
    if img:
        content.append({"type": "image_url", "image_url": {"url": f"data:image/png;base64,{img}"}})
    body = {"model": model, "messages": [{"role": "user", "content": content}], "max_tokens": max_tokens, "temperature": 0}
    try:
        r = requests.post(f"{BASE}/v1/chat/completions", headers=HDR, json=body, timeout=180)
        r.raise_for_status()
        text = r.json()["choices"][0]["message"]["content"].strip()
        return text.replace("\n", " ⏎ ")[:220]
    except Exception as e:
        return f"ERROR: {e}"


def main():
    print("=" * 100)
    for name, prompt, mt, img in TEXT_TESTS + VISION_TESTS:
        print(f"\n### [{name}] {prompt[:60]}")
        for m in MODELS:
            ans = ask(m, prompt, mt, img)
            print(f"  {m:<14}: {ans}")


if __name__ == "__main__":
    main()
