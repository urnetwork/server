package api

// The internal gate's quota routes (QUOTA-M2-11; QD-055) through api's own route table and the gates every route
// has (applyRouteAccess): a host's bearer, which those gates read as no URnetwork credential, reaches the handler,
// and every other request is refused 403 by it before a read. This build links no MG2, so the over-limit read
// past the bearer answers 503 from the model without reaching Postgres; quota_service_handlers_db_test.go has
// the answers on Postgres.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

func TestInternalGateRoutesReachTheirHandlerOnlyWithAHostsBearer(t *testing.T) {
	newBearer := func() (string, string) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		bearer := base64.RawURLEncoding.EncodeToString(raw)
		digest := sha256.Sum256([]byte(bearer))
		return bearer, hex.EncodeToString(digest[:])
	}
	evs, evsDigest := newBearer()
	stranger, _ := newBearer()
	pop := server.Vault.PushSimpleResource(server.InternalGateVerifyFileName, []byte("evs: {class: evs, sha256: ["+evsDigest+"]}\n"))
	defer pop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// api's whole table, as the process serves it: the verify file is read at the first internal call
	handler := router.NewRouter(ctx, Routes())
	networkId := server.NewId().String()
	calls := []struct {
		method string
		path   string
		// with evs's bearer: 503 from the model, or 403 where the route does not take evs
		evsCode int
	}{
		// a read that, past the bearer, answers from this build's state alone, never Postgres
		{method: "GET", path: "/internal/gate/over-limit", evsCode: http.StatusServiceUnavailable},
		{method: "GET", path: "/internal/gate/draw-order/" + networkId, evsCode: http.StatusForbidden},
		{method: "POST", path: "/internal/gate/spent/" + networkId, evsCode: http.StatusForbidden},
	}
	for _, call := range calls {
		if w := serveRouteAccess(handler, call.method, call.path, "Bearer "+evs); w.Code != call.evsCode {
			t.Errorf("%s %s with evs's bearer answered %d %q, want %d", call.method, call.path, w.Code, w.Body.String(), call.evsCode)
		}
		for _, authorization := range []string{"", "Bearer " + stranger, "Bearer " + evsDigest} {
			if w := serveRouteAccess(handler, call.method, call.path, authorization); w.Code != http.StatusForbidden {
				t.Errorf("%s %s with %q answered %d", call.method, call.path, authorization, w.Code)
			}
		}
	}
}
