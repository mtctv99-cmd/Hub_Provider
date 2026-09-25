"""Combined inbound WorkerHub client. Install llama-cpp-python (CUDA wheel on T4),
 piper-tts, onnxruntime, numpy, websocket-client.

Example (models and authentication provided at runtime; never committed):
 python colab/combined_worker.py --server wss://HOST/api/llm/ws/worker \
   --gguf /models/model.gguf --models-dir /models

The Khmer adapter reads its character vocabulary and ONNX input contract from local
khmer_tts_v3.json and khmer_tts_v3.onnx files. Both connections share one loaded LLM
and exactly one session per voice, but have distinct master capacity budgets.
"""
import argparse
import base64
import io
import json
import logging
import os
import hashlib
import re
from pathlib import Path
import queue
import signal
import threading
import time
import wave

VOICE_IDS = ('ngochuyennew', 'th_TH-mms_female-medium', 'khmer_tts_v3')
LOG = logging.getLogger('combined_worker')


def make_wav(samples, sample_rate):
    """Convert floating point [-1,1] or signed 16-bit mono samples to WAV."""
    import numpy as np
    data = np.asarray(samples).reshape(-1)
    if data.dtype.kind == 'f':
        data = (np.clip(data, -1, 1) * 32767).astype('<i2')
    else:
        data = np.clip(data, -32768, 32767).astype('<i2')
    out = io.BytesIO()
    with wave.open(out, 'wb') as wav:
        wav.setnchannels(1)
        wav.setsampwidth(2)
        wav.setframerate(sample_rate)
        wav.writeframes(data.tobytes())
    return out.getvalue()


def registration(worker_id, model_id, voices, lane, token, configured_tts_models=None, tts_concurrency=None, llm_concurrency=None):
    if lane == 'llm':
        capacity = llm_concurrency if (isinstance(llm_concurrency, int) and llm_concurrency > 0) else 4
        models, caps = [model_id], ['chat', 'streaming']
    elif lane == 'tts':
        tts_models = (['piper:' + name for name in VOICE_IDS if name in voices]
                      if configured_tts_models is None else configured_tts_models)
        models = [model for model in sorted(tts_models) if model.removeprefix('piper:') in voices]
        capacity = tts_concurrency if (isinstance(tts_concurrency, int) and tts_concurrency > 0) else len(models)
        caps = ['audio_speech', 'tts']
    else:
        raise ValueError('unknown lane')
    if not models:
        raise ValueError('no models for lane')
    return dict(worker_id=worker_id + '-' + lane, model=models[0], models=models,
                capabilities=caps, max_concurrency=capacity, token=token)


class PiperVoice:
    """One cached piper-tts model/session per voice; no subprocess per utterance."""
    def __init__(self, model_path, use_cuda=False):
        from piper.voice import PiperVoice as Backend
        self.voice = Backend.load(str(model_path), use_cuda=use_cuda)

    def synthesize(self, text):
        out = io.BytesIO()
        with wave.open(out, 'wb') as wav:
            # Current piper-tts exposes synthesize_wav; older releases used
            # synthesize(text, wav_file). Both reuse the same cached model.
            if hasattr(self.voice, 'synthesize_wav'):
                self.voice.synthesize_wav(text, wav)
            else:
                self.voice.synthesize(text, wav)
        return out.getvalue()


