# Audit Gemini — 2026-10-06

> Phạm vi: chỉ đọc source + lệnh kiểm tra liệt kê. Không sửa file, không in secret/token/cookie/password/API key/nội dung chat thật. Host kiểm tra: Windows (`uname -a` → MINGW64_NT-10.0-26200), prod path qua WSL không truy cập được.

## 1. Kết luận hiện trạng

**Đã chứng minh (verified bằng đọc source/chạy lệnh trong đợt audit):**

- 3 alias Gemini dùng chung adapter `gemini_web`, 1 endpoint `StreamGenerate`: `config/models.yaml:4-41`, `internal/provider/gemini/rpc.go:18-21,106,270`, `cmd/server/main.go:70-71`.
- Failover chỉ chat non-stream, stream fail closed: `config/models.yaml:1-2`, `internal/scheduler/dispatch.go:55-80,253-292`.
- Session 2 lớp memory + SQLite, commit trước Done, giữ mapping qua restart, chặn đổi account khi owner lỗi: `internal/session/manager.go:59-74`, `internal/session/persistent.go:50-133`, `internal/scheduler/dispatch.go:239-247,420-470`.
- Live test cô lập PASS: `go test ./internal/gateway -run TestAuditLiveSessionSuite -count=1 -v` (0.20s), `go test -count=1 -v ./internal/provider/gemini/...` PASS 16 test, `go test ./internal/gateway/ -run Google` PASS 3 test, `go test -count=1 ./internal/scheduler/... ./internal/workerhub/... ./internal/pool/... ./internal/session/...` PASS 4 package.
- Secret Google trong `data/orchestrator.db` không giải mã bằng `MASTER_KEY` tại `config/config.yaml:5`: xem §3, §5 P0-1.
- Tồn tại lỗi lease/sweep/stream/audit ở mức code: xem §5 P0.

**Lỗi chắc chắn (cần sửa, đã xác nhận source):**

- P0: secret Google mất khả năng giải mã; lease kẹt credential đầu; sweep thu slot khi stream chạy; response rỗng vẫn `stop`; EOF/cancel vẫn chốt `Done`; `taskkill` không flush + `HEALTHY` chưa validate; mất audit khi `r.Context()` hủy.
- P1: allocator bỏ `Priority`; thiếu FIFO/backpressure; chọn-giữ slot không atomic; regex fallback lấy nhầm ID; `X-Session-ID` thiếu namespace; stream bỏ `http.Client.Timeout`.
- P2: health/ready nông; stream chat thiếu audit; VERSION `dev`; cây dirty; thiếu `AGENTS.md`; thiếu metric span.

**Thiếu bằng chứng (unconfirmed, không dùng từ ổn định/đã sửa):**

- Toàn bộ claim số liệu prod 2h/24h, DeepSeek muted, TTS race, copilot-refresh, PID 1753448, fd DB, sha256 binary, mùi giờ, VERSION prod 0.1.1, `vision` prod cho `gemini-3.5-flash-lite`: `wsl ls /media/...` → `No such file or directory`, `ps/systemctl` → `MainPID=0`, không truy vấn được `observability_events/credentials/chat_session_bindings`, không đọc `journalctl`, không gọi upstream thật.
- Copilot `InvalidRequest` trả 200 rỗng trên prod: local đã có `terminalResult/isSuccessResult` tại `internal/provider/copilot/copilot.go:515-522`, không chạy được `strings/sha256` trên binary prod.
- Nội dung pixel `test_gemini_cat.png` (244K, Oct 3 12:09): chỉ `ls -lh`, chưa mở ảnh.
- Hành vi khác nhau chuỗi model phía Google, reasoning/vision thật: chưa gọi upstream thật, chỉ `httptest`.

## 2. Bảng model và capability

