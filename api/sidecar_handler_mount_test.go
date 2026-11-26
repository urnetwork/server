package api

// The api/compute mount (DESIGN-COMPUTE §14.3; CSRV-M12-04): each route of sidecar/handler's frame mounted once; the
// caller resolved from the credential and api's verified state, never the request; the ledger's tables, the
// credential and an unloaded network answered before the frame; and refusals written as the device's api client
// reads them. Pure but for TestComputeApiRoutesServeTheNetworkCredential, which runs on the test stack.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/handler"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Replaces one of the mount's reads for the test.
func setComputeApiRead[T any](t *testing.T, read *T, replacement T) {
	t.Helper()
	previous := *read
	*read = replacement
	t.Cleanup(func() {
		*read = previous
	})
}

// A verified state as a test writes it.
type testComputeApiState struct {
	sidecars  map[sidecar.Id]*bridge.SidecarRow
	members   map[sidecar.Id]*bridge.MemberRow
	consented map[sidecar.Id]bool
}

func (self *testComputeApiState) SidecarRow(sidecarId sidecar.Id) *bridge.SidecarRow {
	return self.sidecars[sidecarId]
}

func (self *testComputeApiState) Member(memberId sidecar.Id) *bridge.MemberRow {
	return self.members[memberId]
}

func (self *testComputeApiState) Consented(sidecarId sidecar.Id) bool {
	return self.consented[sidecarId]
}

