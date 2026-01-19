# SOCKS5 Proxy Frontend for VPN

This project provides a SOCKS5 proxy server that routes all network connections through our VPN service. By running this proxy locally, you can configure your system or applications to use it, ensuring all your traffic is securely tunneled through the VPN.

## Features

- SOCKS5 proxy server implementation with VPN integration
- Support for both IPv4 and IPv6 traffic
- Flexible location selection (country, region, city)
- Provider-specific routing options
- Built-in DNS resolution through the VPN tunnel
- TCP and UDP protocol support
- Secure authentication system

## Usage

Run the proxy server with the following command:

```bash
go run . --user-auth <your-auth> --password <your-password> --country "United States"
```

Replace `<your-auth>` and `<your-password>` with your VPN authentication details.

### Command-Line Options

- `--user-auth`: Your VPN user auth.
- `--password`: Your VPN password.
- `--country`: *(Optional)* Country to connect to (e.g., "United States").
- `--region`: *(Optional)* Region within the country.
- `--city`: *(Optional)* Specific city to connect through.
- `--provider-id`: *(Optional)* Specific provider ID to connect to.
- `--addr`: *(Optional)* Bind address for the proxy server (default is `:9999`).
- `--api-url`: *(Optional)* Custom API URL (default is `https://api.prod.ur.network`).
- `--platform-url`: *(Optional)* Custom platform URL (default is `wss://connect.prod.ur.network`).

### Example: Connecting via Provider ID

To connect to a specific provider, use the `--provider-id` flag:

```bash
go run . --user-auth <your-auth> --password <your-password> --provider-id <provider-id>
```

## Configuring Your System Proxy

After starting the proxy server:

1. Go to your system's network settings.
2. Set the SOCKS5 proxy to `localhost` and port `9999` (or your custom port if specified).
3. Save the settings.

All your network traffic will now be routed through the VPN via the proxy.

## Technical Details

The proxy server:
- Uses a custom network stack for handling traffic
- Implements full SOCKS5 protocol specification
- Provides transparent DNS resolution through the VPN tunnel
- Supports both TCP and UDP protocols
- Handles IPv4 and IPv6 connections
- Maintains persistent VPN connections
- Provides automatic reconnection on network changes

## Requirements

- Go 1.23 or later
- Network connectivity to VPN servers
- Valid VPN authentication credentials