| Alias | Provider | Đã cấu hình (config) | Gọi được (code) | Đã chứng minh |
|---|---|---|---|---|
| `gemini-3.8-flash` | `gemini_web` | `config/models.yaml:4-8` caps `[chat streaming vision image_generation]`, `failover_to [gemini-3.1-pro]`, timeout 90; `DefaultModel` tại `internal/provider/gemini/rpc.go:219`; fallback rỗng tại `internal/provider/gemini/payload.go:71-76` | `StreamChat` + `generateRaw` cùng URL `StreamGenerate` tại `internal/provider/gemini/rpc.go:106,270`, header chat `71c2d248d3b102ff` tại `:121-124,284-287`; image header Mode 10 `56fdd199312815e2` tại `internal/provider/gemini/image.go:107-110` | `go test -count=1 -v ./internal/provider/gemini/...` PASS (gồm `TestGenerateUsesInjectedRPCServer`, `TestStreamChatUsesGuestEndpointAndReturnsMetadata`, `TestImageStreamGenerateAndCDNDownload`, `TestChatWithImageUploadFlow`); tên model chỉ đổi `inner[3]` tại `payload.go:36` |
| `gemini-3.1-pro` | `gemini_web` | `config/models.yaml:33-36` caps `[chat streaming reasoning vision]`, timeout 180 | Cùng adapter/endpoint như trên; `ChatResponse` không gán `ReasoningContent` tại `rpc.go:61-72`, chunk `Done` cũng không gán tại `:188-197` | Capability `reasoning` cấu hình rộng hơn code chứng minh; chưa gọi upstream thật |
| `gemini-3.5-flash-lite` | `gemini_web` | `config/models.yaml:37-41` caps `[chat streaming]`, timeout 60, `guest:true` | Cùng adapter/endpoint; vision chỉ qua `ExtractAndUploadFiles` khi `HasImages()` tại `rpc.go:41-46,97-103` (`multimodal.go:260-278`); image gate chỉ `Model==gemini-3.8-flash && Prompt nonempty && Size==""` tại `image.go:28-31` | Chưa xác minh claim prod giữ `vision` cho lite; local caps `[chat streaming]` |
| Đăng ký adapter | — | `cmd/server/main.go:70-71` một `gemini.New(nil)` đăng ký một lần | `Name()=gemini_web` tại `rpc.go:18`, `Capabilities` tại `:19-21` | verified |
| Điều phối lỗi model | — | dispatcher `not configured` khi `Models.Get("")` thất bại tại `dispatch.go:82-86`; thiếu capability → `ErrCapabilityUnsupported` tại `:82-89,293-300,481-488`; `maskProviderError` map 404/400 tại `handlers.go:751-773`; ảnh thiếu `vision` chặn tại `handlers.go:257-260` | verified | — |
| Failover | — | `config/models.yaml:1-2` failover chỉ non-stream | `chatWithFailover` visited chống chu kỳ, chỉ `failoverEligible` tại `dispatch.go:55-80,770-779`; `streamChat` không gọi failover tại `:293-384`; deadline chung `chatContext` tại `:219-235`; cùng provider flash→pro; `sessionUpstream/bindSession` ghi đè tại `:106-108,160-163,198-218` | verified |

## 3. Bảng account/profile/auth (ẩn nhạy cảm)

| Đối tượng | Nguồn kiểm tra | Hiện trạng | Ghi chú an toàn |
|---|---|---|---|
| `MASTER_KEY` / secret file | `internal/secret/store.go:32-37` (rỗng → ephemeral, log `data cannot be decrypted across restarts`); `config/config.yaml:5` 64 hex; `head -n 8 config/config.yaml` chỉ `server 0.0.0.0:18080`, `db_path data/orchestrator.db`, placeholder | File mẫu chứa placeholder, cần biến môi trường cho secret thật; không in secret | verified, severity medium |
| Identity/credential DB | Truy vấn SQLite `data/orchestrator.db` bằng `python sqlite3` + giải mã AES-GCM bằng key config (không in secret): 1 identity google + 1 credential `gemini_web` `source_ref browser_profile:Account-71073`; decrypt Google `FAILED`, 2 deepseek `SUCCESS` | Secret Google coi như mất với key hiện tại | P0-1, verified high |
| Profile disk | `ls data/profiles`: 6 thư mục `Account-32905/67881/68809/70730/70773/71073`; inspect `Preferences`/`Cookies` SQLite count: `32905 thanhchuapi2 (48)`, `67881 thanhchuapi1 (51)`, `68809 thanhchuapi1 (74)`, `70730 thanhchuapi2 (8)`, `70773 None (2)`, `71073 thanhchuapi3 (50)`; `is_symlink=False` | 5 orphan, 2 cặp trùng email, 1 rác chưa login | P0-2, verified high |
| Sinh accountID | `internal/browser/browser.go:135-136` `Account-%d = time.Now().Unix()%100000`; `cmd/server/main.go:177-188` callback `UpsertGoogleAccount` ID ngẫu nhiên, không tìm profile cũ theo email | Login lại cùng email tạo profile mới, bỏ rơi profile cũ | verified medium |
| Kill browser + validate | `internal/browser/browser.go:288` `monitorLogin` gọi `terminateBrowserProcess` ngay sau `onCaptured`; `:462-471` Windows `taskkill /PID /T /F`, không `Browser.close` như `refresh.go:272`; `cmd/server/main.go:177-188` `UpsertGoogleAccount` trực tiếp không `ValidateCookie`; `internal/identity/manager.go:97` luôn `StatusHealthy`; `ValidateCookie` chỉ tại `internal/provider/gemini/rpc.go:370`, `cmd/server/refresh_accounts.go:83,91` | Không flush disk + `HEALTHY` chưa validate upstream | P0-3, verified high |
| SingletonLock Windows | `internal/browser/refresh.go:192-199` `existingProfileCDP: if err != nil \|\| GOOS != linux return ErrAutoLoginBusy`; `:35` + `browser.go:125` `CompareAndSwap`; `Singleton*` 6 account `locks=[]`; `Get-Process` ~20 `msedge`+`webview2`; host `win32` | Windows trả `ErrAutoLoginBusy` vĩnh viễn, lock toàn cục chặn refresh/login song song | verified medium |
| Legacy path | Kiểm tra `/media/reup/Data_sv4/geminigo` không tồn tại; `/media/reup/Data_sv4` chỉ chứa `['SVRV']` | Không suy luận thêm | verified |
| Cookie/token đọc | Chỉ đọc `SanitizeCookie`/`Tokens.Get`, không in secret | Tuân thủ che secret | verified |

