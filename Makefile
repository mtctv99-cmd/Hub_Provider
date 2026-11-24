BINARY := sever-provider
VERSION := $(shell cat VERSION)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DIRTY := $(shell git diff --quiet 2>/dev/null || echo -dirty)
LDFLAGS := -X sever_provider/internal/buildinfo.Version=$(VERSION) -X sever_provider/internal/buildinfo.Commit=$(COMMIT)$(DIRTY)
ifeq ($(OS),Windows_NT)
  BINARY := sever-provider.exe
endif

.PHONY: tidy test vet build check secrets run clean

tidy:
	go mod tidy

secrets:
	bash scripts/check-secrets.sh

test:
	go test ./...

vet:
	go vet ./...

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/server

check: tidy secrets test vet build

run: build
	./$(BINARY) -config config

clean:
	rm -f $(BINARY) sever-provider sever-provider.exe sever-provider.exe~
