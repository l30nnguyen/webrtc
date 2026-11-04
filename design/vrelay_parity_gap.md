# What matrix still lacks compared with vrelay

Originally written 2026-10-19 (in Vietnamese, for non-technical readers).
Translated to English and brought up to date on 2026-11-04 against control
server 2.6.0, camera 2.1.4 and `matrix-relay` 1.2.0. Sources: `SERVICE_SPEC.md`
(the old service) and `~/code/vrelay`.

The release plan, the operations gaps (server lists, monitoring, new-server
scripts) and the code review are in [roll-out.md](roll-out.md). This file
keeps the product decisions and the status of each original item.

## 1. Summary

Matrix is the next generation and works differently from vrelay:
- Video goes directly between camera and phone (WebRTC). It passes through a
  relay stream server only when no direct path works.
- The control server only helps the two ends find each other.

Most vrelay features exist or are no longer needed. The rule for choosing
work: **keep the server as simple as possible; whatever the app or the camera
can do, they do.**

## 2. Decisions

| Topic | Decision (2026-10-19) |
| --- | --- |
| Camera and app | Considered complete, apart from cameras in China. No NAT-type detection: the relay stream server covers it |
| Server | Keep it simple, so it has few bugs and is easy to scale. Push work to the app or camera where possible |
| Running beside vrelay | The two services run side by side, independent, sharing no data (MySQL, Redis, files) and no SIDs |
| A. Old and new SIDs | Two independent systems; no bridge |
| B. Shared "which camera is on which server" | Not shared |
| C. Order | Operations first, then scale |
| D. Cameras in China | Later |

### Target architecture (decided 2026-10-19) and where it stands

**One program, one port** for control. Video relaying is a separate relay
stream server on one or more other machines.

| Part | Target | Status 2026-11-04 |
| --- | --- | --- |
| Main program | One binary, one port | **Done.** `matrix_cc` serves the Center APIs, control connections, the node link, downloads and statistics on 443 |
| Relay stream server | Separate, one or more machines | **Done in code.** `matrix-relay` (UDP, TCP, TLS) reports to its region's node. Production still uses violet |
| SID issuing tool (`sidctl`) | On the administrator's machine, not on the server | **Done.** The issuer private key never goes on a server |

Where "one port" is not met yet:

| Issue | Status |
| --- | --- |
| Certificate renewal uses port 80 | **Open.** `setup.sh` uses certbot standalone on port 80 (renewal by `certbot.timer`). Renewing inside the program on 443 (or DNS validation) would free port 80. Not planned yet |
| TURN settings in a local file | **Done.** Relays come from relay reports or `config.json`, with per-session credentials. `cfg/turn.conf` stays only as a fallback |
| Two copies of the program (443 legacy, 8443 dev) | **Done.** The dev VPS copies are stopped, except the separate `webrtc_prod` under tester evaluation. Production is the Center plus sg00 on DigitalOcean |

## 3. The work list and its status

