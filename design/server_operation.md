# Server operation

Updated 2026-11-13. This is the hand-over guide for the person who runs the
matrix servers and deploys them. It covers:
- how the servers fit together
- how to add or remove one
- how to check a new server before it takes traffic
- what to watch
- the planned target: a Center in each region (US, Asia, later EU) that
  back each other up (section 1.4; not built yet)

Other docs, all in `server/design/` and the Drive folder `Matrix/v2/design`:

| Doc | What it holds |
|---|---|
| [roll-out.md](roll-out.md) | versions, open risks (R1–R20), plan, rollback |
| [capacity.md](capacity.md) | how many servers for how many cameras, and the measured numbers |
| [endpoints.md](endpoints.md) | every URL the servers answer |

The release folder (`~/code/webrtc/release/<version>_<date>/server/INSTALL.md`)
has the full install commands. `server/release/README.md` is the control
server reference, and `server/src/relay/README.md` is the relay reference.

Terms: the **control server** (`matrix_cc`) handles signalling. The **relay
stream server** (`matrix-relay`) carries video when the camera and the viewer
can't reach each other directly.

---

## 1. High-level architecture

### 1.1 Servers and how they connect (current, 2026-11-10)

Today there is **one** Center. While the Center is down
or restarting, cameras and viewers that are already connected keep working.
No new camera connection and no new viewing session can start anywhere until
it is back. Section 1.4 has the plan to fix this (R21).

```
                        VIEWERS (mobile app, ws.html player)
                                    │
                1. ask for a view   │ HTTPS 443  /v1/view
                                    v
 CAMERAS ──── WSS 443 ────> ┌───────────────────────────────┐
 (ask which node,           │ CONTROL CENTER                │
  then stay connected)      │ matrix.5gen.care  (DO nyc3)   │
                            │ - SID tickets, activation     │
                            │ - routes country -> region    │
                            │ - also node "center" (US)     │
                            │ - pushes config.json to nodes │
                            └──────────────┬────────────────┘
                                           │ node link: WSS 443 /v1/link
                                           │ (signed, node IP allow-listed,
                                           │  node opens it, Center checks it)
                            ┌──────────────┴────────────────┐
                            │ CONTROL NODE  sg00            │
                            │ matrix-sg00.5gen.care (sgp1)  │
                            │ - Asian cameras (region SG)   │
                            └───────────────────────────────┘

 RELAY STREAM SERVERS: each one reports to the control server of its region
 every 15 s over HTTPS (POST /v1/relay/report, signed with a shared secret)

   mx-relay-us00 (nyc3) ──report──> Center  ──hands it out to US sessions
   mx-relay-sg00 (sgp1) ──report──> sg00    ──hands it out to SG sessions

 VIDEO (after the control server has set up the session):
   camera <──── LAN / direct (peer to peer) ────> viewer
   camera <──UDP──> relay of the region <──UDP / TCP 80 / TLS 443──> viewer
```

- **One Center** (`matrix.5gen.care`). It is the only server that knows the
  fleet configuration (`/opt/matrix/cfg/config.json`: the nodes and the
  country routes). It is also a normal control node for region US.
- **Control nodes**: each one opens a link to the Center and keeps it open.
  The Center pushes each new configuration to every node. A node keeps
  serving its cameras if the Center goes down.
- **Relays** report to their own region's control server (us00 to the
  Center, sg00 to sg00). Since 2026-11-13 (R12, config.json version 3) both
  relays are also listed in `config.json`, and every control server holds
  every relay's shared secret in `/opt/matrix/cfg/relay-secrets.json`. Each
  viewing session gets its region's relay plus the other region's as a
  backup; a relay that is down is skipped. A new relay must be added to
  `config.json` and its secret to every control server.
- **Viewing path**: the viewer asks the Center, the Center sends it to the
  node the camera is on, and that node offers the camera's relay. Each
  viewing session holds 2 relay allocations (one for the camera, one for the
  viewer) for the whole session, even when the video goes over LAN or
  direct.

### 1.2 Adding and removing a server

```
 ADD A CONTROL NODE                            REMOVE A CONTROL NODE
 ──────────────────                            ─────────────────────
 droplet + DNS + firewall                      Center: matrix-fleet apply
        │                                        (config without the node)
        v                                             │  link drops; new
 setup.sh install-node  ──prints──> node entry        │  cameras go elsewhere
        │                                             v
        v                                      node: systemctl stop matrix
 * VERIFY (section 3.2) ── fail? fix, don't add       │  its cameras reconnect
        │ pass                                        │  through the Center
        v                                             v
 Center: matrix-fleet check + apply            fleet status: 0 cameras on it
   (adds the node to config.json)                     │
        │                                             v
        v                                      destroy droplet, DNS record,
 node links within seconds,                    deploy.sh target
 takes NEW cameras of its region

 ADD A RELAY                                   REMOVE A RELAY
 ───────────                                   ──────────────
 droplet + DNS + firewall + cert               control server: remove its
        │                                        name from relay-secrets.json
        v                                        (no new sessions get it)
 install matrix-relay, env, service                   │
        │ (reports, but gets 403 until step 3)        v
        v                                      wait: allocations = 0
 * VERIFY (section 3.3) ── fail? fix, don't add       │  (or sessions drop and
        │ pass                                        │   reconnect elsewhere)
        v                                             v
 control server: add its secret to             systemctl disable --now
   relay-secrets.json (read per request)         matrix-relay, destroy droplet
        │
        v
 handed out to new sessions by load
```

Removing a node or a relay never moves sessions that are already running.
Running sessions stay where they are until they end. A stopped server drops
its sessions, and the camera and the viewer then reconnect. There is no
graceful drain yet (R4), so remove servers at a quiet hour.

### 1.3 When to scale up or down

