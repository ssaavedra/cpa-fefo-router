#!/usr/bin/env bash
# Builds dist/fefo-router.so for linux/amd64 (or $ARCH) on Debian bookworm
# (glibc 2.36, same as the eceasy/cli-proxy-api image) with a baseline CPU target.
set -euo pipefail
arch=${ARCH:-amd64}
version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
cd "$(dirname "$0")/.."
mkdir -p dist
docker run --rm --platform "linux/$arch" \
  -v "$PWD":/src -w /src \
  -v fefo-gomod:/go/pkg/mod -v fefo-gocache:/root/.cache/go-build \
  -e CGO_ENABLED=1 -e GOAMD64=v1 -e CGO_CFLAGS="-O2" -e GOTOOLCHAIN=local \
  golang:1.26.10-bookworm \
  go build -trimpath -buildvcs=false -buildmode=c-shared \
    -ldflags "-s -w -X github.com/ssaavedra/cpa-fefo-router/internal/router.Version=$version" \
    -o "dist/fefo-router.so" ./plugin
rm -f dist/fefo-router.h
for sym in cliproxy_plugin_init cliproxyPluginCall cliproxyPluginFree cliproxyPluginShutdown; do
  docker run --rm --platform "linux/$arch" -v "$PWD/dist":/d golang:1.26.10-bookworm \
    sh -c "nm -D --defined-only /d/fefo-router.so | grep -q ' $sym\$'" || { echo "missing export $sym" >&2; exit 1; }
done
ls -la dist/fefo-router.so
