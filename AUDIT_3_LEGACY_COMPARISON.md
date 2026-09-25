# Audit đối chiếu Sever Provider với 3 repository legacy

Ngày audit: 2026-09-22

Phạm vi:

- Local: `C:\Users\MaiThanhChu\Desktop\Sever_provider`
- Remote read-only:
  - `/media/reup/Data_sv4/TTS_Omivoices`
  - `/media/reup/Data_sv4/Sever llm`
  - `/media/reup/Data_sv4/geminigo`
- Không sửa remote, không đọc/ghi trả về secret, cookie, refresh token hoặc API key.

## Kết luận ngắn

Sever Provider hiện đã có **unified control plane tốt hơn về cấu trúc, persistence, security baseline và testability**, nhưng chưa đạt superset runtime của ba repo cũ. Các gap quan trọng nhất không còn là route contracts mà là khả năng chạy thật:

1. **P0 — Lease ownership sai:** dispatcher register capacity cho credential A nhưng `LeaseManager.Acquire(domain, model)` có thể cấp lease của credential B. Điều này phá account affinity, pool role và có thể gán kết quả/lỗi cho sai account.
2. **P0 — Provider startup chưa usable nếu chưa provision:** TikTok dùng pool rỗng, Copilot không có invocation/template, Google credentials không được seed; route có nhưng runtime provider chưa sẵn sàng.
3. **P0 — `/internal/status` và `/internal/events` bypass API-key/admin auth:** nếu bind ngoài loopback, metadata account/credential/worker có thể bị lộ.
4. **P1 — Audit logger chỉ có storage/API, chưa được gọi trong production dispatch/gateway:** logs thường vẫn rỗng.
5. **P1 — Colab rotate trả `202 queued` nhưng không tạo job/không thực hiện rotate.** Đây là fake success và cần `501` hoặc real supervisor job.
6. **P1 — Image/video và multimodal vẫn là contract:** adapter hình ảnh hiện trả unsupported; video trả `501`; Gemini/Copilot chưa upload/extract media thật.
7. **P1 — Failover model được khai báo ở scheduler nhưng config không map `failover_to`, dispatcher không thử model thay thế.**
8. **P2 — Chưa có Colab rotator 4h/16h, WARP, Piper voice discovery/dynamic loading, TikTok provisioning/scaling thật, media expiry DB integration, admin sessions persistent.**

## 1. So sánh kiến trúc tổng thể

| Hạng mục | TTS_Omivoices | Sever llm | geminigo | Sever Provider hiện tại |
|---|---|---|---|---|
| Runtime | Python/FastAPI/Uvicorn | Python/FastAPI/Uvicorn | Go + Playwright/Node | Go binary + SQLite |
| Vai trò | TTS chuyên dụng | Colab LLM gateway | Gemini/Copilot + image/video | Unified control plane |
| Persistence | SQLite `tts.db` | SQLite `llm.db` | JSON files | SQLite schema chuẩn hóa |
| API | `/api/tts/*` | `/v1/*`, `/api/llm/*` | `/v1/*`, admin | Unified `/v1/*`, `/api/tts/*`, `/api/llm/*`, admin |
| Admin | TUI/host scripts | API key admin | SPA + admin session | W2A-style SPA + admin session |
| Test | Không có test tự động đáng kể | Pytest khá đầy đủ | Go tests + manual scripts | Go unit/integration tests; chưa có real-provider/load/race coverage |
| Watchdog | Có restart/CPU guard/Cloudflare | Có restart + health monitor | Background cleanup/refresh | Có maintenance loop; chưa có process watchdog |

**Đánh giá:** Sever Provider thắng về một binary, SQLite transactions, encrypted secrets, API key hashing, secure filestore, route consistency và test baseline. Legacy vẫn thắng về các integration runtime đặc thù: Playwright/Xvfb, Colab lifecycle, Piper smart pool và media generation.

## 2. TTS_Omivoices đối chiếu

### Legacy có thật

