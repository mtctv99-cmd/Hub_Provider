#!/usr/bin/env python3
"""vLLM WorkerHub Client for Google Colab and remote GPU nodes.

Connects to Sever_provider via inbound WebSocket (/api/llm/ws/worker).
Processes inference requests concurrently using either:
  1. Direct vLLM AsyncLLMEngine (in-process, continuous batching)
  2. Upstream vLLM OpenAI API server (--vllm-url)

Usage:
  # Mode 1: Connect to existing vLLM server
  python colab/vllm_worker.py --server ws://127.0.0.1:18080/api/llm/ws/worker \
    --vllm-url http://127.0.0.1:8000 --model Qwen/Qwen2.5-Coder-7B-Instruct

  # Mode 2: Run in-process vLLM engine on GPU
  python colab/vllm_worker.py --server ws://192.168.1.117:18080/api/llm/ws/worker \
    --model Qwen/Qwen2.5-Coder-1.5B-Instruct --gpu-memory-utilization 0.85
"""

import argparse
import asyncio
import json
import logging
import os
import sys
import time
import urllib.parse
from typing import Any, AsyncGenerator, Dict, List, Optional

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("vllm_worker")

# Try importing vllm if available
try:
    from vllm import AsyncEngineArgs, AsyncLLMEngine, SamplingParams
    from vllm.utils import random_uuid
    VLLM_AVAILABLE = True
except ImportError:
    VLLM_AVAILABLE = False


