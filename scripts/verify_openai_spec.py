import json
import urllib.request
import urllib.error
from openai import OpenAI

API_BASE = "http://127.0.0.1:18080/v1"
VALID_KEY = "sk-sp-test1234567890abcdef"
INVALID_KEY = "sk-sp-invalidkey999"

print("=" * 65)
print("KIỂM TRA ĐỐI SOÁT CHUẨN OPENAI-COMPATIBLE TRÊN SEVER_PROVIDER")
print("=" * 65)

results = {}

# 1. AUTHENTICATION TEST
print("\n[1] Kiểm tra Authentication (Bearer Token & Status 401)...")
req = urllib.request.Request(
    f"{API_BASE}/models",
    headers={"Authorization": f"Bearer {INVALID_KEY}"}
)
try:
    with urllib.request.urlopen(req) as resp:
        results["Auth Invalid Key"] = "FAIL (Expected 401, got 200)"
except urllib.error.HTTPError as e:
    body = json.loads(e.read().decode())
    is_valid = (
        e.code == 401 and
        body.get("error", {}).get("code") == "invalid_api_key" and
        body.get("error", {}).get("type") == "authentication_error"
    )
    results["Auth Invalid Key (401)"] = "PASS" if is_valid else f"FAIL ({e.code}, {body})"
    print(f" -> Status: {e.code}, Code: {body.get('error', {}).get('code')}")

# 2. MODEL REGISTRY SCHEMA TEST
print("\n[2] Kiểm tra Response Schema: GET /v1/models...")
client = OpenAI(base_url=API_BASE, api_key=VALID_KEY)
models_resp = client.models.list()
first_model = models_resp.data[0]
has_fields = (
    hasattr(first_model, "id") and
    hasattr(first_model, "object") and first_model.object == "model" and
    hasattr(first_model, "created") and isinstance(first_model.created, int) and
    hasattr(first_model, "owned_by")
)
results["GET /v1/models Schema"] = "PASS" if has_fields else "FAIL"
print(f" -> Models count: {len(models_resp.data)}, First model: {first_model.id} (created: {first_model.created}, owned_by: {first_model.owned_by})")

# 3. REQUEST SCHEMA TEST (Multimodal Content Array)
print("\n[3] Kiểm tra Multimodal Array Content [{\"type\":\"text\"}]...")
req_multi = urllib.request.Request(
    f"{API_BASE}/chat/completions",
	data=json.dumps({
		"model": "gemini-3.8-flash",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "Say hello in 1 word"}
			]
		}]
	}).encode(),
    headers={
        "Content-Type": "application/json",
        "Authorization": f"Bearer {VALID_KEY}"
    }
)
try:
    with urllib.request.urlopen(req_multi, timeout=30) as resp:
        data = json.loads(resp.read().decode())
        content = data["choices"][0]["message"]["content"].strip()
        results["Multimodal Array Content"] = "PASS"
        print(f" -> Response: {content}")
except Exception as e:
    results["Multimodal Array Content"] = f"FAIL: {e}"
    print(f" -> Lỗi: {e}")

# 4. CHAT COMPLETIONS NON-STREAM RESPONSE SCHEMA
print("\n[4] Kiểm tra Chat Non-Stream Response Schema (id, object, choices, usage)...")
chat_resp = client.chat.completions.create(
    model="gemini-3.8-flash",
    messages=[{"role": "user", "content": "1+1=?"}],
    temperature=0.7,
    max_tokens=50
)
has_chat_schema = (
    chat_resp.id.startswith("chatcmpl-") or chat_resp.id.startswith("gemini-")
) and chat_resp.object == "chat.completion" and len(chat_resp.choices) > 0 and chat_resp.choices[0].finish_reason == "stop"
results["Chat Non-Stream Schema"] = "PASS" if has_chat_schema else "FAIL"
print(f" -> ID: {chat_resp.id}, Object: {chat_resp.object}, Content: {repr(chat_resp.choices[0].message.content)}")

# 5. SSE STREAMING COMPLIANCE TEST
print("\n[5] Kiểm tra SSE Streaming (text/event-stream, [DONE])...")
stream_resp = client.chat.completions.create(
    model="gemini-3.8-flash",
    messages=[{"role": "user", "content": "Count from 1 to 3"}],
    stream=True
)
chunks_count = 0
deltas = []
for c in stream_resp:
    chunks_count += 1
    if c.choices and c.choices[0].delta.content:
        deltas.append(c.choices[0].delta.content)
results["SSE Streaming (OpenAI SDK)"] = "PASS" if chunks_count > 0 else "FAIL"
print(f" -> Chunks received: {chunks_count}, Text: {repr(''.join(deltas)[:40])}")

# 6. ERROR HANDLING & HTTP STATUS TEST (404 Model Not Found)
print("\n[6] Kiểm tra Model Không Tồn Tại -> 404 model_not_found...")
try:
    client.chat.completions.create(
        model="non-existent-model-xyz",
        messages=[{"role": "user", "content": "Hello"}]
    )
    results["Model Not Found (404)"] = "FAIL (Expected 404)"
except Exception as e:
    is_404 = "404" in str(e) and "model_not_found" in str(e)
    results["Model Not Found (404)"] = "PASS" if is_404 else f"FAIL ({e})"
    print(f" -> SDK caught error correctly: {e}")

# 7. AUDIO SPEECH TTS COMPLIANCE
print("\n[7] Kiểm tra Audio Speech (openai.audio.speech.create)...")
try:
    speech_resp = client.audio.speech.create(
        model="piper:ngochuyennew",
        voice="ngochuyennew",
        input="Kiểm tra âm thanh chuẩn OpenAI."
    )
    audio_data = speech_resp.content
    results["Audio Speech Endpoint"] = "PASS" if len(audio_data) > 1000 else "FAIL"
    print(f" -> Audio received: {len(audio_data)} bytes")
except Exception as e:
    results["Audio Speech Endpoint"] = f"FAIL: {e}"
    print(f" -> Lỗi: {e}")

# 8. TOOL CALLING / FUNCTION CALLING TEST
print("\n[8] Kiểm tra Tool / Function Calling (Gemini Web RPC)...")
try:
    tools_spec = [{
        "type": "function",
        "function": {
            "name": "get_current_weather",
            "description": "Lấy thông tin thời tiết theo thành phố",
            "parameters": {
                "type": "object",
                "properties": {"city": {"type": "string"}},
                "required": ["city"]
            }
        }
    }]
    tool_resp = client.chat.completions.create(
        model="gemini-3.8-flash",
        messages=[{"role": "user", "content": "Thời tiết ở Đà Nẵng thế nào? Hãy gọi tool."}],
        tools=tools_spec,
        tool_choice="auto"
    )
    choice = tool_resp.choices[0]
    has_tc = choice.finish_reason == "tool_calls" and len(choice.message.tool_calls or []) > 0
    tc = choice.message.tool_calls[0] if has_tc else None
    results["Tool / Function Calling"] = "PASS" if has_tc else f"FAIL ({choice.finish_reason})"
    if tc:
        print(f" -> Finish Reason: {choice.finish_reason}, Tool: {tc.function.name}, Args: {tc.function.arguments}")
except Exception as e:
    results["Tool / Function Calling"] = f"FAIL: {e}"
    print(f" -> Lỗi: {e}")

# SUMMARY
print("\n" + "=" * 65)
print("BẢNG TỔNG HỢP KIỂM TRA ĐỐI SOÁT OPENAI SPEC")
print("=" * 65)
for k, v in results.items():
    print(f" - {k:<38}: {v}")
print("=" * 65)
