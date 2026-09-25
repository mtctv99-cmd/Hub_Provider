# Feature parity: Sever Provider vs legacy repositories

This document distinguishes **implemented and executable** behavior from route contracts and optional integrations. A route returning `501` is intentionally not counted as a completed provider feature.

## Status legend

- **Implemented**: code path exists and has local unit/integration coverage.
- **Contract**: compatible endpoint exists, but the required external backend is not bundled/configured.
- **Missing**: no equivalent implementation.
- **Legacy-disabled**: present in the old repo but intentionally disabled there too.

## Unified API

| Capability | New Sever Provider | Legacy source | Notes |
|---|---|---|---|
| `GET /health` | Implemented | all repos | Includes uptime, request metrics, worker/job/lease/breaker telemetry. |
| `GET /v1/models` | Implemented/health-aware | `geminigo`, `Sever llm` | Static YAML models plus connected worker-advertised models; configured models now expose `available=false` when their provider breaker is open or Colab has no connected worker. |
| `POST /v1/chat/completions` | Implemented | `geminigo`, `Sever llm` | Non-stream and SSE streaming. Pool allocator, session affinity, and provider breakers wired into dispatch. |
| `POST /v1/messages` | Implemented | `geminigo` | Anthropic-shaped JSON and event-stream responses with session header propagation. |
| `POST /v1/audio/speech` | Implemented | TTS repo | Piper/TikTok adapters with correct MIME responses. |
| `POST /v1/images/generations` | Contract | `geminigo` | Dispatcher/image interface exists; current adapters do not yet implement DALL-E/Imagen generation. |
| `POST /v1/videos/generations` | Contract | `geminigo` | Route/task contract exists; Playwright/CDP Veo backend is not bundled. Returns explicit `501`. |
| `GET /v1/videos/tasks/{id}` | Implemented | `geminigo` | Reads persistent job records. Generation remains backend-dependent. |
| `GET /v1/files/{name}` | Implemented | `geminigo` | Secure filestore prevents traversal and symlink reads. |

## TTS

| Capability | New Sever Provider | Legacy source | Notes |
|---|---|---|---|
| `GET /api/tts/voices` | Implemented/runtime-aware | `TTS_Omivoices` | Discovers flat/nested Piper ONNX models and reports TikTok voice availability from credential readiness. |
| `POST /api/tts/generate` | Implemented/validated | `TTS_Omivoices` | Validates configured speech model before enqueueing; persistent async job processes Piper/TikTok through dispatcher. Piper can run local or remote via `PIPER_TTS_MODE`. |
| `GET /api/tts/jobs/{id}` | Implemented | `TTS_Omivoices` | Shared SQLite job store. |
| `GET /api/tts/jobs/{id}/audio` | Implemented | `TTS_Omivoices` | Correct WAV/MP3 output extension and MIME. |
| `POST /api/tts/clone` | Legacy-disabled | `TTS_Omivoices` | OmniVoice is disabled in the legacy production routing; endpoint explicitly reports this. |
| TikTok account GET | Implemented/read-only | `TTS_Omivoices` | Lists credential metadata without exposing session secrets. |
| TikTok account POST | Implemented/admin-protected | `TTS_Omivoices` | Stores `session_id` encrypted through the identity manager and returns redacted metadata; no plaintext session storage. Admin alias: `/admin/api/tts/tiktok/accounts`. |
| TikTok runtime pool | Implemented/credential-dependent | `TTS_Omivoices` | Startup hydration, round-robin, per-session throttle, 15-minute cooldown, bounded retry and endpoint penalty are implemented. |
| TikTok async capacity | Implemented/configurable | `TTS_Omivoices` | Async runner limits concurrent TikTok jobs using `TIKTOK_WORKER_COUNT` (1–32); pool remains credential-aware. |
| TikTok worker scale | Partial | `TTS_Omivoices` | Runtime capacity control is implemented; a separate persistent dedicated worker thread pool is not copied because SQLite runner already owns job concurrency. |
| Distributed Piper worker | Implemented/optional | `TTS_Omivoices` | `cmd/piper-worker` registers remote voices through authenticated WorkerHub WebSocket, runs Piper on another machine, and returns bounded WAV audio. Server can use `PIPER_TTS_MODE=remote` or `auto` without local Piper. Settings can change mode and enable/disable discovered voices at runtime. |

