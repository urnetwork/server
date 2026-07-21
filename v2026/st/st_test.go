package st

import (
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

// TestConnectionTls flips the scheme to https/wss.
func TestConnectionTls(t *testing.T) {
	res := &fakeResource{
		strings: map[string]string{"authority": "rpc.example.com:443"},
		bools:   map[string]bool{"tls": true},
	}
	conn, err := connectionFromResource(res)
	if err != nil {
		t.Fatal(err)
	}
	if conn.RpcUrls[0] != "https://rpc.example.com:443" || conn.WsUrl != "wss://rpc.example.com:443" {
		t.Fatalf("tls endpoints = %v / %q", conn.RpcUrls, conn.WsUrl)
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

// TestConnectionMissingHostname is tolerant: an absent authority (the real
// RequireString panics on a missing {{ env: }} var) yields an error, not a crash.
func TestConnectionMissingHostname(t *testing.T) {
	res := &fakeResource{} // no authority, no rpc_urls -> RequireString panics
	conn, err := connectionFromResource(res)
	if err == nil {
		t.Fatalf("expected error for missing authority, got %+v", conn)
	}
}
