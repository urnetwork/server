package router

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Root cause, 2026-11-18 (14:18, 16:23, 17:09, 17:23 and 17:38Z): fleet-wide,
// minute-long bursts of "redis: connection pool: failed to dial ... i/o
// timeout". With vault auth.yml revocation_record_enabled on, a tagged client
// credential whose revocation record could not be read was refused with 503
// by every authenticated route, POST /connect/control among them, after
// waiting out the two second authentication budget. Owner decision 3b: the
// record check accepts the credential unchecked, within the check budget, and
// the request is served. The test env supplies only the signing keys; the
// record check asks PostgreSQL nothing.
func TestAuthenticatedRoutesServeATaggedClientWhileRedisDialsTimeOut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer session.Testing_SetRevocationRecordEnabled(true)()
		_, pop := server.Testing_StallAuthRedisDials(48)
		defer pop()
		network := session.NewByJwt(server.NewId(), server.NewId(), "dial-outage", false, false)
		signed := network.Client(server.NewId(), server.NewId()).Testing_Sign()
		for _, wrapper := range []string{"require_auth", "require_client", "input_auth", "input_client"} {
			request := httptest.NewRequest(http.MethodPost, "http://localhost/connect/control", bytes.NewBufferString("{}"))
			request.RemoteAddr = "192.0.2.10:443"
			request.Header.Set("Authorization", "Bearer "+signed)
			response := httptest.NewRecorder()
			called := false
			impl := func(*session.ClientSession) (bool, error) { called = true; return true, nil }
			inputImpl := func(map[string]any, *session.ClientSession) (bool, error) { called = true; return true, nil }
			start := time.Now()
			switch wrapper {
			case "require_auth":
				WrapRequireAuth(impl, response, request)
			case "require_client":
				WrapRequireClient(impl, response, request)
			case "input_auth":
				WrapWithInputRequireAuth(inputImpl, response, request)
			case "input_client":
				WrapWithInputRequireClient(inputImpl, response, request)
			}
			elapsed := time.Since(start)
			if !called || response.Code != http.StatusOK {
				t.Fatalf("%s: a tagged client credential got %d during the Redis dial outage (controller called %v): %s", wrapper, response.Code, called, response.Body.String())
			}
			if 2*time.Second <= elapsed {
				t.Errorf("%s: authentication waited %s for Redis", wrapper, elapsed)
			}
		}
	})
}
