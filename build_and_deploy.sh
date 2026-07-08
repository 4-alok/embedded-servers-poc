#!/usr/bin/env bash
# build_and_deploy.sh — Build TTS binaries for the current OS and deploy to GCS.
#
# Usage:
#   ./build_and_deploy.sh                       # build all engines + upload
#   ./build_and_deploy.sh --engines piper,kokoro # specific engines only
#   ./build_and_deploy.sh --skip-build           # upload existing dist/ without rebuilding
#   ./build_and_deploy.sh --status               # show GCS manifest table only
#   ./build_and_deploy.sh --dry-run              # show what would upload, don't upload
#
# Each machine uploads only its own OS's artifacts. GCS accumulates all platforms.
# manifest.json in GCS is merged (not overwritten) so other machines' entries survive.

set -euo pipefail

# ── Colours ────────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; DIM='\033[2m'; RESET='\033[0m'
TICK="${GREEN}✓${RESET}"; CROSS="${RED}✗${RESET}"; WARN="${YELLOW}⚠${RESET}"

# ── Constants ──────────────────────────────────────────────────────────────────
REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
GCS_BUCKET="gs://adsorp-ai-assets/binaries"
GCS_PUBLIC_BASE="https://storage.googleapis.com/adsorp-ai-assets/binaries"
MANIFEST_LOCAL="/tmp/tts_manifest_$$.json"
ENGINES_ALL=(piper kokoro supertonic)

# ── Detect OS + arch ───────────────────────────────────────────────────────────
_OS="$(uname -s)"
_ARCH="$(uname -m)"
case "$_OS" in
  Darwin)  BUILD_OS="darwin"  ;;
  Linux)   BUILD_OS="linux"   ;;
  MINGW*|MSYS*|CYGWIN*) BUILD_OS="windows" ;;
  *)       echo -e "${RED}Unsupported OS: $_OS${RESET}"; exit 1 ;;
esac
if [[ "$_ARCH" == "arm64" || "$_ARCH" == "aarch64" ]]; then
  BUILD_ARCH="arm64"
else
  BUILD_ARCH="amd64"
fi

# ── Arg parsing ────────────────────────────────────────────────────────────────
ENGINES=("${ENGINES_ALL[@]}")
SKIP_BUILD=false
STATUS_ONLY=false
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --engines)
      IFS=',' read -ra ENGINES <<< "$2"; shift 2 ;;
    --skip-build)
      SKIP_BUILD=true; shift ;;
    --status)
      STATUS_ONLY=true; shift ;;
    --dry-run)
      DRY_RUN=true; shift ;;
    *)
      echo -e "${RED}Unknown option: $1${RESET}"; exit 1 ;;
  esac
done

# Engine names used in directory names and binary prefixes
engine_dir()    { case "$1" in piper) echo "piper_tts";      ;; kokoro) echo "kokoro_tts";  ;; supertonic) echo "supertonic_go"; ;; esac }
engine_prefix() { case "$1" in piper) echo "piper_tts_server";; kokoro) echo "kokoro_tts_server";; supertonic) echo "supertonic_tts_server";; esac }

# ── Helpers ────────────────────────────────────────────────────────────────────
sha256_file() {
  local f="$1"
  if command -v sha256sum &>/dev/null; then
    sha256sum "$f" | awk '{print $1}'
  elif command -v shasum &>/dev/null; then
    shasum -a 256 "$f" | awk '{print $1}'
  else
    echo "unknown"
  fi
}

human_size() {
  local bytes="$1"
  if [[ "$bytes" -ge 1048576 ]]; then
    printf "%.1f MB" "$(echo "scale=1; $bytes/1048576" | bc)"
  elif [[ "$bytes" -ge 1024 ]]; then
    printf "%.1f KB" "$(echo "scale=1; $bytes/1024" | bc)"
  else
    echo "${bytes} B"
  fi
}

# Fetch manifest.json from GCS into $MANIFEST_LOCAL. Creates empty manifest if absent.
fetch_manifest() {
  if gsutil -q stat "$GCS_BUCKET/manifest.json" 2>/dev/null; then
    gsutil -q cp "$GCS_BUCKET/manifest.json" "$MANIFEST_LOCAL" 2>/dev/null || true
  fi
  if [[ ! -f "$MANIFEST_LOCAL" ]]; then
    echo '{"artifacts":{}}' > "$MANIFEST_LOCAL"
  fi
}