## 4. Sơ đồ ngắn luồng phiên và điều phối thực tế

```text
Client --X-Session-ID--> gateway/handlers.go:262-270 (TrimSpace, len>256 chặn)
  |-> (Anthropic superset_handlers.go:53 KHÔNG validate 256) -> provider.ChatRequest{SessionID}
  |-> (Image handlers.go:499-503 lấy header/query)
  |-> scheduler/dispatch.go:228 Sessions.Acquire(ctx, SessionID) [gate serialize persistent.go:18-48]
  |-> chatCredentials/sessionUpstream :106-108,198-248 (Get mapping, check Provider, owner lỗi -> chặn, không đổi account ngầm)
  |-> chatContext timeout TimeoutSec :219-235
  |-> acquireCredential :809-818 (ttl=TimeoutSec) -> lease.go:93-114 poll 25ms [P0 kẹt]
  |-> orderedCredentials sort snapshot :704-731 -> duyệt tuần tự acquire :109-146 [không atomic]
  |-> adapter gemini_web rpc.go:106 StreamGenerate (payload inner[3]=model, payload.go:36)
  |-> stream wrapRelease :412-473 commit (finish/Commit) TRƯỚC emit Done :455-462
  |-> persistent.go:80-133 Commit persist trước khi báo thành công; lỗi DB -> hủy, không cập nhật cache :127-131
  |-> chat non-stream release trực tiếp :154-155 (không defer); stream đã defer :416-417
  |-> Health/Ready handlers.go:124-147 chỉ uptime/requests + đếm models, không chứng minh upstream sống
  |-> Audit: speech/image Record với r.Context() :456,462,508,521 + ExecContext audit.go:37 [hủy theo]; streamChat :355-428 KHÔNG Record
```

## 5. Findings P0/P1/P2

### P0-1. Secret Google không giải mã bằng MASTER_KEY hiện tại

