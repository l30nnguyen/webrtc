#!/bin/bash
# Install or upgrade the Matrix control server from this release package.
# Run as root from the extracted package folder on Ubuntu. The package binary
# bin/matrix_cc_<version> is installed as /opt/matrix/matrix_cc. One server is
# the Control Center (install); the others are control nodes (install-node).
#
# Fresh Control Center (gets a Let's Encrypt certificate, auto-renewed by certbot.timer):
#   sudo ./setup.sh install --domain matrix.5gen.care --keys /path/to/keys \
#        [--port 443] [--kid NAME] [--min-issued 2026-10-01T00:00:00Z] [--discovery-tenant test01] \
#        [--turn turn.conf] [--player-allow 203.0.113.5,10.0.0.0/8]
#   The keys folder must hold issuer.pub.pem, center-ticket.key.pem and
#   tenants.json; blocklist.txt and center-ticket.pub.pem are copied if present.
#
# Fresh control node (certificate as above; creates the node's link key and
# prints its entry for Center's config.json):
#   sudo ./setup.sh install-node --domain matrix-eu00.5gen.care \
#        --center-key center-ticket.pub.pem --kid KID \
#        --link-to wss://matrix.5gen.care/ \
#        [--name eu00] [--region DE] [--port 443] [--blocklist blocklist.txt] \
#        [--relay-secrets relay-secrets.json] [--turn turn.conf] [--link-ca ca.pem] [--player-allow ...] \
#        [--discovery-tenant test01]   (player at /demo/<tenant>, camera list of that tenant on this node)
#   KID is Center's (KID= in its /etc/matrix/matrix.conf); --link-to is Center's
#   address (or its LINK_LISTEN); --link-server-name is the name on Center's
#   certificate, needed only when --link-to names an IP address.
#
# Replace the blocklist (validated first; the server reads it without a restart;
# copy the same file to every server):
#   sudo ./setup.sh blocklist /path/to/blocklist.txt
#
# Set or replace the TURN servers handed to cameras (JSON list of
# {"host", "port", "username", "password"}; read on every connection, no restart):
#   sudo ./setup.sh turn /path/to/turn.conf
# With relays in config.json, install this server's credentials for those
# relays instead: {"turn-vn00": {"username": "u", "password": "p"}} for violet,
# or {"turn-eu00": "<shared secret>"} for coturn (minted per session; README):
#   sudo ./setup.sh relay-secrets /path/to/relay-secrets.json
#
# Relay stream servers (src/relay, matrix-relay) started with
# -control https://<this server> report to it at /v1/relay/report: it keeps
# them in cfg/sid/relays/ and hands them out by their reported load. Only a
# relay whose -name has a shared secret in relay-secrets.json is accepted.
# Their statistics are at /admin/statistic, for the addresses in
# STATISTIC_ALLOW= of /etc/matrix/matrix.conf (default: PLAYER_ALLOW).
#
# Fleet: sudo matrix-fleet status | stats [DAYS] (installed in /usr/local/bin;
# it talks to the server over /run/matrix/admin.sock, no network port).
# /opt/matrix/cfg/config.json (optional) lists the control server nodes and the
# country routes. Change it only with matrix-fleet, which validates, keeps every
# version in cfg/config.d/ and reloads the server without a restart:
#   sudo matrix-fleet check new.json | apply new.json | rollback [VERSION]
# /opt/matrix/cfg/geoip.csv (DB-IP IP-to-Country Lite CSV) adds the country
# choice. A phase 2 cfg/sid/nodes.json is moved to config.json on upgrade.
# Other nodes link to Center's main port at /v1/link; only each node's own IP
# addresses (link_from in config.json, or what its name resolves to) get past a
# 404. LINK_LISTEN=host:port in /etc/matrix/matrix.conf also serves the links on
# a separate (private) address.
#
# Upgrade an installed Center or node to this package (config, keys and data
# are kept; edit /etc/matrix/matrix.conf first to change a setting):
#   sudo ./setup.sh upgrade
set -euo pipefail

APP_DIR=/opt/matrix
DATA_DIR=$APP_DIR/cfg/sid
FLEET_CONFIG=$APP_DIR/cfg/config.json
FLEET_TOOL=/usr/local/bin/matrix-fleet
CONF=/etc/matrix/matrix.conf
UNIT=/etc/systemd/system/matrix.service
HOOK=/etc/letsencrypt/renewal-hooks/deploy/matrix.sh
SERVICE_USER=matrix
PKG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "setup: $*" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "run as root (sudo)"
BINARY=("$PKG_DIR"/bin/matrix_cc_*)
[[ -x "$BINARY" ]] || die "run from the extracted release package"

