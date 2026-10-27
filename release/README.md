# Matrix server release package

`release/make_release.sh` builds `dist/matrix-server-<version>-<commit>.tar.gz`
(linux/amd64 binary built with the toolchain pinned in `src/go/go.mod`, the
player, `cfg/version`, `setup.sh`, and `RELEASE` with the commit and hashes).

## Fresh install (Ubuntu)

DNS for the domain must point at the VPS, and ports 80 (certificate) and 443
must be open.

```bash
tar xzf matrix-server-<version>-<commit>.tar.gz
cd matrix-server-<version>-<commit>
sudo ./setup.sh install --domain matrix.5gen.care --keys /path/to/keys --discovery-tenant test01
```

The keys folder holds `issuer.pub.pem`, `center-ticket.key.pem`, `tenants.json`
and optionally `blocklist.txt` and `center-ticket.pub.pem`. Delete it after the
install.

What it sets up:

| Path | Contents |
| --- | --- |
| `/opt/matrix/` | binary `matrix_cc` (the package's `bin/matrix_cc_<version>`), `player/ws.html`, `cfg/version`, `cfg/turn.conf` (if given), `RELEASE`, `cert.pem` (root:matrix 0640), `backups/` |
| `/opt/matrix/cfg/sid/` | keys, tenants, blocklist, activation store, generated `admin.token` (matrix 0700/0600) |
| `/etc/matrix/matrix.conf` | domain, port, key ID, minimum SID issue time, discovery tenant, player allowlist |
| `matrix.service` | systemd unit, enabled at boot, restarts on failure, runs as `matrix` |
| `/etc/letsencrypt/renewal-hooks/deploy/matrix.sh` | rebuilds `cert.pem` and restarts the service after each renewal (`certbot.timer`) |

`--discovery-tenant` turns on the camera list for that tenant (demo only) and
serves the player at `/demo/<tenant>`; leave it out to keep both off.
`--player-allow` (comma-separated IPs/CIDRs) limits the player page and the
camera list to those client addresses; everyone else gets 404. Health, Center,
WebSocket and downloads stay public.

To change a setting later, edit `/etc/matrix/matrix.conf` (`DISCOVERY_TENANT`,
`PLAYER_ALLOW`, ...) and run `sudo ./setup.sh upgrade` from the current package.

## TURN servers

Cameras get their TURN relays from `/opt/matrix/cfg/turn.conf` (root:matrix
0640), a JSON list of `{"host", "port", "username", "password"}`. It holds
credentials, so it is not in the package: pass `--turn FILE` to `install`, or
set or replace it later (read on every connection, no restart):

```bash
sudo ./setup.sh turn /path/to/turn.conf
```

Upgrades keep the installed file. Without it, cameras get no TURN relay.

## Fleet (several control server nodes)

Center (this server) can spread cameras over several control server nodes,
listed with the country routes in `/opt/matrix/cfg/config.json`:

```json
{
  "nodes": [
    {"name": "us00", "address": "wss://matrix.5gen.care:443/", "region": "US"},
    {"name": "eu00", "address": "wss://matrix-eu00.5gen.care:443/", "region": "DE", "capacity": 60000,
     "public_key": "<the node's link key, from matrix_cc fleet keygen>"}
  ],
  "routes": {"IT": "DE", "DE": "DE", "*": "US"}
}
```

- `routes` maps an ISO 3166 two-letter country (`GB`, not `UK`) to a region;
  `*` is every other country. A region with several nodes splits its cameras
  by load; a region that is down or full sends them to the least loaded node.
  A node's own `"countries": [...]` list still works when no route names the
  country.
- Change the file only with `matrix-fleet`, working on a copy:

```bash
sudo cp /opt/matrix/cfg/config.json ~/new.json && nano ~/new.json
sudo matrix-fleet check ~/new.json      # validate, show the changes and how many online cameras would move
sudo matrix-fleet apply ~/new.json      # install as the next version; the server takes it at once (no restart)
sudo matrix-fleet rollback [VERSION]    # re-apply the previous (or a given) version
```

  Every version is kept in `/opt/matrix/cfg/config.d/`. A rejected file
  changes nothing. Cameras already connected stay where they are; the change
  applies to new connections.

Every other node runs with the same `-kid` and Center public key
(`-ticket-public-key`) and keeps a link open to Center's private link port:

1. On Center, set `LINK_LISTEN=<private ip>:8444` in `/etc/matrix/matrix.conf`
   and run `sudo ./setup.sh upgrade`. Allow that port only from the nodes
   (DigitalOcean: Cloud Firewall rule from the tag `matrix-node`).
2. On the node (DNS pointing at it, port 80 open), from the extracted package:

   ```bash
   sudo ./setup.sh install-node --domain matrix-eu00.5gen.care \
        --center-key center-ticket.pub.pem --kid <Center's KID> \
        --link-to wss://<Center private ip>:8444/ --link-server-name matrix.5gen.care \
        --name eu00 --region DE [--blocklist blocklist.txt] [--relay-secrets relay-secrets.json]
   ```

   It installs the server, gets the certificate (renewed by certbot.timer),
   creates the node's own key (`cfg/sid/node.key`, never copied elsewhere),
   starts the `matrix` service and prints the node's entry for config.json
   (also kept in `/opt/matrix/cfg/node-entry.json`). `KID` is the one in
   Center's `/etc/matrix/matrix.conf`. Upgrades: `sudo ./setup.sh upgrade`.
3. On Center, add that entry to `nodes` and `sudo matrix-fleet apply` it.
   `sudo matrix-fleet status` shows the node up within seconds; on the node it
   says "Control node, linked to Center ...".

The blocklist is not pushed over the link: replace it on every server with
`sudo ./setup.sh blocklist blocklist.txt` (checked first, read without a
restart; a broken file is refused because it would refuse every connection).

The node logs in with its key, sends its camera list, then each camera that
comes or goes and its counters every minute. Center pushes every applied
configuration to the nodes, signed with the Center key; a node keeps the last
one in `cfg/node-config.json` and keeps serving if Center is down. Removing a
node from config.json or changing its key drops its link at once.
`matrix-fleet status` shows the configuration version each node runs.

Center sends each camera to the node it is already on (if that is still in its
region), else to the least loaded linked node of its region, and sends viewers
to the node the camera is on. For the country choice, put the DB-IP "IP to
Country Lite" CSV (CC BY 4.0, https://db-ip.com) at `/opt/matrix/cfg/geoip.csv`;
`matrix-fleet check` then also warns about countries no IP range carries.

Status and viewing counters (per UTC day, 31 days, kept in
`cfg/sid/stats.json`) for the whole fleet come from the server over its local
admin socket `/run/matrix/admin.sock`; nothing administrative is served on the
public port:

```bash
sudo matrix-fleet status        # nodes up/down, cameras, clients, load, config version
sudo matrix-fleet stats 7       # requests, connected/failed by path, errors, per day and per node
sudo matrix-fleet -json stats 7 # the same as JSON
```

## Relay stream servers per region

With `relays` in config.json, each viewing session gets the relay of the
camera's region (the region of the node it is on) plus one from another region
as a backup.

```json
"relays": [
  {"name": "turn-vn00", "region": "VN", "host": "webrtc.5gen.care", "port": 3478,
   "capacity_mbps": 800, "metrics": "http://10.20.0.5:9100/metrics"}
]
```

Every server that hands a relay out keeps its credentials in
`/opt/matrix/cfg/relay-secrets.json` (owner `matrix`, mode 0600;
`sudo ./setup.sh relay-secrets FILE`), read on each viewing request:

- **violet** (what we run) has fixed credentials only:
  `{"turn-vn00": {"username": "matrix", "password": "<the --credentials password>"}}`.
  The same pair goes to every session, as with `cfg/turn.conf`; change it in
  violet's config and in this file together to rotate it.
- **coturn** with `use-auth-secret` and `static-auth-secret=<secret>`:
  `{"turn-eu00": "<secret>"}`. Each session then gets its own user name and
  password, valid 24 hours.

A relay without credentials on a server is not handed out by that server;
with none at all the server keeps using `cfg/turn.conf`.

**Health and load.** Each node checks the relays of its own region every 15 s
by asking the relay port itself (a STUN request; no credentials needed) and, if
`metrics` is set, reads bandwidth from it. Violet has no metrics endpoint: run
Prometheus node_exporter on the relay host (port 9100, reachable from the
region's node only) and point `metrics` at it; bandwidth is the busier
direction of the host's network counters, against `capacity_mbps`. Sessions go
to the least loaded healthy relay; a relay that does not answer, or is full,
gets none. Relays of other regions are picked by capacity. `matrix-fleet
status` lists every relay with its state and load.

## Limits on the public port

The server limits each source address (IPv6: each /64) by default: 20 Center
calls a second (burst 200), 300 new camera/viewer connections a minute, 50
other requests a second (burst 200) and 1000 open connections. Over a limit it
answers 429 and counts it as "Refused, too many" in `matrix-fleet stats`.
DigitalOcean filters network floods for free; these limits cover request
floods. Change them with `-rate-api`, `-rate-connect`, `-rate-web`,
`-conns-per-source` (0 turns one off) and skip trusted addresses with
`-rate-exempt`.

## Upgrade

```bash
tar xzf matrix-server-<version>-<commit>.tar.gz
cd matrix-server-<version>-<commit>
sudo ./setup.sh upgrade
```

The previous binary and player are kept in `/opt/matrix/backups/<time>/`.

## Operate

```bash
systemctl status matrix
journalctl -u matrix -f
sudo certbot renew --dry-run
```
