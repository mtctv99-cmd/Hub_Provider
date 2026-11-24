#!/usr/bin/env python3
"""Colab T4 worker for OmniVoice zero-shot TTS.

Registers a Piper-style model id (default ``piper:omnivoice_vi``) on the
gateway worker hub and answers ``tts_request`` with a 24 kHz WAV in
``audio_result``. Voice id suffixes map to voice-design instructions, e.g.
``omnivoice_vi_female_low`` -> ``female, low``.
"""
import base64
import io
import importlib.util
import json
import logging
import os
import subprocess
import sys
import threading
import time
from urllib.parse import urlencode

os.environ.setdefault("PYTORCH_CUDA_ALLOC_CONF", "expandable_segments:True")
if os.path.isdir("/content") and os.access("/content", os.W_OK):
    os.environ.setdefault("HF_HOME", "/content/hf")

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("omnivoice-worker")
logger.setLevel(logging.INFO)
logger.propagate = False
if not logger.handlers:
    _h = logging.StreamHandler(sys.stdout)
    _h.setFormatter(logging.Formatter("%(asctime)s [%(levelname)s] %(message)s"))
    logger.addHandler(_h)

if importlib.util.find_spec("omnivoice") is None and os.environ.get("VIDEO_SKIP_INSTALL") != "1":
    logger.info("installing omnivoice + soundfile")
    subprocess.check_call([sys.executable, "-m", "pip", "install", "-q", "omnivoice", "soundfile"])
    logger.info("install done")

import numpy as np  # noqa: E402
import soundfile as sf  # noqa: E402
import torch  # noqa: E402


class TTS:
    def __init__(self, args):
        self.args = args
        self.model = None

    def load(self):
        if self.model is not None:
            return self.model
        from omnivoice import OmniVoice

        logger.info("loading OmniVoice (fp16, T4)")
        started = time.time()
        self.model = OmniVoice.from_pretrained(
            "k2-fsa/OmniVoice",
            device_map="cuda:0",
            dtype=torch.float16,
            load_asr=False,
        )
        logger.info("model ready in %.1fs", time.time() - started)
        return self.model

    def instruct_for(self, voice_id):
        raw = (voice_id or "").strip()
        if raw.startswith("omnivoice_vi"):
            raw = raw[len("omnivoice_vi"):]
        tokens = [t for t in raw.replace("-", "_").split("_") if t in
                  ("male", "female", "low", "high", "child", "elderly", "whisper")]
        mapped = {"low": "low pitch", "high": "high pitch"}
        return ", ".join(mapped.get(t, t) for t in tokens)

    def handle(self, msg):
        started = time.time()
        request_id = msg.get("request_id")
        text = (msg.get("text") or "").strip()
        if not text:
            return {"type": "error", "request_id": request_id, "error": "text is required"}
        instruct = self.instruct_for(msg.get("voice_id")) or None
        try:
            model = self.load()
            kwargs = {}
            if instruct:
                kwargs["instruct"] = instruct
            audio = model.generate(text=text, num_step=self.args.num_step, **kwargs)
            wav = np.concatenate([np.asarray(a, dtype=np.float32) for a in audio])
            buf = io.BytesIO()
            sf.write(buf, wav, 24000, format="WAV", subtype="PCM_16")
            data = buf.getvalue()
            logger.info("request %s: %.1fs audio in %.1fs, %d bytes, instruct=%r",
                        request_id, len(wav) / 24000, time.time() - started, len(data), instruct)
            return {
                "type": "audio_result",
                "request_id": request_id,
                "mime": "audio/wav",
                "audio_base64": base64.b64encode(data).decode("ascii"),
                "done": True,
            }
        except torch.cuda.OutOfMemoryError:
            logger.exception("OOM")
            torch.cuda.empty_cache()
            return {"type": "error", "request_id": request_id,
                    "error": "OmniVoice ran out of GPU memory"}
        except Exception as exc:
            logger.exception("tts failed")
            return {"type": "error", "request_id": request_id, "error": str(exc)[:400]}


def run(args):
    from websockets.sync.client import connect

    server = args.server_url.rstrip("/")
    scheme = "wss" if server.startswith(("https", "wss")) else "ws"
    host = server.split("://", 1)[-1]
    for marker in ("/api/llm/ws/worker", "/api/llm/ws"):
        if marker in host:
            host = host.split(marker, 1)[0].rstrip("/")
            break
    query = urlencode({"worker_id": args.worker_id, "token": args.api_key})
    url = f"{scheme}://{host}/api/llm/ws/worker?{query}"
    tts = TTS(args)
    models = [args.model]
    for suffix in ("female_low", "female_high", "male_low", "male_high", "female", "male"):
        alias = f"{args.model}_{suffix}"
        if alias not in models:
            models.append(alias)
    logger.info("dialing %s://%s/api/llm/ws/worker (models=%s)", scheme, host, models)

    while True:
        try:
            with connect(url, max_size=16 * 1024 * 1024, ping_interval=20, ping_timeout=600) as ws:
                ws.send(json.dumps({
                    "type": "register",
                    "worker_id": args.worker_id,
                    "model": args.model,
                    "models": models,
                    "capabilities": ["audio_speech"],
                    "max_concurrency": 1,
                }))
                logger.info("connected; ack=%s", ws.recv())
                stop = threading.Event()

                def keepalive():
                    while not stop.wait(15):
                        try:
                            ws.send(json.dumps({"type": "ping"}))
                        except Exception:
                            return

                threading.Thread(target=keepalive, daemon=True).start()
                try:
                    while True:
                        msg = json.loads(ws.recv())
                        if msg.get("type") == "ping":
                            ws.send(json.dumps({"type": "pong"}))
                            continue
                        if msg.get("type") != "tts_request":
                            continue
                        ws.send(json.dumps(tts.handle(msg)))
                finally:
                    stop.set()
        except Exception as exc:
            logger.warning("connection lost (%r); retrying in 15s", exc)
            time.sleep(15)


def main():
    import argparse

    ap = argparse.ArgumentParser(description="Sever Provider Colab OmniVoice TTS worker")
    ap.add_argument("--server-url", default=os.environ.get("COLAB_SERVER_URL", "https://hub.maika.sbs"))
    ap.add_argument("--worker-id", default=os.environ.get("COLAB_WORKER_ID", "colab-omnivoice"))
    ap.add_argument("--api-key", default=(os.environ.get("WORKER_AUTH_TOKEN")
                                          or os.environ.get("WORKER_TOKEN")
                                          or exit("set WORKER_AUTH_TOKEN")))
    ap.add_argument("--model", default=os.environ.get("OMNIVOICE_MODEL", "piper:omnivoice_vi"))
    ap.add_argument("--num-step", type=int, default=int(os.environ.get("OMNIVOICE_STEPS", "24")))
    args = ap.parse_args()
    run(args)


if __name__ == "__main__":
    main()
