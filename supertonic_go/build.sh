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

  # If this is arm64 (the primary target of the host system), copy the pre-existing libonnxruntime.dylib
  # next to the executable to ensure zero-dependency, plug-and-play functionality out-of-the-box.
  local kokoro_lib="/Users/alok/Projects/adsorp/embedded-servers-poc/kokoro_tts/dist/libonnxruntime.dylib"
  if [ "${arch}" = "arm64" ] && [ -f "${kokoro_lib}" ]; then
    echo "    Packaging Apple Silicon libonnxruntime.dylib alongside the executable..."
    cp "${kokoro_lib}" "${tmp_dir}/"
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
