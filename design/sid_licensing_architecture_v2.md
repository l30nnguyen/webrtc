# SID licensing and camera viewing architecture

Status: **Current architecture and integration contract** (2026-10-13). This is the single authoritative SID contract. It consolidates the current format and camera-password viewing flow with applicable issuance, deployment, and future entitlement/revocation planning. Older 107-character SID details are historical; LicenseServer remains a separate future entitlement service, not a component of the signaling-server binary.

## Product and deployment model

Center is a normal signaling-server instance behind the load balancer. It uses the same `signaling-server` binary as other instances. Its single TLS listener is configured on port **8443** in the current deployment and serves `POST /v1/challenge`, `POST /verify`, `POST /v1/view`, WebSocket signaling (WSS), the player page, health, and other existing HTTP routes. The CLI's default listen port remains 8000, so deployments must pass `-port 8443`. There is no standalone Center executable, service, or port 9443. The `-center` option enables Center APIs on that server process; it does not select a different binary or listener.

The load balancer presents the stable HTTPS/WSS origin and forwards traffic to signaling-server instances. A Center response returns the configured MainServer WSS root; the camera and viewer use that address and the ticket audience must match it exactly. Deployments may direct clients to the same instance or an assigned signaling instance, but cross-node camera/player routing requires fleet routing and shared registry work described below; this document does not claim that capability is deployed.

## Credential model and trust boundaries

The SID remains the canonical **118-character**, unpadded Base64URL Ed25519-signed value. The 88 decoded bytes are laid out as follows; the signature covers exactly the first 24 payload bytes:

| Byte offsets | Size | Field | Encoding / rule |
| --- | ---: | --- | --- |
| 0 | 1 | Version | Unsigned byte; current value `2` |
| 1–4 | 4 | Issued time | Unsigned Unix seconds, big-endian |
| 5–6 | 2 | Flags | Big-endian; exactly one of publisher `0x0001` or viewer `0x0002` |
| 7–14 | 8 | Tenant | 6–8 lowercase ASCII letters/digits, right-padded with zero bytes |
| 15–23 | 9 | Nonce | Fresh cryptographically random bytes |
| 24–87 | 64 | Ed25519 signature | Full signature over bytes 0–23 |

The 24-byte payload plus 64-byte signature is 88 bytes; canonical unpadded Base64URL encodes it in 118 characters. The SID is a bearer license credential. It is tenant/role-bearing and does not itself prove device identity. Each SID is single-seat: first activation binds it to a camera's device key. Replacing a camera requires a new SID; there is no transfer workflow.

Viewing uses two shared credentials:

- The camera's activated `publisher` SID obtains a short-lived, camera-scoped player ticket from Center.
- A separate camera password is supplied by the viewer in the first WSS signaling request and checked by the camera before peer-connection or media setup.

There is no viewer SID, viewer account, camera-ref lookup, pair-grant database, or ownership/password-change API in this approved flow. Pairing is external credential delivery. Anyone with both the camera SID and password can view while the SID remains accepted; anyone with the password can try viewing a known camera but still needs a scoped ticket derived from an activated camera SID. These are shared credentials, not individual user identity or proof-of-possession.

| Secret/material | Trusted holder and use | Must not be sent to |
| --- | --- | --- |
| SID issuer private key | Controlled issuer; signs SID payloads | Cameras, Center, MainServer, viewers, repository |
| SID issuer public key | Center verifies SID; raw 32-byte key is embedded in provisioned camera builds | Not secret; provision through trusted build/key-management workflow |
| Camera SID | Issuer and provisioned camera; viewer presents it to Center for activation/view flow | Logs, URLs, analytics, crash reports |
| Center ticket private key | Signaling-server instances that issue Center tickets | Camera, browser, repository |
| Center ticket public key | Signaling-server admission verifier(s) | Not secret; pin the expected algorithm/key ID and audience |
| Camera password | Viewer and camera, forwarded in initial WSS request through MainServer | Center, URLs, logs, analytics, crash reports |
| Password PHC verifier | Camera persistent storage, mode 0600 | Viewer, Center, general logs |
| Activation/device private key | Camera persistent protected storage | Center and other devices |
| Activation record and tenant registry | Center's configured persistence | Public endpoints except minimum necessary claims |

Never log or persist raw SID, password, ticket, or signaling request body. Center needs the raw SID transiently to verify it and derives its digest for activation and ticket claims. MainServer derives the camera SID digest from the verified ticket and overwrites any browser-supplied digest. The camera compares that trusted digest with its locally verified SID digest before password validation.

## Activation and viewer flows

### Camera activation

A provisioned camera verifies its SID locally (canonical format, signature, tenant, role, and minimum issue time), then proves device-key possession to Center for each connection attempt:

1. Camera calls `POST /v1/challenge`; Center returns a fresh 24-byte, single-use nonce valid for 30 seconds.
2. Camera calls `POST /verify` with `{"proto":2,"sid":"…","device_pub":"…","nonce":"…","sig":"…"}`. `sig` is the device's Ed25519 proof over `"center-activate-v1" || nonce || SHA-256(SID) || main-origin`; the SID digest is SHA-256 of the exact canonical 118 ASCII SID characters.
3. Center verifies the SID and proof, binds the first device key to the SID, checks tenant and activation policy, then returns device identity, tenant/role, exact MainServer origin, and a short-lived ticket (60 seconds by default, at most five minutes).
4. Camera opens WSS at that exact origin and device path with the ticket. TLS/certificate, Center, or origin validation failures fail closed in Center mode; there is no fallback to a fixed URL.

The camera device ID is Base64URL(SHA-256(device public key)). The SID does not contain the device ID. While a SID is assigned it is bound to that device public key; do not silently replace a damaged existing key. Under the pool model below the binding can end by an explicit release.

### SID pool and re-assignment (decided and implemented 2026-10-19)

A SID is not tied to one camera for life. SIDs are issued in batches into a **pool** owned by the external pairing system. When a device is paired (outside this service) the pairing system takes one *available* SID from the pool and gives it to the device together with a fresh password. The rule is unchanged: **one SID is bound to one device at a time**. When the device is unpaired or replaced, the SID is released and returns to the pool.

What changed (Center, nodes and `sidctl` are done; the pairing system itself is outside this service):

| Area | Today | Needed |
| --- | --- | --- |
| Binding | The first device key binds forever; a second key is rejected with an `ALERT` | States `unbound → bound(device_id) → released`. A new key may bind only while unbound or released. A device that held the SID before is refused for it afterwards, so a lost camera cannot race back in |
| Release | None | An authenticated Center operation `release <sid digest>`, callable only by the pairing system (token plus TLS; never by a device or player), audited. `sidctl status` shows bound, released and history |
| Cut-off of the old device | n/a | On release the old device's live WebSocket and any ticket still valid (up to 60 s) must stop working. Add `device:<id>` lines to the blocklist file the nodes already reload; nodes check the ticket `sub` and drop matching connections |
| Old viewers | `camera_ref = SHA-256("camera-ref-v1" \|\| SID)` is a permanent handle, and a viewer who once got the SID can still look the camera up | After re-assignment an old viewer can still resolve the SID and try passwords. Every pairing must therefore set a **new random password** (never the default); optionally derive the handle per binding later |
| Camera firmware | SID flashed at the factory | No SID at manufacture. SID and password are delivered at pairing as a `pair` file written by the firmware; an unpaired `webrtc` idles. Unpairing (wiping `sid` and `auth/password.phc`, keeping `device.key`) is not built yet |
| Batch flow | `sidctl batch` → factory partner → firmware | `sidctl batch` → pairing system's pool. The factory sheet is no longer needed |
| Pool ownership | n/a | The pairing system owns available/assigned. Center owns the truth of bound/released and refuses a bind to a SID currently bound to another key (safety net) |
| Revocation | Blocklist by digest (permanent) | Unchanged and separate from release: a blocklisted SID is dead, a released SID is reusable |

As built:

- `POST /v1/admin/status` and `POST /v1/admin/release` (body `{"sid_digest":"<64 hex>"}`, `Authorization: Bearer <token>` from `-admin-token-file`; no token means the routes do not exist). Status answers `unbound`, `bound`, or `released` with `bindable_at`. Release writes `device:<id> <expiry>` to the blocklist, fsynced, and only then marks the SID released; releasing twice is a no-op, an unknown digest is 404, and a blocklist that cannot be written fails the release with 503 and leaves the SID bound.
- The activation store is still the append-only JSONL file; each line is the full latest state of one SID (`status`, `released_at`, `former`). `Bind` and `Release` are one step under the store lock, so concurrent binds cannot both win.
- `/verify` for a released SID: a former holder gets 401 and an `ALERT` log; anyone inside the 3-second grace gets 503 (retry); after the grace another device binds. The 3-year window is checked on every bind. `/v1/view` and camera lookup treat a released SID as not found.
- Nodes drop open connections whose device is revoked, or whose digest or tenant is blocked, on the 30-second sweep; new connections with an old ticket are refused at the ticket check.
- `sidctl status` lists released SIDs as `released`.
- Limits: a device that held a SID cannot bind it again, so the pairing system must hand a returning camera a different SID; the admin routes have no rate limit; and the grace and the revocation lifetime are constants.

