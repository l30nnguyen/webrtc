# Server operation

Updated 2026-11-10. This is the hand-over guide for the person who runs the
matrix servers and deploys them. It covers:
- how the servers fit together
- how to add or remove one
- how to check a new server before it takes traffic
- what to watch
- the planned target: several Center servers behind a load balancer
  (section 1.4; not built yet)

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

Today there is **one** Center and no load balancer. While the Center is down
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
- **Relays** need no entry in `config.json`. A relay is used once its
  region's control server has its shared secret in
  `/opt/matrix/cfg/relay-secrets.json`. A relay is known only to the control
  server it reports to, so there is no backup from another region yet (R12).
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

### 1.4 Target: the Center behind a load balancer (planned, R21)

**Status 2026-11-10: planned, not built.** Everything above section 1.4
describes the fleet as it runs today. The code changes listed below are
needed before a second Center can run.

**Why.** The Center is the one server every new connection and every new
viewing session passes through. The goal is 99.9% successful views from any
viewer network. For that, the Center must survive the loss of one droplet,
and an upgrade, without stopping new connections.

```
 CAMERAS, VIEWERS, PAIRING SYSTEM   (still https://matrix.5gen.care: nothing changes on them)
                 |
                 v   DNS: matrix.5gen.care -> load balancer IP
 ┌──────────────────────────────────────────────────────────┐
 │ LOAD BALANCER (DigitalOcean, nyc3)                       │
 │ TCP 443 passed through to the Centers + PROXY protocol   │
 │ health check: GET /api/health on each Center every 10 s  │
 └─────────────┬─────────────────────────────┬──────────────┘
               v                             v
 ┌──────────────────────────┐  ┌──────────────────────────┐
 │ Center A                 │  │ Center B                 │   add C, D ...
 │ center-a.5gen.care       │  │ center-b.5gen.care       │   the same way
 │ tickets, routing,        │  │ same keys, KID and       │   (section 3.5)
 │ activation; no cameras   │  │ config version as A      │
 └─────────────┬────────────┘  └─────────────┬────────────┘
               │ activations, takeover state,│
               v revocations, config         v
 ┌──────────────────────────────────────────────────────────┐
 │ SHARED STORE: DigitalOcean Managed PostgreSQL (nyc3),    │
 │ primary + standby, daily backups (also closes R3)        │
 └──────────────────────────────────────────────────────────┘

 CONTROL NODES link to EVERY Center directly (not through the load balancer),
 so each Center knows where every camera is:

   node us00 (nyc3, NEW: takes the US cameras off the Center) ──link──> A, B
   node sg00 (sgp1)                                           ──link──> A, B

 RELAYS report to their region's node, never to a Center:

   mx-relay-us00 ──report──> us00         mx-relay-sg00 ──report──> sg00
```

| | Today | Target |
|---|---|---|
| Center | one droplet, which is also the US control node | 2 or more droplets, Center role only, behind the load balancer |
| US cameras | on the Center (node `center`) | on a new node `us00` in nyc3 |
| `mx-relay-us00` reports to | the Center | `us00` |
| Activations, takeover state, revocations | files on the Center (`cfg/sid/`) | shared PostgreSQL |
| Node link | to `matrix.5gen.care` | to each Center by its own name |
| One Center lost | no new connection or view until it is back | the load balancer stops sending to it within about 30 s; the others serve |
| Center upgrade | all of its connections drop for a few seconds | one Center at a time, taken out of the pool first: no drop |
| Backup (R3) | none off-host | managed database backups; keys kept in the password manager |

**Code work needed (R21). None of it is built yet:**

1. **Client address behind the load balancer.** Several features read the
   TCP source address: rate limits, the refused log, `PLAYER_ALLOW` and
   `STATISTIC_ALLOW`, and the node-link IP check. Behind a load balancer,
   that address is the load balancer. Fix: read the PROXY protocol header,
   and accept it only from the load balancer's addresses.
2. **Shared activation store.** `activations.jsonl` is a local file, so two
   Centers could each bind the same SID to a different device. Fix: keep it
   in PostgreSQL, with a compare-and-set per SID. This is the "registry
   ownership" item of the 2026-10-08 review (roll-out.md section 3).
