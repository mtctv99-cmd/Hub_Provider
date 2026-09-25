BINARY := sever-provider
ifeq ($(OS),Windows_NT)
  BINARY := sever-provider.exe
endif

.PHONY: tidy test vet build check run clean

tidy:
	go mod tidy

test:
	go test ./...

vet:
	go vet ./...

build:
	go build -o $(BINARY) ./cmd/server

check: tidy test vet build

run: build
	./$(BINARY) -config config

clean:
	rm -f $(BINARY) sever-provider sever-provider.exe sever-provider.exe~