# Merge new artifact entry into local manifest JSON.
# Args: filename, size_bytes, sha256
merge_manifest_entry() {
  local filename="$1" size="$2" sha="$3"
  local ts; ts="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
  python3 - "$MANIFEST_LOCAL" "$filename" "$size" "$sha" "$ts" <<'PY'
import json, sys
path, name, size, sha, ts = sys.argv[1:]
with open(path) as f:
    m = json.load(f)
m.setdefault("artifacts", {})[name] = {
    "size": int(size), "sha256": sha, "uploaded_at": ts
}
m["updated_at"] = ts
with open(path, "w") as f:
    json.dump(m, f, indent=2)
PY
}

# Print GCS manifest as a table.
print_manifest_table() {
  if [[ ! -f "$MANIFEST_LOCAL" ]]; then
    echo -e "  ${DIM}(no manifest found)${RESET}"
    return
  fi
  python3 - "$MANIFEST_LOCAL" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    m = json.load(f)
arts = m.get("artifacts", {})
if not arts:
    print("  (empty)")
    sys.exit(0)

# all expected artifacts
ENGINES = ["piper_tts_server", "kokoro_tts_server", "supertonic_tts_server"]
VARIANTS = [
    "darwin_arm64.tar.gz", "darwin_amd64.tar.gz",
    "linux_amd64", "linux_arm64",
    "windows_amd64.exe",
]
# supertonic linux are .tar.gz
SUPER_LINUX = ["linux_amd64.tar.gz", "linux_arm64.tar.gz"]

header = f"  {'Artifact':<52} {'Size':>8}  {'Uploaded':>22}  Status"
print(header)
print("  " + "─" * (len(header) - 2))
for engine in ENGINES:
    for variant in (SUPER_LINUX if engine == "supertonic_tts_server" else VARIANTS):
        name = f"{engine}_{variant}"
        if name in arts:
            e = arts[name]
            sz = e.get("size", 0)
            if sz >= 1048576:
                sz_str = f"{sz/1048576:.1f} MB"
            elif sz >= 1024:
                sz_str = f"{sz/1024:.1f} KB"
            else:
                sz_str = f"{sz} B"
            ts = e.get("uploaded_at", "?")[:19].replace("T", " ")
            print(f"  {name:<52} {sz_str:>8}  {ts:>22}  ✓")
        else:
            print(f"  {name:<52} {'—':>8}  {'never':>22}  ✗")
PY
}

# ── Banner ─────────────────────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}╔══════════════════════════════════════════════════════════╗${RESET}"
echo -e "${BOLD}║  TTS Binary Build & Deploy                              ║${RESET}"
echo -e "${BOLD}║  OS: ${CYAN}${BUILD_OS}/${BUILD_ARCH}${RESET}${BOLD}  │  Engines: ${CYAN}${ENGINES[*]}${RESET}${BOLD}$(printf '%*s' $((19 - ${#BUILD_OS} - ${#BUILD_ARCH} - ${#ENGINES[*]})) '')║${RESET}"
echo -e "${BOLD}║  GCS: ${DIM}${GCS_BUCKET}${RESET}${BOLD}  ║${RESET}"
echo -e "${BOLD}╚══════════════════════════════════════════════════════════╝${RESET}"
echo ""

# ── Step 1: Current GCS state ─────────────────────────────────────────────────
echo -e "${BOLD}[1] Current GCS manifest:${RESET}"
fetch_manifest
print_manifest_table
echo ""

if $STATUS_ONLY; then
  rm -f "$MANIFEST_LOCAL"
  exit 0
fi

# ── Step 2: Build ─────────────────────────────────────────────────────────────
UPLOADED_FILES=()

