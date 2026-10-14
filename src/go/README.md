# Go Signaling Server

High-performance WebSocket signaling server for WebRTC peer connections with HTTP REST API.

## Features

- **Single binary** - no dependencies, easy deployment
- **High performance** - 5-10x more connections than Python version
- **Built-in TLS** - no nginx required
- **Serves player** - ws.html available at `/`
- **Direct close propagation** - clients detect disconnections immediately

## Installation

```bash
cd src/go
go mod download
```

## SID licensing and Center

The authoritative SID format, issuance and key management, camera activation and viewing, Center configuration, and password handling are documented in [SID licensing architecture](../../design/sid_licensing_architecture_v2.md). Center APIs run on the ordinary `signaling-server` binary and its configured HTTPS/WSS listener; there is no standalone Center executable.

## Usage

### Build MainServer

```bash
go build -o signaling-server ./src
```

### Start the server

```bash
# With HTTPS (uses cert.pem from root directory)
./signaling-server -root ../.. -port 8000 -https

# Without HTTPS (HTTP only)
./signaling-server -root ../.. -port 8000
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-port` | 8000 | Listen port |
| `-root` | "" | Root directory for config, downloads, and cert.pem |
| `-https` | false | Enable HTTPS (uses cert.pem from root directory) |
| `-ticket-public-key` | disabled | Center PKIX Ed25519 public-key PEM; enables ticket admission for every client |
| `-ticket-kid` | disabled | Required key ID pinned in Center-issued JWT tickets |
| `-ticket-aud` | disabled | Exact WSS root URL audience, such as `wss://signal.example.com:8000/` |
| `-blocklist-file` | disabled | Required blocklist (SID digests, `tenant:NAME` lines, and `device:ID EXPIRY` lines that Center writes on release); can be empty. Nodes also drop open connections that become blocked, on a 30 s sweep |
| `-admin-token-file` | disabled | File with the pairing system's bearer token (32+ bytes, mode 0600). With `-center` it enables `POST /v1/admin/status` and `POST /v1/admin/release` (body `{"sid_digest":"<hex>"}`); without it those routes return 404 |
| `-demo-tenant` | disabled | DEV ONLY: unauthenticated `Player*` clients join this tenant |

### Run without building

```bash
go run ./src -root ../.. -port 8000 -https
```

## API Endpoints

| Endpoint | Description |
|----------|-------------|
| `WS /{client_id}` | WebSocket signaling endpoint |
| `GET /api/devices` | List connected devices (query: `?type=producer`); with tenancy it lists only the demo tenant and is hidden if there is none |
| `GET /api/health` | Health check with client count |
| `GET /` | WebRTC Player (ws.html) |
| `GET /downloads/{filename}` | Download files from downloads folder |

## Capacity

| RAM | Max Connections | Safe Production |
|-----|----------------|-----------------|
| 2GB | ~100,000+ | ~50,000-80,000 |
| 4GB | ~200,000+ | ~100,000-150,000 |

See [root README](../../README.md) for detailed capacity analysis.

## PM2 Integration

```bash
pm2 start pm2_start.json
```

## Advantages over Python

- **5-10x less memory per connection** (15KB vs 100KB)
- **Multi-core utilization** (Python is single-threaded)
- **Lower GC pressure** (no Python object overhead)
- **Faster message routing** (compiled vs interpreted)
- **No nginx required** (built-in TLS)
