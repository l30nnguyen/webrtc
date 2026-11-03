# matrix-relay

The relay stream server (TURN) of the matrix fleet, built on
[pion/turn](https://github.com/pion/turn) v5. It replaces violet; cameras and
the control server need no change.

| | violet | matrix-relay |
|---|---|---|
| Client transports | UDP | UDP, TCP and TLS, each on its own port (default 3478, 80, 443) |
| Credentials | fixed `--credentials` | fixed users, and the control server's expiring ones (`relay.Credentials`, coturn's use-auth-secret) |
| Limits | `--max`, `--quota` | `-max` in all, `-max-per-user` (per camera id) |
| Status | STUN probe only | STUN probe, and `/metrics` with live `turn_total_*` counters |
| Peer filter | none | refuses loopback, private and link-local peers |

## Run

```
matrix-relay -public-ip 3.95.228.203 -secrets /etc/matrix-relay/secrets.json \
  -cert fullchain.pem -key privkey.pem
```

This listens on UDP 3478, TCP 80 and TLS 443, the ports captive networks
and VPNs pass most often. `-udp`, `-tcp` and `-tls` move them, and an empty
value turns one off; TLS is on only with `-cert` and `-key`. On a host that
already serves 80 and 443 (the dev VPS runs a control server there), use
`-tcp 0.0.0.0:3478 -tls 0.0.0.0:5349`.

`secrets.json` must be owner-only (0600). SIGHUP rereads it:

```json
{"shared": "<the secret in the nodes' cfg/relay-secrets.json>",
 "users": {"<name>": "<fixed password>"}}
```

The TLS certificate is reread within a minute of changing on disk.
`deploy/matrix-relay.service` and `deploy/certbot-deploy-hook.sh` install it
under systemd as user `matrix-relay`.

## Reporting to a control server

Without `-control` the relay runs alone and reports nothing. With it, the
relay reports to that control server when it starts, every 15 s, and once
more when it stops:

```
matrix-relay ... -control https://matrix-sg00.5gen.care -name relay-sg01 -region SG -capacity-mbps 1000
```

Each report (`POST /v1/relay/report`) carries the relay's name, region,
addresses (`-host`, default `-public-ip`; the UDP, TCP and TLS ports; the TLS
name, default the certificate's first DNS name), capacity, version, and its
load: allocations by transport, Mbps since the last report, byte and login
counters. It is signed with the relay's shared secret (HMAC-SHA256 of the
time and body, header `X-Relay-Signature`). The control server accepts it only
when `cfg/relay-secrets.json` has a shared secret under that name, the
signature matches, and the time is within a minute.

The control server then:

- keeps the relay in `cfg/sid/relays/relays.json` (`-relay-dir`), so it is
  known again after a restart;
- hands it to cameras with credentials minted from the same secret, and adds
  its TCP and TLS URLs to the camera's offer for the viewer;
- picks relays for new sessions by the reported load, skipping one that has
  been silent for 45 s, is stopping, or whose port the control server can't
  reach (it probes UDP, TCP and TLS every 15 s);
- keeps per-minute statistics for 90 days in `cfg/sid/relays/stats/`, shown at
  `/admin/statistic` (and `?format=json`) to the addresses of
  `-statistic-allow` only;
- passes the relay's state on to Center over the node link, for
  `matrix-fleet status`.

Relays can still be listed in the fleet configuration instead (`tcp_port`,
`tls_port`, `tls_host`, `metrics`), and `cfg/turn.conf` stays the fallback
when no relay is usable.

## Building

The pion sources are not in this repository: `go build` fetches the modules
in `go.mod` (pion/turn v5.1.3 and its dependencies), checks them against
`go.sum` and keeps them in the Go module cache. The binary is static
(`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w"`),
so the relay host needs nothing but the binary.

## Test

`go test -race ./...` runs a camera over UDP and viewers over TCP and TLS
through a real relay, plus credentials, quotas, the peer filter,
certificate reload and the signed report.