class KhmerVoice:
    """Single ONNX session with the model's bundled Khmer character vocabulary."""
    def __init__(self, session, config, sample_rate=None):
        self.session = session
        self.vocab = {character: index for index, character in enumerate(config['vocab'])}
        self.blank_id = self.vocab.get(config.get('blank', ''))
        self.add_blank = bool(config.get('add_blank'))
        self.sample_rate = sample_rate or int(config['sample_rate'])
        self.inputs = {item.name for item in session.get_inputs()}
        if self.inputs != {'x', 'x_lengths'}:
            raise ValueError(f'unsupported Khmer ONNX inputs: {sorted(self.inputs)}')
        if self.add_blank and self.blank_id is None:
            raise ValueError('Khmer ONNX config enables blank insertion but blank symbol is missing')

    @classmethod
    def load(cls, model_path, config_path, sample_rate, providers):
        import onnxruntime as ort
        config_path = Path(config_path)
        if not config_path.is_file():
            raise FileNotFoundError(f'Khmer ONNX character config missing: {config_path}')
        with config_path.open(encoding='utf-8') as stream:
            config = json.load(stream)
        if not isinstance(config.get('vocab'), list) or not config['vocab']:
            raise ValueError('Khmer ONNX config has no character vocabulary')
        session = ort.InferenceSession(str(model_path), providers=providers)
        return cls(session, config, sample_rate)

    def synthesize(self, text):
        import numpy as np
        ids = [self.vocab[character] for character in text if character in self.vocab]
        if not ids:
            raise ValueError('Khmer text has no characters supported by the model vocabulary')
        if self.add_blank:
            interleaved = [self.blank_id]
            for token in ids:
                interleaved.extend((token, self.blank_id))
            ids = interleaved
        x = np.asarray([ids], dtype=np.int64)
        x_lengths = np.asarray([len(ids)], dtype=np.int64)
        audio = np.asarray(self.session.run(None, {'x': x, 'x_lengths': x_lengths})[0]).reshape(-1)
        if not len(audio):
            raise ValueError('Khmer model returned empty audio')
        return make_wav(audio, self.sample_rate)


class LlamaChat:
    def __init__(self, path, gpu_layers, context_size, mmproj_path=None, flash_attn=True):
        from llama_cpp import Llama
        chat_handler = None
        if mmproj_path:
            mmproj_file = Path(mmproj_path)
            if mmproj_file.is_file():
                try:
                    from llama_cpp.llama_chat_format import Llava15ChatHandler
                    chat_handler = Llava15ChatHandler(clip_model_path=str(mmproj_file), verbose=False)
                    LOG.info('Loaded vision mmproj handler: %s', mmproj_file)
                except Exception as exc:
                    LOG.warning('Failed to load mmproj vision handler (%s)', exc)
        try:
            self.model = Llama(model_path=str(path), chat_handler=chat_handler,
                               n_gpu_layers=gpu_layers, n_ctx=context_size,
                               flash_attn=flash_attn, verbose=False)
        except Exception:
            self.model = Llama(model_path=str(path), chat_handler=chat_handler,
                               n_gpu_layers=gpu_layers, n_ctx=context_size, verbose=False)

    def stream(self, messages, max_tokens, temperature):
        if not any(m.get('role') == 'system' for m in messages):
            messages = [{'role': 'system', 'content': 'You are a helpful and friendly AI assistant. Always respond politely and directly to the user.'}] + list(messages)
        return self.model.create_chat_completion(messages=messages, max_tokens=max_tokens,
                                                 temperature=temperature, stream=True,
                                                 stream_options={'include_usage': True})


class VoiceLane:
    def __init__(self, name, voice, queue_size, num_threads=1):
        self.name, self.voice = name, voice
        self.jobs = queue.Queue(maxsize=queue_size)
        self.threads = [
            threading.Thread(target=self._run, name=f'voice-{name}-{i}', daemon=True)
            for i in range(num_threads)
        ]
        for t in self.threads:
            t.start()

    def _run(self):
        while True:
            item = self.jobs.get()
            try:
                if item is None:
                    return
                msg, send = item
                try:
                    t0 = time.monotonic()
                    audio = self.voice.synthesize(msg['text'])
                    dt = (time.monotonic() - t0) * 1000
                    if not audio or len(audio) > 32 * 1024 * 1024:
                        raise ValueError('empty or oversized audio')
                    send(dict(type='audio_result', request_id=msg['request_id'], mime='audio/wav',
                              audio_base64=base64.b64encode(audio).decode('ascii'), done=True))
                    LOG.info('[TTS] Voice %s completed %s in %.1fms (%d bytes)', self.name, msg['request_id'], dt, len(audio))
                except Exception as exc:
                    LOG.error('[TTS] Voice %s failed %s: %s', self.name, msg.get('request_id'), exc)
                    send(dict(type='error', request_id=msg['request_id'], error=str(exc)))
            finally:
                self.jobs.task_done()

    def close(self):
        for _ in self.threads:
            self.jobs.put(None)
        for t in self.threads:
            t.join()


