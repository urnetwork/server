// The operator routes and the public key lookup answer a failure with a
// fixed message; the cause, which can name the store or the parser, stays in
// the server log. A declared ingest refusal keeps its own words.
package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// An ingest failure that is not a declared refusal was written as the 400's
// body, with whatever the failing call put in it.
func TestWriteProviderEgressIngestErrorHidesUnexpectedCause(t *testing.T) {
	w := httptest.NewRecorder()
	writeProviderEgressIngestError(w, errors.New("synthetic ingest storage failure at 192.0.2.8"))
	requireDeclaredAnswer(t, w.Code, w.Body.String(), http.StatusBadRequest, "Provider ingest failed.", "synthetic", "192.0.2.8")
}

// A destination catalog that cannot be built was appended to the 500.
func TestProviderEgressDestinationsHidesPoolFailureCause(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte("schema_version: 99\ndestinations:\n  - name: synthetic-private-site\n    class: site\n    category: reference\n    url: https://synthetic-private.example/\n")))
		const secret = "correct-operator-secret-0123456789"
		defer withStubOperatorIngestSecret(secret)()
		req := httptest.NewRequest(http.MethodGet, "/network/provider-egress-destinations", nil)
		req.Header.Set(operatorSecretHeader, secret)
		w := httptest.NewRecorder()
		ProviderEgressDestinations(w, req)
		requireDeclaredAnswer(t, w.Code, w.Body.String(), http.StatusInternalServerError, "The destination pool cannot be served.", "schema", "synthetic-private")
	})
}

// A client id that does not parse was answered with the parser's error.
func TestGetClientKeyRefusesMalformedIdWithFixedMessage(t *testing.T) {
	front := router.NewRouter(context.Background(), []*router.Route{router.NewRoute("GET", "/key/([^/]+)", GetClientKey)})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/key/synthetic-not-an-id", nil)
	req.RemoteAddr = "192.0.2.51:4500"
	front.ServeHTTP(w, req)
	requireDeclaredAnswer(t, w.Code, w.Body.String(), http.StatusBadRequest, "Invalid client id.", "uuid", "UUID", "length")
}

// The same, for the key history lookup.
func TestGetClientKeyHistoryRefusesMalformedIdWithFixedMessage(t *testing.T) {
	front := router.NewRouter(context.Background(), []*router.Route{router.NewRoute("GET", "/key/([^/]+)/history", GetClientKeyHistory)})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/key/synthetic-not-an-id/history", nil)
	req.RemoteAddr = "192.0.2.51:4500"
	front.ServeHTTP(w, req)
	requireDeclaredAnswer(t, w.Code, w.Body.String(), http.StatusBadRequest, "Invalid client id.", "uuid", "UUID", "length")
}
