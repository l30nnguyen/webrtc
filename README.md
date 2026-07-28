# WebRTC Signaling Server

Python WebSocket signaling server for WebRTC peer connections with HTTP REST API.

## Capacity Estimation

**Hardware:** EC2 t3.micro

**Estimate: ~2,000-5,000 concurrent connections** (safe), up to ~8,000 theoretical max.

### Resource Breakdown

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
| `GET /devices` | List connected devices (query: `?type=producer`) |
| `GET /health` | Health check with client count |

## Usage

```bash
python src/signaling-server.py [ws_port] [http_port] [ssl_cert]
```

Example:
```bash
python src/signaling-server.py 8000 8080 cert.pem
```