Decisions on expiry and reuse (2026-10-19):

- **Expiry.** The 3-year window applies to **every bind**, counted from the SID's issue time: a SID can be bound (first time or again after a release) only before `issued + 3 years`. A bound SID keeps working. A SID that sits unbound in the pool past that date is dead and must not be handed out; the pairing system should drop it and order new batches ahead of time.
- **Reuse after release needs no long quarantine.** Devices are many and switch on and off at any time, so safety cannot depend on a device noticing the release. It rests on Center rules that hold whether or not the old device is online: (1) the old device's key is refused for that SID forever (Center keeps the history), so it cannot bind again before or after the new device; (2) a new bind is refused while the SID is bound to another key; (3) viewer tickets are scoped to the new device's ID, so the old device is never addressed; (4) every pairing sets a new random password.
- **Only a few seconds of grace.** A released SID becomes bindable **3 seconds** after the release (enforced by Center, not by the pairing system). The delay only orders the steps: the release and its `device:<id>` revocation are written and synced first, so a bind can never overtake them. It does not have to cover the old device's in-flight ticket (up to 60 s) or the node dropping its connection (up to the 30 s sweep): a ticket or connection of the old device carries the old device ID, and viewers are scoped to the new device ID, so the old device can neither be addressed nor take the new device's place. It only stays connected, unreachable, until the node drops it. A longer delay would protect only against a mistaken release, which is a product choice, not a security need.
- **Revocation entries expire.** A released device appears in the blocklist as `device:<id> <expires-unix>` with an expiry after the ticket lifetime plus the node sweep and a margin (about two minutes). The node drops the live connection once when it reloads, and the permanent refusal lives in Center's history, so the blocklist file does not grow with every release in a large fleet.
- **Atomic release and bind.** Release and bind are compare-and-set on the SID's record in the activation store, so two pairing requests can never both succeed for one SID.

### Viewer access

1. Viewer calls Center `POST /v1/view` over HTTPS with the activated camera publisher SID:

   ```json
   {"sid":"<activated camera publisher SID>"}
   ```

2. Center verifies SID, tenant, activation, and applicable block status; it derives camera identity from trusted activation data, never caller-supplied tenant or camera ID. It returns a short-lived player ticket, camera ID, ephemeral viewer device ID, and MainServer address. Ticket claims include the camera ID (`cid`), camera SID digest, tenant (`tid`), ephemeral viewer subject, issuer, audience, issue/expiry time, and unique ticket ID.
3. Viewer connects to the returned WSS origin/path using the ticket. MainServer validates signature, configured key ID/algorithm, expiry, audience, role, and camera scope. It accepts each ticket once and limits signaling to its authorized camera.
4. Viewer sends the initial request with the camera password:

   ```json
   {"id":"<camera_id>","type":"request","password":"<camera password>"}
   ```

   Do not send `sid_digest`. MainServer injects the digest from the verified ticket. The camera checks digest and password before creating a peer connection or sending media.

The ticket is a short-lived, single-use **bearer** credential. Single-use limits later replay but does not prevent theft or a race before first use. A ticket is not cryptographically bound to the viewer device. The scoped ticket limits the request to one camera; it does not establish human identity or authorize who received credentials.

## Password provisioning and rotation

The streamer is configured only by `--config FOLDER`; its `auth/` subfolder holds the persistent camera credentials `device.key` and `password.phc`. The auth directory is mode 0700, the files are mode 0600, and the plaintext password is never stored. Existing credential files are loaded on startup and normal startup never overwrites them. On the first normal run with no existing auth state, the camera creates a device key and a verifier for the shared default password `5genwebrtc`; this is a known default and the owner must change it before relying on the camera for private viewing. Provisioning systems (the firmware) should therefore set the real password before the first start and not start a camera that has none. Default-password provisioning only applies to genuinely fresh state; if `device.key` exists but `password.phc` is missing or malformed, startup fails closed. The owner recovers with the setup-only `printf '%s' 'NEW' | streamer --config FOLDER --set-password`, which atomically writes the verifier and exits before SID activation, media, or network activity. This does not replace the device key.

`--set-password` reads the password from stdin, so it never appears in a process listing; a value after the flag (`--set-password 'NEW'`) is also accepted and is visible in the process list while the command runs. `camera-password --file PATH` does the same for a bare file. The credential the app sends, and that the camera hashes, is the lowercase hex SHA-256 of the password the user types (64 characters); `--set-password`, `camera-password` and the default all apply that hash first. It makes the credential longer and is not extra security, because the camera checks that value. The verifier is PBKDF2-HMAC-SHA256 with a random 16-byte salt, a 32-byte tag and an iteration count stored in the verifier (`i=`, 4,000 to 1,000,000; new verifiers use 8,000, measured 380 ms on the target camera). `camera-password --benchmark` measures derivation time without accessing auth files. The camera rereads the verifier at least once per second and before each request. A changed, missing, or invalid verifier closes active viewers; new requests fail closed until a valid verifier is present. A newly salted verifier closes viewers even if the plaintext password is unchanged. Failed-attempt throttling is camera-wide, and one in-flight password derivation per camera gate is allowed.

