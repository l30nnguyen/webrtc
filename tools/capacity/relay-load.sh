#!/bin/bash
# Capacity test (tools/capacity/README.md): the server on core 0 only, the
# load on cores 1-3. Needs 4 cores and taskset. WORK=DIR keeps the files.
S=${WORK:-$(mktemp -d)}; R=$S/rload; rm -rf $R; mkdir -p $R; umask 077
HERE=$(cd "$(dirname "$0")/../.." && pwd)
(cd "$HERE/src/relay" && go build -o "$S/matrix-relay" . && go build -o "$S/zzrelayload" ./cmd/loadtest) || exit 1
printf '{"users":{"load":"loadpass"}}' > $R/secrets.json
start_relay() {
  GOMAXPROCS=1 taskset -c 0 $S/matrix-relay -public-ip 127.0.0.1 -udp 127.0.0.1:34800 -tcp "" -secrets $R/secrets.json -metrics "" -allow-private-peers -max 0 -relay-min 20000 -relay-max 60000 > $R/relay.log 2>&1 &
  P=$!; sleep 1
}
start_relay
cpu(){ awk '{print $14+$15}' /proc/$P/stat; }; rss(){ awk '/VmRSS/{print $2}' /proc/$P/status; }
echo "baseline RSS $(rss) KB"
run() { # label, args
  label=$1; shift
  taskset -c 1-3 $S/zzrelayload "$@" > $R/out.log 2>&1 & L=$!
  until rg -q "ready|allocate" $R/out.log; do sleep 0.2; done
  r=$(rss); c0=$(cpu); t0=$(date +%s.%N); wait $L; c1=$(cpu); t1=$(date +%s.%N)
  echo "$label: RSS $r KB; relay CPU $(echo "scale=1; ($c1-$c0)/($t1-$t0)" | bc)% of core 0; $(tail -1 $R/out.log)"
  kill $P; sleep 1; start_relay
}
run "hold 1000 sessions, both relayed (2000 allocations)" -sessions 1000 -both -hold -dur 10s
run "100 sessions x 170 pps, viewer relayed" -sessions 100 -pps 170 -dur 20s
run "200 sessions x 170 pps, viewer relayed" -sessions 200 -pps 170 -dur 20s
run "400 sessions x 170 pps, viewer relayed" -sessions 400 -pps 170 -dur 20s
run "100 sessions x 170 pps, both relayed" -sessions 100 -pps 170 -both -dur 20s
run "200 sessions x 170 pps, both relayed" -sessions 200 -pps 170 -both -dur 20s
kill $P
