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