- **Hiện tượng:** Decrypt `credential_secrets` Google `FAILED`, 2 deepseek `SUCCESS` cùng key.
- **Điều kiện tái hiện:** Truy vấn SQLite `data/orchestrator.db` (`identities`/`credentials`/`credential_secrets`) bằng `python sqlite3`, giải mã AES-GCM bằng `MASTER_KEY` tại `config/config.yaml:5` (không in secret).
- **Source:** `internal/secret/store.go:32-37`; `config/config.yaml:1-8`; `data/orchestrator.db` (1 identity google + 1 credential `gemini_web` `source_ref browser_profile:Account-71073`).
- **Bằng chứng prod/test:** Lệnh decrypt trên cho `google-...-gemini_web: FAILED`, deepseek `SUCCESS`. Chưa gọi `/v1/chat/completions` prod vì bảo toàn DB.
- **Nguyên nhân xác nhận / giả thuyết:** Xác nhận: key hiện tại không mở được bản mã Google (key xoay/mất, hoặc bản mã từ ephemeral key sau restart theo `store.go:37`). Giả thuyết chưa loại: DB lẫn bản mã từ môi trường khác.
- **Ảnh hưởng app:** Mọi đường Chat/Stream Gemini dùng cookie sau sanitize vẫn đi tiếp tới `Tokens.Get` rồi mới lỗi tại `rpc.go:32-34,81-83` thay vì fail-fast; prod coi như mất auth Google.
- **Hướng sửa:** Dừng login mới ghi đè; backup read-only DB; đối chiếu `MASTER_KEY` theo môi trường triển khai (không commit secret); login lại 1 account chuẩn qua `ValidateCookie` (`rpc.go:370`, `refresh_accounts.go:83,91`) rồi mới đánh `HEALTHY`; dọn orphan theo §3 sau khi có auth sống.
- **Kiểm chứng sau sửa + rollback:** Chạy decrypt lại phải `SUCCESS`; `go test -run Google ./internal/gateway -count=1 -v` PASS; `ValidateCookie` PASS; rollback = khôi phục file DB backup, không xóa profile disk vội.

### P0-2. Phiên mới kẹt ở credential đầy slot đầu, bỏ qua credential rảnh

- **Hiện tượng:** Request mới chờ hết timeout dù còn credential rảnh cùng model.
- **Điều kiện:** ≥2 credential cùng model, credential đầu hết slot (`tryAcquireCredentialLocked` `canRetry=true`).
- **Source:** `internal/scheduler/dispatch.go:142` (`chatOnce`) + `:353` (`streamChat`) `acquireCredential` rồi `continue`; `internal/scheduler/lease.go:93-114` poll `25ms` tới `ctx.Done()`, không trả `ErrNoCapacity` khi `canRetry=true` (`lease.go:125-126`).
- **Bằng chứng:** Đọc source đối chiếu; `go test -count=1 ./internal/scheduler/...` PASS nhưng không phủ tranh chấp nghìn goroutine.
- **Nguyên nhân / giả thuyết:** Xác nhận: vòng ngoài không bao giờ tới candidate tiếp theo vì `AcquireCredential` block. Không phải do `allocator.go` sort sai.
- **Ảnh hưởng:** Giảm capacity thực tế, latency tăng tới timeout, failover non-stream cũng bị chặn (`dispatch.go:55-80`).
- **Hướng sửa:** Trả `ErrNoCapacity` ngay khi hết slot để vòng ngoài thử candidate tiếp; hoặc `tryAcquire` non-blocking theo danh sách `orderedCredentials`; giữ poll có giới hạn + jitter chỉ khi mọi candidate đầy.
- **Kiểm chứng + rollback:** Thêm test 2 credential (1 đầy slot, 1 rảnh) assert dùng credential rảnh < `TimeoutSec`; `go test -count=1 ./internal/scheduler/... ./internal/pool/...` PASS; rollback = revert `lease.go:93-114`.

### P0-3. Lease hết hạn theo TimeoutSec bị sweep khi stream còn chạy

- **Hiện tượng:** Stream dài hơn `TimeoutSec` mất slot giữa chừng; `Release` sau đó báo `lease not found`.
- **Điều kiện:** Stream dài hơn TTL (`dispatch.go:809-818` `ttl=TimeoutSec`, `chatContext :219-224`).
- **Source:** `internal/scheduler/lease.go:129` (`ExpiresAt=now+ttl`) + `:165-181` (`ReclaimExpired`) + `:151-157` (`Release`); `cmd/server/main.go:113-131` ticker 15s; `dispatch.go:412-418` `wrapRelease` defer `Release`; không có `Renew/Extend/Heartbeat` trong `lease.go`.
- **Bằng chứng:** Đọc source đối chiếu; chưa giả lập stream dài hơn `TimeoutSec` trên live.
- **Nguyên nhân / giả thuyết:** Xác nhận: TTL = timeout request, không phải TTL lease stream. Giả thuyết chưa đo: packet khi sweep trên live.
- **Ảnh hưởng:** Slot mở sớm khi stream còn chạy → oversubscribe; log `lease not found` gây nhiễu.
- **Hướng sửa:** Tách TTL lease stream khỏi `TimeoutSec` (TTL dài + heartbeat/`Renew` khi còn chunk, hoặc sweep bỏ qua lease đang stream); `Release` idempotent khi đã sweep.
- **Kiểm chứng + rollback:** Test stream giả > TTL assert không sweep + không `lease not found`; `go test -count=1 ./internal/scheduler/...` PASS; rollback = revert `lease.go` + ticker.

