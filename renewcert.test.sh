#!/usr/bin/env bash

# Focused offline checks for renewcert.sh. No network, Certbot, PM2, or root
# access is required; temporary commands model the external lifecycle.
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_DIR="$(mktemp -d "${TMPDIR:-/tmp}/webrtc-renewcert-test.XXXXXX")"

cleanup() {
    rm -rf -- "${TEST_DIR}"
}
trap cleanup EXIT

mkdir -p "${TEST_DIR}/bin" "${TEST_DIR}/live"
openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "${TEST_DIR}/live/privkey.pem" \
    -out "${TEST_DIR}/live/fullchain.pem" \
    -subj '/CN=webrtc.5gen.care' -days 1 >/dev/null 2>&1

cat > "${TEST_DIR}/bin/ss-listening" <<'EOF'
#!/usr/bin/env bash
if [[ " $* " == *' -H '* ]]; then
    printf 'LISTEN 0 511 0.0.0.0:80 0.0.0.0:*\n'
fi
exit 0
EOF

cat > "${TEST_DIR}/bin/ss-empty" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat > "${TEST_DIR}/bin/certbot" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "${HARNESS_DIR}/certbot.args"
case " $* " in
    *' --webroot '*|*' --webroot-path '*) exit 0 ;;
    *) exit 44 ;;
esac
EOF

cat > "${TEST_DIR}/bin/pm2" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${HARNESS_DIR}/pm2.args"
printf 'HOME=%s PM2_HOME=%s USER=%s LOGNAME=%s\n' "${HOME}" "${PM2_HOME}" "${USER}" "${LOGNAME}" >> "${HARNESS_DIR}/pm2.env"
EOF

cat > "${TEST_DIR}/bin/runuser" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${HARNESS_DIR}/runuser.args"
[[ "$1" == "-u" && "$3" == "--" ]] || exit 77
shift 3
exec "$@"
EOF

cat > "${TEST_DIR}/bin/unexpected" <<'EOF'
#!/usr/bin/env bash
exit 99
EOF
chmod +x "${TEST_DIR}/bin/"*

HARNESS_DIR="${TEST_DIR}" \
REQUIRE_ROOT=0 \
CERT_DIR="${TEST_DIR}/live" \
CERT_PATH="${TEST_DIR}/cert.pem" \
CERTBOT_WEBROOT="${TEST_DIR}/webroot" \
CERTBOT_BIN="${TEST_DIR}/bin/certbot" \
SS_BIN="${TEST_DIR}/bin/ss-listening" \
PM2_BIN="${TEST_DIR}/bin/pm2" \
RUNUSER_BIN="${TEST_DIR}/bin/runuser" \
PM2_USER=deploy \
PM2_USER_HOME="${TEST_DIR}/deploy-home" \
PM2_HOME="${TEST_DIR}/deploy-home/.pm2" \
"${SCRIPT_DIR}/renewcert.sh"

[[ -s "${TEST_DIR}/cert.pem" ]]
[[ -d "${TEST_DIR}/webroot/.well-known/acme-challenge" ]]
grep -Fqx 'restart webrtc_prod' "${TEST_DIR}/pm2.args"
grep -Fqx 'restart webrtc_dev' "${TEST_DIR}/pm2.args"
grep -Fqx "HOME=${TEST_DIR}/deploy-home PM2_HOME=${TEST_DIR}/deploy-home/.pm2 USER=deploy LOGNAME=deploy" "${TEST_DIR}/pm2.env"
grep -Fq -- "-u deploy --" "${TEST_DIR}/runuser.args"
grep -Fq -- '--webroot' "${TEST_DIR}/certbot.args"
grep -Fq -- "--webroot-path ${TEST_DIR}/webroot" "${TEST_DIR}/certbot.args"
[[ ! -e "${TEST_DIR}/cert.pem.restart-pending" ]]

# A failed older version may already have published cert.pem without restarting
# its PM2 daemon. FORCE_RESTART repairs that exact state.
rm -f -- "${TEST_DIR}/pm2.args" "${TEST_DIR}/pm2.env" "${TEST_DIR}/runuser.args"
HARNESS_DIR="${TEST_DIR}" \
REQUIRE_ROOT=0 \
CERT_DIR="${TEST_DIR}/live" \
CERT_PATH="${TEST_DIR}/cert.pem" \
CERTBOT_WEBROOT="${TEST_DIR}/webroot" \
CERTBOT_BIN="${TEST_DIR}/bin/certbot" \
SS_BIN="${TEST_DIR}/bin/ss-listening" \
PM2_BIN="${TEST_DIR}/bin/pm2" \
RUNUSER_BIN="${TEST_DIR}/bin/runuser" \
PM2_USER=deploy \
PM2_USER_HOME="${TEST_DIR}/deploy-home" \
PM2_HOME="${TEST_DIR}/deploy-home/.pm2" \
FORCE_RESTART=1 \
"${SCRIPT_DIR}/renewcert.sh"
grep -Fqx 'restart webrtc_prod' "${TEST_DIR}/pm2.args"
grep -Fqx 'restart webrtc_dev' "${TEST_DIR}/pm2.args"
[[ ! -e "${TEST_DIR}/cert.pem.restart-pending" ]]

if HARNESS_DIR="${TEST_DIR}" \
    REQUIRE_ROOT=0 \
    CERTBOT_WEBROOT="${TEST_DIR}/webroot-no-listener" \
    CERTBOT_BIN="${TEST_DIR}/bin/unexpected" \
    SS_BIN="${TEST_DIR}/bin/ss-empty" \
    PM2_BIN="${TEST_DIR}/bin/unexpected" \
    "${SCRIPT_DIR}/renewcert.sh" > "${TEST_DIR}/no-listener.log" 2>&1; then
    printf 'expected HTTP listener preflight to fail\n' >&2
    exit 1
fi
grep -Fq 'no TCP listener is active on port 80' "${TEST_DIR}/no-listener.log"

printf 'renewcert webroot tests passed\n'
