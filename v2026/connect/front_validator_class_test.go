package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/nginx"
	"github.com/urnetwork/server/v2026/session"
)

// An api request whose bearer verifies as a client, beside a class token that
// verifies for that same client, is a validator's. A forged token, another
// client's token, a network bearer, a connect request and a class token
// without a bearer never are. Before the class, every verified client was
// charged alike.
func TestFrontRequestIdentifierChargesValidatorClass(t *testing.T) {
	clientId := server.NewId()
	otherClientId := server.NewId()
	networkId := server.NewId()
	parse := &testFrontParse{
		byJwts: map[string]*session.ByJwt{
			"client.token.sig":  {NetworkId: networkId, ClientId: &clientId},
			"network.token.sig": {NetworkId: networkId},
		},
	}
	key := make([]byte, 32)
	key[0] = 7
	now := time.Unix(1_800_000_000, 0)
	identifier := &frontRequestIdentifier{
		parseByJwt:     parse.parse,
		validatorClass: session.Testing_NewValidatorClassVerifier(key),
		now:            func() time.Time { return now },
	}
	mint := func(signingKey []byte, id server.Id) string {
		token, err := session.Testing_SignValidatorClass(signingKey, session.ValidatorClassClaims{ClientId: id, NetworkId: networkId,
			IssuedAt: now.Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	forgedKey := make([]byte, 32)
	forgedKey[0] = 8
	for _, c := range []struct {
		name          string
		authorization string
		class         string
		upstream      nginx.NameClass
		identity      nginx.LimitIdentity
		identified    bool
	}{
		{name: "a validator's api request", authorization: "Bearer client.token.sig", class: mint(key, clientId), upstream: nginx.NameClassApi, identity: nginx.LimitIdentity{Id: clientId, Validator: true}, identified: true},
		{name: "a forged class token", authorization: "Bearer client.token.sig", class: mint(forgedKey, clientId), upstream: nginx.NameClassApi, identity: nginx.LimitIdentity{Id: clientId}, identified: true},
		{name: "another client's class token", authorization: "Bearer client.token.sig", class: mint(key, otherClientId), upstream: nginx.NameClassApi, identity: nginx.LimitIdentity{Id: clientId}, identified: true},
		{name: "junk in the class header", authorization: "Bearer client.token.sig", class: "junk", upstream: nginx.NameClassApi, identity: nginx.LimitIdentity{Id: clientId}, identified: true},
		{name: "a network bearer", authorization: "Bearer network.token.sig", class: mint(key, clientId), upstream: nginx.NameClassApi, identity: nginx.LimitIdentity{Id: networkId, Network: true}, identified: true},
		{name: "a connect request", authorization: "Bearer client.token.sig", class: mint(key, clientId), upstream: nginx.NameClassConnect, identity: nginx.LimitIdentity{Id: clientId}, identified: true},
		{name: "a class token without a bearer", authorization: "", class: mint(key, clientId), upstream: nginx.NameClassApi},
	} {
		r := newTestFrontIdentityRequest(c.authorization)
		r.Header.Set(session.ValidatorClassHeader, c.class)
		identity, identified := identifier.identify(r, c.upstream)
		if identified != c.identified || identity != c.identity {
			t.Errorf("%s: identity %+v %t, want %+v %t", c.name, identity, identified, c.identity, c.identified)
		}
	}
}
