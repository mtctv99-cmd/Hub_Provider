# Fix Gemini — 2026-10-06

## 1. Kết luận build test

- Build pass. Gate: `build=pass gateway=0 gemini=0 core=0`.
- Test chạy trong ask này: `go test -count=1 ./internal/scheduler/... ./internal/pool/... ./internal/session/... ./internal/provider/gemini/... ./internal/gateway/... ./internal/admin/... ./internal/browser/... ./internal/identity/... ./internal/buildinfo/...` → PASS hết (8 package ok, 1 package no test files).
- 5 nhóm fix được duyệt hết (`approved:true`).
- Không in secret.

## 2. Bảng nhóm file đổi

| Nhóm | File đổi |
|---|---|
| scheduler-lease | `internal/scheduler/lease.go:32-64,112-230,255-266`, `internal/scheduler/dispatch.go:97-98,155-168,233,241-282,355-356,401-403,467-469,507-510,872-918`, `internal/pool/allocator.go:20-41,64-98`, `internal/session/persistent.go:108-119` |
| adapter | `internal/provider/gemini/rpc.go:32-47,75-77,97-112,156-157,210-223,283-290,314-318`, `internal/provider/gemini/parser.go:10-44,70-76,90-93,190-205`, `internal/provider/gemini/rpc_regression_test.go:1-125` |
| gateway-audit-session | `internal/admin/audit.go:37`, `internal/gateway/handlers.go:138,154,370,417,547,553,603,616`, `internal/gateway/superset_handlers.go:53,68,81` |
| browser-auth | `internal/browser/browser.go:43-57,153-157,309,484-528`, `internal/browser/refresh.go:195-212`, `internal/browser/refresh_test.go:89-132`, `internal/identity/manager.go:34,93,109,114-128`, `internal/identity/manager_test.go:206-244`, `cmd/server/main.go:80-86,100,189-211,323-339`, `cmd/server/refresh_accounts.go:50` |
| build | `Makefile:2-3,20`, `scripts/build.sh:7,13`, `internal/buildinfo/version.go:1-5`, `AGENTS.md:1-21` |

## 3. Chi tiết P0 P1 P2 đã sửa và test

### scheduler-lease
- P0-2: `internal/scheduler/lease.go:149-162` `AcquireCredential` fail-fast `ErrNoCapacity`; `internal/scheduler/dispatch.go:877-892` thử candidate tiếp qua `tryAcquireFresh` non-blocking.
- P0-3: `internal/scheduler/lease.go:50-53` `StreamLeaseTTL` 30m, `:164-179` `Renew`, `:257-267` `Release` idempotent qua `ErrLeaseNotFound`; `internal/scheduler/dispatch.go:404-407` `acquireStreamLease`, `:465-472` `wrapRelease` defer `Release`, `:510-511` `Renew` mỗi chunk.
- P2-3: `internal/scheduler/dispatch.go:158-163` `chatOnce` bọc `adapter.Chat` trong closure defer release.
- Phụ: `internal/pool/allocator.go:87-92` sort `Active` trước `Priority`; `internal/scheduler/dispatch.go:762-771` refresh `ActiveLeases` live; `internal/scheduler/lease.go:115-124` FIFO 128 fail-fast; `internal/scheduler/dispatch.go:260-274` namespace sha256 idempotent.
- Test: `go test -v -count=1 ./internal/scheduler/... ./internal/pool/... ./internal/session/...` PASS (scheduler 20, pool 1, session 5); `go vet` PASS; `gofmt -w` PASS.

### adapter
- Response rỗng → lỗi `empty upstream response`, không `stop` giả: `internal/provider/gemini/rpc.go:75-77,314-318`.
- Stream cắt EOF/cancel giữa chừng → trả `Err`, không `Done`: `internal/provider/gemini/rpc.go:210-223,199-203`.
- Bỏ regex fallback toàn văn, chỉ nhận ID hợp lệ qua `validConversationID/validResponseID/validChoiceID` từ frame `wrb.fr`: `internal/provider/gemini/parser.go:10-44,70-76,90-93,190-205`.
- Giữ deadline stream minh bạch, fail-fast khi sanitize cookie lỗi.
- Test: `go test -count=1 -v ./internal/provider/gemini/...` PASS 21/21; `go test ./internal/provider/gemini/... ./internal/gateway/... -run Google` PASS; test mới `internal/provider/gemini/rpc_regression_test.go:24-36` `TestChatEmpty200Fails`, `:50-85` `TestStreamCutMidwayErrors`.
- Rủi ro: upstream đổi tiền tố ID `wrb.fr` khác `c_/r_/rc_` cần cập nhật regex validator.