Password change authorization, owner accounts, and secure delivery to viewers belong to the external provisioning/product system. Atomic replacement and file permissions protect the mechanism; they do not prove who is authorized to change the password. Never put it in argv, environment variables, URLs, logs, analytics, or crash reports.

Example trusted provisioning flow with terminal echo disabled:

```sh
IFS= read -r -s -p 'New camera password: ' pw
printf '\n'
printf '%s\n' "$pw" | /path/to/build/examples/streamer/camera-password --file /persistent/camera/auth/password.phc
result=$?
unset pw
exit "$result"
```

The utility accepts 1–256 password bytes (one final LF/CRLF is stripped). Substitute the actual build executable and persistent camera path. Use a secret manager/protected provisioning channel for unattended provisioning.

## Build, configure, and run

### Camera build

The camera must be built with the trusted raw SID public key and explicit minimum issue timestamp. The raw key is 32 bytes represented as 64 hex characters; it is not a fingerprint, PEM text, or private key. For an x86 PC build, set `SID_PUBLIC_KEY_HEX` to the verified raw public-key hex from the controlled key-provisioning workflow, and set `SID_MIN_ISSUED_UNIX` to the policy timestamp:

```sh
SID_PUBLIC_KEY_HEX="<64-hex-characters>"
SID_MIN_ISSUED_UNIX="<minimum-unix-seconds>"
cmake -S libdatachannel_c14 -B build-sid-x86 \
  -DNO_TESTS=ON -DBUILD_SHARED_LIBS=OFF \
  -DSID_PUBLIC_KEY_HEX="$SID_PUBLIC_KEY_HEX" \
  -DSID_MIN_ISSUED_UNIX="$SID_MIN_ISSUED_UNIX"
cmake --build build-sid-x86 --target streamer camera_password_tool
```

The camera has one run-time option, `--config FOLDER` (it also accepts `-h`, `-V` and `--set-password`). The folder holds `webrtc.ini` (all settings, `key=value`), `sid` (the SID text, mode 0600, written from `pair`) and `auth/` (`device.key`, `password.phc`). A missing folder or `webrtc.ini` is created with defaults; the keys are `nickname` (optional, default `CAMERA`), `host`, `port`, `audio`, `video`, `chunksize`, `mp4`, `talkback-port`, `talkback-chunksize`, `timeout`, `entropy-bits`, `verbose`, `ca-file`, `reqIP`, `reqPort`. The streamer carries its defaults; with no `sid` it idles instead of exiting, and pairing is a `pair` file (`<sid>\n<password>\n`, mode 0600) that the firmware writes and `webrtc` consumes at start. Exit status **78** means a bad config folder or a malformed `webrtc.ini`. In the supported MbedTLS camera builds (`x86.cmake` and `anykav330.cmake`), the binary includes only the 1939-byte ISRG Root X1 PEM and uses it by default for Center HTTPS and MainServer WSS; `ca-file` is an optional PEM trust-bundle override, and an invalid override fails startup. `host` and `port` (default `webrtc.5gen.care`, 443) select Center and MainServer: the camera derives `https://HOST:PORT/verify`, uses `/v1/challenge` on that same host, and the exact WSS root `wss://HOST:PORT/`. There are no separate center-URL or main-origin settings. The camera clock must be suitable for TLS validation. Do not delete or replace an activated `device.key` unless the SID is also released (see the SID pool section). The camera build always requires `SID_PUBLIC_KEY_HEX` and `SID_MIN_ISSUED_UNIX`; there is no unprovisioned build.

Representative run:

```sh
./streamer --config /persistent/camera
```

The `nickname` setting supplies display metadata only. It does not change the key-derived device ID or ticket identity. The WSS registration carries `type=register` and `nickname`; an authenticated camera may also carry its SID, which the server validates against the digest in the verified ticket and retains in memory only for development discovery. The SID is not persisted or logged.

### Signaling-server build and service

Build from the Go module directory. This produces the one server binary used for Center APIs, WSS signaling, and player hosting:

```sh
cd server/src/go
go build -o sidctl ./cmd/sidctl
go build -o signaling-server ./src
```

