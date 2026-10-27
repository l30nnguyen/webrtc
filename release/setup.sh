#!/bin/bash
# Install or upgrade the Matrix signaling server from this release package.
# Run as root from the extracted package folder on Ubuntu.
#
# Fresh install (gets a Let's Encrypt certificate, auto-renewed by certbot.timer):
#   sudo ./setup.sh install --domain matrix.5gen.care --keys /path/to/keys \
#        [--port 443] [--kid NAME] [--min-issued 2026-10-01T00:00:00Z] [--discovery-tenant test01] \
#        [--turn turn.conf] [--player-allow 203.0.113.5,10.0.0.0/8]
#   The keys folder must hold issuer.pub.pem, center-ticket.key.pem and
#   tenants.json; blocklist.txt and center-ticket.pub.pem are copied if present.
#
# Set or replace the TURN servers handed to cameras (JSON list of
# {"host", "port", "username", "password"}; read on every connection, no restart):
#   sudo ./setup.sh turn /path/to/turn.conf
#
# Fleet (optional, read at start): /opt/matrix/cfg/sid/nodes.json lists the
# control server nodes ({"nodes":[{"address","region","countries","capacity",
# "token_sha256"}]}); /opt/matrix/cfg/geoip.csv (DB-IP IP-to-Country Lite CSV)
# adds the country choice. Run "setup.sh upgrade" after changing either.
#
# Upgrade an installed server to this package (config, keys and data are kept;
# edit /etc/matrix/matrix.conf first to change a setting, e.g. ASSIGN_ADDRESS=
# wss://node.example:443/ to send new cameras and their viewers to another node):
#   sudo ./setup.sh upgrade
set -euo pipefail

APP_DIR=/opt/matrix
DATA_DIR=$APP_DIR/cfg/sid
CONF=/etc/matrix/matrix.conf
UNIT=/etc/systemd/system/matrix.service
HOOK=/etc/letsencrypt/renewal-hooks/deploy/matrix.sh
SERVICE_USER=matrix
PKG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "setup: $*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "run as root (sudo)"
[[ -x "$PKG_DIR/bin/signaling-server" ]] || die "run from the extracted release package"

# Copies this package's binary, player and version into APP_DIR, keeping the
# previous ones in APP_DIR/backups/<time>/.
install_release() {
    if [[ -f "$APP_DIR/signaling-server" ]]; then
        local backup="$APP_DIR/backups/$(date -u +%Y%m%dT%H%M%SZ)"
        mkdir -p "$backup/player" "$backup/cfg"
        cp -p "$APP_DIR/signaling-server" "$backup/"
        cp -p "$APP_DIR/player/ws.html" "$backup/player/" 2>/dev/null || true
        cp -p "$APP_DIR/cfg/version" "$backup/cfg/" 2>/dev/null || true
        cp -p "$APP_DIR/RELEASE" "$backup/" 2>/dev/null || true
        echo "Backed up the previous release to $backup"
    fi
    install -d -m 755 "$APP_DIR" "$APP_DIR/player" "$APP_DIR/cfg" "$APP_DIR/downloads"
    install -m 755 "$PKG_DIR/bin/signaling-server" "$APP_DIR/signaling-server.new"
    mv "$APP_DIR/signaling-server.new" "$APP_DIR/signaling-server"
    install -m 644 "$PKG_DIR/player/ws.html" "$APP_DIR/player/ws.html"
    install -m 644 "$PKG_DIR/cfg/version" "$APP_DIR/cfg/version"
    install -m 644 "$PKG_DIR/RELEASE" "$APP_DIR/RELEASE"
}