### P0-4. Response rỗng / HTTP 200 mang lỗi nghiệp vụ vẫn trả success rỗng

- **Hiện tượng:** `Chat` trả `Content=""`, `FinishReason="stop"` dù body lỗi hoặc không có frame hợp lệ.
- **Điều kiện:** Upstream trả `200` nhưng parser không trích được text; hoặc `StatusCode>=400` không bắt vì chỉ check `>=400` mà bỏ lỗi nghiệp vụ trong 200.
- **Source:** `internal/provider/gemini/rpc.go:247-253,257-308` (`generateRaw` chỉ check `>=400`); `parser.go:73-128` (`ExtractResponseText` trả `""` khi không có frame); `rpc.go:61-72` gán `Content=""` + `stop` không kiểm tra rỗng.
- **Bằng chứng:** Đọc source; `go test -count=1 -v ./internal/provider/gemini/...` PASS nhưng chưa có case 200-rỗng assert lỗi.
- **Nguyên nhân / giả thuyết:** Xác nhận: thiếu kiểm tra `Content==""` + lỗi nghiệp vụ trong 200. Không phải do payload `inner[3]`.
- **Ảnh hưởng:** Client nhận success giả, session vẫn commit upstream state rỗng (`dispatch.go:420-470`), khó phát hiện.
- **Hướng sửa:** Sau parse, nếu `Content==""` và không có `Err` → trả lỗi (vd `empty upstream response`) để failover non-stream kích hoạt (`dispatch.go:770-779`); log raw có giới hạn độ dài, không log secret.
- **Kiểm chứng + rollback:** Thêm test `httptest` 200-rỗng assert lỗi thay vì `stop`; chạy `go test -count=1 -v ./internal/provider/gemini/...`; rollback = revert check rỗng.

### P0-5. Hủy client / EOF cắt giữa chừng vẫn chốt stream `Done` success

- **Hiện tượng:** Stream bị cắt vẫn phát chunk cuối `Done:true` kèm `prevText`.
- **Điều kiện:** `reader.ReadString` gặp `io.EOF` hoặc lỗi chứa `context canceled` giữa stream.
- **Source:** `internal/provider/gemini/rpc.go:146-179` (`break` khi `EOF`/`context canceled`); `:188-197` vẫn emit `Done:true` + `stop` thay vì `Err`.
- **Bằng chứng:** Đọc source đối chiếu.
- **Nguyên nhân / giả thuyết:** Xác nhận: `break` nuốt tín hiệu hủy. Giả thuyết chưa đo socket teardown trên HTTP streaming thật.
- **Ảnh hưởng:** Client tưởng turn hoàn tất; `wrapRelease` commit state cắt cụt trước `Done` (`dispatch.go:455-462`).
- **Hướng sửa:** Khi `readErr==EOF` mà chưa có frame hợp lệ đầy đủ, hoặc `ctx.Err()!=nil` → emit `Err` thay vì `Done`; chỉ `Done` khi parser xác nhận frame kết thúc hợp lệ.
- **Kiểm chứng + rollback:** Thêm test `httptest` cắt giữa chừng + test hủy `ctx` assert chunk `Err`; `go test -count=1 -v ./internal/provider/gemini/...`; rollback = revert `:172-197`.

### P0-6. Kill browser `taskkill` không flush + credential `HEALTHY` chưa validate

- **Hiện tượng:** `Cookies`/`Preferences` nguy cơ ghi dở; credential mới luôn `HEALTHY` dù cookie chết.
- **Điều kiện:** Login mới trên Windows.
- **Source:** `internal/browser/browser.go:288` (`monitorLogin` `terminateBrowserProcess` sau `onCaptured`); `:462-471` Windows `taskkill /PID /T /F`, không `Browser.close` như `refresh.go:272`; `cmd/server/main.go:177-188` `UpsertGoogleAccount` trực tiếp không `ValidateCookie`; `internal/identity/manager.go:97` luôn `StatusHealthy`; `ValidateCookie` chỉ tại `rpc.go:370`, `refresh_accounts.go:83,91`.
- **Bằng chứng:** Đọc source + `Get-Process` thấy `msedge` chạy; chưa map `--user-data-dir` từng PID.
- **Nguyên nhân / giả thuyết:** Xác nhận code path. Chưa đo file lỗi nhờ thiếu dump SQLite header.
- **Ảnh hưởng:** Auth chết vẫn được điều phối, lỗi chỉ nổ muộn tại `Tokens.Get`; disk rác tăng (6 profile §3).
- **Hướng sửa:** Windows đóng graceful trước khi `taskkill`; callback login bắt buộc `ValidateCookie` rồi mới `Upsert` + `HEALTHY`; sinh ID ổn định theo email thay vì `Unix()%100000` (`browser.go:135-136`).
- **Kiểm chứng + rollback:** Login thử 1 account: `Preferences` email + `Cookies` count + `ValidateCookie` PASS + DB `HEALTHY`; rollback = revert callback + giữ profile cũ.

