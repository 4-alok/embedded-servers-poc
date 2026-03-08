#!/usr/bin/env bash
set -e
export CGO_ENABLED=1
mkdir -p dist
echo "==> Building kokoro_tts_server (CGO_ENABLED=1)..."
go build -ldflags="-s -w" -o dist/kokoro_tts_server .
echo "==> Done:"; ls -lh dist/
