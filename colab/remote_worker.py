import sys
import subprocess
import os

# Auto-install dependencies on Colab if missing
os.environ["HF_HUB_ENABLE_HF_TRANSFER"] = "1"
required_packages = ["websockets", "huggingface_hub", "hf_transfer"]
missing_packages = []
for pkg in required_packages:
    try:
        __import__(pkg)
    except ImportError:
        missing_packages.append(pkg)

if missing_packages:
    print(f"Installing missing packages: {missing_packages}...")
    subprocess.check_call([sys.executable, "-m", "pip", "install", "-q"] + missing_packages)
    print("Packages installed.")

# Check llama-cpp-python (upgrade to latest for Qwen3-VL support)
print("Checking llama-cpp-python version...")
try:
    import llama_cpp
    print(f"Existing llama_cpp version: {llama_cpp.__version__}")
except Exception as e:
    # ImportError AND the common RuntimeError from a wheel whose libllama.so
    # cannot find libcudart.so.12 on a fresh VM — both mean reinstall.
    print(f"llama_cpp unusable ({e}), will reinstall with CUDA runtime...")
    llama_cpp = None

# The cu122 wheel links against libcudart.so.12, which is NOT on the base VM
# (PyTorch bundles its own copy inside site-packages). Install the CUDA 12
# runtime so ctypes can load libllama.so.
try:
    import ctypes
    ctypes.CDLL("libcudart.so.12")
    print("libcudart.so.12 already available.")
except OSError:
    print("Installing CUDA 12 runtime for llama.cpp wheel...")
    subprocess.check_call([
        sys.executable, "-m", "pip", "install", "-q",
        "nvidia-cuda-runtime-cu12>=12.2,<13",
        "nvidia-cublas-cu12>=12.2,<13",
    ])
    # Make the pip-installed libs visible to dlopen: preload the exact paths.
    import glob, ctypes as _ct
    site = subprocess.check_output([
        sys.executable, "-c",
        "import nvidia, os; print(os.path.dirname(nvidia.__file__))"
    ], text=True).strip()
    for so in glob.glob(f"{site}/*/lib/libcudart.so*") + glob.glob(f"{site}/*/lib/libcublas*.so*"):
        try:
            _ct.CDLL(so, mode=_ct.RTLD_GLOBAL)
        except OSError as e:
            print(f"preload skip {so}: {e}")

subprocess.check_call([
    sys.executable, "-m", "pip", "install", "-q", "--upgrade",
    "llama-cpp-python>=0.3.16",
    "--extra-index-url", "https://abetlen.github.io/llama-cpp-python/whl/cu122"
])
try:
    import importlib
    if llama_cpp is not None:
        importlib.reload(llama_cpp)
    else:
        import llama_cpp
    print(f"llama_cpp version after upgrade: {llama_cpp.__version__}")
except Exception as e:
    print(f"Warning: version check failed ({e}), continuing...")

import argparse
import asyncio
import json
import logging
import time
from urllib.parse import urlencode
from huggingface_hub import hf_hub_download
from llama_cpp import Llama

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger(__name__)

# Arguments (may be overridden by injected sys.argv from colab_runner)
parser = argparse.ArgumentParser(description="Colab LLM Remote Worker")
parser.add_argument("--server-url", default="https://hub.maika.sbs")
parser.add_argument("--worker-id", default="colab-default")
parser.add_argument("--api-key", default="omni-secret-key-123")
parser.add_argument("--repo-id", default="unsloth/Qwen3.5-2B-GGUF")
parser.add_argument("--filename", default="Qwen3.5-2B-Q8_0.gguf")
parser.add_argument("--mmproj", default="mmproj-F16.gguf")
parser.add_argument("--model-name", default="qwen3.5-2b", help="Primary model id registered to the hub")
parser.add_argument("--alias", action="append", default=[], help="Extra model alias (repeatable)")
parser.add_argument("--n-ctx", type=int, default=32768)
parser.add_argument("--flash-attn", action="store_true", default=True)
args, _ = parser.parse_known_args()

SERVER_URL = args.server_url.rstrip("/")
WORKER_ID = args.worker_id
API_KEY = args.api_key
REPO_ID = args.repo_id
FILENAME = args.filename
MMPROJ = args.mmproj
N_CTX = args.n_ctx
MODEL_NAME = args.model_name
EXTRA_ALIASES = args.alias

_model = None