// A refusal's code and retry from a written answer, and its Retry-After.
func testComputeApiRefusalOf(t *testing.T, w *httptest.ResponseRecorder) (string, int64, string) {
	t.Helper()
	body := struct {
		Code         string `json:"code"`
		RetryAfterMs int64  `json:"retry_after_ms"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer's body %q: %v", w.Body.String(), err)
	}
	return body.Code, body.RetryAfterMs, w.Header().Get("Retry-After")
}

// Routes() serves each route of the frame's table once, by the frame's method and path, in the frame's order, and
// the mount names no route the table lacks.
func TestComputeApiRoutesAreMountedOnce(t *testing.T) {
	served := map[string]int{}
	for _, route := range Routes() {
		served[routeAccessKey(route)] += 1
	}
	frame := handler.ComputeApiRoutes(nil)
	mounted := computeApiRoutes()
	if len(mounted) != len(frame) {
		t.Fatalf("the mount holds %d routes, the frame %d", len(mounted), len(frame))
	}
	for i, route := range frame {
		key := route.Method + " " + route.Path
		if served[key] != 1 {
			t.Errorf("%s is served %d times", key, served[key])
		}
		if routeAccessKey(mounted[i]) != key {
			t.Errorf("the mount's route %d is %s, the frame's %s", i, routeAccessKey(mounted[i]), key)
		}
		if !slices.Contains(computeApiRouteKeys, key) {
			t.Errorf("%s is no route route_authz_compute_test.go names", key)
		}
	}
}

// The caller comes from the credential and the verified state: a bound sidecar's token is that sidecar, with its
// member, role, tags, marks and consent from the state; the network's credential is the network's, its member's role
// read, an Owner for a personal network's own; an API key short of billing is an agent's; a client bound to no
// sidecar, or one the state does not admit, is refused; and a network whose state is not loaded answers 503.
func TestComputeApiCallerResolvesTheSidecar(t *testing.T) {
	networkId := server.NewId()
	clientId := server.NewId()
	sidecarId := server.NewId()
	memberId := server.NewId()
	adminMemberId := server.NewId()
	state := &testComputeApiState{
		sidecars: map[sidecar.Id]*bridge.SidecarRow{
			sidecarId: {MemberId: memberId.Bytes()},
		},
		members: map[sidecar.Id]*bridge.MemberRow{
			memberId: {Role: bridge.MemberRole_MEMBER_ROLE_BILLING_ADMIN},
		},
		consented: map[sidecar.Id]bool{sidecarId: true},
	}
	loaded := true
	team := false
	setComputeApiRead(t, &computeApiClientSidecar, func(ctx context.Context, callerNetworkId server.Id, callerClientId server.Id) (server.Id, bool) {
		return sidecarId, callerNetworkId == networkId && callerClientId == clientId
	})
	setComputeApiRead(t, &computeApiVerifiedState, func(stateNetworkId server.Id) (handler.ComputeApiVerifiedState, bool) {
		if !loaded || stateNetworkId != networkId {
			return nil, false
		}
		return state, true
	})
	setComputeApiRead(t, &computeApiMemberRole, func(ctx context.Context, roleNetworkId server.Id, roleMemberId server.Id) (bridge.MemberRole, bool) {
		if roleNetworkId == networkId && roleMemberId == adminMemberId {
			return bridge.MemberRole_MEMBER_ROLE_ADMIN, true
		}
		return bridge.MemberRole_MEMBER_ROLE_UNSPECIFIED, false
	})
	setComputeApiRead(t, &computeApiNetworkIsTeam, func(ctx context.Context, teamNetworkId server.Id) bool {
		return team
	})
	mount := &computeApiMount{settings: DefaultComputeApiMountSettings()}
	callerOf := func(byJwt *session.ByJwt, edit func(clientSession *session.ClientSession)) (*handler.ComputeApiCaller, *computeApiMountRefusal) {
		clientSession := session.Testing_CreateClientSession(context.Background(), byJwt)
		defer clientSession.Cancel()
		if edit != nil {
			edit(clientSession)
		}
		return mount.callerOf(clientSession)
	}
	network := session.NewByJwt(networkId, server.NewId(), "compute-api", false, false)

	// a bound sidecar's strict_exp token
	strict := network.Client(server.NewId(), clientId)
	strict.SetStrictExp(true)
	caller, refusal := callerOf(strict, nil)
	if refusal != nil || caller.Credential != handler.ComputeApiCredentialSidecar || caller.NetworkId != networkId ||
		caller.SidecarId == nil || *caller.SidecarId != sidecarId || caller.MemberId == nil || *caller.MemberId != memberId ||
		caller.Role != bridge.MemberRole_MEMBER_ROLE_BILLING_ADMIN || !caller.Consented || caller.Tagged || !caller.MayBuy() {
		t.Fatalf("a bound sidecar's token: %+v, %+v", caller, refusal)
	}
	// its state says what it is: tagged, it buys nothing
	state.sidecars[sidecarId].TagIds = [][]byte{server.NewId().Bytes()}
	if caller, refusal := callerOf(strict, nil); refusal != nil || !caller.Tagged || caller.MemberId != nil || caller.MayBuy() {
		t.Fatalf("a tagged sidecar's token: %+v, %+v", caller, refusal)
	}
	state.sidecars[sidecarId].TagIds = nil

	// the network's own credential: a personal network's Owner, a team member's role, a team's with no member none
	if caller, refusal := callerOf(network, nil); refusal != nil || caller.Credential != handler.ComputeApiCredentialNetwork ||
		caller.Role != bridge.MemberRole_MEMBER_ROLE_OWNER || caller.MemberId != nil || caller.SidecarId != nil || !caller.MayBuy() {
		t.Fatalf("a personal network's credential: %+v, %+v", caller, refusal)
	}
	team = true
	member := session.NewByJwt(networkId, server.NewId(), "compute-api", false, false)
	member.MemberId = &adminMemberId
	if caller, refusal := callerOf(member, nil); refusal != nil || caller.Credential != handler.ComputeApiCredentialNetwork ||
		caller.Role != bridge.MemberRole_MEMBER_ROLE_ADMIN || caller.MemberId == nil || *caller.MemberId != adminMemberId || caller.MayBuy() || !caller.SeesNetwork() {
		t.Fatalf("a team Admin's credential: %+v, %+v", caller, refusal)
	}
	if caller, refusal := callerOf(network, nil); refusal != nil || caller.Role != bridge.MemberRole_MEMBER_ROLE_UNSPECIFIED || caller.SeesNetwork() {
		t.Fatalf("a team's credential naming no member: %+v, %+v", caller, refusal)
	}
	unknownMemberId := server.NewId()
	unknown := session.NewByJwt(networkId, server.NewId(), "compute-api", false, false)
	unknown.MemberId = &unknownMemberId
	if caller, refusal := callerOf(unknown, nil); refusal != nil || caller.Role != bridge.MemberRole_MEMBER_ROLE_UNSPECIFIED || caller.SeesNetwork() {
		t.Fatalf("a credential whose membership does not read: %+v, %+v", caller, refusal)
	}
	team = false

	// API keys: a legacy key or one scoped all is the network's; one scoped short of billing is an agent's
	for label, scopes := range map[string][]string{"a legacy key": nil, "a key scoped all": {model.ApiKeyScopeAll}} {
		if caller, refusal := callerOf(network, func(clientSession *session.ClientSession) {
			clientSession.ApiKeyAuthenticated = true
			clientSession.ApiKeyScopes = scopes
		}); refusal != nil || caller.Credential != handler.ComputeApiCredentialNetwork {
			t.Fatalf("%s: %+v, %+v", label, caller, refusal)
		}
	}
	if caller, refusal := callerOf(network, func(clientSession *session.ClientSession) {
		clientSession.ApiKeyAuthenticated = true
		clientSession.ApiKeyScopes = []string{model.ApiKeyScopePolicyRead}
	}); refusal != nil || caller.Credential != handler.ComputeApiCredentialAgent || caller.MayBuy() || caller.SeesNetwork() {
		t.Fatalf("a key scoped policy:read: %+v, %+v", caller, refusal)
	}

	// a client bound to no sidecar, and a sidecar the state does not admit, are refused
	other := network.Client(server.NewId(), server.NewId())
	other.SetStrictExp(true)
	if caller, refusal := callerOf(other, nil); caller != nil || refusal == nil || refusal.status != http.StatusForbidden || refusal.code != handler.ComputeApiRefusalCredentialRefused {
		t.Fatalf("an unbound client: %+v, %+v", caller, refusal)
	}
	state.sidecars[sidecarId].RevokedAt = 1
	if caller, refusal := callerOf(strict, nil); caller != nil || refusal == nil || refusal.status != http.StatusForbidden || refusal.code != handler.ComputeApiRefusalCredentialRefused {
		t.Fatalf("a revoked sidecar: %+v, %+v", caller, refusal)
	}
	state.sidecars[sidecarId].RevokedAt = 0

	// a network whose verified state is not loaded is never an allow
	loaded = false
	if caller, refusal := callerOf(strict, nil); caller != nil || refusal == nil || refusal.status != http.StatusServiceUnavailable ||
		refusal.code != computeApiRefusalUnavailable || refusal.retryAfter != time.Second {
		t.Fatalf("an unloaded network: %+v, %+v", caller, refusal)
	}
}

// While this binary does not link the ledger's tables, each route answers 503 unavailable with a minute's
// Retry-After, before any credential is read: a credential that would not authenticate gets the same answer.
func TestComputeApiMountAnswersUnavailableWithoutTheLedger(t *testing.T) {
	setComputeApiRead(t, &computeApiLedgerLinked, func() bool { return false })
	byKey := computeApiRoutesByKey(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, key := range computeApiRouteKeys {
		method, path, _ := strings.Cut(key, " ")
		for _, authorization := range []string{"", "Bearer not-a-token"} {
			r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
			r.RemoteAddr = "198.51.100.44:4000"
			if authorization != "" {
				r.Header.Set("Authorization", authorization)
			}
			w := httptest.NewRecorder()
			router.NewRouter(ctx, []*router.Route{byKey[key]}).ServeHTTP(w, r)
			code, retryAfterMs, retryAfter := testComputeApiRefusalOf(t, w)
			if w.Code != http.StatusServiceUnavailable || code != computeApiRefusalUnavailable || retryAfterMs != 60000 || retryAfter != "60" {
				t.Errorf("%s with %q: %d %s %d %q", key, authorization, w.Code, code, retryAfterMs, retryAfter)
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
				t.Errorf("%s: headers %v", key, w.Header())
			}
		}
	}
}

// A refusal is written as the device's api client reads one: its status, its JSON code and retry, Retry-After in
// whole seconds rounded up, never cached; an answer with no retry sends no Retry-After.
func TestComputeApiMountWritesRefusalsAsTheClientReadsThem(t *testing.T) {
	mount := &computeApiMount{settings: DefaultComputeApiMountSettings()}
	w := httptest.NewRecorder()
	mount.writeRefusal(w, &computeApiMountRefusal{status: http.StatusServiceUnavailable, code: computeApiRefusalUnavailable, retryAfter: 1500 * time.Millisecond})
	if code, retryAfterMs, retryAfter := testComputeApiRefusalOf(t, w); w.Code != http.StatusServiceUnavailable || code != "unavailable" || retryAfterMs != 1500 || retryAfter != "2" {
		t.Errorf("a 503: %d %s %d %q", w.Code, code, retryAfterMs, retryAfter)
	}
	w = httptest.NewRecorder()
	mount.writeRefusal(w, &computeApiMountRefusal{status: http.StatusUnauthorized, code: computeApiRefusalUnauthenticated})
	if code, retryAfterMs, retryAfter := testComputeApiRefusalOf(t, w); w.Code != http.StatusUnauthorized || code != "unauthenticated" || retryAfterMs != 0 || retryAfter != "" {
		t.Errorf("a 401: %d %s %d %q", w.Code, code, retryAfterMs, retryAfter)
	}
	// a handler's own refusal through the frame, its retry repeated
	w = httptest.NewRecorder()
	mount.writeAnswer(w, &handler.ComputeApiAnswer{Status: http.StatusTooManyRequests, Body: []byte(`{"code":"rate_limited","retry_after_ms":3600000}`)})
	if code, _, retryAfter := testComputeApiRefusalOf(t, w); w.Code != http.StatusTooManyRequests || code != "rate_limited" || retryAfter != "3600" {
		t.Errorf("a handler's 429: %d %s %q", w.Code, code, retryAfter)
	}
	// a success carries no Retry-After, whatever its body names
	w = httptest.NewRecorder()
	mount.writeAnswer(w, &handler.ComputeApiAnswer{Status: http.StatusOK, Body: []byte(`{"retry_after_ms":5}`)})
	if w.Code != http.StatusOK || w.Header().Get("Retry-After") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a success: %d %v", w.Code, w.Header())
	}
}

// End to end through the router api serves, on the test stack, with the ledger's tables taken as linked: the
// network's root token and a legacy API key reach the frame as the network's Owner, which answers each unbuilt route
// not_built; a client token bound to no sidecar is refused credential_refused, no credential unauthenticated, and a
// body past the frame's bound bad_request.
func TestComputeApiRoutesServeTheNetworkCredential(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		setComputeApiRead(t, &computeApiLedgerLinked, func() bool { return true })
		self := newRouteAccessDbFixture(t, ctx)
		_, clientToken := self.mintClient(t, self.root(), nil)
		serve := func(method string, path string, authorization string, body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.RemoteAddr = "198.51.100.45:4000"
			if authorization != "" {
				r.Header.Set("Authorization", authorization)
			}
			w := httptest.NewRecorder()
			self.router.ServeHTTP(w, r)
			return w
		}
		for _, authorization := range []string{self.root(), "Bearer " + self.apiKey} {
			// routes no task has built yet: auto-renew's and the refund's are built (BAL-M12-11, BAL-M12-14)
			for _, key := range []string{"GET /compute/balance", "GET /compute/charges", "GET /compute/statement", "POST /compute/terms"} {
				method, path, _ := strings.Cut(key, " ")
				body := ""
				if method == http.MethodPost {
					body = "{}"
				}
				w := serve(method, path, authorization, body)
				if code, _, _ := testComputeApiRefusalOf(t, w); w.Code != http.StatusNotImplemented || code != handler.ComputeApiRefusalNotBuilt {
					t.Errorf("%s with %.12s: %d %s", key, authorization, w.Code, w.Body.String())
				}
			}
		}
		if w := serve(http.MethodGet, "/compute/balance", "Bearer "+clientToken, ""); w.Code != http.StatusForbidden {
			t.Errorf("an unbound client's balance: %d %s", w.Code, w.Body.String())
		} else if code, _, _ := testComputeApiRefusalOf(t, w); code != handler.ComputeApiRefusalCredentialRefused {
			t.Errorf("an unbound client's balance: %s", code)
		}
		if w := serve(http.MethodGet, "/compute/balance", "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("no credential: %d %s", w.Code, w.Body.String())
		} else if code, _, _ := testComputeApiRefusalOf(t, w); code != computeApiRefusalUnauthenticated {
			t.Errorf("no credential: %s", code)
		}
		long := `{"lot_id":"` + strings.Repeat("a", handler.DefaultComputeApiSettings().BodyMaxByteCount) + `"}`
		if w := serve(http.MethodPost, "/compute/refund", self.root(), long); w.Code != http.StatusBadRequest {
			t.Errorf("a body past its bound: %d %s", w.Code, w.Body.String())
		} else if code, _, _ := testComputeApiRefusalOf(t, w); code != handler.ComputeApiRefusalBadRequest {
			t.Errorf("a body past its bound: %s", code)
		}
	})
}
