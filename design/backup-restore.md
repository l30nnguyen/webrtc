# Backups: read them and restore them into service

Last checked 2026-11-20. Companion to `server_operation.md` section 3.6 (how
backups are taken). This guide answers two questions: **how do I open a
backup**, and **how do I put a backup back into production**.

## 1. Where the backups are

Paths on the PC are relative: `server/`, `sid_gen/`, `matrix_backup.key.asc`
and `prod_do.pem` are in the project folder (the repo checkout's parent);
`matrix-backups/` is in the home folder (`MATRIX_LOCAL_BACKUPS` changes it).
Paths starting with `/opt` or `/etc` are on the servers. In section 4,
`restore-<server>` is the copied `plain/<server>` folder, placed next to the
unpacked release package on the droplet.

| Where | What | Kept |
|---|---|---|
| Each server | nothing; `matrix-backup snapshot` writes a copy to stdout when asked | — |
| Admin server `/opt/matrix-admin/backups/<server>/<time>.tar.gz.gpg` | encrypted copy of `center`, `sg00`, `mx-relay-us00`, `mx-relay-sg00`, pulled nightly at 03:17 UTC; `sids/` holds `sid_gen` | newest 90 per server |
| Operator's PC `matrix-backups/<time>/` | the newest copy of each, **encrypted and decrypted**, plus the admin server's own files (`plain/admin-server/`), pulled daily at 04:00 UTC by `matrix-local-backup` | 7 days |
| Operator's PC `matrix_backup.key.asc` | the **backup private key**: the only thing that opens the encrypted copies | one copy; to be moved to a USB key as well |

**The private key is the single point of failure.** Without
`matrix_backup.key.asc` none of the encrypted copies can be read, on any
machine. Keep at least two copies (PC and USB key). It has no passphrase, so
treat every copy like the plain backups themselves.

`matrix-backups/` holds plain secrets: Center ticket key, admin token,
relay shared secrets, the SID signing key, the firmware signing and
encryption keys, the root SSH key and the Telegram bot token. Never sync it to
a cloud drive, never commit it, never paste its files into chats or tickets.

## 2. What each backup holds

| Backup | Files (relative to `/`) | Needed for |
|---|---|---|
| `center` | `opt/matrix/cfg/sid/`: `center-ticket.key.pem` (signs tickets; nodes trust its public key), `center-ticket.pub.pem`, `issuer.pub.pem` (checks SIDs), `tenants.json`, `activations.jsonl` (which camera holds which SID), `admin.token`, `blocklist.txt`, `last_addresses.json`, `relays/`, `stats.json`, `statistic/`, `events/`; `opt/matrix/cfg/config.json` + `config.d/` (fleet: nodes, routes), `relay-secrets.json`, `cfg/version`, `RELEASE`; `etc/matrix/matrix.conf` (domain, KID, allowlists); `etc/systemd/system/matrix.service` | rebuilding the Center |
| `sg00` (any control node) | `opt/matrix/cfg/sid/node.key` (the node's link key; the Center's `config.json` knows its public half), `node-config.json`, `center-ticket.pub.pem`, `blocklist.txt`, `relays/`, statistics and connection log; `relay-secrets.json`; `matrix.conf`; unit file | rebuilding a node without touching the Center |
| `mx-relay-*` | `etc/matrix-relay/env` (flags: name, region, control server), `secrets.json` (shared secret, also in its control server's `relay-secrets.json`), `tls/` (certificate); unit file | rebuilding a relay |
| `sids` | `sid_gen/`: `issuer.key.pem` (**signs SIDs**), `issuer.pub.pem`, `issued.jsonl`, `generated/` (every batch, with delivery status), tools | issuing SIDs; proving which SIDs exist |
| `plain/admin-server/` (PC only) | `servers.conf`, `keys/ssh.pem` (root on every droplet), `keys/known_hosts`, `keys/backup.pub.asc`, `firmware/keys/` (`sign.key`, `enc-anycloud.key`, `sign.pub`), `monitor/telegram.env`, `monitor/state.json`, `tools/matrix-backup` | rebuilding the admin server; signing firmware |

Not in any backup, because they are rebuilt: TLS certificates of the control
servers (certbot issues new ones), binaries (release package), the player,
and the firmware files under `/opt/matrix/downloads/` and
`/opt/matrix/firmware/` (republish them, section 4.6).

Every server copy has `BACKUP_INFO` (host, time, server version) and
`MANIFEST.sha256` (checksum of every file).

## 3. Read a backup

### 3.1 The daily copy on the PC (already decrypted)

```bash
server/admin/matrix-local-backup list     # copies, newest first, ok/PROBLEMS
ls matrix-backups/                                     # one folder per day (UTC time)
cat matrix-backups/<time>/STATUS                       # what was copied, manifest result per server
ls matrix-backups/<time>/plain/center/opt/matrix/cfg/sid/
```

To take a copy now (for example right before a risky change):
`server/admin/matrix-local-backup run`.

### 3.2 Any encrypted file (`*.tar.gz.gpg`)

From the admin server, from `matrix-backups/<time>/encrypted/`, or from a
USB key. Use a throw-away keyring so the private key does not stay in your
normal one:

```bash
export GNUPGHOME=$(mktemp -d)
gpg --batch --import matrix_backup.key.asc
scp admin@ec2-3-95-228-203.compute-1.amazonaws.com:/opt/matrix-admin/backups/center/<time>.tar.gz.gpg .
mkdir restore-center
gpg --batch --decrypt <time>.tar.gz.gpg | tar -xz --no-same-owner -C restore-center
(cd restore-center && sha256sum -c --quiet MANIFEST.sha256 && cat BACKUP_INFO)
rm -rf "$GNUPGHOME"; unset GNUPGHOME
```

`sha256sum -c` prints nothing when every file matches. A `sids` copy has no
manifest; it unpacks to `sid_gen/`.

Pick an older copy (`ls /opt/matrix-admin/backups/center/`) when the newest
already holds the damage, for example a bad `config.json` applied yesterday.

### 3.3 Useful look-ups

```bash
cd restore-center/opt/matrix/cfg
wc -l sid/activations.jsonl                       # activated cameras (compare with the live server)
python3 -c 'import json; print(list(json.load(open("sid/tenants.json"))))'   # tenants
grep '^KID=' ../../../etc/matrix/matrix.conf      # the Center's key ID, needed to install nodes
python3 -m json.tool config.json | head -40       # nodes and routes
```

## 4. Restore into service

Rule for every case: **restore the newest good copy, start the service, run
the checks in section 5, then take a fresh backup.** Commands run as root on
the target server unless noted. `prod_do.pem` (or
`plain/admin-server/keys/ssh.pem`, the same key) logs in as root.

### 4.1 One file is lost or damaged on a running server

Example: `activations.jsonl` corrupted on the Center, or a wrong
`relay-secrets.json`.

```bash
# on the PC
scp -i prod_do.pem matrix-backups/<time>/plain/center/opt/matrix/cfg/sid/activations.jsonl \
    root@138.197.12.238:/tmp/
# on the server
cp -a /opt/matrix/cfg/sid/activations.jsonl /opt/matrix/cfg/sid/activations.jsonl.bad-$(date -u +%Y%m%dT%H%M%SZ)
install -o matrix -g matrix -m 0600 /tmp/activations.jsonl /opt/matrix/cfg/sid/activations.jsonl
shred -u /tmp/activations.jsonl
systemctl restart matrix
```

Activations made after the backup are lost; those cameras activate again on
their next connection (same SID, same device). For `config.json` use
`matrix-fleet rollback` first: it keeps every applied version in `config.d/`.
`relay-secrets.json`, `turn.conf` and the blocklist are read without a
restart.

### 4.2 Rebuild a control node (sg00)

The Center is untouched; the node gets its old identity back so the Center's
`config.json` entry still matches.

1. Create the droplet (`server/INSTALL.md` section 0 in the release folder),
   same region. If the IP changes, add it to the DigitalOcean Cloud Firewall
   rule for SSH and update DNS (`matrix-sg00.5gen.care`, Route53).
2. Build or copy the release package (`server/deploy.sh` builds
   `dist/matrix-server-<version>-<commit>.tar.gz`), unpack it on the droplet.
3. Install with the restored settings:

   ```bash
   R=../restore-sg00          # copied from matrix-backups/<time>/plain/sg00
   sudo ./setup.sh install-node --domain matrix-sg00.5gen.care \
       --center-key $R/opt/matrix/cfg/sid/center-ticket.pub.pem \
       --kid <KID from the Center's matrix.conf> --link-to wss://matrix.5gen.care/ \
       --name sg00 --region SG --relay-secrets $R/opt/matrix/cfg/relay-secrets.json \
       --blocklist $R/opt/matrix/cfg/sid/blocklist.txt
   systemctl stop matrix
   cp -a $R/opt/matrix/cfg/sid/. /opt/matrix/cfg/sid/       # node.key, node-config.json, statistics, log
   cp $R/etc/matrix/matrix.conf /etc/matrix/matrix.conf
   chown -R matrix:matrix /opt/matrix/cfg; chmod 700 /opt/matrix/cfg/sid
   find /opt/matrix/cfg/sid -type f -exec chmod 600 {} +
   systemctl start matrix
   ```

4. If the node's IP changed, the Center must allow it to link: update the
   node's address in `config.json` on the Center with
   `matrix-fleet check new.json && matrix-fleet apply new.json`.
5. Cameras of the region reconnect by themselves (the Center sends them to
   the node again).
6. Push the backup tool: on the admin server
   `matrix-admin push-tool sg00 && matrix-admin backup sg00`.

### 4.3 Rebuild the Center

The Center signs tickets and holds activations; while it is down **no one
can start a view** (even on nodes) and no camera can activate. Nodes keep
their current cameras connected.

1. Droplet in nyc3 (`INSTALL.md` section 0). Point `matrix.5gen.care` at the
   new IP in Route53 (low TTL helps), and open SSH for it in the DigitalOcean
   Cloud Firewall.
2. Make the keys folder from the backup and install:

   ```bash
   R=../restore-center        # from matrix-backups/<time>/plain/center
   K=$(mktemp -d); S=$R/opt/matrix/cfg/sid
   cp $S/issuer.pub.pem $S/center-ticket.key.pem $S/center-ticket.pub.pem $S/tenants.json $S/blocklist.txt $K/
   grep -E '^(KID|DISCOVERY_TENANT|PLAYER_ALLOW|MIN_ISSUED)=' $R/etc/matrix/matrix.conf
   sudo ./setup.sh install --domain matrix.5gen.care --keys $K \
       --kid <KID> --min-issued <MIN_ISSUED> --discovery-tenant test01 --player-allow 115.78.12.186
   rm -rf $K
   ```

3. Put the rest of the state back:

   ```bash
   systemctl stop matrix
   cp -a $S/. /opt/matrix/cfg/sid/                 # activations, admin.token, last addresses, relays, statistics, log
   cp -a $R/opt/matrix/cfg/config.json $R/opt/matrix/cfg/config.d /opt/matrix/cfg/
   cp -a $R/opt/matrix/cfg/relay-secrets.json /opt/matrix/cfg/
   cp $R/etc/matrix/matrix.conf /etc/matrix/matrix.conf
   chown -R matrix:matrix /opt/matrix/cfg; chmod 700 /opt/matrix/cfg/sid
   find /opt/matrix/cfg/sid -type f -exec chmod 600 {} +
   systemctl start matrix
   ```

4. Republish the camera firmware (section 4.6).
5. Nodes reconnect to `wss://matrix.5gen.care/` once DNS points at the new
   IP: their link key and the Center ticket key are unchanged.
6. `mx-relay-us00` reports to the Center: nothing to do once DNS has moved.
7. On the admin server: update the Center's IP in `servers.conf`, then
   `matrix-admin push-tool center && matrix-admin backup center`.

### 4.4 Rebuild a relay

Follow `INSTALL.md` section 3 (release folder) but use the restored files
instead of making new ones, so the control server's `relay-secrets.json`
still matches:

```bash
R=../restore-relay        # from matrix-backups/<time>/plain/mx-relay-us00
install -d -m 0750 -o root -g matrix-relay /etc/matrix-relay
cp -a $R/etc/matrix-relay/. /etc/matrix-relay/
chown -R matrix-relay:matrix-relay /etc/matrix-relay/secrets.json /etc/matrix-relay/tls
chmod 600 /etc/matrix-relay/secrets.json
sed -i 's/-public-ip [^ ]*/-public-ip <new ip>/' /etc/matrix-relay/env    # only if the IP changed
install -m 0644 $R/etc/systemd/system/matrix-relay.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now matrix-relay
```

The binary comes from the release package (`relay/matrix-relay`) or
`go build` in `server/src/relay`. Install the certbot hook from section 3 so
the restored certificate keeps renewing; update DNS if the IP changed. While
a relay is down, relayed views use the other region's relay.

### 4.5 Rebuild the admin server

Nothing in production stops while it is down; only backups, monitoring and
firmware signing wait.

1. New VM (Ubuntu), user `admin`, `gpg`, `python3`. Allow its IP for SSH in
   the DigitalOcean Cloud Firewall.
2. From the PC copy `matrix-backups/<time>/plain/admin-server/` to
   `/opt/matrix-admin/` (owner `admin`, mode 0700; `keys/ssh.pem`,
   `firmware/keys/*.key`, `monitor/telegram.env` mode 0600).
3. Put the history back: for each server, copy
   `matrix-backups/*/encrypted/<server>.tar.gz.gpg` to
   `/opt/matrix-admin/backups/<server>/<time>.tar.gz.gpg` (keep the time from
   the folder name). Older copies existed only on the old admin server.
4. Install the tools from the repo and start the timers:

   ```bash
   sudo install -m 755 server/admin/matrix-admin /usr/local/bin/matrix-admin
   sudo install -m 755 server/admin/matrix-monitor /opt/matrix-admin/monitor/matrix-monitor
   sudo matrix-admin install-timer
   sudo /opt/matrix-admin/monitor/matrix-monitor install
   matrix-admin backup && matrix-admin list        # no line may say OLD
   ```

5. Build `mxpkg` (`matrix_c/tools/mxpkg`, static) into
   `/opt/matrix-admin/tools/` for firmware signing.
6. On the PC, point `matrix-local-backup` at the new host
   (`MATRIX_ADMIN=admin@<new host>`, or edit the default) and run it once.

### 4.6 Firmware files (Center)

`/opt/matrix/downloads/<MODEL>/{matrix,matrix.sha256,version}` for HCX130,
HCX330L and GIRAFFE_HD, and `/opt/matrix/firmware/anycloud/matrix.pkg`.
Source: `matrix_c/release/bin/<platform>/matrix_<version>` on the PC and
`/opt/matrix-admin/firmware/packages/` on the admin server (signed
packages). Publish the version listed as current in `server/design/roll-out.md`
with the same steps as a normal publish (checksums checked before the move).
Until then cameras that boot keep their old binary or fail to download.

### 4.7 SID signing key and batches

`plain/sids/sid_gen/` is the complete `sid_gen` folder. To restore it on the
PC: copy it to `sid_gen/` (mode 0700, `issuer.key.pem` 0600).
`issued.jsonl` and the batch CSVs record which SIDs exist and who received
them (`delivered_to`); never issue SIDs from an old copy without merging the
newer batches, or numbers may repeat.

### 4.8 Firmware signing keys

`plain/admin-server/firmware/keys/` (`sign.key`, `enc-anycloud.key`,
`sign.pub`). Put them back in `/opt/matrix-admin/firmware/keys/` (0700,
keys 0600). An encrypted copy is also in
`/opt/matrix-admin/firmware/firmware-keys-*.tar.gpg` and
`matrix-backups/firmware-keys/`. If `sign.key` is lost for good, cameras
cannot accept new packages until their firmware trusts a new key: guard it.

## 5. After every restore

- `curl -s https://<server>/api/health`: `status ok`, expected version,
  `clients` climbing back.
- On the Center: `sudo matrix-fleet status`: every node linked, every relay
  healthy.
- A real view from the office (`/demo/test01`), once direct and once forced
  through the relay.
- On the admin server: `matrix-monitor run` exits 0; `matrix-admin backup
  <server>` succeeds; `matrix-admin list` shows no `OLD`.
- Write what happened in `roll-out.md`.

## 6. Drill

Once a quarter, and after any change to what is backed up: take the newest
copy of each server from `matrix-backups`, confirm `STATUS` says `ok`,
compare the Center's `activations.jsonl` line count with the live one, and
rebuild one node (4.2) on a scratch droplet without linking it to the
Center. Delete the scratch droplet afterwards.