### P0-7. Mất audit khi timeout/cancel (context hủy làm `ExecContext` hủy theo)

- **Hiện tượng:** Request timeout/cancel có thể không để lại `observability_events`.
- **Điều kiện:** `speech`/`image` hủy `r.Context()` đúng lúc ghi audit.
- **Source:** `internal/gateway/handlers.go:456,462,508,521` truyền `r.Context()` vào `AuditLog.Record`; `internal/admin/audit.go:29-44` (`ExecContext(ctx)` tại `:37`).
- **Bằng chứng:** Đọc source (verified medium).
- **Nguyên nhân / giả thuyết:** Xác nhận cơ chế. Chưa đếm bản ghi mất trên prod DB.
- **Ảnh hưởng:** Thiếu dấu vết đúng lúc cần debug nhất (timeout/Copilot 200-rỗng/TTS race).
- **Hướng sửa:** Ghi audit bằng `context.WithoutCancel`/`WithTimeout` nền tách khỏi `r.Context()`; giữ `Latency` tại `handlers.go:325,340`.
- **Kiểm chứng + rollback:** Test hủy `r.Context()` assert audit vẫn ghi; `go test -count=1 ./internal/gateway/...`; rollback = revert context audit.

### P1-1. Allocator bỏ qua `Priority`, không kiểm soát quota

- **Hiện tượng:** `Identity.Priority` không ảnh hưởng chọn account.
- **Source:** `internal/identity/types.go:39` có `Priority`; `internal/pool/allocator.go:35-83` chỉ dùng `ActiveLeases` (`:56`, sort `:71-72`), chia `role/borrow`, không đọc `Priority`; `grep Priority` không match trong `pool`/`scheduler`; `dispatch.go:116` không logic quota.
- **Bằng chứng:** Đọc + grep trong đợt audit. **Ảnh hưởng:** Không ưu tiên account khỏe/quota lớn. **Hướng sửa:** Sort thêm `Priority` sau `ActiveLeases`, hoặc quota riêng; đánh dấu `ponytail:` nếu giữ đơn giản. **Kiểm chứng:** Test allocate 2 credential lệch `Priority` + `go test -count=1 ./internal/pool/...`.

### P1-2. LeaseManager thiếu FIFO, queue limit, backpressure/metric

- **Hiện tượng:** Poll `25ms` giữ `Mutex` từng lượt, request đến trước không bảo đảm phục vụ trước; hết slot không tín hiệu sớm.
- **Source:** `internal/scheduler/lease.go:93-114,98,125-126`; chỉ trả `ErrNoCapacity` khi `canRetry=false`. **Ảnh hưởng:** Fairness kém, khó giám sát. **Hướng sửa:** Hàng đợi FIFO + giới hạn queue + metric/backpressure; tối thiểu trả `ErrNoCapacity` nhanh (liên quan P0-2). **Kiểm chứng:** Test N waiter FIFO + `go test -count=1 ./internal/scheduler/...`.

### P1-3. Chọn account và giữ slot không atomic, sort dùng snapshot cũ

- **Hiện tượng:** Thứ tự `orderedCredentials` tính lúc fetch (`dispatch.go:704-731`) lệch lúc `acquire` (`:109-146` duyệt tuần tự).
- **Source:** `internal/scheduler/dispatch.go:109,116-146,704-731`. **Ảnh hưởng:** Phân tải lệch khi tranh chấp. **Hướng sửa:** `tryAcquire` non-blocking theo vòng + refresh `ActiveLeases`, hoặc lock tổng bao chọn-giữ. **Kiểm chứng:** Test tranh chấp assert không dồn 1 account.

