#!/bin/sh
cd /src
V="$1"
LDF="-s -w -X main.version=$V"
set -e
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$LDF" -o dist/soflink.exe ./cmd/sofmat && echo win-ok
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$LDF" -o dist/soflink-linux-amd64 ./cmd/sofmat && echo linux-amd64-ok
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$LDF" -o dist/soflink-linux-arm64 ./cmd/sofmat && echo linux-arm64-ok
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$LDF" -o dist/soflink-macos-arm64 ./cmd/sofmat && echo mac-arm-ok
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags "$LDF" -o dist/soflink-macos-intel ./cmd/sofmat && echo mac-intel-ok
echo ALL-DONE-$V
