"""Verification runner to benchmark VRAM and latency under concurrent AI+TTS load on T4.

Usage:
  python colab/verify_combined.py --gguf /path/to/model.gguf \
      --models-dir /path/to/models \
      --khmer-config /path/to/models/khmer_tts_v3.json \
      --rounds 5
"""
import argparse
import base64
import json
import logging
import math
import subprocess
import threading
import time
from pathlib import Path

from combined_worker import (CombinedWorker, VOICE_IDS, load_voices, LlamaChat,
                             model_path, resolve_model_id, configured_models,
                             verify_manifest)

LOG = logging.getLogger('verify_combined')


def gpu_memory_mib():
    """Query current GPU VRAM usage in MiB via nvidia-smi, or None if unavailable."""
    try:
        res = subprocess.run(
            ['nvidia-smi', '--query-gpu=memory.used', '--format=csv,nounits,noheader'],
            capture_output=True,
            text=True,
            check=True,
            timeout=2,
        )
        lines = [line.strip() for line in res.stdout.strip().splitlines() if line.strip()]
        if lines:
            return int(lines[0])
    except Exception:
        pass
    try:
        import torch
        if torch.cuda.is_available():
            return int(torch.cuda.max_memory_allocated() / (1024 * 1024))
    except Exception:
        pass
    return None


def summarize(durations_ms):
    if not durations_ms:
        return {'count': 0, 'avg_ms': 0.0, 'p50_ms': 0.0, 'p95_ms': 0.0, 'max_ms': 0.0, 'min_ms': 0.0}
    sorted_durations = sorted(durations_ms)
    n = len(sorted_durations)

    def percentile(p):
        idx = min(n - 1, max(0, int(math.ceil(p * n)) - 1))
        return round(float(sorted_durations[idx]), 2)

    return {
        'count': n,
        'avg_ms': round(float(sum(sorted_durations) / n), 2),
        'min_ms': round(float(sorted_durations[0]), 2),
        'max_ms': round(float(sorted_durations[-1]), 2),
        'p50_ms': percentile(0.50),
        'p95_ms': percentile(0.95),
    }


