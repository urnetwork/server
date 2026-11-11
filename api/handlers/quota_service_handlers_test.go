package handlers

// api's internal quota surface (QUOTA-M2-11; QD-055) before it reads anything: with no verify file every route
// refuses, a bearer reaches only the routes its class is listed on, a batch names only its caller as producer,
// and a network id that does not parse is refused. None of these reaches model, so none needs Postgres;
// quota_service_handlers_db_test.go has the reads and writes.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/router"
)

// One bearer a class, as each service's credential file holds it, and the verify file api holds for them.
type testQuotaServiceBearers struct {
	callerBearers map[server.InternalGateCaller]string
	verifier      *server.InternalGateVerifier
}

func newTestQuotaServiceBearers(t testing.TB) *testQuotaServiceBearers {
	t.Helper()
	callers := []server.InternalGateCaller{
		server.InternalGateCallerEvs,
		server.InternalGateCallerGkm,
		server.InternalGateCallerSockets,
		server.InternalGateCallerTlsedge,
		server.InternalGateCallerMxedge,
	}
	bearers := &testQuotaServiceBearers{callerBearers: map[server.InternalGateCaller]string{}}
	verifyFile := ""
	for _, caller := range callers {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		bearer := base64.RawURLEncoding.EncodeToString(raw)
		digest := sha256.Sum256([]byte(bearer))
		bearers.callerBearers[caller] = bearer
		verifyFile += string(caller) + ": {class: " + string(caller) + ", sha256: [" + hex.EncodeToString(digest[:]) + "]}\n"
	}
	verifier, err := server.NewInternalGateVerifier([]byte(verifyFile))
	if err != nil {
		t.Fatal(err)
	}
	bearers.verifier = verifier
	return bearers
}

// The five routes as api.go registers them.
func testQuotaServiceRouter(t testing.TB, handlers *QuotaServiceHandlers) *router.Router {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return router.NewRouter(ctx, handlers.Routes())
}

func testQuotaServiceServe(serve http.Handler, method string, path string, bearer string, body []byte) *httptest.ResponseRecorder {
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	serve.ServeHTTP(w, request)
	return w
}

// A batch body of producer's, with one event.
func testQuotaServiceBatchBody(t testing.TB, producer string) []byte {
	t.Helper()
	body, err := json.Marshal(quotahost.NewQuotaApiUsageBatch(&sidecar.UsageBatch{
		BatchId:     connect.NewId(),
		Producer:    producer,
		WindowStart: time.Unix(1_800_000_000, 0).UTC(),
		Events:      []*sidecar.UsageEvent{{NetworkId: connect.NewId(), Meter: sidecar.QuotaMeterEvs, TransferByteCount: 1}},
	}, nil))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// A route and the request a test sends it.
type testQuotaServiceCall struct {
	route  string
	method string
	path   string
	body   []byte
}

func testQuotaServiceCalls(t testing.TB, producer string) []*testQuotaServiceCall {
	networkId := server.NewId().String()
	return []*testQuotaServiceCall{
		{route: quotaServiceRouteLimits, method: "GET", path: "/internal/gate/limits/" + networkId},
		{route: quotaServiceRouteOverLimit, method: "GET", path: "/internal/gate/over-limit"},
		{route: quotaServiceRouteDrawOrder, method: "GET", path: "/internal/gate/draw-order/" + networkId},
		{route: quotaServiceRouteSpent, method: "POST", path: "/internal/gate/spent/" + networkId},
		{route: quotaServiceRouteUsageBatch, method: "POST", path: "/internal/gate/usage-batch", body: testQuotaServiceBatchBody(t, producer)},
	}
}

// With no verify file, every route refuses every bearer, a real one included.
func TestQuotaServiceWithNoVerifyFileRefusesEveryCall(t *testing.T) {
	bearers := newTestQuotaServiceBearers(t)
	handlers := NewQuotaServiceHandlers(nil, nil)
	// the vault holds no internal-gate-verify.yml
	serve := testQuotaServiceRouter(t, handlers)
	for _, call := range testQuotaServiceCalls(t, string(server.InternalGateCallerEvs)) {
		w := testQuotaServiceServe(serve, call.method, call.path, bearers.callerBearers[server.InternalGateCallerEvs], call.body)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s answered %d with no verify file", call.route, w.Code)
		}
	}
}

// A bearer api holds no digest of, or none at all, is refused on every route before anything is read.
func TestQuotaServiceRefusesABearerItDoesNotHold(t *testing.T) {
	bearers := newTestQuotaServiceBearers(t)
	serve := testQuotaServiceRouter(t, NewQuotaServiceHandlers(nil, &QuotaServiceDeps{Verifier: bearers.verifier}))
	stranger := newTestQuotaServiceBearers(t)
	for _, call := range testQuotaServiceCalls(t, string(server.InternalGateCallerEvs)) {
		for _, bearer := range []string{"", stranger.callerBearers[server.InternalGateCallerEvs], "not-a-bearer"} {
			w := testQuotaServiceServe(serve, call.method, call.path, bearer, call.body)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s answered %d to bearer %q", call.route, w.Code, bearer)
			}
		}
	}
}