class CombinedWorker:
    def __init__(self, model_id, llm, voices, queue_size=8, configured_tts_models=None, tts_concurrency=None, llm_concurrency=4, voice_threads=1):
        if not voices or any(name not in VOICE_IDS for name in voices):
            raise ValueError('voice configuration must use supported IDs')
        self.model_id, self.llm = model_id, llm
        self.configured_tts_models = configured_tts_models
        self.tts_concurrency = tts_concurrency
        self.llm_concurrency = llm_concurrency
        self.voices = {name: VoiceLane(name, voice, queue_size, num_threads=voice_threads) for name, voice in voices.items()}
        self.llm_jobs = queue.Queue(maxsize=queue_size)
        self.llm_thread = threading.Thread(target=self._run_llm, name='llm', daemon=True)
        self.llm_thread.start()

    @property
    def registration_capacities(self):
        return {'llm': self.llm_concurrency, 'tts': self.tts_concurrency if self.tts_concurrency else len(self.voices)}

    def dispatch(self, msg, send, lane):
        typ, rid, model = msg.get('type'), msg.get('request_id'), msg.get('model')
        if typ == 'ping':
            send({'type': 'pong'})
            return
        if typ not in ('chat_request', 'tts_request') or not isinstance(rid, str) or not rid:
            return
        if lane == 'llm':
            messages = msg.get('messages')
            if model != self.model_id or typ != 'chat_request' or not isinstance(messages, list) or not messages:
                send(dict(type='error', request_id=rid, error='invalid LLM request or model'))
                return
            target = self.llm_jobs
            item = (msg, send)
        else:
            voice_id = msg.get('voice_id') or (model.removeprefix('piper:') if isinstance(model, str) else '')
            voice = self.voices.get(voice_id)
            if not voice or model != 'piper:' + voice_id or not isinstance(msg.get('text'), str) or not msg['text'].strip():
                send(dict(type='error', request_id=rid, error='invalid TTS request or voice'))
                return
            target, item = voice.jobs, (msg, send)
        try:
            target.put_nowait(item)
        except queue.Full:
            send(dict(type='error', request_id=rid, error='request queue full'))

    def _run_llm(self):
        while True:
            item = self.llm_jobs.get()
            try:
                if item is None:
                    return
                msg, send = item
                rid = msg['request_id']
                try:
                    max_tokens = msg.get('max_tokens') or 256
                    if not isinstance(max_tokens, int) or not 1 <= max_tokens <= 4096:
                        raise ValueError('max_tokens must be 1..4096')
                    temperature = msg.get('temperature')
                    if temperature is None:
                        temperature = 0.7
                    if not isinstance(temperature, (int, float)) or not 0 <= temperature <= 2:
                        raise ValueError('temperature must be 0..2')
                    parts, usage = [], {}
                    for chunk in self.llm.stream(msg['messages'], max_tokens, temperature):
                        choice = (chunk.get('choices') or [{}])[0]
                        delta = choice.get('delta', {}).get('content', '') or choice.get('text', '') or ''
                        if delta:
                            parts.append(delta)
                            if msg.get('stream'):
                                send(dict(type='token', request_id=rid, delta=delta))
                        if chunk.get('usage'):
                            usage = chunk['usage']
                    if msg.get('stream'):
                        send(dict(type='token_end', request_id=rid, usage=usage, done=True))
                    else:
                        send(dict(type='job_result', request_id=rid, text=''.join(parts), usage=usage, done=True))
                except Exception as exc:
                    send(dict(type='error', request_id=rid, error=str(exc)))
            finally:
                self.llm_jobs.task_done()

    def close(self):
        self.llm_jobs.put(None)
        self.llm_thread.join()
        for voice in self.voices.values():
            voice.close()


def model_path(root, name):
    root = Path(root)
    candidates = [root / name / (name + '.onnx'), root / (name + '.onnx')]
    if name == 'ngochuyennew':
        candidates.insert(0, root / 'ngoc_huyen' / 'ngochuyennew.onnx')
    for candidate in candidates:
        if candidate.is_file():
            return candidate
    raise FileNotFoundError(f'missing ONNX model for {name} in {root}; expected {root / name / (name + ".onnx")} or {root / (name + ".onnx")}')


