// Package st resolves the UR subnet's subtensor connection from the standard
// vault resource st.yml. The subtensor host is threaded exactly the way pg/redis
// are (server/db.go, server/redis.go): config/<env>/settings.yml sets
// BRINGYOUR_SUBTENSOR_HOSTNAME, st.yml's `authority` interpolates it
// ("{{ env:BRINGYOUR_SUBTENSOR_HOSTNAME }}:9944"), and this package turns that
// authority into the RPC endpoints the chain client dials.
//
// One subtensor node/gateway (host "snow") serves BOTH the substrate WS and the
// EVM JSON-RPC (docs/LAUNCH.md §0), so both endpoints derive from the single
// authority. The server's ST coordination uses the EVM JSON-RPC (RpcUrls); the
// substrate WS (WsUrl) is exposed for completeness.
//
// server/controller/st_controller.go reads the rest of st.yml (contract address,
// netuid, hot wallets, deposit sizing) and sources its RPC endpoints from here,
// so the subtensor connection is resolved in exactly one place.
package st

import (
	"fmt"
	"strings"
	"sync"

	"github.com/urnetwork/server/v2026"
)

// VaultResourceName is the vault resource holding the ST subsystem config,
// including the threaded subtensor `authority` (vault/<env>/st.yml).
const VaultResourceName = "st.yml"

// DefaultGatewayPort is the subtensor RPC gateway port assumed when st.yml's
// authority omits one (the nginx gateway on snow — xops/main/ansible).
const DefaultGatewayPort = "9944"

// Connection is the subtensor endpoints derived from st.yml.
//
//   - Authority is the RPC gateway host:port (BRINGYOUR_SUBTENSOR_HOSTNAME + port).
//   - RpcUrls are the EVM JSON-RPC url(s) the chain client (ethclient) dials for
//     the ST contract — the explicit st.yml `rpc_urls` if set, else derived from
//     the authority.
//   - WsUrl is the substrate/EVM websocket url derived from the authority.
type Connection struct {
	Authority string
	RpcUrls   []string
	WsUrl     string
}

// resolveConnection reads st.yml once. It is deliberately tolerant: a missing
// st.yml, a missing `authority`, or a missing BRINGYOUR_SUBTENSOR_HOSTNAME
// (which the {{ env: }} interpolation panics on, server/env.go) yields an error
// rather than a crash — callers keep the ST subsystem disabled, matching the
// optional-st.yml contract in server/controller.
// stResource is the subset of *server.SimpleResource that connectionFromResource
// reads. Abstracting it keeps the endpoint-derivation logic unit-testable
// without the vault/disk harness (*server.SimpleResource satisfies it).
// RequireString interpolates {{ env: }} and panics if the value is absent.
type stResource interface {
	RequireString(path ...string) string
	StringList(path ...string) []string
	Bool(path ...string) []bool
}

var resolveConnection = sync.OnceValues(func() (*Connection, error) {
	res, err := server.Vault.SimpleResource(VaultResourceName)
	if err != nil {
		return nil, fmt.Errorf("st.yml unavailable: %w", err)
	}
	return connectionFromResource(res)
})

// connectionFromResource derives the subtensor endpoints from an st.yml resource.
// It is pure over the resource (no OnceValues cache) so it can be unit-tested,
// and tolerant: a missing `authority` or a missing BRINGYOUR_SUBTENSOR_HOSTNAME
// (which the {{ env: }} interpolation panics on, server/env.go) yields an error
// rather than a crash.
func connectionFromResource(res stResource) (conn *Connection, err error) {
	defer func() {
		if r := recover(); r != nil {
			conn = nil
			err = fmt.Errorf("st.yml subtensor connection unavailable: %v", r)
		}
	}()

	// scheme: the LAN gateway is plain http/ws (xops/main/ansible — no TLS);
	// set `tls: true` in st.yml for an https/wss endpoint.
	scheme, wsScheme := "http", "ws"
	if tls := res.Bool("tls"); len(tls) == 1 && tls[0] {
		scheme, wsScheme = "https", "wss"
	}

	conn = &Connection{}

	// An explicit `rpc_urls` list overrides the authority-derived default and
	// does not require the threaded host to be set.
	if urls := trimAll(res.StringList("rpc_urls")); len(urls) > 0 {
		conn.RpcUrls = urls
		conn.Authority = authorityBestEffort(res)
		if ws := trimAll(res.StringList("ws_url")); len(ws) > 0 {
			conn.WsUrl = ws[0]
		} else if conn.Authority != "" {
			conn.WsUrl = fmt.Sprintf("%s://%s", wsScheme, conn.Authority)
		}
		return conn, nil
	}

	// Standard path: derive the endpoints from the threaded authority.
	// RequireString interpolates {{ env:BRINGYOUR_SUBTENSOR_HOSTNAME }}
	// (UnmarshalYaml does not — server/env.go), and panics if the var is unset.
	authority := withDefaultPort(strings.TrimSpace(res.RequireString("authority")))
	if authority == "" {
		return nil, fmt.Errorf("st.yml authority is empty")
	}
	conn.Authority = authority
	conn.RpcUrls = []string{fmt.Sprintf("%s://%s", scheme, authority)}
	conn.WsUrl = fmt.Sprintf("%s://%s", wsScheme, authority)
	return conn, nil
}

// GetConnection returns the resolved subtensor connection, or an error when
// st.yml / BRINGYOUR_SUBTENSOR_HOSTNAME is not configured.
func GetConnection() (*Connection, error) {
	return resolveConnection()
}

// RpcUrls returns the EVM JSON-RPC url(s) the chain client should dial, or nil
// when the connection is not configured.
func RpcUrls() []string {
	conn, err := resolveConnection()
	if err != nil {
		return nil
	}
	return conn.RpcUrls
}

// Authority returns the subtensor gateway host:port, or "" when not configured.
func Authority() string {
	conn, err := resolveConnection()
	if err != nil {
		return ""
	}
	return conn.Authority
}

// authorityBestEffort reads `authority` without letting a missing threaded host
// abort an otherwise-valid explicit rpc_urls config.
func authorityBestEffort(res stResource) (authority string) {
	defer func() { _ = recover() }()
	return withDefaultPort(strings.TrimSpace(res.RequireString("authority")))
}

// withDefaultPort appends DefaultGatewayPort when the authority has a bare host.
func withDefaultPort(authority string) string {
	if authority == "" || strings.Contains(authority, ":") {
		return authority
	}
	return authority + ":" + DefaultGatewayPort
}

func trimAll(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