```
                 ┌───────────────────────────────────────────┐
                 │ every day: matrix-fleet status on Center  │
                 │ + /admin/statistic (per region, 7 days)   │
                 └─────────────────────┬─────────────────────┘
                                       v
         ┌──────────────── any SCALE-UP trigger, sustained 3 days ───────────┐
         │  control node: cameras > 3,500, or memory > 70%,                  │
         │                or CPU > 70% at peak                               │
         │  relay:        CPU > 60%, or Mbps > 60% of capacity,              │
         │                or allocations > 6,000 (of -max 8000),             │
         │                or ANY quota_rejects                               │
         └───────┬───────────────────────────────────────────────┬───────────┘
                 │ yes                                           │ no
                 v                                               v
     add one server of that type in      ┌── every SCALE-DOWN condition, 14 days ───┐
     that region (section 3)             │  region has more than 2 of that type,    │
     (or resize the droplet: one         │  and with one server fewer the rest      │
      reboot, at a quiet hour)           │  would stay under 40%                    │
                 │                       └───────┬─────────────────────────┬────────┘
                 v                               │ yes                     │ no
     check the load spreads (1–2 days):          v                         v
     new CAMERAS go to the least loaded   remove one (section 1.2),     do nothing
     node, but cameras already connected  at a quiet hour
     stay where they are until they
     reconnect
```

Rules:
- Each region keeps at least one control node and one relay. From 1,000
  cameras on, keep one spare of each (N+1).
- Relays can be added before a node is full: they are cheap ($6) and each one
  brings 1,000 GiB of monthly transfer into the pool.
- A new **region**, for example Europe: add a control node and a relay
  there, and add a route for its countries in `config.json`.

The triggers above are starting values. They come from
[capacity.md](capacity.md): a 1 GB control node holds about 5,000 cameras
at 70% load, and a 1 GB relay about 67 relayed sessions (about 10,000
cameras) at 70% CPU. Adjust them once real traffic shows the true numbers,
and add a line to capacity.md's tracking table.

### 1.4 Target: a Center in each region (planned, R21)

**Status 2026-11-11: planned, not built.** Everything above section 1.4
describes the fleet as it runs today. The code changes listed below are
needed before a second Center can run.

**Why.** The Center is the one server every new connection and every new
viewing session passes through. The goal is 99.9% successful views from any
viewer network, at the lowest cost. For that, the Center must survive the
loss of one droplet, an upgrade, and even a whole region, without stopping
new connections. The money goes to control nodes and relays, which decide
how many devices the fleet serves. The Center tier only has to be
available, and its work (tickets, routing, activation) is light.

**Design.** Up to 3 Centers that back each other up: US, Asia, later EU.
- **All of them serve:** tickets, node choice, `/v1/view` and statistics.
  They hold the same keys, the same `config.json` and a link from every
  node, so each one answers a device directly. A Center never sends a device
  to another Center.
- **One activation writer, the US Center.** The others forward first-time
  SID binds (and takeovers) to it, server to server, and keep a copy of
  `activations.jsonl` and the blocklist. That way no SID is ever bound twice.
- **The Center role runs on an existing node** of its region, as it does in
  the US today. So it needs no extra droplet.
- **Route53 sends each device to the nearest healthy Center**, using latency
  routing with a health check per Center. If a Center is down, its devices
  go to the next nearest one.
- **Start with 2 Centers** (US, and Asia on sg00), both of which exist today.
  Add EU when the first EU node is needed. Adding a Center needs no new code
  (section 3.5).

```
 CAMERAS, VIEWERS, PAIRING SYSTEM   (still https://matrix.5gen.care: nothing changes on them)
                                    │
         Route53 latency routing + a health check per Center:
         each device gets the NEAREST HEALTHY Center
            ┌───────────────────────┼───────────────────────┐
            v                       v                       v
 ┌─────────────────────┐ ┌─────────────────────┐ ┌─────────────────────┐
 │ Center US (nyc3)    │ │ Center ASIA (sgp1)  │ │ Center EU (fra1)    │
 │ center-us.5gen.care │ │ center-sg.5gen.care │ │ center-eu.5gen.care │
 │ + node "center"     │ │ on node sg00        │ │ later, on node eu00 │
 │ ACTIVATION WRITER   │ │ reader              │ │ reader              │
 └─────────────────────┘ └─────────────────────┘ └─────────────────────┘

 Between Centers, server to server (country blocking does not apply here):
   Center ASIA, Center EU  ── first-time SID binds ──────────────>  Center US
   Center US               ── activations.jsonl, blocklist ──────>  the others
   matrix-fleet apply      ── config.json ───────────────────────>  every Center

 CONTROL NODES link to EVERY Center by its own name (today's signed link, one per Center):
   node sg00, node eu00, node usNN ... ──link──> Center US, Center ASIA, Center EU

 RELAYS: unchanged. Each one reports to its region's control server.
```

| | Today | Target |
|---|---|---|
| Centers | 1 (nyc3), also the US node | 2 now (US; Asia on sg00), 3 later (EU). Each one is also a node of its region |
| How a device finds a Center | `matrix.5gen.care` resolves to one IP | `matrix.5gen.care` resolves to the nearest healthy Center |
| Activations | one file on the Center | one writer (US); the others forward first-time binds and keep a copy |
| Node link | to `matrix.5gen.care` | to every Center, by its own name |
| One Center lost | no new connection or view until it is back | its devices go to the next nearest Center within about 1–2 minutes. If it was the writer, only first-time binds wait |
| A whole region lost | as above | covered: the other Centers serve |
| Center upgrade | all its connections drop for a few seconds | one Center at a time |
| Start of a view from Asia | about 4 round trips to the US (about 0.9 s) | about 4 round trips to Singapore (about 0.1 s) |
| Extra cost | | about $2 a month now (Route53); $6 more with the EU Center |

**Considered, not chosen:**

| Option | Why not |
|---|---|
| Load balancer with 2+ Centers in one region | It covers one region only, so a region outage or a country that can't reach it still stops new connections. It costs about $12 a month per region. To see client IPs it needs PROXY protocol code, or TLS ending at the load balancer (`-trust-proxy-tls`). It also needs separate Center droplets (camera connections count against its limits). Certificates get harder, and nodes still have to link to each Center by name |
| Redis as the shared store, or as the bus between Centers and nodes | **Not the best choice.** Redis becomes the single point of failure, so it needs an HA pair (about $60 a month) in one region. DO Managed Valkey has no backups, so activations still need files. Every node would hold write credentials. And this design needs no shared store: node links plus one activation writer cover it |
| One bigger droplet | It adds capacity, which the Center doesn't need. It is still one server in one place |
| 2 Centers, active plus standby (Route53 failover) | It works, but the standby sits idle, and devices in Asia and EU pay about 0.8 s more at every start. Latency routing gives the same failover plus that speed |

