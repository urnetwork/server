# HTTP Proxy Frontend for VPN

This project provides an HTTP proxy server that routes all network connections through our VPN service. It allows you to securely tunnel your traffic through the VPN by configuring your system or applications to use this proxy.

## Features

- HTTP/HTTPS proxy support
- Automatic VPN connection management
- Rate limiting
- TLS encryption
- Redis-based session management
- Connection statistics monitoring

## Running the Service

### Environment Variables

- `ADDR`: Server address (default: `:30001`)
- `API_URL`: API endpoint URL (default: `https://api.bringyour.com`)
- `PLATFORM_URL`: Platform WebSocket URL (default: `wss://connect.bringyour.com`)
- `PROXY_ADDR`: Proxy listener address (default: `:10000`)
- `CERT_FILE`: Path to TLS certificate file (required)
- `KEY_FILE`: Path to TLS private key file (required)
- `REDIS_ADDR`: Redis server address (required)
- `REDIS_PASSWORD`: Redis password (optional)
- `REDIS_DB`: Redis database number (default: 0)
- `RATE_LIMIT_NETWORK_PER_MINUTE`: Rate limit for network connections per minute (default: 5)

## API Endpoints

### Add Client

Creates a new proxy client connection.

```http
POST /add-client
Content-Type: application/json

{
    "auth_code": "your-auth-code"
}
```

Response:
```json
{
    "host": "proxy.example.com",
    "port": 10000
}
```

### Get Status

Returns the current service status.

```http
GET /status
```

Response:
```json
{
    "version": "0.0.0",
    "config_version": "0.0.0",
    "status": "ok",
    "client_address": "192.168.1.100:12345",
    "host": "proxy-server-1"
}
```

### Get Proxy Stats

Returns proxy connection statistics.

```http
GET /proxy/stats
```

Response:
```json
{
    "connections_open": 5,
    "total_connections_count": 100,
    "bytes_sent": 1024,
    "bytes_received": 2048
}
```

## Security

- All connections are encrypted using TLS
- Rate limiting prevents abuse
- Authentication required for proxy access
- Session management via Redis
- Connection monitoring and statistics

## Development

Requirements:
- Go 1.23 or later
- Redis server
- TLS certificate and private key

Build the service:

```bash
go build -o service .
```

Run tests:

```bash
go test ./...
```
