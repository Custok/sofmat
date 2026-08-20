# sofmat — Go mono-stack build.
.PHONY: build test vet lint tidy all

all: tidy vet test build

build:
	go build ./...

# CGO is needed for the engine binding (libllama). Disable it to build the
# pure-Go planes (transport/gateway/solver) without the native engine.
build-nocgo:
	CGO_ENABLED=0 go build ./cmd/... ./internal/transport/... ./internal/gateway/... ./internal/partitioner/...

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy
