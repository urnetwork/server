// Package st resolves the UR subnet's subtensor connection from the standard
// vault resource st.yml. The subtensor host is threaded exactly the way pg/redis
// are (server/db.go, server/redis.go): config/<env>/settings.yml sets
// BRINGYOUR_SUBTENSOR_HOSTNAME, and st.yml's `authority` / `lightnode-authority`
// interpolate it on ports 9944 / 9946. This package turns both authorities into
// the RPC endpoints chain clients dial.
//
// The archive and light nodes on "snow" each serve Substrate WS and EVM
// JSON-RPC. The primary authority remains the archive endpoint; the lightnode
// authority is resolved independently so callers can opt into recent-state
// traffic without weakening archive-backed consumers.
//
// server/controller/st_controller.go reads the rest of st.yml (contract address,
// netuid, hot wallets, deposit sizing) and sources its RPC endpoints from here,
// so the subtensor connection is resolved in exactly one place.
package st

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/urnetwork/server/v2026"
)

// VaultResourceName is the vault resource holding the ST subsystem config,
// including the threaded subtensor `authority` (vault/<env>/st.yml).
const VaultResourceName = "st.yml"

const ProfileEnvironment = "URNETWORK_ST_PROFILE"

const (
	ProfileTestnet = "testnet"
	ProfileMainnet = "mainnet"
)

// DefaultGatewayPort is the subtensor RPC gateway port assumed when st.yml's
// authority omits one (the nginx gateway on snow — xops/main/ansible).
const DefaultGatewayPort = "9944"

// DefaultLightnodeGatewayPort is the side-by-side lightnode gateway on snow.
const DefaultLightnodeGatewayPort = "9946"

// EventLogBlockRange is the inclusive eth_getLogs window used by settlement
// event indexing and release diagnostics. The official public testnet gateway
// accepts this one-thousand-block shape and rejects the former two-thousand
// block request.
const EventLogBlockRange uint64 = 1000

// Connection is the subtensor endpoints derived from st.yml.
//
//   - Authority is the RPC gateway host:port (BRINGYOUR_SUBTENSOR_HOSTNAME + port).
//   - RpcUrls are the EVM JSON-RPC url(s) the chain client (ethclient) dials for
//     the ST contract — the explicit st.yml `rpc_urls` if set, else derived from
//     the authority.
//   - WsUrl is the substrate/EVM websocket url derived from the authority.
//   - Lightnode* are the equivalent optional endpoints for the warp-synced
//     lightnode. They stay empty when st.yml does not configure one.
type Connection struct {
	Authority          string
	RpcUrls            []string
	WsUrl              string
	LightnodeAuthority string
	LightnodeRpcUrls   []string
	LightnodeWsUrl     string
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
	profile, err := ActiveProfile()
	if err != nil {
		return nil, err
	}
	return connectionFromResourceProfile(res, profile)
})

// ActiveProfile is deliberately explicit. Testnet services may only read
// testnet-* keys, while mainnet services may only read the unprefixed keys.
// Requiring the environment variable prevents a testnet process from silently
// inheriting funded mainnet credentials when a rendered setting is missing.
func ActiveProfile() (string, error) {
	profile := strings.TrimSpace(os.Getenv(ProfileEnvironment))
	switch profile {
	case ProfileTestnet, ProfileMainnet:
		return profile, nil
	default:
		return "", fmt.Errorf("%s must be exactly testnet or mainnet", ProfileEnvironment)
	}
}

func profileKey(profile, key string) string {
	if profile == ProfileTestnet {
		return "testnet-" + key
	}
	return key
}

// connectionFromResource derives the subtensor endpoints from an st.yml resource.
// It is pure over the resource (no OnceValues cache) so it can be unit-tested,
// and tolerant: a missing `authority` or a missing BRINGYOUR_SUBTENSOR_HOSTNAME
// (which the {{ env: }} interpolation panics on, server/env.go) yields an error
// rather than a crash.
func connectionFromResource(res stResource) (conn *Connection, err error) {
	return connectionFromResourceProfile(res, ProfileMainnet)
}

