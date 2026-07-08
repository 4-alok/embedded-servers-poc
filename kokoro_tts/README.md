# Kokoro TTS Server

A Go binary that serves the [Kokoro multi-lang TTS model](https://github.com/k2-fsa/sherpa-onnx) via a high-speed Binary-Framed IPC protocol. It is embedded inside the Adsorp Flutter app as a child process.

---

## Architecture Overview

```
Flutter App (adsorp/anuwad)
│
│  spawn process (Process.start)
▼
kokoro_tts_server_<platform>   ← This binary
│
├── Stdin  ← JSON-RPC requests from Flutter (binary framed)
├── Stdout ← Binary responses to Flutter (binary framed)
│            [8-byte header][JSON meta][raw WAV bytes]
└── Stderr ← Human-readable log lines captured by Flutter UI
```

The binary is **not** a web server in IPC mode — it communicates exclusively over pipes. No TCP port binding is required, which bypasses macOS App Sandbox restrictions and eliminates zombie-process risks.

---

## Files

| File | Purpose |
|---|---|
| `main.go` | Entry point. Handles model download, extraction, IPC server loop, HTTP server loop |
| `voices.go` | Hardcoded catalog of 54 Kokoro voices with metadata |
| `build.sh` | Cross-compilation script for macOS (arm64/amd64) |
| `my_build.sh` | Linux build script (`CGO_ENABLED=1` required for sherpa-onnx) |
| `upload_gh.sh` | Uploads built binaries to the GitHub release via `gh` CLI |
| `dist/` | Cross-compiled output binaries |
| `scripts/` | Miscellaneous helper scripts |

### Released Binaries (GitHub: `4-alok/embedded-servers-poc`)

| Binary Name | Platform |
|---|---|
| `kokoro_tts_server_darwin_arm64` | macOS Apple Silicon |
| `kokoro_tts_server_darwin_amd64` | macOS Intel |
| `kokoro_tts_server_linux_amd64` | Linux x86_64 |

---

## IPC Protocol (ADR-011)

### Frame Format

Every message (both request and response) uses an 8-byte Big-Endian fixed header:

```
[ 0-3: Total Length (Uint32BE) ][ 4-7: JSON Length (Uint32BE) ][ JSON bytes ][ Binary bytes ]
```

- **Total Length**: Size of the entire frame including the 8-byte header.
- **JSON Length**: Exact byte count of the UTF-8 JSON metadata block.
- **Binary Payload**: Raw WAV audio bytes (response only). No Base64.

### Supported Methods

| Method | Direction | Description |
|---|---|---|
| `health` | Flutter → Go | Liveness check. Returns `{"status":"ok"}` instantly. |
| `voices` | Flutter → Go | Returns list of 54 available voice objects. |
| `catalog` | Flutter → Go | Returns extended voice catalog with download status. |
| `speech` | Flutter → Go | Synthesize text. Returns WAV bytes in binary payload. |

### Speech Request Example (JSON metadata)

```json
{
  "id": "1746871234567890",
  "method": "speech",
  "input": "Hello, world!",
  "voice": "af_heart",
  "speed": 1.0
}
```

### Speech Response Example (JSON metadata)

```json
{
  "id": "1746871234567890",
  "method": "speech",
  "status": "ok",
  "sampleRate": 24000
}
```
Followed immediately by raw WAV bytes as the binary payload.

---

## Model Lifecycle

On first run, the server performs:

1. **DOWNLOAD**: Fetches `kokoro-multi-lang-v1_1.tar.bz2` (~370 MB) from sherpa-onnx GitHub releases. Reports `PROGRESS: X%` to stderr every 1%.
2. **EXTRACT**: Decompresses the archive. Reports progress per-file and per 20 MB chunk for the large `model.onnx` (310 MB) and `voices.bin` (51 MB).
3. **LOAD**: Loads the ONNX model into memory using sherpa-onnx Go bindings (CGO). Takes ~4–16 seconds depending on hardware.
4. **READY**: Starts accepting IPC requests.

**Integrity check**: The server checks for `espeak-ng-data/phontab` (a deep file extracted last) to determine if extraction is complete. If absent, extraction is re-run even if `model.onnx` already exists.

### Model Storage Location

| Platform | Path |
|---|---|
| Linux | `~/.local/share/kokoro_tts/` |
| macOS | `~/Library/Application Support/kokoro_tts/` |

---

## Building

### Linux (requires CGO + sherpa-onnx C libs)

```bash
export PATH=$PATH:~/tools/go/bin
export CGO_ENABLED=1
go build -ldflags="-s -w" -o kokoro_tts_server_linux_amd64 .
```

> **Note:** Cross-compilation for Linux from macOS is NOT possible with CGO. Must be built natively on Linux.

### macOS (cross-compile arm64/amd64)

```bash
bash build.sh
```

### Publishing to GitHub

```bash
bash upload_gh.sh
# or manually:
gh release upload v1.8.0 kokoro_tts_server_linux_amd64 -R 4-alok/embedded-servers-poc --clobber
```

---

## How Flutter Integrates This Server

See the Flutter side in `anuwad/lib/service/tts/local_engines/`:

1. **`binary_download_service.dart`**: Downloads the correct platform binary from the GitHub release tagged `latest`. Saves to `~/.local/share/ai.adsorp.app/adsorp_data/downloads/kokoro_tts/`.
2. **`local_server_service.dart`** (tag: `'kokoro'`): Spawns the binary via `Process.start`, pipes Stdin/Stdout for IPC, captures Stderr as log lines for UI display.
3. **`tts.dart` → `_ttsSpeakCustom`**: Routes synthesis requests through IPC using `LocalServerService.generateAudio()`.
4. **`AiBookAudioCacheManager`**: Pre-synthesizes upcoming paragraphs using IPC in the background and caches WAV files locally.

### Key Flutter Config

```dart
// di.dart
Get.put(LocalServerService(
  engineName: 'kokoro',
  downloader: BinaryDownloadService(...),
  useIpc: true,
  startupTimeout: Duration(minutes: 15), // Large due to first-run model extraction
), tag: 'kokoro');
```

---

## Linux-Specific Notes

- **CGO required**: The sherpa-onnx Go bindings (`github.com/k2-fsa/sherpa-onnx-go-linux`) use CGO and include platform-specific shared libraries (`.so` files). Cross-compilation requires a Linux environment.
- **Startup timeout**: Model extraction on slower disks (e.g., HDD) can take 5–12 minutes. The Flutter `LocalServerService.startupTimeout` must be ≥15 minutes.
- **Audio playback on Linux**: The Flutter app uses `just_audio_media_kit` + `media_kit_libs_linux` (backed by `mpv`) on Linux since `just_audio` has no built-in Linux backend. Requires `libmpv-dev` installed on the system.

---

## Known Issues & Gotchas

| Issue | Root Cause | Fix |
|---|---|---|
| `release not found` on `gh release upload` | Release tag doesn't exist | Create release first: `gh release create v1.8.0 -R 4-alok/embedded-servers-poc` |
| Extraction hangs on first run | Large model.onnx (310 MB) on slow disk | Normal. Wait. 15-minute timeout in Flutter is sufficient. |
| `MissingPluginException` for audio on Linux | `just_audio` has no Linux native implementation | Add `just_audio_media_kit` + call `JustAudioMediaKit.ensureInitialized(linux: true)` |
| Server re-downloads binary on every start | Binary got deleted or permissions stripped | Binary is cached in `adsorp_data/downloads/`. Check permissions. |
