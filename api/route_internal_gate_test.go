package api

// The internal gate's quota routes (QUOTA-M2-11; QD-055) and projection reads (ACL-M4-05; QD-680) through api's own
// route table and the gates every route has (applyRouteAccess): a host's bearer, which those gates read as no
// URnetwork credential, reaches the handler, and every other request is refused 403 by it before a read. This build
// links no MG2, so the over-limit read past the bearer answers 503 from the model without reaching Postgres;
// quota_service_handlers_db_test.go has the answers on Postgres. The process holds no first metadata cluster here,
// so a projection read past connect's bearer answers 503; internal_gate_projection_handlers_test.go has the answers
// on a sequencer.

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
	connect, connectDigest := newBearer()
	stranger, _ := newBearer()
	pop := server.Vault.PushSimpleResource(server.InternalGateVerifyFileName,
		[]byte("evs: {class: evs, sha256: ["+evsDigest+"]}\nconnect: {class: connect, sha256: ["+connectDigest+"]}\n"))
	defer pop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// api's whole table, as the process serves it: the verify file is read at the first internal call
	handler := router.NewRouter(ctx, Routes())
	networkId := server.NewId().String()
	rowsSha256 := sha256.Sum256([]byte("rows"))
	calls := []struct {
		method string
		path   string
		// with evs's and connect's bearers: 503 past the bearer, or 403 where the route does not take the class
		evsCode     int
		connectCode int
	}{
		// a read that, past the bearer, answers from this build's state alone, never Postgres
		{method: "GET", path: "/internal/gate/over-limit", evsCode: http.StatusServiceUnavailable, connectCode: http.StatusForbidden},
		{method: "GET", path: "/internal/gate/draw-order/" + networkId, evsCode: http.StatusForbidden, connectCode: http.StatusForbidden},
		{method: "POST", path: "/internal/gate/spent/" + networkId, evsCode: http.StatusForbidden, connectCode: http.StatusForbidden},
		// evs reads the log from the store itself, and this process holds no first metadata cluster
		{method: "GET", path: "/internal/gate/projection/" + networkId + "?after=0", evsCode: http.StatusForbidden, connectCode: http.StatusServiceUnavailable},
		{method: "GET", path: "/internal/gate/rows/" + networkId + "/" + hex.EncodeToString(rowsSha256[:]), evsCode: http.StatusForbidden, connectCode: http.StatusServiceUnavailable},
	}
	for _, call := range calls {
		if w := serveRouteAccess(handler, call.method, call.path, "Bearer "+evs); w.Code != call.evsCode {
			t.Errorf("%s %s with evs's bearer answered %d %q, want %d", call.method, call.path, w.Code, w.Body.String(), call.evsCode)
		}
		if w := serveRouteAccess(handler, call.method, call.path, "Bearer "+connect); w.Code != call.connectCode {
			t.Errorf("%s %s with connect's bearer answered %d %q, want %d", call.method, call.path, w.Code, w.Body.String(), call.connectCode)
		}
		for _, authorization := range []string{"", "Bearer " + stranger, "Bearer " + evsDigest} {
			if w := serveRouteAccess(handler, call.method, call.path, authorization); w.Code != http.StatusForbidden {
				t.Errorf("%s %s with %q answered %d", call.method, call.path, authorization, w.Code)
			}
		}
	}
}
