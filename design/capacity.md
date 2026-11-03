# Capacity: how many servers for how many cameras

Measured on 2026-11-03, server 2.4.0 (`e746c97`) and matrix-relay 1.1.0. The
reference server is the SG control node **sg00**: DigitalOcean Basic Regular,
1 vCPU (KVM, 2.0 GHz), 1 GB RAM (about 600 MB free with the OS), 25 GB disk,
1 TB of outbound transfer a month.

Re-run with `tools/capacity/` and add a line to [Tracking](#tracking) when the
code, the droplet type or the assumptions change.

## Result

Planned at 70% of what one server can do. **Bold** numbers are estimates until
they are measured on a real droplet (see [Limits of the test](#limits-of-the-test)).

| Cameras | Control nodes, 1 GB (sg00) | or control nodes, 4 GB / 2 vCPU | Relays, 1 GB (sg00) | Peak relayed sessions | Peak relay traffic | Relay egress a month |
|---:|---:|---:|---:|---:|---:|---:|
| 1,000 | 1 | 1 | **1** | 6 | 10 Mbps | 1.1 TB |
| 50,000 | 10 | 2 | **3** | 312 | 0.5 Gbps | 55 TB |
| 100,000 | 20 | 3 | **5** | 625 | 1.0 Gbps | 110 TB |
| 500,000 | 100 | 13 | **25** | 3,125 | 5.1 Gbps | 550 TB |
| 1,000,000 | 200 | 25 | **50** | 6,250 | 10.2 Gbps | 1.1 PB |

Add to every row:

- **The Control Center**: one server (it only issues tickets, activates and
  routes; a camera reaches it once per view), plus a standby.
- **One spare** control node and one spare relay per region (N+1), so a
  server can fail or be upgraded without dropping its cameras.
- **Per region**: every region needs at least one control node and one relay,
  so with R regions the minimum is R of each, whatever the camera count.

The money is in the relay egress, not in the servers: 1.1 GB per camera a
month. DigitalOcean pools the 1 TB included with each droplet; above that,
outbound transfer is billed (about $0.01/GB at the time of writing; check the
current price). At 1M cameras that is about 1.1 PB, minus about 0.3 PB included
with ~275 droplets.

## One server's capacity

| | Limit | Planned (70%) | What limits it |
|---|---:|---:|---|
| Control node, 1 GB (sg00) | 8,268 connections | **5,000 cameras** (+3% viewers at peak) | memory: 56 KB per connection |
| Control node, 2 GB | 26,600 connections | 18,000 cameras | memory |
| Control node, 4 GB / 2 vCPU | 62,500 connections | 40,000 cameras | memory |
| Control node: viewing sessions | 711 per second (sg00) | — | CPU; 1M cameras need about 35 per second at peak, so never the limit |
| Relay, 1 GB (sg00) | **33,000 packets/s** | **137 sessions** (viewer side relayed) or **69** (both sides) | CPU; about 20,000 cameras per relay |
| Relay memory | 23 KB per allocation | — | not a limit (25,000 allocations in 600 MB) |

A camera takes one control connection all day. A viewer takes one while it
watches. Disk is not a limit (see [statistics retention](../src/relay/README.md#statistics-and-the-connection-log)):
the connection log is about 650 bytes per viewing session, 3 MB a day for a
5,000-camera node.

## Assumptions (check them on /admin/statistic)

| | Value | Where to check it |
|---|---|---|
| Viewing | 1 viewer per camera, 15 minutes a day | `requests` and `viewer_connected` per day; the length is not measured yet |
| Peak | 3 × the daily average | `viewers` peak against its average |
| Concurrent viewers at peak | 15/1440 × 3 = **3.1% of cameras** | `viewers` max ÷ `cameras` |
| Sessions through the relay | **20%** (the rest go direct or on the local network) | `viewer_path_relay` ÷ `viewer_connected` |
| Stream | **1.6 Mbps** (170 packets of 1,200 bytes a second) | relay `mbps` ÷ relayed sessions |
| Relayed side | the viewer's (the camera sends to the viewer's relay address) | both-sides sessions cost twice the CPU |

How the table follows from them:

- Control nodes = cameras × (1 + 3.1%) ÷ planned connections per node.
- Peak relayed sessions = cameras × 3.1% × 20%; peak traffic = that × 1.6 Mbps
  in and the same out.
- Relays = peak relayed sessions ÷ 137 (about cameras ÷ 20,000).
- Egress a month = cameras × 20% × 15 min × 1.6 Mbps × 30 days = cameras × 1.1 GB.

Halve the relay numbers if the relay share is 40%, double the egress if the
stream is 3.2 Mbps, and so on: each scales linearly.

## Measurements

The servers were pinned to one core of an Intel i5-6500 (3.2 GHz) with
`GOMAXPROCS=1`, the load on the other three cores. The control server ran as in
production: TLS 1.3 with an ECDSA P-256 certificate (as sg00's), statistics
and connection log on, one log line per connection.

| Test | Result |
|---|---|
| Control, at rest | 11 MB RSS |
| Control, 5,000 idle cameras | 290 MB RSS (55.8 KB each), 1.8% CPU |
| Control, 20,000 idle cameras | 1,124 MB RSS (55.7 KB each), 8.0% CPU (a ping every 30 s each); all connected in 10.3 s |
| Control, viewing sessions | 1,292 per second at 100% CPU: new TLS connection, request, 3 KB offer, 3 KB answer, camera and viewer reports; 4.3 log lines (650 bytes) each |
| Relay, 2,000 allocations held | 46 MB more RSS (23.2 KB each) |
| Relay, 100 sessions, viewer side relayed | 16,745 packets/s, 60% CPU, no loss |
| Relay, 200 sessions | 33,480 packets/s, 55% CPU, no loss |
| Relay, 400 sessions | 66,925 packets/s, 86% CPU, no loss (13 µs a packet) |
| Relay, 100 sessions, both sides relayed | 16,745 packets/s, 61% CPU, 1.9% lost |
| Relay, 200 sessions, both sides relayed | 33,479 packets/s, 69% CPU, 11.5% lost |

sg00 against the test PC, one core each, the same Go benchmark
(`tools/capacity/bench`):

| | Test PC | sg00 | sg00 ÷ PC |
|---|---:|---:|---:|
| ECDSA P-256 sign | 32,630/s | 18,286/s | 0.56 |
| X25519 key exchange | 16,515/s | 8,643/s | 0.52 |
| AES-GCM, 1,200 bytes | 2,595,355/s | 1,549,215/s | 0.60 |
| JSON, 3 KB | 6,084/s | 3,491/s | 0.57 |
| UDP send + receive, 1,200 bytes | 159,954/s | 113,114/s | 0.71 |

CPU numbers were scaled by 0.55 (control) and 0.71 (relay). Memory is the
same binary on both, so it was not scaled. sg00 itself ran only the
benchmark (5 s); no load was put on production.

## Limits of the test

- **Relay CPU on sg00 is an estimate.** On the test PC the network work of
  the loopback ran on the load generator's cores. On a droplet the NIC's
  kernel work also lands on the one vCPU, so the relay estimate takes 60% of
  the scaled figure (33,000 instead of 55,000 packets a second). Measure it
  with a relay on a spare droplet and the load from another droplet.
- **Both-sides relaying lost packets** at 61-69% CPU. That points at socket
  buffers, not CPU (the default `net.core.rmem_max`); raising them is worth a
  test before counting on the 69 sessions.
- **The droplet's network** speed was not measured. 137 sessions are about
  224 Mbps each way.
- **Control memory** (56 KB a connection) is mostly the TLS and WebSocket
  buffers and two goroutines per connection. Pooling the buffers could about
  halve it and double the cameras per node; it is not done.
- The control test had no ticket admission (each check is one Ed25519
  verify, about 20,000 a second on sg00: not a limit).

## Tracking

| Date | Server | Control (sg00) | Relay (sg00) | Notes |
|---|---|---|---|---|
| 2026-11-03 | 2.4.0 / relay 1.1.0 | 56 KB a connection, 5,000 cameras planned; 711 sessions/s | **33,000 pps, 137 sessions, ~20,000 cameras** | first measurement; relay estimated from the test PC |
