#!/bin/bash
set -e

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$DIR"

export PYTHONUNBUFFERED=1
export LD_LIBRARY_PATH="$DIR/venv_tts/lib/nvidia:/home/reup/.local/lib/python3.12/site-packages/nvidia/cu13/lib:/home/reup/.local/lib/python3.12/site-packages/nvidia/cudnn/lib:$LD_LIBRARY_PATH"
PYTHON="$DIR/venv_tts/bin/python"

echo "[RUNNER] Starting dual GPU TTS workers..."

run_worker_0() {
  while true; do
    echo "[GPU 0] Starting worker tts-worker-gpu0..."
    CUDA_VISIBLE_DEVICES=0 $PYTHON scripts/local_tts_worker.py \
      --server ws://127.0.0.1:18080/api/llm/ws/worker \
      --worker-id tts-worker-gpu0 \
      --token omni-secret-key-123 \
      --models-dir models \
      --concurrency 10 \
      --threads-per-voice 3 \
      --gpu \
      --device-id 0 || true
    echo "[GPU 0] Worker exited, restarting in 3s..."
    sleep 3
  done
}

run_worker_1() {
  while true; do
    echo "[GPU 1] Starting worker tts-worker-gpu1..."
    CUDA_VISIBLE_DEVICES=1 $PYTHON scripts/local_tts_worker.py \
      --server ws://127.0.0.1:18080/api/llm/ws/worker \
      --worker-id tts-worker-gpu1 \
      --token omni-secret-key-123 \
      --models-dir models \
      --concurrency 10 \
      --threads-per-voice 3 \
      --gpu \
      --device-id 1 || true
    echo "[GPU 1] Worker exited, restarting in 3s..."
    sleep 3
  done
}

trap 'kill 0' SIGINT SIGTERM EXIT

run_worker_0 &
PID0=$!

run_worker_1 &
PID1=$!

echo "[RUNNER] Workers started: GPU 0 (PID $PID0), GPU 1 (PID $PID1)"
wait
