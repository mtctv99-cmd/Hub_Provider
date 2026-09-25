"""Colab bridge worker for local Sever_provider.

Connects to Sever_provider WorkerHub via WebSocket and advertises gemma-4-colab.
Forwards inference requests directly to the live Colab T4 GPU instance,
streaming tokens back through the WebSocket in real time.
"""
import argparse
import json
import logging
import os
import signal
import sys
import threading
import time
import urllib.request
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

LOG = logging.getLogger('colab_bridge_worker')

COLAB_UPSTREAM_URL = 'http://192.168.1.117:8095/v1/chat/completions'
COLAB_UPSTREAM_MODEL = 'gemma-4-E2B-it-ultra-uncensored-heretic-Q4_K_M'
COLAB_UPSTREAM_KEY = 'omni-secret-key-123'


def format_colab_messages(messages):
    has_system = any(m.get('role') == 'system' for m in messages)
    clean_messages = []
    if not has_system:
        clean_messages.append({'role': 'system', 'content': 'You are a helpful and friendly AI assistant. Always respond politely and directly to the user.'})
    for m in messages:
        role = m.get('role', 'user')
        content = m.get('content', '')
        if isinstance(content, str) and content.strip():
            clean_messages.append({'role': role, 'content': content})
    return clean_messages


def stream_from_colab(messages, max_tokens, temperature):
    payload = {
        'model': COLAB_UPSTREAM_MODEL,
        'messages': format_colab_messages(messages),
        'temperature': temperature,
        'max_tokens': max_tokens,
        'stream': True,
    }
    req = urllib.request.Request(
        COLAB_UPSTREAM_URL,
        data=json.dumps(payload).encode('utf-8'),
        headers={
            'Content-Type': 'application/json',
            'Authorization': f'Bearer {COLAB_UPSTREAM_KEY}',
        },
    )
    with urllib.request.urlopen(req, timeout=60) as resp:
        for line in resp:
            line = line.decode('utf-8', errors='ignore').strip()
            if line.startswith('data: '):
                chunk_str = line[6:]
                if chunk_str == '[DONE]':
                    break
                try:
                    data = json.loads(chunk_str)
                    delta = data['choices'][0].get('delta', {}).get('content', '')
                    if delta:
                        yield delta
                except Exception:
                    pass


def run_bridge_worker(server_url, token, worker_id, stop):
    import websocket

    parsed = urlsplit(server_url)
    query = dict(parse_qsl(parsed.query))
    if token:
        query['token'] = token
    url = urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urlencode(query), ''))

    reg = {
        'worker_id': worker_id,
        'model': 'gemma-4-colab',
        'models': ['gemma-4-colab', COLAB_UPSTREAM_MODEL],
        'capabilities': ['chat', 'streaming'],
        'max_concurrency': 4,
        'token': token or '',
    }

    while not stop.is_set():
        sock = None
        try:
            headers = {'X-Worker-Token': token} if token else {}
            sock = websocket.create_connection(url, timeout=15, header=headers, enable_multithread=True)
            write_lock = threading.Lock()

            def send(msg):
                with write_lock:
                    try:
                        sock.send(json.dumps(msg, ensure_ascii=False))
                    except Exception:
                        pass

            send(reg)
            ack_raw = sock.recv()
            LOG.info('Registered Colab worker on master: %s', ack_raw)

            sock.settimeout(1.0)
            while not stop.is_set():
                try:
                    raw = sock.recv()
                except websocket.WebSocketTimeoutException:
                    continue
                if not raw:
                    break
                try:
                    msg = json.loads(raw)
                except Exception:
                    continue

                if msg.get('type') == 'ping':
                    send({'type': 'pong'})
                elif msg.get('type') == 'chat_request':
                    rid = msg.get('request_id')
                    messages = msg.get('messages', [])
                    max_tokens = msg.get('max_tokens') or 256
                    if max_tokens <= 0:
                        max_tokens = 256
                    temperature = msg.get('temperature')
                    if temperature is None or temperature <= 0.0:
                        temperature = 0.7
                    is_stream = msg.get('stream', False)

                    def handle_chat(req_id, msgs, m_tokens, temp, stream_mode):
                        try:
                            full_text = []
                            for delta in stream_from_colab(msgs, m_tokens, temp):
                                full_text.append(delta)
                                if stream_mode:
                                    send({'type': 'token', 'request_id': req_id, 'delta': delta})
                            joined = ''.join(full_text).strip()
                            if stream_mode:
                                send({
                                    'type': 'token_end',
                                    'request_id': req_id,
                                    'done': True,
                                    'usage': {
                                        'prompt_tokens': 32,
                                        'completion_tokens': len(full_text),
                                        'total_tokens': 32 + len(full_text),
                                    },
                                })
                            else:
                                send({
                                    'type': 'job_result',
                                    'request_id': req_id,
                                    'text': joined,
                                    'done': True,
                                    'usage': {
                                        'prompt_tokens': 32,
                                        'completion_tokens': len(full_text),
                                        'total_tokens': 32 + len(full_text),
                                    },
                                })
                            LOG.info('Completed Colab chat %s (%d chars)', req_id, len(joined))
                        except Exception as exc:
                            LOG.error('Colab inference error on %s: %s', req_id, exc)
                            send({'type': 'error', 'request_id': req_id, 'error': str(exc)})

                    threading.Thread(target=handle_chat, args=(rid, messages, max_tokens, temperature, is_stream), daemon=True).start()
        except Exception as exc:
            if not stop.is_set():
                LOG.warning('Bridge connection error (%s); retrying...', exc)
                stop.wait(3.0)
        finally:
            if sock:
                try:
                    sock.close()
                except Exception:
                    pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server', default='ws://127.0.0.1:18080/api/llm/ws/worker')
    parser.add_argument('--token', default=os.environ.get('WORKER_TOKEN') or os.environ.get('COLAB_WORKER_KEY') or 'omni-secret-key-123')
    parser.add_argument('--worker-id', default='colab-t4-live-bridge')
    args = parser.parse_args()

    logging.basicConfig(level=logging.INFO, format='%(asctime)s [%(levelname)s] %(message)s')
    LOG.info('Starting Colab T4 Live Bridge -> %s (model: gemma-4-colab)', args.server)

    stop = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())

    run_bridge_worker(args.server, args.token, args.worker_id, stop)


if __name__ == '__main__':
    main()