def sha256_path(path):
    """Hash file bytes or directory entries with relative names and content."""
    path = Path(path)
    digest = hashlib.sha256()
    if path.is_file():
        with path.open('rb') as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                digest.update(chunk)
        return digest.hexdigest()
    if not path.is_dir():
        raise FileNotFoundError(f'asset does not exist: {path}')
    files = sorted(item for item in path.rglob('*') if item.is_file())
    if not files:
        raise ValueError(f'asset directory is empty: {path}')
    for item in files:
        digest.update(item.relative_to(path).as_posix().encode('utf-8') + b'\\0')
        with item.open('rb') as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                digest.update(chunk)
    return digest.hexdigest()


def verify_manifest(manifest_path, assets):
    """Manifest maps logical asset keys to {path, sha256}; hashes are mandatory."""
    if not manifest_path:
        return {}
    manifest_path = Path(manifest_path)
    with manifest_path.open(encoding='utf-8') as stream:
        manifest = json.load(stream)
    if not isinstance(manifest, dict):
        raise ValueError('asset manifest must be a JSON object')
    results = {}
    for key, item in manifest.items():
        if key not in assets or not isinstance(item, dict):
            raise ValueError(f'unknown or malformed asset manifest entry: {key}')
        expected_path = item.get('path')
        actual_path = Path(assets[key]).resolve()
        if expected_path and Path(expected_path).resolve() != actual_path:
            raise ValueError(f'asset path mismatch for {key}: expected {Path(expected_path).resolve()}, got {actual_path}')
        expected = item.get('sha256', '')
        if not re.fullmatch(r'[0-9a-fA-F]{64}', expected):
            raise ValueError(f'asset manifest {key} requires 64-hex sha256')
        actual = sha256_path(assets[key])
        if actual.lower() != expected.lower():
            raise ValueError(f'asset hash mismatch for {key}: expected {expected.lower()}, got {actual}')
        results[key] = {'path': str(Path(assets[key]).resolve()), 'sha256': actual}
    return results


def configured_models(config_path):
    """Read configured models from models.yaml (PyYAML required).
    Returns (llm_models, tts_models)."""
    try:
        import yaml
    except ImportError as exc:
        raise RuntimeError('PyYAML required to read --config; install pyyaml') from exc
    with Path(config_path).open(encoding='utf-8') as stream:
        config = yaml.safe_load(stream) or {}
    configured = config.get('models', {})
    llm = sorted(model_id for model_id, spec in configured.items()
                 if isinstance(spec, dict) and spec.get('provider') == 'google_colab'
                 and 'chat' in (spec.get('capabilities') or []))
    required_voice_models = {'piper:' + name for name in VOICE_IDS}
    tts = sorted(model_id for model_id, spec in configured.items()
                 if isinstance(spec, dict) and model_id in required_voice_models
                 and 'audio_speech' in (spec.get('capabilities') or []))
    return llm, tts


def configured_llm_models(config_path):
    """Read configured google_colab chat IDs from models.yaml."""
    return configured_models(config_path)[0]


def resolve_model_id(config_path, requested):
    models = configured_llm_models(config_path)
    if not models:
        raise ValueError(f'no provider=google_colab model with chat capability in {config_path}')
    if requested:
        if requested not in models:
            raise ValueError(f'model {requested!r} is not configured as google_colab chat in {config_path}; available: {models}')
        return requested
    if len(models) != 1:
        raise ValueError(f'multiple google_colab chat models in {config_path}; select --model-id from {models}')
    return models[0]


