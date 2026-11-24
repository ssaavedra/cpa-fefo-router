#!/usr/bin/env bash
# Builds dist/fefo-router.so (or $OUT) for linux/amd64 (or $ARCH) on Debian
# bookworm (glibc 2.36, same as the eceasy/cli-proxy-api image) with a baseline
# CPU target. The Go image follows the go directive in go.mod unless $GO_IMAGE
# is set.
set -euo pipefail
cd "$(dirname "$0")/.."
arch=${ARCH:-amd64}
version=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
out=${OUT:-dist/fefo-router.so}
image=${GO_IMAGE:-golang:$(sed -n 's/^go //p' go.mod)-bookworm}
mkdir -p "$(dirname "$out")"
docker run --rm --platform "linux/$arch" \
  -v "$PWD":/src -w /src \
  -v fefo-gomod:/go/pkg/mod -v fefo-gocache:/root/.cache/go-build \
  -e CGO_ENABLED=1 -e GOAMD64=v1 -e CGO_CFLAGS="-O2" -e GOTOOLCHAIN=local \
  "$image" \
  go build -trimpath -buildvcs=false -buildmode=c-shared \
    -ldflags "-s -w -X github.com/ssaavedra/cpa-fefo-router/internal/router.Version=$version" \
    -o "$out" ./plugin
rm -f "${out%.so}.h"
for sym in cliproxy_plugin_init cliproxyPluginCall cliproxyPluginFree cliproxyPluginShutdown; do
  docker run --rm --platform "linux/$arch" -v "$PWD":/src -w /src "$image" \
    sh -c "nm -D --defined-only '$out' | grep -q ' $sym\$'" || { echo "missing export $sym" >&2; exit 1; }
done
ls -la "$out"
