# Video generation qua Colab (Wan2.1 / LTX-Video)

Gateway nhận job video bất đồng bộ, đẩy qua worker hub WebSocket tới Colab, lưu MP4 vào media store.

## Model

| Model ID | Worker | Cấu hình gợi ý trên T4 | Ghi chú |
|---|---|---|---|
| `wan-2.1-1.3b` | `colab/video_worker.py --backend wan` | 832×480, 49 frame (~3s @16fps), 30 steps | Chất lượng khá, chậm hơn LTX |
| `ltx-video-2b` | `colab/video_worker.py --backend ltx` | 704×480, 121 frame (~5s @24fps), 8 steps | Distilled, nhanh hơn |

Khai báo trong `config/models.yaml` với `capabilities: [video_generation]` và `timeout_sec` lớn (Wan 2700, LTX 1200).

## Gọi API

```bash
curl -sS -X POST https://hub.maika.sbs/v1/videos/generations \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"ltx-video-2b","prompt":"a red tram in rainy Hanoi at dusk","size":"704x480","duration":5}'
# -> 202 {"job_id":"...","status":"pending","status_url":"/v1/videos/tasks/<id>"}

curl -sS https://hub.maika.sbs/v1/videos/tasks/<id> -H "Authorization: Bearer $API_KEY"
# completed -> {"output_json":{"video_url":"/v1/files/<file>","file_id":"..."},...}
```

- `duration` (giây) được worker đổi thành số frame theo fps của backend, trần bởi `--max-frames`.
- Job video dùng timeout riêng 45 phút (`jobs.Runner.VideoTimeout`), không bị reaper 5 phút của chat.
- Kết quả worker là base64 MP4; hub chỉ nhận đúng một `video_result`, tối đa 100MB, kiểm tra magic `ftyp`.
- `video_progress` được bỏ qua (advisory).

## Chạy worker trên Colab

```python
!git clone https://github.com/<you>/Sever_provider /content/sp
!pip -q install --upgrade "diffusers>=0.35" transformers accelerate safetensors imageio imageio-ffmpeg bitsandbytes websockets

import os
os.environ["COLAB_SERVER_URL"] = "wss://hub.maika.sbs"
os.environ["WORKER_AUTH_TOKEN"] = "<worker-token>"
!cd /content/sp && python colab/video_worker.py --backend ltx --model ltx-video-2b --size 704x480 --max-frames 121
```

Wan:

```python
!cd /content/sp && python colab/video_worker.py --backend wan --model wan-2.1-1.3b --size 832x480 --max-frames 49
```

Worker lazy-load pipeline ở request đầu tiên (1–3 phút), sau đó giữ trong VRAM. `max_concurrency=1` nên mỗi worker xử lý tuần tự.

## Chạy trên GPU nhỏ / chia sẻ (ví dụ RTX 3060 Ti 8GB)

```bash
VIDEO_TEXT_ENCODER_CPU=1 VIDEO_VAE_CPU=1 VIDEO_STEPS=8 \
python colab/video_worker.py --backend wan --model wan-2.1-1.3b \
  --size 640x384 --max-frames 33 --device cuda:0 --text-encoder-cpu --vae-cpu
```

- `--text-encoder-cpu`: UMT5 nằm trong RAM host (~11GB fp16), chỉ transformer lên GPU.
- `--vae-cpu`: decode latent trên CPU, tránh OOM lúc decode khi VRAM còn <2GB.
- Đo thực tế: 17 frame 256×192, 8 steps, transformer trên 3060 Ti + T5/VAE CPU → **197.8s**, MP4 H.264 16fps 6.9KB.
- Job timeout phía gateway vẫn 45 phút; hub nới read deadline khi worker đang bận nên generation dài không bị cắt.

## Giới hạn / troubleshooting

- T4 không có bf16/FlashAttention: fp16 cho transformer, VAE fp32, `enable_model_cpu_offload()`, VAE tiling.
- Wan 1.3B dùng UMT5-XXL: mặc định thử 8-bit text encoder (`VIDEO_QUANTIZE_T5=1`); Colab thiếu bitsandbytes thì fallback fp16 và dễ OOM RAM 12.7GB.
- OOM khi generate: giảm `--max-frames` trước (49→33, 121→97) rồi giảm size; giữ chiều rộng chia hết cho 32.
- Session Colab free sống ~2h; job >2h có thể mất worker giữa chừng, gateway trả lỗi job thay vì treo.
- Muốn so sánh nhanh chất lượng/tốc độ: chạy `notebooks/colab_t4_wan_ltx.ipynb`.