write_unit() {
    local discovery=""
    [[ -n "${DISCOVERY_TENANT:-}" ]] && discovery=" -discovery-tenant $DISCOVERY_TENANT"
    [[ -n "${PLAYER_ALLOW:-}" ]] && discovery+=" -player-allow $PLAYER_ALLOW"
    [[ -n "${ASSIGN_ADDRESS:-}" ]] && discovery+=" -assign-address $ASSIGN_ADDRESS"
    # Fleet: with cfg/sid/nodes.json Center assigns cameras across the listed
    # control server nodes; cfg/geoip.csv adds the region (country) choice.
    [[ -f "$DATA_DIR/nodes.json" ]] && discovery+=" -nodes-file $DATA_DIR/nodes.json"
    [[ -f "$DATA_DIR/nodes.json" && -f "$APP_DIR/cfg/geoip.csv" ]] && discovery+=" -geoip-file $APP_DIR/cfg/geoip.csv"
    discovery+=" -stats-file $DATA_DIR/stats.json"
    cat > "$UNIT" <<EOF
[Unit]
Description=Matrix signaling server
After=network-online.target
Wants=network-online.target

[Service]
User=$SERVICE_USER
Group=$SERVICE_USER
WorkingDirectory=$APP_DIR
ExecStart=$APP_DIR/signaling-server -root $APP_DIR -port $PORT -https -center \\
    -sid-public-key $DATA_DIR/issuer.pub.pem -min-issued $MIN_ISSUED \\
    -center-private-key $DATA_DIR/center-ticket.key.pem -kid $KID \\
    -main-address wss://$DOMAIN:$PORT/ \\
    -blocklist-file $DATA_DIR/blocklist.txt -tenants-file $DATA_DIR/tenants.json \\
    -activation-store $DATA_DIR/activations.jsonl -admin-token-file $DATA_DIR/admin.token$discovery
Restart=always
RestartSec=2
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$DATA_DIR
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
}

# certbot runs this after every renewal: the server reads one PEM holding the
# chain and the key, and loads it only at start.
write_cert_hook() {
    mkdir -p "$(dirname "$HOOK")"
    cat > "$HOOK" <<EOF
#!/bin/bash
set -euo pipefail
LIVE=/etc/letsencrypt/live/$DOMAIN
TMP=\$(mktemp $APP_DIR/cert.pem.XXXXXX)
cat "\$LIVE/fullchain.pem" "\$LIVE/privkey.pem" > "\$TMP"
chown root:$SERVICE_USER "\$TMP"
chmod 640 "\$TMP"
mv "\$TMP" $APP_DIR/cert.pem
if systemctl is-active --quiet matrix; then systemctl restart matrix; fi
EOF
    chmod 755 "$HOOK"
}

# The TURN file holds credentials, so it is not part of the package; upgrades
# leave APP_DIR/cfg/turn.conf as it is.
install_turn() {
    local file=$1
    [[ -f "$file" ]] || die "missing $file"
    python3 -c 'import json, sys; servers = json.load(open(sys.argv[1])); assert isinstance(servers, list) and all("host" in s and "port" in s for s in servers)' "$file" ||
        die "$file must be a JSON list of TURN servers with host and port"
    install -o root -g "$SERVICE_USER" -m 640 "$file" "$APP_DIR/cfg/turn.conf"
    echo "Installed the TURN configuration ($(python3 -c 'import json, sys; print(len(json.load(open(sys.argv[1]))))' "$file") server(s))"
}

health_check() {
    for _ in $(seq 1 15); do
        if curl -sf --max-time 5 --resolve "$DOMAIN:$PORT:127.0.0.1" "https://$DOMAIN:$PORT/api/health"; then
            echo
            return 0
        fi
        sleep 1
    done
    echo "Health check failed; see: journalctl -u matrix -n 50" >&2
    return 1
}

