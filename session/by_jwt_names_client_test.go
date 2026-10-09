package session

import (
	"testing"

	"github.com/urnetwork/server"
)

// The unverified client peek answers from the claims alone: a network token
// does not name a client, a client token does, and anything that does not
// parse names nothing.
func TestByJwtNamesClientUnverified(t *testing.T) {
	network := NewByJwt(server.NewId(), server.NewId(), "peek", false, false)
	if ByJwtNamesClientUnverified(network.Testing_Sign()) {
		t.Fatal("a network token names a client")
	}
	if !ByJwtNamesClientUnverified(network.Client(server.NewId(), server.NewId()).Testing_Sign()) {
		t.Fatal("a client token does not name a client")
	}
	for _, token := range []string{"", "not.a.jwt", "urn_" + "abc"} {
		if ByJwtNamesClientUnverified(token) {
			t.Fatalf("%q names a client", token)
		}
	}
}
