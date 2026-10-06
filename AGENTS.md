# AGENTS.md

Quy uoc toi thieu cho agent sua repo Sever_provider.

## Build

- `make build` — build binary, inject version tu file VERSION qua `-ldflags -X sever_provider/internal/buildinfo.Version`.
- `bash scripts/build.sh` — tuong duong make build khi khong co make (Windows git-bash).
- Kiem tra version: chay binary, log khoi dong phai in version trong VERSION, khong bao `dev`.
- `/ready` tra `version` tu `internal/buildinfo/version.go`.

## Test

- `go test ./internal/buildinfo/...` — check version (neu co test).
- `go build ./...` — kiem tra build lien quan sau khi sua Makefile/buildinfo.
- Khong commit, khong in secret.

## Anh test

- `test_gemini_cat.png` (244K, 512x279 PNG) nam ngoai repo tai Desktop, khong tracked trong git. Giu nguyen, khong dua vao repo.