Run an ordinary signaling-server instance with Center APIs on the same HTTPS/WSS listener at 8443. Use the exact public MainServer root in `-main-address`. If explicitly setting `-ticket-public-key`, `-ticket-kid`, or `-ticket-aud`, each must agree with the Center signing configuration; the audience must exactly match the configured MainServer root. Paths shown here are deployment placeholders; protect all private files and persistent state with restrictive ownership and permissions.

```sh
./signaling-server -root ../.. -port 8443 -https -center \
  -sid-public-key /secure/sid-issuer.pub.pem \
  -min-issued <minimum-issued-RFC3339> \
  -center-private-key /secure/center-ticket.key.pem -kid center-2026-01 \
  -main-address wss://webrtc.5gen.care:8443/ \
  -tenants-file /etc/webrtc/tenants.json \
  -activation-store /var/lib/webrtc/activations.jsonl \
  -ticket-public-key /secure/center-ticket.pub.pem \
  -ticket-kid center-2026-01 \
  -ticket-aud wss://webrtc.5gen.care:8443/ \
  -blocklist-file /etc/webrtc/sid-blocklist.txt
```

The load balancer must preserve the HTTPS/WSS routes, forward the correct host/proto information, and target only healthy instances. If TLS terminates at a proxy, `-trust-proxy-tls` is an alternative to `-https` only when the listener is reachable exclusively from that trusted proxy and it sets `X-Forwarded-Proto: https`. Do not expose a plain HTTP trusted-proxy listener directly to clients. With `-https`, the server reads the combined TLS certificate and private key from `<root>/cert.pem`.

`POST /v1/challenge`, `/verify`, `/v1/view`, ticket-admitted WSS, and the player are routes on this same listener. `/verify` activation and `/v1/view` viewing are distinct operations. The Center instance must have access to the SID public key, Center ticket private key, tenant registry, activation store, and blocklist; admission verifiers need the matching Center ticket public key, key ID, audience, and blocklist configuration. Keep Center ticket private-key distribution limited to trusted ticket-issuing instances. Never enable `-demo-tenant` in production: it intentionally allows unauthenticated development players.

The development player can list connected cameras from one explicitly configured tenant with `-discovery-tenant test01`. This read-only `/api/devices` option requires ticket admission and returns only that tenant's camera metadata (internal device ID, display nickname, tenant and signaling path, connection time, type, and latency). For admitted development cameras only, the response may also include the camera's 118-character SID to let the player fill the selected camera's SID field; the response uses `Cache-Control: no-store` and never returns passwords or tickets. Device `id` remains internal for routing and camera-scope checks, and user-facing labels use the nickname or a friendly `Camera N` fallback. This development endpoint makes included SIDs discoverable to anyone who can reach it; the camera password remains required for viewing. The option does not enable anonymous WSS or change ticket routing, camera scope, or authorization. Keep it on the development instance; production does not set it.

In the demo player, select a camera card, enter its camera-local password, and press **Play**. The card's SID fills the editable Camera SID field when discovery provides one; Play obtains a fresh `/v1/view` ticket and then connects over authenticated WSS. If an older admitted camera's discovery record has no SID, the user must paste that camera's SID; the player still checks the returned camera scope against the selected card. Passwords remain user-entered and are never stored as defaults. On legacy production servers where health reports ticket admission disabled, Play without an SID continues to use the legacy WSS request flow.

## Issuance, key rotation, development, and rollback

Use `sidctl` for issuance and offline verification from `server/src/go`. Generate the SID issuer key pair once in a controlled environment, keep the private key offline/restricted, and distribute only the public key through the camera build and Center configuration. Generate a separate Center ticket key pair; the Center private key signs admission tickets and the public key is used by ticket verifiers. Issue a distinct random-nonce SID for each camera:

```sh
cd server/src/go
go build -o sidctl ./cmd/sidctl
./sidctl keygen --private /secure/sid-issuer.key.pem --public /secure/sid-issuer.pub.pem
./sidctl keygen --private /secure/center-ticket.key.pem --public /secure/center-ticket.pub.pem
./sidctl issue --private-key /secure/sid-issuer.key.pem --audit /secure/issued.jsonl \
  --issued-at <issued-at-RFC3339> --tenant acme01 --role publisher > /secure/camera0.sid
./sidctl batch --private-key /secure/sid-issuer.key.pem --audit /secure/issued.jsonl \
  --tenant acme01 --role publisher --count 50000 --out /secure/acme01-001
./sidctl verify --public-key /secure/sid-issuer.pub.pem \
  --min-issued <minimum-issued-RFC3339> --sid-file /secure/camera0.sid
```