if ! $SKIP_BUILD; then
  echo -e "${BOLD}[2] Building engines for ${CYAN}${BUILD_OS}/${BUILD_ARCH}${RESET}${BOLD}...${RESET}"

  for engine in "${ENGINES[@]}"; do
    dir="$(engine_dir "$engine")"
    prefix="$(engine_prefix "$engine")"
    engine_path="$REPO_ROOT/$dir"
    echo ""
    echo -e "  ${BOLD}▶ $engine${RESET}  ${DIM}($engine_path)${RESET}"

    case "$BUILD_OS" in
      darwin)
        # Use the existing build.sh — it handles darwin arm64+amd64 and attempts linux
        # Supertonic must run after kokoro so it can copy libonnxruntime.dylib
        echo -e "  ${DIM}Running $dir/build.sh ...${RESET}"
        (cd "$engine_path" && bash build.sh)
        ;;

      linux)
        mkdir -p "$engine_path/dist"
        # Clear only the linux artifacts so darwin artifacts from another run are preserved
        rm -f "$engine_path/dist/${prefix}_linux_"*
        echo -e "  ${DIM}Building linux/amd64...${RESET}"
        (
          cd "$engine_path"
          CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
            go build -ldflags="-s -w" -o "dist/${prefix}_linux_amd64" . \
            && echo -e "    ${TICK} linux/amd64  $(human_size "$(wc -c < "dist/${prefix}_linux_amd64")")" \
            || echo -e "    ${CROSS} linux/amd64  build failed"
        )
        # arm64 — only if running on arm64 hardware (native) or cross-compiler available
        if [[ "$BUILD_ARCH" == "arm64" ]] || command -v aarch64-linux-gnu-gcc &>/dev/null; then
          local_cc=""
          [[ "$BUILD_ARCH" != "arm64" ]] && local_cc="CC=aarch64-linux-gnu-gcc"
          echo -e "  ${DIM}Building linux/arm64...${RESET}"
          (
            cd "$engine_path"
            eval "CGO_ENABLED=1 GOOS=linux GOARCH=arm64 $local_cc \
              go build -ldflags=\"-s -w\" -o \"dist/${prefix}_linux_arm64\" ." \
              && echo -e "    ${TICK} linux/arm64  $(human_size "$(wc -c < "dist/${prefix}_linux_arm64")")" \
              || echo -e "    ${CROSS} linux/arm64  build failed (skipping)"
          ) || true
        fi
        # Supertonic linux bins are wrapped in .tar.gz (matching Flutter's download logic)
        if [[ "$engine" == "supertonic" ]]; then
          for arch in amd64 arm64; do
            bin="$engine_path/dist/${prefix}_linux_${arch}"
            if [[ -f "$bin" ]]; then
              (cd "$engine_path/dist" && tar -czf "${prefix}_linux_${arch}.tar.gz" "${prefix}_linux_${arch}" && rm "${prefix}_linux_${arch}")
              echo -e "    ${DIM}→ archived to ${prefix}_linux_${arch}.tar.gz${RESET}"
            fi
          done
        fi
        ;;

      windows)
        mkdir -p "$engine_path/dist"
        rm -f "$engine_path/dist/${prefix}_windows_"*
        echo -e "  ${DIM}Building windows/amd64...${RESET}"
        (
          cd "$engine_path"
          CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
            go build -ldflags="-s -w" -o "dist/${prefix}_windows_amd64.exe" . \
            && echo -e "    ${TICK} windows/amd64  $(human_size "$(wc -c < "dist/${prefix}_windows_amd64.exe")")" \
            || echo -e "    ${CROSS} windows/amd64  build failed"
        ) || true
        ;;
    esac
  done
else
  echo -e "${BOLD}[2] Skipping build (--skip-build)${RESET}"
fi

# ── Collect artifacts ──────────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}[3] Collecting artifacts...${RESET}"

ALL_ARTIFACTS=()
for engine in "${ENGINES[@]}"; do
  dir="$(engine_dir "$engine")"
  engine_path="$REPO_ROOT/$dir/dist"
  if [[ -d "$engine_path" ]]; then
    while IFS= read -r -d '' f; do
      fname="$(basename "$f")"
      # Filter to only upload files for the current OS (don't accidentally upload stale macOS
      # artifacts from a previous run when we're now on Linux, etc.)
      case "$BUILD_OS" in
        darwin)  [[ "$fname" == *_darwin_* || "$fname" == *_linux_* ]] && ALL_ARTIFACTS+=("$f") ;;
        linux)   [[ "$fname" == *_linux_* ]] && ALL_ARTIFACTS+=("$f") ;;
        windows) [[ "$fname" == *_windows_* ]] && ALL_ARTIFACTS+=("$f") ;;
      esac
    done < <(find "$engine_path" -maxdepth 1 -type f \( -name "*.tar.gz" -o -name "*.exe" -o -name "*_linux_amd64" -o -name "*_linux_arm64" \) -print0)
  fi
