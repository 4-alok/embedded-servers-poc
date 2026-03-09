#!/usr/bin/env bash
set -e
export CGO_ENABLED=1
mkdir -p dist
echo "==> Building piper_tts_server (CGO_ENABLED=1)..."
go build -ldflags="-s -w" -o dist/piper_tts_server .
echo "==> Done:"; ls -lh dist/