| Capability | Bằng chứng legacy | Sever Provider |
|---|---|---|
| Piper CPU ONNX | `app/services/piper_service.py`, `app/core/job_queue.py:339` | Có adapter Piper, nhưng chưa có smart voice pool/load-unload tương đương |
| Dynamic Piper worker/model balancing | `SmartPiperWorkerPool` | Chưa có; voice catalog hiện hard-code trong `internal/gateway/superset_handlers.go:88-99` |
| TikTok TTS | `app/services/tiktok_tts_service.py` | Có adapter + UTF-8 chunk/cooldown primitives |
| TikTok session rotation | `TikTokSessionPool`, SQLite `tiktok_accounts` | Credential model có thể chứa secret, nhưng `main.go` khởi tạo `tiktok.New(nil,nil)` và không provision pool thực tế |
| TikTok account POST | `app/api/tts.py:228` | Route hiện contract/501 hoặc partial; chưa có provisioning thật |
| TikTok worker scale 1–32 | `app/api/tts.py:238-248` | Chưa có scaling runtime thật |
| Piper/TikTok async jobs | `app/api/tts.py:130`, jobs table | Có `jobstore`, runner và `/api/tts/jobs/*` |
| Cleaner 1h | `app/core/cleaner.py` | Có supervisor, nhưng cleanup mặc định media 24h và jobs 7 ngày |
| OmniVoice clone | Code có nhưng production disabled, `app/api/tts.py:256` | Đúng khi giữ explicit disabled/501 |
| TUI account/queue | `tui.py` | Chưa có TUI; có web dashboard |

### Kết luận TTS

Sever Provider đã có contract và queue tốt hơn nhưng **chưa đạt parity runtime TTS**. Đặc biệt, không nên ghi “TikTok implemented” nếu chưa import/session secret và pool thực tế. Cần tách status:

- Piper local: usable nếu binary/model directory được cấu hình.
- TikTok: adapter tồn tại nhưng runtime unavailable khi pool rỗng.
- Dynamic voice discovery: missing.
- Account CRUD/scaling: contract.

## 3. Sever llm đối chiếu

### Legacy có thật

| Capability | Bằng chứng legacy | Sever Provider |
|---|---|---|
| OpenAI chat + SSE | `app/api/v1_chat.py:35` | Có `/v1/chat/completions` + SSE |
| Async jobs | `app/api/jobs.py` | Có persistent `jobstore` + runner |
| Inbound worker WS | `app/api/worker_ws.py` | Có `workerhub`, max 8 slots, stream/cancel/disconnect |
| Token query auth worker | `worker_ws.py` | Có optional `AuthToken`; bật qua `COLAB_WORKER_KEY`/`WORKER_AUTH_TOKEN`, nhưng env rỗng thì vẫn open |
| Per-worker concurrency | `app/core/worker_manager.py` | Có worker active slots |
| Heartbeat/stale sweep | worker manager | Có heartbeat; cần kiểm tra persistence/telemetry đầy đủ |
| Colab account discovery | `app/automation/colab_runner.py` | Chưa có; `/api/llm/colab/accounts` chủ yếu phản ánh connected workers |
| 4h runtime + 16h cooldown | `app/automation/rotator.py` | Chưa có |
| Pre-warm successor 10 phút | `rotator.py` | Chưa có |
| Zero-worker recovery | `rotator.py` | Chưa có |
| WARP SOCKS5 | deployment/runtime | Chưa có |
| Mock worker mode | `colab/mock_worker.py`, `run.sh --mock-worker` | Có unit mock ở workerhub nhưng chưa có equivalent launch mode |
| Tests | `tests/test_*.py` | Go tests tốt hơn legacy TTS, nhưng chưa real Colab integration |

### Kết luận Colab

Sever Provider đã port đúng phần **protocol worker inbound**, nhưng chưa port phần **lifecycle supervisor**. Vì vậy `/api/llm/colab/start`, `/rotate` không thể được xem là hoàn tất. Đặc biệt `ColabRotate` hiện trả `202 queued` nhưng không tạo persistent job hay gọi supervisor — cần sửa thành `501` cho trung thực hoặc triển khai job thật.