3. **Takeover and revocation.** A takeover is decided from the camera list
   that the node links feed in, and the revocation is written to the local
   blocklist. Fix: every Center sees every node (item 4), and revocations go
   to the shared store.
4. **A node links to several Centers.** `LINK_TO` takes one address; it must
   take a list. `matrix-fleet apply` must reach every Center: either keep
   the configuration versions in the shared store, or apply on each Center.
5. **A Center with no node of its own.** Today `center` is also a node in
   `config.json`. Check, and fix if needed, that a Center runs with no node
   of its own.
6. **Statistics and the connection log are per server.** Each Center's
   `/admin/statistic` would show only its own share: add a merged view, or
   link the instances' pages to each other.
7. **Secrets on every Center.** The Center ticket key and `admin.token` must
   be on every Center. Copy them over SSH, never through the load balancer,
   and keep them in the password manager.

**Migration.** Each step has its own check before it takes traffic, and
today's setup keeps running until step 6:

1. R3 first: an off-host backup of the Center, restored once on a scratch
   droplet.
2. Add node `us00` in nyc3 (section 3.2, with its checks). Move
   `mx-relay-us00` to report to `us00`: put its secret in `us00`'s
   `relay-secrets.json`, then change its `-control`. Once item 5 is checked,
   take node `center` out of `config.json`; US cameras then move to `us00`
   as they reconnect. Check: a US test camera is on `us00` and can be
   watched.
3. Build the R21 code, and test it on the dev box: a local load balancer
   (haproxy), 2 Centers and 1 node. Tests:
   - stop one Center during viewing;
   - upgrade one Center;
   - send the same SID to both Centers at the same moment: only one bind
     may win.
4. Create the managed PostgreSQL, move the activations into it, create
   Center B, and create the load balancer with A and B. A day before the
   switch, lower the DNS TTL of `matrix.5gen.care` to 60 s.
5. **Check before the switch:**
   - section 3.5 passes on each Center;
   - the load balancer shows both Centers healthy;
   - through the load balancer, with DNS not yet changed:
     `curl --resolve matrix.5gen.care:443:<LB IP> https://matrix.5gen.care/api/health`
     answers;
   - a test camera and a test view work through the same `--resolve` path.
6. Point the DNS A record of `matrix.5gen.care` at the load balancer, and
   watch for 24 hours. **Rollback:** point DNS back at Center A's IP
   (Center A keeps running).

**Cost:** about $12 a month for the load balancer (DO small). Managed
PostgreSQL starts at about $15 a month, or about $30 with a standby. Add one
more $6–12 droplet for each extra Center. Check the current DO prices before
ordering.

**Once built, also watch:**
- the load balancer's health of each Center (DO console);
- PostgreSQL CPU, connections and backups;
- that every node shows a link to every Center;
- that all Centers run the same version and config version.

A whole-region outage of nyc3 would still stop the Centers, because the
load balancer and the database are in one region. A second region with DNS
failover is a later step, not part of R21.

---

## 2. The servers

### 2.1 Inventory (checked live 2026-11-10)

All four are DigitalOcean droplets: Basic, 1 vCPU, 1 GB RAM, 25 GB disk,
Ubuntu 24.04 LTS, $6 a month.

| Role | DNS name | IP | DO region | Software | Shortcut |
|---|---|---|---|---|---|
| Control Center (+ node `center`, region US) | `matrix.5gen.care` | 138.197.12.238 | nyc3 (New York) | `matrix_cc` 2.6.6, systemd `matrix` | `./vps.sh prod_us` |
| Control node `sg00` (region SG) | `matrix-sg00.5gen.care` | 152.42.232.159 | sgp1 (Singapore) | `matrix_cc` 2.6.6, systemd `matrix` | `./vps.sh prod_sg` |
| Relay `mx-relay-us00` (region US, reports to the Center) | `mx-relay-us00.5gen.care` | 45.55.167.162 | nyc3 | `matrix-relay` 1.2.0, systemd `matrix-relay` | `./vps.sh relay_us` |
| Relay `mx-relay-sg00` (region SG, reports to sg00) | `mx-relay-sg00.5gen.care` | 188.166.244.220 | sgp1 | `matrix-relay` 1.2.0, systemd `matrix-relay` | `./vps.sh relay_sg` |

