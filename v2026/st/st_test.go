package st

import (
	"os"
	"strings"
	"testing"
)

// fakeResource is a stResource whose accessors are backed by maps, mimicking
// *server.SimpleResource: RequireString returns the (already-interpolated)
// value and panics when a key is missing (as the real one does on a missing
// {{ env: }} var).
type fakeResource struct {
	strings map[string]string
	lists   map[string][]string
	bools   map[string]bool
}

func key(path []string) string { return strings.Join(path, ".") }

func (f *fakeResource) RequireString(path ...string) string {
	v, ok := f.strings[key(path)]
	if !ok {
		panic("missing value for " + key(path))
	}
	return v
}

func (f *fakeResource) StringList(path ...string) []string { return f.lists[key(path)] }

func (f *fakeResource) Bool(path ...string) []bool {
	if v, ok := f.bools[key(path)]; ok {
		return []bool{v}
	}
	return nil
}

// TestConnectionFromAuthority is the standard path: the interpolated authority
// yields plain http/ws for the LAN gateway.
func TestConnectionFromAuthority(t *testing.T) {
	res := &fakeResource{strings: map[string]string{"authority": "10.1.2.3:9944"}}

	conn, err := connectionFromResource(res)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Authority != "10.1.2.3:9944" {
		t.Fatalf("authority = %q", conn.Authority)
	}
	if len(conn.RpcUrls) != 1 || conn.RpcUrls[0] != "http://10.1.2.3:9944" {
		t.Fatalf("rpc_urls = %v, want [http://10.1.2.3:9944]", conn.RpcUrls)
	}
	if conn.WsUrl != "ws://10.1.2.3:9944" {
		t.Fatalf("ws_url = %q, want ws://10.1.2.3:9944", conn.WsUrl)
	}
}

func TestConnectionWithLightnodeAuthority(t *testing.T) {
	res := &fakeResource{strings: map[string]string{
		"authority":           "10.1.2.3:9944",
		"lightnode-authority": "10.1.2.3:9946",
	}}

	conn, err := connectionFromResource(res)
	if err != nil {
		t.Fatal(err)
	}
	if conn.LightnodeAuthority != "10.1.2.3:9946" {
		t.Fatalf("lightnode authority = %q", conn.LightnodeAuthority)
	}
	if len(conn.LightnodeRpcUrls) != 1 || conn.LightnodeRpcUrls[0] != "http://10.1.2.3:9946" {
		t.Fatalf("lightnode rpc urls = %v", conn.LightnodeRpcUrls)
	}
	if conn.LightnodeWsUrl != "ws://10.1.2.3:9946" {
		t.Fatalf("lightnode ws url = %q", conn.LightnodeWsUrl)
	}
}

func TestConnectionLightnodeBareHostDefaultPort(t *testing.T) {
	res := &fakeResource{strings: map[string]string{
		"authority":           "archive.internal:9944",
		"lightnode-authority": "light.internal",
	}}
	conn, err := connectionFromResource(res)
	if err != nil {
		t.Fatal(err)
	}
	want := "light.internal:" + DefaultLightnodeGatewayPort
	if conn.LightnodeAuthority != want || conn.LightnodeRpcUrls[0] != "http://"+want {
		t.Fatalf("lightnode endpoints = %q / %v, want %q", conn.LightnodeAuthority, conn.LightnodeRpcUrls, want)
	}
}

// TestConnectionTls flips the scheme to https/wss.
func TestConnectionTls(t *testing.T) {
	res := &fakeResource{
		strings: map[string]string{
			"authority":           "rpc.example.com:443",
			"lightnode-authority": "light-rpc.example.com:443",
		},
		bools: map[string]bool{"tls": true},
	}
	conn, err := connectionFromResource(res)
	if err != nil {
		t.Fatal(err)
	}
	if conn.RpcUrls[0] != "https://rpc.example.com:443" || conn.WsUrl != "wss://rpc.example.com:443" {
		t.Fatalf("tls endpoints = %v / %q", conn.RpcUrls, conn.WsUrl)
	}
	if conn.LightnodeRpcUrls[0] != "https://light-rpc.example.com:443" || conn.LightnodeWsUrl != "wss://light-rpc.example.com:443" {
		t.Fatalf("lightnode tls endpoints = %v / %q", conn.LightnodeRpcUrls, conn.LightnodeWsUrl)
	}
}

// TestConnectionBareHostDefaultPort appends the gateway port to a bare host.
func TestConnectionBareHostDefaultPort(t *testing.T) {
	res := &fakeResource{strings: map[string]string{"authority": "snow.bringyour.com"}}
	conn, err := connectionFromResource(res)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Authority != "snow.bringyour.com:"+DefaultGatewayPort {
		t.Fatalf("authority = %q, want default port appended", conn.Authority)
	}
	if conn.RpcUrls[0] != "http://snow.bringyour.com:"+DefaultGatewayPort {
		t.Fatalf("rpc_urls = %v", conn.RpcUrls)
	}
}

