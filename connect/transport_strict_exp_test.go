package connect

import (
	"context"
	"net/http"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func newExpiredStrictExpToken() *session.ByJwt {
	network := session.NewByJwt(server.NewId(), server.NewId(), "strict-connect", false, false)
	client := network.Client(server.NewId(), server.NewId())
	client.SetStrictExp(true)
	client.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Hour))
	return client
}

// §22.5's §7 row: with reject_expired off, connect's transport auth refuses a
// strict_exp token past its exp (D-TEAM-14), on both the h1 and the h3 path, which
// share the verifier and connectClientAuthentication, whatever sidecar.yml says
// about M0's p0_enforce switch. A definitive refusal, before any state is read,
// so a sidecar re-enrolls and does not retry. Pure.
func TestConnectRefusesAnExpiredStrictExpToken(t *testing.T) {
	defer session.Testing_SetRejectExpired(false)()
	ctx := context.Background()

	for _, config := range []string{"", "sidecar:\n  p0_enforce: {enabled: false}\n", "sidecar:\n  p0_enforce: {enabled: true}\n"} {
		func() {
			defer server.Testing_PushSidecarConfig(config)()

			byJwt := newExpiredStrictExpToken()
			if _, err := session.ParseByJwtForAudience(ctx, byJwt.Testing_Sign(), session.ByJwtAudienceConnect); err == nil {
				t.Fatalf("%q: the transport's verifier passed an expired strict_exp token", config)
			}

			// the final read, which is the check a token parsed just before its exp meets
			_, status, err := connectClientAuthentication(ctx, byJwt, server.NewId().Bytes())
			if err == nil || status != http.StatusUnauthorized {
				t.Fatalf("%q: status = %d, err = %v; want 401", config, status, err)
			}
			if !connectCredentialRefused(status) {
				t.Fatalf("%q: the refusal is not a credential refusal", config)
			}
			if connectAuthDependencyUnavailable(err) {
				t.Fatalf("%q: the refusal reads as an outage, which a client retries", config)
			}
		}()
	}
}