The droplets' own host names don't match their roles (sg00 is `matrix-02`,
mx-relay-us00 is `matrix-03`). Always go by IP or DNS name.

There is one Center and no load balancer yet. The planned Center pool
(`center-a`, `center-b`, load balancer, `us00`) is in section 1.4.

**Not part of the fleet. Do not change:**
- The AWS dev VPS `webrtc.5gen.care` (3.95.228.203) will become the license
  server. It runs the testers' old `webrtc_prod` and its violet relay.
  **Never restart or touch `webrtc_prod`.**
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
| Server secrets on disk | — | stay on the servers; never copy them out | Center keys `/opt/matrix/cfg/sid/` (`center-ticket.key.pem`, `admin.token`, `tenants.json`, activations); relay secrets `/opt/matrix/cfg/relay-secrets.json` (control) and `/etc/matrix-relay/secrets.json` (relay); node link key `/opt/matrix/cfg/sid/node.key` |

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
disaster recovery, and it needs the off-host backup, which does not exist
yet (R3). Follow INSTALL.md section 2, and do not try it without the project
owner.

### 3.5 Adding or removing a Center behind the load balancer (after R21)

This applies only once section 1.4 is built.

**Add:**
1. Create a droplet in the same DO region as the load balancer. Give it its
   own DNS name, `center-<x>.5gen.care`.
2. Install it as a Center, with the keys copied over SSH and the shared
   store's address.
3. **Verify before it joins the pool (required):**

   | # | Check | Pass when |
   |---|---|---|
   | 1 | `curl -s https://center-<x>.5gen.care/api/health` | ok, the same version as the other Centers |
   | 2 | `KID`, `sha256sum` of the Center ticket keys and of `admin.token` | equal to Center A's |
   | 3 | `journalctl -u matrix -n 50` | connected to the shared store; no errors |
   | 4 | `matrix-fleet status` on the new Center | every node linked; the same config version as the others |
   | 5 | a test `/v1/view` sent to it directly (`curl --resolve matrix.5gen.care:443:<its IP> ...`) | answered |
   | 6 | `timedatectl` | synchronised |

4. Add it to the load balancer pool (the DO tag `matrix-center`). Check that
   the load balancer shows it healthy.

**Remove:**
1. Take it out of the pool (remove the tag).
2. Wait 2 minutes. Center requests are short, apart from the node links,
   which go to it directly.
3. Stop the service. Remove it from the nodes' link lists, then destroy the
   droplet.

Keep at least 2 Centers in the pool at all times.

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

Camera firmware is out of scope for server operation. It is published from
`libdatachannel_c14/publish_firmware.sh` to
`https://matrix.5gen.care/downloads/HCX130/matrix`, and cameras take it at
their next boot. Rollback: publish the previous binary.

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

There is no automatic alerting yet (R5, planned for phase 2). Until there
is, the operator runs the daily check by hand.

### 5.1 Where to look

| Tool | What it shows | How |
|---|---|---|
| `sudo matrix-fleet status` on the **Center** | every node and relay: up/down, cameras, clients, load, CPU, memory, disk, network, version, config version, last report | `./vps.sh prod_us`, then the command |
| `sudo matrix-fleet stats 7` on the Center | per day and node: requests, connected/failed by path, errors | same |
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
| R3 | No off-host Center backup | losing the Center droplet loses activations and keys. Take a DO snapshot of the Center before each risky change |
| R4 | No graceful shutdown | every restart drops sessions for a few seconds |
| R5 | No alerting | the daily check above is the only monitoring |
| R6 | No `deploy.sh all`, relay installer or new-node script | follow section 3 by hand |
| R12 | No cross-region backup relay | if a region's only relay dies, relayed views in that region fail until it is back |
| R21 | One Center, no load balancer | while the Center is down or restarting, no new connection or view can start anywhere. Plan: section 1.4 |
