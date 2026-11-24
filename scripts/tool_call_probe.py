#!/usr/bin/env python3
"""Probe tool-calling behaviour of Gemini web models behind a Sever Provider gateway.

Usage:
  python scripts/tool_call_probe.py --key-file .secrets/key.txt
  python scripts/tool_call_probe.py --base http://127.0.0.1:18080 --key-file key.txt --models a,b

Never hardcodes an API key: it is read (stripped) from --key-file.
Exit code 0 only when no case fails; model-declining under tool_choice=auto is PARTIAL.
"""
import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

TIMEOUT = 200
RESULTS = []

TOOLS = [
    {"type": "function", "function": {
        "name": "get_weather",
        "description": "Get current weather for a city.",
        "parameters": {"type": "object",
                       "properties": {"city": {"type": "string", "description": "City name"}},
                       "required": ["city"]}}},
    {"type": "function", "function": {
        "name": "get_time",
        "description": "Get current local time for a timezone.",
        "parameters": {"type": "object",
                       "properties": {"tz": {"type": "string", "description": "IANA timezone"}},
                       "required": ["tz"]}}},
]


def record(model, case, status, detail=""):
    detail = " ".join(str(detail).split())[:600]
    RESULTS.append({"model": model, "case": case, "status": status, "detail": detail})
    print(f"[{status:7}] {model} {case}: {detail}", flush=True)


def request(base, key, path, body, stream=False, timeout=TIMEOUT):
    req = urllib.request.Request(
        base.rstrip("/") + path,
        data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"},
    )
    start = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            if stream:
                chunks, raw, errors = [], [], []
                done = False
                for line in resp:
                    line = line.decode("utf-8", "replace").strip()
                    raw.append(line)
                    if line.startswith("data:"):
                        payload = line[5:].strip()
                        if payload == "[DONE]":
                            done = True
                            continue
                        try:
                            obj = json.loads(payload)
                        except json.JSONDecodeError:
                            continue
                        if isinstance(obj, dict) and obj.get("error"):
                            errors.append(obj["error"])
                        chunks.append(obj)
                return {"status": resp.status, "chunks": chunks, "raw": raw,
                        "done": done, "errors": errors, "elapsed": time.time() - start}
            text = resp.read().decode("utf-8", "replace")
            try:
                body_json = json.loads(text)
            except json.JSONDecodeError:
                body_json = {"raw": text[:300]}
            return {"status": resp.status, "body": body_json, "elapsed": time.time() - start}
    except urllib.error.HTTPError as e:
        text = e.read().decode("utf-8", "replace")
        try:
            body_json = json.loads(text)
        except json.JSONDecodeError:
            body_json = {"raw": text[:300]}
        return {"status": e.code, "body": body_json, "elapsed": time.time() - start}
    except Exception as e:
        return {"status": 0, "error": type(e).__name__ + ": " + str(e)[:120],
                "elapsed": time.time() - start}


def post_chat(base, key, model, messages, tools=None, tool_choice=None):
    body = {"model": model, "messages": messages}
    if tools is not None:
        body["tools"] = tools
    if tool_choice is not None:
        body["tool_choice"] = tool_choice
    return request(base, key, "/v1/chat/completions", body)


def tool_calls_of(resp):
    """Normalise choices[0].message.tool_calls into [{name, args_str, args_ok, args}]."""
    calls = []
    try:
        raw_calls = resp["body"]["choices"][0]["message"].get("tool_calls") or []
    except (KeyError, IndexError, TypeError):
        return calls
    for call in raw_calls:
        fn = call.get("function") or {}
        raw_args = fn.get("arguments")
        type_ok = isinstance(raw_args, str)
        args = raw_args if type_ok else (json.dumps(raw_args, ensure_ascii=False) if raw_args is not None else "")
        try:
            parsed = json.loads(args)
            ok = isinstance(parsed, dict)
        except (json.JSONDecodeError, TypeError):
            parsed, ok = None, False
        calls.append({"name": fn.get("name") or "", "args_str": args,
                      "args_ok": ok, "args": parsed, "args_type_ok": type_ok})
    return calls

def finish_of(resp):
    try:
        return resp["body"]["choices"][0].get("finish_reason") or ""
    except (KeyError, IndexError, TypeError):
        return ""


def content_of(resp):
    try:
        return resp["body"]["choices"][0]["message"].get("content") or ""
    except (KeyError, IndexError, TypeError):
        return ""