The single-issue command writes a raw SID to the selected output file; the batch export contains CSV/XLSX data with raw SIDs plus a digest-only manifest. Protect delivery artifacts as credentials. The private JSONL audit stores digests and issuance metadata, not raw SIDs. Batch issuance appends and syncs audit entries before creating export files. If a later export write fails, audit entries remain for allocated SIDs even though partial output files are removed; treat them as issued and do not assume the operation left no record. `sidctl` does not overwrite existing export paths.

By default, an SID must be activated within **three years** of issue time; a registered tenant may configure a different activation window. Once activated, an SID does not expire under the current policy. Revocation is a separate online/future entitlement concern; do not infer expiry or current entitlement from the signature alone. Raw SID delivery files are secrets and must be protected and removed after provisioning as operationally appropriate.

The SID and Center ticket keys are independent. Never use the SID issuer key to sign tickets. Current SID v2 has no embedded key ID or automatic key rotation; rotating issuer keys requires coordinated public-key provisioning and an explicit migration plan. Rotating the Center ticket key requires all ticket verifiers to trust the replacement key/key ID before issuers begin signing with it. Retain rollback-compatible verifier configuration during the transition and keep old private keys restricted.

For development, use non-production keys, tenants, activation data, and a local TLS trust setup. Exercise camera activation, ticket scope, wrong-password rejection, password rotation disconnect, and tenant/camera isolation before deployment. Do not use dev/demo admission on a production listener. Confirm persistent activation and password files survive restart and preserve restrictive permissions.

For a production rollout, build a versioned binary, stage config and keys with the target release, then replace one signaling-server instance at a time behind health checks. Confirm Center HTTP routes, WSS admission, player assets, and health on the new instance before draining the next. Roll back the binary and matching configuration together if health, ticket verification, activation persistence, or camera reconnect fails. Preserve activation records and device/password files across binary rollback; restore them from the deployment backup if a migration changed their format. Do not roll back issuer keys or delete activation state as a way to recover service. These are operational recommendations; no automatic migration or rollback mechanism is claimed here.

## Capacity planning and deferred fleet work

A prior planning estimate used **~24 signaling nodes** at 4 vCPU/8 GB for 1.0M cameras plus 30% concurrent viewers, based on a planning density of 60,000 connections/node (stop assignments around 80,000; 100,000 hard ceiling). Scenarios were 20 nodes for cameras-only and ~51 nodes for two simultaneous viewers per camera. These are estimates, not measured capacity; reconnect storms, TLS handshakes, memory, descriptor limits, geography, and real media-session mix must be measured before procurement. The one-process client map means a camera and its viewers must currently reach the same signaling process.

The current approved path is one MainServer address per ticket. Ordered multi-node assignment, registry-backed camera-to-node routing, cross-node signaling, load/latency scoring, multi-region failover, Center rate limiting, globally replicated revocation, and shared ephemeral TURN are separate future work. TURN is not required for initial signaling; any production TURN design must separately specify transport policy, ephemeral credentials, bandwidth sizing, and regional failure behavior. Fleet estimates and options are retained here only as planning context; they are not implemented guarantees.

### Future entitlement and revocation target

LicenseServer is a separate future service and entitlement system of record; it is not the Center handler or another role of the current signaling-server binary. The target design is for Center to verify SID cryptography and activation policy, then submit SID digests for online entitlement evaluation without putting LicenseServer's database in the signaling hot path. If LicenseServer is unavailable, the intended availability policy is to allow a cryptographically valid, otherwise admissible SID and queue its digest check durably. After recovery, queued checks are drained and invalid, expired, or revoked digests are published to a highly available replicated Redis blocklist. If the blocklist itself cannot be checked, protected admission must fail closed.

MainServer admission checks the blocklist at connect time. The production revocation target is to recheck active sessions at least every **60 minutes** and terminate sessions whose digest has been blocked; a shorter notification path may later improve revocation latency. A durable retry queue, LicenseServer, HA Redis deployment, global replication, and periodic active-session enforcement are future design targets, not capabilities of the current local blocklist implementation. This future service design does not imply a separate Center binary or port.

## Verified implementation scope and limits

Go race tests, vet, builds, and shared-listener HTTPS/WSS integration passed. Camera authentication tests and ARM cross-builds also passed; ARM tests were compiled but not executed. The PC build dependency was repaired by exporting the committed usrsctp source into a temporary build and installing its x86 library/header, preserving the dirty submodule. The original ARM build remains in `libdatachannel_c14/build.arm-preserved-20261013-134621`.

### Camera CLI verification (2026-10-13)