# Copies this package's binary, player and version into APP_DIR, keeping the
# previous ones in APP_DIR/backups/<time>/.
install_release() {
    # Older releases installed the binary as signaling-server.
    local old
    for old in "$APP_DIR/matrix_cc" "$APP_DIR/signaling-server"; do
        [[ -f "$old" ]] || continue
        local backup="$APP_DIR/backups/$(date -u +%Y%m%dT%H%M%SZ)"
        mkdir -p "$backup/player" "$backup/cfg"
        cp -p "$old" "$backup/"
        cp -p "$APP_DIR/player/ws.html" "$backup/player/" 2>/dev/null || true
        cp -p "$APP_DIR/cfg/version" "$backup/cfg/" 2>/dev/null || true
        cp -p "$APP_DIR/RELEASE" "$backup/" 2>/dev/null || true
        echo "Backed up the previous release to $backup"
        break
    done
    install -d -m 755 "$APP_DIR" "$APP_DIR/player" "$APP_DIR/cfg" "$APP_DIR/downloads"
    install -m 755 "$BINARY" "$APP_DIR/matrix_cc.new"
    mv "$APP_DIR/matrix_cc.new" "$APP_DIR/matrix_cc"
    install -m 644 "$PKG_DIR/player/ws.html" "$APP_DIR/player/ws.html"
    install -m 644 "$PKG_DIR/cfg/version" "$APP_DIR/cfg/version"
    install -m 644 "$PKG_DIR/RELEASE" "$APP_DIR/RELEASE"
    write_fleet_tool
}

# matrix-fleet runs "matrix_cc fleet" with this Center's paths, address and
# admin socket (status, stats, reload after apply/rollback).
write_fleet_tool() {
    cat > "$FLEET_TOOL" <<'TOOL'
#!/bin/bash
# Fleet of this Center: status | stats [DAYS] | check FILE | apply FILE | rollback [VERSION]
# On a node only status works; configuration is changed on Center.
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "matrix-fleet: run with sudo" >&2; exit 1; }
# shellcheck source=/dev/null
source /etc/matrix/matrix.conf
if [[ "${ROLE:-center}" == node ]]; then
    case "${1:-}" in
        status|keygen|pubkey|blocklist-check|-json) ;;
        *) echo "matrix-fleet: this is a control node; run \"${1:-}\" on the Control Center" >&2; exit 1 ;;
    esac
fi
geoip=()
[[ -f /opt/matrix/cfg/geoip.csv ]] && geoip=(-geoip /opt/matrix/cfg/geoip.csv)
exec /opt/matrix/matrix_cc fleet -config /opt/matrix/cfg/config.json -self "wss://$DOMAIN:$PORT/" \
    -socket /run/matrix/admin.sock "${geoip[@]}" "$@"
TOOL
    chmod 755 "$FLEET_TOOL"
}

# Phase 2 kept the node list in cfg/sid/nodes.json; it is now config.json.
migrate_fleet_config() {
    if [[ -f "$DATA_DIR/nodes.json" && ! -f "$FLEET_CONFIG" ]]; then
        install -m 644 "$DATA_DIR/nodes.json" "$FLEET_CONFIG"
        mv "$DATA_DIR/nodes.json" "$DATA_DIR/nodes.json.moved-to-config.json"
        echo "Moved $DATA_DIR/nodes.json to $FLEET_CONFIG"
    fi
}

# relay_flags: relays may report to this server; their statistics page is
# served to STATISTIC_ALLOW (default PLAYER_ALLOW; nobody when both are empty).
relay_flags() {
    local allow="${STATISTIC_ALLOW:-${PLAYER_ALLOW:-}}"
    printf ' -relay-dir %s' "$DATA_DIR/relays"
    [[ -n "$allow" ]] && printf ' -statistic-allow %s' "$allow"
    return 0
}

