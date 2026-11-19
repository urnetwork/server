package connect

import (
	"net/http"
	"testing"
)

// The handshake marks a proxied transport with the value 1 alone, in any
// header case: anything else, or nothing, is a transport from its host.
func TestEgressProxyFromHeader(t *testing.T) {
	for _, c := range []struct {
		name   string
		header http.Header
		want   bool
	}{
		{"declared", http.Header{"X-Ur-Egress-Proxy": {"1"}}, true},
		{"absent", http.Header{}, false},
		{"empty", http.Header{"X-Ur-Egress-Proxy": {""}}, false},
		{"other value", http.Header{"X-Ur-Egress-Proxy": {"true"}}, false},
	} {
		if got := egressProxyFromHeader(c.header); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// as a client sets it
	header := http.Header{}
	header.Set("X-UR-Egress-Proxy", "1")
	if !egressProxyFromHeader(header) {
		t.Errorf("a header the client set is not read")
	}
}