## Colab / distributed workers

| Capability | New Sever Provider | Legacy source | Notes |
|---|---|---|---|
| `WS /api/llm/ws/worker` | Implemented | `Sever llm` | Inbound worker registration, heartbeat, least-loaded dispatch, max 8 slots, cancellation and fail-fast disconnect. |
| `POST /api/llm/jobs` | Implemented | `Sever llm` | Persistent async job. Uses connected worker first, then configured dispatcher. |
| `GET /api/llm/jobs/{id}` | Implemented | `Sever llm` | Persistent status/result record. |
| `GET /api/llm/colab/accounts` | Implemented/partial | `Sever llm` | Reports connected workers; account discovery is external. |
| `POST /api/llm/colab/rotate` | Contract/supervisor-backed | `Sever llm` | Supported when `COLAB_SUPERVISOR_CMD` and token directory are configured; otherwise returns explicit `501`. |
| `POST /api/llm/colab/start` | Contract/supervisor-backed | `Sever llm` | Supported when supervisor is configured; otherwise returns explicit `501`. |
| `POST /api/llm/colab/stop` | Implemented | `Sever llm` | Stops an attached worker by worker ID. |
| 8-slot worker concurrency | Implemented | `Sever llm` | Enforced by worker hub. |
| Colab 3h/155m/16h rotator | Partial/supervisor contract | `Sever llm` | `internal/colabsupervisor` models token discovery, cooldown persistence and an external command hook; actual `google-colab-cli` provisioning/prewarm remains external. |
| WARP SOCKS5 routing | Missing/optional | `Sever llm` | Host-level dependency; not embedded in Go core. |

## LLM/provider capabilities

| Capability | New Sever Provider | Legacy source | Notes |
|---|---|---|---|
| Gemini Web RPC | Implemented/credential-dependent | `geminigo` | Token/SAPISIDHASH/payload/parser path is ported; token page endpoint follows the adapter base URL for fixtures and controlled deployments. Requires valid Google session cookies for production. |
| Gemini incremental streaming | Implemented/credential-dependent | `geminigo` | Line-by-line `bufio.Reader`, cumulative `wrb.fr` parsing, incremental delta diffing, context cancellation and upstream metadata propagation. Handles upstream text revisions. |
| Gemini Guest Mode | Implemented/endpoint-dependent | `geminigo` | Credential-less scheduler path for `gemini-3.5-flash-lite`; calls the real Gemini Web RPC with an empty credential and never mutates credential status. |
| Gemini upstream session continuity | Implemented/tested | `geminigo` | `ConversationID`, `ResponseID`, and `ChoiceID` are returned by adapters and persisted on the sticky session binding for subsequent turns. |
| Copilot SignalR chat | Implemented/credential-dependent | `geminigo` | Token URL validation, SignalR framing, streaming, and encrypted admin provisioning route (`/admin/api/copilot/add`) are implemented. Requires a live Copilot token URL. |
| Copilot upstream session continuity | Implemented/tested | `geminigo` | Conversation ID, chat session ID, and invocation ID travel through generic upstream/session bindings across `X-Session-ID` turns. |
| Copilot DALL-E image extraction | Implemented/credential-dependent | `geminigo` | SignalR completion frames are scanned for image URLs, downloads are size/MIME/redirect bounded, and bytes are returned through the secure filestore. Live Copilot credentials are required. |
| Sticky sessions | Implemented | `geminigo` | In-memory session manager with credential affinity, 25-turn max, 30-minute idle TTL, periodic sweep. |
| Pool 40/60 + overload borrowing | Implemented | `geminigo` | Pool allocator with AUTO/SESSION/WORKER roles, least-active preference, borrow limit of 2. Wired into dispatcher. |
| Provider circuit breakers | Implemented | `geminigo` | Provider breaker primitives with Allow/Success/Failure. Wired into dispatcher credential loop. |
| Vision input | Implemented/credential-dependent | `geminigo` | Data URL/remote URL parsing with 16 MB bounds, Scotty resumable upload, ProcessFile registration and file references in Gemini payloads. Requires real Gemini credentials; invalid/unsupported payloads fail closed. |
| DALL-E 3 via Copilot | Implemented/credential-dependent | `geminigo` | `dall-e-3` is configured; the adapter extracts Adaptive Card image URLs and downloads validated image bytes. Requires a valid Copilot token URL. |
| Imagen | Contract | `geminigo` | Gemini Imagen backend is not bundled. |
| Veo video | Contract | `geminigo` | Requires Playwright/CDP backend. |
| Dynamic model sync | Partial | `geminigo`, `Sever llm` | Worker models are surfaced. Provider-account probing returns explicit `501`. |

