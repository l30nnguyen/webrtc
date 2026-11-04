# Roll-out of matrix 2.5.0

Written 2026-11-04. This covers what to do before matrix carries more than
the tester cameras, and what it still needs to be run the way vrelay is run
(`~/code/vrelay`, mostly `deploy/`). The release itself is in
`~/code/webrtc/release/2.5.0_20261104/` (`server/INSTALL.md` for a new
droplet), and it is tagged `release_2.5.0` in both repositories.

## 1. What is released

| Part | Version | Where it runs now |
|---|---|---|
| Control server (`matrix_cc`, package `matrix-server-2.5.0-a091483`) | 2.5.0, `a091483` | Center `matrix.5gen.care` (DO nyc3) and node `matrix-sg00` (DO sgp1), deployed 2026-11-04 |
| Relay stream server `matrix-relay` | 1.1.0, `a091483` | dev VPS only. Production still uses violet, and sg00's `turn.conf` points at the dev relay for a test |
| Camera `matrix` (HCX130, ARM) | 2.1.4, `libdatachannel_c14` `062c4228` | **not published**. The download URL still serves 2.1.3 |

Camera 2.1.4 is 2.1.3 plus the UDP bind address (`IPv4:PORT` media
sources), which had been pushed to `origin/main` from another worktree on
2026-10-22 and was missing from local `main`. They were merged today (the
merge keeps both sides). Before publishing, test it on one camera with
`publish_firmware.sh`.

## 2. Code review

**What was checked:**
- the paths that changed since the last push: SID takeover, Center verify, the
  fleet registry, the node link, relay reporting, statistics
- `setup.sh` and `deploy.sh`
- the camera's Center client

**Tests run:**
- `go vet` and `go test -race ./...` pass for `src/control` and `src/relay`
- the camera's `config_file_test` passes, and the ARM build succeeds

**Not run:**
- staticcheck and govulncheck (not installed)
- a load test on a DigitalOcean droplet (the numbers in `capacity.md` come
  from a PC)
- failure tests (Center down, node down, relay down while a stream is running)

