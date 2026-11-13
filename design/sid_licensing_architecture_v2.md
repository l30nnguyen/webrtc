# SID licensing and camera viewing architecture

Status: **current architecture and integration contract**, checked against
the code on 2026-11-04: control server 2.6.0 (`9808bcf`), camera 2.1.4
(`libdatachannel_c14` `062c4228`), relay stream server `matrix-relay` 1.2.0.
Later changes (server 2.6.6, camera 2.1.6) are listed under *History*.
This is the single authoritative SID contract. Older 107-character SID
details (`sid_licensing_v2_changelog.xlsx`) are historical. LicenseServer is a
separate future entitlement service, not part of the `matrix_cc` binary; it is
planned on the AWS machine `webrtc.5gen.care`, which will hold the keys.

User-facing names: the signaling server is the **control server**, TURN is the
**relay stream server**.

## Product and deployment model

One binary, `matrix_cc` (release packages name it `matrix_cc_<version>`), runs
every server role on one HTTPS/WSS listener:

| Role | Host (2026-11-04) | Installed with | Serves |
| --- | --- | --- | --- |
| Control Center | `matrix.5gen.care`, DigitalOcean nyc3 | `setup.sh install` | Center APIs (`/v1/challenge`, `/verify`, `/v1/view`, `/v1/resolve`, pairing-system admin), its own control node, the node link (`/v1/link`), downloads |
| Control node | `matrix-sg00.5gen.care`, DigitalOcean sgp1 | `setup.sh install-node` | Camera and viewer WSS, relay reports, statistics, downloads; links to Center |
| Relay stream server | `mx-relay-us00` (DO nyc3, reports to the Center) and `mx-relay-sg00` (DO sgp1, reports to sg00), `matrix-relay` 1.2.0 since 2026-11-04; violet stopped | manual (`server/src/relay/deploy/`, release `INSTALL.md` section 3) | TURN over UDP 3478, TCP 80 and TLS 443  |

`setup.sh` runs the server on port 443 under systemd (`matrix.service`, user
`matrix`); the CLI default `-port` is 8000. `-center` turns the Center APIs on
in the same process. There is no separate Center executable or port.
Endpoint by endpoint: [endpoints.md](endpoints.md).

## Credential model and trust boundaries

The SID is the canonical **118-character**, unpadded Base64URL Ed25519-signed
value. The 88 decoded bytes; the signature covers exactly the first 24 bytes:

| Byte offsets | Size | Field | Encoding / rule |
| --- | ---: | --- | --- |
| 0 | 1 | Version | Unsigned byte; current value `2` |
| 1–4 | 4 | Issued time | Unsigned Unix seconds, big-endian |
| 5–6 | 2 | Flags | Big-endian; exactly one of publisher `0x0001` or viewer `0x0002` |
| 7–14 | 8 | Tenant | 6–8 lowercase ASCII letters/digits, right-padded with zero bytes |
| 15–23 | 9 | Nonce | Fresh cryptographically random bytes |
| 24–87 | 64 | Ed25519 signature | Full signature over bytes 0–23 |

The SID is a bearer licence credential. It carries tenant and role and does
not prove device identity. **One SID is bound to one device key at a time**;
the seat moves to another device by a release from the pairing system or, for
cameras, by takeover after the holder has been offline for 3 minutes (see
*SID pool*).

Viewing uses two shared credentials:

- The camera's activated `publisher` SID obtains a short-lived, camera-scoped
  player ticket from Center.
- The camera password, which the viewer sends in the first WSS request and the
  camera checks before any peer connection or media.

There is no viewer account, pair-grant database or ownership/password-change
API. Pairing is external credential delivery. Anyone with both the camera SID
and the password can view while the SID is accepted.

