#!/usr/bin/env bash
# Cross-build containerd for air/OS (Haiku) from Linux.
#   tools/build-haiku.sh [arch ...]     default: amd64 arm64
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
TOOLCHAIN=${GO_HAIKU_TOOLCHAIN:-/mnt/HaikuWork/toolchains/go-haiku}
GO="$TOOLCHAIN/bin/go"
ARCHES=("$@")
[ ${#ARCHES[@]} -eq 0 ] && ARCHES=(amd64 arm64)
for arch in "${ARCHES[@]}"; do
    out="build/haiku-$arch/containerd"
    mkdir -p "$(dirname "$out")"
    echo ">> building haiku/$arch -> $out"
    GOOS=haiku GOARCH="$arch" CGO_ENABLED=0 GOFLAGS=-mod=vendor \
        "$GO" build -trimpath -ldflags="-s -w" -o "$out" ./cmd/containerd
    ls -la "$out"
done
