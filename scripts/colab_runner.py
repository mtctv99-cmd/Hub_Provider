#!/usr/bin/env python3
"""CLI Bridge for Colab Supervisor actions (start, stop, rotate).

Invoked by Sever_provider's Go colabsupervisor module when
COLAB_SUPERVISOR_CMD is configured:
  python scripts/colab_runner.py <action> <account_id>

Actions:
  start  - Allocates a T4 GPU notebook, injects worker code (combined, vLLM or
           llama.cpp), and starts worker
  stop   - Terminates and unassigns active Colab session for the account
  rotate - Gracefully stops the current account and starts next eligible account

Worker types (--worker-type, default: combined):
  combined - colab/combined_worker.py: 1 LLM (llama.cpp) + 3 TTS voices,
             2 WS lanes. Last-known-good on T4.
  llama    - colab/remote_worker.py: LLM text-only (llama.cpp).
  vllm     - colab/vllm_worker.py: high-throughput chat, needs big VRAM.
"""
import argparse
import base64
import json
import logging
import os
import subprocess
import sys
import threading
import time
import uuid
from pathlib import Path

LOG = logging.getLogger('colab_runner')

REPO_ROOT = Path(__file__).resolve().parent.parent
REMOTE_WORKER_SCRIPT = REPO_ROOT / 'colab' / 'remote_worker.py'
VLLM_WORKER_SCRIPT = REPO_ROOT / 'colab' / 'vllm_worker.py'
COMBINED_WORKER_SCRIPT = REPO_ROOT / 'colab' / 'combined_worker.py'

# Last-known-good on T4 (Sep 2026): gemma-4 E2B heretic Q4_K_M via llama.cpp.
DEFAULT_MODEL = os.environ.get('COLAB_MODEL', 'gemma-4-colab')
DEFAULT_REPO_ID = 'llmfan46/gemma-4-E2B-it-ultra-uncensored-heretic-GGUF'
DEFAULT_FILENAME = 'gemma-4-E2B-it-ultra-uncensored-heretic-Q4_K_M.gguf'
DEFAULT_MMPROJ = 'gemma-4-E2B-it-mmproj-BF16.gguf'
DEFAULT_LLM_NAME = 'gemma-4-colab'
DEFAULT_CONTEXT_SIZE = 32768
DEFAULT_LLM_CONCURRENCY = 8


def check_warp_proxy():
    try:
        r = subprocess.run(['warp-cli', 'status'], capture_output=True, text=True, timeout=3)
        if 'Connected' in r.stdout:
            LOG.info('WARP proxy active, routing via socks5://127.0.0.1:40000')
            return {
                'http': 'socks5://127.0.0.1:40000',
                'https': 'socks5://127.0.0.1:40000',
            }
    except Exception:
        pass
    return None


def resolve_token_file(token_dir, account_id):
    name = account_id.removeprefix('colab-')
    candidates = [
        Path(token_dir) / f'token_{name}.json',
        Path(token_dir) / f'token_{base64.urlsafe_b64encode(name.encode()).decode().rstrip("=")}.json',
        Path(token_dir) / f'token_{name.replace("@", "_at_").replace(".", "_")}.json',
        Path(token_dir) / 'token.json',
    ]
    for c in candidates:
        if c.is_file():
            return c
    return None


def get_account_email(account_id, token_file):
    name = account_id.removeprefix('colab-')
    if '@' in name:
        return name
    if '_at_' in name:
        parts = name.split('_at_', 1)
        return parts[0] + '@' + parts[1].replace('_', '.')
    try:
        with open(token_file, 'r', encoding='utf-8') as f:
            data = json.load(f)
            if 'client_email' in data:
                return data['client_email']
    except Exception:
        pass
    return name


