# Sever Provider

Unified AI provider orchestrator, deployed as the active production service on `reup@192.168.1.117`.

## Production status (decision 2026-11-24, audit H10)

This repository is not a shadow workspace. Production traffic is served by this binary: `/ready` reported `version=0.1.58` on 2026-11-24 and the service has been running on the host above. The previous README rule ("must not be copied to or started on the production server until ... an explicit cutover decision") is therefore **superseded by observed operation**: documenting a live production deployment as shadow would hide operational risk instead of reducing it.

This is a decision to state reality, not a certification that cutover was clean. The 2026-11-24 production audit records open governance gaps, notably H2 (the running binary is not yet traceable to a clean commit/manifest) and H10 itself. Formal owner sign-off is tracked in `docs/production-audit-2026-11-24.md` §7.1.

Operating rules for changes to this production deployment:

- Build from a clean commit with the version embedded (`make build`), and record the commit/build manifest for every deployment.
- Take and verify a database backup before any schema change or destructive data operation (`scripts/ops/sqlite-backup.py`).
- Use the runbook in `docs/operations-runbook.md` for backup/restore, retention, log rotation and systemd changes.
- Never place cookies, JWTs, Google/TikTok sessions, master keys or production API keys in this repository.

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
- OpenAI/Anthropic-compatible core: `/v1/models`, `/v1/chat/completions`, `/v1/messages`, `/v1/audio/speech`, `/v1/images/generations`, `/v1/videos/*`, `/v1/files/*` (see `PARITY.md` for executable vs optional-backend status, and `docs/chat-api-limits.md` for which sampling parameters are silently ignored, and `docs/video-colab.md` for Wan/LTX video workers on Colab). `/v1/models` is browsable without a key, but a key that is sent is validated and must carry the `models` (or `all`) scope; the catalog is not counted as consumer traffic.
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

The local test suite and `go vet` pass. Race testing requires a local C compiler (`CGO_ENABLED=1`) and is not claimed until that environment is available. Production deployment is active; operational procedures (backup/restore, retention, log rotation, systemd limits and known data-integrity repairs) are documented in `docs/operations-runbook.md`.