def load_voices(root, khmer_rate=22050, providers=None, khmer_onnx=None, khmer_config=None, use_cuda=False):
    root = Path(root)
    if providers is None:
        providers = ['CUDAExecutionProvider', 'CPUExecutionProvider'] if use_cuda else ['CPUExecutionProvider']
    voices = {}
    for name in VOICE_IDS[:2]:
        path = model_path(root, name)
        sidecar = Path(str(path) + '.json')
        if not sidecar.is_file():
            alt = path.with_suffix('.json')
            if alt.is_file():
                sidecar = alt
            else:
                raise FileNotFoundError(f'Piper config sidecar missing for {name}: {path}.json')
        voices[name] = PiperVoice(path, use_cuda=use_cuda)
    khmer_onnx_path = Path(khmer_onnx) if khmer_onnx else model_path(root, 'khmer_tts_v3')
    if not khmer_onnx_path.is_file():
        raise FileNotFoundError(f'Khmer ONNX file missing: {khmer_onnx_path}')
    if khmer_config:
        khmer_cfg_path = Path(khmer_config)
    else:
        candidates = [
            root / 'khmer_tts_v3.json',
            khmer_onnx_path.with_suffix('.json'),
            root / 'khmer_tts_v3' / 'khmer_tts_v3.json',
        ]
        khmer_cfg_path = next((c for c in candidates if c.is_file()), candidates[0])
    voices['khmer_tts_v3'] = KhmerVoice.load(khmer_onnx_path, khmer_cfg_path, khmer_rate, providers)
    return voices