**Code work needed (R21). None of it is built yet:**

1. **A node links to several Centers.** `LINK_TO` takes one address; it must
   take a list. Each Center keeps its own link to the node, and tickets
   signed by any Center are accepted, because they all use the same key.
2. **Writer and reader Centers.** A reader forwards first-time binds and
   takeovers to the writer over an authenticated server-to-server call, and
   receives `activations.jsonl` and the blocklist from it. If the writer is
   unreachable, only a first-time bind waits (`503`, retry); SIDs that are
   already bound keep working.
3. **`matrix-fleet apply` reaches every Center**, and `matrix-fleet status`
   shows each Center, its config version and its copy of the activations.
4. **Promote a writer:** a `matrix-fleet promote` command, run by hand only
   when the writer is lost for a long time. The old writer must be stopped
   first, so there is never more than one writer.
5. **Challenge then verify may reach different Centers** (after a DNS
   change). The camera restarts from `/v1/challenge` when `/verify` answers
   that the nonce is unknown. Check that the firmware does this (`matrix_c`).
6. **The same secrets on every Center:** the Center ticket key, KID and
   `admin.token`. Copy them over SSH, and keep them in the password manager.
7. **A certificate for `matrix.5gen.care` on every Center.** The port-80
   check of Let's Encrypt can land on another Center, so use DNS validation
   through Route53, with an AWS key that may only write the `_acme-challenge`
   record.
8. **Old cameras (proto 2)** sign for the Center's own address. Check that
   this is `matrix.5gen.care` on every Center, not each Center's own name.

Rate limits stay per Center: a client that reaches two Centers gets at most
twice the limit, which is acceptable.

**Migration.** Each step has its own check before it takes traffic, and
today's setup keeps running until step 5:

1. R3 first: an off-host backup of the Center, restored once on a scratch
   droplet.
2. Build the R21 code, and test it on the dev box with 2 Centers and 1 node:
   - stop one Center while cameras connect and viewers start sessions;
   - a first-time bind through the reader Center;
   - the same SID sent to both Centers at the same moment: only one bind may
     win;
   - the writer down: bound SIDs still work, and first-time binds wait;
   - `matrix-fleet apply` changes both Centers.
3. Add the names `center-us.5gen.care` (138.197.12.238) and
   `center-sg.5gen.care` (152.42.232.159). Issue the `matrix.5gen.care`
   certificate by DNS validation on both. Deploy the new version with sg00
   as a reader Center, and give every node both Centers in its link list.
4. **Check before the switch:** section 3.5 passes for the Asia Center, and
   `matrix-fleet status` shows every node linked to both Centers. A day
   before the switch, lower the DNS TTL of `matrix.5gen.care` to 60 s.
5. In Route53, replace the single A record of `matrix.5gen.care` with
   latency records: `us-east-1` for the US IP and `ap-southeast-1` for the
   SG IP, each with its health check. Watch for 24 hours. **Rollback:** put
   the single A record back, pointing at the US Center.
6. EU later: a node in fra1 (section 3.2), then section 3.5 on it, with the
   latency region `eu-central-1`.

**Cost:** about $2 a month for Route53 (one health check per Center, and the
latency queries), with no new droplet. The EU Center comes with the first EU
node, which is needed anyway once EU devices arrive.

**Once built, also watch:**
- the Route53 health check of each Center;
- that every node shows a link to every Center;
- that all Centers run the same version and config version;
- that each reader's activation count equals the writer's.

---

## 2. The servers

### 2.1 Inventory (checked live 2026-11-10)

All four are DigitalOcean droplets: Basic, 1 vCPU, 1 GB RAM, 25 GB disk,
Ubuntu 24.04 LTS, $6 a month.

| Role | DNS name | IP | DO region | Software | Shortcut |
|---|---|---|---|---|---|
| Control Center (+ node `center`, region US) | `matrix.5gen.care` | 138.197.12.238 | nyc3 (New York) | `matrix_cc` 2.6.7, systemd `matrix` | `./vps.sh prod_us` |
| Control node `sg00` (region SG) | `matrix-sg00.5gen.care` | 152.42.232.159 | sgp1 (Singapore) | `matrix_cc` 2.6.7, systemd `matrix` | `./vps.sh prod_sg` |
| Relay `mx-relay-us00` (region US, reports to the Center) | `mx-relay-us00.5gen.care` | 45.55.167.162 | nyc3 | `matrix-relay` 1.2.0, systemd `matrix-relay` | `./vps.sh relay_us` |
| Relay `mx-relay-sg00` (region SG, reports to sg00) | `mx-relay-sg00.5gen.care` | 188.166.244.220 | sgp1 | `matrix-relay` 1.2.0, systemd `matrix-relay` | `./vps.sh relay_sg` |

The droplets' own host names don't match their roles (sg00 is `matrix-02`,
mx-relay-us00 is `matrix-03`). Always go by IP or DNS name.

There is one Center today. The planned Centers (`center-us`, `center-sg`,
later `center-eu`) are in section 1.4.

**Not part of the fleet. Do not change:**
- The AWS server `webrtc.5gen.care` (3.95.228.203) is the admin server
  (section 3.6): backups now, deploys and the license service later. Until
  its dev services are turned off it still runs the testers' old
  `webrtc_prod` and its violet relay. **Never restart or touch `webrtc_prod`.**
- The old AWS Center (34.228.78.198) is retired and is waiting to be deleted
  (R2).

Country routes (config version 2): AU, BD, BN, CN, HK, ID, IN, JP, KH, KR,
LA, LK, MM, MO, MY, NP, NZ, PH, SG, TH, TL, TW and VN go to region **SG**.
Every other country (`*`) goes to **US**.

### 2.2 Logins and access

**This document holds no passwords or keys.** Hand them over in person or
through a password manager, never by chat, e-mail, git or Drive.

