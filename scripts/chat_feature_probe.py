#!/usr/bin/env python3
"""Probe the OpenAI-compatible chat surface of a Sever Provider gateway.

Usage: python scripts/chat_feature_probe.py [--base URL] [--key KEY] [--model ID]

Prints one PASS/FAIL/PARTIAL line per feature so gaps are easy to enumerate.
"""
import argparse
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.request

RESULTS = []


def record(feature, status, detail=""):
    RESULTS.append((feature, status, detail))
    print(f"[{status:7}] {feature}: {detail}", flush=True)


def post(base, key, path, body, timeout=180, raw=False, stream=False):
    req = urllib.request.Request(
        base.rstrip("/") + path,
        data=json.dumps(body).encode() if not raw else body,
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"},
    )
    start = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            if stream:
                chunks = []
                for line in resp:
                    line = line.decode("utf-8", "replace").strip()
                    if line.startswith("data:"):
                        payload = line[5:].strip()
                        if payload == "[DONE]":
                            chunks.append({"__done__": True})
                            continue
                        try:
                            chunks.append(json.loads(payload))
                        except json.JSONDecodeError:
                            chunks.append({"__raw__": payload})
                return {"status": resp.status, "headers": dict(resp.headers), "chunks": chunks,
                        "elapsed": time.time() - start}
            payload = json.loads(resp.read().decode("utf-8", "replace"))
            return {"status": resp.status, "headers": dict(resp.headers), "body": payload,
                    "elapsed": time.time() - start}
    except urllib.error.HTTPError as e:
        body_text = e.read().decode("utf-8", "replace")
        try:
            parsed = json.loads(body_text)
        except json.JSONDecodeError:
            parsed = {"raw": body_text[:300]}
        return {"status": e.code, "body": parsed, "elapsed": time.time() - start}
    except Exception as e:  # timeout, reset
        return {"status": 0, "error": type(e).__name__, "elapsed": time.time() - start}


def text_of(resp):
    try:
        return resp["body"]["choices"][0]["message"]["content"] or ""
    except (KeyError, IndexError, TypeError):
        return ""


