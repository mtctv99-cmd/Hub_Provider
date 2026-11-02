#!/usr/bin/env python3
"""Benchmark chat models on the live gateway: latency, throughput, accuracy.

Usage: python scripts/bench_models.py [--base URL] [--key KEY] [--models a,b,c]
Writes bench_results.json and prints a summary table.
"""
import argparse
import json
import re
import statistics
import sys
import unicodedata
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

DEFAULT_BASE = "http://192.168.1.117:18080"
DEFAULT_KEY = "sk-sp-unlimited-master-key"

def _norm(text):
    text = unicodedata.normalize("NFD", text.lower().replace("đ", "d"))
    return "".join(c for c in text if unicodedata.category(c) != "Mn")


PROMPTS = [
    {
        "id": "math",
        "prompt": "Một cửa hàng có 3 hộp, mỗi hộp 24 quả cam. Họ bán 17 quả. Còn bao nhiêu quả? Chỉ trả lời bằng số.",
        "check": lambda t: "55" in t,
    },
    {
        "id": "weekday",
        "prompt": "Hôm nay là thứ Ba. 100 ngày nữa là thứ mấy? Chỉ trả lời đúng một từ tiếng Việt.",
        "check": lambda t: "nam" in _norm(t) or "5" in t,
    },
    {
        "id": "code",
        "prompt": "Viết hàm Python reverse_words(s) đảo thứ tự các từ. Chỉ trả code trong một block.",
        "check": lambda t: "split" in t and ("join" in t or "reversed" in t),
    },
    {
        "id": "json",
        "prompt": 'Trả về đúng JSON {"ok": true, "n": 3} và không kèm gì khác.',
        "check": lambda t: _json_ok(t),
    },
    {
        "id": "words",
        "prompt": "Viết một câu tiếng Việt đúng 5 từ về con mèo. Chỉ đưa câu đó.",
        "check": lambda t: 5 <= len(re.findall(r"\w+", t.strip())) <= 6,
    },
]


def _json_ok(text):
    m = re.search(r"\{.*\}", text, re.S)
    if not m:
        return False
    try:
        d = json.loads(m.group(0))
    except Exception:
        return False
    return d.get("ok") is True and d.get("n") == 3


def stream_chat(base, key, model, prompt, timeout):
    body = json.dumps({
        "model": model,
        "stream": True,
        "messages": [{"role": "user", "content": prompt}],
    }).encode()
    req = urllib.request.Request(
        base.rstrip("/") + "/v1/chat/completions",
        data=body,
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"},
    )
    start = time.time()
    ttft = None
    parts, usage = [], None
    err = None
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            for raw in resp:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if payload == "[DONE]":
                    break
                try:
                    chunk = json.loads(payload)
                except Exception:
                    continue
                if chunk.get("error"):
                    err = chunk["error"].get("code") or chunk["error"].get("message")
                    break
                if chunk.get("usage"):
                    usage = chunk["usage"]
                for choice in chunk.get("choices") or []:
                    text = (choice.get("delta") or {}).get("content") or ""
                    if text:
                        if ttft is None:
                            ttft = time.time() - start
                        parts.append(text)
    except urllib.error.HTTPError as e:
        err = "http_%d" % e.code
        try:
            detail = json.loads(e.read().decode("utf-8", "replace"))
            err = (detail.get("error") or {}).get("code") or err
        except Exception:
            pass
    except Exception as e:  # timeout, reset
        err = type(e).__name__.lower()
    total = time.time() - start
    text = "".join(parts).strip()
    completion = (usage or {}).get("completion_tokens") or max(1, len(text) // 4)
    gen = total - (ttft or total)
    tps = completion / gen if gen > 0 and text else 0.0
    return {
        "text": text,
        "ttft": ttft,
        "total": total,
        "completion_tokens": completion,
        "tps": tps,
        "error": err,
    }


def bench_model(base, key, model, timeout, rounds):
    runs = []
    for _ in range(rounds):
        for p in PROMPTS:
            r = stream_chat(base, key, model, p["prompt"], timeout)
            r["check"] = p["id"]
            r["ok"] = bool(r["text"]) and p["check"](r["text"])
            runs.append(r)
    good = [r for r in runs if not r["error"] and r["text"]]
    return {
        "model": model,
        "runs": len(runs),
        "errors": sum(1 for r in runs if r["error"]),
        "error_kinds": sorted({r["error"] for r in runs if r["error"]}),
        "accuracy": round(sum(1 for r in runs if r["ok"]) / len(runs), 3),
        "ttft_ms": round(1000 * statistics.median([r["ttft"] for r in good]), 0) if good else None,
        "total_ms": round(1000 * statistics.median([r["total"] for r in good]), 0) if good else None,
        "tps": round(statistics.median([r["tps"] for r in good]), 1) if good else None,
        "failed_checks": [r["check"] for r in runs if r["text"] and not r["ok"]],
        "sample": (good[0]["text"][:120] if good else ""),
        "answers": [{"check": r["check"], "ok": r["ok"], "text": r["text"][:90], "err": r["error"]} for r in runs],
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default=DEFAULT_BASE)
    ap.add_argument("--key", default=DEFAULT_KEY)
    ap.add_argument("--models", default="")
    ap.add_argument("--timeout", type=int, default=200)
    ap.add_argument("--rounds", type=int, default=1)
    ap.add_argument("--concurrency", type=int, default=3)
    ap.add_argument("--out", default="bench_results.json")
    args = ap.parse_args()

    if args.models:
        models = [m.strip() for m in args.models.split(",") if m.strip()]
    else:
        req = urllib.request.Request(
            args.base.rstrip("/") + "/v1/models",
            headers={"Authorization": "Bearer " + args.key},
        )
        with urllib.request.urlopen(req, timeout=30) as resp:
            listing = json.load(resp)
        models = []
        for m in listing.get("data") or []:
            caps = m.get("capabilities") or []
            if "chat" in caps and m["id"] not in ("gemini-image", "muse-image", "dall-e-3"):
                models.append(m["id"])
        models = sorted(models)

    print("benchmarking %d models: %s" % (len(models), ", ".join(models)), flush=True)
    results = []
    lock = threading.Lock()

    def work(model):
        r = bench_model(args.base, args.key, model, args.timeout, args.rounds)
        with lock:
            results.append(r)
            print("%-45s acc=%.2f ttft=%sms tps=%s err=%d %s" % (
                r["model"], r["accuracy"], r["ttft_ms"], r["tps"], r["errors"],
                ",".join(r["error_kinds"])), flush=True)

    with ThreadPoolExecutor(max_workers=args.concurrency) as ex:
        list(ex.map(work, models))

    results.sort(key=lambda r: (-r["accuracy"], r["total_ms"] or 10**9))
    with open(args.out, "w", encoding="utf-8") as fh:
        json.dump(results, fh, ensure_ascii=False, indent=2)

    print("\n%-45s %5s %8s %10s %7s %5s" % ("model", "acc", "ttft_ms", "total_ms", "tps", "err"))
    for r in results:
        print("%-45s %5.2f %8s %10s %7s %5d" % (
            r["model"], r["accuracy"], r["ttft_ms"], r["total_ms"], r["tps"], r["errors"]))


if __name__ == "__main__":
    sys.exit(main())
