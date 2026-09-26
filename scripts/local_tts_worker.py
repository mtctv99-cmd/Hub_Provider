"""Local in-memory TTS worker for Sever_provider.

Preloads Piper and Khmer ONNX models into RAM once, maintaining persistent
sessions across synthesis requests to eliminate per-utterance process spawn
and disk I/O overhead. Connects directly to Sever_provider WorkerHub WebSocket.

Usage:
  python scripts/local_tts_worker.py --server ws://127.0.0.1:18080/api/llm/ws/worker --models-dir models
"""
import argparse
import base64
import json
import logging
import os
import signal
import sys
import threading
import time
from pathlib import Path
from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit

# Add repo root and colab directory to path
REPO_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO_ROOT))
sys.path.insert(0, str(REPO_ROOT / 'colab'))

from combined_worker import (
    VOICE_IDS,
    KhmerVoice,
    PiperVoice,
    VoiceLane,
    load_voices,
    make_wav,
    model_path,
)

LOG = logging.getLogger('local_tts_worker')


class LocalTTSWorker:
    """Manages in-memory voice lanes and serializes per-voice synthesis."""
    def __init__(self, voices, queue_size=32, voice_threads=3):
        self.voices = {name: VoiceLane(name, voice, queue_size, num_threads=voice_threads) for name, voice in voices.items()}

    def dispatch(self, msg, send):
        typ, rid, model = msg.get('type'), msg.get('request_id'), msg.get('model')
        if typ == 'ping':
            send({'type': 'pong'})
            return
        if typ != 'tts_request' or not isinstance(rid, str) or not rid:
            return
        voice_id = msg.get('voice_id') or (model.removeprefix('piper:') if isinstance(model, str) else '')
        voice = self.voices.get(voice_id)
        if not voice or (isinstance(model, str) and model != 'piper:' + voice_id):
            send({'type': 'error', 'request_id': rid, 'error': f'unsupported voice: {voice_id}'})
            return
        text = msg.get('text')
        if not isinstance(text, str) or not text.strip():
            send({'type': 'error', 'request_id': rid, 'error': 'empty text'})
            return
        LOG.info('[DISPATCH] Received %s for %s (%d chars)', rid, voice_id, len(text))
        try:
            voice.jobs.put_nowait((msg, send))
        except Exception:
            LOG.error('[DISPATCH] Queue full for %s on %s', voice_id, rid)
            send({'type': 'error', 'request_id': rid, 'error': 'voice queue full'})

    def close(self):
        for lane in self.voices.values():
            lane.close()


def run_worker_loop(server_url, token, worker_id, worker, stop, concurrency=10, retry=3):
    import websocket

    parsed = urlsplit(server_url)
    if parsed.scheme not in ('ws', 'wss') or not parsed.netloc:
        raise ValueError('server must be ws(s)://host/api/llm/ws/worker')

    query = dict(parse_qsl(parsed.query))
    if token:
        query['token'] = token
    url = urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urlencode(query), ''))

    models = sorted('piper:' + name for name in worker.voices)
    reg = {
        'worker_id': worker_id,
        'model': models[0],
        'models': models,
        'capabilities': ['audio_speech', 'tts'],
        'max_concurrency': concurrency,
        'token': token or '',
    }

    LOG.info('Registered voices in RAM: %s', list(worker.voices.keys()))

    while not stop.is_set():
        sock = None
        try:
            headers = {'X-Worker-Token': token} if token else {}
            sock = websocket.create_connection(url, timeout=10, header=headers, enable_multithread=True)
            write_lock = threading.Lock()

            def send(message):
                with write_lock:
                    try:
                        sock.send(json.dumps(message, ensure_ascii=False))
                    except Exception as exc:
                        LOG.error('[SOCKET] send error on %s: %s', message.get('request_id'), exc)

            send(reg)
            ack_raw = sock.recv()
            ack = json.loads(ack_raw) if ack_raw else {}
            if ack.get('type') != 'registered':
                LOG.error('Registration rejected by server: %s', ack)
                stop.wait(retry)
                continue

            LOG.info('Worker %s connected and active on %s (models: %s)', worker_id, server_url, models)
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
                    if isinstance(msg, dict):
                        worker.dispatch(msg, send)
                except json.JSONDecodeError:
                    continue
        except Exception as exc:
            if not stop.is_set():
                LOG.warning('Connection error (%s); retrying in %ss...', type(exc).__name__, retry)
                stop.wait(retry)
        finally:
            if sock:
                try:
                    sock.close()
                except Exception:
                    pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server', default='ws://127.0.0.1:18080/api/llm/ws/worker', help='WorkerHub WebSocket URL')
    parser.add_argument('--worker-id', default='local-inmemory-tts', help='Worker identifier')
    parser.add_argument('--token', default=os.environ.get('WORKER_TOKEN') or os.environ.get('COLAB_WORKER_KEY') or '', help='WorkerHub auth token')
    parser.add_argument('--models-dir', type=Path, default=Path('models'), help='Directory containing voice models')
    parser.add_argument('--khmer-onnx', type=Path, help='Path to khmer_tts_v3.onnx')
    parser.add_argument('--khmer-config', type=Path, help='Path to khmer_tts_v3.json')
    parser.add_argument('--queue-size', type=int, default=32, help='Internal job queue size per voice (default: 32)')
    parser.add_argument('--khmer-rate', type=int, default=22050)
    parser.add_argument('--concurrency', type=int, default=20, help='Max worker TTS concurrency (default: 20)')
    parser.add_argument('--threads-per-voice', type=int, default=3, help='Parallel threads per voice lane (default: 3)')
    parser.add_argument('--gpu', action='store_true', help='Use CUDA GPU for Piper and Khmer TTS')
    parser.add_argument('--device-id', type=int, default=None, help='CUDA device ID (0 for GPU 0, 1 for GPU 1)')
    args = parser.parse_args()

    if args.device_id is not None:
        os.environ['CUDA_VISIBLE_DEVICES'] = str(args.device_id)
        os.environ['NVIDIA_VISIBLE_DEVICES'] = str(args.device_id)

    logging.basicConfig(level=logging.INFO, format='%(asctime)s [%(levelname)s] %(message)s')

    if not args.models_dir.is_dir():
        parser.error(f'models directory missing: {args.models_dir}')

    if args.gpu:
        LOG.info('CUDA GPU acceleration enabled (device_id: %s)', args.device_id if args.device_id is not None else os.environ.get('CUDA_VISIBLE_DEVICES', '0'))

    LOG.info('Loading models into memory (cold start)...')
    t0 = time.monotonic()
    providers = ['CUDAExecutionProvider', 'CPUExecutionProvider'] if args.gpu else ['CPUExecutionProvider']
    voices = load_voices(args.models_dir, khmer_rate=args.khmer_rate,
                        providers=providers,
                        khmer_onnx=args.khmer_onnx, khmer_config=args.khmer_config,
                        use_cuda=args.gpu)
    dt = (time.monotonic() - t0) * 1000
    LOG.info('Loaded %d models in %.1fms. Models stay in RAM for zero-latency warm synthesis.', len(voices), dt)

    worker = LocalTTSWorker(voices, queue_size=args.queue_size, voice_threads=args.threads_per_voice)
    stop = threading.Event()

    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())

    try:
        run_worker_loop(args.server, args.token, args.worker_id, worker, stop, concurrency=args.concurrency)
    finally:
        worker.close()
        LOG.info('Local TTS worker stopped.')


if __name__ == '__main__':
    main()
