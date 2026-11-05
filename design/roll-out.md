# Roll-out of matrix

Updated 2026-11-04. This covers:
- what is released and where it runs
- the code review findings
- what matrix still needs to be run the way vrelay is run (`~/code/vrelay`,
  mostly `deploy/`)
- the plan before matrix carries more than the tester cameras

It also takes in the production strategy review of 2026-10-08 (formerly
`prod_strategy_review.md`, section 3).

Releases are in `~/code/webrtc/release/<version>_<date>/`; for a new droplet
see `server/INSTALL.md` there. Each release is tagged `release_<version>` in
both repositories. The task list and the test results are in the shared
sheet `todo` (Google Drive, matrix docs folder).

## 1. Versions

| Part | Version | Commit | State |
| --- | --- | --- | --- |
| Control server `matrix_cc` | 2.5.0 | `a091483` (tag `release_2.5.0`) | Ran on the Center `matrix.5gen.care` (DO nyc3) and node `matrix-sg00` (DO sgp1) 2026-11-04 to 2026-11-05 |
| Control server `matrix_cc` | 2.6.1 | 2.6.0 `9808bcf` + `4244dfa` | Deployed 2026-11-05 to the Center and sg00. 2.6.0 adds machine load (CPU, memory, disk, network) and the server version to `matrix-fleet status` and `/admin/statistic`; 2.6.1 lists reporting relays in the Center's status |
| Relay stream server `matrix-relay` | 1.2.0 | `9808bcf` (sends its machine load) | Production since 2026-11-04 on `mx-relay-us00` (nyc3, reports to the Center) and `mx-relay-sg00` (sgp1, the former violet droplet, reports to sg00). See [Relay stream servers](#relay-stream-servers-2026-11-04) |
| Camera `matrix` (HCX130) | 2.1.3 published; 2.1.4 in release 2.5.0; **2.1.5** built 2026-11-05 | `libdatachannel_c14` `062c4228`, `11be1993` | 2.1.5 fixes the camera's relay login (R15). Not published: test it on one camera, then run `publish_firmware.sh` |

Camera 2.1.4 is 2.1.3 plus the UDP bind address (`IPv4:PORT` media sources).
That feature was pushed to `origin/main` from another worktree on
2026-10-22 and was missing from local `main`; it was merged on 2026-11-04,
keeping both sides.

### Relay stream servers (2026-11-04)

| Relay | Droplet | Reports to | Transports | Logins |
|---|---|---|---|---|
| `mx-relay-us00.5gen.care` | DO nyc3, 45.55.167.162, 1 vCPU / 1 GB (`vps.sh relay_us`) | Center `matrix.5gen.care`, region US | UDP 3478, TCP 80, TLS 443 (Let's Encrypt, certbot stops the relay a few seconds every 60 days) | per session only |
| `mx-relay-sg00.5gen.care` | DO sgp1, 188.166.244.220, 1 vCPU / 1 GB (`vps.sh relay_sg`) | node `matrix-sg00.5gen.care`, region SG | UDP 3478, TCP 80, TLS 443 (Let's Encrypt since 2026-11-05) | per session only (violet's fixed login was removed on 2026-11-04) |

- violet and Docker are removed from both relay droplets (2026-11-05). The
  control servers' `cfg/turn.conf` (violet, and the AWS dev relay) is removed:
  each hands out only its reporting relay. Checked in sg00's connection log:
  requests got `3.95.228.203` from `turn.conf` until the relay registered at
  09:39:26, then `mx-relay-sg00`.
- The old binaries in `/opt/matrix/backups/` and old packages in
  `~/releases` were removed on 2026-11-05; only 2.5.0 is left. The next
  `setup.sh upgrade` saves a new backup again.
- The SG droplet sits behind a DigitalOcean cloud firewall: UDP 3478 and
  49152–65535, TCP 22, 80 and 443 pass; TCP 3478 and other UDP ports don't.
  `mx-relay-us00` uses ufw with the same ports.
- Each control server has its relay's shared secret in
  `cfg/relay-secrets.json` (2026-11-04). Without it a relay logs
  `403 unknown relay` and is not handed out.
- The AWS dev VPS (`webrtc.5gen.care`, 3.95.228.203) is not part of the
  matrix fleet. It will become the license server (the machine that holds
  the keys). Since 2026-11-05 it runs violet in Docker (`violet-turn`,
  restart `always`, relay ports 49152–65535) for the testers' `webrtc_prod`
  on the same box, whose `cfg/turn.conf` points at it; the dev matrix-relay
  there is removed.
- Tests and measurements: [capacity.md](capacity.md#droplet-measurement-2026-11-04),
  including violet against matrix-relay on the same droplet.

## 2. Code review (2026-11-04)

**What was checked:**
- the paths changed since the last push: SID takeover, Center verify, fleet
  registry, node link, relay reports, statistics, machine load
- `setup.sh` and `deploy.sh`
- the camera's Center client

**Tests run:**
- `go vet` and `go test -race ./...` pass for `src/control` and `src/relay`
- the camera's `config_file_test` passes, and the ARM build succeeds

**Not run:**
- staticcheck and govulncheck (not installed here; govulncheck ran on
  2026-10-14, see `server_security.md`)
- a control-server load test on a droplet (the relay was measured on
  `mx-relay-us00` on 2026-11-04, capacity.md)
- failure tests (Center down, node down, relay down during a stream)

| # | Severity | Finding | What to do | Status |
|---|---|---|---|---|
| R1 | High | **A copied SID can permanently lock out its real camera.** Any key presenting a camera SID takes it over once the holder has been offline on every node for 3 minutes (`center/handler.go` `takeOver`). The old key is then revoked and refused for good. A camera that is off for more than 3 minutes (power cut, ISP outage) while someone holding the SID text verifies loses its licence until an admin releases the SID. The SID text could leak from the pairing system, a batch file or a log. A takeover is only logged and counted (`sid_takeover`, plus an event) | Before going past testers: (a) alert on every `sid_takeover`; (b) at most one takeover per SID per day; (c) let a former holder take the SID back the same way instead of refusing it forever, or (d) restrict takeover to tenants whose pairing system can't release. `no_takeover` per tenant exists | Open, needs a decision |
| R2 | High | **The old AWS node can make takeover unsafe.** The AWS machine (34.228.78.198) still ran the unlinked node service when it was replaced on 2026-10-27. If it accepts the Center's tickets, a camera connected to it is online but invisible to the fleet, so its SID can be taken over while it streams | Stop the service and delete the machine | Open (the user's action) |
| R3 | High | **The Center's state has no off-host backup.** `/opt/matrix/cfg/sid/` (activations, Center ticket key, tenants, admin token, blocklist) and `config.json` with its versions exist only on the nyc3 droplet. Losing it makes every bound SID first-come again and every node needs a new Center key. The copy on this PC (`backups/prod_aws-center-20261027T083625Z`) is from 2026-10-27 | DO droplet backups (weekly, 20% of the droplet price) plus a nightly encrypted copy of `cfg/sid` and `cfg/config*` to off-host storage (DO Spaces); practise a restore (INSTALL.md section 2) | Open |
| R4 | High | **No graceful shutdown.** On SIGTERM `saveOnStop` (`src/history.go`) saves statistics and calls `os.Exit(0)`. Every upgrade drops all cameras and viewers at once; cameras return 25–35 s later, each with two TLS handshakes and a Center verify. Fine at 5,000 cameras a node, but every deploy breaks the live viewing sessions | Stop accepting, close WebSockets spread over about 30 s, then `http.Server.Shutdown`; exponential camera backoff before 50,000 cameras | Open |
| R5 | Medium | **No alerting.** There is `/api/health`, `matrix-fleet status` (machine load since 2.6.0), `/admin/statistic` and the event log, but nothing tells anyone when the Center, a node or a relay is down, a certificate renewal fails, a disk fills, or ALERT, takeover or 401 counts jump. vrelay has `do_server_monitor.py` and `send_email.sh` from cron | A cron monitor on a separate machine: `/api/health` of every server, `matrix-fleet -json status`, certificate expiry, counts from `/admin/statistic/data`; email or chat on change. DO Monitoring alerts on CPU, memory, disk and bandwidth for free | Data done in 2.6.0, alerts open |
| R6 | Medium | **Fleet operations are per host.** `deploy.sh` knows `prod` and `sg` by IP. Nothing deploys to every server, and the relay has no installer | One `servers` list and `deploy.sh all`; a relay installer (INSTALL.md section 3 is the manual version). Since 2.6.0 `matrix-fleet status` shows each node's and relay's version | Version check done in 2.6.0, the rest open |
| R7 | Medium | **Firmware has no staged rollout.** Every camera downloads `/downloads/HCX130/matrix` at boot, so a publish reaches the whole fleet within one reboot cycle | Versioned paths, and a channel per tenant or camera group chosen by Center; at minimum publish to testers first and keep the previous binary for a one-step rollback | Open |
| R8 | Medium | **A takeover's revocation stays on the Center.** The blocklist is not pushed over the link, so on sg00 the old key's current ticket stays valid until it expires (impact low: the old camera was offline) | Push revocations over the link, or copy the blocklist with `setup.sh blocklist` | Open |
| R9 | Low | **Revoke happens before replace.** If `Activations.Replace` fails with an I/O error after `RevokeDevice`, the camera still holding the SID is blocklisted until the revocation expires | Replace first and revoke after, or undo on failure | Open |
| R10 | Low | **Hand-made host setup.** `ufw` was set by hand on each droplet, `setup.sh` does not set it, and journald keeps its default retention (vrelay's `export_log.sh` keeps 30 days of gzip) | `setup.sh install`/`install-node` open 22, 80 and 443 in ufw; set `SystemMaxUse=` for journald | Open |
| R12 | High | **One relay per region, no backup.** A reporting relay is known only to the control server it reports to, so the Center hands out only `mx-relay-us00` and sg00 only `mx-relay-sg00`. The cross-region backup exists only for relays listed in `config.json`. While a region's relay is down, relayed sessions there fail (direct and LAN still work); `cfg/turn.conf` is not used as a fallback once a reporting relay is configured | A second relay per region (N+1), or list the other region's relay in `config.json` with its secret on every node, or let a relay report to several control servers | Open |
| R13 | Medium | **No TLS on the SG relay.** | Done 2026-11-05: `mx-relay-sg00.5gen.care`, certbot with pre/post hooks | Done |
| R14 | Medium | **UDP quality is not verified from a real viewer network.** The office line lets in only about 100–150 UDP packets a second from outside (about 1.2 Mbps), so office tests of relayed UDP show heavy loss that the relays don't cause (their counters show every packet forwarded) | Check relayed UDP from a phone on 4G and from a home line | Open |
| R15 | High | **Cameras up to 2.1.4 can't log in to matrix-relay.** The camera built its relay address as `turn:USER:PASS@host`; the per-session user name `<expiry>:<camera id>` has a `:`, so libdatachannel split it and the relay refused the login (`auth_failures` on the relay, one per session). The camera then offers no relay candidate of its own; the viewer's side can still be relayed, so most sessions still connect. Found by the user on 2026-11-05 (viewing on sg00) | Camera 2.1.5 passes host, port, user and password as fields (`11be1993`). Publish after a one-camera test. The C port (`matrix_c`) already passes fields | Fix built |
| R11 | Low | **`LimitNOFILE=65536`** in `matrix.service`, while a 4 GB node is rated at 62,500 connections (capacity.md) | Raise to 262144 (as vrelay) when 4 GB nodes are used | Open |

Takeover itself worked as designed in the unit tests and in a local
end-to-end run (2026-11-04):
- refused while the holder was online
- 503 while a node was unlinked or the holder had been gone for less than 3
  minutes
- taken over at 3m13s

## 3. Production strategy review of 2026-10-08, revisited

That review read the multi-region proposal against the code of that time; it
ran no load, runtime or chaos tests. Its findings, with today's status:

| # | 2026-10-08 finding | Status 2026-11-04 |
|---|---|---|
| P1 | **Authentication prerequisite.** The licensing design took an Ed25519 signature for 32 bytes (it is 64). Admission needed an authenticated camera identity, and tokens had to bind role and URL | **Resolved.** The SID is 118 characters (24-byte payload, 64-byte signature). The camera proves its device key over the nonce, the SID digest and the assigned node (`center-activate-v2`). Tickets carry role, tenant, camera scope and an audience equal to one node |
| P1 | **Player authorization.** Checking only `request.id` left the answer and other forwarded messages unchecked; every non-`bye` message was forwarded by destination ID | **Resolved.** A viewer can message only the camera of its ticket (anything else answers `Device offline`), a camera's messages reach only viewers whose ticket is for that camera, all within one tenant |
| P1 | **Fallback contract.** One five-minute node-bound token could not authenticate at other nodes or after a 24-hour cache, and a Center outage had no reconnect path | **Changed, partly open.** Every connection gets a fresh 60 s ticket from Center for one node, so nothing is cached. If the Center is down, connected cameras and viewers keep working and nodes keep their configuration, but **no new camera connection or viewing session can start** until the Center is back. Accepted for now. A second Center (shared store) is the fix, and R3 (backup) comes first |
| P1 | **Registry ownership.** Shared storage, atomic epochs, compare-and-set, fencing of a partitioned old node | **Resolved for one Center.** Center is the only registry, fed by the node links. One link per node, with link generations so a replaced link's late messages are ignored. A node whose link is down counts as unhealthy (no assignments, and takeover waits for it). Several Centers would need this again (deferred) |
| P1 | **Legacy rollout behind a Center-only load balancer** | **No longer applies.** There is no load balancer. Old cameras on the legacy service are a separate system (vrelay_parity_gap.md decision A); old matrix cameras (proto 2) are served on their configured server |
| P1 | **Regional recovery capacity.** The emergency capacity in the plan conflicted with the admission cap | **Open, smaller.** If a region is down or full its cameras go to the least loaded node anywhere. If every node is full, Center sends cameras to its own fallback address, which can overload it. Plan N+1 control nodes per region with capacity.md's numbers (5,000 cameras per 1 GB node at 70%), and test a node loss once on droplets (phase 3) |
| P2 | **Relay recovery.** A live stream on a failed relay is not moved; ICE restart and new credentials are needed, and relay choice needs bandwidth and allocation load | **Load done, recovery open.** Relays report allocations and Mbps and are chosen by load. A backup from another region is offered only for relays listed in `config.json`; the reporting relays of 2026-11-04 have none (R12). A stream on a relay that dies still drops: the app and the camera reconnect (new request, new offer). ICE restart is not built |
| — | **Blind spot:** a healthy WebSocket and a successful assignment don't prove playable video | **Partly done.** Every viewing attempt reports connected or failed, the path, the relay protocol and the setup time (the web player and the camera; the mobile app not yet). First-frame time and forced-relay recovery are not measured |

## 4. Parity with vrelay

| vrelay has | matrix has | Gap |
|---|---|---|
| `install.sh` / `update.sh` over SSH, `vrelay.service` | Checksummed package, `setup.sh install / install-node / upgrade` with a backup of the previous binary, `matrix.service` as user `matrix` | At parity or better |
| `sop-new-server.txt` | `release/README.md`, release folder `server/INSTALL.md` | At parity (the relay part is manual, R6) |
| `do_new_magic_server.sh`: doctl create, Route53 record, deploy, database row, server list, reload all | Each step by hand (INSTALL.md sections 0–1) | **Gap.** Script it: doctl create, Route53 UPSERT, `install-node`, `matrix-fleet apply` of the printed entry |
| `all-server.sh`, `all-check.sh` (version per server), `all-update.sh`, `all-reload.sh`, `all-command.sh` | `deploy.sh dev/prod/sg`; `matrix-fleet status` with versions and machine load (2.6.0) | **Gap** for deploy-all (R6); the version check is covered |
| Region routing from MySQL `geoip` and `relay` tables | `config.json` routes and the DB-IP CSV, `matrix-fleet check/apply/rollback` without a restart | Better (no database) |
| Managed MySQL: `server`, `relay`, `connection_log` | Files per server: activations (Center), statistics and the connection log per region, `/admin/statistic` with CSV/JSON | At parity per region; no single place holds every region's log |
| `do_server_monitor.py` (adds a droplet above 90 Mbps, emails), `magic_server_monitor.sh` from cron | Relay load reports; full or silent relays are skipped; machine load in status and statistics (2.6.0) | **Gap** for alerts (R5). Autoscaling not needed yet (one 1 GB relay is about 10,000 cameras, measured 2026-11-04) |
| `grafana-dashboard.json`, `do_server_metrics.py` | `/admin/statistic` charts (including "Machine %"), `/admin/statistic/data` for Grafana | Optional |
| `export_log.sh` (daily journal gzip, 30 days) | journald; events kept 30 days | Small gap (R10) |
| Stress scripts (`tcpkali`) and results | `cmd/loadtest` for control and relay, `tools/capacity/`, capacity.md | Run once on a droplet |
| A relay that carries all video, 15-minute sessions | Video peer to peer; relay only when needed, over UDP, TCP or TLS | Move production from violet to `matrix-relay` (phase 1) |
| `devconn` release package and readme per flavour | Camera binary downloaded at boot, `publish_firmware.sh`, release folder `device/` | **Gap** (R7, staged rollout) |

## 5. Plan

### Phase 0: now, before more testers

1. Stop and delete the old AWS node (R2).
2. Set up the Center backup and restore it once on a scratch droplet (R3).
3. Decide the takeover policy (R1). At least alert on `sid_takeover`.
4. sg00's relay: done 2026-11-04, `mx-relay-sg00` on the former violet
   droplet (waits for its secret on sg00).
5. Deploy 2.6.0 to the Center and sg00 (machine load in status).
6. Push and tag `release_2.5.0`. Done 2026-11-04.

### Phase 1: pilot (testers, up to about 100 cameras, Center plus sg00)

1. Camera 2.1.4:
   - test it on one camera: verify, view over LAN, direct and relay, and an
     RTSP or UDP `IPv4:PORT` source
   - publish it
   - keep 2.1.3 for rollback
2. A production `matrix-relay` per region: `mx-relay-us00` and
   `mx-relay-sg00` in service since 2026-11-04, violet retired. Add a backup
   relay (R12). SG TLS done 2026-11-05.
3. The mobile app sends its connection report (application_todo.md, step 7).
4. Watch for 7 days on `/admin/statistic` and `matrix-fleet status`:
   - verify success
   - `viewer_connected` against `viewer_failed`, and the error codes
   - relayed share by protocol
   - `sid_takeover` and ALERT lines
   - machine load
   - restarts

   **Gate:** no unexplained failure class, and no takeover the pairing team
   can't explain.

### Phase 2: operations at vrelay's level (before 1,000 cameras)

1. Graceful shutdown and camera backoff (R4).
2. Monitoring and alerts (R5).
3. A server list and `deploy.sh all`, a relay installer and a new-node script
   (R6).
4. Firmware channels (R7).
5. ufw in `setup.sh`, journald retention, revocations over the link (R8–R10).

**Gate:**
- a restart of each server type in the pilot without complaints
- an alert seen for a stopped node, a stopped relay and an expiring certificate
- a restore of the Center from backup

### Phase 3: scale (1,000 to 50,000 cameras)

- Add control nodes and relays per region from capacity.md, with N+1 per
  region (section 3, regional recovery).
- Measure the first ones on droplets.
- Drill a node loss and a Center outage.
- Re-check costs against the DO bill.
- Consider ICE restart for relay failover (section 3, P2) and a second Center
  with a shared store.

## 6. Rollback

| Part | How |
|---|---|
| Control server | `/opt/matrix/backups/<time>/` holds the previous binary and player: copy it back as `/opt/matrix/matrix_cc` and run `systemctl restart matrix`, or run `setup.sh upgrade` from the previous package. 2.5.0 and 2.6.0 change no data format; a takeover writes ordinary activation records (2.4.0 reads them); 2.6.0's extra statistics gauges are ignored by older versions |
| Fleet configuration | `sudo matrix-fleet rollback [VERSION]` |
| Takeover only | Per tenant: `"no_takeover": true` in `tenants.json`. For the whole server there is no flag yet: set `TakeoverAfter: -1` in the Center config and rebuild, or roll back to 2.4.0 |
| Relay | A new relay binary: keep the old one, replace `/usr/local/bin/matrix-relay`, `systemctl restart matrix-relay` (sessions on it drop and reconnect). There is no violet to go back to on the relay droplets; in an emergency a control server can hand out fixed relays again from a `cfg/turn.conf` once `cfg/relay-secrets.json` is removed (read per request, no restart) |
| Camera | Publish the previous binary to the download URL; cameras take it at their next boot |
