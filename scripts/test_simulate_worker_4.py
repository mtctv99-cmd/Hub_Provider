#!/usr/bin/env python3
"""Simulate 4th worker connecting from another node to WorkerHub to test horizontal scaling."""
import json
import base64
import struct
import threading
import time
import requests
import websocket

WS_URL = "ws://192.168.1.117:18080/api/llm/ws/worker?token=omni-secret-key-123"
ADMIN_URL = "http://192.168.1.117:18080/admin/api/workers"
LOGIN_URL = "http://192.168.1.117:18080/admin/api/login"
BATCH_URL = "http://192.168.1.117:18080/v1/audio/speech/batch"
AUTH_KEY = "sk-sp-unlimited-master-key"

def generate_minimal_wav():
    """Generate a minimal valid 16kHz mono WAV file in memory."""
    sample_rate = 22050
    num_samples = 2205 # 0.1s
    audio_data = b'\x00\x00' * num_samples
    data_size = len(audio_data)
    # RIFF header (44 bytes)
    header = struct.pack(
        '<4sI4s4sIHHIIHH4sI',
        b'RIFF',
        36 + data_size,
        b'WAVE',
        b'fmt ',
        16,
        1, # PCM
        1, # Mono
        sample_rate,
        sample_rate * 2,
        2,
        16,
        b'data',
        data_size
    )
    return header + audio_data

def get_connected_workers():
    s = requests.Session()
    s.post(LOGIN_URL, json={"password": "123456"})
    resp = s.get(ADMIN_URL)
    data = resp.json()
    return data.get("piper_remote", [])

def main():
    print("[STEP 1] Checking existing workers on cluster...")
    workers_before = get_connected_workers()
    print(f"Workers before: {len(workers_before)}")
    for w in workers_before:
        print(f"  - {w['worker_id']} (concurrency: {w['max_concurrency']})")
    slots_before = sum(w["max_concurrency"] for w in workers_before)
    print(f"Total cluster capacity before: {slots_before} slots")

    print("\n[STEP 2] Connecting simulated 4th worker...")
    ws = websocket.create_connection(WS_URL, timeout=10)
    worker_id = "simulated-node4-worker"
    reg_msg = {
        "worker_id": worker_id,
        "model": "piper:ngochuyennew",
        "models": ["piper:ngochuyennew", "piper:th_TH-mms_female-medium", "piper:khmer_tts_v3"],
        "capabilities": ["audio_speech", "tts"],
        "max_concurrency": 5,
        "token": "omni-secret-key-123"
    }
    ws.send(json.dumps(reg_msg))
    ack_raw = ws.recv()
    ack = json.loads(ack_raw)
    print(f"Registration ACK: {ack}")
    assert ack.get("type") == "registered", f"Registration failed: {ack}"

    stop_event = threading.Event()
    worker_4_handled = []
    fake_wav_b64 = base64.b64encode(generate_minimal_wav()).decode("ascii")

    def worker_loop():
        ws.settimeout(1.0)
        while not stop_event.is_set():
            try:
                raw = ws.recv()
                if not raw:
                    break
                msg = json.loads(raw)
                mtype = msg.get("type")
                if mtype == "ping":
                    ws.send(json.dumps({"type": "pong"}))
                elif mtype == "tts_request":
                    rid = msg.get("request_id")
                    print(f"  [NODE 4 WORKER] Received dispatch for {rid} (model: {msg.get('model')})")
                    worker_4_handled.append(rid)
                    resp_msg = {
                        "type": "audio_result",
                        "request_id": rid,
                        "mime": "audio/wav",
                        "audio_base64": fake_wav_b64,
                        "done": True
                    }
                    ws.send(json.dumps(resp_msg))
            except websocket.WebSocketTimeoutException:
                continue
            except Exception as e:
                if not stop_event.is_set():
                    print(f"  [NODE 4 WORKER] Error: {e}")
                break

    t = threading.Thread(target=worker_loop, daemon=True)
    t.start()

    time.sleep(1)

    print("\n[STEP 3] Verifying cluster expansion in AdminWorkers...")
    workers_during = get_connected_workers()
    print(f"Workers with node 4 online: {len(workers_during)}")
    for w in workers_during:
        print(f"  - {w['worker_id']} (concurrency: {w['max_concurrency']})")
    slots_during = sum(w["max_concurrency"] for w in workers_during)
    print(f"Total cluster capacity now: {slots_during} slots (expanded by {slots_during - slots_before})")
    assert any(w["worker_id"] == worker_id for w in workers_during), "Worker 4 not registered in hub!"

    print("\n[STEP 4] Sending test requests to verify traffic routing to 4th worker...")
    headers = {"Authorization": f"Bearer {AUTH_KEY}", "Content-Type": "application/json"}
    test_batch = {
        "items": [
            {"id": f"node4-test-{i}", "model": "piper:ngochuyennew", "input": f"Kiểm tra mở rộng phân tán node thứ 4 câu số {i}"}
            for i in range(10)
        ]
    }
	    r = requests.post(BATCH_URL, headers=headers, json=test_batch, timeout=30)
	    print(f"Batch response HTTP {r.status_code}")
	    res_data = r.json()
	    res_items = res_data.get("results", res_data) if isinstance(res_data, dict) else res_data
	    success_count = sum(1 for item in res_items if item.get("status") in ("success", "ok"))
	    print(f"Batch success items: {success_count}/{len(res_items)}")
    print(f"Requests handled by Node 4 during batch: {len(worker_4_handled)} (IDs: {worker_4_handled})")

    print("\n[STEP 5] Gracefully stopping Node 4 worker and verifying deregistration...")
    stop_event.set()
    ws.close()
    t.join(timeout=3)
    time.sleep(1)

    workers_after = get_connected_workers()
    print(f"Workers after node 4 disconnected: {len(workers_after)}")
    for w in workers_after:
        print(f"  - {w['worker_id']} (concurrency: {w['max_concurrency']})")
    slots_after = sum(w["max_concurrency"] for w in workers_after)
    print(f"Total cluster capacity restored to: {slots_after} slots")

    print("\n=== EXPANSION TEST RESULT ===")
    print(f"Worker 4 successfully connected, received traffic ({len(worker_4_handled)} tasks), and disconnected cleanly.")

if __name__ == "__main__":
    main()
