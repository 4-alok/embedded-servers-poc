# Piper TTS Server

A Go binary that serves [Piper](https://github.com/rhasspy/piper) VITS ONNX neural TTS models via a lightweight HTTP REST API. It is embedded inside the Adsorp Flutter app as a child process.

---

## Architecture Overview

```
Flutter App (adsorp/anuwad)
│
│  spawn process (Process.start)
▼
piper_tts_server_<platform>   ← This binary
│
└── HTTP Server on dynamic localhost port
    ├── GET  /health          ← Liveness probe
    ├── GET  /v1/audio/voices ← List available voices
    └── POST /v1/audio/speech ← Synthesize text → WAV bytes
```

Unlike the Kokoro server, Piper uses **HTTP on localhost** (not IPC). Flutter polls the health endpoint and talks to it via standard REST calls. This is the **legacy architecture** — Kokoro has since been migrated to Binary IPC (ADR-011). Piper migration to IPC is planned but not yet executed.

---

## Files

| File | Purpose |
|---|---|
| `main.go` | Entry point. HTTP server, model download/load, synthesis handler |
| `voices.go` | Comprehensive list of Piper ONNX voice models with download URLs |
| `build.sh` | Cross-compilation script for macOS (arm64/amd64) |
| `dist/` | Cross-compiled output binaries |
| `scripts/` | Miscellaneous helper scripts |

### Released Binaries (GitHub: `4-alok/embedded-servers-poc`)

| Binary Name | Platform |
|---|---|
| `piper_tts_server_darwin_arm64` | macOS Apple Silicon |
| `piper_tts_server_darwin_amd64` | macOS Intel |
| `piper_tts_server_linux_amd64` | Linux x86_64 |

---

## HTTP API

### `GET /health`

Returns immediately (even before model is loaded):
```json
{"status": "ok"}
```

### `GET /v1/audio/voices`

Returns available voices. Empty list `[]` if the model is still loading.

```json
[
  {
    "id": "en_US-lessac-medium",
    "name": "English (US) Lessac Medium",
    "language": "en-US",
    "quality": "medium"
  }
]
```

### `POST /v1/audio/speech`

```json
{
  "model": "en_US-lessac-medium",
  "input": "Hello, world!",
  "speed": 1.0
}
```

**Response**: Raw WAV audio bytes (`Content-Type: audio/wav`).
Returns `503 Service Unavailable` if the model hasn't loaded yet.

---

## Fast-Start Pattern

The server binds its HTTP port **before** the model is loaded. This eliminates the visible "Starting..." delay in the Flutter UI:

```go
var modelReadyCh = make(chan struct{})

func main() {
  // 1. HTTP server starts IMMEDIATELY
  go http.ListenAndServe(addr, mux)

  // 2. Model downloads + loads in background
  ensureVoice(defaultVoice, modelDir)
  tts = sherpa.NewOfflineTts(config)

  // 3. Signal that synthesis is ready
  close(modelReadyCh)

  select {} // Keep alive
}
```

Flutter's `/health` poll succeeds in <1 second → status turns green immediately. Voices appear as soon as the model finishes loading, with Flutter retrying `GET /v1/audio/voices` every 3 seconds.

---

## Voice Hot-Swapping

Piper supports switching voices at runtime without restarting the server:

```go
var ttsLock sync.RWMutex

func handleSpeech(w http.ResponseWriter, r *http.Request) {
  req := parseRequest(r)

  ttsLock.Lock()
  if req.Model != currentModel {
    tts = sherpa.NewOfflineTts(buildConfig(req.Model))
    currentModel = req.Model
  }
  ttsLock.Unlock()

  ttsLock.RLock()
  defer ttsLock.RUnlock()
  audio := tts.Generate(req.Input, req.Speed)
  writeWAV(w, audio)
}
```

---

## Model Storage

```
{modelDir}/
└── {voiceId}/
    ├── {voiceId}.onnx       ← ONNX model weights
    └── {voiceId}.onnx.json  ← Config (sample rate, phoneme set, etc.)
```

Downloaded as `.tar.bz2` from sherpa-onnx GitHub releases.

**Sample rates**: Piper outputs **22050 Hz** WAV (not 24000 Hz like Kokoro). `just_audio` / `media_kit` handles resampling automatically.

---

## Building

### macOS (cross-compile)

```bash
bash build.sh
```

### Linux (requires CGO)

```bash
export CGO_ENABLED=1
go build -ldflags="-s -w" -o piper_tts_server_linux_amd64 .
```

### Publishing to GitHub

```bash
gh release upload v1.8.0 piper_tts_server_linux_amd64 -R 4-alok/embedded-servers-poc --clobber
```

---

## How Flutter Integrates This Server

1. **`binary_download_service.dart`**: Downloads the correct binary from the GitHub `latest` release. Saves to `adsorp_data/downloads/piper_tts/`.
2. **`local_server_service.dart`** (tag: `'piper'`): Spawns the binary, passes `--port {freePort}` and `--model-dir {dir}`. Polls `/health` every 1s until alive. Polls `/v1/audio/voices` every 3s until non-empty.
3. **`tts.dart` → `_ttsSpeakCustom`**: Issues `POST /v1/audio/speech` via HTTP to the local port. Caches the WAV result using `AudioCacheManager`.

### Key Flutter Config

```dart
// di.dart
Get.put(LocalServerService(
  engineName: 'piper',
  downloader: BinaryDownloadService(...),
  useIpc: false,           // Piper uses HTTP, not IPC
  startupTimeout: Duration(minutes: 5),
), tag: 'piper');
```

---

## Differences vs. Kokoro

| Aspect | Piper | Kokoro |
|---|---|---|
| **Communication** | HTTP REST on localhost | Binary-Framed IPC (Stdin/Stdout) |
| **Port binding** | Yes (dynamic port) | No (IPC only) |
| **macOS Sandbox** | Requires network entitlements | No entitlements needed |
| **Zombie process risk** | Yes (HTTP server survives parent death) | No (EOF on Stdin triggers clean exit) |
| **Audio format** | 22050 Hz WAV | 24000 Hz WAV |
| **Model size** | ~60–120 MB per voice | ~370 MB total (one model, 54 voices) |
| **Voice selection** | Per-synthesis (`model` field) | Per-synthesis (`voice` field) |
| **IPC migration** | Planned (not done) | ✅ Complete (ADR-011) |

---

## Known Issues & Gotchas

| Issue | Root Cause | Fix |
|---|---|---|
| Port collision on restart | Old process still alive on port | Kill old process: `pkill piper_tts_server` |
| Voices empty indefinitely | Model download failed | Check network, check model dir permissions |
| 503 on synthesis | Model not ready yet | Wait for voice list to populate |
| Audio garbled | Hardcoded sample rate mismatch | Verify player accepts 22050 Hz WAV |
| Process orphaned after crash | `Process.kill()` not called | Kill manually; IPC migration would fix this permanently |