def parse_stream(resp):
    """Assemble tool calls exactly as an OpenAI client would."""
    content_parts = []
    calls = {}
    order = []
    finish = []
    finish_pos = []
    last_tool_pos = -1
    missing_index = 0
    nonstring_args = 0
    for pos, chunk in enumerate(resp.get("chunks", [])):
        for choice in chunk.get("choices") or []:
            delta = choice.get("delta") or {}
            if delta.get("content"):
                content_parts.append(delta["content"])
            if choice.get("finish_reason"):
                finish.append(choice["finish_reason"])
                finish_pos.append(pos)
            for tc in delta.get("tool_calls") or []:
                last_tool_pos = pos
                idx = tc.get("index")
                if not isinstance(idx, int):
                    missing_index += 1
                    idx = len(order)
                if idx not in calls:
                    order.append(idx)
                    calls[idx] = {"id": "", "name": "", "fragments": [],
                                  "first_had_id": False, "first_had_name": False}
                slot = calls[idx]
                first = not slot["fragments"] and not slot["id"] and not slot["name"]
                fn = tc.get("function") or {}
                if tc.get("id"):
                    slot["id"] = tc["id"]
                if fn.get("name"):
                    slot["name"] = fn["name"]
                if first:
                    slot["first_had_id"] = bool(tc.get("id"))
                    slot["first_had_name"] = bool(fn.get("name"))
                frag = fn.get("arguments")
                if frag is None:
                    frag = ""
                if not isinstance(frag, str):
                    nonstring_args += 1
                    frag = json.dumps(frag)
                slot["fragments"].append(frag)
    return {"content": "".join(content_parts), "order": order, "calls": calls,
            "finish": finish, "finish_pos": finish_pos, "last_tool_pos": last_tool_pos,
            "missing_index": missing_index, "nonstring_args": nonstring_args,
            "errors": list(resp.get("errors") or []), "done": bool(resp.get("done"))}

def case_auto(base, key, model):
    hits, details = 0, []
    for i in range(3):
        r = post_chat(base, key, model, [{"role": "user", "content": "Weather in Hanoi? Use the tool."}],
                      tools=TOOLS, tool_choice="auto")
        calls = tool_calls_of(r)
        good = [c for c in calls if c["name"] == "get_weather" and c["args_ok"]]
        if r["status"] == 200 and good:
            hits += 1
            details.append(f"run{i + 1}:ok")
        else:
            details.append(f"run{i + 1}:status={r['status']},calls={len(calls)}")
    if hits == 3:
        status = "PASS"
    elif hits:
        status = "PARTIAL"
    else:
        status = "FAIL"
    record(model, "nonstream_auto", status, f"rate={hits}/3 " + " ".join(details))


def case_required(base, key, model):
    r = post_chat(base, key, model, [{"role": "user", "content": "Weather in Hanoi? Use the tool."}],
                  tools=TOOLS, tool_choice="required")
    calls = tool_calls_of(r)
    names = [c["name"] for c in calls]
    args_ok = bool(calls) and all(c["args_ok"] and c["args_type_ok"] for c in calls)
    city_ok = bool(calls) and isinstance(calls[0]["args"] or {}, dict) and bool(calls[0]["args"].get("city"))
    finish = finish_of(r)
    ok = r["status"] == 200 and names == ["get_weather"] and args_ok and city_ok and finish == "tool_calls"
    record(model, "nonstream_required", "PASS" if ok else "FAIL",
           f"status={r['status']} finish={finish} names={names} args_ok={[c['args_ok'] for c in calls]}")

def case_none(base, key, model):
    r = post_chat(base, key, model, [{"role": "user", "content": "Weather in Hanoi? Use the tool."}],
                  tools=TOOLS, tool_choice="none")
    calls = tool_calls_of(r)
    record(model, "nonstream_none", "PASS" if r["status"] == 200 and not calls else "FAIL",
           f"status={r['status']} calls={len(calls)}")


def case_parallel(base, key, model):
    r = post_chat(base, key, model,
                  [{"role": "user", "content": "Call get_weather for Hanoi AND get_time for Asia/Ho_Chi_Minh. "
                                               "Use both tools in the same turn."}],
                  tools=TOOLS, tool_choice="auto")
    calls = tool_calls_of(r)
    names = [c["name"] for c in calls]
    wanted = {"get_weather", "get_time"}
    ok = (r["status"] == 200 and set(names) == wanted and len(calls) == 2
          and all(c["args_ok"] and c["args_type_ok"] for c in calls)
          and finish_of(r) == "tool_calls")
    record(model, "nonstream_parallel", "PASS" if ok else "FAIL",
           f"status={r['status']} names={names} args_ok={[c['args_ok'] for c in calls]} finish={finish_of(r)}")