### P1-4. Fallback regex có thể lấy nhầm ID hội thoại từ nội dung model

- **Hiện tượng:** Text model chứa token khớp regex bị lấy làm `ConversationID/ResponseID/ChoiceID`.
- **Source:** `internal/provider/gemini/parser.go:169-184` quét `conversationRE/responseRE/choiceRE` trên raw khi `wrb.fr` không có metadata; `payload.go:21-35` gửi `ConversationMetadata`, `parser.go:17-185` parse `c_/r_/rc_`. **Ảnh hưởng:** Tiếp nối nhầm upstream, ghi đè mapping (`dispatch.go:106-108,198-218`). **Hướng sửa:** Chỉ nhận metadata từ JSON `wrb.fr` có cấu trúc; bỏ/quarantine regex fallback hoặc giới hạn vùng frame metadata + validate định dạng. **Kiểm chứng:** Test text chứa `c_/r_/rc_` giả assert không nhận ID.

### P1-5. `X-Session-ID` thiếu namespace theo client/API key

- **Hiện tượng:** Hai client khác nhau dùng cùng session ID có thể trùng/chèn phiên.
- **Source:** `internal/scheduler/dispatch.go:228` (`Acquire(ctx, req.SessionID)`); `:236-248` chỉ so `Provider`, không prefix API key; `persistent.go:80-92` `Commit` dùng trực tiếp ID; đối chiếu `handlers.go:262-264` có validate 256 còn `superset_handlers.go:53` không. **Ảnh hưởng:** Lộ/chèn hội thoại giữa client. **Hướng sửa:** Prefix namespace `hash(API key)+SessionID` tại gateway trước khi vào scheduler; bổ sung validate 256 cho `superset_handlers.go:53`. **Kiểm chứng:** Test 2 API key cùng ID assert mapping tách biệt.

### P1-6. Stream bỏ `http.Client.Timeout`, cookie lỗi không fail-fast

- **Hiện tượng:** Stream phụ thuộc hoàn toàn `ctx` dispatcher; cookie hỏng sau sanitize vẫn đi tiếp tới `Tokens.Get`.
- **Source:** `internal/provider/gemini/rpc.go:126` (`streamClient` bỏ `Timeout` của `a.Client` 180s; deadline từ `dispatch.go:219-235` mặc định 120s/`TimeoutSec`); `rpc.go:32-34,81-83` giữ cookie thối khi `SanitizeCookie` lỗi, trong khi `ValidateCookie :371-373` và `google_handlers.go:204-208` chặn ngay. **Ảnh hưởng:** Treo stream khó chẩn đoán; lỗi muộn. **Hướng sửa:** Giữ deadline stream tường minh + fail-fast khi sanitize lỗi. **Kiểm chứng:** Test cookie hỏng assert lỗi ngay, test stream timeout tường minh.

### P1-7. SingletonLock Windows chặn refresh/login song song (vận hành)

- **Hiện tượng:** Windows luôn `ErrAutoLoginBusy`, không kiểm PID/dọn lock như Linux.
- **Source:** `internal/browser/refresh.go:192-224,272`; `browser.go:125`; host `win32`; `Singleton*` 6 account `locks=[]`. **Hướng sửa:** Triển khai kiểm PID/dọn lock Windows hoặc tách lock refresh/login; tối thiểu log rõ `GOOS` khi busy. **Kiểm chứng:** Gọi refresh 2 lần song song trên Windows assert không busy vĩnh viễn.

### P2-1. Health/Ready nông, không chứng minh upstream sống

- **Hiện tượng:** `/health` chỉ `uptime/requests` (`handlers.go:124-130`); `/ready` chỉ check `Models/Dispatcher` + đếm models (`:132-147`, dùng `buildinfo.Version` tại `:146`). **Ảnh hưởng:** 200 không nói Gemini/Copilot/DeepSeek sống. **Hướng sửa:** Thêm `/ready/deep` hoặc endpoint probe `ValidateCookie`/quota (read-only, rate-limit), giữ `/ready` nhẹ hiện tại. **Kiểm chứng:** Gọi ready khi cookie chết assert deep probe fail trong khi ready nhẹ vẫn 200.

### P2-2. Streaming chat không ghi audit