The revised camera CLI was built for x86 with the existing SID trust key/cutoff and `NO_TESTS=ON`. Direct camera-password/device-key tests passed, including concurrent creation and unsafe-file rejection. Isolated tests of the compiled streamer verified first-run `5genwebrtc`, private file permissions, unchanged files on restart, custom password setup before first startup, password rotation preserving the device key, missing/malformed established verifiers failing closed, FIFO/symlink rejection, and password byte limits. An independent Argon2 library verified the emitted PHC files.

A temporary local TLS signaling server and compiled camera completed same-port HTTPS activation and WSS admission using only `-d`/`-p` for the address. The server received the camera nickname while the 43-character key-derived ID matched its activation record. The full Go race suite and focused nickname registration test passed. The server nickname changes were subsequently deployed to the VPS from commit `979fa74`, as recorded below. The camera CLI binary/config now runs under PM2 on the PC, with its existing device key and current password preserved during migration. Physical ARM validation remains pending.

## Current verified development deployment (2026-10-14)

The dev-only release `9a0a800` is online on the isolated HTTPS/WSS listener at 8443. Deployment, automated checks, and the final live camera/viewer probe are verified.

| Item | Verified location/configuration |
| --- | --- |
| Source commit/release | `9a0a800` / `release-9a0a800-9a39d1d0` |
| Remote binary | `/home/admin/webrtc-dev-sid/signaling-server`; SHA-256 `a10fb436ef52573ed2eec958f10b9849d530d6260dcf441012138866980ca7a7` |
| Remote player | `/home/admin/webrtc-dev-sid/player/ws.html`; SHA-256 `0b8d6618200d2914bf8f4838eee5c8f9920c74b7825dd9e03a7da6e00495f015` |
| Dev process | PM2 `webrtc_dev`, PID 537677, online on HTTPS/WSS port 8443, version 2.1.1 |
| PC camera | PM2 `webrtc_camera`, PID 2460215; source `1b0af67e`, tree matches commit `5a2bd02c`; runtime SHA-256 `501301e51750870555e606fb998df9b826b5418225ce32cb351e4869089b18c6` (unchanged) |
| Rollback | `/home/admin/webrtc-dev-sid/backups/release-9a0a800-9a39d1d0/` |

The final TLS-verified probe passed public health (`ok`, version 2.1.1), public player hash parity, discovery of one `Front door` camera with a 118-character SID, `/v1/view`, WSS admission, viewer nickname-registration rejection, wrong-password rejection, correct-password camera offer (1692 SDP bytes), and consumed-ticket replay rejection. The camera reconnected after two Center retries without a restart or PID change. Final dev PID 537677 ran the expected binary SHA-256; production PID 430487/restarts 3 and activation-store SHA-256 `24a825a74ae04a16abdae32a4065f290c45f89bc0a5cee857a1280332e8bc58e` remained unchanged. Root verified Go tests, admission race tests, `go vet`, build, and JavaScript syntax.

The recovery branch `codex/pre-dev-redeploy-20261014` remains at `9a0a800`. The final amend on `main` contains deployment documentation only; compiled application source remains `9a0a800`, so no rebuild or process restart is needed. Keep `origin/main..HEAD` at one commit. Root will update the remote release-manifest commit mapping to the amended HEAD. The previous release's browser and camera validation below remains historical evidence only.

## Previous verified development deployment (2026-10-14; historical)

The previous dev release was `70d5820` (`release-70d5820-af0a0bd2`), with binary SHA-256 `52f8478c7dda94699706f428f794f3508edc703831ffb7b9478a3337500d8294`, player SHA-256 `0b8d6618200d2914bf8f4838eee5c8f9920c74b7825dd9e03a7da6e00495f015`, and PM2 dev PID 536018. Its rollback files are at `/home/admin/webrtc-dev-sid/backups/release-70d5820-af0a0bd2/`.

That release's discovery returned one `Front door` card. Its 118-character registered SID matched the digest in the authenticated camera ticket; the response used `Cache-Control: no-store`, and the SID value was not recorded. A browser test selected the card, showed a wrong-password error, then connected with the current password and rendered advancing 640x360 video. The TLS-verified probe passed `/v1/view`, WSS admission, wrong-password rejection, current-password offer generation (1691 SDP bytes), and consumed-ticket replay rejection. The camera SID, device key, and password verifier were unchanged. These results apply to release `70d5820`, not the current release.

## Historical development deployment (2026-10-13)

This section preserves earlier release-specific server deployment and PC/browser validation evidence. Its process IDs, hashes, and no-SID discovery behavior describe the 2026-10-13 releases, not the current runtime.