def stream_text(resp):
    out = []
    for ch in resp.get("chunks", []):
        for choice in ch.get("choices") or []:
            delta = choice.get("delta") or {}
            if delta.get("content"):
                out.append(delta["content"])
    return "".join(out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://192.168.1.117:18080")
    ap.add_argument("--key", default=os.environ.get("SEVER_API_KEY", ""),
                    help="API key; defaults to $SEVER_API_KEY (never hardcode one here)")
    ap.add_argument("--model", default="gemini-4-hydrogen")
    ap.add_argument("--tool-model", default="gemini-4-hydrogen")
    ap.add_argument("--vision-model", default="gemini-4-hydrogen")
    args = ap.parse_args()
    base, key = args.base, args.key
    if not key:
        print("error: pass --key or set SEVER_API_KEY", file=sys.stderr)
        return 2

    # 1. Basic non-stream chat
    r = post(base, key, "/v1/chat/completions",
             {"model": args.model, "messages": [{"role": "user", "content": "Say OK"}]})
    ok = r["status"] == 200 and text_of(r)
    record("chat.basic", "PASS" if ok else "FAIL", f"status={r['status']} {round(r['elapsed'],1)}s")

    # 2. Params: temperature / top_p / max_tokens / seed / penalties
    body = {"model": args.model, "messages": [{"role": "user", "content": "Say OK"}],
            "temperature": 0.2, "top_p": 0.9, "max_tokens": 20, "seed": 7,
            "presence_penalty": 0.1, "frequency_penalty": 0.1}
    r = post(base, key, "/v1/chat/completions", body)
    record("chat.params", "PASS" if r["status"] == 200 else "FAIL", f"status={r['status']}")

    # 3. max_tokens enforcement
    r = post(base, key, "/v1/chat/completions",
             {"model": args.model, "messages": [{"role": "user", "content": "Count from 1 to 200"}],
              "max_tokens": 8})
    content = text_of(r)
    record("chat.max_tokens", "PASS" if r["status"] == 200 and len(content) < 400 else "PARTIAL",
           f"len={len(content)} finish={r.get('body', {}).get('choices', [{}])[0].get('finish_reason')}")

    # 4. stop sequences (OpenAI semantics: exact, case-sensitive substring)
    r = post(base, key, "/v1/chat/completions",
             {"model": args.model,
              "messages": [{"role": "user", "content": "Reply exactly: alpha BRAVO charlie delta"}],
              "stop": ["charlie"]})
    kept = text_of(r)
    record("chat.stop", "PASS" if "charlie" not in kept else "FAIL", f"text={kept[:40]!r}")

    # 5. n > 1: rejected explicitly (web providers answer one turn per request)
    r = post(base, key, "/v1/chat/completions",
             {"model": args.model, "messages": [{"role": "user", "content": "Say OK"}], "n": 2})
    if r["status"] == 200:
        n = len(r["body"].get("choices") or [])
        record("chat.n", "FAIL", f"n>1 silently returned choices={n}")
    else:
        code = ((r.get("body") or {}).get("error") or {}).get("code")
        record("chat.n", "PASS" if r["status"] == 400 and code == "unsupported_parameter" else "PARTIAL",
               f"status={r['status']} code={code}")

    # 6. response_format json_object
    r = post(base, key, "/v1/chat/completions",
             {"model": args.model, "messages": [{"role": "user", "content": 'Return JSON {"ok":true}'}],
              "response_format": {"type": "json_object"}})
    body_text = text_of(r)
    parsed_ok = False
    try:
        parsed_ok = isinstance(json.loads(body_text.strip().strip("`")), dict)
    except Exception:
        parsed_ok = "{" in body_text and "}" in body_text
    record("chat.response_format", "PASS" if r["status"] == 200 and parsed_ok else "PARTIAL",
           f"status={r['status']} text={body_text[:50]!r}")

    # 7. system prompt obeyed
    r = post(base, key, "/v1/chat/completions",
             {"model": args.model,
              "messages": [{"role": "system", "content": "Always answer with the single word BANANA."},
                           {"role": "user", "content": "What is 2+2?"}]})
    record("chat.system_prompt", "PASS" if "banana" in text_of(r).lower() else "PARTIAL",
           f"text={text_of(r)[:40]!r}")

    # 8. usage accounting (non-stream)
    r = post(base, key, "/v1/chat/completions",
             {"model": args.model, "messages": [{"role": "user", "content": "Say OK"}]})
    usage = (r.get("body") or {}).get("usage") or {}
    record("chat.usage", "PASS" if usage.get("total_tokens") else "FAIL", f"usage={usage}")

    # 9. stream basics: chunks + finish_reason + [DONE]
    r = post(base, key, "/v1/chat/completions",
             {"model": args.model, "stream": True,
              "messages": [{"role": "user", "content": "Count 1 to 10"}]}, stream=True)
    chunks = r.get("chunks", [])
    has_done = any(c.get("__done__") for c in chunks)
    finish = ""
    for c in chunks:
        for choice in c.get("choices") or []:
            if choice.get("finish_reason"):
                finish = choice["finish_reason"]
    text = stream_text(r)
    record("sse.basic", "PASS" if r["status"] == 200 and has_done and text else "FAIL",
           f"chunks={len(chunks)} done={has_done} finish={finish} len={len(text)}")

    # 10. stream usage
    usage_stream = {}
    for c in chunks:
        if c.get("usage"):
            usage_stream = c["usage"]
    record("sse.usage", "PASS" if usage_stream.get("total_tokens") else "FAIL", f"usage={usage_stream}")

    # 11. SSE headers
    h = r.get("headers", {})
    need = {"Content-Type": "text/event-stream", "X-Accel-Buffering": "no"}
    got = {k: h.get(k) for k in need}
    ok = (got["Content-Type"] or "").startswith("text/event-stream") and got["X-Accel-Buffering"] == "no"
    record("sse.headers", "PASS" if ok else "FAIL", json.dumps(got))

    # 12. tool calling (non-stream)
    tools = [{"type": "function", "function": {
        "name": "get_weather", "description": "Get weather for a city",
        "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}]
    r = post(base, key, "/v1/chat/completions",
             {"model": args.tool_model, "messages": [{"role": "user", "content": "Weather in Hanoi? Use the tool."}],
              "tools": tools, "tool_choice": "auto"})
    msg = (r.get("body") or {}).get("choices", [{}])[0].get("message", {})
    calls = msg.get("tool_calls") or []
    record("tools.nonstream", "PASS" if calls and calls[0]["function"]["name"] == "get_weather" else "FAIL",
           f"status={r['status']} calls={len(calls)} finish={(r.get('body') or {}).get('choices', [{}])[0].get('finish_reason')}")

    # 13. tool_choice=required
    r = post(base, key, "/v1/chat/completions",
             {"model": args.tool_model, "messages": [{"role": "user", "content": "Hanoi weather"}],
              "tools": tools, "tool_choice": "required"})
    calls_req = ((r.get("body") or {}).get("choices", [{}])[0].get("message", {}) or {}).get("tool_calls") or []
    record("tools.tool_choice_required", "PASS" if calls_req else "PARTIAL", f"calls={len(calls_req)}")

    # 14. tool_choice=none must not call
    r = post(base, key, "/v1/chat/completions",
             {"model": args.tool_model, "messages": [{"role": "user", "content": "Weather in Hanoi? Use the tool."}],
              "tools": tools, "tool_choice": "none"})
    calls_none = ((r.get("body") or {}).get("choices", [{}])[0].get("message", {}) or {}).get("tool_calls") or []
    record("tools.tool_choice_none", "PASS" if not calls_none else "FAIL", f"calls={len(calls_none)}")

    # 15. parallel tool calls
    two_tools = tools + [{"type": "function", "function": {
        "name": "get_time", "description": "Get local time",
        "parameters": {"type": "object", "properties": {"tz": {"type": "string"}}}}}]
    r = post(base, key, "/v1/chat/completions",
             {"model": args.tool_model,
              "messages": [{"role": "user", "content": "Weather in Hanoi and the time there. Call both tools."}],
              "tools": two_tools, "tool_choice": "auto"})
    parallel = ((r.get("body") or {}).get("choices", [{}])[0].get("message", {}) or {}).get("tool_calls") or []
    record("tools.parallel", "PASS" if len(parallel) >= 2 else "PARTIAL", f"calls={len(parallel)}")

    # 16. streaming tool calls
    r = post(base, key, "/v1/chat/completions",
             {"model": args.tool_model, "stream": True,
              "messages": [{"role": "user", "content": "Weather in Hanoi? Use the tool."}],
              "tools": tools, "tool_choice": "required"}, stream=True)
    stream_calls, indexes = 0, set()
    for c in r.get("chunks", []):
        for choice in c.get("choices") or []:
            for tc in ((choice.get("delta") or {}).get("tool_calls") or []):
                stream_calls += 1
                indexes.add(tc.get("index"))
    record("tools.stream", "PASS" if stream_calls else "FAIL",
           f"deltas={stream_calls} indexes={sorted(i for i in indexes if i is not None)}")

    # 17. tool result round-trip
    follow = [{"role": "user", "content": "Weather in Hanoi? Use the tool."}]
    if calls:
        follow.append({"role": "assistant", "content": "", "tool_calls": calls})
        follow.append({"role": "tool", "tool_call_id": calls[0].get("id", "call_0"),
                       "content": '{"temp_c": 31, "sky": "clear"}'})
    r = post(base, key, "/v1/chat/completions", {"model": args.tool_model, "messages": follow, "tools": tools})
    final_text = text_of(r)
    record("tools.roundtrip", "PASS" if r["status"] == 200 and ("31" in final_text or "clear" in final_text.lower())
           else "PARTIAL", f"status={r['status']} text={final_text[:60]!r}")

    # 18. vision with data URI
    try:
        raw = open("test_gemini_cat.png", "rb").read()
    except OSError:
        raw = None
    if raw:
        b64 = base64.b64encode(raw).decode()
        r = post(base, key, "/v1/chat/completions",
                 {"model": args.vision_model, "messages": [{"role": "user", "content": [
                     {"type": "text", "text": "What animal is in this image? One word."},
                     {"type": "image_url", "image_url": {"url": "data:image/png;base64," + b64}}]}]})
        record("vision.data_uri", "PASS" if r["status"] == 200 and text_of(r) else "FAIL",
               f"status={r['status']} text={text_of(r)[:40]!r}")
    else:
        record("vision.data_uri", "SKIP", "test_gemini_cat.png not found")

    # 19. reasoning_content exposure
    r = post(base, key, "/v1/chat/completions",
             {"model": args.model + "-high", "messages": [{"role": "user", "content": "17*23?"}]})
    msg = (r.get("body") or {}).get("choices", [{}])[0].get("message", {}) or {}
    record("chat.reasoning_content", "PASS" if "reasoning_content" in msg else "PARTIAL",
           f"keys={sorted(msg.keys())}")

    # 20. session continuity
    sid = "probe-sess-%d" % int(time.time())
    headers_first = {"Authorization": "Bearer " + key, "Content-Type": "application/json",
                     "X-Session-ID": sid}
    req = urllib.request.Request(base.rstrip("/") + "/v1/chat/completions",
                                 data=json.dumps({"model": args.model,
                                                  "messages": [{"role": "user", "content": "My name is Probe123. Remember it."}]}).encode(),
                                 headers=headers_first)
    with urllib.request.urlopen(req, timeout=180) as resp:
        resp.read()
    req = urllib.request.Request(base.rstrip("/") + "/v1/chat/completions",
                                 data=json.dumps({"model": args.model,
                                                  "messages": [{"role": "user", "content": "What is my name?"}]}).encode(),
                                 headers=headers_first)
    try:
        with urllib.request.urlopen(req, timeout=180) as resp:
            payload = json.loads(resp.read())
        answer = payload["choices"][0]["message"]["content"]
        record("session.continuity", "PASS" if "probe123" in answer.lower() else "PARTIAL", f"text={answer[:50]!r}")
    except Exception as e:
        record("session.continuity", "FAIL", type(e).__name__)

    # 21. error masking
    r = post(base, key, "/v1/chat/completions", {"model": "no-such-model", "messages": [{"role": "user", "content": "hi"}]})
    code = ((r.get("body") or {}).get("error") or {}).get("code")
    # OpenAI returns 404 model_not_found for an unknown model; 400 is also acceptable.
    record("errors.unknown_model", "PASS" if r["status"] in (400, 404) and code else "FAIL", f"status={r['status']} code={code}")

    r = post(base, "bad-key", "/v1/chat/completions", {"model": args.model, "messages": [{"role": "user", "content": "hi"}]})
    code = ((r.get("body") or {}).get("error") or {}).get("code")
    record("errors.bad_api_key", "PASS" if r["status"] == 401 and code == "invalid_api_key" else "FAIL",
           f"status={r['status']} code={code}")

    # 22. Anthropic-compatible surface
    r = post(base, key, "/v1/messages",
             {"model": args.model, "max_tokens": 64,
              "messages": [{"role": "user", "content": "Say OK"}]})
    record("api.anthropic_messages", "PASS" if r["status"] == 200 else "FAIL", f"status={r['status']}")

    # 23. Anthropic top-level system field
    r = post(base, key, "/v1/messages",
             {"model": args.model, "max_tokens": 64,
              "system": "Always answer with the single word BANANA.",
              "messages": [{"role": "user", "content": "What is 2+2?"}]})
    blocks = ((r.get("body") or {}).get("content") or [{}])
    text = blocks[0].get("text", "") if blocks else ""
    record("api.anthropic_system", "PASS" if "banana" in text.lower() else "PARTIAL", f"text={text[:40]!r}")

    # 24. Anthropic streaming usage
    r = post(base, key, "/v1/messages",
             {"model": args.model, "max_tokens": 64, "stream": True,
              "messages": [{"role": "user", "content": "Say OK"}]}, stream=True)
    usage_a = {}
    for c in r.get("chunks", []):
        u = c.get("usage") or {}
        if u.get("total_tokens") or u.get("output_tokens"):
            usage_a = u
    ok_a = bool(usage_a.get("total_tokens") or usage_a.get("output_tokens"))
    record("api.anthropic_stream_usage", "PASS" if ok_a else "FAIL", f"usage={usage_a}")

    print("\n=== SUMMARY ===")
    fails = [f for f in RESULTS if f[1] == "FAIL"]
    partial = [f for f in RESULTS if f[1] == "PARTIAL"]
    print(f"pass={len([f for f in RESULTS if f[1]=='PASS'])} partial={len(partial)} fail={len(fails)}")
    for name, status, detail in fails:
        print(f"  FAIL    {name}: {detail}")
    for name, status, detail in partial:
        print(f"  PARTIAL {name}: {detail}")
    return 0 if not fails else 1


if __name__ == "__main__":
    sys.exit(main())
