package session

import (
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server"
)

// A session carries the host its request was addressed to, which is how an
// extender activation knows the family api host it was posted to (support
// inbox 8083), and a view with other claims keeps it.
func TestClientSessionCarriesTheRequestHost(t *testing.T) {
	req := httptest.NewRequest("POST", "/network/extender-activate", nil)
	req.Host = "api-v4.bringyour.com"
	req.RemoteAddr = "[2001:db8::1]:41000"

	clientSession, err := NewClientSessionFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Cancel()
	if clientSession.Host != "api-v4.bringyour.com" {
		t.Fatalf("session host = %q, want the request's api-v4.bringyour.com", clientSession.Host)
	}

	view := clientSession.WithByJwt(&ByJwt{NetworkId: server.NewId(), UserId: server.NewId()})
	if view.Host != "api-v4.bringyour.com" {
		t.Fatalf("the claims view dropped the host: %q", view.Host)
	}
}
