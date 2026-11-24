#!/usr/bin/env python3
"""Colab video worker for Sever Provider (Wan2.1-1.3B / LTX-Video 2B).

Connects to the gateway worker hub, registers one video model, and answers
``video_request`` with a base64 MP4 in ``video_result``.

Run on a Colab GPU runtime:

    COLAB_SERVER_URL=wss://hub.maika.sbs/api/llm/ws/worker \
    WORKER_AUTH_TOKEN=... \
    python colab/video_worker.py --backend wan --model wan-2.1-1.3b

T4 notes: fp16 only (no bf16, no FlashAttention), VAE fp32, CPU offload.
"""
import argparse
import asyncio
import base64
import gc
import json
import logging
import os
import subprocess
import sys
import tempfile
import threading
import time
from urllib.parse import urlencode

os.environ.setdefault("PYTORCH_CUDA_ALLOC_CONF", "expandable_segments:True")
if os.path.isdir("/content") and os.access("/content", os.W_OK):
    os.environ.setdefault("HF_HOME", "/content/hf")

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("video-worker")
logger.setLevel(logging.INFO)
logger.propagate = False
if not logger.handlers:
    _handler = logging.StreamHandler(sys.stdout)
    _handler.setFormatter(logging.Formatter("%(asctime)s [%(levelname)s] %(message)s"))
    logger.addHandler(_handler)

import importlib.util
from importlib.metadata import PackageNotFoundError, version

REQUIRED = ["websockets", "diffusers", "transformers", "accelerate", "safetensors",
            "bitsandbytes", "imageio", "imageio_ffmpeg"]
IMPORTS = {"imageio_ffmpeg": "imageio-ffmpeg"}
missing = [IMPORTS.get(p, p) for p in REQUIRED if importlib.util.find_spec(p) is None]


def _older(pkg, minimum):
    try:
        have = tuple(int(x) for x in version(pkg).split(".")[:3] if x.isdigit())
    except (PackageNotFoundError, ValueError):
        return True
    want = tuple(int(x) for x in minimum.split("."))
    return have < want


# LTX needs AutoModel.from_single_file (diffusers >= 0.36) and a modern T5 API.
if _older("diffusers", "0.36"):
    missing.append("diffusers>=0.36")
if _older("transformers", "4.45"):
    missing.append("transformers>=4.45")
if missing and os.environ.get("VIDEO_SKIP_INSTALL") != "1":
    logger.info("installing %s", missing)
    subprocess.check_call([sys.executable, "-m", "pip", "install"] + missing)
logger.info("dependencies ready")

import torch  # noqa: E402


def parse_size(value, default=(832, 480)):
    try:
        w, h = value.lower().split("x")
        return int(w), int(h)
    except Exception:
        return default


class WanBackend:
    """Wan2.1 T2V 1.3B, fp16 transformer + fp32 VAE, CPU offload."""

    fps = 16
    default_frames = 49
    default_steps = int(os.environ.get("VIDEO_STEPS", "30"))
    guidance = 6.0

    def __init__(self, model_id="Wan-AI/Wan2.1-T2V-1.3B-Diffusers", device=None, text_encoder_cpu=False,
                 vae_cpu=False):
        from diffusers import AutoencoderKLWan, WanPipeline

        self.text_encoder_cpu = text_encoder_cpu
        self.vae_cpu = vae_cpu
        logger.info("wan: loading VAE")
        vae = AutoencoderKLWan.from_pretrained(model_id, subfolder="vae", dtype=torch.float32)
        kwargs = {}
        if not text_encoder_cpu and os.environ.get("VIDEO_QUANTIZE_T5", "1") == "1":
            try:
                from diffusers.quantizers import PipelineQuantizationConfig

                kwargs["quantization_config"] = PipelineQuantizationConfig(
                    quant_backend="bitsandbytes_8bit",
                    quant_kwargs={"load_in_8bit": True},
                    components_to_quantize=["text_encoder"],
                )
            except Exception as exc:  # pragma: no cover - depends on Colab image
                logger.warning("8-bit text encoder unavailable: %s", exc)
        logger.info("wan: loading transformer/text encoder (8-bit=%s)", bool(kwargs))
        self.pipe = WanPipeline.from_pretrained(model_id, vae=vae, dtype=torch.float16, **kwargs)
        logger.info("wan: pipeline loaded, enabling offload")
        if os.environ.get("VIDEO_SEQUENTIAL_OFFLOAD") == "1":
            # One submodule on GPU at a time; fits beside other services.
            self.pipe.enable_sequential_cpu_offload()
        elif text_encoder_cpu:
            # Small-VRAM hosts: UMT5 stays in host RAM, only transformer+VAE use the GPU.
            self.pipe.text_encoder.to("cpu")
            self.pipe.transformer.to(device or "cuda")
            if vae_cpu:
                self.pipe.vae.to("cpu")
            else:
                self.pipe.vae.to(device or "cuda")
        else:
            self.pipe.enable_model_cpu_offload()
        try:
            self.pipe.vae.enable_tiling()
        except Exception:
            pass

    def generate(self, prompt, size, frames, steps, negative):
        width, height = parse_size(size, (832, 480))
        common = dict(
            negative_prompt=negative,
            height=height,
            width=width,
            num_frames=max(9, frames),
            num_inference_steps=max(8, steps or self.default_steps),
            guidance_scale=self.guidance,
        )
        if self.vae_cpu:
            common["output_type"] = "latent"
        if self.text_encoder_cpu:
            prompt_embeds, negative_embeds = self.pipe.encode_prompt(
                prompt, negative_prompt=negative, do_classifier_free_guidance=True,
                num_videos_per_prompt=1, max_sequence_length=512, device=torch.device("cpu"),
            )
            device = self.pipe._execution_device
            common.pop("negative_prompt")
            latents = self.pipe(
                prompt_embeds=prompt_embeds.to(device),
                negative_prompt_embeds=negative_embeds.to(device),
                **common,
            ).frames
        else:
            latents = self.pipe(prompt=prompt, **common).frames
        if self.vae_cpu:
            vae = self.pipe.vae
            latents = latents.to(device=vae.device, dtype=vae.dtype)
            mean = torch.tensor(vae.config.latents_mean).view(1, vae.config.z_dim, 1, 1, 1).to(latents)
            std = 1.0 / torch.tensor(vae.config.latents_std).view(1, vae.config.z_dim, 1, 1, 1).to(latents)
            with torch.no_grad():
                video = vae.decode(latents / std + mean, return_dict=False)[0]
            out = self.pipe.video_processor.postprocess_video(video.detach(), output_type="pil")[0]
        else:
            out = latents
        return out, self.fps