cmd_install() {
    local keys="" turn=""
    DOMAIN="" PORT=443 KID="matrix-$(date -u +%Y%m%d)" MIN_ISSUED=2026-10-01T00:00:00Z DISCOVERY_TENANT="" PLAYER_ALLOW=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --domain) DOMAIN=$2; shift 2 ;;
            --keys) keys=$2; shift 2 ;;
            --port) PORT=$2; shift 2 ;;
            --kid) KID=$2; shift 2 ;;
            --min-issued) MIN_ISSUED=$2; shift 2 ;;
            --discovery-tenant) DISCOVERY_TENANT=$2; shift 2 ;;
            --turn) turn=$2; shift 2 ;;
            --player-allow) PLAYER_ALLOW=$2; shift 2 ;;
            *) die "unknown option $1" ;;
        esac
    done
    [[ -n "$DOMAIN" && -n "$keys" ]] || die "install needs --domain and --keys"
    [[ -f "$CONF" ]] && die "already installed ($CONF exists); use: setup.sh upgrade"
    for f in issuer.pub.pem center-ticket.key.pem tenants.json; do
        [[ -f "$keys/$f" ]] || die "missing $keys/$f"
    done

    echo "Installing packages..."
    apt-get update -q
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q certbot curl

    id "$SERVICE_USER" >/dev/null 2>&1 ||
        useradd --system --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"

    install_release
    [[ -n "$turn" ]] && install_turn "$turn"

    install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 700 "$APP_DIR/cfg/sid"
    for f in issuer.pub.pem center-ticket.key.pem center-ticket.pub.pem tenants.json blocklist.txt; do
        [[ -f "$keys/$f" ]] && install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 600 "$keys/$f" "$DATA_DIR/$f"
    done
    [[ -f "$DATA_DIR/blocklist.txt" ]] || install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 600 /dev/null "$DATA_DIR/blocklist.txt"
    if [[ ! -f "$DATA_DIR/admin.token" ]]; then
        (umask 077; head -c 48 /dev/urandom | base64 | tr -d '\n=+/' > "$DATA_DIR/admin.token")
        chown "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR/admin.token"
        echo "Generated the admin token in $DATA_DIR/admin.token"
    fi

    install -d -m 755 "$(dirname "$CONF")"
    cat > "$CONF" <<EOF
DOMAIN=$DOMAIN
PORT=$PORT
KID=$KID
MIN_ISSUED=$MIN_ISSUED
DISCOVERY_TENANT=$DISCOVERY_TENANT
PLAYER_ALLOW=$PLAYER_ALLOW
EOF

    echo "Requesting the certificate for $DOMAIN (port 80 must be reachable)..."
    write_cert_hook
    certbot certonly --standalone --non-interactive --agree-tos --register-unsafely-without-email \
        --keep-until-expiring -d "$DOMAIN"
    "$HOOK"
    systemctl enable --now certbot.timer >/dev/null 2>&1 || true

    write_unit
    systemctl enable --now matrix
    health_check
    echo "Installed $(grep '^version=' "$APP_DIR/RELEASE" | cut -d= -f2) at https://$DOMAIN:$PORT/"
}

cmd_upgrade() {
    [[ -f "$CONF" ]] || die "not installed ($CONF missing); use: setup.sh install"
    # shellcheck source=/dev/null
    source "$CONF"
    install_release
    write_unit
    systemctl restart matrix
    if ! health_check; then
        echo "Roll back: copy the newest $APP_DIR/backups/<time>/ files back and run: systemctl restart matrix" >&2
        exit 1
    fi
    echo "Upgraded to $(grep '^version=' "$APP_DIR/RELEASE" | cut -d= -f2) ($(grep '^commit=' "$APP_DIR/RELEASE" | cut -c8-14))"
}

cmd_turn() {
    [[ -f "$CONF" ]] || die "not installed ($CONF missing); use: setup.sh install"
    [[ $# -eq 1 ]] || die "usage: setup.sh turn FILE"
    install_turn "$1"
}

case "${1:-}" in
    install) shift; cmd_install "$@" ;;
    upgrade) shift; cmd_upgrade "$@" ;;
    turn) shift; cmd_turn "$@" ;;
    *) die "usage: setup.sh install --domain DOMAIN --keys DIR [options] | setup.sh upgrade | setup.sh turn FILE" ;;
esac
