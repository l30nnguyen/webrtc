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

## Control server side

In the fleet configuration, a relay entry names its ports and points the
node monitor at the metrics endpoint (it must be reachable from the nodes of
the relay's region):

```json
{"name": "turn-us00", "region": "US", "host": "webrtc.5gen.care", "port": 3478,
 "tcp_port": 80, "tls_port": 443, "tls_host": "webrtc.5gen.care",
 "capacity_mbps": 1000, "metrics": "http://127.0.0.1:9641/metrics"}
```

Cameras still get only the UDP address. When a camera's offer passes
through, the control server adds `urls` to each relay it names:
`turn:host:port?transport=udp`, `turn:host:tcp_port?transport=tcp`, then
`turns:tls_host:tls_port?transport=tcp`. It leaves out a port that the
node's monitor last found down; the TLS probe checks the certificate.
Viewers use the URLs together, and ICE prefers UDP, then TCP, then TLS.
`cfg/turn.conf` entries take the same `tcp_port`, `tls_port` and `tls_host`
keys, which are kept from cameras.

In `cfg/relay-secrets.json`, `"turn-us00": "<shared secret>"` mints expiring
credentials per session. `{"username", "password"}` keeps fixed credentials.

## Test

`go test -race ./...` runs a camera over UDP and viewers over TCP and TLS
through a real relay, plus credentials, quotas, the peer filter and
certificate reload.