- **Hiện tượng:** `streamChat` (`handlers.go:355-428`) không có `AuditLog.Record` ở nhánh nào. **Ảnh hưởng:** Mất `Latency`/lỗi/TTFT streaming (`handlers.go:325` chỉ tổng HTTP, thiếu span lock/lease/TTFT). **Hướng sửa:** Ghi audit sau `Done/Err` bằng context nền (kết hợp P0-7) + metric span lock/lease/TTFT. **Kiểm chứng:** Test stream assert 1 bản ghi audit.

### P2-3. Nhánh chat non-stream release lease không `defer`

- **Hiện tượng:** `adapter.Chat` rồi `release` trực tiếp (`dispatch.go:154-155`); panic giữa hai dòng giữ slot tới TTL (sweep `lease.go:165`); stream đã `defer` (`:416-417`). **Hướng sửa:** Bọc `defer release` ngay sau acquire thành công. **Kiểm chứng:** Test panic assert slot giải phóng.

### P2-4. Vệ sinh repo/build: VERSION `dev`, cây dirty, thiếu `AGENTS.md`, file ảnh test

- **Hiện tượng:** `VERSION:1` = `0.1.3` nhưng `internal/buildinfo/version.go:4` mặc định `dev` (thiếu `-ldflags`); `git log --oneline -5` (`f593d23, eb4c37d, 2168127, 2b8d512, 7376f23`) vs `git status --short` Modified 32 + Untracked 26 (gồm `VERSION`, `deploy/`, `docs/`, `internal/buildinfo/`); `ls AGENTS.md` → `No such file`; `ls docs/audits` 4 file; `test_gemini_cat.png` 244K chưa mở.
- **Ảnh hưởng:** `/ready` báo `dev`, khó đối chiếu build; thiếu baseline quy ước; rác ảnh test. **Hướng sửa:** Thêm `-ldflags -X ...buildinfo.Version=$(cat VERSION)` vào script build; tạo `AGENTS.md` tối thiểu; dọn/mô tả file ảnh test. **Kiểm chứng:** `/ready` trả `0.1.3`; `git status --short` sạch sau commit có chủ ý.

## 6. Thứ tự ưu tiên sửa theo ảnh hưởng/phụ thuộc

1. **P0-1 secret Google + P0-6 login/validate:** Mọi kiểm chứng upstream đều chặn ở đây. Làm trước, song song backup DB.
2. **P0-7 audit context + P2-2 stream audit:** Có dấu vết mới debug được các lỗi còn lại.
3. **P0-4 response rỗng + P0-5 EOF/cancel:** Chặn success giả trước khi sửa failover/metric, tránh commit state cắt cụt.
4. **P0-2 lease block + P0-3 sweep + P1-2 FIFO + P1-3 atomic + P2-3 defer:** Gói điều phối chung, test tranh chấp một lần.
5. **P1-4 regex ID + P1-5 namespace session + P1-6 timeout/fail-fast:** Đúng tiếp nối và biên lỗi.
6. **P1-1 Priority/quota + P1-7 Singleton Windows + P2-1 ready deep + P2-4 vệ sinh build:** Vận hành và quan sát, làm sau cùng.

## 7. Giới hạn, dữ liệu cần bổ sung, bài thử chưa làm

- **Chưa chạy:** toàn bộ suite `gateway/session/scheduler/workerhub` ngoài các lệnh §1; benchmark tranh chấp nghìn goroutine; giả lập stream dài hơn `TimeoutSec` trên live; gọi upstream Gemini/Copilot/DeepSeek thật; truy vấn SQLite prod read-only (`observability_events` 2h/24h, `credentials`, `chat_session_bindings`); `journalctl`; kiểm TTS/DeepSeek cooldown/`copilot-refresh`/timezone; mở pixel `test_gemini_cat.png`; map `msedge --user-data-dir` từng PID.
- **Cần bổ sung từ prod (qua kênh có quyền, không in secret):** `VERSION` + `sha256` binary + `service file` + `MainPID/fd`; output `git log --oneline -5` + `git status --short` trên prod; `sqlite` count 2h/24h + mẫu lỗi đã che; `journalctl` cửa sổ lỗi; `config/models.yaml` prod (đối chiếu `vision` lite); `data/profiles` prod + `Preferences` email đã che.
- **Lưu ý phương pháp:** Mọi claim prod trong đầu vào chưa đủ lệnh/output mới được hạ xuống `unconfirmed`; không gọi request chat thật trong đợt này để tránh hao quota và xáo mapping; mọi secret chỉ đọc qua `SanitizeCookie`/`Tokens.Get`, đầu ra đã che.
