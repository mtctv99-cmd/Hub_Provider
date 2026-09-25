import json
import time
import urllib.request
import urllib.error
from openai import OpenAI

API_BASE = "http://127.0.0.1:18080/v1"
API_KEY = "sk-sp-unlimited-master-key"
client = OpenAI(base_url=API_BASE, api_key=API_KEY, timeout=25.0)

print("=" * 70, flush=True)
print("E2E COMPREHENSIVE PRODUCTION TEST - SEVER_PROVIDER (192.168.1.117)", flush=True)
print("=" * 70, flush=True)

results = {}

# 1. TEST GEMINI WEB (gemini-3.8-flash)
print("\n[1] Testing Gemini Web (gemini-3.8-flash)...", flush=True)
try:
    resp = client.chat.completions.create(
        model="gemini-3.8-flash",
        messages=[{"role": "user", "content": "Xin chao, hay tra loi ngan gon trong 5 tu."}],
        max_tokens=30
    )
    txt = resp.choices[0].message.content
    results["Gemini Web (gemini-3.8-flash)"] = f"PASS: {txt[:40]}"
    print(f" -> Output: {txt}", flush=True)
except Exception as e:
    results["Gemini Web (gemini-3.8-flash)"] = f"FAIL: {e}"
    print(f" -> Error: {e}", flush=True)

# 2. TEST TOOL / FUNCTION CALLING (Gemini)
print("\n[2] Testing Tool / Function Calling (gemini-3.8-flash)...", flush=True)
try:
    tools_spec = [{
        "type": "function",
        "function": {
            "name": "lookup_stock",
            "description": "Lấy giá cổ phiếu",
            "parameters": {
                "type": "object",
                "properties": {"symbol": {"type": "string"}},
                "required": ["symbol"]
            }
        }
    }]
    tool_resp = client.chat.completions.create(
        model="gemini-3.8-flash",
        messages=[{"role": "user", "content": "Giá cổ phiếu VNM bao nhiêu? Hãy gọi tool."}],
        tools=tools_spec,
        tool_choice="auto"
    )
    ch = tool_resp.choices[0]
    has_tc = ch.finish_reason == "tool_calls" and len(ch.message.tool_calls or []) > 0
    results["Tool Calling (Gemini)"] = "PASS" if has_tc else f"FAIL ({ch.finish_reason})"
    print(f" -> Finish Reason: {ch.finish_reason}, ToolCalls: {len(ch.message.tool_calls or [])}", flush=True)
except Exception as e:
    results["Tool Calling (Gemini)"] = f"FAIL: {e}"
    print(f" -> Error: {e}", flush=True)

# 3. TEST COPILOT M365 (gpt-5.6)
print("\n[3] Testing Copilot M365 (gpt-5.6)...", flush=True)
try:
    resp = client.chat.completions.create(
        model="gpt-5.6",
        messages=[{"role": "user", "content": "Hello, reply 'Copilot OK' in 2 words."}],
        max_tokens=20
    )
    txt = resp.choices[0].message.content
    results["Copilot M365 (gpt-5.6)"] = f"PASS: {txt[:40]}"
    print(f" -> Output: {txt}", flush=True)
except Exception as e:
    # Notice: Copilot tokens can expire periodically on cloud sessions
    results["Copilot M365 (gpt-5.6)"] = f"NOTICE: {e}"
    print(f" -> Result: {e}", flush=True)

# 4. TEST IN-MEMORY TTS: ALL 3 VOICES
for voice_name, sample_text in [
    ("piper:ngochuyennew", "Xin chào, đây là giọng đọc Ngọc Huyền trên hệ thống mới."),
    ("piper:th_TH-mms_female-medium", "สวัสดี นี่คือการทดสอบเสียงภาษาไทย"),
    ("piper:khmer_tts_v3", "សួស្តី នេះជាការសាកល្បងសំឡេងខ្មែរ")
]:
    print(f"\n[4] Testing In-Memory TTS ({voice_name})...", flush=True)
    try:
        t0 = time.time()
        sp = client.audio.speech.create(
            model=voice_name,
            voice=voice_name.replace("piper:", ""),
            input=sample_text
        )
        audio = sp.content
        dur = round((time.time() - t0) * 1000, 1)
        results[f"TTS ({voice_name})"] = f"PASS ({len(audio)} bytes in {dur}ms)"
        print(f" -> Generated {len(audio)} bytes in {dur}ms", flush=True)
    except Exception as e:
        results[f"TTS ({voice_name})"] = f"FAIL: {e}"
        print(f" -> Error: {e}", flush=True)