def load_model() -> Llama:
    global _model
    if _model is not None:
        return _model

    models_dir = "/content/models" if os.path.exists("/content") else "/tmp/models"
    os.makedirs(models_dir, exist_ok=True)
    model_path = os.path.join(models_dir, FILENAME)

    if not os.path.exists(model_path):
        print(f"Downloading model {FILENAME} from {REPO_ID}...")
        sys.stdout.flush()
        token = os.environ.get("HF_TOKEN", "")
        hf_token = token if token else None
        print(f"Using HF_TOKEN: {'yes (length %d)' % len(token) if token else 'no'}")
        sys.stdout.flush()
        downloaded = hf_hub_download(
            repo_id=REPO_ID,
            filename=FILENAME,
            local_dir=models_dir,
            local_dir_use_symlinks=False,
            token=hf_token
        )
        print(f"Download completed: {downloaded}")
        sys.stdout.flush()

    chat_handler = None
    if MMPROJ:
        mmproj_path = os.path.join(models_dir, MMPROJ)
        if not os.path.exists(mmproj_path):
            print(f"Downloading vision mmproj {MMPROJ} from {REPO_ID}...")
            sys.stdout.flush()
            try:
                token = os.environ.get("HF_TOKEN", "")
                hf_hub_download(
                    repo_id=REPO_ID,
                    filename=MMPROJ,
                    local_dir=models_dir,
                    local_dir_use_symlinks=False,
                    token=token or None
                )
                print(f"Vision mmproj downloaded: {mmproj_path}")
            except Exception as e:
                print(f"Warning: Failed to download mmproj: {e}")

        if os.path.exists(mmproj_path):
            try:
                if "qwen" in FILENAME.lower() or "qwen" in REPO_ID.lower():
                    try:
                        from llama_cpp.llama_chat_format import Qwen25VLChatHandler as VisionHandler
                        print("Using Qwen2.5/3-VL chat handler")
                    except ImportError:
                        from llama_cpp.llama_chat_format import Llava15ChatHandler as VisionHandler
                        print("Qwen handler not available, falling back to Llava15ChatHandler")
                elif "gemma" in FILENAME.lower() or "gemma" in REPO_ID.lower():
                    # Gemma vision projector needs its own handler; the Qwen
                    # handler leaks <|im_start|> into answers and no handler
                    # at all drops images entirely.
                    VisionHandler = None
                    try:
                        from llama_cpp.llama_chat_format import Gemma4ChatHandler as VisionHandler
                        print("Using Gemma4 chat handler")
                    except ImportError:
                        try:
                            from llama_cpp.llama_chat_format import Gemma3ChatHandler as VisionHandler
                            print("Using Gemma3 chat handler")
                        except ImportError:
                            from llama_cpp.llama_chat_format import MTMDChatHandler as VisionHandler
                            print("Gemma handler not available, falling back to generic MTMD handler")
                else:
                    from llama_cpp.llama_chat_format import Llava15ChatHandler as VisionHandler
                    print("Using generic Llava15 chat handler")
                chat_handler = VisionHandler(clip_model_path=mmproj_path, verbose=False)
                print("Vision mmproj handler loaded successfully.")
            except Exception as e:
                print(f"Warning: Could not initialize vision handler: {e}")
        else:
            print(f"Vision mmproj file not found: {mmproj_path}. Loading text-only model.")

    print("Loading Llama model on CUDA (all layers offloaded, 32k context)...")
    sys.stdout.flush()

    try:
        _model = Llama(
            model_path=model_path,
            chat_handler=chat_handler,
            n_gpu_layers=-1,   # Offload all layers to GPU T4
            n_ctx=N_CTX,
            flash_attn=True,
            verbose=False
        )
    except Exception as e:
        print(f"Loading with flash_attn failed ({e}), loading standard without vision handler...")
        sys.stdout.flush()
        try:
            _model = Llama(
                model_path=model_path,
                chat_handler=chat_handler,
                n_gpu_layers=-1,
                n_ctx=N_CTX,
                verbose=False
            )
        except Exception as e2:
            print(f"Loading with vision handler failed ({e2}), loading text-only GGUF...")
            sys.stdout.flush()
            _model = Llama(
                model_path=model_path,
                n_gpu_layers=-1,
                n_ctx=N_CTX,
                verbose=False
            )

    # Warmup
    print("Warming up model...")
    sys.stdout.flush()
    _model.create_chat_completion(
        messages=[{"role": "user", "content": "Hi"}],
        max_tokens=4
    )
    print("Model loaded successfully.")
    sys.stdout.flush()
    return _model

