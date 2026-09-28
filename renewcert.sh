#!/usr/bin/env bash

# Renew the Let's Encrypt certificate used by the Go signaling server and
# atomically publish its certificate+key PEM. Run this from root's crontab.
# PM2 is restarted as the owner of this script directory, not as root:
#
#   0 0 * * * /home/leon/code/webrtc/server/renewcert.sh >> /var/log/webrtc-cert-renewal.log 2>&1
#
# nginx owns port 80 and serves the HTTP-01 webroot. The signaling server
# itself uses TLS on 443/8443, so it remains online during renewal.

set -Eeuo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DOMAIN="${DOMAIN:-webrtc.5gen.care}"
EMAIL="${EMAIL:-admin@${DOMAIN}}"
CERT_DIR="${CERT_DIR:-/etc/letsencrypt/live/${DOMAIN}}"
CERT_PATH="${CERT_PATH:-${SCRIPT_DIR}/cert.pem}"
CERTBOT_WEBROOT="${CERTBOT_WEBROOT:-/var/www/certbot}"
CERTBOT_BIN="${CERTBOT_BIN:-certbot}"
SS_BIN="${SS_BIN:-ss}"
PM2_BIN="${PM2_BIN:-pm2}"
RUNUSER_BIN="${RUNUSER_BIN:-runuser}"

# nginx redirects public traffic to the development listener on 8443, while
# the production listener terminates direct TLS on 443. Reload both by default.
PM2_APPS="${PM2_APPS:-webrtc_prod,webrtc_dev}"
REQUIRE_ROOT="${REQUIRE_ROOT:-1}"
# Preserve the existing deployment's readable PEM mode. Hosts with a service
# group should set CERT_MODE=640 and grant that group access instead.
CERT_MODE="${CERT_MODE:-644}"
# Restart even when cert.pem already matches the certificate source. Use this
# to recover a process whose previous restart failed after publication.
FORCE_RESTART="${FORCE_RESTART:-0}"

# `sudo renewcert.sh` must not create a second, empty root PM2 daemon.  The
# deployment user owns the script directory by default; deployments that keep
# PM2 under another account must set PM2_USER explicitly.
PM2_USER="${PM2_USER:-$(stat -c '%U' "${SCRIPT_DIR}")}"
PM2_USER_HOME="${PM2_USER_HOME:-}"
PM2_HOME="${PM2_HOME:-}"

FULLCHAIN="${CERT_DIR}/fullchain.pem"
PRIVKEY="${CERT_DIR}/privkey.pem"
TMP_CERT=""
RESTART_MARKER="${RESTART_MARKER:-${CERT_PATH}.restart-pending}"

log() {
    printf '%s: %s\n' "$(date '+%Y-%m-%dT%H:%M:%S%z')" "$*"
}

cleanup() {
    if [[ -n "${TMP_CERT}" && -e "${TMP_CERT}" ]]; then
        rm -f -- "${TMP_CERT}"
    fi
}
trap cleanup EXIT

require_root() {
    if [[ "${REQUIRE_ROOT}" == "1" && "${EUID}" -ne 0 ]]; then
        log "ERROR: run as root so certbot can access ${CERT_DIR} and update ${CERT_PATH}"
        exit 1
    fi
}

validate_source_certificate() {
    [[ -r "${FULLCHAIN}" ]] || { log "ERROR: missing ${FULLCHAIN}"; return 1; }
    [[ -r "${PRIVKEY}" ]] || { log "ERROR: missing ${PRIVKEY}"; return 1; }

    openssl x509 -in "${FULLCHAIN}" -noout
    openssl pkey -in "${PRIVKEY}" -noout

    local certificate_public_key private_public_key
    certificate_public_key="$(openssl x509 -in "${FULLCHAIN}" -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum | awk '{print $1}')"
    private_public_key="$(openssl pkey -in "${PRIVKEY}" -pubout -outform DER | sha256sum | awk '{print $1}')"
    [[ "${certificate_public_key}" == "${private_public_key}" ]] || {
        log "ERROR: ${FULLCHAIN} and ${PRIVKEY} are not a key pair"
        return 1
    }
}

obtain_or_renew_certificate() {
    prepare_http01_webroot
    require_http_listener

    # Do not use `certbot renew`: an existing renewal configuration may select
    # a stale standalone authenticator, which cannot bind while nginx owns
    # port 80. An explicit webroot request works for initial, existing, and
    # expired certificates. --keep-until-expiring avoids needless renewal.
    log "Checking ${DOMAIN} through nginx's HTTP-01 webroot"
    "${CERTBOT_BIN}" certonly --webroot --webroot-path "${CERTBOT_WEBROOT}" \
        --non-interactive --agree-tos --email "${EMAIL}" --keep-until-expiring \
        --cert-name "${DOMAIN}" --domain "${DOMAIN}"
}