Who: **Server**, **Camera**, **App** (the user's app team). Size: S, M, L.

### Group 1: operations (first)

| # | Item | Who | Size | Status 2026-11-04 |
|---|---|---|---|---|
| 1 | **Graceful shutdown.** Stop taking new connections and close the old ones spread over a few seconds, so a restart doesn't drop every camera at once | Server | S | **Open** (roll-out R4). SIGTERM saves statistics and exits |
| 2 | **Structured, readable logs** that can be filtered by camera, without secrets | Server | S | **Partly done.** The connection log (`cfg/sid/events/`, JSON lines, 30 days) can be searched by camera, viewer and result. The service log is still plain text in journald |
| 3 | **Health dashboard** (cameras online, refusals, latency) | Server | M | **Done**, without alerts. Covered by `/admin/statistic` (charts, a year of 30-minute rows, CSV/JSON), `matrix-fleet status` (including CPU, memory, disk and network since 2.6.0) and `matrix-fleet stats`. Alerting is open (roll-out R5) |
| 4 | **Secrets in the right place**, not on the command line | Server | S | **Done.** Keys, tokens and relay secrets are files under `cfg/sid/` (mode 0600), given to the server by path |
| 5 | **Certificate renewal inside the program**, to need only one port | Server | M | **Open**, see above |
| 6 | **Smarter camera reconnect**: back off (10, 30, 60, 300 s) with jitter; stop for good if the SID is revoked | Camera | S | **Open.** The camera waits a random 25–35 s every time |
| 7 | **Less memory per connection**: a send queue of about 32 instead of 256, and a memory cap | Server | S | **Open.** The queue is still 256 messages; there is no `MemoryMax`. Measured 56 KB per connection (capacity.md) |

### Group 2: scale

| # | Item | Who | Size | Status 2026-11-04 |
|---|---|---|---|---|
| 8 | **Shared relay list**: several relays from the main program, temporary credentials per session, TLS for phones | Server + App | M | **Done.** Relays per region, chosen by load, with a backup in another region. Credentials last 24 h per session; UDP, TCP and TLS URLs are in the offer |
| 9 | **The app picks the nearest relay** | App | M | **Changed.** The control server picks the relay (the camera's region, by load). The app picks the transport (UDP, then TCP, then TLS; `pickRelayURL()` in `ws.html`) |
| 10 | **Several servers**: cameras can be sent to another server | Server + Camera | L | **Done.** Center assigns each camera a node by country and load, and sends viewers to the camera's node. The fleet configuration is pushed over the node link |
| 11 | **The camera limits viewers**: how many at once, and the longest session | Camera + App | M | **Open**, not verified in the camera code |

### Group 3: later

| # | Item | Status |
|---|---|---|
| 12 | Cameras in China (find the server through a China DNS) | Deferred |
| 13 | Shared storage for several Centers | Deferred. One Center today; its state needs an off-host backup first (roll-out R3) |

Dropped: a bridge between old and new SIDs (decision A).

## 4. vrelay features deliberately not built

| vrelay feature | Why not |
| --- | --- |
| A relay that carries all video (paired relay) | Video goes directly; the relay stream server is used only when needed |
| 15-minute limit and session counting on the server | Left to the camera and app (item 11) |
| Server picks a relay by its own region tables | Replaced by Center routes and relay load (item 9) |
| Pinning test devices for QA | Testers use an app build that points at a test server |
| Home-made traffic obfuscation | WebRTC encryption is standard and stronger |
| NAT-type detection | The relay stream server covers it |
| AIO multiplexed channel and generic TCP port forwarding | The JSON DataChannel is enough; only if the camera's old API must be called |
| Two packaging flavours | Not applicable |
| PROXY protocol | Not needed; `-trust-proxy-tls` exists for running behind a load balancer |

## 5. Running beside vrelay (historical, 2026-10-19)

The parallel-run check was done on the old dev VPS (2 vCPU, 939 MB RAM, no
swap, shared with other projects). The conclusions were:
- give vrelay ports 443 and 80 if both ran there
- don't install MySQL on that machine
- measure each service on its own

Production matrix has since moved to its own DigitalOcean droplets
(2026-10-27), so the port and memory conflicts no longer apply. vrelay itself
runs on its own `vrelay-*` servers.

## 6. Capacity

Superseded by [capacity.md](capacity.md): measurements on 2026-11-03,
servers for 1K–1M cameras, and the DigitalOcean cost per camera. The
2026-10-19 estimate (about 6,000 cameras on the 1 GB dev VPS, limited by
memory) agrees with today's 5,000 cameras per 1 GB control node.

## 7. Glossary

| Term | Meaning |
| --- | --- |
| Control server (signaling) | Helps camera and phone find each other and agree how to connect; carries no video |
| Relay stream server (TURN) | Forwards video when the two ends can't connect directly |
| WebRTC | The technology that connects two devices directly for video |
| SID | The camera's licence code |
| Tenant | A customer or group; viewers of one tenant can't see another's cameras |
| Control Center | The server that checks SIDs, issues tickets and assigns nodes |
| Control node | A control server in a region, linked to the Center |
| Graceful shutdown | Stopping a server without dropping everyone at once |