// Each class reaches only the routes it is listed on: every other is refused 403 before a read, the route
// table and these classes agreeing (quotaServiceRouteCallers).
func TestQuotaServiceRefusesAClassOffItsRoutes(t *testing.T) {
	bearers := newTestQuotaServiceBearers(t)
	serve := testQuotaServiceRouter(t, NewQuotaServiceHandlers(nil, &QuotaServiceDeps{Verifier: bearers.verifier}))
	refusedRoutes := map[server.InternalGateCaller][]string{
		server.InternalGateCallerEvs:     {quotaServiceRouteDrawOrder, quotaServiceRouteSpent},
		server.InternalGateCallerGkm:     {quotaServiceRouteOverLimit, quotaServiceRouteDrawOrder, quotaServiceRouteSpent, quotaServiceRouteUsageBatch},
		server.InternalGateCallerSockets: {quotaServiceRouteDrawOrder, quotaServiceRouteSpent, quotaServiceRouteUsageBatch},
		server.InternalGateCallerTlsedge: {quotaServiceRouteOverLimit},
		server.InternalGateCallerMxedge:  {},
	}
	for caller, refused := range refusedRoutes {
		for _, call := range testQuotaServiceCalls(t, string(caller)) {
			want := true
			for _, route := range refused {
				if route == call.route {
					want = false
				}
			}
			connect.AssertEqual(t, slices.Contains(quotaServiceRouteCallers[call.route], caller), want)
			if want {
				continue
			}
			w := testQuotaServiceServe(serve, call.method, call.path, bearers.callerBearers[caller], call.body)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s's bearer on %s answered %d", caller, call.route, w.Code)
			}
		}
	}
}

// A batch reports only its caller's own bytes: one naming another producer, or none, is refused 403, and one
// that does not parse, has a null event, holds a field the wire lacks or runs past the bound is refused before
// model sees it.
func TestQuotaServiceBatchNamesOnlyItsCallersProducer(t *testing.T) {
	bearers := newTestQuotaServiceBearers(t)
	settings := DefaultQuotaServiceSettings()
	settings.MaxUsageBatchByteCount = 64 * 1024
	serve := testQuotaServiceRouter(t, NewQuotaServiceHandlers(settings, &QuotaServiceDeps{Verifier: bearers.verifier}))
	tlsedge := bearers.callerBearers[server.InternalGateCallerTlsedge]
	for _, producer := range []string{sidecar.QuotaProducerEvs, sidecar.QuotaProducerMxEdge, sidecar.QuotaProducerTaskworker, ""} {
		w := testQuotaServiceServe(serve, "POST", "/internal/gate/usage-batch", tlsedge, testQuotaServiceBatchBody(t, producer))
		if w.Code != http.StatusForbidden {
			t.Errorf("tlsedge posting %q's batch answered %d", producer, w.Code)
		}
	}
	nullEvent := []byte(`{"batch_id":"` + server.NewId().String() + `","producer":"tlsedge","window_start":"2027-01-15T08:00:00Z","events":[null],"carves":[]}`)
	unknownField := []byte(`{"batch_id":"` + server.NewId().String() + `","producer":"tlsedge","window_start":"2027-01-15T08:00:00Z","events":[],"carves":[],"repo_id":"x"}`)
	tooLarge := append([]byte(`{"producer":"tlsedge","events":[`), bytes.Repeat([]byte(`{"meter":"tls"},`), 8*1024)...)
	for _, body := range [][]byte{[]byte("{"), []byte("[]"), nullEvent, unknownField} {
		w := testQuotaServiceServe(serve, "POST", "/internal/gate/usage-batch", tlsedge, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%q answered %d", body, w.Code)
		}
	}
	w := testQuotaServiceServe(serve, "POST", "/internal/gate/usage-batch", tlsedge, tooLarge)
	connect.AssertEqual(t, w.Code, http.StatusRequestEntityTooLarge)
}

// A network id that does not parse, or the zero id, is refused 400 on each route that takes one.
func TestQuotaServiceRefusesANetworkIdItCannotRead(t *testing.T) {
	bearers := newTestQuotaServiceBearers(t)
	serve := testQuotaServiceRouter(t, NewQuotaServiceHandlers(nil, &QuotaServiceDeps{Verifier: bearers.verifier}))
	mxedge := bearers.callerBearers[server.InternalGateCallerMxedge]
	for _, path := range []string{"/internal/gate/limits/", "/internal/gate/draw-order/", "/internal/gate/spent/"} {
		method := "GET"
		if path == "/internal/gate/spent/" {
			method = "POST"
		}
		for _, networkId := range []string{"not-an-id", server.Id{}.String()} {
			w := testQuotaServiceServe(serve, method, path+networkId, mxedge, nil)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s%s answered %d", path, networkId, w.Code)
			}
		}
	}
}
