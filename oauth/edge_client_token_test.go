package oauth

// An edge client's sockets token (edge_client_token.go; QD-1054): a service token of audience sockets under the
// svc- set, naming the client in urnetwork-edge and nothing else, for a service token's life, its end answered as
// the token carries it; no other audience takes it, and an issuer with no svc- key mints none.

import (
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
)

func TestEdgeClientSocketsTokenNamesTheClientAlone(t *testing.T) {
	keys := installTestSignerKeys(t)
	networkId := server.NewId()
	clientId := server.NewId()
	token, expireTime, err := MintEdgeClientSocketsToken(networkId, clientId)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := gojwt.NewParser().ParseUnverified(token, &ServiceTokenClaims{})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header["kid"] != keys.service.Kid {
		t.Fatalf("signed under %v, not the svc- key", parsed.Header["kid"])
	}
	claims, err := newTestServiceTokenVerifier(t).Verify(token, EdgeClientSocketsTokenAudience)
	if err != nil {
		t.Fatal(err)
	}
	if claims.NetworkId != networkId || claims.ClientId != clientId || claims.Subject != clientId.String() ||
		claims.Audience() != "sockets" || claims.Issuer != Issuer() {
		t.Fatalf("names %s %s %q aud %q", claims.NetworkId, claims.ClientId, claims.Subject, claims.Audience())
	}
	// no sidecar, no member, no subject client, no actor and no scope: an edge client has none of them
	if claims.SidecarId != nil || claims.MemberId != nil || claims.SubjectClientId != nil || claims.Act != nil ||
		claims.Scope != "" || claims.ProjVersion != 0 {
		t.Fatalf("claims more than its client: %+v", claims)
	}
	// its end as the token carries it, a service token's life after its iat
	if claims.ExpiresAt == nil || claims.IssuedAt == nil || !claims.ExpiresAt.Time.Equal(expireTime) ||
		claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time) != ServiceTokenDuration {
		t.Fatalf("exp %v, iat %v, answered %v", claims.ExpiresAt, claims.IssuedAt, expireTime)
	}
	if d := expireTime.Sub(server.NowUtc().Add(ServiceTokenDuration)); d < -time.Minute || time.Minute < d {
		t.Fatalf("ends %s", expireTime)
	}
	// another service's audience refuses it
	for _, audience := range []string{"evs", "gkm", "dns"} {
		if _, err := newTestServiceTokenVerifier(t).Verify(token, audience); err == nil {
			t.Errorf("%s took an edge client's sockets token", audience)
		}
	}
}

func TestEdgeClientSocketsTokenNeedsAnSvcKey(t *testing.T) {
	installTestIssuer(t)
	t.Cleanup(Testing_SetSignerKeys(newTestSignerKey(t, SignerKeySetMcp), newTestSignerKey(t, SignerKeySetAdmin)))
	if token, _, err := MintEdgeClientSocketsToken(server.NewId(), server.NewId()); err == nil || token != "" {
		t.Fatalf("minted with no svc- key: %v", err)
	}
}