## Operations and security

| Capability | New Sever Provider | Legacy source | Notes |
|---|---|---|---|
| AES-GCM encrypted credential store | Implemented/hardened | local rewrite | No key logging; launcher no longer embeds key. Ephemeral key warning on missing `MASTER_KEY`. |
| Persistent SQLite jobs | Implemented | `Sever llm`, `TTS_Omivoices` | Shared schema and atomic claim transitions. |
| Secure media store | Implemented | `geminigo`, TTS | Atomic writes, safe IDs, traversal/symlink rejection, cleanup. |
| Lease TTL/reclaim | Implemented | local rewrite | Background reclaimer wired in main. |
| Maintenance supervisor | Implemented | local rewrite | Periodic stale job reset (10 min), old job cleanup (7 days), media cleanup (24h), session sweep. |
| API Key CRUD (SQLite) | Implemented | `geminigo` | SHA-256 hashed storage. Create/List/Revoke/Delete. Plaintext returned only on creation. |
| API Key middleware | Implemented | `geminigo` | Static key + SQLite keys. Path-scoped: admin/internal/health/web exempt. Invalid/revoked keys rejected even if no active keys remain. |
| Audit/observability logs | Implemented | `geminigo` | Persistent `observability_events` table. Record/Recent/Clear via admin API (`/admin/api/logs`, `/admin/api/logs/clear`). |
| Admin login/session | Implemented | `geminigo` | In-memory sessions. HttpOnly cookie + bearer token. 24h TTL. Login/logout/check/stats/sessions/models. |
| Shared Google account center | Implemented | `geminigo`, `Sever llm` | Identity-centric model. Normalized email dedup. Gemini+Colab credentials on one identity. Discover/import/disable/validate/delete routes. W2A-style dashboard cards. |
| Google credential route parsing | Implemented/fixed | local rewrite | Segment-based URL parsing; `path.Base` bug fixed. Tested. |
| Embedded dashboard | Implemented | all repos | W2A-style tabbed shell: Overview, Google Accounts, Models, TTS Studio, Live Logs. Semantic status badges. |
| Provider & account breakers | Implemented | local rewrite | Provider and account breaker Allow checks and Success/Failure recording in dispatch loop; credential status and cooldown persisted in DB. |
| Watchdog restart | Missing | `Sever llm`, TTS | Prefer systemd/container supervisor in production. |

## Remaining gaps before production cutover / complete legacy parity

1. Gemini multimodal pipeline is implemented and tested locally (Scotty resumable upload, `ProcessFile` RPC, data URL parsing, MIME sniffing and attachment refs in payload). Production use requires valid Google session cookies.
2. Veo browser backend remains a contract/501; Playwright/CDP is intentionally not bundled in the Go core.
3. Browser-based Copilot and Google token refresh flows remain external/manual; browser auto-login is intentionally not bundled in the core Go binary.
4. Dedicated Piper smart worker scaling, RAM model load/unload and model-directory voice discovery.
5. Add TikTok account provisioning, persistent session-pool hydration and dynamic worker scaling.
6. Add the actual `google-colab-cli` provisioning/prewarm/zero-worker recovery implementation behind the existing supervisor contract; WARP/SOCKS5 routing remains external.
7. Persist admin sessions in `admin_sessions` if restart-survivable admin login is required; current admin sessions are intentionally in-memory.
8. Add integration, race, load and browser tests against live provider fixtures. Local unit/mock verification currently covers Gemini RPC, streaming, guest mode, upstream session metadata, Scotty multimodal upload, and the rest of the Go test suite (18 passing test packages).

These items are intentionally not marked complete until their external runtime dependencies and tests exist. The local unified control plane, routing, persistence, lease/session/pool/breaker core, admin/account UI, maintenance, and security baseline are implemented and verified separately.
