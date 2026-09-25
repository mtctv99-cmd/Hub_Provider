# Remote Piper worker

Build with `go build -o piper-worker ./cmd/piper-worker` on the machine hosting Piper and the voice assets. Install the Piper CLI (`piper` on `PATH`, or set `PIPER_BIN`) and place each `.onnx` model beside its Piper `.onnx.json` metadata. This worker advertises only valid Piper model/metadata pairs. The bundled `khmer_tts_v3` model is **not supported**: its separate vocabulary/ONNX inputs require a different executable pipeline; it is never advertised and an explicit voice request fails.

Supply the WorkerHub authentication token through `PIPER_WORKER_TOKEN` (not a command-line argument or URL). Use `wss://` outside a trusted private network. For example:

```sh
export PIPER_WORKER_TOKEN='your-runtime-secret'
export PIPER_SERVER_URL='wss://YOUR_HOST/api/llm/ws/worker'
export PIPER_MODELS_DIR='/path/to/models'
./piper-worker -id remote-piper-1 -concurrency 1
```

Server-side WorkerHub must be configured with the same token through `COLAB_WORKER_KEY` or `WORKER_AUTH_TOKEN`. The worker sends authentication only in the `X-Worker-Token` header and registration message, never in a query string; it rejects URLs containing `token=`. It logs neither URL nor synthesis text or credentials. The default is one concurrent job globally, with at most one job per voice even if `-concurrency` is raised (up to 8). Extra requests receive a busy error rather than accumulating in an unbounded queue. Jobs time out after 30 seconds, cancellation/disconnect kills subprocesses, text is limited to 32 KiB, and each returned WAV is limited to 16 MiB. Temporary WAV files are removed after use.