| # | Severity | Finding | What to do |
|---|---|---|---|
| R1 | High | **A copied SID can permanently lock its real camera out.** Since 2.5.0, any device key that presents a camera SID takes it over once the holder has been offline for 3 minutes on every node (`center/handler.go` `takeOver`). The old key is then revoked and listed as a former holder, so it is refused for good. If a camera is off for more than 3 minutes (a power cut, an ISP outage) while someone holding the SID text verifies, the owner's camera loses its licence until an admin releases the SID. The SID text could leak from the pairing system, the factory batch files or a log. A takeover is only logged and counted (`sid_takeover`, plus an event). Nothing alerts anyone. | Before going past testers: (a) alert on every `sid_takeover`; (b) allow at most one takeover per SID per day; (c) let a former holder take the SID back the same way instead of refusing it forever, or (d) restrict takeover to tenants whose pairing system can't call `/v1/admin/release`. `no_takeover` per tenant already exists for strict tenants. |
| R2 | High | **The old AWS node can make takeover unsafe.** The AWS machine (34.228.78.198) still ran the unlinked node service when it was replaced on 2026-10-27. If it accepts the Center's tickets, a camera still connected to it is online but invisible to the fleet, so its SID can be taken over while it is streaming. | Stop the service and delete the machine (the user planned to). Until then, check that no camera is connected to it. |
| R3 | High | **The Center's state has no off-host backup.** `/opt/matrix/cfg/sid/` exists only on the nyc3 droplet: activations, the Center ticket key, tenants, admin token, blocklist and `config.json` with its versions. `backups/` keeps only old binaries. If the droplet is lost, every bound SID becomes first-come again and every node needs a new Center key. The copy on this PC (`backups/prod_aws-center-20261027T083625Z`) is a week old. | Turn on DO droplet backups for the Center (weekly, 20% of the droplet price). Add a nightly encrypted copy of `cfg/sid` and `cfg/config*` to off-host storage (DO Spaces), and practise a restore once (INSTALL.md section 2). |
| R4 | High | **No graceful shutdown.** On SIGTERM, `saveOnStop` (`src/history.go`) saves statistics and calls `os.Exit(0)`. Every upgrade or restart drops all cameras and viewers at once, and the cameras all come back 25–35 s later, each with two TLS handshakes plus a Center verify. That is fine at 5,000 cameras per node (about 950 connections/s measured), but it is the restart storm `vrelay_parity_gap.md` item 1 describes, and live viewing sessions break on every deploy. | Stop accepting connections, close WebSockets spread over about 30 s with jitter, then `http.Server.Shutdown`. Add exponential camera backoff (parity item 6) before 50,000 cameras. |
| R5 | Medium | **No alerting.** There is `/api/health`, `matrix-fleet status`, `/admin/statistic` and the event log, but nothing tells anyone when the Center, a node or a relay is down, when a certificate renewal fails, when a disk fills, or when ALERT, takeover or 401 counts jump. vrelay has `do_server_monitor.py` with `send_email.sh` from cron. | Run a cron monitor on a separate machine. It should check `/api/health` of every server, `matrix-fleet -json status` (nodes, relays), certificate expiry, and the counts in `/admin/statistic/data`, then email or chat on change. DO Monitoring alerts cover CPU, memory, disk and bandwidth for free. |
| R6 | Medium | **Fleet operations are per host.** `deploy.sh` knows `prod` and `sg` by IP. Nothing deploys to every server, and nothing checks that each runs the same version. `matrix-fleet status` shows each node's *configuration* version but not its binary version. The relay has no installer: the unit and env file are copied by hand. | Keep one `servers` list. Add `deploy.sh all`, have nodes report their binary version over the link so `matrix-fleet status` shows it, and add a `setup.sh`-style installer for the relay (INSTALL.md section 3 is the manual version). |
| R7 | Medium | **Firmware has no staged rollout.** Every camera downloads `/downloads/HCX130/matrix` at boot, so publishing a binary reaches the whole fleet within one reboot cycle, and there is no canary group. | Serve versioned paths and let the Center (or a small `channel` file per tenant) say which version a camera gets. At minimum, publish to the testers' cameras first and keep the previous binary next to it for a one-step rollback. |
| R8 | Medium | **A takeover's revocation stays on the Center.** The Center writes the old key to its own blocklist, but the blocklist is not pushed over the link. On sg00 the old key's current ticket stays valid until it expires. The impact is low (the old camera was offline), but it is inconsistent. | Push revocations to the nodes over the link, or copy the blocklist with `setup.sh blocklist` after a takeover. |
| R9 | Low | **Revocation happens before the activation store update.** If `Activations.Replace` fails with an I/O error after `RevokeDevice` has succeeded, the camera that still holds the SID is blocklisted and refused until the revocation expires. | Replace first and revoke after, or undo the revocation when Replace fails. |
| R10 | Low | **Hand-made host setup.** `ufw` was set by hand on each droplet, `setup.sh` does not set it, and journald keeps its own default retention. vrelay's `export_log.sh` keeps 30 days of gzipped logs. | Make `setup.sh install`/`install-node` open 22, 80 and 443 in ufw, and set `SystemMaxUse=` in journald. The events log already keeps 30 days. |
| R11 | Low | **`LimitNOFILE=65536` in `matrix.service`.** A 4 GB node is rated at 62,500 connections in `capacity.md`, right at that limit (each connection is one file). | Raise it to 262144 (as vrelay) when 4 GB nodes are used. |

Takeover itself (`fleet.Absence`) behaved as designed in the unit tests and
the local end-to-end run on 2026-11-04:
- refused while the holder was online
- 503 while a node was unlinked or the holder had been gone for less than 3
  minutes
- taken over at 3m13s

## 3. Parity with vrelay