def connect_loop(server, token, worker_id, worker, lane, stop, retry=3):
    """One connection per capacity class; socket writes serialized including heartbeats."""
    import websocket
    from urllib.parse import parse_qsl, urlencode, urlsplit, urlunsplit
    parsed = urlsplit(server)
    if parsed.scheme not in ('ws', 'wss') or not parsed.netloc or parsed.path != '/api/llm/ws/worker':
        raise ValueError('server must be ws(s)://host/api/llm/ws/worker')
    query = dict(parse_qsl(parsed.query))
    query['token'] = token
    url = urlunsplit((parsed.scheme, parsed.netloc, parsed.path, urlencode(query), ''))
    reg = registration(worker_id, worker.model_id, worker.voices, lane, token,
                       configured_tts_models=worker.configured_tts_models,
                       tts_concurrency=getattr(worker, 'tts_concurrency', None),
                       llm_concurrency=getattr(worker, 'llm_concurrency', 4))
    while not stop.is_set():
        sock = None
        try:
            sock = websocket.create_connection(url, timeout=15, header={'X-Worker-Token': token},
                                               enable_multithread=True)
            write_lock = threading.Lock()
            def send(message):
                with write_lock:
                    try:
                        sock.send(json.dumps(message, ensure_ascii=False))
                    except Exception as exc:
                        LOG.warning('%s response dropped: %s', lane, exc)
            send(reg)
            ack = json.loads(sock.recv())
            if ack.get('type') != 'registered':
                raise RuntimeError('registration rejected')
            LOG.info('%s registered with %s models', lane, len(reg['models']))
            sock.settimeout(1)
            while not stop.is_set():
                try:
                    msg = json.loads(sock.recv())
                except websocket.WebSocketTimeoutException:
                    continue
                if isinstance(msg, dict):
                    worker.dispatch(msg, send, lane)
        except Exception as exc:
            if not stop.is_set():
                LOG.warning('%s disconnected (%s); retrying', lane, type(exc).__name__)
                stop.wait(retry)
        finally:
            if sock:
                sock.close()


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--server', required=True, help='WorkerHub websocket URL')
    parser.add_argument('--worker-id', default='colab-combined')
    parser.add_argument('--model-id', help='Must match a google_colab chat model in --config; auto-selected only when exactly one exists')
    parser.add_argument('--config', type=Path, default=Path('config/models.yaml'))
    parser.add_argument('--gguf', type=Path, required=True)
    parser.add_argument('--models-dir', type=Path, required=True, help='Must contain ngochuyennew and th_TH-mms_female-medium ONNX plus Khmer ONNX/config')
    parser.add_argument('--khmer-onnx', type=Path, help='Optional explicit Khmer ONNX path; otherwise resolved under --models-dir')
    parser.add_argument('--khmer-config', type=Path, help='Optional explicit khmer_tts_v3.json path')
    parser.add_argument('--asset-manifest', type=Path, help='Optional JSON map of asset keys to path and expected SHA-256')
    parser.add_argument('--khmer-sample-rate', type=int, default=22050)
    parser.add_argument('--gpu-layers', type=int, default=-1, help='-1 for all layers, 0 for CPU')
    parser.add_argument('--context-size', type=int, default=32768, help='Context window size in tokens (default: 32768 / 32k)')
    parser.add_argument('--llm-concurrency', type=int, default=4, help='Max concurrent LLM requests (default: 4)')
    parser.add_argument('--flash-attn', action='store_true', default=True, help='Enable Flash Attention for memory-efficient 32k context')
    parser.add_argument('--mmproj', type=Path, help='Optional multimodal projector GGUF (e.g. gemma-4-E2B-it-mmproj-BF16.gguf)')
    parser.add_argument('--tts-concurrency', type=int, default=20, help='Max TTS concurrency for worker registration (default: 20)')
    parser.add_argument('--tts-gpu', action='store_true', help='Use CUDA GPU for Piper and Khmer TTS models')
    parser.add_argument('--queue-size', type=int, default=32, help='Internal job queue size per voice (default: 32)')
    args = parser.parse_args(argv)
    token = os.environ.get('WORKER_TOKEN', '')
    if not token:
        parser.error('WORKER_TOKEN environment variable required')
    if args.queue_size < 1 or args.khmer_sample_rate < 1:
        parser.error('queue-size and khmer-sample-rate must be positive')
    if not args.config.is_file():
        parser.error(f'config file not found: {args.config}; supply --config config/models.yaml')
    try:
        model_id = resolve_model_id(args.config, args.model_id)
        _, configured_tts = configured_models(args.config)
    except Exception as exc:
        parser.error(str(exc))
    if not args.gguf.is_file():
        parser.error(f'LLM GGUF file missing: {args.gguf}')
    if not args.models_dir.is_dir():
        parser.error(f'models directory missing: {args.models_dir}')
    try:
        khmer_onnx_path = Path(args.khmer_onnx) if args.khmer_onnx else model_path(args.models_dir, 'khmer_tts_v3')
        khmer_config_path = Path(args.khmer_config) if args.khmer_config else args.models_dir / 'khmer_tts_v3.json'
        assets = {
            'gguf': args.gguf,
            'ngochuyennew': model_path(args.models_dir, 'ngochuyennew'),
            'th_TH-mms_female-medium': model_path(args.models_dir, 'th_TH-mms_female-medium'),
            'khmer_tts_v3': khmer_onnx_path,
            'khmer_tts_v3_config': khmer_config_path,
        }
        if args.mmproj:
            assets['mmproj'] = Path(args.mmproj)
        verified = verify_manifest(args.asset_manifest, assets)
        if verified:
            LOG.info('Verified %d assets against manifest', len(verified))
    except Exception as exc:
        parser.error(f'asset verification failed: {exc}')
    logging.basicConfig(level=logging.INFO)
    voices = load_voices(args.models_dir, args.khmer_sample_rate,
                         ['CUDAExecutionProvider', 'CPUExecutionProvider'] if args.tts_gpu else ['CPUExecutionProvider'],
                         args.khmer_onnx, args.khmer_config,
                         use_cuda=args.tts_gpu)
    configured_missing = [model for model in configured_tts if model.removeprefix('piper:') not in voices]
    if configured_missing:
        parser.error(f'configured TTS models lack loaded assets: {configured_missing}; required voices: {sorted(set(model.removeprefix("piper:") for model in configured_missing))}')
    worker = CombinedWorker(model_id, LlamaChat(args.gguf, args.gpu_layers, args.context_size, mmproj_path=args.mmproj, flash_attn=args.flash_attn),
                            voices, args.queue_size, configured_tts_models=configured_tts,
                            tts_concurrency=args.tts_concurrency, llm_concurrency=args.llm_concurrency)
    stop = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())
    threads = [threading.Thread(target=connect_loop,
                                args=(args.server, token, args.worker_id, worker, lane, stop), daemon=True)
               for lane in ('llm', 'tts')]
    for thread in threads:
        thread.start()
    try:
        while not stop.wait(.5):
            pass
    finally:
        worker.close()


if __name__ == '__main__':
    main()
