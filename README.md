# Arkitekt Sidecar

A lightweight Tailscale-based proxy sidecar that enables applications to access resources on a Tailnet without requiring Tailscale to be installed on the host machine.

## Features

- **HTTP Proxy Mode**: Standard HTTP/HTTPS proxy with CONNECT tunneling support
- **SOCKS5 Proxy Mode**: SOCKS5 proxy for broader application compatibility
- **Status API**: REST API to inspect connection status and peer information
- **IPC Signaling**: Magic word signals for integration with parent processes
- **Embedded Tailscale**: No system-wide Tailscale installation required

## Installation

```bash
go install arkitekt.live/arkitekt-sidecar@latest
```

Or build from source:

```bash
git clone https://github.com/jhnnsrs/arkitekt-sidecar.git
cd arkitekt-sidecar
go build .
```

## Usage

### Basic Usage

```bash
# HTTP proxy mode (default)
./arkitekt-sidecar -authkey YOUR_TAILSCALE_AUTH_KEY -coordserver https://your-control-server

# SOCKS5 proxy mode
./arkitekt-sidecar -authkey YOUR_AUTH_KEY -coordserver https://your-control-server -mode socks5

# With custom port and hostname
./arkitekt-sidecar -authkey YOUR_AUTH_KEY -coordserver https://your-control-server -port 1080 -hostname my-proxy
```

### Command Line Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-authkey` | (required) | Tailscale auth key for authentication |
| `-coordserver` | (required) | Coordination server URL |
| `-hostname` | `ts-proxy` | Hostname to use in the Tailnet |
| `-port` | `8080` | Port for the proxy to listen on |
| `-mode` | `http` | Proxy mode: `http` or `socks5` |
| `-statedir` | current directory | Directory to store Tailscale state |
| `-statusport` | (disabled) | Port for the status API (enables status API when set) |

### Using the Proxy

#### HTTP Proxy

Configure your application to use the HTTP proxy:

```bash
# Environment variables
export HTTP_PROXY=http://127.0.0.1:8080
export HTTPS_PROXY=http://127.0.0.1:8080

# curl example
curl -x http://127.0.0.1:8080 http://internal-service.tailnet/api

# Python requests
import requests
proxies = {"http": "http://127.0.0.1:8080", "https": "http://127.0.0.1:8080"}
response = requests.get("http://internal-service/api", proxies=proxies)
```

#### SOCKS5 Proxy

```bash
# curl with SOCKS5
curl --socks5 127.0.0.1:8080 http://internal-service.tailnet/api

# Python with PySocks
import socks
import socket
socks.set_default_proxy(socks.SOCKS5, "127.0.0.1", 8080)
socket.socket = socks.socksocket
```

## Status API

Enable the status API to inspect connection details:

```bash
./arkitekt-sidecar -authkey YOUR_KEY -coordserver URL -statusport 9090
```

### Endpoints

#### `GET /health`

Simple health check endpoint.

```bash
curl http://127.0.0.1:9090/health
# Response: OK
```

#### `GET /status`

Returns detailed connection status including peer information.

```bash
curl http://127.0.0.1:9090/status | jq
```

**Response:**

```json
{
  "self": {
    "name": "my-proxy.tailnet.ts.net",
    "hostname": "my-proxy",
    "tailscale_ips": ["100.64.0.1"],
    "online": true
  },
  "peers": [
    {
      "name": "server.tailnet.ts.net",
      "hostname": "server",
      "tailscale_ips": ["100.64.0.10"],
      "online": true,
      "direct": true,
      "relayed_via": "",
      "current_address": "192.168.1.100:41641",
      "rx_bytes": 12345,
      "tx_bytes": 67890,
      "last_seen": "2026-01-19T20:30:00Z",
      "last_handshake": "2026-01-19T20:29:55Z"
    }
  ],
  "backend_state": "Running"
}
```

**Key fields:**
- `direct: true` — Connection is peer-to-peer (best performance)
- `direct: false` + `relayed_via: "region"` — Traffic is relayed through DERP
- `current_address` — The actual IP:port when using direct connection

## IPC Signaling

The sidecar emits magic word signals to stdout for integration with parent processes (e.g., Python scripts):

| Signal | Description |
|--------|-------------|
| `@@SIDECAR:STARTING@@` | Sidecar is initializing |
| `@@SIDECAR:CONNECTING@@` | Connecting to Tailnet |
| `@@SIDECAR:CONNECTED@@` | Successfully connected (includes IPs) |
| `@@SIDECAR:LISTENING@@` | Proxy is listening |
| `@@SIDECAR:READY@@` | Fully ready to accept connections |
| `@@SIDECAR:ERROR@@` | An error occurred (includes details) |
| `@@SIDECAR:SHUTDOWN@@` | Graceful shutdown |
| `@@SIDECAR:AUTH_REQUIRED@@` | Authentication required |

### Example Output

```
Arkitekt Sidecar v0.1.0
@@SIDECAR:STARTING@@ v0.1.0
>>> Starting Tailscale Node 'my-proxy'...
@@SIDECAR:CONNECTING@@ my-proxy
>>> Tailscale is Online!
@@SIDECAR:CONNECTED@@ ips=[100.64.0.1]
>>> HTTP Proxy listening on 127.0.0.1:8080
@@SIDECAR:LISTENING@@ mode=http addr=127.0.0.1:8080
@@SIDECAR:READY@@ http://127.0.0.1:8080
```

### Python Integration Example

```python
import subprocess
import sys

proc = subprocess.Popen(
    ["./arkitekt-sidecar", "-authkey", "YOUR_KEY", "-coordserver", "URL"],
    stdout=subprocess.PIPE,
    stderr=subprocess.STDOUT,
    text=True,
    bufsize=1
)

proxy_url = None
for line in proc.stdout:
    print(line, end="")
    
    if "@@SIDECAR:READY@@" in line:
        proxy_url = line.split(" ", 1)[1].strip()
        print(f"Proxy ready at: {proxy_url}")
        break
    elif "@@SIDECAR:ERROR@@" in line:
        error = line.split(" ", 1)[1].strip()
        raise Exception(f"Sidecar error: {error}")

# Now use proxy_url in your application
```

## Development

### Running Tests

```bash
# Run unit tests
go test -v

# Run only unit tests (no integration)
go test -v -run "TestHandle|TestPeerStatus|TestStatusResponse"

# Run integration tests (requires .env file)
go test -v -run "TestIntegration"
```

### Environment Variables for Testing

Create a `.env` file:

```env
TEST_COORD_SERVER="https://your-control-server"
TEST_AUTH_KEY="your-test-auth-key"
TEST_SERVER="hostname-of-test-server"
```

Integration tests are automatically skipped on CI (when `GITHUB_ACTIONS=true` or `CI=true`).

## License

MIT License
