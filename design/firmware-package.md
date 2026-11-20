# Signed, encrypted camera firmware (MXPKG1)

Status: design and reference code ready (2026-11-17); not yet in the camera firmware.

## Goal

Cameras download the `matrix` binary at every boot. Today it is a plain file at
`/downloads/<MODEL>/matrix` that anyone can download, and the camera runs whatever
it receives (its downloader does not verify the TLS certificate, and nobody
checks `matrix.sha256`). After this change:

- **Nobody can make a camera run their code** by replacing the binary on the way
  (man in the middle) or on the server: the camera runs only a package signed with
  a key that never leaves the admin server.
- **Only cameras can download it**: the control server sends the package only to a
  valid camera SID.
- **The binary is encrypted**, so a download alone shows nothing. This slows down
  reverse engineering; it cannot stop it, because every camera holds the key and
  runs the plain binary from RAM. The binary must never contain secrets.

## Platforms

One package per build platform, not per camera model (decided 2026-11-17).
fepe_sdk knows only its platform, at build time, and does the download, check and
decryption in one call (`fepe_matrix_binary_download(target)`); the firmware app
only calls it. matrix is linked statically, so one build per compiler family is
enough:

| Platform | matrix build | fepe_sdk builds | Cameras today |
|---|---|---|---|
| `anycloud` | `./build.sh anycloud` (GCC 5.5.0) | anykav130 and anykav330 (both anycloud compilers) | HCX130 and HCX330L: both download this signed package (HCX330L shares the compiler; tested by the owner) |
| `anykav200` | `./build.sh anykav200` (GCC 4.8.5) | anykav200 | GIRAFFE_HD: `matrix` is bundled into the firmware image, no download |
| `anykav500` | `./build.sh anykav500` (GCC 4.9.4) | anykav500 | no camera downloads it today (HCX330L runs the `anycloud` build) |

For now (2026-11-17) fepe_sdk turns the download on only for its anycloud builds
(anykav130, anykav330); on other platforms the call returns an error. So only
`anycloud` is packaged and only `enc-anycloud.key` is needed; GIRAFFE_HD stays on the
plain `/downloads/GIRAFFE_HD/` file. The HCX330L firmware links the anykav130
fepe_sdk, so it will run the `anycloud` build instead of its anykav500 build: test that
on an HCX330L before its firmware switches.

## Keys

| Key | Where the private part lives | Where the camera has it |
|---|---|---|
| Signing key (Ed25519), id 1 | Admin server only, `/opt/matrix-admin/firmware/keys/sign.key` (0600) | Public key compiled into the firmware app |
| Encryption key per platform (32 bytes, XChaCha20-Poly1305), id 1 | Admin server, `/opt/matrix-admin/firmware/keys/enc-<PLATFORM>.key` (0600) | Compiled into fepe_sdk for that platform |

The control server holds neither key: a break-in there can stop downloads but not
make cameras run other code. Key ids let a new key be added to the firmware before
packages switch to it (rotation). Keys stay out of all repositories: the fepe_sdk
build reads the encryption key from a file outside the repo and generates a header.

## Package format

All integers big-endian. Reference code: `matrix_c/tools/mxpkg/mxpkg_open.{h,c}`
(C99, needs only Monocypher 4, ~2 files).

| Offset | Size | Field |
|---|---|---|
| 0 | 8 | magic `MXPKG1\0\0` |
| 8 | 16 | platform, ASCII, NUL padded (`anycloud`) |
| 24 | 16 | version, ASCII, NUL padded (`2.1.9`) |
| 40 | 4 | version code: major×1000000 + minor×1000 + patch |
| 44 | 1 | encryption key id |
| 45 | 1 | signing key id |
| 46 | 2 | zero |
| 48 | 4 | payload size n (plain binary) |
| 52 | 24 | XChaCha20 nonce (random per package) |
| 76 | 4 | issued, UNIX seconds |
| 80 | 16 | zero |
| 96 | n | binary, XChaCha20-Poly1305 encrypted, the 96-byte header as associated data |
| 96+n | 16 | Poly1305 tag |
| 112+n | 64 | Ed25519 signature over bytes [0, 112+n) |

Size is the binary plus 176 bytes. The signature is standard Ed25519 (checked
against Go's `crypto/ed25519`); the encryption is the XChaCha20-Poly1305 of
Monocypher's `crypto_aead_lock` (same as libsodium's `xchacha20poly1305_ietf`).

## Camera check (before anything runs)

`mxpkg_open_file(path, platform, min_version, sign_keys, enc_keys, &info)`, in order:

1. Header well formed, size matches.
2. Platform equals the platform fepe_sdk was built for (an anykav200 build never runs
   on an anycloud camera).
3. Version code at least the lowest allowed version (compiled into the firmware;
   optionally raised by a number kept in `/mnt/config`, which a factory reset may
   clear).
4. Key ids known.
5. Signature and tag checked over the whole file (one read in 4 KB blocks).
6. Only then decrypted in place and shortened to the plain binary.