def case_stream(base, key, model):
    body = {"model": model, "stream": True,
            "messages": [{"role": "user", "content": "Weather in Hanoi? Use the tool."}],
            "tools": TOOLS, "tool_choice": "required"}
    r = request(base, key, "/v1/chat/completions", body, stream=True)
    if r["status"] != 200:
        record(model, "stream_required", "FAIL", f"status={r['status']} {r.get('error', '')}")
        return
    p = parse_stream(r)
    calls = p["calls"]

    def merged(slot):
        return "".join(slot["fragments"])

    def args_obj(slot):
        try:
            obj = json.loads(merged(slot))
            return obj if isinstance(obj, dict) else None
        except (json.JSONDecodeError, TypeError):
            return None

    leak = any(m in p["content"] for m in ('"tool_calls"', '"get_weather"', '{"city"'))
    ids = [slot["id"] for slot in calls.values() if slot["id"]]
    dup_ids = len(ids) != len(set(ids))
    checks = {
        "names": bool(calls) and all(slot["name"] == "get_weather" for slot in calls.values()),
        "args_json": bool(calls) and all(args_obj(slot) is not None for slot in calls.values()),
        "args_type": p["nonstring_args"] == 0,
        "first_id_name": bool(calls) and all(slot["first_had_id"] and slot["first_had_name"] for slot in calls.values()),
        "index": p["missing_index"] == 0 and len(p["order"]) >= 1,
        "finish_once": p["finish"].count("tool_calls") == 1,
        "finish_last": bool(p["finish_pos"]) and p["finish_pos"][0] >= p["last_tool_pos"],
        "done": p["done"],
        "no_error": not p["errors"],
        "no_leak": not leak,
        "unique_ids": not dup_ids,
    }
    total_args = sum(len(merged(slot)) for slot in calls.values())
    shape = (f"deltas={len(r['chunks'])} indexes={p['order']} args_len={total_args} "
             f"frags={[len(slot['fragments']) for slot in calls.values()]} "
             f"ids={ids} finish={p['finish']} done={p['done']} "
             f"failed={[k for k, v in checks.items() if not v]}")
    record(model, "stream_required", "PASS" if all(checks.values()) else "FAIL", shape)

def case_roundtrip(base, key, model):
    messages = [
        {"role": "user", "content": "Weather in Hanoi?"},
        {"role": "assistant", "content": None, "tool_calls": [
            {"id": "call_1", "type": "function",
             "function": {"name": "get_weather", "arguments": json.dumps({"city": "Hanoi"})}}]},
        {"role": "tool", "tool_call_id": "call_1", "content": '{"temp_c":31,"sky":"clear"}'},
        {"role": "user", "content": "From that tool result, what is the temperature and sky in Hanoi? Answer briefly."},
    ]
    r = post_chat(base, key, model, messages, tools=TOOLS, tool_choice="none")
    text = content_of(r).lower()
    ok = r["status"] == 200 and ("31" in text or "clear" in text)
    record(model, "roundtrip", "PASS" if ok else "FAIL",
           f"status={r['status']} answer={content_of(r)[:120]!r}")


def case_large_args(base, key, model):
    city = "Hanoi " + "a" * 394  # exactly 400 chars
    r = post_chat(base, key, model,
                  [{"role": "user", "content": "Get the weather for this city (use the tool, pass the city verbatim): "
                                               + city}],
                  tools=TOOLS, tool_choice="required")
    calls = tool_calls_of(r)
    if r["status"] != 200 or not calls:
        record(model, "large_args", "FAIL", f"status={r['status']} calls={len(calls)}")
        return
    got = None
    for c in calls:
        if c["args_ok"] and isinstance(c["args"].get("city"), str):
            got = c["args"]["city"]
            break
    if got == city:
        record(model, "large_args", "PASS", f"len={len(got)} exact=true")
    else:
        got_len = len(got or "")
        record(model, "large_args", "PARTIAL",
               f"exact=false got_len={got_len} truncated={got_len < len(city)}")

