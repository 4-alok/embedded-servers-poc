#!/usr/bin/env bash
set -e
mkdir -p dist
echo "==> Building piper_tts_server..."
GOOS=darwin  GOARCH=arm64  go build -ldflags="-s -w" -o dist/piper_tts_server_darwin_arm64   .
GOOS=darwin  GOARCH=amd64  go build -ldflags="-s -w" -o dist/piper_tts_server_darwin_amd64   .
GOOS=linux   GOARCH=amd64  go build -ldflags="-s -w" -o dist/piper_tts_server_linux_amd64    .
GOOS=windows GOARCH=amd64  go build -ldflags="-s -w" -o dist/piper_tts_server_windows_amd64.exe .
echo "==> Sizes:"; ls -lh dist/