| Item | Verified location/configuration |
| --- | --- |
| Remote binary | `/home/admin/webrtc-dev-sid/signaling-server` |
| Process/listener | PM2 `webrtc_dev`, HTTPS and WSS on 8443, version 2.1.1 |
| Public test URL | `https://webrtc.5gen.care:8443/` |
| Remote config | `/home/admin/webrtc-dev-sid/pm2_start.json` (dev-only app) |
| Rollback backup | `/home/admin/webrtc-dev-sid/backups/release-979fa74-3def9a5b/` (binary, player, prior configuration and manifest) |
| Binary SHA-256 | `8360c18b36697cab8a4ada202477f1f0fcbb6a8e1d473523791e535b19028ed7` (staged and running executable matched) |
| Deployed source commit | `979fa74` |
| Player | Source `979fa74`; served SHA-256 `64c7b3973c9c5db424ddb11aecf329bb6e03c6f64c05c6a5231660659d2279a8` |
| PC camera | PM2 `webrtc_camera`, compiled x86-64 `libdatachannel_c14/build/examples/streamer/streamer`, version 2.1.1; runtime SHA-256 `ddfbf0399b94c84e9310d7c10007f27b10eb06f89dc204c439aa7134efead6d2` |
| Private camera runtime | `libdatachannel_c14/.runtime/camera/` (private, gitignored SID/auth/recordings/PID/rollback directory); original test inputs/media PID files remain under `/tmp/webrtc-oneport-NDJJcA/` |

Repository PM2 files: `server/pm2_start.json` keeps the production app configuration intact and defines the compiled dev app; `libdatachannel_c14/pm2_start.json` defines the new camera CLI using PC paths under `.runtime/camera/`, with nickname `Front door` and talkback disabled. The existing SID, device key, and current verifier were migrated byte-for-byte into mode-0600 files under private mode-0700 directories. PM2 `webrtc_camera` (PID 1940520 at verification, zero restarts) replaced manual PID 1783059 while retaining the existing synthetic video/audio feeder processes. The dev server remained online during this camera startup.

Historical dev picker verification for release `979fa74`: `-discovery-tenant test01` was enabled on `webrtc_dev`; that release's public device list returned exactly the online camera `Front door` without SID, digest, password, or ticket fields. The browser displayed its card, and clicking it selected the matching key-derived device ID. Full Go race tests, vet, build and JavaScript syntax checks passed; anonymous WSS and camera-scope restrictions remained enforced. Screenshot proof: `/tmp/webrtc-dev-release/camera-picker.png`. Production configuration and process were unchanged. The no-SID result describes that earlier release behavior; current discovery may include an authenticated SID as described above.

Post-deployment verification: PM2 `webrtc_dev` online (PID 531937 at verification), local TLS health and public HTTPS health `ok`/2.1.1, running executable and served player matched the release hashes. Production PID 430487 and restart count 3 remained unchanged. Activation-store byte count was unchanged. The PC camera connected successfully under PM2; a TLS-verified live probe passed `/v1/view`, WSS admission, viewer nickname-registration rejection, wrong-password rejection, a correct-password camera offer (1,691 SDP bytes with the newly started PM2 camera), and consumed-ticket replay rejection (401).

The deployment uses an isolated test tenant and SID; it does not reuse a real camera's SID. The issuer private key remains local to its issuer. The ticket-signing key was generated on the VPS and remains there; this update retains it. The existing production process on 443 was not restarted. The dev player defaults its HTTP, Center, and WSS host/port to its page origin, so loading the 8443 page does not silently select production 443.

Earlier PC/browser validation on this date: camera activation; HTTPS `/v1/view`; TLS-verified ticket-admitted WSS; missing-password rejection (`Invalid request`); wrong-password rejection (`Unauthorized`); correct-password camera offer; consumed-ticket replay rejection (401); decoded 640x360 synthetic H.264 video and received 8 kHz PCMA audio. Password rotation during active playback stopped video/audio, rejected the old password, and allowed the new password to resume playback without changing SID or device identity.

Synthetic feeds use local UDP inputs 8553 (H.264, single encoder thread/slice) and 8552 (PCMA). The camera is supervised by PM2 `webrtc_camera`; `.runtime/camera/camera.pid` records its PID at verification. Original `streamer.pid` was updated to this PM2 PID, while `video.pid` and `audio.pid` still identify the unchanged feeder processes. Verification used host-to-host LAN media; it does not establish TURN/WAN behavior, microphone/talkback, physical-camera behavior, power-loss durability, ARM memory/latency, production capacity, or cross-instance activation/replay consistency. The activation store and replay cache remain process-local; fleet consistency requires additional work before multiple independent Center instances share a load balancer.

Other limits: Center rate limiting and production fleet routing are deferred; ticket bearer theft before first use remains possible; external systems own password distribution and change authorization; camera password checking is shared-password authorization, not user identity; a locally configured blocklist is not global revocation. Do not infer expiration or global revocation from signature validation alone.
