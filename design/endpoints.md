# Matrix endpoints

Every endpoint of `matrix_cc`, by who calls it. Written from the code at commit `eb6ef23` (2026-10-27).

Both server roles run the same binary:

- **Control Center** (`matrix.5gen.care`, `setup.sh install`). It runs everything below, including its own control node.
- **Control node** (e.g. `matrix-eu00.5gen.care`, `setup.sh install-node`). It runs the control server endpoints (section 3), the common pages (section 6) and its end of the node link. It has no Center endpoints.

| Port | Where | Who reaches it |
| --- | --- | --- |
| 443 (HTTPS + WSS) | Center and every node | Everyone: cameras, apps, browsers, the pairing system |
| 8444 (private, `LINK_LISTEN`) | Center only | Control nodes only (firewall) |
| `/run/matrix/admin.sock` (Unix socket) | Center and every node | `matrix-fleet` on that machine, through sudo |
| 3478/udp + relay range | Relay hosts (violet) | Cameras and apps |
| 9100 (node_exporter) | Relay hosts | That region's control node only |

All JSON bodies are `Content-Type: application/json`, one strict object, at most 4 KB on Center calls. Unknown fields are rejected. Errors come back as `{"error": "..."}`.

---

## 1. Camera

The camera always starts at the Center and is sent to its control node from there.

### 1.1 `POST https://matrix.5gen.care/v1/challenge`

Asks the Center for a challenge and the assigned node.

```json
{"proto": 3, "device": "<device id, 43 chars>"}
```

`device` is a hint only (not authenticated). The Center uses it to keep the camera on the node it is already on.

Response `200`:

```json
{"nonce": "<base64url, 24 bytes>", "expires_in": 30, "main_server_address": "wss://matrix-eu00.5gen.care:443/"}
```

- **Node choice:** the routes for the camera's country (GeoIP) give the region; within it the least loaded linked node wins; a camera already on a healthy node of its region stays there.
- **Old cameras** (body `{}` or no `proto: 3`) get only `nonce` and `expires_in`, and keep using their configured server.

### 1.2 `POST https://matrix.5gen.care/verify`

Proves the camera holds its key and its SID, and gets a ticket for the node.

```json
{"proto": 3, "sid": "<SID, 118 chars>", "device_pub": "<base64url Ed25519 public key>",
 "nonce": "<from 1.1>", "sig": "<base64url signature>", "main_server_address": "<from 1.1>"}
```

- `sig` signs `"center-activate-v2" || nonce || SHA-256(SID) || main_server_address` with the device key.
- **Proto 2** (old cameras) omits `main_server_address` and signs the v1 message for the Center's own address.

Response `200`:

```json
{"main_server_address": "wss://matrix-eu00.5gen.care:443/", "ticket": "<JWT>", "expires_in": 60,
 "device_id": "<43 chars>", "tenant": "acme01", "role": "camera"}
```

| Code | Meaning |
| --- | --- |
| `400` | Malformed request |
| `401 SID rejected` | Bad SID, unknown tenant, blocklisted, expired challenge, wrong signature, nonce already used, SID bound to another device, or activation window passed |
| `503 verification unavailable` | Store or blocklist could not be read, or the SID was released a moment ago. Retry. |

The ticket is single-use, valid 60 s, and only for that node (its audience).

### 1.3 `wss://<node>/<device id>?ticket=<ticket>`

The camera's control connection, on the node from 1.2. The path must be exactly `/<device id>`, and the query must hold only `ticket`.

| Upgrade answer | Meaning |
| --- | --- |
| `101` | Connected |
| `401 Admission required` | Missing, bad, expired or already-used ticket |
| `503` | Admission temporarily unavailable |
| `429` | Too many connections from this address |

The server pings every 30 s. Messages the camera sends:

| Message | What the server does |
| --- | --- |
| `{"type":"register","nickname":"Front door","sid":"<own SID, optional>"}` | Stores the name (1–128 chars). `sid` must match the ticket and is shown only on a discovery tenant. |
| `{"id":"<viewer id>","type":"offer","sdp":"...","candidates":[{"url":"host:port?transport=udp","username":"...","password":"..."}]}` | Forwarded to that viewer, if the viewer's ticket is for this camera |
| `{"id":"<viewer id>","type":"error","msg":"Unauthorized"}` | Forwarded (wrong viewing password) |
| `{"type":"report","result":"connected","path":"relay","relay_offered":true,"relay_ready":true,"setup_ms":840}` | Counted for statistics, never forwarded (see 2.4) |
| `{"type":"bye"}` | The server closes the connection cleanly |
| Anything else with `"id"` | Forwarded unchanged to that viewer, within the same tenant and ticket scope |

Messages the camera receives:

| Message | From |
| --- | --- |
| `{"id":"<viewer id>","type":"request","password":"...","sid_digest":"<hex>","turn":[{"host":"turn-eu00.5gen.care","port":3478,"username":"...","password":"..."}]}` | A viewer, with fields added by the server. `sid_digest` comes from the viewer's ticket, never from the viewer. `turn` lists the relay stream servers for this session: the relay of the node's region first, then a backup from another region. With violet these are its fixed credentials; with coturn they are minted per session and valid 24 h. |
| `{"id":"<viewer id>","type":"answer","sdp":"..."}` and other viewer messages | A viewer |
| `{"type":"error","msg":"Device offline"}` | The server: the viewer this message was for is not connected |

### 1.4 `GET https://matrix.5gen.care/downloads/<MODEL>/matrix`

Firmware binary. Next to it are `matrix.sha256` and `version`. These are plain files from `/opt/matrix/downloads/` and are served by every server (section 6).

---

## 2. App (mobile app and web player)

The app asks the Center where the camera is, then connects to that camera's node.

### 2.1 `POST https://matrix.5gen.care/v1/view`

Exchanges a camera's SID for a viewer ticket.

```json
{"sid": "<camera SID>"}
```

Response `200` (sent with `Cache-Control: no-store`):

```json
{"main_server_address": "wss://matrix-eu00.5gen.care:443/", "ticket": "<JWT>", "expires_in": 60,
 "device_id": "<viewer id: random, use it as the connection path>", "camera_id": "<camera device id>",
 "tenant": "acme01", "role": "player"}
```

- **Which node:** the Center's own node if the camera is connected there, else the node that reports it online, else the node it was last sent to.
- **The ticket** is single-use, valid 60 s, scoped to this one camera and to that node.
- **Codes:** `401 SID rejected` for a bad SID, a viewer SID, an unknown tenant, a blocklisted SID or a camera never activated. `503` as in 1.2.
- **Browsers:** allowed from the same origin, or from `-view-origin` (CORS preflight `OPTIONS` supported).

### 2.2 `POST https://matrix.5gen.care/v1/resolve`

An activated viewer device looks up a camera of its own tenant by the camera's reference.

```
Authorization: Bearer <viewer ticket from /verify with a viewer SID>
```

```json
{"ref": "<camera reference, 64 hex>"}
```

Response `200`:

```json
{"found": true, "device_id": "<camera device id>", "main_server_address": "wss://..."}
```

or `{"found": false}`. Unknown, blocklisted and non-camera references all look the same.

### 2.3 `wss://<main_server_address host>/<device_id>?ticket=<ticket>`

The viewer's control connection, using `device_id` and `ticket` from 2.1. The upgrade answers are the same as in 1.3.

Messages the app sends:

| Message | Notes |
| --- | --- |
| `{"id":"<camera_id>","type":"request","password":"<viewing password>"}` | Starts a session. `password` (1–256 chars) is required and checked by the camera, not the server. |
| `{"id":"<camera_id>","type":"answer","sdp":"..."}` | Reply to the camera's offer |
| `{"type":"report", ...}` | Once per attempt (2.4) |
| `{"type":"bye"}` | Clean close |

Messages the app receives:

| Message | Notes |
| --- | --- |
| `{"id":"<camera_id>","type":"offer","sdp":"...","candidates":[{"url":"host:port?transport=udp","username":"...","password":"..."}]}` | `candidates` carries the relay stream server and its credentials for this session |
| `{"id":"<camera_id>","type":"error","msg":"Unauthorized"}` | Wrong viewing password |
| `{"type":"error","msg":"Device offline"}` | The camera is not connected to this node |

The app only ever messages its ticket's camera; any other `id` answers `Device offline`.

### 2.4 Connection report (app and camera)

Send once per viewing attempt, on the control connection:

```json
{"type":"report","result":"connected","path":"lan|direct|relay","relay_offered":true,"relay_ready":true,"setup_ms":840}
{"type":"report","result":"failed","error":"unauthorized|camera_offline|no_offer|no_answer|no_path|timeout|closed|other","relay_offered":true,"relay_ready":false}
```

The role (camera or viewer) comes from the ticket. Reports feed `matrix-fleet stats` and are never forwarded. **The web player sends them; the mobile app does not yet.**

---

## 3. Every control server on 443 (Center and nodes)

| Endpoint | Purpose |
| --- | --- |
| `wss://<host>/<id>?ticket=...` | Control connections (1.3, 2.3) |
| `GET /` | Plain text `OK` (no page) |
| `GET /demo/<tenant>` | Web player (`ws.html`), only when `-discovery-tenant <tenant>` is set; limited to `-player-allow` IPs when that is set (Center: `/demo/test01`, `115.78.12.186`) |
| `GET /api/devices[?type=producer]` | Cameras online in the discovery tenant: `{"count":N,"devices":[{"id","nickname","tenant","connected_at","path","type","rtt","sid"}]}`. Only with `-discovery-tenant` or `-demo-tenant`; limited by `-player-allow`. |
| `GET /api/health` | `{"status":"ok","clients":N,"version":"2.1.1","ticket_admission":true}` |
| `GET /downloads/<path>` | Files under `/opt/matrix/downloads/` (firmware) |
| `GET /v1/demo/resolve?ref=<64 hex>` | Center only, only with `-demo-tenant` (development): unauthenticated 2.2 for the demo tenant |