// TestConnectionRpcUrlsOverride uses explicit rpc_urls without a threaded host
// (authority is unset here, so the standard path would panic — the override
// must win first).
func TestConnectionRpcUrlsOverride(t *testing.T) {
	res := &fakeResource{lists: map[string][]string{"rpc_urls": {"http://a:9944", " http://b:9944 "}}}
	conn, err := connectionFromResource(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(conn.RpcUrls) != 2 || conn.RpcUrls[0] != "http://a:9944" || conn.RpcUrls[1] != "http://b:9944" {
		t.Fatalf("rpc_urls override = %v", conn.RpcUrls)
	}
}

func TestConnectionLightnodeRpcUrlsOverride(t *testing.T) {
	res := &fakeResource{
		strings: map[string]string{"authority": "archive.internal:9944"},
		lists: map[string][]string{
			"lightnode-rpc_urls": {"https://light-a.example", " https://light-b.example "},
			"lightnode-ws_url":   {"wss://light-ws.example"},
		},
	}
	conn, err := connectionFromResource(res)
	if err != nil {
		t.Fatal(err)
	}
	if len(conn.LightnodeRpcUrls) != 2 || conn.LightnodeRpcUrls[1] != "https://light-b.example" {
		t.Fatalf("lightnode rpc override = %v", conn.LightnodeRpcUrls)
	}
	if conn.LightnodeWsUrl != "wss://light-ws.example" {
		t.Fatalf("lightnode ws override = %q", conn.LightnodeWsUrl)
	}
}

// TestConnectionMissingHostname is tolerant: an absent authority (the real
// RequireString panics on a missing {{ env: }} var) yields an error, not a crash.
func TestConnectionMissingHostname(t *testing.T) {
	res := &fakeResource{} // no authority, no rpc_urls -> RequireString panics
	conn, err := connectionFromResource(res)
	if err == nil {
		t.Fatalf("expected error for missing authority, got %+v", conn)
	}
}

func TestTestnetProfileNeverFallsBackToMainnet(t *testing.T) {
	res := &fakeResource{
		strings: map[string]string{
			"authority":                   "mainnet.internal:9944",
			"lightnode-authority":         "mainnet.internal:9946",
			"testnet-authority":           "testnet.internal:9944",
			"testnet-lightnode-authority": "testnet.internal:9946",
		},
		lists: map[string][]string{
			"rpc_urls":         {"https://mainnet.invalid"},
			"testnet-rpc_urls": {"https://testnet.example"},
		},
	}
	conn, err := connectionFromResourceProfile(res, ProfileTestnet)
	if err != nil {
		t.Fatal(err)
	}
	if len(conn.RpcUrls) != 1 || conn.RpcUrls[0] != "https://testnet.example" {
		t.Fatalf("testnet rpc urls = %v", conn.RpcUrls)
	}
	if conn.Authority != "testnet.internal:9944" {
		t.Fatalf("testnet authority = %q", conn.Authority)
	}
	if conn.LightnodeAuthority != "testnet.internal:9946" {
		t.Fatalf("testnet lightnode authority = %q", conn.LightnodeAuthority)
	}
}

func TestTestnetProfileMissingKeysDoesNotUseMainnet(t *testing.T) {
	res := &fakeResource{
		strings: map[string]string{
			"authority":           "mainnet.internal:9944",
			"lightnode-authority": "mainnet.internal:9946",
		},
		lists: map[string][]string{"rpc_urls": {"https://mainnet.invalid"}},
	}
	if conn, err := connectionFromResourceProfile(res, ProfileTestnet); err == nil {
		t.Fatalf("missing testnet keys unexpectedly resolved %+v", conn)
	}
}

func TestTestnetLightnodeDoesNotFallBackToMainnet(t *testing.T) {
	res := &fakeResource{strings: map[string]string{
		"testnet-authority":   "testnet.internal:9944",
		"lightnode-authority": "mainnet.internal:9946",
	}}
	conn, err := connectionFromResourceProfile(res, ProfileTestnet)
	if err != nil {
		t.Fatal(err)
	}
	if conn.LightnodeAuthority != "" || len(conn.LightnodeRpcUrls) != 0 || conn.LightnodeWsUrl != "" {
		t.Fatalf("testnet inherited mainnet lightnode endpoints: %+v", conn)
	}
}

func TestActiveProfileIsExplicit(t *testing.T) {
	old, had := os.LookupEnv(ProfileEnvironment)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(ProfileEnvironment, old)
		} else {
			_ = os.Unsetenv(ProfileEnvironment)
		}
	})
	_ = os.Unsetenv(ProfileEnvironment)
	if _, err := ActiveProfile(); err == nil {
		t.Fatal("missing profile accepted")
	}
	_ = os.Setenv(ProfileEnvironment, ProfileTestnet)
	if got, err := ActiveProfile(); err != nil || got != ProfileTestnet {
		t.Fatalf("profile = %q, %v", got, err)
	}
	_ = os.Setenv(ProfileEnvironment, "staging")
	if _, err := ActiveProfile(); err == nil {
		t.Fatal("unknown profile accepted")
	}
}