It needs no extra RAM beyond the downloaded file (the camera's `/tmp` is RAM). On
any failure the caller deletes the file and does not start matrix.

## Download

`GET https://matrix.5gen.care/v1/firmware/<PLATFORM>/matrix?sid=<camera SID>`

- Control server (Center) only. `200` with `Content-Length` and the package; `401`
  for a missing, invalid, viewer, unknown-tenant or blocked SID; `404` when there is
  no package for the platform. Rate limited like the other Center calls.
- Serves `/opt/matrix/firmware/<PLATFORM>/matrix.pkg`; not reachable under `/downloads/`.
- Logs the platform, size, first 12 hex of the SID digest and source address, never
  the SID.
- The SID travels in the URL because the firmware's downloader only does a GET.
  It is protected by TLS only if the camera verifies the certificate, so the
  firmware should turn certificate checking on (below).

## Publishing (admin server)

```sh
mxpkg pack --model anycloud --version 2.1.9 \
  --sign-key keys/sign.key --sign-id 1 --enc-key keys/enc-anycloud.key --enc-id 1 \
  matrix_2.1.9 anycloud.pkg
mxpkg open --model anycloud --sign-pub <hex> --sign-id 1 --enc-key keys/enc-anycloud.key \
  --enc-id 1 anycloud.pkg check.bin && cmp check.bin matrix_2.1.9   # the camera's check
scp anycloud.pkg center:/opt/matrix/firmware/anycloud/matrix.pkg.new  # then mv into place
```

## fepe_sdk changes (`src/events/src/fp_bin_download.c`, `fepe_ipc_generic_download`)

Reviewed by the fepe_sdk session on 2026-11-17. The firmware cannot do steps 2 and
5 below without these:

1. **HTTP status:** `fileinfo.success` is set from the byte count only, so a 401 or 404
   body is saved as `matrix` with success true. Set success only on HTTP 200, or add
   the HTTP code to `fepe_file_handle_info_t`, so the firmware can tell "SID refused"
   (401: stop and log) from "no package" (404) and network errors (retry).
2. **Certificate checking:** `VERIFYPEER`/`VERIFYHOST` are hardcoded to 0. Add an option
   (or a new function) that turns them on with a compiled-in CA (`CURLOPT_CAINFO_BLOB`,
   ISRG Root X1 and X2; curl runs on mbedTLS) and `CURLOPT_FOLLOWLOCATION=0`, so the
   `?sid=` URL is never sent to another host.
3. **Log:** `akae_log_debug("Downloading file %s to %s", url, ...)` prints the SID. Log
   the URL without its query string.

## Firmware changes (ak_v330_sdk, `MainApp/fepe_cam/fepe_matrix.c`)

1. URL: `https://matrix.5gen.care/v1/firmware/%s/matrix?sid=%s` with `snprintf` (platform,
   and the raw SID from the cert via `fepe_ipc_cert_matrix_sid_get`, not the base64 form
   of `fepe_matrix_get_sid()`; strip a trailing newline if read from the `sid` file).
   The downloaded file name stays `matrix`.
2. In `_after_download` (runs on the SDK download thread, 128 KB stack; the opener
   needs ~5 KB): require HTTP 200, then `mxpkg_open_file()` on `/tmp/matrix_dl/matrix`.
   On success `chmod 0755` and move to `/tmp/matrix` as now; on failure delete it, log
   `mxpkg_status_text()`, retry later.
3. Retry with backoff (today a failed download is never retried); no retry on 401.
4. The SD card override (`/mnt/sdcard/matrix`) must go through the same check (or
   be removed in production builds): otherwise anyone with the SD card runs code.
5. Turn certificate checking on for this download (fepe_sdk change 2), so the SID
   cannot be read on the way. The signature keeps the binary safe either way.
6. **Clock:** certificate checking needs the right time, which the camera gets from
   the fepe_sdk time sync after it connects (`fepesdk.c`), while `fepe_matrix_run` starts
   at boot. Start the download after the first time sync, or treat a TLS failure
   before it as retryable. The package check itself does not use the clock.
7. RAM: the package is the binary plus 176 bytes; opening it needs no second copy.
   Measure the open time once on an HCX130 (expected 1-2 s on ARM926).

## Rollout

1. Generate keys on the admin server; give the public key and the platform encryption
   keys to the firmware build (outside the repo).
2. Deploy the control server with `/v1/firmware/`; publish packages for all platforms
   next to the old plain files.
3. Ship the new firmware. Cameras with old firmware keep using `/downloads/`.
4. When no camera uses `/downloads/<MODEL>/matrix` any more (its downloads in the
   server log stop), delete the plain files. Until then the plain binary stays
   downloadable. **Done 2026-11-20:** the owner turned `/downloads` off for
   every model at 12:02 (+07) (`matrix-firmware downloads off`, 410 Gone;
   `firmware-channels.md`). No camera needs it: HCX130 and HCX330L take the
   signed `anycloud` package, GIRAFFE_HD has `matrix` in its firmware image.

## What this does not cover

- Root on the camera (shared console password, open telnet/FTP on the LAN) lets
  anyone run anything, signed or not.
- A camera after a factory reset accepts any signed version at least the compiled-in
  minimum (no rollback number survives the reset).
- Encryption keys are per platform, not per camera: one leaked key exposes that
  platform's binaries (not the ability to run code).