def case_anthropic(base, key, model):
    body = {"model": model, "max_tokens": 512,
            "messages": [{"role": "user", "content": "Weather in Hanoi? Use the tool."}],
            "tools": [{"name": "get_weather", "description": "Get current weather for a city.",
                       "input_schema": {"type": "object",
                                        "properties": {"city": {"type": "string"}},
                                        "required": ["city"]}}],
            "tool_choice": {"type": "any"}}
    req = urllib.request.Request(
        base.rstrip("/") + "/v1/messages", data=json.dumps(body).encode(),
        headers={"Authorization": "Bearer " + key, "x-api-key": key,
                 "anthropic-version": "2023-06-01", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            data = json.loads(resp.read().decode("utf-8", "replace"))
        blocks = [b.get("type") for b in data.get("content") or []]
        has_tool = "tool_use" in blocks
        stop = data.get("stop_reason")
        ok = has_tool and stop == "tool_use"
        record(model, "anthropic_tools", "PASS" if ok else "FAIL",
               f"tool_use={has_tool} stop_reason={stop} blocks={blocks}")
    except urllib.error.HTTPError as e:
        text = e.read().decode("utf-8", "replace")[:200]
        record(model, "anthropic_tools", "FAIL", f"status={e.code} body={text}")
    except Exception as e:
        record(model, "anthropic_tools", "FAIL", f"error={type(e).__name__}: {str(e)[:120]}")

def case_stream_large(base, key, model):
    city = "Hanoi city name " + "X" * 100
    body = {"model": model, "stream": True,
            "messages": [{"role": "user", "content": "Use get_weather now. Set city to exactly: " + city}],
            "tools": TOOLS, "tool_choice": "required"}
    r = request(base, key, "/v1/chat/completions", body, stream=True)
    if r["status"] != 200:
        record(model, "stream_large_args", "FAIL", f"status={r['status']} {r.get('error', '')}")
        return
    p = parse_stream(r)
    calls = p["calls"]
    exact = False
    for slot in calls.values():
        try:
            obj = json.loads("".join(slot["fragments"]))
            if isinstance(obj, dict) and obj.get("city") == city:
                exact = True
        except (json.JSONDecodeError, TypeError):
            pass
    frags = sum(len(slot["fragments"]) for slot in calls.values())
    checks = {
        "one_call": len(calls) == 1 and list(calls.values())[0]["name"] == "get_weather",
        "exact_args": exact,
        "first_id_name": all(slot["first_had_id"] and slot["first_had_name"] for slot in calls.values()),
        "index": p["missing_index"] == 0,
        "finish_last": p["finish"].count("tool_calls") == 1 and bool(p["finish_pos"]) and p["finish_pos"][0] >= p["last_tool_pos"],
        "done": p["done"] and not p["errors"],
    }
    record(model, "stream_large_args", "PASS" if all(checks.values()) else "FAIL",
           f"frags={frags} exact={exact} failed={[k for k, v in checks.items() if not v]}")

def case_tool_choice_target(base, key, model):
    r = post_chat(base, key, model, [{"role": "user", "content": "Weather in Hanoi?"}],
                  tools=TOOLS,
                  tool_choice={"type": "function", "function": {"name": "get_weather"}})
    calls = tool_calls_of(r)
    names = [c["name"] for c in calls]
    ok = r["status"] == 200 and names == ["get_weather"]
    record(model, "tool_choice_target", "PASS" if ok else "FAIL",
           f"status={r['status']} names={names}")


def ready_version(base):
    try:
        with urllib.request.urlopen(base.rstrip("/") + "/ready", timeout=30) as resp:
            data = json.loads(resp.read().decode("utf-8", "replace"))
        return str(data.get("version") or "unknown")
    except Exception as e:
        return "unknown (" + type(e).__name__ + ")"


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base", default="http://192.168.1.117:18080")
    ap.add_argument("--key-file", required=True)
    ap.add_argument("--out", default=".probe/tool_calls.json")
    ap.add_argument("--models", default="gemini-4-hydrogen,gemini-3.5-flash-lite,gemini-3.1-pro")
    args = ap.parse_args()

    try:
        with open(args.key_file, "r", encoding="utf-8") as fh:
            key = fh.read().strip()
    except OSError as e:
        print(f"error: cannot read --key-file: {e}", file=sys.stderr)
        return 2
    if not key:
        print("error: --key-file is empty", file=sys.stderr)
        return 2

    models = [m.strip() for m in args.models.split(",") if m.strip()]
    version = ready_version(args.base)
    print(f"gateway={args.base} version={version} models={models}", flush=True)

    cases = [case_auto, case_required, case_none, case_parallel, case_stream, case_stream_large,
             case_roundtrip, case_large_args, case_anthropic, case_tool_choice_target]
    for model in models:
        for case in cases:
            try:
                case(args.base, key, model)
            except Exception as e:  # never abort the whole sweep
                status = "FAIL"
                record(model, case.__name__.replace("case_", ""), status,
                       f"probe_error={type(e).__name__}: {str(e)[:120]}")

    summary = {"pass": 0, "fail": 0, "partial": 0, "info": 0}
    for item in RESULTS:
        summary[item["status"].lower()] += 1
    payload = {"version": version, "results": RESULTS, "summary": summary}
    out_dir = os.path.dirname(os.path.abspath(args.out))
    os.makedirs(out_dir, exist_ok=True)
    with open(args.out, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, ensure_ascii=False, indent=2)
    print(f"summary pass={summary['pass']} fail={summary['fail']} partial={summary['partial']} "
          f"info={summary['info']} -> {args.out}", flush=True)
    return 1 if summary["fail"] else 0


if __name__ == "__main__":
    sys.exit(main())