done

if [[ ${#ALL_ARTIFACTS[@]} -eq 0 ]]; then
  echo -e "  ${WARN} No artifacts found to upload."
  rm -f "$MANIFEST_LOCAL"
  exit 0
fi

echo -e "  Found ${#ALL_ARTIFACTS[@]} artifact(s):"
for f in "${ALL_ARTIFACTS[@]}"; do
  sz="$(wc -c < "$f")"
  echo -e "    ${DIM}$(basename "$f")  $(human_size "$sz")${RESET}"
done

# ── Step 4: Upload ─────────────────────────────────────────────────────────────
echo ""
if $DRY_RUN; then
  echo -e "${BOLD}[4] Dry run — would upload:${RESET}"
  for f in "${ALL_ARTIFACTS[@]}"; do
    echo -e "  ${CYAN}$GCS_BUCKET/$(basename "$f")${RESET}"
  done
  echo ""
  echo -e "  ${WARN} Dry run complete. No files uploaded."
  rm -f "$MANIFEST_LOCAL"
  exit 0
fi

echo -e "${BOLD}[4] Uploading to GCS...${RESET}"
UPLOAD_OK=0
UPLOAD_FAIL=0

for f in "${ALL_ARTIFACTS[@]}"; do
  fname="$(basename "$f")"
  sz="$(wc -c < "$f")"
  sha="$(sha256_file "$f")"
  echo -e "  → ${fname}  $(human_size "$sz")"
  # Show gsutil progress (no -q); redirect stderr→stdout so progress bar is visible
  if gsutil -h "Cache-Control:no-cache" cp "$f" "$GCS_BUCKET/$fname" 2>&1; then
    echo -e "    ${TICK} uploaded"
    merge_manifest_entry "$fname" "$sz" "$sha"
    UPLOAD_OK=$((UPLOAD_OK + 1))
    UPLOADED_FILES+=("$fname")
  else
    echo -e "    ${CROSS} upload failed"
    UPLOAD_FAIL=$((UPLOAD_FAIL + 1))
  fi
  echo ""
done

# ── Step 5: Update manifest.json ───────────────────────────────────────────────
if [[ $UPLOAD_OK -gt 0 ]]; then
  echo -ne "  → manifest.json  "
  if gsutil -q -h "Cache-Control:no-cache" cp "$MANIFEST_LOCAL" "$GCS_BUCKET/manifest.json" 2>/dev/null; then
    echo -e "${TICK}"
  else
    echo -e "${WARN} Failed to update manifest (artifacts uploaded fine)"
  fi
fi

# ── Summary ────────────────────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}╔══════════════════════════════════════════════════════════╗${RESET}"
if [[ $UPLOAD_FAIL -eq 0 ]]; then
  echo -e "${BOLD}║  ${GREEN}Done!${RESET}${BOLD} ${UPLOAD_OK} file(s) uploaded to GCS.$(printf '%*s' $((28 - ${#UPLOAD_OK})) '')║${RESET}"
else
  echo -e "${BOLD}║  ${YELLOW}Done with warnings:${RESET}${BOLD} ${UPLOAD_OK} uploaded, ${RED}${UPLOAD_FAIL} failed${RESET}${BOLD}.$(printf '%*s' $((14 - ${#UPLOAD_OK} - ${#UPLOAD_FAIL})) '')║${RESET}"
fi
echo -e "${BOLD}║                                                          ║${RESET}"
echo -e "${BOLD}║  Run ${CYAN}--status${RESET}${BOLD} on other machines to check coverage.     ║${RESET}"
echo -e "${BOLD}╚══════════════════════════════════════════════════════════╝${RESET}"

if [[ ${#UPLOADED_FILES[@]} -gt 0 ]]; then
  echo ""
  echo -e "${DIM}Public URLs:${RESET}"
  for fname in "${UPLOADED_FILES[@]}"; do
    echo -e "  ${DIM}${GCS_PUBLIC_BASE}/${fname}${RESET}"
  done
fi

echo ""
rm -f "$MANIFEST_LOCAL"
