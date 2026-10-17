#!/usr/bin/env bash
# Build verloc: dashboard SPA (embedded via go:embed) + Go binary.
# Usage: ./scripts/build.sh [output]
set -euo pipefail
cd "$(dirname "$0")/.."

OUT="${1:-verloc}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

echo "==> building dashboard (npm run build → internal/server/static)"
if command -v npm >/dev/null 2>&1; then
  (cd dashboard && npm run build)
else
  echo "npm not found; using committed internal/server/static as-is"
fi

LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}"

echo "==> building verloc -> $OUT ($VERSION $COMMIT)"
CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o "$OUT" ./cmd/verloc

case "$OUT" in */*) BIN="$OUT";; *) BIN="./$OUT";; esac
echo "==> done: $OUT ($(du -h "$OUT" | cut -f1))"
"$BIN" -version || true

