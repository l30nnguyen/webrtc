#!/bin/bash
# Capacity test (tools/capacity/README.md): the server on core 0 only, the
# load on cores 1-3. Needs 4 cores and taskset. WORK=DIR keeps the files.
S=${WORK:-$(mktemp -d)}; R=$S/load; rm -rf $R; mkdir -p $R/cfg; umask 077
HERE=$(cd "$(dirname "$0")/../.." && pwd)
(cd "$HERE/src/control" && go build -o "$S/matrix_cc" ./src && go build -o "$S/zzload" ./cmd/loadtest) || exit 1
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout $R/key.pem -out $R/crt.pem -days 2 -subj /CN=localhost 2>/dev/null; cat $R/crt.pem $R/key.pem > $R/cert.pem
echo '[{"host":"203.0.113.7","port":3478,"username":"u","password":"p"}]' > $R/cfg/turn.conf
start_server() {
  GOMAXPROCS=1 taskset -c 0 $S/matrix_cc -root $R/ -port 18443 -https -rate-exempt 127.0.0.1/32 -conns-per-source 0 -statistic-dir $R/statistic -event-dir $R/events > $R/control.log 2>&1 &
  P=$!; sleep 1
}
cpu(){ awk '{print $14+$15}' /proc/$P/stat; }; rss(){ awk '/VmRSS/{print $2}' /proc/$P/status; }
start_server
echo "baseline RSS $(rss) KB"
for N in 5000 20000; do
  taskset -c 1-3 $S/zzload -mode idle -n $N > $R/idle.log 2>&1 & L=$!
  until rg -q connected $R/idle.log; do sleep 1; done; cat $R/idle.log
  sleep 5; r=$(rss); c0=$(cpu); sleep 62; c1=$(cpu)
  echo "$N idle cameras: RSS $r KB; CPU over 62 s: $(echo "scale=2; ($c1-$c0)/62" | bc)% of core 0"
  kill $L; sleep 5
done
kill $P; sleep 1; start_server
echo "--- sessions (1000 cameras, server on 1 core)"
taskset -c 1-3 $S/zzload -mode sessions -n 1000 -workers 64 -dur 30s > $R/sess.log 2>&1 & L=$!
until rg -q "connected 1000|connected 9" $R/sess.log; do sleep 0.5; done
c0=$(cpu); t0=$(date +%s.%N)
wait $L
c1=$(cpu); t1=$(date +%s.%N); cat $R/sess.log
echo "server CPU during session phase: $(echo "scale=1; ($c1-$c0)/($t1-$t0)" | bc)% of core 0; RSS $(rss) KB"
kill $P; sleep 1
echo "events $(du -sh $R/events | cut -f1), lines $(cat $R/events/*.jsonl | wc -l), bytes $(cat $R/events/*.jsonl | wc -c)"
