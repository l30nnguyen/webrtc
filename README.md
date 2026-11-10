# Matrix server

The servers of the matrix camera service. User-facing names: the signaling
server is the **control server**, TURN is the **relay stream server**.

| Folder | What |
| --- | --- |
| `src/control` | `matrix_cc` (Go): the Control Center (SID activation, viewer tickets, node assignment, pairing-system admin), the control server (camera and viewer WebSockets), the node link, statistics, `matrix-fleet`; and `sidctl` (SID issuing, on the administrator's machine) |
| `src/relay` | `matrix-relay` (Go, pion/turn): the relay stream server over UDP, TCP and TLS, reporting to its region's control node |
| `player/ws.html` | Web player (demo at `/demo/<tenant>`, office IP only) |
| `release/` | `make_release.sh` (the package), `setup.sh` (install, install-node, upgrade on a droplet), `README.md` (operating guide) |
| `cfg/version` | Server version (2.6.6) |
| `tools/capacity/` | Load tests behind `design/capacity.md` |
| `design/` | Architecture and plans (below) |

## Documents

| Document | Contents |
| --- | --- |
| [design/sid_licensing_architecture_v2.md](design/sid_licensing_architecture_v2.md) | The SID and viewing contract, credentials, the fleet as built |
| [design/endpoints.md](design/endpoints.md) | Every endpoint and message, by caller |
| [design/roll-out.md](design/roll-out.md) | Versions, code review, the 2026-10-08 strategy review, parity with vrelay, plan and rollback |
| [design/capacity.md](design/capacity.md) | Measured limits, servers for 1K–1M cameras, DigitalOcean cost per camera |
| [design/vrelay_parity_gap.md](design/vrelay_parity_gap.md) | Product decisions against vrelay and the status of each item |
| [design/server_operation.md](design/server_operation.md) | Hand-over for operators: servers, logins (where, not what), adding/removing servers with the pre-deployment check, scaling, deploys, monitoring |
| [release/README.md](release/README.md) | Installing and operating a Center, a node, relays and the fleet |
| [src/relay/README.md](src/relay/README.md) | The relay, its reports, the statistics and the connection log |
| [../application_todo.md](../application_todo.md) | What the mobile app implements, with [details and tests](../application_todo_details.md) |

## Build and test

```bash
cd src/control && go vet ./... && go test -race ./...
cd src/relay && go vet ./... && go test -race ./...
release/make_release.sh          # dist/matrix-server-<version>-<commit>.tar.gz
```

`matrix_cc -h` lists every flag. On the servers `setup.sh` writes them into
the systemd unit from `/etc/matrix/matrix.conf`.

## Deploy

```bash
./deploy.sh prod   # Control Center matrix.5gen.care (DO nyc3); asks you to type "prod"
./deploy.sh sg     # control node matrix-sg00.5gen.care (DO sgp1)
./deploy.sh dev    # the AWS dev VPS (PM2 webrtc_dev, 8443; stopped since 2026-10-27; to become the license server)
```

- Each target builds the package and refuses uncommitted changes in
  `src/control`, `player/ws.html`, `cfg/version` or `release/`.
- It uploads the package and checks its checksum before anything changes.
- **prod/sg** run `setup.sh upgrade`, which keeps the configuration, keys and
  data and backs up the previous binary to `/opt/matrix/backups/<time>/`.
- Each target ends with a health check.

New droplets: `release/README.md`, and `server/INSTALL.md` in the release
folder.

`webrtc_prod` on the dev VPS is a separate process under tester evaluation.
It is not a deploy target; don't touch it. The AWS dev VPS (`webrtc.5gen.care`)
is no longer part of the matrix fleet or its relays: it will become the
license server, the machine that holds the keys.

## Legacy

`renewcert.sh` renews the dev VPS certificate (`webrtc.5gen.care`) through
nginx and restarts its PM2 apps. The DigitalOcean servers don't use it: they
renew with certbot and a deploy hook (`setup.sh`). `pm2_start.json` holds the
dev VPS PM2 apps.
