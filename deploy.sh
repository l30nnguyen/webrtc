#!/bin/bash
# Build a release package (release/make_release.sh) and deploy it.
#
#   dev:  the dev VPS; installs the package's binary (as matrix_cc) and player
#         into /home/admin/webrtc-dev-sid and restarts only PM2 webrtc_dev
#         (8443), keeping its runtime arguments.
#   prod: the Control Center matrix.5gen.care (DigitalOcean, vps.sh prod_us);
#         runs the package's setup.sh upgrade (systemd matrix.service, config
#         and data kept).
#   sg:   the control node matrix-sg00.5gen.care (vps.sh prod_sg); same.
#
# Usage: ./deploy.sh dev|prod|sg
#   ALLOW_DIRTY=1 deploys uncommitted changes as a -dirty test package.
set -euo pipefail

TARGET="${1:-}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "$TARGET" in
    dev)
        HOST="${DEPLOY_HOST:-admin@ec2-3-95-228-203.compute-1.amazonaws.com}"
        KEY="${DEPLOY_KEY:-$ROOT/../webrtc_instance.pem}"
        ;;
    prod)
        HOST="${DEPLOY_HOST:-root@138.197.12.238}"
        KEY="${DEPLOY_KEY:-$ROOT/../prod_do.pem}"
        CHECK=https://matrix.5gen.care/api/health
        ;;
    sg)
        HOST="${DEPLOY_HOST:-root@152.42.232.159}"
        KEY="${DEPLOY_KEY:-$ROOT/../prod_do.pem}"
        CHECK=https://matrix-sg00.5gen.care/api/health
        ;;
    *) echo "Usage: $0 dev|prod|sg" >&2; exit 1 ;;
esac
SSH=(ssh -i "$KEY" -o ConnectTimeout=15)
SCP=(scp -i "$KEY" -o ConnectTimeout=15)

if [[ "$TARGET" != dev ]]; then
    read -r -p "Deploy $(git -C "$ROOT" rev-parse --short HEAD) to PRODUCTION ($TARGET, ${CHECK%/api/health})? Type 'prod' to continue: " answer
    [[ "$answer" == prod ]] || { echo "Aborted."; exit 1; }
fi

# make_release.sh refuses uncommitted changes (unless ALLOW_DIRTY=1) and stops
# on a build failure.
"$ROOT/release/make_release.sh"
NAME="$(cat "$ROOT/dist/LATEST")"
TARBALL="$ROOT/dist/$NAME.tar.gz"

STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
echo "Uploading $NAME to $TARGET..."
"${SSH[@]}" "$HOST" "mkdir -p ~/releases"
"${SCP[@]}" "$TARBALL" "$TARBALL.sha256" "$HOST:releases/"

if [[ "$TARGET" != dev ]]; then
    "${SSH[@]}" "$HOST" bash -s -- "$NAME" <<'EOF'
set -euo pipefail
NAME=$1
cd ~/releases
sha256sum -c "$NAME.tar.gz.sha256"
rm -rf "$NAME" && tar xzf "$NAME.tar.gz"
sudo "./$NAME/setup.sh" upgrade
EOF
    curl -sf --max-time 10 "$CHECK" && echo
    echo "Deployment of $NAME to $TARGET complete!"
    exit 0
fi

DEST=/home/admin/webrtc-dev-sid
APP=webrtc_dev
PORT=8443
BACKUP="$DEST/backups/release-${NAME#matrix-server-}-$STAMP"
"${SSH[@]}" "$HOST" bash -s -- "$NAME" "$DEST" "$APP" "$PORT" "$BACKUP" <<'EOF'
set -euo pipefail
NAME=$1 DEST=$2 APP=$3 PORT=$4 BACKUP=$5
cd ~/releases
sha256sum -c "$NAME.tar.gz.sha256"
rm -rf "$NAME" && tar xzf "$NAME.tar.gz"
PKG=~/releases/$NAME

mkdir -p "$BACKUP"
# Older releases installed the binary as signaling-server.
OLD=$DEST/matrix_cc
[[ -f "$OLD" ]] || OLD=$DEST/signaling-server
cp -p "$OLD" "$BACKUP/matrix_cc"
cp -p "$DEST/player/ws.html" "$BACKUP/"
[[ -f "$DEST/release.json" ]] && cp -p "$DEST/release.json" "$BACKUP/"

install -m 755 "$PKG"/bin/matrix_cc_* "$DEST/matrix_cc.new"
mv "$DEST/matrix_cc.new" "$DEST/matrix_cc"
install -m 644 "$PKG/player/ws.html" "$DEST/player/ws.html"
. "$PKG/RELEASE"
cat > "$DEST/release.json" <<JSON
{"package": "$NAME", "commit": "$commit", "go_version": "$go", "binary_sha256": "$binary_sha256", "deployed_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)", "backup": "$BACKUP"}
JSON

# PM2 keeps the script path it was started with; move it to matrix_cc once,
# with the same arguments.
SCRIPT=$(pm2 jlist | python3 -c 'import json, sys; print(next(p["pm2_env"]["pm_exec_path"] for p in json.load(sys.stdin) if p["name"] == sys.argv[1]))' "$APP")
if [[ "$SCRIPT" != "$DEST/matrix_cc" ]]; then
    pm2 jlist | python3 -c 'import json, sys; print("\0".join(next(p["pm2_env"]["args"] for p in json.load(sys.stdin) if p["name"] == sys.argv[1])), end="")' "$APP" > "$BACKUP/pm2-args"
    mapfile -d '' ARGS < "$BACKUP/pm2-args"
    pm2 delete "$APP" >/dev/null
    pm2 start "$DEST/matrix_cc" --name "$APP" --cwd "$DEST" --interpreter none -- "${ARGS[@]}" >/dev/null
    pm2 save >/dev/null
    rm -f "$DEST/signaling-server"
else
    pm2 restart "$APP" >/dev/null
fi
sleep 3
if ! curl -skf --max-time 10 "https://localhost:$PORT/api/health"; then
    echo >&2
    echo "Health check failed. Roll back with:" >&2
    echo "  cp -p $BACKUP/matrix_cc $DEST/ && cp -p $BACKUP/ws.html $DEST/player/ && pm2 restart $APP" >&2
    exit 1
fi
echo
echo "Backup: $BACKUP"
EOF
echo "Deployment of $NAME to dev complete!"
