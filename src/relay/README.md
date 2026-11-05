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
matrix-relay -public-ip 45.55.167.162 -secrets /etc/matrix-relay/secrets.json \
  -cert fullchain.pem -key privkey.pem
```

This listens on UDP 3478, TCP 80 and TLS 443, the ports captive networks
and VPNs pass most often. `-udp`, `-tcp` and `-tls` move them, and an empty
value turns one off; TLS is on only with `-cert` and `-key`. On a host that
already serves 80 and 443 (a control server), use
`-tcp 0.0.0.0:3478 -tls 0.0.0.0:5349`.

In production (2026-11-04): `mx-relay-us00.5gen.care` (UDP, TCP, TLS;
reports to the Center) and `mx-relay-sg00` on 188.166.244.220 (UDP and TCP,
`mx-relay-sg00.5gen.care`, UDP, TCP and TLS since 2026-11-05; reports to sg00). Measurements, including violet against matrix-relay:
[capacity.md](../../design/capacity.md#droplet-measurement-2026-11-04).

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
matrix-relay ... -control https://matrix-sg00.5gen.care -name mx-relay-sg00 -region SG -capacity-mbps 1000
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
- records it in the region's statistics (`-statistic-dir`, below);
- passes the relay's state on to Center over the node link, for
  `matrix-fleet status`.

Relays can still be listed in the fleet configuration instead (`tcp_port`,
`tls_port`, `tls_host`, `metrics`), and `cfg/turn.conf` stays the fallback
when no relay is usable.

## Statistics and the connection log

The control server keeps two things about its region, itself and the relays
reporting to it, both shown at `/admin/statistic` to `-statistic-allow`
addresses only.

**Statistics** (`-statistic-dir`, `cfg/sid/statistic/` from `setup.sh`): one
row per source and step (`-statistic-step`, 30 minutes, or `hour`).

| File | Rows | Kept |
|---|---|---|
| `day/YYYY-MM-DD.jsonl` | one per source and step | 30 days, then moved into its month |
| `month/YYYY-MM.jsonl` | the same rows | 12 months, then summed into days in its year |
| `year/YYYY.jsonl` | one per source and day | always |

A row is `{"t", "step", "source": "control"|"relay", "name", "region",
"minutes", "gauges": {NAME: {"max", "avg"}}, "counts": {NAME: N}}`: gauges are
sampled every 15 s, counts are what happened in the step, `minutes` the
minutes with data (a relay's minutes up). At most about 800 bytes (600
without the machine gauges), so one source takes about 14 MB a year at 30
minutes.

- Relay gauges: `clients` and `clients_udp`, `clients_tcp`, `clients_tls` (the
  clients it serves, by how they reach it), `mbps`. Counts: `rx_bytes`,
  `tx_bytes`, `new_clients`, `auth_failures`, `quota_rejects`, `peers_denied`,
  `reports`.
- Control gauges: `cameras`, `viewers` (connected). Counts: `requests`,
  `camera_offline`, `admission_rejected`, `rate_limited`, and from the reports
  after each viewing attempt `viewer_connected`, `viewer_failed`,
  `viewer_path_lan|direct|relay`, `viewer_relay_udp|tcp|tls`,
  `viewer_error_<code>` (the same with `camera_` for camera reports).
- Machine gauges, of both (control server 2.6.0 and relay 1.2.0 on): `cpu_pct`
  (all cores), `load1`, `mem_pct` (memory in use, total less available),
  `rss_mb` (the server process), `disk_pct`, `net_mbps` (the busier direction
  of every interface but loopback), read from `/proc`. The relay sends them in
  each report as `"machine"`; the page charts them as "Machine %".

`/admin/statistic/data?from=2026-11-01&to=2026-11-03&step=hour&source=relay&name=mx-relay-sg00&format=csv`
returns rows for a charting tool (Grafana with a CSV/JSON data source, Excel,
pandas): `from`/`to` take RFC 3339 or YYYY-MM-DD (default the last 24 h),
`step` 30m, hour or day (rows older than 12 months stay daily), `format` csv
(one column per gauge `_max`/`_avg` and per count) or json.

**Connection log** (`-event-dir`, `cfg/sid/events/`, `-event-days`, default
30): one JSON line per event, one file per UTC day, so a viewing attempt at a
given time can be looked up with its outcome and cause.

| Event | Result | With |
|---|---|---|
| `request` | `forwarded`, `camera_offline` | camera, viewer, relays handed out |
| `report` | `connected`, `failed` | role (camera or viewer), camera, viewer, error (the cause), path, relay_protocol, relay offered/ready, setup_ms |
| `refused` | `no_ticket`, `bad_ticket`, `too_many_connections` | source address, the client ID asked for (at most one a minute per address and reason) |
| `camera` | `online`, `offline` | camera |
| `relay` | `registered`, `restarted`, `stopping`, `silent`, `back` | relay, detail |

About 1 KB per viewing attempt; a day's file is capped at 512 MB.
`/admin/statistic` searches it by time, camera, viewer, event and result;
`/admin/statistic/events?from=2026-11-03T08:00&to=2026-11-03T09:00&camera=ID&result=failed&format=csv`
serves the same search (at most 5000 events, newest first).

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
