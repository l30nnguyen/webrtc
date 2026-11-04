# matrix_cc: Control Center and control server

One Go binary for every server role: the Control Center (`-center`), the
control server that cameras and viewers connect to, the node link, the
statistics and the `fleet` admin commands (`matrix-fleet` on the servers).
`cmd/sidctl` issues SIDs; `cmd/loadtest` drives load tests.

The contract (SID format, activation, viewing, credentials, the fleet) is in
[sid_licensing_architecture_v2.md](../../design/sid_licensing_architecture_v2.md);
every endpoint is in [endpoints.md](../../design/endpoints.md).

## Build and test

```bash
go vet ./... && go test -race ./...
go build -o matrix_cc ./src
go build -o sidctl ./cmd/sidctl
```

Release packages (`../../release/make_release.sh`) name the binary
`matrix_cc_<version>`; `setup.sh` installs it as `/opt/matrix/matrix_cc`.

## Run

`./matrix_cc -h` lists every flag. On the servers, `setup.sh` builds the
command line from `/etc/matrix/matrix.conf` (see `../../release/README.md`).
The flags used most:

| Flag | Use |
| --- | --- |
| `-port` (8000), `-root`, `-https` | Listener; with `-https` the certificate is `<root>/cert.pem` |
| `-center`, `-sid-public-key`, `-center-private-key`, `-kid`, `-tenants-file`, `-activation-store`, `-admin-token-file` | Center APIs |
| `-ticket-public-key`, `-ticket-kid`, `-ticket-aud`, `-blocklist-file` | Ticket admission (every server) |
| `-config-file`, `-geoip-file`, `-admin-socket` | Fleet on the Center (`config.json`, `matrix-fleet`) |
| `-link-to`, `-node-key`, `-node-address`, `-node-config-file` | A control node's link to the Center |
| `-relay-dir`, `-statistic-dir`, `-statistic-allow`, `-event-dir`, `-stats-file` | Relay reports, statistics, connection log |
| `-rate-api`, `-rate-connect`, `-rate-web`, `-conns-per-source`, `-rate-exempt` | Limits per source address |
| `-discovery-tenant`, `-player-allow` | The demo player and camera list (demo only) |
| `-demo-tenant` | **Development only:** unauthenticated players |

A local run without TLS, for development:

```bash
go run ./src -root ../.. -port 8000
```

Measured capacity: about 56 KB per connection, 5,000 cameras on a 1 GB node
([capacity.md](../../design/capacity.md)).
