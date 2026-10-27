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
| `/opt/matrix/` | binary, `player/ws.html`, `cfg/version`, `cfg/turn.conf` (if given), `RELEASE`, `cert.pem` (root:matrix 0640), `backups/` |
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

Center (this server) can spread cameras over several control server nodes. List
them in `/opt/matrix/cfg/sid/nodes.json` (owner `matrix`, mode 0600) and run
`sudo ./setup.sh upgrade`:

```json
{"nodes": [
  {"address": "wss://matrix.5gen.care:443/", "region": "us-east", "countries": ["US"]},
  {"address": "wss://sg1.5gen.care:443/", "region": "sg", "countries": ["VN", "SG"], "capacity": 60000,
   "token_sha256": "<sha256 hex of that node's report token>"}
]}
```

Every other node runs with the same `-kid` and Center public key and reports to
Center every 15 s: `-report-to https://matrix.5gen.care/ -node-token-file FILE`
(the token, 32+ bytes, mode 0600). Center sends each camera to the node it is
already on, else to the least loaded healthy node of its country's region, and
sends viewers to the node the camera is on. For the country choice, put the
DB-IP "IP to Country Lite" CSV (CC BY 4.0, https://db-ip.com) at
`/opt/matrix/cfg/geoip.csv`.

Viewing counters (per UTC day, 31 days, kept in `cfg/sid/stats.json`) for the
whole fleet:

```bash
curl -s -H "Authorization: Bearer $(sudo cat /opt/matrix/cfg/sid/admin.token)" \
     "https://matrix.5gen.care/v1/admin/stats?days=7"
```

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