class LTXBackend:
    """LTX-Video 2B 0.9.8 distilled: single-file transformer + shared T5/VAE."""

    fps = 24
    default_frames = 121
    default_steps = 8
    guidance = 1.0

    def __init__(self, base_id="Lightricks/LTX-Video"):
        from diffusers import (AutoencoderKLLTXVideo, FlowMatchEulerDiscreteScheduler,
                               LTXPipeline, LTXVideoTransformer3DModel)
        from huggingface_hub import hf_hub_download
        from transformers import T5EncoderModel, T5TokenizerFast

        logger.info("ltx: downloading/loading components")
        single = hf_hub_download(base_id, "ltxv-2b-0.9.8-distilled.safetensors")
        logger.info("ltx: transformer file %s", single)
        tokenizer = T5TokenizerFast.from_pretrained(base_id, subfolder="tokenizer")
        text_encoder = T5EncoderModel.from_pretrained(base_id, subfolder="text_encoder", torch_dtype=torch.float16)
        vae = AutoencoderKLLTXVideo.from_pretrained(base_id, subfolder="vae", torch_dtype=torch.float32)
        transformer = LTXVideoTransformer3DModel.from_single_file(
            single, config=base_id, subfolder="transformer", torch_dtype=torch.float16)
        logger.info("ltx: components loaded")
        scheduler = FlowMatchEulerDiscreteScheduler.from_pretrained(base_id, subfolder="scheduler")
        self.pipe = LTXPipeline(
            tokenizer=tokenizer,
            text_encoder=text_encoder,
            vae=vae,
            scheduler=scheduler,
            transformer=transformer,
        )
        self.pipe.enable_model_cpu_offload()
        try:
            self.pipe.vae.enable_tiling()
        except Exception:
            pass

    def generate(self, prompt, size, frames, steps, negative):
        width, height = parse_size(size, (704, 480))
        out = self.pipe(
            prompt=prompt,
            negative_prompt=negative,
            width=width,
            height=height,
            num_frames=max(17, frames),
            num_inference_steps=max(4, steps or self.default_steps),
            guidance_scale=self.guidance,
            decode_timestep=0.05,
            decode_noise_scale=0.025,
        ).frames[0]
        return out, self.fps