| Secret/material | Trusted holder and use | Must not be sent to |
| --- | --- | --- |
| SID issuer private key | Controlled issuer (`sidctl`); signs SIDs | Cameras, Center, nodes, viewers, repository |
| SID issuer public key | Center verifies SIDs; the raw 32-byte key is built into camera binaries | Not secret; provision through the trusted build |
| Camera SID | Pairing system and the paired camera; the app presents it to Center | Logs, URLs, analytics, crash reports |
| Center ticket private key | Center only (`cfg/sid/center-ticket.key.pem`): signs tickets and pushed configurations | Nodes, cameras, browsers, repository |
| Center ticket public key | Every node: verifies tickets, Center's link challenge and pushed configurations | Not secret; pinned with the key ID (`-kid`) |
| Node link key | Each node's own `cfg/sid/node.key`; its public key is in Center's `config.json` | Never copied off the node |
| Admin token | Center (`cfg/sid/admin.token`) and the pairing system | Cameras, apps, logs |
| Relay shared secrets | `cfg/relay-secrets.json` on the node and the relay's `secrets.json` | Cameras and apps (they get per-session credentials) |
| Camera password | Viewer and camera; forwarded in the first WSS request | Center, URLs, logs, analytics, crash reports |
| Password verifier | Camera `auth/password.phc`, mode 0600 | Viewer, Center, logs |
| Device private key | Camera `auth/device.key`, mode 0600 | Center and other devices |
| Activation store and tenant registry | Center `cfg/sid/` | Public endpoints, except the minimum claims |

Never log or persist a raw SID, password, ticket or signaling request body.
Center needs the raw SID only to verify it and derives its digest (SHA-256 of
the 118 ASCII characters) for the activation store and ticket claims. The
control server takes the camera's SID digest from the verified viewer ticket
and overwrites any digest a browser sends. The camera compares it with its
own SID digest before checking the password.

## Activation and viewer flows

### Camera activation

A camera verifies its SID locally (format, signature, tenant, role, minimum
issue time), then on every connection attempt:

1. `POST /v1/challenge` with `{"proto":3,"device":"<device id>"}`. Center
   returns a single-use 24-byte nonce valid for 30 s and the **assigned node**
   (`main_server_address`), chosen by the camera's country (GeoIP), the
   region's routes and node load; a camera already on a healthy node of its
   region stays there.
2. The camera accepts the node only on a domain built into the binary
   (`MATRIX_NODE_DOMAINS`, default `5gen.care`) or its configured host.
3. `POST /verify` with `{"proto":3,"sid","device_pub","nonce","sig","main_server_address"}`.
   `sig` is the device's Ed25519 signature over
   `"center-activate-v2" || nonce || SHA-256(SID) || node`. Center checks the
   SID, the proof, the binding (next section) and the tenant's activation
   window, then returns the device ID, tenant, role, the node and a ticket
   whose audience is that node (60 s, single use).
4. The camera opens `wss://<node>/<device id>?ticket=…`. TLS, Center or node
   check failures fail closed; there is no fallback address.

Old cameras send proto 2 (`"center-activate-v1"` over the Center's own
address) and are served on their configured server.

The camera device ID is Base64URL(SHA-256(device public key)). On a refusal
the camera logs Center's status and reason (2.1.2 and later), for example
`Center answered 503 …: retry; the SID was just released, or the camera
holding it went offline less than 3 minutes ago`, and retries after 25–35 s.

### SID pool, release and takeover

SIDs are issued in batches (`sidctl batch`) into a **pool owned by the
external pairing system**. When a camera is paired (outside this service) the
pairing system takes an available SID and gives it to the camera with a fresh
password. When it is unpaired or replaced, the SID goes back to the pool.

| Area | As built |
| --- | --- |
| Binding | States `unbound → bound(device) → released`. A new key binds while unbound or released (after a 3 s grace) |
| Release | `POST /v1/admin/release {"sid_digest"}` with `Authorization: Bearer <admin token>`, by the pairing system only. Writes `device:<id> <expiry>` to the blocklist (fsynced), then marks the SID released. Twice is a no-op; unknown digest 404; a blocklist write failure answers 503 and leaves the SID bound. `POST /v1/admin/status` answers `unbound`, `bound` or `released` with `bindable_at` |
| Takeover (2.5.0) | A camera SID bound to another key passes to the new camera once the holder has been offline **on every node for 3 minutes**: the camera was reset and paired again with a new key, or the pairing system gave the SID to another camera without a release. Holder online: 401 and an `ALERT` log. Offline for less, or any node not linked (the holder may be on it): 503, retry. On takeover the old key is revoked like a release, `sid_takeover` is counted and a `camera`/`takeover` event is logged. Needs the fleet configuration (`-config-file`). A tenant with `"no_takeover": true` keeps the strict rule. Viewer SIDs are never taken over |
| Former holders | A device that held a SID is refused for it afterwards (Center keeps up to 1,000 former device IDs per SID), so a lost or reset camera cannot race back in |
| Cut-off of the old device | Nodes drop connections whose device is revoked, or whose digest or tenant is blocked, on the 30 s sweep; old tickets are refused at the ticket check |
| Old viewers | A viewer who once had the SID can still try passwords, so every pairing must set a **new random password** |
| Camera firmware | No SID at manufacture. The firmware writes the SID and password as a `pair` file; an unpaired camera idles. Unpairing on the camera is not built, which is why takeover exists |
| Revocation | The blocklist by digest is separate from release: a blocklisted SID is dead, a released SID is reusable |