## 4. geminigo đối chiếu

### Legacy có thật

| Capability | Bằng chứng legacy | Sever Provider |
|---|---|---|
| Gemini Web RPC | `internal/client/` | Có token/payload/parser/stream cơ bản |
| Gemini SSE streaming | `internal/server/server.go:803` | Có incremental `wrb.fr` line parsing |
| Gemini server-side sessions | cookie/session store | Có sticky manager 25 turns/30m, nhưng chưa lưu upstream conversation IDs |
| 40/60 pool | `internal/client/cookie.go:362` | Có role allocator/borrowing primitives; chưa có quota/counter 40/60 thật |
| Account cooldown/auth handling | cookie pool | Có breaker/status pipeline, nhưng persistence breaker còn thiếu |
| Copilot SignalR | `internal/copilot/` | Có framing/adapter, nhưng `main.go` dùng `copilot.New(nil)` nên chưa usable nếu thiếu config/template |
| DALL-E 3 | `internal/server/image.go` | Chưa có; Copilot `Image` unsupported |
| Imagen 3 | RPC + Playwright fallback | Chưa có; Gemini `Image` unsupported |
| Veo | Playwright/CDP async task | Route có nhưng trả `501` |
| Gemini multimodal upload | `internal/client/multimodal.go:212`, Scotty/push upload | Request schema có image parts/16MB check nhưng chưa upload/ProcessFile |
| Guest mode | Gemini lite | Chưa có |
| Admin cookie add/refresh/delete | `server.go:225-484` | Google import/discover/disable/validate có; discover rỗng, PATCH/rotate 501 |
| Copilot auto-login/refresh | `server.go:1966+` | Compatibility routes nhưng explicit unsupported |
| Client API key CRUD | `server.go:519-609` | Có SQLite hash/create/list/revoke/delete + middleware |
| Dynamic model sync | `server.go:652` | Explicit 501, chưa có provider probing |
| Logs/clear | `server.go:672-684` | Có persistent observability API; production event recording vẫn chưa wire đầy đủ |
| Docker/Chromium deployment | `Dockerfile`, compose | Chưa có equivalent browser runtime bundle |

### Kết luận Gemini/Copilot

Sever Provider giữ được kiến trúc control-plane sạch hơn và không port các automation browser nguy hiểm một cách mù quáng, nhưng **chưa phải superset chức năng runtime** của geminigo. Ba phần lớn còn thiếu là:

1. Multimodal upload thật.
2. Image/video backend thật.
3. Browser-based account/token refresh và Guest Mode.

## 5. Các lỗi correctness phát hiện trong Sever Provider

### P0 — Lease không gắn chặt credential

Trong `internal/scheduler/dispatch.go`, dispatcher đăng ký capacity cho candidate credential rồi gọi `d.acquire(ctx, cred.Domain, ...)`. `internal/scheduler/lease.go` chọn capacity theo domain/capacity, không nhận `credentialID`. Vì vậy:

- candidate A có thể nhận lease của B;
- secret đọc từ A nhưng release lease của B;
- session affinity/pool role bị phá;
- active lease và breaker/status có thể ghi nhận sai account.

**Cách sửa bắt buộc:** thêm `AcquireForCredential(ctx, credentialID, model, ttl)` hoặc truyền `CredentialID` vào acquire; lease phải reserve đúng capacity ID.

### P0 — Startup provider không usable mặc định

- `cmd/server/main.go:53-58`: register providers nhưng chỉ seed local Piper/TikTok credential.
- TikTok dùng `tiktok.New(nil, nil)`, pool rỗng.
- Copilot dùng `copilot.New(nil)`, không có token/invocation config.
- Google credentials không được provision tự động.

Cần `/health`/`/internal/status` phân biệt `registered` với `ready`, và không claim provider đã hoạt động nếu chưa có credential/config.