prepare_http01_webroot() {
    local challenge_dir

    [[ "${CERTBOT_WEBROOT}" == /* ]] || {
        log "ERROR: certbot webroot must be an absolute path: ${CERTBOT_WEBROOT}"
        return 1
    }

    challenge_dir="${CERTBOT_WEBROOT}/.well-known/acme-challenge"
    if [[ -e "${challenge_dir}" && ! -d "${challenge_dir}" ]]; then
        log "ERROR: HTTP-01 challenge path is not a directory: ${challenge_dir}"
        return 1
    fi
    if [[ ! -d "${challenge_dir}" ]]; then
        mkdir -p -m 0755 -- "${challenge_dir}"
    fi
    [[ -d "${challenge_dir}" && -w "${challenge_dir}" ]] || {
        log "ERROR: certbot cannot write HTTP-01 challenges to ${challenge_dir}"
        return 1
    }
    log "Prepared nginx HTTP-01 challenge path ${challenge_dir}"
}

require_http_listener() {
    local listeners

    command -v "${SS_BIN}" >/dev/null 2>&1 || {
        log "ERROR: HTTP listener check command not found: ${SS_BIN}"
        return 1
    }
    listeners="$("${SS_BIN}" -H -ltn 'sport = :80')"
    [[ -n "${listeners}" ]] || {
        log "ERROR: no TCP listener is active on port 80; nginx must serve ${CERTBOT_WEBROOT}/.well-known/acme-challenge/"
        return 1
    }
    log "Verified a local TCP listener is active on port 80 for nginx HTTP-01"
}

publish_certificate_if_changed() {
    validate_source_certificate

    TMP_CERT="$(mktemp "${CERT_PATH}.tmp.XXXXXX")"
    cat "${FULLCHAIN}" "${PRIVKEY}" > "${TMP_CERT}"
    chmod "${CERT_MODE}" "${TMP_CERT}"

    if [[ -f "${CERT_PATH}" ]] && cmp -s "${TMP_CERT}" "${CERT_PATH}"; then
        log "${CERT_PATH} already matches the current Let's Encrypt certificate"
        rm -f -- "${TMP_CERT}"
        TMP_CERT=""
        return 1
    fi

    mv -f -- "${TMP_CERT}" "${CERT_PATH}"
    TMP_CERT=""
    : > "${RESTART_MARKER}"
    log "Published updated certificate to ${CERT_PATH}"
    return 0
}

restart_signaling_servers() {
    local app
    local -a apps
    configure_pm2_environment
    IFS=',' read -r -a apps <<< "${PM2_APPS}"
    for app in "${apps[@]}"; do
        app="${app//[[:space:]]/}"
        [[ -n "${app}" ]] || continue
        log "Restarting PM2 application ${app} as ${PM2_USER} (PM2_HOME=${PM2_HOME})"
        run_pm2 restart "${app}"
    done
}

configure_pm2_environment() {
    if [[ -z "${PM2_USER_HOME}" ]]; then
        PM2_USER_HOME="$(getent passwd "${PM2_USER}" | awk -F: 'NR == 1 { print $6 }')"
    fi
    [[ -n "${PM2_USER_HOME}" && "${PM2_USER_HOME}" == /* ]] || {
        log "ERROR: cannot determine an absolute home directory for PM2_USER=${PM2_USER}; set PM2_USER_HOME explicitly"
        return 1
    }
    PM2_HOME="${PM2_HOME:-${PM2_USER_HOME}/.pm2}"
    [[ "${PM2_HOME}" == /* ]] || {
        log "ERROR: PM2_HOME must be an absolute path: ${PM2_HOME}"
        return 1
    }
}

run_pm2() {
    if [[ "${PM2_USER}" == "$(id -un)" ]]; then
        HOME="${PM2_USER_HOME}" PM2_HOME="${PM2_HOME}" \
            USER="${PM2_USER}" LOGNAME="${PM2_USER}" \
            "${PM2_BIN}" "$@"
        return
    fi

    command -v "${RUNUSER_BIN}" >/dev/null 2>&1 || {
        log "ERROR: user switch command not found: ${RUNUSER_BIN}"
        return 1
    }
    "${RUNUSER_BIN}" -u "${PM2_USER}" -- \
        env "HOME=${PM2_USER_HOME}" "PM2_HOME=${PM2_HOME}" \
        "USER=${PM2_USER}" "LOGNAME=${PM2_USER}" \
        "${PM2_BIN}" "$@"
}

require_root
obtain_or_renew_certificate

if publish_certificate_if_changed; then
    restart_signaling_servers
    rm -f -- "${RESTART_MARKER}"
    log "Certificate renewal and deployment completed"
elif [[ "${FORCE_RESTART}" == "1" ]]; then
    log "Forcing signaling-server restart for the current certificate"
    : > "${RESTART_MARKER}"
    restart_signaling_servers
    rm -f -- "${RESTART_MARKER}"
    log "Forced signaling-server restart completed"
elif [[ -e "${RESTART_MARKER}" ]]; then
    log "Retrying a signaling-server restart left pending by an earlier failed deployment"
    restart_signaling_servers
    rm -f -- "${RESTART_MARKER}"
    log "Pending signaling-server restart completed"
else
    log "No signaling-server restart needed"
fi
