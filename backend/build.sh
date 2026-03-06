#!/usr/bin/env bash
# Builds backend_server for macOS (arm64 + x86_64), Linux, and Windows.
# Outputs to dist/ with platform-specific names matching what Flutter expects.
set -e

mkdir -p dist

echo "==> macOS arm64 (Apple Silicon)..."
GOOS=darwin  GOARCH=arm64  go build -ldflags="-s -w" -o dist/backend_server_darwin_arm64   .

echo "==> macOS x86_64 (Intel)..."
GOOS=darwin  GOARCH=amd64  go build -ldflags="-s -w" -o dist/backend_server_darwin_amd64   .

echo "==> Linux amd64..."
GOOS=linux   GOARCH=amd64  go build -ldflags="-s -w" -o dist/backend_server_linux_amd64    .

echo "==> Windows amd64..."
GOOS=windows GOARCH=amd64  go build -ldflags="-s -w" -o dist/backend_server_windows_amd64.exe .

echo ""
echo "==> Sizes:"
ls -lh dist/
