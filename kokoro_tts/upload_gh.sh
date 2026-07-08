#!/usr/bin/env bash
# Get the latest release tag
LATEST_TAG=$(gh release view -R 4-alok/embedded-servers-poc --json tagName -q .tagName)

if [ -z "$LATEST_TAG" ]; then
    echo "Could not fetch latest release tag."
    exit 1
fi

echo "Uploading to release: $LATEST_TAG"

# Upload the binary to the latest release
gh release upload "$LATEST_TAG" kokoro_tts_server_linux_amd64 -R 4-alok/embedded-servers-poc --clobber

echo "Upload complete!"
