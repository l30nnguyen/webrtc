#!/usr/bin/env bash
set -euo pipefail

script_path=$(readlink -f -- "$0") || {
  printf 'Error: cannot resolve script path.\n' >&2
  exit 1
}
script_dir=$(dirname -- "$script_path")
sid_gen_dir=${SID_GEN_DIR:-"$(cd -- "$script_dir/../../../sid_gen" && pwd -P)"}

python3 - "$sid_gen_dir/issued.jsonl" "$sid_gen_dir/generated" <<'PY'
import csv
import base64
import binascii
import fcntl
import hashlib
import json
import os
import re
import sys

audit_path, generated_dir = sys.argv[1:]
batch_header = ["batch_id", "tenant", "role", "sid_digest", "sid", "issued_at", "status", "delivered_to"]
manifest_suffix = ".manifest.csv"
records = []
seen_digests = set()
lock_file = None

def fail(message):
    print(f"Error: {message}", file=sys.stderr)
    raise SystemExit(1)

lock_path = os.path.join(generated_dir, ".generate.lock")
try:
    # Generation holds this exclusive lock through audit and all three output
    # files. A shared lock gives this listing one complete, consistent view.
    # Opening an existing lock read-only avoids creating files during listing.
    lock_file = open(lock_path, "rb")
    fcntl.flock(lock_file.fileno(), fcntl.LOCK_SH)
except FileNotFoundError:
    pass
except OSError as exc:
    fail(f"cannot lock generated directory ({exc.strerror or 'I/O error'})")

try:
    with open(audit_path, "r", encoding="utf-8") as source:
        for line_number, line in enumerate(source, 1):
            try:
                item = json.loads(line)
            except (json.JSONDecodeError, UnicodeError):
                fail(f"malformed audit record at line {line_number}")
            if not isinstance(item, dict):
                fail(f"malformed audit record at line {line_number}")
            digest = item.get("digest_sha256")
            tenant = item.get("tenant")
            role = item.get("role")
            issued_at = item.get("issued_at")
            if not all(isinstance(value, str) and value for value in (digest, tenant, role, issued_at)):
                fail(f"incomplete audit record at line {line_number}")
            if digest in seen_digests:
                fail(f"duplicate audit digest at line {line_number}")
            seen_digests.add(digest)
            records.append({"date": issued_at, "tenant": tenant, "role": role,
                            "digest": digest, "sid": "SID_UNAVAILABLE"})
except FileNotFoundError:
    # A new SID directory has no issuance history yet.
    pass
except UnicodeError:
    fail("audit file is not valid UTF-8")
except OSError as exc:
    fail(f"cannot read audit file ({exc.strerror or 'I/O error'})")

if not os.path.isdir(generated_dir):
    # A missing output directory is valid when only historical audit entries exist.
    generated_files = []
else:
    try:
        generated_files = sorted(
            name for name in os.listdir(generated_dir)
            if name.endswith(".csv") and not name.endswith(manifest_suffix)
            and os.path.isfile(os.path.join(generated_dir, name))
        )
    except OSError as exc:
        fail(f"cannot inspect generated directory ({exc.strerror or 'I/O error'})")

saved = {}
for filename in generated_files:
    path = os.path.join(generated_dir, filename)
    try:
        with open(path, "r", encoding="utf-8", newline="") as source:
            reader = csv.DictReader(source, strict=True)
            if reader.fieldnames != batch_header:
                fail(f"unexpected CSV header in {filename}")
            for row_number, row in enumerate(reader, 2):
                if None in row or any(row.get(key) is None for key in batch_header):
                    fail(f"malformed CSV row in {filename} at line {row_number}")
                digest = row["sid_digest"]
                if not digest or not row["sid"]:
                    fail(f"incomplete CSV row in {filename} at line {row_number}")
                sid = row["sid"]
                if not re.fullmatch(r"[A-Za-z0-9_-]{118}", sid):
                    fail(f"invalid SID encoding in {filename} at line {row_number}")
                try:
                    decoded = base64.urlsafe_b64decode(sid + "==")
                    canonical_sid = base64.urlsafe_b64encode(decoded).decode("ascii").rstrip("=")
                    digest_bytes = hashlib.sha256(sid.encode("ascii")).hexdigest()
                except (UnicodeEncodeError, binascii.Error, ValueError):
                    fail(f"invalid SID encoding in {filename} at line {row_number}")
                if len(decoded) != 88 or canonical_sid != sid:
                    fail(f"invalid SID encoding in {filename} at line {row_number}")
                if not re.fullmatch(r"[0-9a-f]{64}", digest) or digest_bytes != digest:
                    fail(f"SID digest mismatch in {filename} at line {row_number}")
                if digest in saved:
                    fail(f"duplicate saved digest in {filename} at line {row_number}")
                saved[digest] = {"date": row["issued_at"], "tenant": row["tenant"],
                                 "role": row["role"], "digest": digest, "sid": row["sid"]}
    except csv.Error:
        fail(f"malformed CSV data in {filename}")
    except (OSError, UnicodeError) as exc:
        fail(f"cannot read {filename} ({getattr(exc, 'strerror', None) or 'invalid text or I/O error'})")

# Preserve issuance order from the audit; append any saved CSV row absent from
# the audit only as a defensive visibility measure for interrupted history.
rows = []
for record in records:
    rows.append(saved.pop(record["digest"], record))
rows.extend(saved[digest] for digest in sorted(saved))

writer = csv.writer(sys.stdout, lineterminator="\n")
writer.writerow(["date", "tenant", "role", "digest", "SID"])
for row in rows:
    writer.writerow([row["date"], row["tenant"], row["role"], row["digest"], row["sid"]])
raw_count = sum(row["sid"] != "SID_UNAVAILABLE" for row in rows)
missing_count = len(rows) - raw_count
print(f"Total: {len(rows)} SIDs ({raw_count} raw available, {missing_count} unavailable)", file=sys.stderr)
PY