write_unit() {
    if [[ "${ROLE:-center}" == node ]]; then
        write_node_unit
        return
    fi
    local discovery=""
    [[ -n "${DISCOVERY_TENANT:-}" ]] && discovery=" -discovery-tenant $DISCOVERY_TENANT"
    [[ -n "${PLAYER_ALLOW:-}" ]] && discovery+=" -player-allow $PLAYER_ALLOW"
    [[ -n "${ASSIGN_ADDRESS:-}" ]] && discovery+=" -assign-address $ASSIGN_ADDRESS"
    # Fleet: with cfg/config.json Center assigns cameras across the listed
    # control server nodes; cfg/geoip.csv adds the region (country) choice.
    [[ -f "$FLEET_CONFIG" ]] && discovery+=" -config-file $FLEET_CONFIG"
    [[ -f "$FLEET_CONFIG" && -f "$APP_DIR/cfg/geoip.csv" ]] && discovery+=" -geoip-file $APP_DIR/cfg/geoip.csv"
    # Nodes link to the main port; LINK_LISTEN (host:port) adds a private one.
    [[ -f "$FLEET_CONFIG" && -n "${LINK_LISTEN:-}" ]] && discovery+=" -link-listen $LINK_LISTEN"
    discovery+=" -stats-file $DATA_DIR/stats.json -admin-socket /run/matrix/admin.sock"
    discovery+="$(relay_flags)"
    cat > "$UNIT" <<EOF
[Unit]
Description=Matrix control server
After=network-online.target
Wants=network-online.target

[Service]
User=$SERVICE_USER
Group=$SERVICE_USER
WorkingDirectory=$APP_DIR
ExecStart=$APP_DIR/matrix_cc -root $APP_DIR -port $PORT -https -center \\
    -sid-public-key $DATA_DIR/issuer.pub.pem -min-issued $MIN_ISSUED \\
    -center-private-key $DATA_DIR/center-ticket.key.pem -kid $KID \\
    -main-address wss://$DOMAIN:$PORT/ \\
    -blocklist-file $DATA_DIR/blocklist.txt -tenants-file $DATA_DIR/tenants.json \\
    -activation-store $DATA_DIR/activations.jsonl -admin-token-file $DATA_DIR/admin.token$discovery
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=2
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$DATA_DIR
RuntimeDirectory=matrix
RuntimeDirectoryMode=0700
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
}

