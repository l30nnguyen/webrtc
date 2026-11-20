# Adding a server: matrix-provision (admin server)

Since 2026-11-20. Ops create the droplet; one command on the admin server
installs it, registers it for backups and monitoring, tests it and prints a
report; a person then puts it in service with the steps the report lists.
Rebuilding a lost server from its backup is `backup-restore.md`.

## 1. Before: the release on the admin server

The admin server installs from the release files it holds
(`/opt/matrix-admin/releases/`). After each release, from the PC:

```bash
server/admin/push-release        # builds the server package, matrix-relay and relaycheck; stores them
ssh admin@<admin server> matrix-provision releases
```

## 2. Ops: create the droplet

1. DigitalOcean, Ubuntu 24.04, 1 vCPU / 1 GB or more, in the region it
   serves; choose the **matrix SSH key** (the admin server logs in with it).
2. Route53: point the name at the droplet's IP, for example
   `matrix-sg01.5gen.care` (control node) or `mx-relay-sg01.5gen.care`
   (relay). The certificate needs it, so do this first.

## 3. Run it (on the admin server, as admin)

```bash
# control node
matrix-provision node 203.0.113.10 --name sg01 --region SG --domain matrix-sg01.5gen.care
# relay stream server, reporting to the control server of its region
matrix-provision relay 203.0.113.11 --name mx-relay-sg01 --region SG \
    --domain mx-relay-sg01.5gen.care --control matrix-sg00.5gen.care
```

What it does, in order (it stops at the first step that fails):

| Step | Node | Relay |
|---|---|---|
| Preflight | SSH as root with the matrix key; Ubuntu 22.04+; 1 GB memory; 10 GB free disk; no earlier install; ports free; DNS points at the IP; the name and IP are not already in `servers.conf` | same, plus UDP 3478 |
| Firewall | ufw: 22, 80, 443 | ufw: 22, 80, 443, UDP 3478, UDP 49152-65535 |
| Install | the newest server package with `setup.sh install-node`; the Center's ticket key, KID and blocklist are read from the Center; player limited to the office | `matrix-relay` binary and unit, a new shared secret, Let's Encrypt certificate with the renewal hook, flags reporting to `--control` |
| Register | added to `servers.conf`, backup tool installed, first encrypted backup | same |
| Tests | health (status, version), TLS certificate, service enabled and running, comes back after a restart, firewall on, linked to the Center | service running, metrics, TLS certificate, a relay allocation over **UDP, TCP and TLS** with data sent through it (`relaycheck`, using a credential minted from the relay's secret), reports to its control server |

Then it prints the report (PASS / FAIL / INFO per step) and the manual
steps, and keeps both with the full log in
`/opt/matrix-admin/provision/<time>-<name>.txt`. INFO lines are steps that
can only pass after the manual part (the link to the Center, relay reports).

Fix a FAIL and test again without reinstalling:
`matrix-provision check <name>`. On a server already in service `check`
never restarts anything.

## 4. Put in service (manual)

These change production, so a person runs them, in this order:

**Control node**

1. DigitalOcean Cloud Firewall: add the droplet to the matrix firewall (SSH
   only from the admin server and the office).
2. On the Center, add the node entry the report printed to `nodes` in
   `config.json` (and a route for the region if it is new), then
   `sudo matrix-fleet check ~/new.json && sudo matrix-fleet apply ~/new.json`.
3. `sudo matrix-fleet status` on the Center: the node is `up`;
   `matrix-provision check <name>` now shows the link as PASS.
4. View a camera of the region from the office, once direct and once with
   the relay forced.

**Relay**

1. DigitalOcean Cloud Firewall: add the droplet to the relay firewall.
2. Give its control server the secret:
   `matrix-provision relay-secret <relay name> <control server name>`. It
   copies the secret by file, keeps a backup of `relay-secrets.json` and
   prints no secret. Within 15 s the relay's reports are accepted.
3. `matrix-provision check <relay name>`: "reports to its control server"
   is PASS; `/admin/statistic` on the control server lists the relay.
4. View a camera of the region from the office with the relay forced (UDP,
   TCP, TLS in `ws.html`).

`matrix-monitor` starts watching the new server on its next run (it reads
`servers.conf`), and the nightly backup includes it.

## Not covered yet

- A new Center (only `backup-restore.md` 4.3) and a second Center per
  region (R21).
- Creating the droplet and the DNS record (doctl, Route53) from the script.
- Upgrades still run from the PC with `server/deploy.sh` (R6b).
