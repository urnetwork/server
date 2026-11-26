package api

// sidecar/handler's api/compute routes on api (DESIGN-COMPUTE §14.3; CSRV-M12-04): §14.3's eleven api/compute rows,
// each a router.Route under the session's auth, served by the handler frame (sidecar/handler's compute_api.go,
// BAL-M12-06), which checks the caller's rights (§9.10) and the request's shape before the route's handler runs, or
// answers 501 not_built for a route no task has registered:
//
//	GET  /compute/balance         GET  /compute/charges         POST /compute/refund
//	POST /compute/checkout        GET  /compute/statement       POST /compute/promo/redeem
//	GET  /compute/auto-renew      GET  /compute/spend           GET  /compute/terms
//	POST /compute/auto-renew                                    POST /compute/terms
//
// The model routes, GET /compute/models/{sha256}/..., are public and read no credential (compute_models_mount.go,
// CSRV-M11-01), so they are not these.
//
// Each request, in order, every answer but a handler's own success JSON as the device's api client reads a refusal,
// {"code", "retry_after_ms"}, with Retry-After repeating the retry:
//
//  1. 503 unavailable while this binary does not link MG-C1, the compute ledger's tables, before any credential is
//     read.
//  2. The session's credential: 401 unauthenticated for none or one that does not authenticate, 503 unavailable when
//     the check could not run.
//  3. The caller (handler.ComputeApiCaller), from the credential and api's verified state, never from the request:
//     - a client JWT: the sidecar its client is bound to (§5.15), the binding its newest generation and the identity
//     active and of the token's network, so an older generation's token speaks for no sidecar; its row, member,
//     role, tags, enclave capability, ephemeral and Delegation marks and consent from api's gate cache
//     (controller.ApiAuthorizationCache); 503 unavailable while the network's verified state is not loaded, never an
//     allow; 403 credential_refused for a client bound to no sidecar, or one the state does not admit;
//     - the network's own credential: its signed login, which names no client, or an API key whose scopes reach
//     billing (a legacy key, or one scoped all), as the network's, its member the credential's membership and its
//     role that membership's, an Owner for a personal network's own;
//     - any other credential, an API key scoped short of billing or an unsigned identity, as an agent's, which no
//     right reaches (§9.10).
//  4. The body, read to the frame's bound and one byte more, so a longer one is refused whole.
//  5. The frame: the rights, the shape, then the route's handler or not_built.
//
// route_authz.go classifies each route Client, so a client JWT reaches it and the frame narrows, and lists each on
// the strict_exp allowlist and off the forwarding one, so a sidecar's token reaches it and a forwarding client's is
// refused before this runs. Nothing of a caller is kept across requests, and no request's body, query, credential or
// address is logged (I-13).
//
// The routes' handlers run on what this process installs for them: the ledger's payer and promotion hash keys
// (controller.InstallComputeLedgerSecrets) and compute's Stripe Checkout (controller.InstallComputeStripe), each
// once a process, when the routes are mounted.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/handler"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The mount's own refusal codes, beside the frame's (handler.ComputeApiRefusal...).
const (
	// No credential, or one that does not authenticate.
	computeApiRefusalUnauthenticated = "unauthenticated"
	// The api cannot answer now: the ledger's tables are not linked, the credential could not be checked, or the
	// network's verified state is not loaded.
	computeApiRefusalUnavailable = handler.ComputeApiRefusalUnavailable
)

// The mount's settings (QD-702).
type ComputeApiMountSettings struct {
	// The frame's settings, the body's bound among them.
	Frame *handler.ComputeApiSettings
	// When a caller asks again after a 503 for a network whose verified state is not loaded, or a credential whose
	// check could not run: 1 s, by when the load the request started, or the store, may answer. Non-positive sends
	// no Retry-After.
	UnavailableRetryAfter time.Duration
	// When a caller asks again after a 503 from a binary that does not link the ledger's tables: 60 s, as a closed
	// gate's, since only a deploy changes it. Non-positive sends no Retry-After.
	UnlinkedRetryAfter time.Duration
}

func DefaultComputeApiMountSettings() *ComputeApiMountSettings {
	return &ComputeApiMountSettings{
		Frame:                 handler.DefaultComputeApiSettings(),
		UnavailableRetryAfter: 1 * time.Second,
		UnlinkedRetryAfter:    60 * time.Second,
	}
}

// Whether this binary links MG-C1, the compute ledger's tables. Tests replace it.
var computeApiLedgerLinked = server.SidecarMgC1Linked

// The sidecar a client of networkId is the current client of (§5.15): its binding's, the binding its sidecar's newest
// generation, the identity active and of networkId, so an older generation's token, or a client a share binds to
// another network's sidecar, speaks for none. False for none. Tests replace it.
var computeApiClientSidecar = func(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, bool) {
	if _, err := model.GetClientBindingKey(ctx, clientId); err != nil {
		return server.Id{}, false
	}
	view := model.GetSidecarIdentityByClient(ctx, networkId, clientId)
	if view == nil || view.Status != model.SidecarIdentityStatusActive || view.NetworkId != networkId {
		return server.Id{}, false
	}
	return view.SidecarId, true
}