func connectionFromResourceProfile(res stResource, profile string) (conn *Connection, err error) {
	if profile != ProfileTestnet && profile != ProfileMainnet {
		return nil, fmt.Errorf("unknown st profile %q", profile)
	}
	defer func() {
		if r := recover(); r != nil {
			conn = nil
			err = fmt.Errorf("st.yml subtensor connection unavailable: %v", r)
		}
	}()

	// scheme: the LAN gateway is plain http/ws (xops/main/ansible — no TLS);
	// set `tls: true` in st.yml for an https/wss endpoint.
	scheme, wsScheme := "http", "ws"
	if tls := res.Bool(profileKey(profile, "tls")); len(tls) == 1 && tls[0] {
		scheme, wsScheme = "https", "wss"
	}

	primary, err := endpointFromResource(res, profile, "", DefaultGatewayPort, scheme, wsScheme, true)
	if err != nil {
		return nil, err
	}
	lightnode, err := endpointFromResource(res, profile, "lightnode", DefaultLightnodeGatewayPort, scheme, wsScheme, false)
	if err != nil {
		return nil, err
	}
	return &Connection{
		Authority:          primary.Authority,
		RpcUrls:            primary.RpcUrls,
		WsUrl:              primary.WsUrl,
		LightnodeAuthority: lightnode.Authority,
		LightnodeRpcUrls:   lightnode.RpcUrls,
		LightnodeWsUrl:     lightnode.WsUrl,
	}, nil
}

type rpcEndpoint struct {
	Authority string
	RpcUrls   []string
	WsUrl     string
}

// endpointFromResource resolves either the primary endpoint (prefix "") or an
// optional named endpoint such as "lightnode". Explicit *_rpc_urls win over an
// authority-derived URL, matching the primary connection contract.
func endpointFromResource(res stResource, profile, prefix, defaultPort, scheme, wsScheme string, required bool) (rpcEndpoint, error) {
	key := func(base string) string {
		if prefix == "" {
			return profileKey(profile, base)
		}
		return profileKey(profile, prefix+"-"+base)
	}

	authority := authorityBestEffortKey(res, key("authority"), defaultPort)
	urls := trimAll(res.StringList(key("rpc_urls")))
	wsUrls := trimAll(res.StringList(key("ws_url")))
	if len(urls) > 0 {
		endpoint := rpcEndpoint{Authority: authority, RpcUrls: urls}
		if len(wsUrls) > 0 {
			endpoint.WsUrl = wsUrls[0]
		} else if authority != "" {
			endpoint.WsUrl = fmt.Sprintf("%s://%s", wsScheme, authority)
		}
		return endpoint, nil
	}
	if authority == "" {
		if required {
			return rpcEndpoint{}, fmt.Errorf("st.yml %sauthority is empty", endpointErrorPrefix(prefix))
		}
		return rpcEndpoint{}, nil
	}
	endpoint := rpcEndpoint{
		Authority: authority,
		RpcUrls:   []string{fmt.Sprintf("%s://%s", scheme, authority)},
		WsUrl:     fmt.Sprintf("%s://%s", wsScheme, authority),
	}
	if len(wsUrls) > 0 {
		endpoint.WsUrl = wsUrls[0]
	}
	return endpoint, nil
}

func endpointErrorPrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	return prefix + " "
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

// LightnodeRpcUrls returns the lightnode's EVM JSON-RPC url(s), or nil when
// the connection or optional lightnode endpoint is not configured.
func LightnodeRpcUrls() []string {
	conn, err := resolveConnection()
	if err != nil {
		return nil
	}
	return conn.LightnodeRpcUrls
}

// Authority returns the subtensor gateway host:port, or "" when not configured.
func Authority() string {
	conn, err := resolveConnection()
	if err != nil {
		return ""
	}
	return conn.Authority
}

// LightnodeAuthority returns the lightnode gateway host:port, or "" when it is
// not configured.
func LightnodeAuthority() string {
	conn, err := resolveConnection()
	if err != nil {
		return ""
	}
	return conn.LightnodeAuthority
}

// authorityBestEffort reads `authority` without letting a missing threaded host
// abort an otherwise-valid explicit rpc_urls config.
func authorityBestEffort(res stResource) (authority string) {
	return authorityBestEffortProfile(res, ProfileMainnet)
}

func authorityBestEffortProfile(res stResource, profile string) (authority string) {
	return authorityBestEffortKey(res, profileKey(profile, "authority"), DefaultGatewayPort)
}

func authorityBestEffortKey(res stResource, key, defaultPort string) (authority string) {
	defer func() { _ = recover() }()
	return withDefaultPortFor(strings.TrimSpace(res.RequireString(key)), defaultPort)
}

// withDefaultPort appends DefaultGatewayPort when the authority has a bare host.
func withDefaultPort(authority string) string {
	return withDefaultPortFor(authority, DefaultGatewayPort)
}

func withDefaultPortFor(authority, defaultPort string) string {
	if authority == "" || strings.Contains(authority, ":") {
		return authority
	}
	return authority + ":" + defaultPort
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