### P0 — Internal telemetry bypass auth

`internal/gateway/router.go` miễn API key cho `/internal/status` và `/internal/events`. Các endpoint này trả account/credential/worker metadata trong `handlers.go:255-292`. Nếu host bind non-loopback, đây là information disclosure.

Khuyến nghị:

- bind internal routes loopback-only; hoặc
- yêu cầu admin session/API key riêng; hoặc
- redact identity email, credential ID/fingerprint và worker details.

### P1 — Audit không được emit production

`AuditLogger.Record` tồn tại và endpoints đọc/xóa đã có, nhưng chưa có call path trung tâm từ gateway/dispatcher. Cần emit các event tối thiểu:

- request start/end;
- provider attempt success/failure;
- credential status change;
- API key create/revoke/delete;
- worker connect/disconnect;
- admin mutation.

### P1 — Model failover bị bỏ rơi

`ModelSpec` có `FailoverTo` nhưng config model không map field và dispatcher không thử chain model. Cần thêm field trong config, validate cycle, và dispatch fallback có giới hạn.

### P1 — Job/media consistency

- Runner chỉ xử lý `tts` và `llm/chat`; image/video job kind chưa có worker.
- `VideoTask` lấy job theo ID nhưng không verify `kind == video`.
- `media_files` table chưa được ghi/đọc; filestore cleanup dùng mtime thay vì `expires_at` DB.
- Runner/supervisor dùng `context.Background()` và chưa có graceful cancellation từ server shutdown.

### P1 — Advertised capabilities không khớp runtime

Gemini/Copilot capability có vision/image nhưng adapter trả unsupported; request multimodal chỉ parse/validate, chưa upload. Models nên expose `available=false` hoặc omit capability cho tới khi backend sẵn sàng.

### P2 — Dashboard còn fake actions

`web/app.js:27-29`:

- Discover chỉ gọi dry-run empty response.
- Disable chỉ `alert`, không mutation.
- Refresh gọi toàn dashboard thay vì validate credential cụ thể.
- Dashboard gọi `/internal/status` không auth.

### P0 — TikTok voice và model registry bị lệch (gây lỗi 404 ngay lập tức)

- Trong `internal/gateway/superset_handlers.go:93–96`, API `TTSVoices` quảng bá:
  - `BV074_streaming`
  - `BV075_streaming`
- Nhưng trong `config/models.yaml:22–25`, model được đăng ký duy nhất cho TikTok là:
  - `tiktok:vi_male`
- Khi client hoặc Web Dashboard gọi `TTSGenerate` với `voice_id: BV074_streaming`, handler tự sinh model name là `tiktok:BV074_streaming` (`superset_handlers.go:120`), model này không tồn tại trong registry và trả lỗi HTTP 404 `model_not_configured`.

### Các lỗi P0/P1 lịch sử đã được sửa

Bản audit ban đầu phát hiện các lỗi sau trong phiên bản trước. Chúng được giữ lại ở đây để truy vết, nhưng không còn là trạng thái hiện tại:

- **Sticky session:** `orderedCredentials` hiện ưu tiên credential đã bind rồi mới gọi pool allocator; `StreamChat` bind session sau khi stream thành công. Dead helper `sessionOrder` đã được xóa.
- **Worker WebSocket:** `/api/llm/ws/worker` được miễn consumer API-key middleware và dùng worker token riêng khi được cấu hình.
- **Speech/Image:** cả hai pipeline hiện kiểm tra provider/account breaker và gọi `recordAttempt` để cập nhật status/cooldown.
- **Lease ownership:** dispatcher dùng `AcquireCredential`, không còn chọn ngẫu nhiên credential theo domain.

### P1 — Copilot template nil ở startup

- Phiên bản cũ khởi tạo `copilot.New(nil)` khiến request lỗi `copilot invocation template is required`.
- Hiện `copilot.New(nil)` tự nạp `DefaultTemplate`; credential/token runtime vẫn phải được provision bên ngoài.
### P1 — Copilot template nil ở startup