// The network's verified state as api's gate cache holds it, false while none is loaded, which asks the cache for a
// load. Tests replace it.
var computeApiVerifiedState = func(networkId server.Id) (handler.ComputeApiVerifiedState, bool) {
	cache := controller.ApiAuthorizationCache()
	if cache == nil {
		return nil, false
	}
	snapshot := sidecar.RequestAclGateSnapshot(cache, networkId)
	if snapshot == nil {
		return nil, false
	}
	state, err := handler.ComputeApiStateOf(snapshot)
	if err != nil {
		return nil, false
	}
	return state, true
}

// A member's role in networkId as network_member mirrors it, false for none. Tests replace it.
var computeApiMemberRole = func(ctx context.Context, networkId server.Id, memberId server.Id) (bridge.MemberRole, bool) {
	role, ok := model.MemberRole(ctx, networkId, memberId)
	if !ok {
		return bridge.MemberRole_MEMBER_ROLE_UNSPECIFIED, false
	}
	return role.BridgeRole(), true
}

// Whether networkId is a team, whose credentials each name a member. Tests replace it.
var computeApiNetworkIsTeam = model.NetworkIsTeam

// A refusal the mount answers before the frame: its status, code and retry.
type computeApiMountRefusal struct {
	status     int
	code       string
	retryAfter time.Duration
}

// The mounted routes over one settings.
type computeApiMount struct {
	settings *ComputeApiMountSettings
}

// §14.3's api/compute routes, each a literal registration of one route of the frame's table (TestComputeApiRoutes
// AreMountedOnce holds the two together), and what their handlers run on installed for this process.
func computeApiRoutes() []*router.Route {
	controller.InstallComputeLedgerSecrets()
	controller.InstallComputeStripe()
	mount := &computeApiMount{settings: DefaultComputeApiMountSettings()}
	return []*router.Route{
		router.NewRoute("GET", "/compute/balance", mount.handler(http.MethodGet, "/compute/balance")),
		router.NewRoute("POST", "/compute/checkout", mount.handler(http.MethodPost, "/compute/checkout")),
		router.NewRoute("GET", "/compute/auto-renew", mount.handler(http.MethodGet, "/compute/auto-renew")),
		router.NewRoute("POST", "/compute/auto-renew", mount.handler(http.MethodPost, "/compute/auto-renew")),
		router.NewRoute("GET", "/compute/charges", mount.handler(http.MethodGet, "/compute/charges")),
		router.NewRoute("GET", "/compute/statement", mount.handler(http.MethodGet, "/compute/statement")),
		router.NewRoute("GET", "/compute/spend", mount.handler(http.MethodGet, "/compute/spend")),
		router.NewRoute("POST", "/compute/refund", mount.handler(http.MethodPost, "/compute/refund")),
		router.NewRoute("POST", "/compute/promo/redeem", mount.handler(http.MethodPost, "/compute/promo/redeem")),
		router.NewRoute("GET", "/compute/terms", mount.handler(http.MethodGet, "/compute/terms")),
		router.NewRoute("POST", "/compute/terms", mount.handler(http.MethodPost, "/compute/terms")),
	}
}

// The frame's route of method and path. A route the table does not hold is a build error, which panics.
func (self *computeApiMount) route(method string, path string) *handler.ComputeApiRoute {
	for _, route := range handler.ComputeApiRoutes(self.settings.Frame) {
		if route.Method == method && route.Path == path {
			return route
		}
	}
	panic("compute api: " + method + " " + path + " is no route of the frame's table")
}

// One route's handler (the file header's steps).
func (self *computeApiMount) handler(method string, path string) http.HandlerFunc {
	route := self.route(method, path)
	return func(w http.ResponseWriter, r *http.Request) {
		if !computeApiLedgerLinked() {
			self.writeRefusal(w, &computeApiMountRefusal{status: http.StatusServiceUnavailable, code: computeApiRefusalUnavailable, retryAfter: self.settings.UnlinkedRetryAfter})
			return
		}
		clientSession, err := session.NewClientSessionFromRequest(r)
		if err != nil {
			self.writeRefusal(w, &computeApiMountRefusal{status: http.StatusBadRequest, code: handler.ComputeApiRefusalBadRequest})
			return
		}
		defer clientSession.Cancel()
		if err := clientSession.Auth(r); err != nil {
			if errors.Is(err, session.ErrAuthUnavailable) {
				self.writeRefusal(w, &computeApiMountRefusal{status: http.StatusServiceUnavailable, code: computeApiRefusalUnavailable, retryAfter: self.settings.UnavailableRetryAfter})
			} else {
				self.writeRefusal(w, &computeApiMountRefusal{status: http.StatusUnauthorized, code: computeApiRefusalUnauthenticated})
			}
			return
		}
		caller, refusal := self.callerOf(clientSession)
		if refusal != nil {
			self.writeRefusal(w, refusal)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, int64(self.settings.Frame.BodyMaxByteCount)+1))
		if err != nil {
			self.writeRefusal(w, &computeApiMountRefusal{status: http.StatusBadRequest, code: handler.ComputeApiRefusalBadRequest})
			return
		}
		self.writeAnswer(w, route.Serve(clientSession.Ctx, caller, r.URL.Query(), body))
	}
}

