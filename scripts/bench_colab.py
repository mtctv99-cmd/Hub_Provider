#!/usr/bin/env python3
"""Text + vision benchmark for the Colab llama.cpp workers via the gateway.

Usage: python3 /tmp/bench_colab.py
No thinking: Qwen models get a /no_think suffix; Gemma ignores it.
"""
import base64
import json
import time

import requests

BASE = "http://127.0.0.1:18080"
KEY = "sk-sp-unlimited-master-key"
HDR = {"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"}

MODELS = ["qwen3.5-2b", "gemma4-e2b", "qwen3.5-0.8b"]

TEXT_PROMPTS = [
    "Viết một đoạn văn ngắn khoảng 80 từ về biển vào mùa hạ. /no_think",
    "Giải thích ngắn gọn giữa tam giác Pythagoras là gì. /no_think",
    "Liệt kê 5 lợi ích của việc đọc sách, mỗi ý một dòng. /no_think",
]
VISION_PROMPT = "Ảnh này là con gì? Mô tả ngắn trong 1 câu. /no_think"
MAX_TEXT_TOKENS = 256
MAX_VISION_TOKENS = 128


def stream_chat(model, content, max_tokens, image_b64=None):
    parts = [{"type": "text", "text": content}]
    if image_b64:
        parts.append({"type": "image_url", "image_url": {"url": f"data:image/png;base64,{image_b64}"}})
    body = {
        "model": model,
        "messages": [{"role": "user", "content": parts if image_b64 else content}],
        "max_tokens": max_tokens,
        "stream": True,
    }
    t0 = time.perf_counter()
    ttft = None
    chunks = 0
    with requests.post(f"{BASE}/v1/chat/completions", headers=HDR, json=body, stream=True, timeout=300) as r:
        r.raise_for_status()
        usage = {}
        for line in r.iter_lines():
            if not line or not line.startswith(b"data: "):
                continue
            payload = line[6:].decode("utf-8", "ignore")
            if payload == "[DONE]":
                break
            obj = json.loads(payload)
            if obj.get("usage") and obj["usage"].get("completion_tokens"):
                usage = obj["usage"]
            for ch in obj.get("choices", []):
                delta = (ch.get("delta") or {}).get("content") or ""
                if delta:
                    chunks += 1
                    if ttft is None:
                        ttft = time.perf_counter() - t0
    total = time.perf_counter() - t0
    comp = usage.get("completion_tokens") or chunks
    return {
        "ttft": ttft,
        "total": total,
        "completion_tokens": comp,
        "tok_s": comp / total if total > 0 else 0,
    }


def bench_model(model, image_b64):
    rows = {"text": [], "vision": []}
    for p in TEXT_PROMPTS:
        rows["text"].append(stream_chat(model, p, MAX_TEXT_TOKENS))
    for _ in range(2):
        rows["vision"].append(stream_chat(model, VISION_PROMPT, MAX_VISION_TOKENS, image_b64))
    return rows


def agg(rows, key):
    vals = [r[key] for r in rows if r[key] is not None]
    return sum(vals) / len(vals) if vals else 0.0


def main():
    with open("/tmp/bench_cat.png", "rb") as f:
        image_b64 = base64.b64encode(f.read()).decode()
    print(f"{'model':<14} {'text ttft(s)':>12} {'text tok/s':>11} {'vision ttft(s)':>15} {'vision tok/s':>13} {'v.runtimes(s)':>14}")
    for m in MODELS:
        try:
            rows = bench_model(m, image_b64)
        except Exception as e:
            print(f"{m:<14} ERROR: {e}")
            continue
        t_t = agg(rows["text"], "ttft")
        t_s = agg(rows["text"], "tok_s")
        v_t = agg(rows["vision"], "ttft")
        v_s = agg(rows["vision"], "tok_s")
        v_runs = ", ".join(f"{r['total']:.1f}" for r in rows["vision"])
        print(f"{m:<14} {t_t:>12.2f} {t_s:>11.1f} {v_t:>15.2f} {v_s:>13.1f} {v_runs:>14}")


if __name__ == "__main__":
    main()
