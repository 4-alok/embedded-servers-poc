#!/usr/bin/env bash
export PATH=$PATH:~/tools/go/bin
export CGO_ENABLED=1
go build -o kokoro_tts_server .
