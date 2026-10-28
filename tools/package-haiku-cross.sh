#!/usr/bin/env bash
# Build the containerd .hpkg(s) on Linux with the host Haiku `package` tool.
# Usage: tools/package-haiku-cross.sh [arch ...]   (default: amd64 arm64)
set -euo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
PACKAGE_TOOL=${PACKAGE_TOOL:-/mnt/HaikuWork/x399/build/x86_64/objects/linux/x86_64/release/tools/package/package}
BASETAG=$(git tag -l 'v2.*' --sort=-v:refname | head -1)
AHEAD=$(git rev-list --count "$BASETAG"..HEAD 2>/dev/null || echo 0)
VERSION=$(echo "$BASETAG" | sed 's/^v//')~git.$AHEAD.$(git rev-parse --short HEAD)
[ -z "$VERSION" ] && VERSION=0.0.0
STAGE=$(mktemp -d /tmp/containerd-pkg-XXXXXX)
trap 'rm -rf -- "$STAGE"' EXIT
mkdir -p "$ROOT/artifacts" "$STAGE/bin" "$STAGE/documentation/packages/containerd"

for arch in "${@:-amd64 arm64}"; do
    haiku_arch=$arch
    [ "$arch" = "amd64" ] && haiku_arch=x86_64
    bin="build/haiku-$arch/containerd"
    [ -f "$bin" ] || { echo ">> building $arch first"; bash tools/build-haiku.sh "$arch"; }
    rm -rf "$STAGE/bin/containerd" "$STAGE/bin/containerd-shim-runc-v2" "$STAGE/.PackageInfo"
    cp "$bin" "$STAGE/bin/containerd"
    cp "build/haiku-$arch/containerd-shim-runc-v2" "$STAGE/bin/containerd-shim-runc-v2"
    cp docs/airos.md "$STAGE/documentation/packages/containerd/"
    cat > "$STAGE/.PackageInfo" <<PKGEOF
name containerd
version $VERSION-1
architecture $haiku_arch
summary "containerd for air/OS"
description "The containerd daemon for air/OS, ported from the open-source containerd project. Serves the Docker Engine (airos-works/moby) with content, image and snapshot services; the vfs snapshotter operates without kernel mounts."
packager "air/OS contributors"
vendor "air/OS"
copyrights { "2026 air/OS contributors" "The containerd Authors" }
licenses { "Apache v2" }
provides {
    containerd = $VERSION
    cmd:containerd = $VERSION
    cmd:containerd-shim-runc-v2 = $VERSION
}
requires {
    haiku >= r1~beta5
}
PKGEOF
    out="$ROOT/artifacts/containerd-$VERSION-1-${haiku_arch}.hpkg"
    "$PACKAGE_TOOL" create -C "$STAGE" "$out"
    echo ">> $out"
done