# A control node: ticket admission with Center's public key, linked to Center.
write_node_unit() {
    local extra=""
    [[ -n "${DISCOVERY_TENANT:-}" ]] && extra+=" -discovery-tenant $DISCOVERY_TENANT"
    [[ -n "${PLAYER_ALLOW:-}" ]] && extra+=" -player-allow $PLAYER_ALLOW"
    [[ -f "$DATA_DIR/link-ca.pem" ]] && extra+=" -link-ca $DATA_DIR/link-ca.pem"
    extra+="$(relay_flags)"
    cat > "$UNIT" <<EOF
[Unit]
Description=Matrix control node
After=network-online.target
Wants=network-online.target

[Service]
User=$SERVICE_USER
Group=$SERVICE_USER
WorkingDirectory=$APP_DIR
ExecStart=$APP_DIR/matrix_cc -root $APP_DIR -port $PORT -https \\
    -ticket-public-key $DATA_DIR/center-ticket.pub.pem -ticket-kid $KID -ticket-aud wss://$DOMAIN:$PORT/ \\
    -blocklist-file $DATA_DIR/blocklist.txt -stats-file $DATA_DIR/stats.json \\
    -link-to $LINK_TO -link-server-name $LINK_SERVER_NAME -node-key $DATA_DIR/node.key \\
    -node-config-file $DATA_DIR/node-config.json -admin-socket /run/matrix/admin.sock$extra
Restart=always
RestartSec=2
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$DATA_DIR
RuntimeDirectory=matrix
RuntimeDirectoryMode=0700
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

# The relay secrets file holds this server's shared secrets of the relays in
# config.json; only the service user may read it.
install_relay_secrets() {
    local file=$1
    [[ -f "$file" ]] || die "missing $file"
    python3 -c 'import json, sys; s = json.load(open(sys.argv[1])); assert isinstance(s, dict) and s and all((isinstance(v, str) and len(v) >= 16) or (isinstance(v, dict) and v.get("username") and v.get("password")) for v in s.values())' "$file" ||
        die "$file must map each relay name to a shared secret (16+ characters) or {\"username\", \"password\"}"
    install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 600 "$file" "$APP_DIR/cfg/relay-secrets.json"
    echo "Installed the secrets of $(python3 -c 'import json, sys; print(", ".join(sorted(json.load(open(sys.argv[1])))))' "$file")"
}

# A broken blocklist refuses every connection, so it is checked first and
# replaced atomically; the server notices the new file without a restart.
install_blocklist() {
    local file=$1
    [[ -f "$file" ]] || die "missing $file"
    "$APP_DIR/matrix_cc" fleet blocklist-check "$file" >/dev/null || die "$file is not a valid blocklist; nothing changed"
    install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 600 "$file" "$DATA_DIR/.blocklist.txt.new"
    mv "$DATA_DIR/.blocklist.txt.new" "$DATA_DIR/blocklist.txt"
    echo "Installed the blocklist ($(grep -cv '^[[:space:]]*\(#\|$\)' "$DATA_DIR/blocklist.txt" || true) entries)"
}

# Packages, service user and the certificate, shared by install and install-node.
prepare_host() {
    echo "Installing packages..."
    apt-get update -q
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q certbot curl
    id "$SERVICE_USER" >/dev/null 2>&1 ||
        useradd --system --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
}

request_certificate() {
    echo "Requesting the certificate for $DOMAIN (port 80 must be reachable)..."
    write_cert_hook
    certbot certonly --standalone --non-interactive --agree-tos --register-unsafely-without-email \
        --keep-until-expiring -d "$DOMAIN"
    "$HOOK"
    systemctl enable --now certbot.timer >/dev/null 2>&1 || true
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

    prepare_host
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
ROLE=center
DOMAIN=$DOMAIN
PORT=$PORT
KID=$KID
MIN_ISSUED=$MIN_ISSUED
DISCOVERY_TENANT=$DISCOVERY_TENANT
PLAYER_ALLOW=$PLAYER_ALLOW
EOF

    request_certificate
    ROLE=center
    write_unit
    systemctl enable --now matrix
    health_check
    echo "Installed $(grep '^version=' "$APP_DIR/RELEASE" | cut -d= -f2) at https://$DOMAIN:$PORT/"
}

cmd_install_node() {
    local center_key="" blocklist="" turn="" secrets="" link_ca="" name="" region=""
    DOMAIN="" PORT=443 KID="" LINK_TO="" LINK_SERVER_NAME="" PLAYER_ALLOW="" DISCOVERY_TENANT=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --domain) DOMAIN=$2; shift 2 ;;
            --port) PORT=$2; shift 2 ;;
            --center-key) center_key=$2; shift 2 ;;
            --kid) KID=$2; shift 2 ;;
            --link-to) LINK_TO=$2; shift 2 ;;
            --link-server-name) LINK_SERVER_NAME=$2; shift 2 ;;
            --link-ca) link_ca=$2; shift 2 ;;
            --blocklist) blocklist=$2; shift 2 ;;
            --relay-secrets) secrets=$2; shift 2 ;;
            --turn) turn=$2; shift 2 ;;
            --player-allow) PLAYER_ALLOW=$2; shift 2 ;;
            --discovery-tenant) DISCOVERY_TENANT=$2; shift 2 ;;
            --name) name=$2; shift 2 ;;
            --region) region=$2; shift 2 ;;
            *) die "unknown option $1" ;;
        esac
    done
    [[ -n "$DOMAIN" && -n "$center_key" && -n "$KID" && -n "$LINK_TO" ]] ||
        die "install-node needs --domain, --center-key, --kid and --link-to"
    [[ -f "$CONF" ]] && die "already installed ($CONF exists); use: setup.sh upgrade"
    [[ -f "$center_key" ]] || die "missing $center_key"
    grep -q "BEGIN PUBLIC KEY" "$center_key" || die "$center_key must be Center's public key (center-ticket.pub.pem), never its private key"
    [[ "$LINK_TO" =~ ^wss://[^/]+/$ ]] || die "--link-to must be wss://host[:port]/ (Center's address)"
    local link_host=${LINK_TO#wss://}
    link_host=${link_host%/}
    link_host=${link_host%:*}
    if [[ -z "$LINK_SERVER_NAME" ]]; then
        [[ "$link_host" =~ ^[0-9.]+$|: ]] && die "--link-to names an IP address; add --link-server-name (the name on Center's certificate)"
        LINK_SERVER_NAME=$link_host
    fi

    prepare_host
    install_release
    [[ -n "$turn" ]] && install_turn "$turn"
    [[ -n "$secrets" ]] && install_relay_secrets "$secrets"

    install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 700 "$DATA_DIR"
    install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 600 "$center_key" "$DATA_DIR/center-ticket.pub.pem"
    [[ -n "$link_ca" ]] && install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 600 "$link_ca" "$DATA_DIR/link-ca.pem"
    if [[ -n "$blocklist" ]]; then
        install_blocklist "$blocklist"
    elif [[ ! -f "$DATA_DIR/blocklist.txt" ]]; then
        install -o "$SERVICE_USER" -g "$SERVICE_USER" -m 600 /dev/null "$DATA_DIR/blocklist.txt"
    fi
    if [[ ! -f "$DATA_DIR/node.key" ]]; then
        runuser -u "$SERVICE_USER" -- "$APP_DIR/matrix_cc" fleet keygen "$DATA_DIR/node.key" >/dev/null
    fi
    local public_key
    public_key=$("$APP_DIR/matrix_cc" fleet pubkey "$DATA_DIR/node.key" | sed 's/.*: "\(.*\)"/\1/')

    install -d -m 755 "$(dirname "$CONF")"
    cat > "$CONF" <<EOF
ROLE=node
DOMAIN=$DOMAIN
PORT=$PORT
KID=$KID
LINK_TO=$LINK_TO
LINK_SERVER_NAME=$LINK_SERVER_NAME
PLAYER_ALLOW=$PLAYER_ALLOW
DISCOVERY_TENANT=$DISCOVERY_TENANT
EOF

    request_certificate
    ROLE=node
    write_unit
    systemctl enable --now matrix
    health_check

    local entry="{\"name\": \"${name:-NAME}\", \"address\": \"wss://$DOMAIN:$PORT/\", \"region\": \"${region:-REGION}\", \"public_key\": \"$public_key\"}"
    echo "$entry" > "$APP_DIR/cfg/node-entry.json"
    echo
    echo "Installed control node $(grep '^version=' "$APP_DIR/RELEASE" | cut -d= -f2) at https://$DOMAIN:$PORT/"
    echo "Add this entry to \"nodes\" in config.json on the Control Center, then run: sudo matrix-fleet apply config.json"
    echo "  $entry"
    echo "(also kept in $APP_DIR/cfg/node-entry.json). Until then the log says \"cannot reach Center\"; afterwards"
    echo "\"sudo matrix-fleet status\" on Center shows this node up."
}

cmd_upgrade() {
    [[ -f "$CONF" ]] || die "not installed ($CONF missing); use: setup.sh install"
    # shellcheck source=/dev/null
    source "$CONF"
    install_release
    migrate_fleet_config
    write_unit
    systemctl restart matrix
    rm -f "$APP_DIR/signaling-server"
    if ! health_check; then
        echo "Roll back: copy the newest $APP_DIR/backups/<time>/ files back (the binary as $APP_DIR/matrix_cc) and run: systemctl restart matrix" >&2
        exit 1
    fi
    echo "Upgraded to $(grep '^version=' "$APP_DIR/RELEASE" | cut -d= -f2) ($(grep '^commit=' "$APP_DIR/RELEASE" | cut -c8-14))"
}

cmd_blocklist() {
    [[ -f "$CONF" ]] || die "not installed ($CONF missing)"
    [[ $# -eq 1 ]] || die "usage: setup.sh blocklist FILE"
    install_blocklist "$1"
}

cmd_relay_secrets() {
    [[ -f "$CONF" ]] || die "not installed ($CONF missing); use: setup.sh install"
    [[ $# -eq 1 ]] || die "usage: setup.sh relay-secrets FILE"
    install_relay_secrets "$1"
}

cmd_turn() {
    [[ -f "$CONF" ]] || die "not installed ($CONF missing); use: setup.sh install"
    [[ $# -eq 1 ]] || die "usage: setup.sh turn FILE"
    install_turn "$1"
}

case "${1:-}" in
    install) shift; cmd_install "$@" ;;
    install-node) shift; cmd_install_node "$@" ;;
    upgrade) shift; cmd_upgrade "$@" ;;
    blocklist) shift; cmd_blocklist "$@" ;;
    turn) shift; cmd_turn "$@" ;;
    relay-secrets) shift; cmd_relay_secrets "$@" ;;
    *) die "usage: setup.sh install ... | install-node ... | upgrade | blocklist FILE | turn FILE | relay-secrets FILE (see the top of this file)" ;;
esac