The activation store is an append-only JSONL file; each line is the full
latest state of one SID (`status`, `released_at`, `former`), fsynced. Bind,
release and takeover are compare-and-set under the store lock, so two
requests can never both win one SID.

Decisions (2026-10-19, takeover added 2026-11-04):

- **Expiry.** A SID can be bound, the first time or again, only before
  `issued + 3 years` (per tenant: `activation_window_days`). A bound SID keeps
  working. A SID left unbound past that date is dead.
- **No long quarantine.** Safety does not depend on the old device noticing
  anything: its key is refused for that SID for good, a bind is refused while
  another online key holds the SID, viewer tickets are scoped to the new device
  ID, and every pairing sets a new password.
- **3 s grace after a release** only orders the steps (the revocation is
  written before any new bind).
- **Revocation entries expire** after the ticket lifetime plus the sweep and
  a margin (about two minutes); the permanent refusal lives in Center's history.
- **Takeover risk.** Anyone holding a copy of the SID text can take it over
  from a camera that is off for more than 3 minutes, and the owner's key is
  then refused. See [roll-out.md](roll-out.md) R1 for the mitigations to
  decide before going past testers.

### Viewer access

1. The app calls `POST /v1/view` with `{"sid":"<camera SID>"}`.
2. Center checks the SID, tenant, activation and block status; the camera
   identity comes from the activation store, never from the caller. It
   returns a player ticket (60 s, single use, scoped to the camera and the
   node), the camera ID, a random viewer ID and the node the camera is on now
   (Center's own node, else the node reporting it online, else the node it
   was last sent to).
3. The app connects to `wss://<node>/<viewer id>?ticket=…` and sends
   `{"id":"<camera_id>","type":"request","password":"<SHA-256 hex>"}`. The node
   adds the SID digest from the ticket and the relay stream servers for the
   session, and forwards it to the camera.
4. The camera checks the digest and the password, then sends the offer with
   the relay credentials (`candidates`, with `urls` for UDP, TCP and TLS).

The ticket is a bearer credential: single use limits replay but does not stop
theft before first use, and it is not bound to the viewer device.

## Password provisioning and rotation

The camera's `auth/` folder (mode 0700) holds `device.key` and `password.phc`
(mode 0600); the plaintext password is never stored. On the first run with no
auth state, the camera creates a device key and a verifier for the default
password `5genmatrix` (cameras initialised before 2026-10-23 keep
`5genwebrtc`). The firmware should set the real password at pairing (the
`pair` file) before relying on the camera. If `device.key` exists but
`password.phc` is missing or malformed, startup fails closed.

The credential the app sends, and that the camera hashes, is the lowercase hex
SHA-256 of the password the user types (64 characters). The verifier is
PBKDF2-HMAC-SHA256 with a random 16-byte salt, a 32-byte tag and the iteration
count stored in it (`i=`, 4,000 to 1,000,000; new verifiers use 8,000, about
0.4 s on the target camera). `--set-password` (from stdin) and
`camera-password --file PATH` write a new verifier atomically. The camera
rereads the verifier at least once a second; a changed, missing or invalid
verifier closes active viewers. Failed attempts are throttled camera-wide.

Who may change a password, and delivering it to viewers, belongs to the
external pairing/product system.

## Build, configure and run

### Camera

Build with the trusted raw SID public key (64 hex characters) and the minimum
issue time; there is no unprovisioned build. For the HCX130 and HCX330L
(ARM, the same C++ build): `libdatachannel_c14/compile.sh` (toolchain
`anykav330.cmake`, binary `build/examples/streamer/matrix_<version>`),
published to `https://matrix.5gen.care/downloads/HCX130/matrix` and
`.../HCX330L/matrix` with `publish_firmware.sh MODEL` (once per model). The
old GIRAFFE_HD cameras, which can't run C++, use the C client `matrix_c`
(toolchain anykav200), published to `.../GIRAFFE_HD/matrix`. Cameras
download it to `/tmp/matrix` at boot.

The camera's only run-time option is `--config FOLDER` (also `-h`, `-V`,
`--set-password`). The folder holds:

- `matrix.ini` (`key=value`; a legacy `webrtc.ini` is renamed once), with
  the keys `nickname` (default `CAMERA`), `host` and `port` (default
  `matrix.5gen.care`, 443), `audio`, `video`, `video2`, `chunksize`, `mp4`,
  `talkback-port`, `talkback-chunksize`, `timeout`, `entropy-bits`,
  `verbose`, `ca-file`, `reqIP`, `reqPort`
- `sid` (mode 0600, written from `pair`)
- `auth/`

A media source (`audio`, `video`, `video2`) is a UNIX socket path, a UDP port
(`8553` listens on 127.0.0.1; `IPv4:PORT`, e.g. `0.0.0.0:8553`, picks the
address; 2.1.4), or an `rtsp://` address. Exit status 78 means a bad config
folder or a malformed `matrix.ini`. MbedTLS builds trust only the embedded
ISRG Root X1 unless `ca-file` is set.

### Control server

```sh
cd server
release/make_release.sh            # dist/matrix-server-<version>-<commit>.tar.gz
sudo ./setup.sh install ...        # on a new Center (release/README.md)
sudo ./setup.sh install-node ...   # on a new node
./deploy.sh prod | sg              # upgrade the Center / sg00 from this PC
```

`setup.sh` writes `/etc/matrix/matrix.conf` and the systemd unit with the
flags (`-https -center -port 443 -kid … -config-file … -relay-dir …
-statistic-dir … -event-dir … -admin-socket /run/matrix/admin.sock`, and
more). New droplets: release folder `server/INSTALL.md`.

Never enable `-demo-tenant` in production. `-discovery-tenant test01` (camera
list and `/demo/test01` player) is for the demo and limited to the office IP
with `-player-allow`; turn it off before the official production launch.

## Issuance and key rotation

Use `sidctl` (`server/src/control/cmd/sidctl`) on a controlled machine:
`keygen` for the SID issuer and Center ticket key pairs, `issue` for one SID,
`batch` for a pool export (CSV/XLSX with raw SIDs, plus a digest manifest),
`verify` and `status`. The private JSONL audit keeps digests, not raw SIDs.
Raw SID files are secrets; remove them after handing them to the pairing
system.

The SID issuer key and the Center ticket key are independent. SID v2 has no
key ID, so rotating the issuer key means new camera binaries and a migration
plan. Rotating the Center ticket key needs every node to trust the new key and
key ID before Center signs with it.

## Fleet: Center, nodes and relays (built)

**Why Center decides.** A camera and its viewers must reach the same control
server process. GeoDNS or a regional load balancer routes each client by its
own location, so a viewer in Vietnam and a camera in the US would meet on
different nodes. Center knows where every camera is, so it sends cameras to a
node of their region and viewers to the camera's node.

| Piece | As built |
| --- | --- |
| `config.json` (Center) | `nodes` (name, address, region, capacity, link `public_key`, optional `link_from`), `routes` (ISO country → region, `*` default), optional `relays`. Changed only with `matrix-fleet check / apply / rollback`; every version kept in `cfg/config.d/`; reloaded without a restart and pushed to the nodes |
| Node link | The node opens `wss://matrix.5gen.care/v1/link` on 443 (nyc3 and sgp1 share no private network). Center answers 404 unless the request comes from the node's own IP with a fresh signed header; Center proves itself with its ticket key, the node logs in with its own key. Then `hello`, `camera` changes, `stats` every 60 s (counters, relays, server version and machine load since 2.6.0), signed `config` pushes and `ack`. 5 failures block an IP for an hour |
| Center down | Nodes keep their last configuration and keep serving connected cameras and viewers; only new activations wait. Planned (R21): a Center in each region (US, Asia, later EU) with Route53 latency routing and one activation writer, so that one Center down stops nothing except first-time binds while the writer is down (server_operation.md section 1.4) |
| Relays | `matrix-relay` reports to its region's node (`POST /v1/relay/report`, HMAC-signed) every 15 s: addresses, load, and since 1.2.0 its machine load. The node hands out the least loaded healthy relay that reports to it, with per-session credentials (24 h); it probes UDP, TCP and TLS every 15 s. A backup from another region is added only for relays listed in `config.json`, so with one reporting relay per region there is no backup (roll-out.md R12). Without reporting relays it uses `relays` in config.json (violet, fixed credentials) or `cfg/turn.conf` |
| Limits | Per source address on 443: 20 Center calls/s (burst 200), 300 new connections/min, 50 other requests/s (burst 200), 1,000 open connections; 429 with `Retry-After: 1` |
| Operations | `matrix-fleet status` (nodes and relays: state, cameras, clients, load, CPU, memory, disk, network, version, config version), `matrix-fleet stats [DAYS]`, `/admin/statistic` per region (30-minute rows for a year, connection log for 30 days, office IP only) |

