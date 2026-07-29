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

## Usage

### Build binary

```bash
go build -o signaling-server src/signaling-server.go
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

### Run without building

```bash
go run src/signaling-server.go -root ../.. -port 8000 -https
```

## API Endpoints

| Endpoint | Description |
|----------|-------------|
| `WS /{client_id}` | WebSocket signaling endpoint |
| `GET /api/devices` | List connected devices (query: `?type=producer`) |
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