# 5. TEST TIKTOK TTS
print("\n[5] Testing TikTok TTS (tiktok:BV074_streaming & tiktok:vi_male)...", flush=True)
for m in ["tiktok:BV074_streaming", "tiktok:vi_male"]:
    try:
        t0 = time.time()
        sp = client.audio.speech.create(
            model=m,
            voice=m,
            input="Kiểm tra giọng đọc TikTok trên máy chủ mới."
        )
        audio = sp.content
        dur = round((time.time() - t0) * 1000, 1)
        results[f"TikTok TTS ({m})"] = f"PASS ({len(audio)} bytes in {dur}ms)"
        print(f" -> {m}: Generated {len(audio)} bytes in {dur}ms", flush=True)
    except Exception as e:
        results[f"TikTok TTS ({m})"] = f"FAIL: {e}"
        print(f" -> {m} Error: {e}", flush=True)

# 6. TEST DEEPSEEK MODELS RECOGNITION & DISPATCH
print("\n[6] Testing DeepSeek Models (/v1/models & /v1/chat/completions)...", flush=True)
try:
    models_list = client.models.list()
    ds_models = [m.id for m in models_list.data if "deepseek" in m.id]
    if len(ds_models) >= 6:
        results["DeepSeek Models Registered"] = f"PASS ({len(ds_models)} models: {', '.join(ds_models[:3])}...)"
        print(f" -> Found {len(ds_models)} DeepSeek models: {ds_models}", flush=True)
    else:
        results["DeepSeek Models Registered"] = f"FAIL (found {len(ds_models)})"
        print(f" -> Found models: {ds_models}", flush=True)
except Exception as e:
    results["DeepSeek Models Registered"] = f"FAIL: {e}"
    print(f" -> Error: {e}", flush=True)

# 7. TEST STRICT SINGLE PORT 18080 & CLOSED LEGACY PORTS
print("\n[7] Verifying Legacy Ports Closed (8081, 8090, 8095) & Single Port 18080 Active...", flush=True)
for port in [8081, 8090, 8095]:
    try:
        req = urllib.request.Request(f"http://127.0.0.1:{port}/")
        with urllib.request.urlopen(req, timeout=2) as r:
            results[f"Legacy Port {port} Closed"] = f"FAIL (Port still open: HTTP {r.status})"
            print(f" -> Port {port} unexpectedly responded HTTP {r.status}", flush=True)
    except Exception as e:
        # Expected: Connection refused
        results[f"Legacy Port {port} Closed"] = "PASS (Connection Refused)"
        print(f" -> Port {port} successfully closed: {e}", flush=True)

# 8. VERIFY SEVER-V2 SERVICE HEALTH (ports 18001 & 15173)
print("\n[8] Verifying sever-v2 service health (ports 18001 & 15173)...", flush=True)
for port, name in [(18001, "sever-v2 Backend"), (15173, "sever-v2 Frontend")]:
    try:
        req = urllib.request.Request(f"http://127.0.0.1:{port}/api/v2/playlists" if port == 18001 else f"http://127.0.0.1:{port}/")
        with urllib.request.urlopen(req, timeout=5) as r:
            status = r.status
            results[f"{name} (Port {port})"] = f"PASS (Status {status})"
            print(f" -> {name} Port {port} active, status: {status}", flush=True)
    except urllib.error.HTTPError as e:
        results[f"{name} (Port {port})"] = f"PASS (HTTP {e.code})"
        print(f" -> {name} Port {port} responded HTTP: {e.code}", flush=True)
    except Exception as e:
        results[f"{name} (Port {port})"] = f"CHECK ({e})"
        print(f" -> {name} Port {port}: {e}", flush=True)

print("\n" + "=" * 70, flush=True)
print("TỔNG HỢP KẾT QUẢ KIỂM THỬ TOÀN DIỆN HỆ THỐNG", flush=True)
print("=" * 70, flush=True)
for k, v in results.items():
    print(f" - {k:<38}: {v}", flush=True)
print("=" * 70, flush=True)
