# WebRTC Signaling Server

Python WebSocket signaling server for WebRTC peer connections with HTTP REST API.

## Capacity Estimation

**Hardware:** EC2 t3.micro (1GB RAM) / t3.small (2GB RAM)

### Python (Current Implementation)

| RAM | Max Connections | Safe Production |
|-----|----------------|-----------------|
| 1GB | ~8,000 | ~2,000-5,000 |
| 2GB | ~17,000 | ~10,000-12,000 |

### Go (Recommended for High Scale)

| RAM | Max Connections | Safe Production |
|-----|----------------|-----------------|
| 1GB | ~50,000 | ~30,000-40,000 |
| 2GB | ~100,000+ | ~50,000-80,000 |

**Why Go is 5-10x more efficient:**

| Metric | Python | Go |
|--------|--------|-----|
| Per-connection memory | ~80-100 KB | ~10-20 KB |
| Concurrency overhead | ~10 KB | ~2-4 KB |
| Network buffers | ~64 KB | ~8-16 KB |
| Concurrency model | Single-threaded (GIL) | True parallel (goroutines) |
| CPU utilization | 1 core max | All cores |
| Message routing | Interpreted | Compiled |

For signaling servers with many idle connections, Go is ideal. Libraries like `gorilla/websocket` handle 100K+ connections easily.

### Resource Breakdown (Python)

| Resource | Limit | Calculation |
|----------|-------|-------------|
| RAM | ~8,000 | 1GB - 200MB (OS) - 40MB (Python) = 760MB / ~100KB per conn |
| File descriptors | ~1,000 | Default ulimit 1024 (must raise to 65536) |
| CPU | Not bottleneck | Single-threaded asyncio; keepalive = 1 ping/30s/conn |
| Network | Not bottleneck | Signaling messages are tiny (~1-2KB) |

### Per-Connection Memory

| Component | Size |
|-----------|------|
| WebSocket protocol object | ~10 KB |
| asyncio task (keepalive_loop) | ~2 KB |
| TCP socket buffers (OS) | ~64 KB |
| dict entries (clients + device_info) | ~1 KB |
| **Total** | **~80-100 KB** |

### Practical Limits

| Scenario | Max Connections |
|----------|----------------|
| Without raising ulimit | ~900 (fd-limited) |
| With `ulimit -n 65536` | ~5,000 (RAM-limited) |
| Safe production number | ~2,000-3,000 (headroom for spikes) |

### Why CPU Is Not a Concern

The workload is I/O-bound, not compute-bound:
- 5,000 connections x 1 ping/30s = ~167 pings/sec (trivial for asyncio)
- Signaling messages are infrequent (only during connection setup)
- No media processing — the server only relays small JSON messages

### Bottleneck Order

```
file descriptors → RAM → CPU
```

To increase capacity:
1. Raise file descriptor limit: `ulimit -n 65536`
2. Add more RAM (each additional 1GB ≈ +8,000 connections)
3. CPU upgrade has minimal impact (already sufficient)

## API Endpoints

| Endpoint | Description |
|----------|-------------|
| `WS /{client_id}` | WebSocket signaling endpoint |
| `GET /api/devices` | List connected devices (query: `?type=producer`) |
| `GET /api/health` | Health check with client count |
| `GET /downloads/{filename}` | Download files from downloads folder |
| `GET /` | WebRTC Player (ws.html) |

## Usage

### Python Version

```bash
cd src/python
python src/signaling-server.py [ws_port] [http_port] [ssl_cert]
```

Example:
```bash
python src/signaling-server.py 8000 8080 cert.pem
```

See [src/python/README.md](src/python/README.md) for details.

### Go Version (Recommended)

```bash
cd src/go
go mod download
go run src/signaling-server.go -port 8000 -cert cert.pem -player ../../player/ws.html
```

Build binary:
```bash
cd src/go
go build -o signaling-server src/signaling-server.go
./signaling-server -port 8000 -cert cert.pem -player ../../player/ws.html
```

Flags:
- `-port`: Listen port (default: 8000)
- `-cert`: TLS cert+key PEM file (optional, enables WSS)
- `-player`: Path to ws.html (default: player/ws.html)

See [src/go/README.md](src/go/README.md) for details.

## PM2 Integration

```bash
pm2 start pm2_start.json
```

The default configuration uses the Go server.

## TLS certificate renewal

`renewcert.sh` checks and renews `webrtc.5gen.care` through nginx's HTTP-01
webroot at `/var/www/certbot`, then atomically writes `cert.pem` as the current
`fullchain.pem` plus `privkey.pem`. It uses an explicit
`certbot certonly --webroot` request every time, rather than `certbot renew`,
so a stale standalone renewal configuration cannot conflict with nginx on port
80. Before starting Certbot, it creates the challenge directory if needed and
verifies that a local TCP listener is active on port 80. It restarts the PM2
applications only when that generated file differs from the one currently
served.

Run it daily as root. It restarts PM2 as the owner of the deployment directory
(for example, `admin` for `/home/admin/webrtc`), so it uses that user's PM2
daemon rather than creating `/root/.pm2`:

```bash
0 0 * * * /home/leon/code/webrtc/server/renewcert.sh >> /var/log/webrtc-cert-renewal.log 2>&1
```

The deployed nginx site must serve this path from the same webroot:

```nginx
location /.well-known/acme-challenge/ {
    root /var/www/certbot;
}
```

If an earlier deployment failed with `certbot webroot does not exist`, pull the
updated script and run it again as root. It creates
`/var/www/certbot/.well-known/acme-challenge` safely when absent:

```bash
git pull
sudo ./renewcert.sh
```

If it reports no listener on port 80, start or repair nginx before retrying.
The command is safe to use to inspect that listener:

```bash
sudo ss -ltnp 'sport = :80'
sudo ./renewcert.sh
```

The script restarts `webrtc_prod` and `webrtc_dev` by default because nginx
redirects public HTTP traffic to port 8443. On a host that uses different
process names, set them explicitly, for example
`PM2_APPS=signaling-prod renewcert.sh`.

If the script is deployed in a root-owned directory but PM2 belongs to another
account, set that account explicitly. This also supports a deliberate
root-owned PM2 deployment:

```bash
sudo PM2_USER=admin /home/admin/webrtc/server/renewcert.sh
sudo PM2_USER=root PM2_USER_HOME=/root PM2_HOME=/root/.pm2 ./renewcert.sh
```

If an older version published `cert.pem` but failed to restart PM2, the
running Go server still presents its old in-memory certificate. Repair that
state once after pulling this update:

```bash
sudo FORCE_RESTART=1 ./renewcert.sh
```

The script records a pending restart after each certificate publication and
retries it on the next run if PM2 fails. `FORCE_RESTART=1` is only needed to
recover publications made by the older script, which did not leave that marker.

For compatibility the generated PEM keeps mode `644`. If the signaling server
runs under a dedicated group, set `CERT_MODE=640` and grant that group read
access to the file.
