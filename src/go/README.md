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
# Basic usage (no TLS)
./signaling-server -port 8000 -player ../player/ws.html

# With TLS certificate (combined cert+key PEM file)
./signaling-server -port 8000 -cert cert.pem -player ../player/ws.html
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-port` | 8000 | Listen port |
| `-cert` | "" | TLS cert+key PEM file (enables WSS) |
| `-player` | "player/ws.html" | Path to ws.html |

### Run without building

```bash
go run src/signaling-server.go -port 8000 -cert cert.pem -player ../player/ws.html
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
