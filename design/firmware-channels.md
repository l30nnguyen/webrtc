# Staged camera firmware releases (R7)

Since control server 2.7.6. Before it, every publish replaced
`/downloads/<MODEL>/matrix` and the signed package at once, so a bad build
reached every camera at its next boot.

## How it works

The Center keeps every published version in its own folder and a small
channels file says who gets which:

| Path on the Center | What |
|---|---|
| `/opt/matrix/downloads/<MODEL>/<VERSION>/{matrix,matrix.sha256,version}` | plain binary per model (HCX130, HCX330L, GIRAFFE_HD) |
| `/opt/matrix/firmware/<PLATFORM>/<VERSION>/matrix.pkg` | signed, encrypted package (anycloud) |
| `/opt/matrix/downloads/<MODEL>/matrix` … and `firmware/anycloud/matrix.pkg` | unversioned copies, always equal to **stable** (servers without channels, and the fallback) |
| `/opt/matrix/cfg/firmware-channels.json` | per model and platform: `stable`, `canary`, `previous`; and the canary group |

```json
{
  "firmware": {
    "HCX130":   {"stable": "2.2.2", "canary": "2.2.3", "previous": "2.2.1"},
    "anycloud": {"stable": "2.2.2", "canary": "2.2.3", "previous": "2.2.1"}
  },
  "canary": {"ips": ["115.78.12.186"], "tenants": [], "sids": []}
}
```

On each download the control server picks the version:

- a camera in the **canary group** gets `canary` (when set), every other camera
  gets `stable`;
- `/downloads/<MODEL>/…` knows only the camera's address, so there the group
  is `ips` (addresses or CIDR prefixes; the office by default);
- `/v1/firmware/<PLATFORM>/matrix?sid=…` also knows the SID, so `tenants` and
  `sids` (SID digest prefix, at least 12 hex, as the logs print it) count too;
- a missing canary folder falls back to stable, a missing stable folder to the
  unversioned file; no file, or a model it does not list, means the
  unversioned file as before;
- the file is read again when it changes; a file that does not parse is logged
  and the last good one stays in use.

The URLs do not change, so no camera firmware change is needed. Each download
of the binary is logged with model, version and channel
(`firmware download HCX130 2.2.3 (canary) from …`,
`center: firmware anycloud 2.2.3 (canary, … bytes) to SID …`).

## Releasing a version (admin server)

`matrix-firmware` (repo `server/admin/matrix-firmware`, installed in
`/usr/local/bin`, run as `admin`). Releases are stored in
`/opt/matrix-admin/firmware/releases/<VERSION>/<platform>/matrix`; every
action is appended to `/opt/matrix-admin/firmware/history.log`.

1. **Upload** from the PC after building `matrix_c` for the three platforms
   (`./build.sh <platform>` and `scripts/package_anykav200.sh <platform>`):

   ```bash
   tar -C matrix_c/release/bin -cf - anycloud/matrix_2.2.3 anykav200/matrix_2.2.3 anykav500/matrix_2.2.3 |
     ssh admin@<admin server> matrix-firmware upload 2.2.3
   ```

   It refuses a tar without all three, a file that is not a 32-bit ARM
   executable, a binary that does not carry the version string, and a version
   that was already uploaded with other content.
2. **Publish** as canary: `matrix-firmware publish 2.2.3`. It builds and
   checks the signed anycloud package (pack, then open and compare), copies
   each file to `<MODEL>/2.2.3/` on the Center and checks the checksums there,
   and sets `canary` for every model. Cameras in the group take it at their
   next boot (reboot the office cameras to start at once).
3. **Watch** for a day or two: the daily summary's per-version table (success
   per camera version), the connection log, and the cameras themselves.
4. **Promote**: `matrix-firmware promote`. The canary becomes stable for
   everyone, the old stable becomes `previous`, and the unversioned files are
   updated. Or **cancel**: `matrix-firmware cancel` drops the canary.
5. **Roll back** at any time: `matrix-firmware rollback` makes `previous`
   stable again for every model (cameras take it at their next boot).

`matrix-firmware status` shows stable / canary / previous per model, the
versions present on the Center and the canary group;
`matrix-firmware canary --ips 115.78.12.186 --tenants test01` changes the
group. Publishing keeps stable, canary, previous and the two newest other
versions per model on the Center and deletes older ones.

## Limits

- On `/downloads` the canary group can only be chosen by address: in
  practice the office's cameras. A tenant or single cameras can be canaries
  only on firmware that uses the signed `/v1/firmware` route (anycloud).
- A camera takes a new version only when it boots; promotion and rollback are
  not instant.
- The Center alone serves firmware: if it is down, cameras that boot keep the
  binary they have, or fail to download (as before).
