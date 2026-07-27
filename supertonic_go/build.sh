#!/usr/bin/env bash
set -e

export CGO_ENABLED=1
mkdir -p dist
rm -rf dist/*

package_macos() {
  local arch=$1
  local bin_name="supertonic_tts_server_darwin_${arch}"
  echo "==> Building for darwin/${arch}..."
  
  # We use Go build with optimization flags -s and -w to reduce binary size
  GOOS=darwin GOARCH=${arch} go build -ldflags="-s -w" -o "dist/${bin_name}" .

  # Create a temporary folder for staging the package content
  local tmp_dir="dist/tmp_${arch}"
  mkdir -p "${tmp_dir}"
  mv "dist/${bin_name}" "${tmp_dir}/"

  # Ship libonnxruntime.dylib next to the executable so the server is
  # zero-dependency out-of-the-box. A tarball without the dylib crashes on
  # machines that don't have onnxruntime installed system-wide, so the arm64
  # build hard-fails if the dylib is missing.
  local onnx_lib="$(cd "$(dirname "$0")" && pwd)/libs/libonnxruntime.dylib"
  if [ "${arch}" = "arm64" ]; then
    if [ ! -f "${onnx_lib}" ]; then
      echo "ERROR: ${onnx_lib} not found — refusing to package a dylib-less arm64 tarball." >&2
      exit 1
    fi
    echo "    Packaging Apple Silicon libonnxruntime.dylib alongside the executable..."
    cp "${onnx_lib}" "${tmp_dir}/"
  else
    echo "    WARNING: ${arch} tarball ships WITHOUT libonnxruntime.dylib (no x86_64 dylib available);" >&2
    echo "             users must install onnxruntime or set ONNXRUNTIME_LIB_PATH." >&2
  fi

  # Package into tar.gz
  echo "    Creating tarball archive..."
  cd "${tmp_dir}"
  tar -czf "../../dist/${bin_name}.tar.gz" *
  cd ../../

  # Clean up staging directory
  rm -rf "${tmp_dir}"
  echo "    Successfully packaged: dist/${bin_name}.tar.gz"
}

# Package macOS architectures
package_macos "arm64"
package_macos "amd64"

# Package Linux amd64
echo "==> Building for linux/amd64..."
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dist/supertonic_tts_server_linux_amd64 . || true

if [ -f "dist/supertonic_tts_server_linux_amd64" ]; then
  cd dist
  tar -czf "supertonic_tts_server_linux_amd64.tar.gz" "supertonic_tts_server_linux_amd64"
  rm "supertonic_tts_server_linux_amd64"
  cd ..
  echo "    Successfully packaged: dist/supertonic_tts_server_linux_amd64.tar.gz"
fi

echo "==> Packaging complete! Listing build products:"
ls -lh dist/
