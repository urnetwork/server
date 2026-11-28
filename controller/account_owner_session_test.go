package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// An API key, an OAuth/MCP identity, a client token and a guest are refused;
// only a network token passes.
func TestAccountOwnerSessionIsTheNetworksOwnSignIn(t *testing.T) {
	networkJwt := func() *session.ByJwt {
		return &session.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()}
	}
	clientId := server.NewId()
	cases := []struct {
		name    string
		session func() *session.ClientSession
		owner   bool
	}{
		{"a network token", func() *session.ClientSession {
			return session.Testing_CreateClientSession(context.Background(), networkJwt())
		}, true},
		{"an api key", func() *session.ClientSession {
			clientSession := session.Testing_CreateClientSession(context.Background(), networkJwt())
			clientSession.ApiKeyAuthenticated = true
			return clientSession
		}, false},
		{"an oauth identity", func() *session.ClientSession {
			clientSession := session.Testing_CreateClientSession(context.Background(), networkJwt())
			clientSession.UnsignedIdentity = true
			return clientSession
		}, false},
		{"a client token", func() *session.ClientSession {
			byJwt := networkJwt()
			byJwt.ClientId = &clientId
			return session.Testing_CreateClientSession(context.Background(), byJwt)
		}, false},
		{"a guest", func() *session.ClientSession {
			byJwt := networkJwt()
			byJwt.GuestMode = true
			return session.Testing_CreateClientSession(context.Background(), byJwt)
		}, false},
		{"a lag guest", func() *session.ClientSession {
			byJwt := networkJwt()
			byJwt.LagGuest = true
			return session.Testing_CreateClientSession(context.Background(), byJwt)
		}, false},
		{"no session", func() *session.ClientSession { return nil }, false},
	}
	for _, c := range cases {
		if owner := accountOwnerSession(c.session()); owner != c.owner {
			t.Errorf("%s: owner = %t, want %t", c.name, owner, c.owner)
		}
	}
}
