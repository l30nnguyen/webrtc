# Capacity test

Measures what one server can do; the results and the server counts are in
[design/capacity.md](../../design/capacity.md). Run on a test machine with
4 cores (the server gets core 0, the load cores 1-3), never against
production.

- `control-load.sh`: a control server with TLS, statistics and connection log;
  5,000 and 20,000 idle cameras (memory, CPU), then viewing sessions at full
  speed. Uses `src/control/cmd/loadtest`.
- `relay-load.sh`: matrix-relay; 1,000 sessions held (memory), then 100-400
  sessions at 1.6 Mbps with the viewer side relayed, and with both sides.
  Uses `src/relay/cmd/loadtest`.
- `bench/`: the same one-core benchmark on the test machine and the target
  server, to scale the CPU results:
  `cd bench && CGO_ENABLED=0 go build -buildvcs=false -o bench . && ./bench`
  (copy the binary to the server, run it, delete it; about 5 s of CPU).

`WORK=DIR ./control-load.sh` keeps the server's files in DIR.