### gateway-audit-session
- P0-7: `internal/admin/audit.go:37` dùng `context.WithoutCancel` + timeout 5s, tránh mất log khi timeout/cancel.
- P2-2: `internal/gateway/handlers.go:138,154,370,417,547,553,603,616` ghi audit sau `Done/Err` cho streamChat kèm `Latency` và `TTFT`.
- P1-5: `internal/gateway/superset_handlers.go:53-57` validate `X-Session-ID` tối đa 256 ký tự, quá trả HTTP 400.
- P2-1: probe upstream `ValidateCookie` có rate-limit giữ `/ready` nhẹ.
- Test: `go test ./internal/gateway/... -count=1` PASS; `go test ./internal/admin/... -count=1` PASS.
- Rủi ro: probe Gemini phụ thuộc mạng/cookie thật; gọi dày bị Google rate limit ngoài đệm 10s local.

### browser-auth
- `internal/browser/browser.go:46-57` hash email thành AccountID ổn định; `:309,484-528` đóng graceful qua CDP trước kill.
- `internal/browser/refresh.go:203-205` dọn file singleton lock stale, giữ nguyên profile dir, log rõ GOOS; Windows đang có Chrome/Edge giữ lock vẫn báo `ErrAutoLoginBusy`.
- `cmd/server/main.go:191-201` callback autologin bắt buộc `ValidateCookie` trước `Upsert` `HEALTHY`; `:80-86` fail-fast khi `MASTER_KEY` rỗng.
- `internal/identity/manager.go:119-128` chặn ghi đè secret khi không giải mã được.
- Test: `go test -count=1 -v ./internal/browser/...` PASS 15/15 (gồm `TestAccountIDForEmail`, `TestExistingProfileCDPWindowsLockCleanup`); `go test -count=1 -v ./internal/identity/...` PASS 6/6 (gồm `TestUpsertGoogleSecretNotOverwrittenWhenUndecryptable`); `go build ./...` PASS.

### build
- `Makefile:3,20` và `scripts/build.sh:13` inject `-ldflags -X .../buildinfo.Version` từ `VERSION:1`.
- `internal/buildinfo/version.go:1-5`, `AGENTS.md:1-21` hướng dẫn build/test tối thiểu.
- Ảnh `test_gemini_cat.png` nằm ngoài repo tại Desktop, `git ls-files` xác nhận không tracked.
- Test: `gofmt -l internal/buildinfo/version.go` exit 0; `go build ./internal/buildinfo/...` exit 0; `bash scripts/build.sh` exit 0; `go build ./...` exit 0.
- Rủi ro: binary prod cần build qua `make build` hoặc `bash scripts/build.sh` mới nhận đúng version, không `dev`.

## 4. Việc còn lại cần user

- `MASTER_KEY` prod: secret Google hiện không khớp key config, giữ nguyên bản mã chống mất dữ liệu. Cần cấp đúng `MASTER_KEY` hoặc chạy browser autologin mới lưu credentials hợp lệ.
- Đăng nhập lại: login cùng email trước tạo profile mới do ID ngẫu nhiên cũ; cần đăng nhập lại để gom về ID hash ổn định.
- Dọn orphan: 5 profile orphan, 2 cặp trùng email, 1 rác chưa login trong `data/profiles`. Chưa xóa tự động.
- Gateway chưa gắn API key vào ctx (`scheduler.SessionNSKey`) nên namespace hiện no-op; cần 1 sửa ở gateway ngoài phạm vi nhóm này.
- Stream treo không chunk quá 30 phút vẫn bị sweep (chấp nhận theo audit).
- Thiếu test chuyên biệt cho fail-fast đa credential, Renew chống sweep, namespace tách 2 key, FIFO (phạm vi file sở hữu không cho tạo file test mới).

## 5. Lệnh kiểm chứng và rollback

Kiểm chứng:

```sh
go test -count=1 ./internal/scheduler/... ./internal/pool/... ./internal/session/... ./internal/provider/gemini/... ./internal/gateway/... ./internal/admin/... ./internal/browser/... ./internal/identity/... ./internal/buildinfo/...
go test -count=1 -v ./internal/provider/gemini/...
go test ./internal/gateway/ -run Google
bash scripts/build.sh
go build ./...
```

Rollback revert từng file:

```sh
git restore --staged --worktree internal/scheduler/lease.go internal/scheduler/dispatch.go internal/pool/allocator.go internal/session/persistent.go
git restore --staged --worktree internal/provider/gemini/rpc.go internal/provider/gemini/parser.go internal/provider/gemini/rpc_regression_test.go
git restore --staged --worktree internal/admin/audit.go internal/gateway/handlers.go internal/gateway/superset_handlers.go
git restore --staged --worktree internal/browser/browser.go internal/browser/refresh.go internal/browser/refresh_test.go internal/identity/manager.go internal/identity/manager_test.go cmd/server/main.go cmd/server/refresh_accounts.go
git restore --staged --worktree Makefile scripts/build.sh internal/buildinfo/version.go AGENTS.md
```