- `cmd/server/main.go:57` khởi tạo: `copilot.New(nil)`.
- Khi client gọi model copilot, `internal/provider/copilot/copilot.go:153` trả về lỗi ngay lập tức: `copilot invocation template is required`.

### P1 — Dead handler code: LegacyTTS

- `internal/gateway/handlers.go:323–344` định nghĩa `LegacyTTS` nhưng trong `internal/gateway/router.go` route `/api/tts/synthesize` không được đăng ký. Đây là dead code còn sót lại sau tái cấu trúc.

## 6. Bảng trạng thái parity cuối cùng

| Nhóm | Parity thật | Nhận xét |
|---|---:|---|
| Unified HTTP routing | Cao | Routes chính đã có |
| OpenAI chat/SSE | Cao/partial | Có pipeline, cần sửa lease ownership |
| Anthropic messages | Partial | Contract có, provider backend phụ thuộc credential |
| Piper TTS | Partial | Adapter có, thiếu smart pool/discovery |
| TikTok TTS | Partial/contract | Adapter có nhưng pool mặc định rỗng, provisioning/scaling thiếu |
| Colab worker protocol | Cao | Inbound WS/stream/cancel tốt |
| Colab lifecycle | Thấp | Thiếu start/rotate/prewarm/cooldown/WARP |
| Gemini RPC chat | Partial | Streaming có; thiếu upstream conversation persistence/error nuances |
| Copilot chat | Partial | Protocol có; startup config/token flow thiếu |
| Vision | Partial contract | Schema/limit có; upload backend thiếu |
| Image | Chưa đạt | Các adapter hiện unsupported |
| Video | Chưa đạt | Route contract/501 |
| 40/60 pool | Partial | Role/borrow có; quota 40/60 thật chưa có |
| Credential lifecycle | Partial | Identity/import/status có; discover/rotate/real validation thiếu |
| API key | Cao | Hash/CRUD/middleware đã test |
| Admin audit | Partial | Storage/API có; production emit thiếu |
| Maintenance | Partial/Cao | Supervisor có; graceful shutdown/media DB integration thiếu |
| Security baseline | Partial | Secrets tốt; internal telemetry/CORS/worker optional auth cần harden |
| Automated tests | Khá | Local tests pass; thiếu real-provider/load/race/dashboard/jobs coverage |

## 7. Ưu tiên triển khai đề xuất

### P0 — Không nên production trước khi xử lý

1. Sửa `Acquire` thành credential-bound lease.
2. Bắt buộc worker auth trong production config; fail startup hoặc mark unavailable nếu thiếu token.
3. Bảo vệ `/internal/status` và `/internal/events`.
4. Sửa Colab rotate fake `202` thành `501` hoặc job thật.
5. Phân biệt provider `registered` và `ready`; không seed/claim credential giả.

### P1 — Hoàn thiện superset runtime

1. Wire audit events vào request/dispatch/admin/worker lifecycle.
2. Thêm model failover chain và tests.
3. Implement hoặc remove advertised image/vision capabilities.
4. Implement media job runner + verify video job kind.
5. Persistent account cooldown/breaker/lease state hoặc documented restart semantics.
6. Graceful shutdown cho runner, supervisor, worker hub.

### P2 — Parity nâng cao

1. Piper voice discovery + smart model load/unload.
2. TikTok account provisioning + scaling thật.
3. Colab rotator 4h/16h, prewarm và WARP sidecar contract.
4. Google/Copilot browser token refresh.
5. Dashboard mutation/auth UX.
6. Integration, race, load và dashboard browser tests.

## Tuyên bố chính xác

Sever Provider hiện là **unified foundation có nhiều parity primitives**, chưa phải bản superset runtime hoàn chỉnh. Không nên đánh dấu image/video, Colab lifecycle, TikTok provisioning, multimodal upload hoặc audit telemetry là “implemented” cho tới khi có backend thực thi và test tương ứng.