async def run_worker():
    import websockets

    print(f"=== Colab LLM Worker ===")
    print(f"Worker ID: {WORKER_ID}")
    print(f"Server URL: {SERVER_URL}")
    print(f"Model File: {FILENAME}")
    print(f"========================")
    sys.stdout.flush()

    # Preload model before opening socket
    model = await asyncio.to_thread(load_model)

    ws_scheme = "wss" if SERVER_URL.startswith("https") or SERVER_URL.startswith("wss") else "ws"
    base_netloc = SERVER_URL.split("://", 1)[-1].rstrip("/")
    # Accept both a base URL (https://hub.maika.sbs) and a full worker URL
    # (ws://hub/api/llm/ws/worker) — COLAB_SERVER_URL passes the full path,
    # which must not be appended twice or the hub answers 401.
    for marker in ("/api/llm/ws/worker", "/api/llm/ws"):
        if marker in base_netloc:
            base_netloc = base_netloc.split(marker, 1)[0].rstrip("/")
            break
    query = urlencode({"worker_id": WORKER_ID, "token": API_KEY})
    ws_url = f"{ws_scheme}://{base_netloc}/api/llm/ws/worker?{query}"

    print(f"Connecting to Master WebSocket at {ws_url}...")
    sys.stdout.flush()

    while True:
        try:
            async with websockets.connect(ws_url, ping_interval=20, ping_timeout=20) as ws:
                # Register the configured model and its aliases
                models_list = [
                    MODEL_NAME, FILENAME.replace(".gguf", ""), "vllm-default",
                ] + list(EXTRA_ALIASES)
                reg_payload = {
                    "type": "register",
                    "worker_id": WORKER_ID,
                    "model": MODEL_NAME,
                    "models": list(dict.fromkeys(models_list)),
                    "capabilities": ["chat", "streaming", "vision", "reasoning"],
                    "max_concurrency": 4,
                }
                await ws.send(json.dumps(reg_payload))
                ack = await ws.recv()
                print(f"Connected to Master. Registered: {ack}")
                sys.stdout.flush()

                async def ping_loop():
                    while True:
                        await asyncio.sleep(15)
                        try:
                            await ws.send(json.dumps({"type": "ping"}))
                        except Exception:
                            break

                ping_task = asyncio.create_task(ping_loop())

                try:
                    while True:
                        msg_raw = await ws.recv()
                        try:
                            msg = json.loads(msg_raw)
                        except json.JSONDecodeError as jde:
                            logger.warning("Ignoring malformed JSON frame: %s", jde)
                            continue
                        msg_type = msg.get("type")

                        if msg_type == "pong":
                            continue

                        if msg_type == "chat_request":
                            req_id = msg["request_id"]
                            stream = msg.get("stream", False)
                            messages = msg.get("messages", [])
                            temperature = msg.get("temperature")
                            if temperature is None:
                                temperature = 0.7
                            top_p = msg.get("top_p")
                            if top_p is None:
                                top_p = 0.95
                            max_tokens = msg.get("max_tokens")
                            if not max_tokens or max_tokens <= 0:
                                max_tokens = 1024

                            # Ensure messages format handles multimodal / vision parts
                            clean_messages = []
                            for m in messages:
                                role = m.get("role", "user")
                                content = m.get("content")
                                parts = m.get("content_parts")
                                if (not content or content == "") and parts:
                                    content = parts
                                clean_messages.append({"role": role, "content": content})

                            try:
                                if stream:
                                    def _stream_inference():
                                        return model.create_chat_completion(
                                            messages=clean_messages,
                                            temperature=temperature,
                                            top_p=top_p,
                                            max_tokens=max_tokens,
                                            stream=True
                                        )

                                    stream_iter = await asyncio.to_thread(_stream_inference)
                                    for chunk in stream_iter:
                                        choice = chunk.get("choices", [{}])[0]
                                        delta = choice.get("delta", {}).get("content", "")
                                        finish_reason = choice.get("finish_reason")
                                        if delta:
                                            await ws.send(json.dumps({
                                                "type": "token",
                                                "request_id": req_id,
                                                "delta": delta,
                                                "finish_reason": None
                                            }))
                                    await ws.send(json.dumps({
                                        "type": "token_end",
                                        "request_id": req_id,
                                        "finish_reason": "stop"
                                    }))
                                else:
                                    def _non_stream_inference():
                                        return model.create_chat_completion(
                                            messages=clean_messages,
                                            temperature=temperature,
                                            top_p=top_p,
                                            max_tokens=max_tokens,
                                            stream=False
                                        )

                                    completion = await asyncio.to_thread(_non_stream_inference)
                                    choice = completion.get("choices", [{}])[0]
                                    text = choice.get("message", {}).get("content", "")
                                    usage = completion.get("usage", {})

                                    await ws.send(json.dumps({
                                        "type": "job_result",
                                        "request_id": req_id,
                                        "text": text,
                                        "usage": usage
                                    }))
                            except Exception as infer_err:
                                logger.error(f"Inference error on {req_id}: {infer_err}")
                                await ws.send(json.dumps({
                                    "type": "error",
                                    "request_id": req_id,
                                    "error": str(infer_err)
                                }))
                finally:
                    ping_task.cancel()

        except Exception as e:
            print(f"WebSocket error: {e}. Reconnecting in 3s...")
            sys.stdout.flush()
            await asyncio.sleep(3)

def main():
    import threading
    def _target():
        loop = asyncio.new_event_loop()
        asyncio.set_event_loop(loop)
        try:
            loop.run_until_complete(run_worker())
        finally:
            loop.close()

    t = threading.Thread(target=_target, daemon=True)
    t.start()
    t.join()

if __name__ == "__main__":
    main()
