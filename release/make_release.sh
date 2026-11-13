#!/bin/bash
# Build a release package of the Matrix signaling server:
#   dist/matrix-server-<version>-<commit>.tar.gz
# containing the linux/amd64 binary bin/matrix_cc_<version>, the player,
# cfg/version and setup.sh.
# Install or upgrade a VPS from it with setup.sh (see release/README.md).
#
# Usage: release/make_release.sh
#   ALLOW_DIRTY=1 builds from uncommitted changes for testing; the package is
#   named ...-<commit>-dirty and its RELEASE says dirty=true.
# The package name is written to dist/LATEST.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

DIRTY=false
if [[ -n "$(git -C "$ROOT" status --porcelain -- src/control player/ws.html cfg/version release)" ]]; then
    if [[ "${ALLOW_DIRTY:-}" != 1 ]]; then
        echo "Uncommitted changes in src/control, player/ws.html, cfg/version or release; commit them first (or ALLOW_DIRTY=1 for a test build)." >&2
        exit 1
    fi
    DIRTY=true
fi

VERSION="$(tr -d '[:space:]' < "$ROOT/cfg/version")"
COMMIT="$(git -C "$ROOT" rev-parse --short HEAD)"
NAME="matrix-server-$VERSION-$COMMIT"
[[ "$DIRTY" == true ]] && NAME+="-dirty"
mkdir -p "$ROOT/dist"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
PKG="$STAGE/$NAME"
mkdir -p "$PKG/bin" "$PKG/player" "$PKG/cfg"
BINARY="$PKG/bin/matrix_cc_$VERSION"

echo "Building $NAME..."
(cd "$ROOT/src/control" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$BINARY" ./src)
cp "$ROOT/player/ws.html" "$PKG/player/ws.html"
cp "$ROOT/cfg/version" "$PKG/cfg/version"
cp "$ROOT/release/setup.sh" "$ROOT/release/matrix-backup" "$ROOT/release/README.md" "$PKG/"
chmod 755 "$PKG/setup.sh" "$PKG/matrix-backup" "$BINARY"

GO_VERSION="$(go version "$BINARY" | awk '{print $2}')"
cat > "$PKG/RELEASE" <<EOF
version=$VERSION
binary=$(basename "$BINARY")
commit=$(git -C "$ROOT" rev-parse HEAD)
dirty=$DIRTY
go=$GO_VERSION
built=$(date -u +%Y-%m-%dT%H:%M:%SZ)
binary_sha256=$(sha256sum "$BINARY" | awk '{print $1}')
player_sha256=$(sha256sum "$PKG/player/ws.html" | awk '{print $1}')
EOF

tar -C "$STAGE" --owner=0 --group=0 -czf "$ROOT/dist/$NAME.tar.gz" "$NAME"
(cd "$ROOT/dist" && sha256sum "$NAME.tar.gz" > "$NAME.tar.gz.sha256")
echo "$NAME" > "$ROOT/dist/LATEST"
echo "Created dist/$NAME.tar.gz ($GO_VERSION)"