class Worker:
    def __init__(self, args):
        self.args = args
        self.backend = None

    def load(self):
        if self.backend is None:
            logger.info("loading backend %s", self.args.backend)
            if self.args.backend == "wan":
                self.backend = WanBackend(device=self.args.device,
                                          text_encoder_cpu=self.args.text_encoder_cpu,
                                          vae_cpu=self.args.vae_cpu)
            else:
                self.backend = LTXBackend()
            logger.info("backend ready")
        return self.backend

    def handle(self, msg):
        started = time.time()
        request_id = msg.get("request_id")
        prompt = (msg.get("prompt") or "").strip()
        logger.info("video_request %s (%d chars, size=%s)", request_id, len(prompt), msg.get("size"))
        if not prompt:
            return {"type": "error", "request_id": request_id, "error": "video prompt is required"}
        size = msg.get("size") or self.args.size
        steps = int(msg.get("steps") or 0)
        duration = int(msg.get("duration") or 0)
        negative = msg.get("negative_prompt") or "worst quality, blurry, jittery, distorted"
        try:
            backend = self.load()
            from diffusers.utils import export_to_video

            frames = int(msg.get("num_frames") or 0)
            if not frames:
                frames = min(duration * backend.fps, backend.default_frames) if duration else self.args.max_frames
            frames = min(frames, self.args.max_frames)
            result, fps = backend.generate(prompt, size, frames, steps, negative)
            with tempfile.NamedTemporaryFile(suffix=".mp4", delete=False) as tmp:
                path = tmp.name
            export_to_video(result, path, fps=fps)
            with open(path, "rb") as fh:
                data = fh.read()
            os.unlink(path)
            logger.info("request %s: %d frames in %.1fs, %d bytes",
                        request_id, len(result), time.time() - started, len(data))
            save_dir = os.environ.get("VIDEO_SAVE_DIR")
            if save_dir:
                try:
                    os.makedirs(save_dir, exist_ok=True)
                    with open(os.path.join(save_dir, f"{request_id}.mp4"), "wb") as fh:
                        fh.write(data)
                except OSError as exc:
                    logger.warning("could not save video copy: %s", exc)
            return {
                "type": "video_result",
                "request_id": request_id,
                "mime": "video/mp4",
                "video_base64": base64.b64encode(data).decode("ascii"),
                "done": True,
            }
        except torch.cuda.OutOfMemoryError:
            gc.collect()
            torch.cuda.empty_cache()
            return {"type": "error", "request_id": request_id,
                    "error": "video generation ran out of GPU memory; reduce num_frames or size"}
        except Exception as exc:  # keep the worker alive for the next request
            logger.exception("video request failed")
            return {"type": "error", "request_id": request_id, "error": str(exc)[:500]}


def run(args):
    # websockets.sync keeps protocol heartbeats on its own thread, so a long
    # diffusers call cannot starve the hub's keepalive ping (async loop does).
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
    worker = Worker(args)
    logger.info("dialing %s://%s/api/llm/ws/worker (model=%s, backend=%s)",
                scheme, host, args.model, args.backend)

    while True:
        try:
            logger.info("connecting to %s", host)
            with connect(url, max_size=16 * 1024 * 1024, ping_interval=20, ping_timeout=600) as ws:
                ws.send(json.dumps({
                    "type": "register",
                    "worker_id": args.worker_id,
                    "model": args.model,
                    "models": [args.model],
                    "capabilities": ["video_generation"],
                    "max_concurrency": 1,
                }))
                logger.info("connected; ack=%s", ws.recv())
                stop = threading.Event()

                def keepalive():
                    # Cloudflare Tunnel drops idle WebSockets; control pings alone
                    # are not enough while a generation blocks the read loop.
                    while not stop.wait(15):
                        try:
                            ws.send(json.dumps({"type": "ping"}))
                        except Exception:
                            return

                threading.Thread(target=keepalive, daemon=True).start()

                try:
                    while True:
                        raw = ws.recv()
                        try:
                            msg = json.loads(raw)
                        except json.JSONDecodeError:
                            continue
                        if msg.get("type") == "ping":
                            ws.send(json.dumps({"type": "pong"}))
                            continue
                        if msg.get("type") != "video_request":
                            continue
                        ws.send(json.dumps(worker.handle(msg)))
                finally:
                    stop.set()
        except Exception as exc:
            logger.warning("connection lost (%r); retrying in 15s", exc, exc_info=True)
            time.sleep(15)


def main():
    ap = argparse.ArgumentParser(description="Sever Provider Colab video worker")
    ap.add_argument("--server-url", default=os.environ.get("COLAB_SERVER_URL", "https://hub.maika.sbs"))
    ap.add_argument("--worker-id", default=os.environ.get("COLAB_WORKER_ID", "colab-video"))
    ap.add_argument("--api-key", default=(os.environ.get("WORKER_AUTH_TOKEN")
                                          or os.environ.get("WORKER_TOKEN")
                                          or exit("set WORKER_AUTH_TOKEN")))
    ap.add_argument("--model", default=os.environ.get("VIDEO_MODEL", "wan-2.1-1.3b"))
    ap.add_argument("--backend", choices=["wan", "ltx"], default=os.environ.get("VIDEO_BACKEND", "wan"))
    ap.add_argument("--size", default=os.environ.get("VIDEO_SIZE", "832x480"))
    ap.add_argument("--max-frames", type=int, default=int(os.environ.get("VIDEO_MAX_FRAMES", "49")))
    ap.add_argument("--device", default=os.environ.get("VIDEO_DEVICE", "cuda"))
    ap.add_argument("--text-encoder-cpu", action="store_true",
                    default=os.environ.get("VIDEO_TEXT_ENCODER_CPU") == "1",
                    help="keep T5/UMT5 in host RAM (small-VRAM GPUs)")
    ap.add_argument("--vae-cpu", action="store_true",
                    default=os.environ.get("VIDEO_VAE_CPU") == "1",
                    help="decode latents on CPU to save VRAM")
    args = ap.parse_args()
    run(args)


if __name__ == "__main__":
    main()