| What | User | How you get in | Where the secret is |
|---|---|---|---|
| SSH to all four droplets | `root` | SSH key only. There is no password login and no root password is set. | Private key `prod_do.pem`, now on the owner's PC in `~/code/webrtc/`. For the hand-over, add the new operator's own public key to `/root/.ssh/authorized_keys` on each droplet rather than copying `prod_do.pem` (see below). |
| DigitalOcean (droplets, cloud firewall, monitoring graphs, billing) | the operator's own DO account, invited to the team | DO web console, `doctl` | the DO team admin invites them |
| DNS (zone `5gen.care`) | — | AWS Route53 | managed by the project owner: ask for each new A record |
| `/admin/statistic` (charts, connection log) | none | allowed by IP: `STATISTIC_ALLOW` / `PLAYER_ALLOW` in `/etc/matrix/matrix.conf`, now only the office IP 115.78.12.186 | to open it from elsewhere, add the IP there and run `setup.sh upgrade` (section 4.4) |
| `matrix-fleet status/stats` | `root` on a control server | local admin socket `/run/matrix/admin.sock`, no network port | — |
| Server secrets on disk | — | stay on the servers; only `matrix-backup` copies them out, encrypted (section 3.6) | Center keys `/opt/matrix/cfg/sid/` (`center-ticket.key.pem`, `admin.token`, `tenants.json`, activations); relay secrets `/opt/matrix/cfg/relay-secrets.json` (control) and `/etc/matrix-relay/secrets.json` (relay); node link key `/opt/matrix/cfg/sid/node.key` |

Adding the new operator's key (they send their **public** key `id_ed25519.pub`):

```bash
for ip in 138.197.12.238 152.42.232.159 45.55.167.162 188.166.244.220; do
  ssh -i prod_do.pem root@$ip 'cat >> /root/.ssh/authorized_keys' < id_ed25519.pub
done
```

When someone leaves, remove their line from `authorized_keys` on every
droplet the same way.

### 2.3 Ports and firewalls

| Server | Open | Firewall |
|---|---|---|
| Center, sg00 | TCP 22, 80 (certificate), 443 | ufw |
| mx-relay-us00 | TCP 22, 80, 443; UDP 3478 and 49152–65535 | ufw |
| mx-relay-sg00 | the same | DigitalOcean cloud firewall (ufw is off) |

### 2.4 Files on each server

| Control server (Center and nodes) | |
|---|---|
| `/opt/matrix/matrix_cc` | binary |
| `/opt/matrix/player/ws.html` | web player |
| `/etc/matrix/matrix.conf` | settings: role, domain, KID, allow lists, link target |
| `/opt/matrix/cfg/config.json`, `config.d/` | fleet configuration and every past version (Center only) |
| `/opt/matrix/cfg/relay-secrets.json` | the shared secret of each relay this server hands out |
| `/opt/matrix/cfg/sid/` | keys, tenants, activations, statistics (`statistic/`), connection log (`events/`), relays (`relays/`) |
| `/opt/matrix/backups/<time>/` | previous binary and player, written by each upgrade |
| `journalctl -u matrix` | log |

| Relay | |
|---|---|
| `/usr/local/bin/matrix-relay` | binary |
| `/etc/matrix-relay/env` | flags (`RELAY_FLAGS=...`, now `-max 8000`) |
| `/etc/matrix-relay/secrets.json` | shared secret (0600) |
| `/etc/matrix-relay/tls/` | certificate, refreshed by the certbot deploy hook |
| `/etc/systemd/system/matrix-relay.service` | unit (`MemoryMax=384M`) |
| `journalctl -u matrix-relay` | log |

---

## 3. Adding a server

Each kind follows the same order: **prepare**, **install**, **verify**, then
**put it in service**. The server takes no traffic until the last step. If
any check in "verify" fails, stop: fix it, or destroy the droplet. Never put
a server in service with a failed check.

### 3.1 Prepare (all kinds)

1. Create the droplet: Basic 1 vCPU / 1 GB, Ubuntu 24.04 LTS, monitoring on,
   the operator's SSH key; the same DO region as its users.
   ```bash
   doctl compute droplet create <name> --region <sgp1|nyc3|fra1...> --size s-1vcpu-1gb \
       --image ubuntu-24-04-x64 --ssh-keys <key id> --enable-monitoring --wait
   ```
2. Naming: `matrix-<region><NN>.5gen.care` for a control node (as
   `matrix-sg00`), and `mx-relay-<region><NN>.5gen.care` for a relay (as
   `mx-relay-us00`).
3. Ask for the DNS A record (Route53), and wait until it resolves:
   `dig +short <name>` must print the new IP. certbot needs it.
4. `apt update && apt -y upgrade && reboot`.
5. Copy only the package the droplet needs from the release folder, and
   check it: `sha256sum -c <package>.tar.gz.sha256`.

### 3.2 Control node

**Install**, on the new droplet:

```bash
ufw allow 22/tcp && ufw allow 80/tcp && ufw allow 443/tcp && ufw --force enable
tar xzf matrix-server-<ver>-<commit>.tar.gz && cd matrix-server-<ver>-<commit>
sudo ./setup.sh install-node --domain matrix-<cc>NN.5gen.care \
     --center-key ~/center-ticket.pub.pem --kid matrix-20261023 \
     --link-to wss://matrix.5gen.care/ --name <cc>NN --region <REGION> \
     --blocklist ~/blocklist.txt [--relay-secrets ~/relay-secrets.json]
shred -u ~/blocklist.txt ~/relay-secrets.json 2>/dev/null || true
```

Copy `center-ticket.pub.pem` and `blocklist.txt` from the Center's
`/opt/matrix/cfg/sid/` with `scp`. `KID` is the one in the Center's
`/etc/matrix/matrix.conf`. The install prints the node's entry for
`config.json`; it is also kept in `/opt/matrix/cfg/node-entry.json`.

**★ Verify before deployment (required).** Run these from your PC and from
the new droplet. Every one must pass before the node goes into
`config.json`.