**Limits** apply per source address (IPv6: per /64), on by default:

| Kind | Default |
| --- | --- |
| Center calls (sections 1, 2, 4.1) | 20/s, burst 200 |
| New control connections | 300/min |
| Other requests | 50/s, burst 200 |
| Open connections | 1000 |

Over a limit the answer is `429` with `Retry-After: 1`. Change the limits with `-rate-api`, `-rate-connect`, `-rate-web` and `-conns-per-source`; skip trusted addresses with `-rate-exempt`.

---

## 4. Other servers

### 4.1 Pairing system → Center (443)

Exists only when the Center has an admin token (`/opt/matrix/cfg/sid/admin.token`).

```
Authorization: Bearer <admin token>
```

**`POST /v1/admin/status`** with `{"sid_digest": "<64 hex>"}` returns one of:

- `{"state":"unbound"}`
- `{"state":"bound","tenant":"acme01","role":"camera"}`
- `{"state":"released","tenant":"...","role":"...","bindable_at":<unix>}`

**`POST /v1/admin/release`** with `{"sid_digest": "<64 hex>"}`:

- Ends the current device's seat so the SID can go to another device. The old device is blocklisted until its tickets expire.
- Response: `{"released":true,"changed":true,"bindable_at":<unix>}`.
- `404` if the SID was never bound.

Without the token: `401`. With no token configured: `404`.

### 4.2 Control node ↔ Center link (`wss://<Center private IP>:8444/v1/link`)

The node opens the connection and keeps it open. TLS is checked against the Center's name (`-link-server-name`). The full protocol is in `src/go/internal/link/link.go`.

| Step | Message |
| --- | --- |
| Center → node | `{"type":"challenge","nonce":"<32 bytes>"}` |
| node → Center, within 5 s, ≤ 1 KB | `{"type":"login","address":"wss://<node>:443/","sig":"..."}`. Ed25519 with the node's key over `"matrix-node-link-v1" ‖ nonce ‖ Center name ‖ address`, checked against `public_key` in `config.json`. |
| node → Center | `{"type":"hello","cameras":[...],"clients":N,"stats":{...},"relays":[...],"version":N}` |
| Center → node | `{"type":"welcome"}`. The node counts itself linked only after this. A refused node is simply disconnected. |
| node → Center | `{"type":"camera","id":"<device id>","online":true}`, sent as cameras come and go |
| node → Center, every 60 s | `{"type":"stats","clients":N,"stats":{...},"relays":[...]}` |
| Center → node | `{"type":"config","version":N,"config":"<base64url>","sig":"..."}`, signed with the Center ticket key over `"matrix-config-v1" ‖ version ‖ config` |
| node → Center | `{"type":"ack","version":N}` or `{"type":"config_error","version":N,"error":"..."}` |

- **Liveness:** the Center pings every 10 s, and either side drops a link silent for 35 s.
- **Node accepts a config only** if the signature is valid and the version is newer than the one it runs. It saves it to `cfg/sid/node-config.json`.
- **One link per node:** a new one replaces the old. At most 32 connections may be waiting to log in.

### 4.3 Control node → relay hosts

- **Health:** a STUN binding request to `<relay host>:<port>/udp` every 15 s, with no credentials.
- **Load:** `GET <metrics>` every 15 s. node_exporter gives `node_network_{receive,transmit}_bytes_total`; coturn would give `turn_total_*`.

Only the relays of the node's own region are measured.

---

## 5. Operator (no network port)

`sudo matrix-fleet ...` on the machine itself talks to `/run/matrix/admin.sock` (mode 0600, service user):

| Socket call | `matrix-fleet` command | Where |
| --- | --- | --- |
| `GET /v1/status` | `status` (nodes, relays, config versions; on a node: its link) | Center, node |
| `GET /v1/stats?days=N` (1–31) | `stats [DAYS]` | Center |
| `POST /v1/reload` | done by `apply` and `rollback` | Center |
| `POST /v1/moves` (body: a config) | done by `check`: online cameras that would move | Center |
| — | `check FILE`, `apply FILE`, `rollback [VERSION]`: edit `config.json` | Center |
| — | `keygen FILE`, `pubkey FILE`, `blocklist-check FILE` | any |

`-json` prints the raw answer. `systemctl reload matrix` (SIGHUP) also reloads `config.json` on the Center.

---

## 6. Removed (do not use)

| Endpoint | Replaced by |
| --- | --- |
| `GET /v1/admin/stats` (443) | `matrix-fleet stats` on the Center |
| `POST /v1/node/report` (443, bearer token) | The node link (4.2) |
