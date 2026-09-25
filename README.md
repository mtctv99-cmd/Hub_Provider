# Sever Provider

Unified AI provider orchestrator, built locally before any production cutover.

## Scope

This repository is the clean rewrite workspace. It must not be copied to or started on the production server until provider validation, integration tests, load tests, credential migration review, and an explicit cutover decision are complete.

## Current architecture

- Go core with `net/http` gateway and embedded dashboard.
- SQLite WAL persistence owned by the core process.
- AES-256-GCM credential secret store; secrets are supplied at runtime.
- Identity and credential domains are separate (`gemini_web`, `google_colab`, `copilot_ws`, `piper_local`, `tiktok_api`).
- Runtime lease capacity is separate from persistent credential status.
- Provider adapters:
  - Gemini Web RPC: page token acquisition, SAPISIDHASH, 69-slot `f.req`, response parser.
  - Copilot SignalR: token URL injection, SignalR record separator/framing, invocation template.
  - Colab worker WebSocket: register, ping/pong, chat request, token stream, result/error events.
  - Piper: local binary/model execution.
  - TikTok: bounded text splitting, session pool, rate-limit cooldown and base64 MP3 response.
- Scheduler primitives: model registry, leases with TTL/reclaim, account/provider breakers and canonical error classification.
- OpenAI/Anthropic-compatible core: `/v1/models`, `/v1/chat/completions`, `/v1/messages`, `/v1/audio/speech`, `/v1/images/generations`, `/v1/videos/*`, `/v1/files/*` (see `PARITY.md` for executable vs optional-backend status).
- Persistent async jobs for TTS and LLM, secure media filestore, inbound Colab worker hub, and worker-advertised model discovery.
- Compatibility APIs: `/api/tts/*` (including legacy synchronous `/api/tts/synthesize`), `/api/llm/*`, `/internal/status`, `/internal/events`.
- Operational probes: `/health` is liveness; `/ready` is readiness and fails when the core dispatcher/models are not initialized.
- Gateway layers: route registration, request limits/API-key and telemetry auth middleware, panic recovery, then application handlers; provider dispatch is isolated behind scheduler leases and breakers.
- Shutdown lifecycle: HTTP server, runner, supervisor, lease reclaimer, Colab supervisor loop and worker hub share a cancellable application context.

## Local development

```powershell
go mod tidy
go test ./...
go vet ./...
go build -o sever-provider.exe ./cmd/server
$env:SERVER_PORT = "18080"
./sever-provider.exe -config config
```

The real external provider adapters require runtime-injected credentials and URLs. No cookie, JWT, Google token, TikTok session ID, or production API key belongs in this repository.

## Verification status

The local test suite and `go vet` pass. Race testing requires a local C compiler (`CGO_ENABLED=1`) and is not claimed until that environment is available. Production deployment is intentionally out of scope for this stage.