**Not built yet** (see [roll-out.md](roll-out.md)):
- Several Center instances (a shared activation store, nonces, replay and
  presence).
- Cross-node signaling, where a camera and its viewer are on different nodes.
- Graceful shutdown.
- Alerting.
- Pushing revocations over the link.

Capacity and cost per fleet size: [capacity.md](capacity.md).

## Connection analytics

After each viewing attempt the camera and the viewer each send one report on
their control connection. The server counts it, logs it in the connection log
and never forwards it:

```json
{"type":"report","result":"connected","path":"relay","relay_protocol":"tcp","relay_offered":true,"relay_ready":true,"setup_ms":840}
{"type":"report","result":"failed","error":"no_path","relay_offered":true,"relay_ready":false}
```

| Field | Values |
| --- | --- |
| `result` | `connected` or `failed` |
| `path` (connected) | `lan`, `direct`, `relay` |
| `relay_protocol` (connected through a relay) | `udp`, `tcp`, `tls` |
| `error` (failed) | `unauthorized`, `camera_offline`, `no_offer`, `no_answer` (camera side: the viewer never answered), `no_path`, `timeout`, `closed`, `other` |
| `relay_offered` / `relay_ready` | a relay was offered / it gave an address |
| `setup_ms` | request to connected |
| `version` (optional, 2.6.6) | the reporter's software version, e.g. the web player's `2.6.6`; shown in the connection log |

The role comes from the ticket. The web player and the camera send reports;
the mobile app will not (decided 2026-11-05). From 2.7.0 a session counts as
successful when the camera reports its data channel open and a ping
answered, and the camera adds stream quality from the viewer's RTCP feedback
([roll-out.md](roll-out.md) phase 1, step 4).

## Future entitlement and revocation target

LicenseServer is a future, separate entitlement service. The target:
- Center submits SID digests for an online entitlement check, without putting
  LicenseServer's database in the hot path.
- If LicenseServer is down, a valid SID is admitted and its check is queued.
- Digests found invalid are published to a replicated blocklist. If the
  blocklist cannot be checked, admission fails closed.
- Active sessions are rechecked at least every 60 minutes.

None of this is built. Today the blocklist is a file on each server
(`setup.sh blocklist FILE`), reread without a restart.

## Known limits

- **Bearer credentials.** A ticket can be stolen before its first use, and the
  password check is a shared password, not a user's identity.
- **The Center is a single instance.** Its state is not backed up off the host
  yet (roll-out R3).
- **The blocklist is local.** It is copied to every server by hand and is not
  a global revocation.
- **Takeover can be abused.** A copied SID can take over from an offline camera
  (roll-out R1).
- **Not yet measured:**
  - restoring active media sessions after a relay fails
  - physical ARM behaviour under load
  - control-server capacity on a real droplet (the relay was measured on
    2026-11-04: about 67 relayed sessions per 1 GB droplet)

## History

- 2026-10-13/14: first dev deployments on `webrtc.5gen.care:8443`, PM2. The
  verification logs, hashes and PIDs are in git history.
- 2026-10-19: SID pool and release.
- 2026-10-26/27: multi-region phases 1–2, then the fleet control plane; the
  Center moved to DigitalOcean.
- 2026-11-03: `matrix-relay` with relay reports, statistics and the
  connection log.
- 2026-11-04: SID takeover (2.5.0) and machine load (2.6.0); `matrix-relay`
  in production on `mx-relay-us00` and `mx-relay-sg00`.
- 2026-11-05: violet and `cfg/turn.conf` retired from the fleet; camera 2.1.5
  fixes the relay login and 2.1.6 the ICE role check for TCP/TLS-relayed
  viewers ([roll-out.md](roll-out.md) R15, R16); server 2.6.3 (player relay
  fallback after 5 s).
- 2026-11-06: server 2.6.5 and 2.6.6: the player answers without waiting
  for address gathering to finish, is not cached, and reports its version;
  relays allow 8000 allocations ([roll-out.md](roll-out.md) R18–R20).