class VLLMWorker:
    def __init__(
        self,
        server_url: str,
        worker_id: str,
        token: str,
        model_name: str,
        model_aliases: List[str],
        vllm_url: Optional[str] = None,
        max_concurrency: int = 8,
        gpu_memory_utilization: float = 0.85,
        max_model_len: int = 8192,
        trust_remote_code: bool = True,
    ):
        self.server_url = server_url
        self.worker_id = worker_id
        self.token = token
        self.model_name = model_name
        self.model_aliases = model_aliases or []
        self.vllm_url = vllm_url.rstrip("/") if vllm_url else None
        self.max_concurrency = max_concurrency
        self.gpu_memory_utilization = gpu_memory_utilization
        self.max_model_len = max_model_len
        self.trust_remote_code = trust_remote_code
        self.engine: Optional[Any] = None
        self.engine_ready = False
        self.engine_task: Optional[asyncio.Task] = None

    async def initialize(self):
        if self.vllm_url:
            logger.info("Using upstream vLLM API server at %s", self.vllm_url)
            return

        if not VLLM_AVAILABLE:
            raise RuntimeError(
                "vllm package not installed and --vllm-url not provided. "
                "Install with: pip install vllm"
            )

        logger.info("Initializing in-process vLLM AsyncLLMEngine for %s...", self.model_name)
        engine_kwargs = dict(
            model=self.model_name,
            trust_remote_code=self.trust_remote_code,
            gpu_memory_utilization=self.gpu_memory_utilization,
            max_model_len=self.max_model_len,
            tensor_parallel_size=1,
        )
        try:
            engine_args = AsyncEngineArgs(**engine_kwargs, disable_log_requests=True)
        except TypeError:
            # Newer vLLM removed disable_log_requests from AsyncEngineArgs.
            engine_args = AsyncEngineArgs(**engine_kwargs)
        self.engine = AsyncLLMEngine.from_engine_args(engine_args)
        logger.info("vLLM AsyncLLMEngine ready.")

    async def stream_from_vllm_http(self, messages: List[Dict[str, Any]], temperature: float, top_p: float, max_tokens: int) -> AsyncGenerator[Dict[str, Any], None]:
        import aiohttp
        payload = {
            "model": self.model_name,
            "messages": messages,
            "temperature": temperature,
            "top_p": top_p,
            "max_tokens": max_tokens,
            "stream": True,
        }
        headers = {"Content-Type": "application/json"}
        url = f"{self.vllm_url}/v1/chat/completions"
        async with aiohttp.ClientSession() as session:
            async with session.post(url, json=payload, headers=headers, timeout=aiohttp.ClientTimeout(total=300)) as resp:
                if resp.status != 200:
                    text = await resp.text()
                    raise RuntimeError(f"vLLM HTTP error {resp.status}: {text}")
                async for line in resp.content:
                    line_str = line.decode("utf-8").strip()
                    if not line_str.startswith("data: "):
                        continue
                    data_str = line_str[6:].strip()
                    if data_str == "[DONE]":
                        break
                    try:
                        chunk = json.loads(data_str)
                        yield chunk
                    except json.JSONDecodeError:
                        continue

    async def run_vllm_http_inference(self, req: Dict[str, Any], ws: Any):
        req_id = req["request_id"]
        messages = req.get("messages", [])
        stream = req.get("stream", False)
        temperature = req.get("temperature", 0.7)
        top_p = req.get("top_p", 0.95)
        max_tokens = req.get("max_tokens", 1024)

        try:
            if stream:
                collected_usage = None
                finish_reason = "stop"
                async for chunk in self.stream_from_vllm_http(messages, temperature, top_p, max_tokens):
                    choices = chunk.get("choices", [])
                    if choices:
                        delta = choices[0].get("delta", {})
                        content = delta.get("content", "")
                        reasoning = delta.get("reasoning_content", "")
                        fr = choices[0].get("finish_reason")
                        if fr:
                            finish_reason = fr
                        if content or reasoning:
                            msg = {
                                "type": "token",
                                "request_id": req_id,
                                "delta": content,
                                "reasoning_content": reasoning,
                                "finish_reason": None,
                            }
                            await ws.send(json.dumps(msg))
                    if chunk.get("usage"):
                        collected_usage = chunk["usage"]

                end_msg = {
                    "type": "token_end",
                    "request_id": req_id,
                    "finish_reason": finish_reason,
                    "usage": collected_usage,
                }
                await ws.send(json.dumps(end_msg))
            else:
                import aiohttp
                payload = {
                    "model": self.model_name,
                    "messages": messages,
                    "temperature": temperature,
                    "top_p": top_p,
                    "max_tokens": max_tokens,
                    "stream": False,
                }
                url = f"{self.vllm_url}/v1/chat/completions"
                async with aiohttp.ClientSession() as session:
                    async with session.post(url, json=payload, timeout=aiohttp.ClientTimeout(total=300)) as resp:
                        if resp.status != 200:
                            err_text = await resp.text()
                            raise RuntimeError(f"vLLM error {resp.status}: {err_text}")
                        data = await resp.json()
                        choices = data.get("choices", [])
                        text = choices[0].get("message", {}).get("content", "") if choices else ""
                        reasoning = choices[0].get("message", {}).get("reasoning_content", "") if choices else ""
                        usage = data.get("usage", {})
                        fr = choices[0].get("finish_reason", "stop") if choices else "stop"

                        await ws.send(json.dumps({
                            "type": "job_result",
                            "request_id": req_id,
                            "text": text,
                            "reasoning_content": reasoning,
                            "usage": usage,
                            "finish_reason": fr,
                        }))
        except Exception as e:
            logger.error("HTTP inference failed for %s: %s", req_id, e)
            await ws.send(json.dumps({"type": "error", "request_id": req_id, "error": str(e)}))

    async def run_direct_vllm_inference(self, req: Dict[str, Any], ws: Any):
        req_id = req["request_id"]
        messages = req.get("messages", [])
        stream = req.get("stream", False)
        temperature = req.get("temperature", 0.7)
        top_p = req.get("top_p", 0.95)
        max_tokens = req.get("max_tokens", 1024)

        try:
            # Build prompt using tokenizer chat template
            tokenizer = await self.engine.get_tokenizer()
            prompt = tokenizer.apply_chat_template(messages, tokenize=False, add_generation_prompt=True)

            sampling_params = SamplingParams(
                temperature=temperature,
                top_p=top_p,
                max_tokens=max_tokens,
            )
            vllm_req_id = f"vllm-{req_id}-{random_uuid()}"
            results_generator = self.engine.generate(prompt, sampling_params, vllm_req_id)

            if stream:
                previous_text = ""
                final_output = None
                async for request_output in results_generator:
                    final_output = request_output
                    text = request_output.outputs[0].text
                    delta = text[len(previous_text):]
                    previous_text = text
                    if delta:
                        await ws.send(json.dumps({
                            "type": "token",
                            "request_id": req_id,
                            "delta": delta,
                            "finish_reason": None,
                        }))

                prompt_tokens = len(final_output.prompt_token_ids) if final_output else 0
                completion_tokens = len(final_output.outputs[0].token_ids) if final_output else 0
                usage = {
                    "prompt_tokens": prompt_tokens,
                    "completion_tokens": completion_tokens,
                    "total_tokens": prompt_tokens + completion_tokens,
                }
                fr = final_output.outputs[0].finish_reason or "stop" if final_output else "stop"
                await ws.send(json.dumps({
                    "type": "token_end",
                    "request_id": req_id,
                    "finish_reason": fr,
                    "usage": usage,
                }))
            else:
                final_output = None
                async for request_output in results_generator:
                    final_output = request_output

                text = final_output.outputs[0].text if final_output else ""
                prompt_tokens = len(final_output.prompt_token_ids) if final_output else 0
                completion_tokens = len(final_output.outputs[0].token_ids) if final_output else 0
                usage = {
                    "prompt_tokens": prompt_tokens,
                    "completion_tokens": completion_tokens,
                    "total_tokens": prompt_tokens + completion_tokens,
                }
                fr = final_output.outputs[0].finish_reason or "stop" if final_output else "stop"
                await ws.send(json.dumps({
                    "type": "job_result",
                    "request_id": req_id,
                    "text": text,
                    "usage": usage,
                    "finish_reason": fr,
                }))
        except Exception as e:
            logger.error("Direct inference failed for %s: %s", req_id, e)
            await ws.send(json.dumps({"type": "error", "request_id": req_id, "error": str(e)}))

    async def handle_request(self, req: Dict[str, Any], ws: Any):
        if not self.engine_ready:
            # Engine still loading (or failed); wait up to 10 min for it.
            try:
                await asyncio.wait_for(self.engine_task, timeout=600)
            except Exception:
                pass
            if not self.engine_ready:
                await ws.send(json.dumps({
                    "type": "error",
                    "request_id": req.get("request_id"),
                    "error": "engine is still loading, retry shortly",
                }))
                return
        if self.vllm_url:
            await self.run_vllm_http_inference(req, ws)
        else:
            await self.run_direct_vllm_inference(req, ws)

    async def run(self):
        import websockets

        all_models = list(dict.fromkeys([self.model_name] + self.model_aliases))
        ws_scheme = "wss" if self.server_url.startswith("https") or self.server_url.startswith("wss") else "ws"
        raw_netloc = self.server_url.split("://", 1)[-1].rstrip("/")
        query = urllib.parse.urlencode({"worker_id": self.worker_id, "token": self.token})
        ws_url = f"{ws_scheme}://{raw_netloc}/api/llm/ws/worker?{query}"

        # Register with the hub FIRST so the gateway shows the worker online
        # while the (slow, engine-core-spawning) vLLM init runs in a side task.
        # Running initialize() inline hangs the registration on Colab: engine
        # core process RPC handshake monopolizes the loop this coroutine runs on.
        self.engine_task = asyncio.get_running_loop().create_task(self._init_engine())

        logger.info("Connecting to WorkerHub at %s...", ws_url)
        while True:
            try:
                async with websockets.connect(ws_url, ping_interval=20, ping_timeout=25) as ws:
                    reg_payload = {
                        "type": "register",
                        "worker_id": self.worker_id,
                        "model": self.model_name,
                        "models": all_models,
                        "capabilities": ["chat", "streaming", "vision", "reasoning"],
                        "max_concurrency": self.max_concurrency,
                    }
                    await ws.send(json.dumps(reg_payload))
                    ack = await ws.recv()
                    logger.info("Connected & registered with WorkerHub: %s", ack)

                    # Background ping loop
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
                            typ = msg.get("type")
                            if typ == "pong":
                                continue
                            if typ == "ping":
                                await ws.send(json.dumps({"type": "pong"}))
                                continue
                            if typ == "chat_request":
                                # Dispatch in background task for concurrent serving
                                asyncio.create_task(self.handle_request(msg, ws))
                    finally:
                        ping_task.cancel()

            except Exception as e:
                logger.warning("WebSocket error: %s. Reconnecting in 3s...", e)
                await asyncio.sleep(3)

    async def _init_engine(self):
        try:
            await self.initialize()
            self.engine_ready = True
            logger.info("vLLM engine READY — serving chat requests.")
        except Exception as e:
            logger.error("vLLM engine init failed: %s", e)