| # | Check | Command | Pass when |
|---|---|---|---|
| 1 | DNS points at the droplet | `dig +short matrix-<cc>NN.5gen.care` | the new IP |
| 2 | Clock synchronised (the link refuses a skew over 60 s) | `timedatectl` | `System clock synchronized: yes` |
| 3 | Service running and enabled | `systemctl is-active matrix; systemctl is-enabled matrix` | `active`, `enabled` |
| 4 | Certificate valid, public port answers | `curl -s https://matrix-<cc>NN.5gen.care/api/health` (from your PC) | `{"status":"ok",...,"version":"<release>"}`, no certificate error |
| 5 | Right version | the `version` from check 4, against `cfg/version` of the release | equal |
| 6 | Same KID and Center key as the Center | `grep KID /etc/matrix/matrix.conf` on both; `sha256sum` of `center-ticket.pub.pem` on both | equal |
| 7 | Firewall | `ufw status` | only 22, 80, 443 open |
| 8 | Log clean | `journalctl -u matrix -n 50` | no `error`/`panic`; until step 9 it may say the Center refused the link (the node is not in `config.json` yet) |
| 9 | **Dry run** of the configuration change, on the Center | `sudo cp /opt/matrix/cfg/config.json ~/new.json`, add the entry (and a route for a new region), `sudo matrix-fleet check ~/new.json` | valid; the change shows only the new node (and routes); the "cameras that would move" count is what you expect |
| 10 | Certificate renewal works | `sudo certbot renew --dry-run` | `succeeded` |
| 11 | Disk and memory | `df -h /; free -m` | disk under 30%, over 500 MB available |

If check 9 shows more cameras moving than you expect, do not apply. Re-read
the routes.

**Put in service**, on the Center:

```bash
sudo matrix-fleet apply ~/new.json      # the server takes it at once, no restart
sudo matrix-fleet status                # the new node: state "up" within seconds
```

**After**:
- `journalctl -u matrix` on the node says "Control node, linked to Center".
- `curl -s https://matrix-<cc>NN.5gen.care/api/health` now shows
  `"center":"https://matrix.5gen.care"`.
- A test camera of that region connects to the new node, and a viewer can
  watch it (from the office player `/demo/test01`, or the app).
- Add a target for the node to `server/deploy.sh` (copy the `sg` block, with
  its IP and health URL), so that upgrades reach it.
- Add a line to the inventory (section 2.1), to `vps.sh`, and to the `todo`
  sheet (Releases tab).

**If it goes wrong**: `sudo matrix-fleet rollback` on the Center restores
the previous configuration, and the node's link drops.

### 3.3 Relay stream server

**Install**, on the new droplet (full commands: INSTALL.md section 3):

```bash
ufw allow 22/tcp && ufw allow 3478/udp && ufw allow 80/tcp && ufw allow 443/tcp \
    && ufw allow 49152:65535/udp && ufw --force enable
useradd --system --no-create-home --shell /usr/sbin/nologin matrix-relay
install -m 0755 matrix-relay /usr/local/bin/matrix-relay
install -d -m 0750 -o root -g matrix-relay /etc/matrix-relay
umask 077; printf '{"shared":"%s","users":{}}\n' "$(openssl rand -hex 32)" > /etc/matrix-relay/secrets.json
chown matrix-relay:matrix-relay /etc/matrix-relay/secrets.json
# certificate: certbot certonly --standalone -d mx-relay-<cc>NN.5gen.care
#   --pre-hook "systemctl stop matrix-relay" --post-hook "systemctl start matrix-relay"
# then install certbot-deploy-hook.sh as /etc/letsencrypt/renewal-hooks/deploy/matrix-relay.sh and run it once
cat > /etc/matrix-relay/env <<EOF
RELAY_FLAGS=-public-ip <ip> -cert /etc/matrix-relay/tls/fullchain.pem -key /etc/matrix-relay/tls/privkey.pem -metrics 127.0.0.1:9641 -control https://<region's control server> -name mx-relay-<cc>NN -region <REGION> -tls-host mx-relay-<cc>NN.5gen.care -capacity-mbps 1000 -max 8000
EOF
install -m 0644 matrix-relay.service /etc/systemd/system/     # has MemoryMax=384M
systemctl daemon-reload && systemctl enable --now matrix-relay
```

The relay starts reporting, but its control server answers
`403 unknown relay` until its secret is added in "put in service". This is
expected: the relay takes no sessions while you verify it.

**★ Verify before deployment (required).** Every check must pass before the
secret goes to the control server.

| # | Check | Command | Pass when |
|---|---|---|---|
| 1 | DNS | `dig +short mx-relay-<cc>NN.5gen.care` | the new IP |
| 2 | Service | `systemctl is-active matrix-relay; systemctl show matrix-relay -p MemoryMax` | `active`, `MemoryMax=402653184` |
| 3 | Flags | `cat /etc/matrix-relay/env` | right `-public-ip`, `-name`, `-region`, `-control`, `-max 8000` |
| 4 | Metrics answer | `curl -s 127.0.0.1:9641/metrics \| grep matrix_relay_allocations` | a value (0) |
| 5 | Ports reachable **from outside**, from your PC | UDP (STUN probe, below); `nc -vz <ip> 80`; `openssl s_client -connect mx-relay-<cc>NN.5gen.care:443 -servername mx-relay-<cc>NN.5gen.care </dev/null \| grep Verify` | `UDP ok`; open; `Verify return code: 0 (ok)` |
| 6 | Certificate expiry | `certbot certificates` | over 60 days left |
| 7 | Reports reach the control server | `journalctl -u matrix-relay -n 20` | report attempts, answered `403 unknown relay` (the secret is not there yet). A network error here means the relay can't reach the control server: fix that first |
| 8 | Firewall | `ufw status` (or the DO cloud firewall) | 22, 80, 443 TCP; 3478 and 49152–65535 UDP |
| 9 | Clock | `timedatectl` | synchronised (reports more than 1 minute off are refused) |

UDP probe for check 5 (`nc -u` can't tell an open UDP port from a closed one,
so this sends a real STUN request):

```bash
python3 -c 'import socket,os,sys;s=socket.socket(2,2);s.settimeout(3);s.sendto(b"\x00\x01\x00\x00\x21\x12\xa4\x42"+os.urandom(12),(sys.argv[1],3478));print("UDP ok" if s.recv(200)[:2]==b"\x01\x01" else "bad answer")' <ip>
```

**Put in service**, on the region's control server. Copy the relay's
`shared` value by file, never by printing it:

```bash
# on the relay
python3 -c 'import json;print(json.dumps({"mx-relay-<cc>NN": json.load(open("/etc/matrix-relay/secrets.json"))["shared"]}))' > /root/new-relay.json
# copy new-relay.json to the control server (scp), then on the control server merge it with the existing file:
cd /opt/matrix && cp cfg/relay-secrets.json ~/relay-secrets.bak-$(date +%Y%m%d)
python3 -c 'import json,sys;a=json.load(open("cfg/relay-secrets.json"));a.update(json.load(open(sys.argv[1])));print(json.dumps(a))' ~/new-relay.json > ~/relay-secrets.json
cd ~/releases/<current package> && sudo ./setup.sh relay-secrets ~/relay-secrets.json
shred -u ~/new-relay.json ~/relay-secrets.json      # and /root/new-relay.json on the relay
```

`relay-secrets.json` is read on each request, so no restart is needed.

**After**:
- Within 15 s the relay log stops showing 403.
- On the Center, `sudo matrix-fleet status` lists the relay as
  `up tcp:up tls:up`.
- `/admin/statistic` on its control server lists it.
- A viewer forced onto the relay plays over each protocol. In `ws.html` from
  the office, pick the relay protocol UDP, then TCP, then TLS. The log must
  say "connected · relay", and on `/admin/statistic` the report shows
  `path relay`.
- Add it to the inventory, `vps.sh` and the `todo` sheet.

**If it goes wrong**: put the backup `relay-secrets.bak-<date>` back. The
relay stops being handed out to new sessions at once.

### 3.4 A new Center (today)

Today a new Center is needed only to replace the current one. That is
disaster recovery from the off-host backup (section 3.6). Follow INSTALL.md section 2, and do not try it without the project
owner.

### 3.5 Adding or removing a Center (after R21)

This applies only once section 1.4 is built.

**Add:**
1. Use a node in the new region. If there is none, create one first
   (sections 3.1 and 3.2). Give the Center its own name,
   `center-<region>.5gen.care`.
2. Turn on the Center role in reader mode, with the writer's address and
   the keys copied over SSH. Issue the `matrix.5gen.care` certificate by
   DNS validation. Add the new Center to every node's link list
   (`matrix-fleet apply`).
3. **Verify before it gets any DNS traffic (required):**

   | # | Check | Pass when |
   |---|---|---|
   | 1 | `curl -s https://center-<region>.5gen.care/api/health` | ok, the same version as the other Centers |
   | 2 | `KID`, `sha256sum` of the Center ticket keys and of `admin.token` | equal to the writer's |
   | 3 | `openssl s_client -connect <its IP>:443 -servername matrix.5gen.care </dev/null \| grep Verify` | Verify return code: 0 (ok) |
   | 4 | `matrix-fleet status` on the new Center | every node linked; the same config version as the others; the writer reachable |
   | 5 | the activation count on it | equal to the writer's |
   | 6 | a test camera connection and a test `/v1/view` sent to it directly (`curl --resolve matrix.5gen.care:443:<its IP> ...`) | answered |
   | 7 | `timedatectl` | synchronised |

4. Add its Route53 health check and latency record. Check that the health
   check is healthy, and that a lookup from that region gets its IP.

**Remove:**
1. Delete its latency record. Wait for the TTL plus 2 minutes.
2. Remove it from the nodes' link lists, then turn off the Center role (or
   destroy the droplet).
3. Never remove the writer this way: promote another Center first
   (`matrix-fleet promote`, with the old writer stopped).

Keep at least 2 Centers at all times.

### 3.6 Backup and restore (R3)

Status 2026-11-13: in place. The admin server pulls from all four servers
every night (the DigitalOcean Cloud Firewall allows SSH from 3.95.228.203),
and a copy it pulled was restored on the PC with a matching manifest.

**The admin server.** `admin@ec2-3-95-228-203.compute-1.amazonaws.com`
(AWS, `../vps.sh dev` until renamed) is where operations start. It holds,
in `/opt/matrix-admin/` (owner `admin`, mode 0700):

| Path | What |
|---|---|
| `servers.conf` | every server: name, IP, role (center, node, relay). Add each new server here |
| `keys/ssh.pem` | root login to every DigitalOcean server |
| `keys/backup.pub.asc` | the backup public key |
| `backups/<server>/<time>.tar.gz.gpg` | encrypted copies, newest 90 per server |
| `backups/sids/` | encrypted copies of `sid_gen` (SID signing key, issued list, batches) |
| `tools/matrix-backup` | the copy installed on servers by `push-tool` |

It is not needed while the servers run: if it is down, cameras and viewers
are not affected; only backups and deploys wait.

**What a copy holds.** `matrix-backup snapshot` on a server writes one
encrypted copy: `cfg/sid/` (activations, Center ticket key, node link key,
tenants, admin token, blocklist, relay registry, statistics, connection
log), `config.json` and `config.d/`, `relay-secrets.json`, `turn.conf`,
`matrix.conf`, the unit files, and on relays `/etc/matrix-relay/` (secrets,
flags). Each copy has `MANIFEST.sha256` and `BACKUP_INFO` (host, time,
versions). Today a copy is 5–50 KB. Certificates are not copied; certbot
issues new ones.

**Who can read it.** Copies are encrypted on the server to the backup public
key. The private key is on **no server**: the project owner keeps
`matrix_backup.key.asc` in the password manager, in two places. Without it
nothing can be restored. The admin server refuses any file that is not
encrypted.

**Commands** (on the admin server):

```bash
matrix-admin backup [NAME...]   # pull a copy now (default: every server); the timer runs it nightly, 03:17 UTC
matrix-admin list               # copies per server; "OLD" when the newest is over 36 h or missing
matrix-admin push-tool [NAME]   # install matrix-backup and the public key on a (new) server
# after generating SIDs, on the PC:
tar -czf - sid_gen | gpg --batch --trust-model always --recipient-file matrix_backup.pub.asc --encrypt |
  ssh admin@<admin server> 'matrix-admin store sids'
```

A new server: add it to `servers.conf`, run `matrix-admin push-tool NAME`,
then `matrix-admin backup NAME`.

**Check it** (daily check, section 5.2): `matrix-admin list` shows no
`OLD`. Until alerting exists (R5), a failed night shows only there and in
`journalctl -u matrix-admin-backup`.

