#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'Usage: %s COUNT [TENANT]\n' "${0##*/}" >&2
  printf 'COUNT must be an integer from 1 to 100000. TENANT defaults to test01.\n' >&2
}

if [[ $# -lt 1 || $# -gt 2 ]]; then
  usage
  exit 2
fi
count=$1
tenant=${2:-test01}
if [[ ! $count =~ ^[0-9]{1,6}$ ]] || (( 10#$count < 1 || 10#$count > 100000 )); then
  printf 'Error: COUNT must be an integer from 1 to 100000.\n' >&2
  exit 2
fi

script_path=$(readlink -f -- "$0") || {
  printf 'Error: cannot resolve script path.\n' >&2
  exit 1
}
script_dir=$(dirname -- "$script_path")
sid_gen_dir=${SID_GEN_DIR:-"$(cd -- "$script_dir/../../../sid_gen" && pwd -P)"}
private_key=$sid_gen_dir/issuer.key.pem
audit_file=$sid_gen_dir/issued.jsonl
generated_dir=$sid_gen_dir/generated
sidctl_bin=${SIDCTL_BIN:-$sid_gen_dir/sidctl}

if [[ ! -x $sidctl_bin ]]; then
  printf 'Error: sidctl is missing or not executable: %s\n' "$sidctl_bin" >&2
  exit 1
fi
if [[ ! -r $private_key ]]; then
  printf 'Error: issuer private key is missing or unreadable: %s\n' "$private_key" >&2
  exit 1
fi

umask 077
mkdir -p -- "$generated_dir"
chmod 700 -- "$generated_dir"
exec 9>"$generated_dir/.generate.lock"
flock -x 9

stamp=$(date -u +%Y%m%dT%H%M%SZ)
random=$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
prefix=$generated_dir/${stamp}-$$-$random
while [[ -e $prefix.csv || -e $prefix.xlsx || -e $prefix.manifest.csv ]]; do
  random=$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
  prefix=$generated_dir/${stamp}-$$-$random
done
batch_id=${prefix##*/}

exec "$sidctl_bin" batch \
  --private-key "$private_key" \
  --audit "$audit_file" \
  --tenant "$tenant" \
  --role publisher \
  --count "$((10#$count))" \
  --batch-id "$batch_id" \
  --out "$prefix"