def main():
    parser = argparse.ArgumentParser(description="vLLM Worker for Sever_provider WorkerHub")
    parser.add_argument("--server", default="ws://127.0.0.1:18080/api/llm/ws/worker", help="WorkerHub WebSocket endpoint")
    parser.add_argument("--worker-id", default=f"vllm-{int(time.time())}", help="Unique worker ID")
    parser.add_argument("--token", default=os.environ.get("WORKER_AUTH_TOKEN", "omni-secret-key-123"), help="WorkerHub auth token")
    parser.add_argument("--model", default="Qwen/Qwen3.5-2B", help="Model name or HuggingFace ID")
    parser.add_argument("--alias", action="append", default=[], help="Additional model alias (can be repeated)")
    parser.add_argument("--vllm-url", default=None, help="Existing vLLM OpenAI API URL (e.g. http://127.0.0.1:8000)")
    parser.add_argument("--concurrency", type=int, default=8, help="Max concurrency (default: 8)")
    parser.add_argument("--gpu-mem", type=float, default=0.85, help="GPU memory utilization (0..1)")
    parser.add_argument("--max-len", type=int, default=32768, help="Max model context length (default: 32768)")
    args = parser.parse_args()

    default_aliases = ["qwen3.5-2b", "qwen3-5-2b", "qwen3-vl", "gemma-4-colab", "vllm-default"]
    aliases = list(set(args.alias + default_aliases))

    worker = VLLMWorker(
        server_url=args.server,
        worker_id=args.worker_id,
        token=args.token,
        model_name=args.model,
        model_aliases=aliases,
        vllm_url=args.vllm_url,
        max_concurrency=args.concurrency,
        gpu_memory_utilization=args.gpu_mem,
        max_model_len=args.max_len,
    )
    # Inside a Colab/Jupyter kernel a loop is already running AND vLLM itself
    # creates and runs its own event loop during engine core startup, which
    # stalls any coroutine sharing that thread. Run the whole worker on a
    # dedicated thread with its own loop so neither side blocks the other.
    import threading as _threading

    _result: Dict[str, Any] = {}

    def _worker_main():
        try:
            asyncio.run(worker.run())
        except Exception as exc:  # pragma: no cover - surfaced to caller
            _result["error"] = exc

    try:
        asyncio.get_running_loop()
    except RuntimeError:
        asyncio.run(worker.run())
        return
    thread = _threading.Thread(target=_worker_main, name="vllm-worker-loop", daemon=True)
    thread.start()
    thread.join()


if __name__ == "__main__":
    main()
