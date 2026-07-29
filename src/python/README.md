# Python Signaling Server

WebSocket signaling server for WebRTC peer connections with HTTP REST API.

## Installation

```bash
cd src/python
python3 -m venv venv
source venv/bin/activate
pip install -r requirement.txt
```

## Usage

### Start the server

```bash
# Basic usage (no TLS)
python src/signaling-server.py [ws_port] [http_port]

# With TLS certificate
python src/signaling-server.py [ws_port] [http_port] [ssl_cert]

# Example
python src/signaling-server.py 8000 8080 cert.pem
```

### API Endpoints

| Endpoint | Description |
|----------|-------------|
| `WS /{client_id}` | WebSocket signaling endpoint |
| `GET /devices` | List connected devices (query: `?type=producer`) |
| `GET /health` | Health check with client count |

## Capacity

| RAM | Max Connections | Safe Production |
|-----|----------------|-----------------|
| 1GB | ~8,000 | ~2,000-5,000 |
| 2GB | ~17,000 | ~10,000-12,000 |

See [root README](../../README.md) for detailed capacity analysis.