| vrelay has | matrix has | Gap / action |
|---|---|---|
| `install.sh` / `update.sh` over SSH, `vrelay.service` | Checksummed package, `setup.sh install / install-node / upgrade`, backup of the previous binary, `matrix.service` running as user `matrix` | At parity or better |
| `sop-new-server.txt` | `release/README.md`, release folder `server/INSTALL.md` | At parity (relay section is manual, see R6) |
| `do_new_magic_server.sh`: doctl create, Route53 record, deploy, database row, server list, reload all | Each step by hand (INSTALL.md section 0–1) | **Gap.** Script it: `doctl` create, `add_dns_record.sh`-style Route53 UPSERT, `install-node`, then `matrix-fleet apply` of the printed entry |
| `all-server.sh`, `all-check.sh` (git version per server), `all-update.sh`, `all-reload.sh`, `all-command.sh` | `deploy.sh dev/prod/sg`, `matrix-fleet status` (node up/down, cameras, config version) | **Gap**, R6 |
| Region routing from MySQL `geoip`/`relay` tables (dual country codes) | `config.json` routes and the DB-IP CSV, changed with `matrix-fleet check/apply/rollback` without a restart | At parity or better (no database) |
| Managed MySQL: `server`, `relay`, `connection_log` | Files on each server: activations (Center), statistics and connection log (30 days) per region, `/admin/statistic` with CSV/JSON export | At parity for one region at a time. No single place holds every region's log: export to one store only if support needs it |
| `do_server_monitor.py` (bandwidth > 90 Mbps adds a droplet, emails), `magic_server_monitor.sh` from cron | Relays report their load; the control server skips full or silent relays | **Gap**, R5. Autoscaling is not needed yet (`capacity.md`: one 1 GB relay is about 20,000 cameras) |
| `grafana-dashboard.json`, `do_server_metrics.py` | `/admin/statistic` page, `/admin/statistic/data` for Grafana (CSV/JSON data source) | Optional: add a Grafana dashboard when there are several regions |
| `export_log.sh` (journal, daily gzip, 30 days) | journald, events 30 days | Small gap, R10 |
| Stress scripts (`tcpkali`), results per region | `cmd/loadtest` for control and relay, `capacity.md` | Run the load test once on a droplet (see `capacity.md` "Limits of the test") |
| Relay carries all video (TCP), 15-minute sessions | Video is peer to peer; relay only when needed, over UDP/TCP/TLS (`matrix-relay`) | Switch production from violet to `matrix-relay` (phase 1) |
| `devconn` release package with readme per flavour | Camera binary downloaded at boot, `publish_firmware.sh` | **Gap**, R7 (no staged rollout) |
| `sshd_config` keep-alive tweaks | none | Not needed |

## 4. Plan

### Phase 0: now, before more testers

1. Stop and delete the old AWS node (R2).
2. Set up the Center backup and restore it once on a scratch droplet (R3).
3. Decide the takeover policy (R1). At the least, alert on `sid_takeover`.
4. Decide sg00's relay: put `turn.conf` back to violet, or move it to a
   production `matrix-relay` (phase 1, step 2). Today it points at the dev VPS.
5. Push and tag `release_2.5.0`. Done 2026-11-04.

### Phase 1: pilot (testers, up to about 100 cameras, Center plus sg00)

1. Camera 2.1.4:
   - test it on one camera: verify, view over LAN, direct and relay, and an
     RTSP or UDP `IPv4:PORT` source
   - publish it with `publish_firmware.sh`
   - keep 2.1.3 for rollback
2. A production `matrix-relay` for each region, `relay-sg01` first (INSTALL.md
   section 3), reporting to its node. Retire violet after a week without
   relay errors.
3. Watch for 7 days on `/admin/statistic`:
   - verify success
   - `viewer_connected` against `viewer_failed`, and the error codes
   - share of relayed sessions by protocol
   - `sid_takeover` and ALERT lines
   - restarts

   **Gate:** no unexplained failure class, and no takeover the pairing team
   can't account for.

### Phase 2: operations at vrelay's level (before 1,000 cameras)

1. Graceful shutdown and camera backoff (R4).
2. Monitoring and alerts (R5).
3. A server list, `deploy.sh all`, node binary versions in `matrix-fleet
   status`, a relay installer and a new-node script (R6, parity table).
4. Firmware channels (R7).
5. ufw in `setup.sh`, journald retention, revocations over the link (R8–R10).

**Gate:**
- a restart of each server type in the pilot without a support complaint
- an alert seen for a stopped node, a stopped relay and an expiring certificate
- a restore of the Center from backup

### Phase 3: scale (1,000 to 50,000 cameras)

- Add control nodes and relays per region from `capacity.md`: 1 GB nodes for
  5,000 cameras each, one relay for about 20,000 cameras.
- Measure the first ones on a droplet before trusting the numbers.
- Run a Center outage drill: cameras that are already connected keep
  streaming, and new verifications retry until the Center is back.
- Re-check costs against the DO bill (egress is most of it).

## 5. Rollback

| Part | How |
|---|---|
| Control server | `/opt/matrix/backups/<time>/` holds the previous binary and player: copy it back as `/opt/matrix/matrix_cc` and run `systemctl restart matrix`, or run `setup.sh upgrade` from the previous package. 2.5.0 adds no data format change. Records written by a takeover are ordinary activation records, which 2.4.0 reads. |
| Fleet configuration | `sudo matrix-fleet rollback [VERSION]` |
| Takeover only | Per tenant: set `"no_takeover": true` in `tenants.json`. For the whole server there is no flag yet: either set `TakeoverAfter: -1` in the Center config and rebuild, or roll back to 2.4.0 |
| Relay | Point `turn.conf` / `relay-secrets.json` back at violet, or replace the binary and restart |
| Camera | Publish the previous binary to the download URL; cameras take it at their next boot |
