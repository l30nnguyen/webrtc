# Roll-out of matrix

Updated 2026-11-13. This covers:
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
| Control server `matrix_cc` | 2.6.6 | tag `release_2.6.6` | Deployed 2026-11-06 to the Center and sg00. 2.6.0 adds machine load and the server version to `matrix-fleet status` and `/admin/statistic`; 2.6.1 lists reporting relays in the Center's status; 2.6.2/2.6.3: the player moves a session to the next relay URL (TCP, then TLS) when the chosen one gives no relay address within **5 s** (2.6.2 used 3 s, which sent slow UDP sessions to TCP); 2.6.4: the connection log on `/admin/statistic` shows 50 events a page with newer/older links, and Path, Setup and Relays come before Camera and Viewer; 2.6.5: the player sends its answer 1 s after the relay address (6 s at most) instead of waiting for gathering to finish (R18); 2.6.6: the player page is sent with `Cache-Control: no-cache` and carries its own version (`PLAYER_VERSION`, shown in its log and in every report; a test keeps it equal to `cfg/version`), shown as `report (viewer) v2.6.6` on `/admin/statistic` and in the CSV `version` column (R20) |
| Relay stream server `matrix-relay` | 1.2.0 | `9808bcf` (sends its machine load) | Production since 2026-11-04 on `mx-relay-us00` (nyc3, reports to the Center) and `mx-relay-sg00` (sgp1, the former violet droplet, reports to sg00). Since 2026-11-06 both run with `-max 8000` and `MemoryMax=384M` (R19). See [Relay stream servers](#relay-stream-servers-2026-11-04) |
| Camera `matrix` (HCX130) | **2.1.6** published 2026-11-05 (cameras take it at their next boot) | `libdatachannel_c14` tag `release_2.6.3` | 2.1.5 fixes the relay login (R15); 2.1.6 patches libjuice to accept ICE checks with a zero tie-breaker (R16) |

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
| R3 | High | **The Center's state has no off-host backup.** `/opt/matrix/cfg/sid/` (activations, Center ticket key, tenants, admin token, blocklist) and `config.json` with its versions exist only on the nyc3 droplet. Losing it makes every bound SID first-come again and every node needs a new Center key. The copy on this PC (`backups/prod_aws-center-20261027T083625Z`) is from 2026-10-27 | DO droplet backups (weekly, 20% of the droplet price) plus a nightly encrypted copy of `cfg/sid` and `cfg/config*` to off-host storage (DO Spaces); practise a restore (INSTALL.md section 2) | Done 2026-11-13: matrix-backup on every server, pulled nightly by the AWS admin server (matrix-admin), encrypted to an offline key; SID batches stored there; restore drill passed (server_operation 3.6) |
| R4 | High | **No graceful shutdown.** On SIGTERM `saveOnStop` (`src/history.go`) saves statistics and calls `os.Exit(0)`. Every upgrade drops all cameras and viewers at once; cameras return 25–35 s later, each with two TLS handshakes and a Center verify. Fine at 5,000 cameras a node, but every deploy breaks the live viewing sessions | Stop accepting, close WebSockets spread over about 30 s, then `http.Server.Shutdown`; exponential camera backoff before 50,000 cameras | Open |
| R5 | Medium | **No alerting.** There is `/api/health`, `matrix-fleet status` (machine load since 2.6.0), `/admin/statistic` and the event log, but nothing tells anyone when the Center, a node or a relay is down, a certificate renewal fails, a disk fills, or ALERT, takeover or 401 counts jump. vrelay has `do_server_monitor.py` and `send_email.sh` from cron | A cron monitor on a separate machine: `/api/health` of every server, `matrix-fleet -json status`, certificate expiry, counts from `/admin/statistic/data`; email or chat on change. DO Monitoring alerts on CPU, memory, disk and bandwidth for free | Data done in 2.6.0, alerts open |
| R6 | Medium | **Fleet operations are per host.** `deploy.sh` knows `prod` and `sg` by IP. Nothing deploys to every server, and the relay has no installer | One `servers` list and `deploy.sh all`; a relay installer (INSTALL.md section 3 is the manual version). Since 2.6.0 `matrix-fleet status` shows each node's and relay's version | Version check done in 2.6.0, the rest open |
| R7 | Medium | **Firmware has no staged rollout.** Every camera downloads `/downloads/HCX130/matrix` at boot, so a publish reaches the whole fleet within one reboot cycle | Versioned paths, and a channel per tenant or camera group chosen by Center; at minimum publish to testers first and keep the previous binary for a one-step rollback | Open |
| R8 | Medium | **A takeover's revocation stays on the Center.** The blocklist is not pushed over the link, so on sg00 the old key's current ticket stays valid until it expires (impact low: the old camera was offline) | Push revocations over the link, or copy the blocklist with `setup.sh blocklist` | Open |
| R9 | Low | **Revoke happens before replace.** If `Activations.Replace` fails with an I/O error after `RevokeDevice`, the camera still holding the SID is blocklisted until the revocation expires | Replace first and revoke after, or undo on failure | Open |
| R10 | Low | **Hand-made host setup.** `ufw` was set by hand on each droplet, `setup.sh` does not set it, and journald keeps its default retention (vrelay's `export_log.sh` keeps 30 days of gzip) | `setup.sh install`/`install-node` open 22, 80 and 443 in ufw; set `SystemMaxUse=` for journald | Open |
| R12 | High | **One relay per region, no backup.** A reporting relay is known only to the control server it reports to, so the Center hands out only `mx-relay-us00` and sg00 only `mx-relay-sg00`. The cross-region backup exists only for relays listed in `config.json`. While a region's relay is down, relayed sessions there fail (direct and LAN still work); `cfg/turn.conf` is not used as a fallback once a reporting relay is configured | A second relay per region (N+1), or list the other region's relay in `config.json` with its secret on every node, or let a relay report to several control servers | Partly done 2026-11-13: both relays listed in config.json v3 with their secrets on every node, so the other region is the backup. Open: failover test; a second relay per region |
| R13 | Medium | **No TLS on the SG relay.** | Done 2026-11-05: `mx-relay-sg00.5gen.care`, certbot with pre/post hooks | Done |
| R14 | Medium | **UDP quality is not verified from a real viewer network.** The office line lets in only about 100–150 UDP packets a second from outside (about 1.2 Mbps), so office tests of relayed UDP show heavy loss that the relays don't cause (their counters show every packet forwarded) | Check relayed UDP from a phone on 4G and from a home line. 2026-11-06: from the office, viewers played over UDP to the camera's relay address (sg00 log 08:12 and 08:13 UTC); quality not measured | Open |
| R15 | High | **Cameras up to 2.1.4 can't log in to matrix-relay.** The camera built its relay address as `turn:USER:PASS@host`; the per-session user name `<expiry>:<camera id>` has a `:`, so libdatachannel split it and the relay refused the login (`auth_failures` on the relay, one per session). The camera then offers no relay candidate of its own; the viewer's side can still be relayed, so most sessions still connect. Found by the user on 2026-11-05 (viewing on sg00) | Camera 2.1.5 passes host, port, user and password as fields (`11be1993`). Checked 2026-11-05 with the camera's libdatachannel against `mx-relay-sg00`: URL form refused, fields form got a relay candidate. Published 2026-11-05. The C port (`matrix_c`) already passes fields | Fixed, published; on-device check by the user |
| R16 | High | **Cameras up to 2.1.5 reject a TCP/TLS-relayed viewer.** Chrome's ICE checks over a TCP or TLS relay carry `ICE-CONTROLLED=0`. libjuice 1.6.2 keeps the role attributes by value and reads 0 as "absent", so it saw neither attribute and answered every check with 400: the viewer stayed in `checking → disconnected` and no stream came up (found by the user with a viewer on a VPN, 2026-11-05; reproduced with the camera's library and a packet capture on the relay) | Camera 2.1.6: `patches/libjuice-ice-role-presence.patch` records presence separately, applied by `compile.sh`. Checked: 240 s of data both ways, camera relay UDP ↔ Chrome relay TCP. `matrix_c` pins the same libjuice and needs the same patch | Fixed, published |
| R17 | Medium | **A VPN viewer's stream stopped after about 110 s.** Camera 2.1.6, server 2.6.2, viewer on a VPN over a TCP relay: the stream played, then stopped; the servers logged no error (found by the user, 2026-11-05) | Needs a `chrome://webrtc-internals` dump of a failing run, or the same run without the VPN, to tell the VPN from the relay or the camera | Open |
| R18 | High | **The player never answered on a PC with several network adapters.** `sendAnswer` waited for address gathering to complete with no limit. With a Tailscale/VPN adapter, Chrome gathers on every interface; one that drops the relay request keeps gathering open about 40 s, so the answer was never sent and the camera closed the session after 30 s (`no_answer`; viewer `timeout`). The VPN extension hid it by allowing only the relay. Found by the user on 2026-11-06 (incognito, office line, camera v130 on sg00: four `no_answer` in a row, the one TCP-relayed try connected) | Player 2.6.5 (deployed 2026-11-06): the answer goes out 1 s after the relay candidate, 6 s at most, with the candidates gathered so far (`ANSWER_GATHER_MS`). The mobile app must do the same (application_todo) | Fixed in the player. First retest 08:10 UTC still `no_answer` (most likely a tab still running the cached 2.6.4 page, R20); the next tries at 08:12 and 08:13 connected through the camera's relay over UDP. Retest with 2.6.6, where the log line `Player v2.6.6` proves the page |
| R19 | High | **Each relay held only 500 viewing sessions, LAN and direct included.** Both sides of every session take a relay allocation while gathering (camera and viewer, both under the camera's login) and keep it until the session closes, even when the stream goes over LAN or direct. With the default `-max 1000`, a relay stopped at 500 concurrent sessions (about 5% of the 10k cameras a relay is sized for) and later viewers got no relay address. `-max-per-user 16` allows about 8 viewers per camera at once. Idle allocations cost about 23 KB and no CPU (capacity.md) | 2026-11-06: `-max 8000` (4,000 sessions) in `/etc/matrix-relay/env` on both relays, and `MemoryMax=384M` in `matrix-relay.service` (8000 × 23 KB ≈ 190 MB; it was 128M). Watch `matrix_relay_quota_rejects_total` | Done |
| R20 | Medium | **Open tabs kept an old player after a deploy.** `/demo/<tenant>` was served with `Last-Modified` only, so Chrome could reuse the cached page, and nothing showed which player a viewer ran | 2.6.6: `Cache-Control: no-cache`; the player logs `Player v…`, warns when the server's version differs ("reload the page"), and sends `version` in its reports. The camera and the mobile app may send `version` too (up to 32 of `0-9A-Za-z._+-`) | Done |
| R21 | High | **One Center.** Every new camera connection and every new viewing session passes through the single Center (`matrix.5gen.care`, nyc3), which is also the US control node. While it is down or restarting, connected cameras and viewers keep working, but nothing new can start anywhere. The code assumes one Center: activations are a local file and each node links to one Center | Target (2026-11-11, user's decision): a Center in each region (US, Asia on sg00, later EU) that back each other up. Route53 latency routing with a health check per Center sends each device to the nearest healthy one. One activation writer (US); the others forward first-time binds and keep a copy. Every node links to every Center. About $2 a month, no new droplet. A load balancer, Redis, a bigger droplet and active/standby were considered and not chosen (reasons in server_operation.md section 1.4, with the code work, migration and checks) | Planned |
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
| P1 | **Fallback contract.** One five-minute node-bound token could not authenticate at other nodes or after a 24-hour cache, and a Center outage had no reconnect path | **Changed, partly open.** Every connection gets a fresh 60 s ticket from Center for one node, so nothing is cached. If the Center is down, connected cameras and viewers keep working and nodes keep their configuration, but **no new camera connection or viewing session can start** until the Center is back. Accepted for now. A second Center (shared store) is the fix, and R3 (backup) comes first 2026-11-10: planned as R21. 2026-11-11: R21 is now a Center in each region (US, Asia, later EU), with no load balancer and no shared store. |
| P1 | **Registry ownership.** Shared storage, atomic epochs, compare-and-set, fencing of a partitioned old node | **Resolved for one Center.** Center is the only registry, fed by the node links. One link per node, with link generations so a replaced link's late messages are ignored. A node whose link is down counts as unhealthy (no assignments, and takeover waits for it). Several Centers would need this again (deferred) |
| P1 | **Legacy rollout behind a Center-only load balancer** | **No longer applies.** There is no load balancer. Old cameras on the legacy service are a separate system (vrelay_parity_gap.md decision A); old matrix cameras (proto 2) are served on their configured server 2026-11-11: still no load balancer. R21 uses a Center in each region with Route53 latency routing instead. |
| P1 | **Regional recovery capacity.** The emergency capacity in the plan conflicted with the admission cap | **Open, smaller.** If a region is down or full its cameras go to the least loaded node anywhere. If every node is full, Center sends cameras to its own fallback address, which can overload it. Plan N+1 control nodes per region with capacity.md's numbers (5,000 cameras per 1 GB node at 70%), and test a node loss once on droplets (phase 3) |
| P2 | **Relay recovery.** A live stream on a failed relay is not moved; ICE restart and new credentials are needed, and relay choice needs bandwidth and allocation load | **Load done, recovery open.** Relays report allocations and Mbps and are chosen by load. A backup from another region is offered only for relays listed in `config.json`; the reporting relays of 2026-11-04 have none (R12). A stream on a relay that dies still drops: the app and the camera reconnect (new request, new offer). ICE restart is not built |
| — | **Blind spot:** a healthy WebSocket and a successful assignment don't prove playable video | **Partly done.** Every viewing attempt reports connected or failed, the path, the relay protocol and the setup time (the web player and the camera). Decided 2026-11-05: a connection counts as working when the camera's data channel is open and answers a ping; a viewer who then sees no picture points at bandwidth or the video source, measured by the camera from the viewer's RTCP feedback (2.7.0, phase 1 step 4) |

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
| A relay that carries all video, 15-minute sessions | Video peer to peer; relay only when needed, over UDP, TCP or TLS | Done 2026-11-04: production runs `matrix-relay` only |
| `devconn` release package and readme per flavour | Camera binary downloaded at boot, `publish_firmware.sh`, release folder `device/` | **Gap** (R7, staged rollout) |

## 5. Plan

### Phase 0: now, before more testers

1. Stop and delete the old AWS node (R2).
2. Set up the Center backup and restore it once on a scratch droplet (R3).
3. Decide the takeover policy (R1). At least alert on `sid_takeover`.
4. sg00's relay: done 2026-11-04, `mx-relay-sg00` on the former violet
   droplet.
5. Deploy 2.6.x to the Center and sg00: done 2026-11-05 (2.6.4); 2.6.5 and
   2.6.6 on 2026-11-06.
6. Push and tag `release_2.5.0`. Done 2026-11-04.

### Phase 1: pilot (testers, up to about 100 cameras, Center plus sg00)

1. Camera 2.1.6 (2.1.4 plus the relay login fix R15 and the ICE role fix
   R16): published 2026-11-05.
   - seen working 2026-11-06 on sg00: viewers connected through the
     camera's own relay candidate on `mx-relay-sg00` (UDP) and over a TCP
     relay (VPN viewer)
   - still to test on one camera: an RTSP or UDP `IPv4:PORT` source, and a
     long stream over a VPN (R17)
   - keep the previous binary for rollback
2. A production `matrix-relay` per region: `mx-relay-us00` and
   `mx-relay-sg00` in service since 2026-11-04, violet retired. Add a backup
   relay (R12). SG TLS done 2026-11-05. `-max 8000` (4,000 sessions a relay)
   since 2026-11-06 (R19).
3. The player (done in 2.6.5/2.6.6) and the mobile app answer without
   waiting for address gathering to finish (R18), and say their version in
   reports (R20). The app's part is in application_todo.
4. Statistics 2.7.0 (decided 2026-11-05): the control server counts the
   requests; a session succeeds when the **camera** reports its data channel
   open and a ping answered. The camera also reports stream quality from the
   viewer's RTCP feedback (loss, jitter, RTT, the viewer's bandwidth
   estimate). The mobile app adds nothing; country comes from the Center.
5. Watch for 7 days on `/admin/statistic` and `matrix-fleet status`:
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
6. A Center in each region (R21, server_operation.md section 1.4):
   - the R21 code: links to several Centers, writer and reader Centers,
     `matrix-fleet apply` to every Center, DNS-validated certificates;
   - sg00 becomes the Asia Center (reader);
   - then Route53 latency routing with health checks;
   - the EU Center later, with the first EU node.

**Gate:**
- a restart of each server type in the pilot without complaints
- an alert seen for a stopped node, a stopped relay and an expiring certificate
- a restore of the Center from backup
- one Center stopped while cameras connect and viewers start sessions:
  its devices move to the other Center, and only first-time binds wait
  if it was the writer (R21)

### Phase 3: scale (1,000 to 50,000 cameras)

- Add control nodes and relays per region from capacity.md, with N+1 per
  region (section 3, regional recovery).
- Measure the first ones on droplets.
- Drill a node loss and a Center outage.
- Re-check costs against the DO bill.
- Consider ICE restart for relay failover (section 3, P2). A second Center
  is now planned for phase 2 (R21, a Center in each region).
- Add the EU Center with the first EU node (R21, server_operation.md
  section 3.5).

## 6. Rollback

| Part | How |
|---|---|
| Control server | `/opt/matrix/backups/<time>/` holds the previous binary and player: copy it back as `/opt/matrix/matrix_cc` and run `systemctl restart matrix`, or run `setup.sh upgrade` from the previous package. 2.5.0 and 2.6.0 change no data format; a takeover writes ordinary activation records (2.4.0 reads them); 2.6.0's extra statistics gauges are ignored by older versions; 2.6.6's `version` field in the connection log is ignored by older versions |
| Fleet configuration | `sudo matrix-fleet rollback [VERSION]` |
| Takeover only | Per tenant: `"no_takeover": true` in `tenants.json`. For the whole server there is no flag yet: set `TakeoverAfter: -1` in the Center config and rebuild, or roll back to 2.4.0 |
| Relay | Flags and limits are in `/etc/matrix-relay/env` and `matrix-relay.service` (since 2026-11-06 `-max 8000`, `MemoryMax=384M`); remove `-max 8000` to go back to 1000. A new relay binary: keep the old one, replace `/usr/local/bin/matrix-relay`, `systemctl restart matrix-relay` (sessions on it drop and reconnect). There is no violet to go back to on the relay droplets; in an emergency a control server can hand out fixed relays again from a `cfg/turn.conf` once `cfg/relay-secrets.json` is removed (read per request, no restart) |
| Camera | Publish the previous binary to the download URL; cameras take it at their next boot |