def benchmark(worker, rounds=3, sample_interval=0.05, gpu_sampler=gpu_memory_mib):
    chat_durations = []
    tts_durations = []
    errors = []
    peak_vram = 0

    stop_sampling = threading.Event()

    def sample_gpu():
        nonlocal peak_vram
        while not stop_sampling.is_set():
            vram = gpu_sampler()
            if vram is not None and vram > peak_vram:
                peak_vram = vram
            stop_sampling.wait(sample_interval)

    sampler_thread = threading.Thread(target=sample_gpu, daemon=True)
    sampler_thread.start()

    try:
        for r in range(rounds):
            round_events = []
            lock = threading.Lock()

            def make_send(collector):
                def _send(evt):
                    with lock:
                        collector.append((time.monotonic(), evt))
                return _send

            # Concurrently dispatch 1 Chat + 3 TTS (1 for each voice)
            threads = []

            # Chat task
            chat_events = []
            chat_send = make_send(chat_events)
            chat_req_id = f"bench-chat-{r}"
            chat_msg = {
                'type': 'chat_request',
                'request_id': chat_req_id,
                'model': worker.model_id,
                'messages': [{'role': 'user', 'content': f'Benchmark test prompt #{r}. Explain concurrency.'}],
                'stream': True,
                'max_tokens': 64,
            }

            def run_chat():
                t0 = time.monotonic()
                worker.dispatch(chat_msg, chat_send, 'llm')
                # Wait for token_end or error
                timeout = time.monotonic() + 30.0
                done = False
                while time.monotonic() < timeout:
                    with lock:
                        for _, evt in chat_events:
                            if evt.get('request_id') == chat_req_id:
                                if evt.get('type') in ('token_end', 'job_result'):
                                    chat_durations.append((time.monotonic() - t0) * 1000.0)
                                    done = True
                                    break
                                elif evt.get('type') == 'error':
                                    errors.append(f"Chat error: {evt.get('error')}")
                                    done = True
                                    break
                    if done:
                        break
                    time.sleep(0.005)
                if not done:
                    errors.append(f"Chat timeout for round {r}")

            threads.append(threading.Thread(target=run_chat))

            # TTS tasks
            sample_texts = {
                'ngochuyennew': 'Xin chào, đây là kiểm tra đồng thời cho giọng Ngọc Huyền.',
                'th_TH-mms_female-medium': 'สวัสดี นี่คือการทดสอบการทำงานพร้อมกัน',
                'khmer_tts_v3': 'សួស្តី នេះជាការសាកល្បង',
            }

            for voice_id in VOICE_IDS:
                if voice_id not in worker.voices:
                    continue
                tts_events = []
                tts_send = make_send(tts_events)
                tts_req_id = f"bench-tts-{voice_id}-{r}"
                tts_msg = {
                    'type': 'tts_request',
                    'request_id': tts_req_id,
                    'model': f"piper:{voice_id}",
                    'voice_id': voice_id,
                    'text': sample_texts.get(voice_id, 'Hello world'),
                }

                def make_run_tts(req_id, msg, send, evts):
                    def _run():
                        t0 = time.monotonic()
                        worker.dispatch(msg, send, 'tts')
                        timeout = time.monotonic() + 30.0
                        done = False
                        while time.monotonic() < timeout:
                            with lock:
                                for _, evt in evts:
                                    if evt.get('request_id') == req_id:
                                        if evt.get('type') == 'audio_result':
                                            try:
                                                audio = base64.b64decode(evt.get('audio_base64', ''), validate=True)
                                                if (evt.get('mime') != 'audio/wav' or len(audio) < 12 or
                                                        audio[:4] != b'RIFF' or audio[8:12] != b'WAVE'):
                                                    raise ValueError('invalid WAV response')
                                            except (ValueError, TypeError) as exc:
                                                errors.append(f"Invalid TTS response ({req_id}): {exc}")
                                            else:
                                                tts_durations.append((time.monotonic() - t0) * 1000.0)
                                            done = True
                                            break
                                        elif evt.get('type') == 'error':
                                            errors.append(f"TTS error ({req_id}): {evt.get('error')}")
                                            done = True
                                            break
                            if done:
                                break
                            time.sleep(0.005)
                        if not done:
                            errors.append(f"TTS timeout for {req_id}")
                    return _run

                threads.append(threading.Thread(target=make_run_tts(tts_req_id, tts_msg, tts_send, tts_events)))

            # Launch all concurrent requests for this round
            for t in threads:
                t.start()
            for t in threads:
                t.join()

    finally:
        stop_sampling.set()
        sampler_thread.join()

    # Final sample check
    final_vram = gpu_sampler()
    if final_vram is not None and final_vram > peak_vram:
        peak_vram = final_vram

    return {
        'chat': summarize(chat_durations),
        'tts': summarize(tts_durations),
        'gpu_peak_mib': peak_vram,
        'errors': errors,
    }


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--gguf', type=Path, required=True, help='Path to LLM GGUF model')
    parser.add_argument('--models-dir', type=Path, required=True, help='Directory containing configured voice models')
    parser.add_argument('--khmer-onnx', type=Path, help='Optional explicit Khmer ONNX path')
    parser.add_argument('--khmer-config', type=Path, help='Optional explicit khmer_tts_v3.json path')
    parser.add_argument('--config', type=Path, default=Path('config/models.yaml'))
    parser.add_argument('--asset-manifest', type=Path, help='Optional asset paths/SHA-256 JSON manifest')
    parser.add_argument('--khmer-sample-rate', type=int, default=22050)
    parser.add_argument('--model-id', help='Configured google_colab chat model ID; required if config has multiple')
    parser.add_argument('--gpu-layers', type=int, default=-1)
    parser.add_argument('--context-size', type=int, default=2048)
    parser.add_argument('--tts-gpu', action='store_true')
    parser.add_argument('--rounds', type=int, default=5, help='Number of concurrent rounds to execute')
    args = parser.parse_args(argv)

    logging.basicConfig(level=logging.INFO, format='%(asctime)s [%(levelname)s] %(message)s')
    if not args.config.is_file():
        parser.error(f'config file missing: {args.config}; supply --config config/models.yaml')
    try:
        model_id = resolve_model_id(args.config, args.model_id)
        _, configured_tts = configured_models(args.config)
        if not args.gguf.is_file():
            raise FileNotFoundError(f'LLM GGUF file missing: {args.gguf}')
        khmer_onnx = args.khmer_onnx or model_path(args.models_dir, 'khmer_tts_v3')
        khmer_cfg = args.khmer_config or (args.models_dir / 'khmer_tts_v3.json')
        assets = {'gguf': args.gguf,
                  'ngochuyennew': model_path(args.models_dir, 'ngochuyennew'),
                  'th_TH-mms_female-medium': model_path(args.models_dir, 'th_TH-mms_female-medium'),
                  'khmer_tts_v3': khmer_onnx,
                  'khmer_tts_v3_config': khmer_cfg}
        verify_manifest(args.asset_manifest, assets)
    except Exception as exc:
        parser.error(str(exc))
    LOG.info('Resolved configured model ID: %s', model_id)
    LOG.info("Loading voice sessions...")
    providers = ['CUDAExecutionProvider', 'CPUExecutionProvider'] if args.tts_gpu else ['CPUExecutionProvider']
    voices = load_voices(args.models_dir, args.khmer_sample_rate, providers, args.khmer_onnx, args.khmer_config, use_cuda=args.tts_gpu)
    configured_missing = [model for model in configured_tts if model.removeprefix('piper:') not in voices]
    if configured_missing:
        parser.error(f'configured TTS models lack loaded assets: {configured_missing}')

    LOG.info("Loading LLM model with llama-cpp...")
    llm = LlamaChat(args.gguf, args.gpu_layers, args.context_size)

    worker = CombinedWorker(model_id, llm, voices)
    try:
        LOG.info(f"Starting benchmark: {args.rounds} concurrent rounds (1 Chat + {len(voices)} TTS per round)...")
        results = benchmark(worker, rounds=args.rounds)
        print("\n================ BENCHMARK RESULTS ================")
        print(f"Peak VRAM: {results['gpu_peak_mib']} MiB")
        print("\nChat Latency:")
        print(f"  Count: {results['chat']['count']}")
        print(f"  P50:   {results['chat']['p50_ms']} ms")
        print(f"  P95:   {results['chat']['p95_ms']} ms")
        print(f"  Avg:   {results['chat']['avg_ms']} ms")
        print(f"  Min:   {results['chat']['min_ms']} ms | Max: {results['chat']['max_ms']} ms")
        print("\nTTS Latency:")
        print(f"  Count: {results['tts']['count']}")
        print(f"  P50:   {results['tts']['p50_ms']} ms")
        print(f"  P95:   {results['tts']['p95_ms']} ms")
        print(f"  Avg:   {results['tts']['avg_ms']} ms")
        print(f"  Min:   {results['tts']['min_ms']} ms | Max: {results['tts']['max_ms']} ms")
        if results['errors']:
            print(f"\nErrors ({len(results['errors'])}):")
            for err in results['errors']:
                print(f"  - {err}")
        print("===================================================\n")
    finally:
        worker.close()


if __name__ == '__main__':
    main()
