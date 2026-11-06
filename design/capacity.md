# Capacity: how many servers for how many cameras

Measured on 2026-11-03, server 2.4.0 (`e746c97`) and matrix-relay 1.1.0. The
reference server is the SG control node **sg00**: DigitalOcean Basic Regular,
1 vCPU (KVM, 2.0 GHz), 1 GB RAM (about 600 MB free with the OS), 25 GB disk,
1 TB of outbound transfer a month.

Re-run with `tools/capacity/` and add a line to [Tracking](#tracking) when the
code, the droplet type or the assumptions change. Since server 2.6.0 and
`matrix-relay` 1.2.0, every server records its own CPU, memory, disk and
network (`matrix-fleet status`, `/admin/statistic`), so the estimates below can
be checked against real droplets under real load.

## Result

> **Measured on a droplet, 2026-11-04 ([below](#droplet-measurement-2026-11-04)):**
> a 1 GB relay does about **half** the estimate: about 67 relayed sessions at
> 70% CPU, not 137, so **about 10,000 cameras per relay**. Double the relay
> column below. The cost barely changes: each extra relay costs $6 a month
> but brings 1,000 GiB more transfer into the pool.

Planned at 70% of what one server can do. **Bold** numbers are estimates until
they are measured on a real droplet (see [Limits of the test](#limits-of-the-test)).

| Cameras | Control nodes, 1 GB (sg00) | or control nodes, 4 GB / 2 vCPU | Relays, 1 GB (estimate; measured: double these) | Peak relayed sessions | Peak relay traffic | Relay egress a month |
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
month. See [Cost](#cost).

## Cost

DigitalOcean prices on 2026-11-03 ([droplets](https://www.digitalocean.com/pricing/droplets),
[bandwidth](https://docs.digitalocean.com/platform/billing/bandwidth/)):

| | Price a month | Outbound transfer included |
|---|---:|---:|
| Basic, 1 vCPU / 1 GB (sg00) | $6 | 1,000 GiB |
| Basic, 1 vCPU / 2 GB | $12 | 2,000 GiB |
| Basic, 2 vCPU / 4 GB | $24 | 4,000 GiB |
| Outbound transfer above the included | $0.01 a GiB | the allowance of all droplets in the team is pooled |
| Inbound transfer | free | — |

The price is the same in every region. Each row below counts the table
above plus one spare control node, one spare relay and the Center with a
standby (two 2 GB droplets), in one region. Egress is 1.1 GB (1.02 GiB) per
camera a month; control traffic is a few KB a session and left out.

| Cameras | Droplets | Droplets a month | Transfer included | Transfer above it | Total a month | **Per camera a month** | Per camera a year |
|---:|---|---:|---:|---:|---:|---:|---:|
| 1,000 | 2 control + 2 relays + 2 Center | $48 | 8,000 GiB | $0 | $48 | **$0.048** | $0.58 |
| 50,000 | 11 + 4 + 2 | $114 | 19,000 GiB | $322 | $436 | **$0.0087** | $0.10 |
| 100,000 | 21 + 6 + 2 | $186 | 31,000 GiB | $714 | $900 | **$0.0090** | $0.11 |
| 500,000 | 101 + 26 + 2 | $786 | 131,000 GiB | $3,812 | $4,598 | **$0.0092** | $0.11 |
| 1,000,000 | 201 + 51 + 2 | $1,536 | 256,000 GiB | $7,685 | $9,221 | **$0.0092** | $0.11 |

- **About $0.01 per camera a month** from 50,000 cameras on: $0.0015 of
  droplets and about $0.008 of transfer. At 1,000 cameras the fixed droplets
  dominate ($0.05).
- **1 GB control nodes are cheaper than 4 GB ones** even though they cost
  more per camera in droplets: each brings 1,000 GiB into the pool. With
  4 GB / 2 vCPU control nodes, 1M cameras cost $9,609 ($0.0096 a camera).
- **Each extra region** adds at least a control node, a relay and their
  spares: $24 a month, minus the transfer they bring.
- **The cost follows the assumptions**: twice the relay share (40%), twice
  the viewing (30 minutes a day) or twice the bitrate (3.2 Mbps) doubles the
  egress, so about $0.018 per camera a month. Every GiB above the pool is
  $0.01, so one more relayed camera costs about $0.01 a month at the margin.

## One server's capacity

| | Limit | Planned (70%) | What limits it |
|---|---:|---:|---|
| Control node, 1 GB (sg00) | 8,268 connections | **5,000 cameras** (+3% viewers at peak) | memory: 56 KB per connection |
| Control node, 2 GB | 26,600 connections | 18,000 cameras | memory |
| Control node, 4 GB / 2 vCPU | 62,500 connections | 40,000 cameras | memory |
| Control node: viewing sessions | 711 per second (sg00) | — | CPU; 1M cameras need about 35 per second at peak, so never the limit |
| Relay, 1 GB (sg00) | **33,000 packets/s** | **137 sessions** (viewer side relayed) or **69** (both sides) | CPU; about 20,000 cameras per relay |
| Relay memory | 23 KB per allocation | — | not a limit (25,000 allocations in 600 MB); the unit's `MemoryMax=384M` covers `-max 8000` |
| Relay allocations | 2 per viewing session (camera and viewer), held for the whole session even over LAN or direct | `-max 8000` on both relays since 2026-11-06 (4,000 sessions; the default 1000 was only 500) | `matrix_relay_quota_rejects_total` |

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

## Droplet measurement (2026-11-04)

`mx-relay-us00` (DigitalOcean nyc3, Basic 1 vCPU / 1 GB, matrix-relay 1.2.0,
default socket buffers), loaded from the office PC in Vietnam over the
internet. Steady 10 s windows, CPU from `/proc/stat` (busy includes the
kernel's network work and 6–8% steal):

| Viewer side | Sessions | Packets/s in and out | CPU busy | RSS |
|---|---:|---:|---:|---:|
| UDP | 25 | 4,780 | 36% | 15 MB |
| UDP | 34 | 8,680 | 53% | 15 MB |
| TCP | 50 | 8,330 | 47% | 15 MB |
| TCP | 100 | 17,320 | 75% | 16 MB |

- **UDP: about 16,000 packets a second at 100% CPU, so about 11,500 at
  70%: 67 sessions of 1.6 Mbps.** That's half the 33,000 estimated from the
  test PC. TCP costs less per packet (the kernel batches writes).
- The relay forwarded every packet: its kernel counters showed as many UDP
  packets out as in, and no receive-buffer errors. The office line, though,
  dropped nearly all inbound UDP above about 25 sessions (40 Mbps) and some
  STUN replies, while TCP carried 80 Mbps with 0.04% loss. Long UDP paths
  from Vietnam to the US are a risk for real viewers too: relay Asian
  viewers from an Asian relay.
- The benchmark on this droplet: ECDSA 20,319/s, AES-GCM 1,372,916/s, UDP
  161,330/s (sg00: 18,286, 1,549,215, 113,114).
- **SG relay (`mx-relay-sg00`, the former violet droplet), 2026-11-04:** violet's
  fixed login works on matrix-relay (2 sessions, both sides relayed, run on
  the droplet: 0% lost). From the office, TCP: 5 sessions, 0.01% lost. UDP
  from the office lost 29% at 1 session and 90% at 5, while the relay's
  counters showed every packet forwarded (in = out, no buffer errors): **this
  office line lets in only about 100–150 UDP packets a second from outside**
  (about 1.2 Mbps). Office tests of relayed UDP are therefore not meaningful;
  check from a phone on 4G or a home line (roll-out.md R14).
- Not measured here: raised socket buffers (no buffer errors showed at this
  load).
- **violet against matrix-relay**, same droplet, port, image of production
  violet (`fecd3895336d`) and load, one after the other. Relay process CPU per
  1,000 UDP packets/s forwarded, at about 7,400 packets/s: violet 3.1–3.4%,
  matrix-relay 3.7–4.0%, so violet is about 10–20% cheaper a packet. Total
  busy CPU (with the kernel's work) was 42–47% for violet and 41–44% for
  matrix-relay at similar rates; steal varied 5–18%, so the gap is within the
  noise of a shared vCPU. RSS: violet about 1 MB, matrix-relay about 15 MB
  (neither matters). Both stop at 1,000 allocations by default (`--max`,
  `-max`). Violet serves UDP only. The office path lost most packets and
  some allocations, so neither relay was taken to its limit; a load generator
  in the same region is needed for that.

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

| Date | Server | Control (sg00) | Relay (sg00) | Cost (1M cameras) | Notes |
|---|---|---|---|---|---|
| 2026-11-03 | 2.4.0 / relay 1.1.0 | 56 KB a connection, 5,000 cameras planned; 711 sessions/s | **33,000 pps, 137 sessions, ~20,000 cameras** | $9,221 a month, $0.0092 a camera | first measurement; relay estimated from the test PC; DO prices of 2026-11-03 |
| 2026-11-04 | relay 1.2.0 | (unchanged) | 16,000 pps at 100% (UDP), 67 sessions planned, ~10,000 cameras | about the same (more relays, more pooled transfer) | measured on `mx-relay-us00` (nyc3) from the office; UDP path loss Vietnam → US above ~40 Mbps |