**Restore** (on the operator's PC; copy the file from the admin server):

```bash
gpg --import matrix_backup.key.asc        # once
mkdir restore && gpg --decrypt <time>.tar.gz.gpg | tar -xz -C restore
(cd restore && sha256sum -c MANIFEST.sha256)
```

Then, on a new droplet: install from the release package (INSTALL.md
section 2, using the restored `cfg/sid/` keys; for a relay, section 3),
copy the restored `opt/matrix/cfg/` and `etc/matrix/matrix.conf` (or
`etc/matrix-relay/`) over the new ones (owner `matrix`, `cfg/sid/` mode
0700, files 0600), restart the service, and point DNS at it. Nodes keep
their link because the Center ticket key and `config.json` are the same.

**Drill.** Once a quarter, and after any change to what is copied: decrypt
the newest copy of each server, check the manifest, and compare the Center's
`activations.jsonl` line count and key checksums with the live ones.

DigitalOcean droplet backups (weekly, 20% of the droplet price, about $1.20
a month each) and an AWS snapshot of the admin server are a second layer;
turn them on in each console.

---

## 4. Deploying a new version

### 4.1 Control servers

From `~/code/webrtc/server` on a PC with the key, and only from a committed
and tagged release:

1. **Verify the build before deployment.**
   - `cd src/control && go test ./...` passes, and `git status` is clean.
   - `cfg/version` holds the new version, and the player's
     `PLAYER_VERSION` matches it (a test checks this).
   - Read the release notes in roll-out.md section 1. Check any data-format
     change, and check that rollback still works.
2. Deploy to the **node first**:
   ```bash
   ./deploy.sh sg          # asks you to type 'prod'; builds, uploads, checks sha256, runs setup.sh upgrade
   ```
   It ends with the health answer, which must show the new version.
3. Check sg00 for 15 minutes:
   - `journalctl -u matrix -n 100` shows no errors;
   - `matrix-fleet status` on the Center shows sg00 up, on the new version;
   - one test view works.
4. Then deploy the **Center**: `./deploy.sh prod`, and run the same checks.
5. Write the deploy in the `todo` sheet (Releases tab) and in roll-out.md
   section 1.

Each upgrade saves the previous binary and player in
`/opt/matrix/backups/<time>/`. **Rollback**: copy them back to
`/opt/matrix/` and run `systemctl restart matrix`, or run
`setup.sh upgrade` from the previous package. A restart drops that server's
connections for a few seconds; cameras and viewers reconnect by themselves.

### 4.2 Relays

There is no deploy script for relays yet (R6):

```bash
scp matrix-relay root@<relay>:/usr/local/bin/matrix-relay.new
ssh root@<relay> 'cp -p /usr/local/bin/matrix-relay /usr/local/bin/matrix-relay.prev &&
  mv /usr/local/bin/matrix-relay.new /usr/local/bin/matrix-relay && systemctl restart matrix-relay'
```

A restart drops the sessions on that relay, and they reconnect. Do one
relay at a time, at a quiet hour. Rollback: move `matrix-relay.prev` back
and restart.

### 4.3 Camera firmware

Camera firmware is published to
`https://matrix.5gen.care/downloads/<MODEL>/matrix`, with `matrix.sha256`
and `version` next to it, and cameras take it at their next boot. Each model
has its own folder, and a binary goes only to the models it was built for:

| Model | Build | Folder |
|---|---|---|
| HCX130 | C++ (`libdatachannel_c14/compile.sh`, toolchain anykav330); publish with `publish_firmware.sh HCX130` | `/downloads/HCX130/` |
| GIRAFFE_HD (old cameras that can't run C++); HCX330L | C (`matrix_c`): `./build.sh anykav200` for GIRAFFE_HD, `./build.sh anykav500` for HCX330L, then `scripts/package_anykav200.sh <platform>` | `/downloads/GIRAFFE_HD/`, `/downloads/HCX330L/` (different builds) |

Before a publish, copy the folder's current files to
`/opt/matrix/backups/firmware-<MODEL>-<time>/` on the Center. Rollback: put
them back. A publish reaches every camera of that model at its next boot
(R7), so test on one camera first.

### 4.4 Changing a setting

| Setting | Where | Applies |
|---|---|---|
| Fleet (nodes, routes) | Center: `matrix-fleet check` / `apply` / `rollback` | at once, no restart |
| Relay secrets | `setup.sh relay-secrets FILE` | at once (read per request) |
| Blocklist | `setup.sh blocklist FILE` on **every** control server | at once |
| Allow lists, discovery tenant | edit `/etc/matrix/matrix.conf`, then `setup.sh upgrade` from the current package | after the restart |
| Relay flags | edit `/etc/matrix-relay/env`, then `systemctl restart matrix-relay` | after the restart (drops its sessions) |

---

## 5. Monitoring

Since 2026-11-13 the admin server checks the fleet every 2 minutes and
alerts the Matrix Telegram group (R5, section 5.5). The daily check below is
still worth 5 minutes a day: it shows trends the alerts don't.

### 5.1 Where to look

| Tool | What it shows | How |
|---|---|---|
| `sudo matrix-fleet status` on the **Center** | every node and relay: up/down, cameras, clients, load, CPU, memory, disk, network, version, config version, last report | `./vps.sh prod_us`, then the command |
| `sudo matrix-fleet stats 7` on the Center | per day and node: requests, connected/failed by path, errors; since 2.7.0 also per country (top 20 and other): requests and viewer success by the viewer's country, camera reports by the camera's (`--` = unknown) | same |
| `/admin/statistic` on each control server | charts per 30 min (cameras, viewers, success, relay protocol, machine %), and the connection log (each request, report, error cause, player version) | from the office IP: `https://matrix.5gen.care/admin/statistic`, `https://matrix-sg00.5gen.care/admin/statistic` |
| Health URLs | `{"status":"ok","clients":N,"version":...}` | `curl -s https://<server>/api/health` (public) |
| Relay metrics | `matrix_relay_allocations`, `matrix_relay_quota_rejects_total`, `matrix_relay_auth_failures_total`, `turn_total_traffic_sentb` ... | on the relay: `curl -s 127.0.0.1:9641/metrics` |
| Logs | errors, ALERT lines, takeovers, relay registration | `journalctl -u matrix --since today`, `journalctl -u matrix-relay --since today` |
| DigitalOcean graphs | CPU, memory, disk, bandwidth per droplet, and the monthly transfer pool | DO console → Droplets → Graphs |

### 5.2 Daily check (5 minutes)

```bash
# from the PC
for h in matrix.5gen.care matrix-sg00.5gen.care; do curl -s --max-time 10 https://$h/api/health; echo; done
# on the Center
sudo matrix-fleet status
sudo matrix-fleet stats 1
journalctl -u matrix --since "24 hours ago" | grep -E "ALERT|sid_takeover|panic" | tail
# on the admin server: last night's backups (no line may say OLD)
matrix-admin list
```

Then open `/admin/statistic` on both control servers and look at the last
24 hours.

### 5.3 What to watch, and what to do

| Signal | Normal | Act when | First action |
|---|---|---|---|
| Node or relay state in `matrix-fleet status` | `up` (relay `up tcp:up tls:up`) | `down`, `silent`, or a protocol `:down` | `systemctl status` on it; `journalctl -u matrix(-relay) -n 100`; restart the service; check the DO console |
| Health URL | `"status":"ok"` | no answer, or a certificate error | as above; `certbot certificates` |
| Version, config version | the same on every node | different | redeploy the odd one (section 4) |
| Viewer success (`viewer_connected` against `viewer_failed`, `/admin/statistic`) | over 99% (goal 99.9%) | under 99% in a day, or a new error code | connection log, filter `result=failed`: look at the error cause, path, relay protocol and player version |
| `matrix_relay_quota_rejects_total`, `quota_rejects` count | 0 | any rise | allocations are full: raise `-max`, or add a relay (section 1.3) |
| `auth_failures` | low | a burst | relay secret mismatch between relay and control server; or a client with an old player (version in the log) |
| Relay allocations | under 6,000 | over 6,000 (of 8,000) | add a relay |
| CPU / memory / disk, from status or DO | CPU under 60%, memory under 70%, disk under 70% | over, sustained | section 1.3 (scale), or clean up (disk: old logs, `/opt/matrix/backups/`, `~/releases`) |
| Monthly transfer (DO) | inside the pooled allowance | close to it | add relays (each brings 1,000 GiB) or plan for $0.01 a GiB |
| Certificates | over 30 days left (certbot renews at 30) | under 20 days | `certbot renew`; check `systemctl list-timers certbot.timer` |
| `sid_takeover`, `ALERT` lines | rare, and explained by pairing | unexplained | tell the pairing team (R1) |
| Restarts | none | the service restarted by itself | `journalctl -u matrix -b`: look for a panic or OOM, and keep the log |

Certificates on 2026-11-10: `matrix.5gen.care` to 2027-01-25 and
`mx-relay-us00.5gen.care` to 2027-02-02. All renew automatically through
`certbot.timer`.

### 5.4 Known gaps (roll-out.md)

| # | Gap | Effect for the operator |
|---|---|---|
| R3 | Off-host backup done 2026-11-13 (section 3.6) | check `matrix-admin list` daily; the backup private key is offline with the owner |
| R4 | No graceful shutdown | every restart drops sessions for a few seconds |
| R5 | Telegram alerts done 2026-11-13 (section 5.5); DigitalOcean Monitoring alerts not set up yet | if the admin server is down there are no alerts: the missing daily summary is the sign |
| R6 | No `deploy.sh all`, relay installer or new-node script | follow section 3 by hand |
| R12 | One relay per region (other region is the backup since 2026-11-13) | if a region's relay dies, its relayed views go to the other region's relay, with more delay. A second relay per region removes that |
| R21 | One Center | while the Center is down or restarting, no new connection or view can start anywhere. Plan: section 1.4 |

### 5.5 Alerts: matrix-monitor (R5)

`matrix-monitor` runs on the admin server every 2 minutes (systemd timer
`matrix-monitor.timer`) and posts to the Matrix Telegram group. It sends one
message when a check starts failing (after 2 failing runs in a row, about
4 minutes) and one when it recovers; nothing while all is well. A daily
summary arrives at 08:00 Vietnam time (01:00 UTC): views connected and
failed per control server, versions, open problems. **No summary means the
monitor itself is down**: check the admin server.

| Check | How | Alert when |
|---|---|---|
| `<server> health` | `https://<hostname>/api/health` from outside | not `ok` or no answer |
| `<relay> udp / tcp / tls` | an anonymous STUN request to UDP 3478, TCP 80 and TLS 443 | no answer |
| `<server> certificate` | the certificate on 443 | expires in under 14 days (renewal failing) |
| `<name> node`, `<relay> relay`, `… tcp/tls (fleet)` | `matrix-fleet -json status` on the Center | not healthy, or last report older than 5 min |
| `<name> cpu / mem / disk` | same | above 85 % / 90 % / 85 % |
| `versions` | health of each control server | the control servers run different versions |
| `<server> viewing` | the event log of the last 15 min | at least 3 viewing sessions failed and 20 % or more of them |
| 🚨 takeover ALERT | the event log | a SID was taken over from another network (R1): block the SID and issue a new one if it leaked |
| `<relay> full`, `<relay> logins` | relay metrics over SSH | allocations refused (`-max` reached); more than 50 login failures in 2 minutes |
| `<server> backup` | `/opt/matrix-admin/backups` | newest copy older than 36 h |
| `<server> events / metrics / ssh`, `fleet status` | SSH from the admin server | the admin server can't reach the server |

On the admin server:

```bash
matrix-monitor status          # every check and its state
matrix-monitor run             # one round now (the timer does this)
matrix-monitor summary         # send the summary now
matrix-monitor test            # test message to Telegram
journalctl -u matrix-monitor   # the runs
```

- Files: `/opt/matrix-admin/servers.conf` (4th column: the public
  hostname), `/opt/matrix-admin/monitor/telegram.env` (bot token and group
  id, mode 0600; never copy it elsewhere), optional
  `/opt/matrix-admin/monitor/monitor.env` to change a limit, e.g.
  `FAIL_RATE_PCT=10` (names in the script's `LIMITS`), `state.json`.
- A new server: add its line (with hostname) to `servers.conf`; the next
  run checks it. A removed server: delete its line.
- Install or update: copy `server/admin/matrix-monitor` to
  `/usr/local/bin/`; `sudo matrix-monitor install` sets up the timer.
- A message that fails to send is kept and sent on the next run.
