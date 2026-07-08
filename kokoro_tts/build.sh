#!/usr/bin/env bash
set -e
export CGO_ENABLED=1
mkdir -p dist
rm -rf dist/*

package_macos() {
  local arch=$1
  local bin_name="kokoro_tts_server_darwin_${arch}"
  echo "==> Building for darwin/${arch}..."
  GOOS=darwin GOARCH=${arch} go build -ldflags="-s -w" -o "dist/${bin_name}" .

  # Get the rpath from the built binary
  local rpath=$(otool -l "dist/${bin_name}" | awk '/LC_RPATH/{getline; getline; print $2}')
  
  if [ -n "$rpath" ] && [ -d "$rpath" ]; then
    echo "    Found rpath: $rpath"
    # Change rpath to look next to the executable
    install_name_tool -add_rpath @executable_path "dist/${bin_name}" || true
    
    # Create staging dir
    mkdir -p "dist/tmp_${arch}"
    mv "dist/${bin_name}" "dist/tmp_${arch}/"
    cp "$rpath"/*.dylib "dist/tmp_${arch}/" 2>/dev/null || true
    
    # Package into tar.gz
    cd "dist/tmp_${arch}"
    tar -czf "../../dist/${bin_name}.tar.gz" *
    cd ../../
    
    rm -rf "dist/tmp_${arch}"
    echo "    Packaged to ${bin_name}.tar.gz"
  else
    echo "    Warning: No valid rpath found, packaging binary only."
    cd dist
    tar -czf "${bin_name}.tar.gz" "${bin_name}"
    cd ..
    rm "dist/${bin_name}"
  fi
}

package_macos "arm64"
package_macos "amd64"

echo "==> Building for linux/amd64..."
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o dist/kokoro_tts_server_linux_amd64 . || true

echo "==> Done:"
ls -lh dist/