// The caller of an authenticated session (the file header's step 3), or the refusal the mount answers in its place.
func (self *computeApiMount) callerOf(clientSession *session.ClientSession) (*handler.ComputeApiCaller, *computeApiMountRefusal) {
	ctx := clientSession.Ctx
	byJwt := clientSession.ByJwt
	if byJwt == nil {
		return nil, &computeApiMountRefusal{status: http.StatusUnauthorized, code: computeApiRefusalUnauthenticated}
	}
	networkId := byJwt.NetworkId
	switch {
	case clientSession.UnsignedIdentity || byJwt.LagGuest || byJwt.GuestMode:
		// an admin token's, an OAuth client's or a guest's: never a person's surface
		return &handler.ComputeApiCaller{Credential: handler.ComputeApiCredentialAgent, NetworkId: networkId}, nil
	case clientSession.ApiKeyAuthenticated:
		if !model.ScopesPermit(clientSession.ApiKeyScopes, model.RolePermissionBilling) {
			return &handler.ComputeApiCaller{Credential: handler.ComputeApiCredentialAgent, NetworkId: networkId}, nil
		}
		return self.networkCaller(ctx, byJwt), nil
	case byJwt.ClientId == nil:
		return self.networkCaller(ctx, byJwt), nil
	}
	sidecarId, bound := computeApiClientSidecar(ctx, networkId, *byJwt.ClientId)
	if !bound {
		return nil, &computeApiMountRefusal{status: http.StatusForbidden, code: handler.ComputeApiRefusalCredentialRefused}
	}
	state, loaded := computeApiVerifiedState(networkId)
	if !loaded {
		return nil, &computeApiMountRefusal{status: http.StatusServiceUnavailable, code: computeApiRefusalUnavailable, retryAfter: self.settings.UnavailableRetryAfter}
	}
	caller, err := handler.NewComputeApiSidecarCaller(state, networkId, sidecarId)
	if err != nil {
		if glog.V(1) {
			glog.Infof("[compute-api][%s]a client's sidecar is not admitted: %s\n", networkId, err)
		}
		return nil, &computeApiMountRefusal{status: http.StatusForbidden, code: handler.ComputeApiRefusalCredentialRefused}
	}
	return caller, nil
}

// The network's own credential as a caller: its membership and that membership's role, an Owner for a personal
// network's own credential, which names none; a role that cannot be read leaves none, which every right refuses.
func (self *computeApiMount) networkCaller(ctx context.Context, byJwt *session.ByJwt) *handler.ComputeApiCaller {
	caller := &handler.ComputeApiCaller{Credential: handler.ComputeApiCredentialNetwork, NetworkId: byJwt.NetworkId}
	if byJwt.MemberId == nil {
		if !computeApiNetworkIsTeam(ctx, byJwt.NetworkId) {
			caller.Role = bridge.MemberRole_MEMBER_ROLE_OWNER
		}
		return caller
	}
	memberId := *byJwt.MemberId
	caller.MemberId = &memberId
	if role, ok := computeApiMemberRole(ctx, byJwt.NetworkId, memberId); ok {
		caller.Role = role
	}
	return caller
}

// Writes the frame's answer: its JSON, private to the caller, with Retry-After repeating a refusal's retry.
func (self *computeApiMount) writeAnswer(w http.ResponseWriter, answer *handler.ComputeApiAnswer) {
	if http.StatusBadRequest <= answer.Status {
		refusal := struct {
			RetryAfterMs int64 `json:"retry_after_ms"`
		}{}
		if json.Unmarshal(answer.Body, &refusal) == nil && 0 < refusal.RetryAfterMs {
			self.setRetryAfter(w, time.Duration(refusal.RetryAfterMs)*time.Millisecond)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(answer.Status)
	w.Write(answer.Body)
}

// Writes the mount's own refusal as the frame writes one.
func (self *computeApiMount) writeRefusal(w http.ResponseWriter, refusal *computeApiMountRefusal) {
	retryAfterMs := int64(0)
	if 0 < refusal.retryAfter {
		retryAfterMs = refusal.retryAfter.Milliseconds()
	}
	body, _ := json.Marshal(map[string]any{"code": refusal.code, "retry_after_ms": retryAfterMs})
	self.writeAnswer(w, &handler.ComputeApiAnswer{Status: refusal.status, Body: body})
}

// Sets Retry-After to retryAfter in whole seconds, rounded up, at least 1.
func (self *computeApiMount) setRetryAfter(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
}