def get_client(token_file):
    from google.oauth2.credentials import Credentials
    from google.auth.transport.requests import Request, AuthorizedSession
    from colab_cli.client import Client, Prod

    proxies = check_warp_proxy()
    with open(token_file, 'r', encoding='utf-8') as f:
        data = json.load(f)
    creds = Credentials.from_authorized_user_info(data)
    if creds.expired and creds.refresh_token:
        creds.refresh(Request())
    sess = AuthorizedSession(creds)
    if proxies:
        sess.proxies.update(proxies)
    return Client(Prod, session=sess)


# Minimal models.yaml materialized on the Colab VM for combined_worker --config.
# combined_worker requires a config file; the notebook has no repo checkout.
COMBINED_CONFIG_YAML = """models:
  gemma-4-colab:
    provider: google_colab
    capabilities: [chat, streaming, vision, reasoning]
    timeout_sec: 180
  piper:ngochuyennew:
    provider: piper_local
    capabilities: [audio_speech]
  piper:th_TH-mms_female-medium:
    provider: piper_local
    capabilities: [audio_speech]
  piper:khmer_tts_v3:
    provider: piper_local
    capabilities: [audio_speech]
"""
COMBINED_CONFIG_PATH = '/content/combined_models.yaml'


def do_start(account_id, token_dir, server_url, worker_token, worker_type='combined',
             context_size=DEFAULT_CONTEXT_SIZE, llm_concurrency=DEFAULT_LLM_CONCURRENCY,
             repo_id=None, filename=None, mmproj=None, model_name=None, aliases=None):
    token_file = resolve_token_file(token_dir, account_id)
    if not token_file:
        LOG.error('Token file not found for account %s in %s', account_id, token_dir)
        sys.exit(1)
    if not worker_token:
        LOG.error('Worker token is required: set WORKER_TOKEN/WORKER_AUTH_TOKEN or pass --token')
        sys.exit(1)

    email = get_account_email(account_id, token_file)
    LOG.info('Starting Colab worker for %s (email: %s, type: %s)', account_id, email, worker_type)

    try:
        from colab_cli.client import Variant, Accelerator
        from colab_cli.runtime import ColabRuntime
    except ImportError:
        LOG.warning('colab_cli not installed in local environment; dry-run mode')
        return

    client = get_client(token_file)

    # 1. Clean up stale notebook assignments
    try:
        for a in client.list_assignments():
            try:
                LOG.info('[%s] Unassigning stale notebook: %s', email, a.endpoint)
                client.unassign(a.endpoint)
            except Exception:
                pass
    except Exception as e:
        LOG.warning('[%s] list_assignments warning: %s', email, e)

    # 2. Assign fresh T4 GPU session
    LOG.info('[%s] Requesting fresh T4 GPU session from Google Colab...', email)
    active_assignment = client.assign(uuid.uuid4(), variant=Variant.GPU, accelerator=Accelerator.T4)
    LOG.info('[%s] Session assigned: %s', email, active_assignment.endpoint)

    url = active_assignment.runtime_proxy_info.url
    proxy_token = active_assignment.runtime_proxy_info.token
    endpoint = active_assignment.endpoint
    runtime = ColabRuntime(url, proxy_token)

    worker_id = f"colab-{uuid.uuid4().hex[:6]}"

    # Pick script to inject
    if worker_type == 'vllm' and VLLM_WORKER_SCRIPT.is_file():
        target_script = VLLM_WORKER_SCRIPT
    elif worker_type == 'llama' or not COMBINED_WORKER_SCRIPT.is_file():
        target_script = REMOTE_WORKER_SCRIPT
    else:
        target_script = COMBINED_WORKER_SCRIPT
        worker_type = 'combined'
    with open(target_script, 'r', encoding='utf-8') as f:
        script_code = f.read()

    hf_token = os.environ.get('HF_TOKEN', '')
    preamble_config = ''
    if worker_type == 'combined':
        argv_repr = [
            'combined_worker',
            '--server', str(server_url),
            '--worker-id', str(worker_id),
            '--token', str(worker_token),
            '--config', COMBINED_CONFIG_PATH,
            '--model-id', model_name or DEFAULT_LLM_NAME,
            '--repo-id', repo_id or DEFAULT_REPO_ID,
            '--filename', filename or DEFAULT_FILENAME,
            '--context-size', str(context_size),
            '--llm-concurrency', str(llm_concurrency),
            '--flash-attn',
            '--tts-gpu',
        ]
        # NOTE: --mmproj intentionally omitted: LlamaChat skips vision when the
        # file is absent, and the projector is optional for chat.
        # Materialize the minimal models.yaml combined_worker --config requires.
        preamble_config = (
            "from pathlib import Path as _P\n"
            f"_P({COMBINED_CONFIG_PATH!r}).write_text({COMBINED_CONFIG_YAML!r}, encoding='utf-8')\n"
        )
    elif worker_type == 'vllm':
        argv_repr = [
            'vllm_worker',
            '--server', str(server_url),
            '--worker-id', str(worker_id),
            '--token', str(worker_token),
            '--model', DEFAULT_MODEL,
            '--concurrency', str(llm_concurrency),
            '--max-len', str(context_size),
        ]
    else:
        argv_repr = [
            'remote_worker',
            '--server-url', str(server_url),
            '--worker-id', str(worker_id),
            '--api-key', str(worker_token),
            '--repo-id', repo_id or DEFAULT_REPO_ID,
            '--filename', filename or DEFAULT_FILENAME,
            '--mmproj', mmproj or DEFAULT_MMPROJ,
            '--model-name', model_name or DEFAULT_LLM_NAME,
            '--n-ctx', str(context_size),
            '--flash-attn',
        ]
        for alias in (aliases or []):
            argv_repr += ['--alias', str(alias)]

    bootstrap = ''
    if worker_type == 'combined':
        # Fresh Colab T4 ships almost nothing; install before worker imports.
        bootstrap = """
import importlib.util, subprocess, sys
def _pip(*pkgs):
    missing = [p for p in pkgs if importlib.util.find_spec(p) is None]
    if missing:
        print('Installing ' + ' '.join(missing) + '...', flush=True)
        subprocess.check_call([sys.executable, '-m', 'pip', 'install', '-q'] + missing)
_pip('websockets')
_pip('websocket-client')
_pip('huggingface_hub')
_pip('hf_transfer')
_pip('numpy')
_pip('pyyaml')
_pip('onnxruntime')
_pip('piper-tts')
if importlib.util.find_spec('llama_cpp') is None:
    print('Installing llama-cpp-python (CUDA)...', flush=True)
    subprocess.check_call([sys.executable, '-m', 'pip', 'install', '-q', 'llama-cpp-python', '--extra-index-url', 'https://abetlen.github.io/llama-cpp-python/whl/cu122'])
# CUDA wheel links against libcudart.so.12, which stock Colab images do not
# expose on the loader path (a preinstalled nvidia.cuda_runtime may exist yet
# still not resolve). Always (re)install and prepend its lib dir so ctypes
# dlopen finds libcudart at import time.
# CUDA wheel links against libcudart/cublas .so.12, which stock Colab images
# do not expose on the loader path (a preinstalled nvidia.cuda_runtime may
# exist yet still not resolve). Always (re)install the runtime libs, prepend
# every nvidia lib dir, and preload the .so files by absolute path.
print('Ensuring nvidia CUDA runtime libs...', flush=True)
subprocess.check_call([sys.executable, '-m', 'pip', 'install', '-q', '-U',
                       'nvidia-cuda-runtime-cu12', 'nvidia-cublas-cu12'])
import os as _os
import site as _site
for _sp in _site.getsitepackages() + [_site.getusersitepackages()]:
    _nvroot = _os.path.join(_sp, 'nvidia')
    if not _os.path.isdir(_nvroot):
        continue
    for _entry in sorted(_os.listdir(_nvroot)):
        _libdir = _os.path.join(_nvroot, _entry, 'lib')
        if _os.path.isdir(_libdir):
            _old = _os.environ.get('LD_LIBRARY_PATH', '')
            if _libdir not in _old.split(':'):
                _os.environ['LD_LIBRARY_PATH'] = _libdir + ((':' + _old) if _old else '')
                print('Added to LD_LIBRARY_PATH: ' + _libdir, flush=True)
import ctypes as _ct
import glob as _glob
_preloaded = []
for _pat in (
    'nvidia/cuda_runtime/lib/libcudart.so*',
    'nvidia/cublas/lib/libcublas*.so*',
):
    for _base in _site.getsitepackages() + [_site.getusersitepackages()]:
        for _p in sorted(_glob.glob(_os.path.join(_base, _pat))):
            try:
                _ct.CDLL(_p)
                _preloaded.append(_p)
                print('Preloaded ' + _p, flush=True)
            except Exception as _e:
                print('Preload failed ' + _p + ': ' + str(_e)[:160], flush=True)
if not _preloaded:
    print('WARNING: no CUDA libs preloaded; libllama load may fail', flush=True)
print('combined bootstrap done.', flush=True)
"""
    elif worker_type == 'vllm':
        # Colab runtime does not ship vllm; install before the worker imports it.
        bootstrap = """
import importlib.util, subprocess, sys
if importlib.util.find_spec('vllm') is None:
    print('Installing vllm...', flush=True)
    subprocess.check_call([sys.executable, '-m', 'pip', 'install', '-q', 'vllm'])
if importlib.util.find_spec('websockets') is None:
    subprocess.check_call([sys.executable, '-m', 'pip', 'install', '-q', 'websockets'])
if importlib.util.find_spec('nest_asyncio') is None:
    subprocess.check_call([sys.executable, '-m', 'pip', 'install', '-q', 'nest_asyncio'])
print('vllm bootstrap done.', flush=True)
"""

    injected_code = f"""# === Injected by Sever_provider Launcher ===
import os
import sys
HF_TOKEN = {hf_token!r}
if HF_TOKEN:
    os.environ["HF_TOKEN"] = HF_TOKEN
os.environ["HF_HUB_ENABLE_HF_TRANSFER"] = "1"
sys.argv = {argv_repr!r}
{preamble_config}# ============================================
""" + bootstrap + script_code

    stop_event = threading.Event()
    ready_event = threading.Event()

    def run_worker():
        try:
            def output_hook(out):
                if out.get('output_type') == 'stream':
                    text = out.get('text', '').strip()
                    if text:
                        LOG.info('[%s] Colab: %s', email, text[:200])
                        if 'Connected' in text or 'Registered' in text or 'Model loaded' in text:
                            ready_event.set()
                elif out.get('output_type') == 'error':
                    tb = ''.join(out.get('traceback', []))
                    LOG.error('[%s] Colab error: %s', email, tb[-1500:])

            runtime.execute_code(injected_code, timeout=1800, output_hook=output_hook)
        except Exception as e:
            if not stop_event.is_set():
                LOG.error('[%s] Execution error: %s', email, e)
        finally:
            stop_event.set()

    def keep_alive():
        fails = 0
        while not stop_event.is_set():
            if stop_event.wait(120):
                break
            try:
                client.keep_alive_assignment(endpoint)
                fails = 0
            except Exception as e:
                fails += 1
                LOG.warning('[%s] Keep-alive error (%d/3): %s', email, fails, e)
                if fails >= 3:
                    LOG.error('[%s] Keep-alive failed 3x, terminating', email)
                    stop_event.set()
                    break

    worker_thread = threading.Thread(target=run_worker, daemon=True)
    ka_thread = threading.Thread(target=keep_alive, daemon=True)
    worker_thread.start()
    ka_thread.start()

    LOG.info('[%s] Waiting for Colab worker to register with Server...', email)
    # Cold boot takes long: vllm pip install + model load 10-20 min,
    # combined (llama-cpp CUDA + GGUF + 3 TTS voices) up to ~30 min.
    ready_timeout = 1800.0 if worker_type == 'combined' else 1200.0
    ready_event.wait(timeout=ready_timeout)
    if ready_event.is_set():
        LOG.info('Colab worker %s online and registered successfully.', worker_id)
    else:
        LOG.warning('Worker %s did not signal ready within %d minutes.', worker_id, int(ready_timeout // 60))
        # ponytail: release GPU on failed start, else quota leaks until manual stop.
        try:
            client.unassign(endpoint)
        except Exception as e:
            LOG.warning('[%s] unassign on failed start: %s', email, e)
        stop_event.set()
        sys.exit(1)
    # Keep main thread alive so daemon keep_alive keeps the assignment from idling out.
    try:
        while not stop_event.is_set():
            time.sleep(30)
    except KeyboardInterrupt:
        LOG.info('Stopping worker on interrupt...')
        stop_event.set()


def do_stop(account_id, token_dir):
    token_file = resolve_token_file(token_dir, account_id)
    if not token_file:
        LOG.warning('Token file not found for %s, skipping stop', account_id)
        return

    email = get_account_email(account_id, token_file)
    LOG.info('Stopping Colab session for %s (%s)', account_id, email)

    try:
        client = get_client(token_file)
        assignments = client.list_assignments()
        for a in assignments:
            try:
                LOG.info('[%s] Unassigning notebook endpoint: %s', email, a.endpoint)
                client.unassign(a.endpoint)
            except Exception as e:
                LOG.warning('[%s] Error unassigning %s: %s', email, a.endpoint, e)
        LOG.info('Colab sessions stopped for %s.', email)
    except Exception as e:
        LOG.warning('Error stopping Colab session for %s: %s', email, e)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['start', 'stop', 'rotate'])
    parser.add_argument('account_id')
    parser.add_argument('--token-dir', default=os.environ.get('COLAB_TOKEN_DIR') or str(Path.home() / '.config' / 'colab-cli'))
    parser.add_argument('--server', default=os.environ.get('COLAB_SERVER_URL') or os.environ.get('PIPER_SERVER_URL') or 'ws://127.0.0.1:18080/api/llm/ws/worker')
    parser.add_argument('--token', default=os.environ.get('WORKER_TOKEN') or os.environ.get('WORKER_AUTH_TOKEN') or '', help='Worker auth token (required, no insecure default)')
    parser.add_argument('--worker-type', default=os.environ.get('COLAB_WORKER_TYPE', 'combined'), choices=['combined', 'vllm', 'llama'])
    parser.add_argument('--context-size', type=int, default=DEFAULT_CONTEXT_SIZE)
    parser.add_argument('--llm-concurrency', type=int, default=DEFAULT_LLM_CONCURRENCY)
    parser.add_argument('--repo-id', default=None, help='GGUF repo (llama worker)')
    parser.add_argument('--filename', default=None, help='GGUF file (llama worker)')
    parser.add_argument('--mmproj', default=None, help='Vision mmproj file (llama worker)')
    parser.add_argument('--model-name', default=None, help='Primary model id registered to the hub')
    parser.add_argument('--alias', action='append', default=[], help='Extra hub model alias (repeatable)')
    args = parser.parse_args()

    logging.basicConfig(level=logging.INFO, format='%(asctime)s [%(levelname)s] %(message)s')
    LOG.info('Colab action: %s for %s', args.action, args.account_id)

    if args.action == 'start':
        do_start(args.account_id, args.token_dir, args.server, args.token,
                 worker_type=args.worker_type, context_size=args.context_size,
                 llm_concurrency=args.llm_concurrency, repo_id=args.repo_id,
                 filename=args.filename, mmproj=args.mmproj,
                 model_name=args.model_name, aliases=args.alias)
    elif args.action == 'stop':
        do_stop(args.account_id, args.token_dir)
    elif args.action == 'rotate':
        do_stop(args.account_id, args.token_dir)
        do_start(args.account_id, args.token_dir, args.server, args.token,
                 worker_type=args.worker_type, context_size=args.context_size,
                 llm_concurrency=args.llm_concurrency, repo_id=args.repo_id,
                 filename=args.filename, mmproj=args.mmproj,
                 model_name=args.model_name, aliases=args.alias)


if __name__ == '__main__':
    main()
