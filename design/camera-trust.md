# Camera trust for the control servers (draft)

Status: draft for decision, 2026-11-27. Camera 2.2.3 already ships step 1.

## Problem

Cameras check the control server certificate strictly (mbedTLS 3.6.7,
`VERIFY_REQUIRED`, dates checked for every certificate including the trusted
root). They trust only the roots built into the binary
(`matrix_c/src/mx_ca.c`), or a `ca-file=` set by hand in `matrix.ini`.

When verification fails the camera can't activate or connect, so it is
offline for every viewer. It can't be repaired remotely either: a firmware
download goes over the same TLS connection. GIRAFFE_HD bundles `matrix` in
its firmware image and may never be updated in the field.

## Today (checked 2026-11-27)

Both `matrix.5gen.care` and `matrix-sg00.5gen.care` serve the Let's Encrypt
chain:

| Certificate | Valid until |
|---|---|
| server certificate (ECDSA P-256) | renewed every ~60-90 days |
| YE2 (intermediate) | 2028-09-02, replaced at renewal |
| Root YE, signed by ISRG Root X2 | 2032-09-02 |
| ISRG Root X2, cross-signed by ISRG Root X1 | 2032-09-02 |
| ISRG Root X1 (self-signed) | 2035-06-04 |
| ISRG Root X2 (self-signed) | 2040-09-17 |

| Camera firmware | Trusts | Stops connecting |
|---|---|---|
| up to 2.2.2 | X1 | 2032-09-02 at the latest; earlier if Let's Encrypt drops the X2 cross-sign; never after 2035-06-04 |
| 2.2.3 and later | X1 + X2 | 2040-09-17 at the latest; earlier only if Let's Encrypt moves to a hierarchy not under X1 or X2 |

Verified 2026-11-27 with the camera's own TLS code against both servers: the
2.2.3 store, X1 alone and X2 alone all verify.

## Proposal: our own root for cameras

Cameras would stop depending on public CA lifetimes and policy changes.

1. **A Matrix camera root CA.** ECDSA P-256, valid 30 years, key generated
   and kept offline like the SID issuer key (never on a server). It signs only
   an intermediate.
2. **A camera intermediate.** Valid ~5 years, key on the admin server. It
   signs the server certificates for camera hosts (1-year certificates).
3. **A camera-only host name per server,** for example `cam.5gen.care` and
   `cam-sg00.5gen.care`. Apps and browsers keep using `matrix.*` with Let's
   Encrypt; only cameras use `cam.*`. The control server serves both
   certificates (Go selects by SNI).
4. **Cameras embed the Matrix root next to X1 and X2,** and use the camera
   host. Public roots stay as a fallback during the transition.

Result: cameras keep connecting for the root's 30 years, and certificate
renewal for cameras is in our hands.

### Costs and risks

- A second certificate to issue and renew on each server (a script on the
  admin server, plus an expiry check in `matrix-monitor`).
- The root key must be kept offline and backed up as carefully as the SID
  issuer key: whoever holds it can impersonate the control server to cameras.
- The host name change goes out with new firmware, so old cameras keep using
  `matrix.*` until updated.

## Steps

1. Done in 2.2.3: embed ISRG Root X2 (cameras last to 2040).
2. Monitor certificate expiry for both hosts (alert under 14 days left): the
   likeliest outage is a failed renewal, not a root expiring.
3. Decide on the own root before the next long-lived model ships (target:
   before 2030).

## Open questions

- Which models are expected to stay in the field past 2040?
- Can GIRAFFE_HD firmware be updated in the field (vendor channel, SD card),
  and does an image update keep the config folder (`auth/` in particular)?
