package handlers

// api's internal quota surface (QUOTA-M2-11; QD-055's plan-QUOTA-host-reach): the Postgres half of
// ServiceHost.Quota for the hosts with none, which quotahost.NewHttpQuotaApi calls. Each route sits under
// server.InternalGatePathPrefix and takes only the bearers of the classes it lists (server.InternalGateVerifier),
// answering every other request 403 before it reads anything. Each call reads or writes model once and is never
// cached here, since the hosts cache (§20.7's 5 s limits, the 5 s over-limit poll):
//
//	GET  /internal/gate/limits/{network_id}      evs gkm sockets tlsedge mxedge  model.NetworkQuotaLimits
//	GET  /internal/gate/over-limit               evs sockets mxedge              model.GetQuotaOverLimit
//	GET  /internal/gate/draw-order/{network_id}  tlsedge mxedge                  model.LoadDrawOrderBalances
//	POST /internal/gate/spent/{network_id}       tlsedge mxedge                  model.MarkNetworkSpent
//	POST /internal/gate/usage-batch              evs tlsedge mxedge              model.ApplyUsageBatch
//
// The classes are this draft's reading of who calls what: limits feed every such host's caps and write-budget
// weights; the over-limit list feeds evs's storage gate, sockets' Find and mxedge's deposits; draws and the spent
// mark are the edge leases' on the tls and mx meters; and a batch's producer must be its caller's own service, so
// no host reports another's bytes. Bodies are quotahost's QuotaApi* types in JSON. A model that cannot answer
// (MG2 not linked, Postgres away) answers 503, and a batch the model refuses answers 400 with nothing applied.
//
// The lb refuses the prefix from outside (QD-055); the bearer is the gate either way. Safe for concurrent use.

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/router"
)

// Each route by the name its refusals count under, with the classes it takes.
const (
	quotaServiceRouteLimits     = "limits"
	quotaServiceRouteOverLimit  = "over-limit"
	quotaServiceRouteDrawOrder  = "draw-order"
	quotaServiceRouteSpent      = "spent"
	quotaServiceRouteUsageBatch = "usage-batch"
)

var quotaServiceRouteCallers = map[string][]server.InternalGateCaller{
	quotaServiceRouteLimits: {
		server.InternalGateCallerEvs,
		server.InternalGateCallerGkm,
		server.InternalGateCallerSockets,
		server.InternalGateCallerTlsedge,
		server.InternalGateCallerMxedge,
	},
	quotaServiceRouteOverLimit: {
		server.InternalGateCallerEvs,
		server.InternalGateCallerSockets,
		server.InternalGateCallerMxedge,
	},
	quotaServiceRouteDrawOrder: {
		server.InternalGateCallerTlsedge,
		server.InternalGateCallerMxedge,
	},
	quotaServiceRouteSpent: {
		server.InternalGateCallerTlsedge,
		server.InternalGateCallerMxedge,
	},
	quotaServiceRouteUsageBatch: {
		server.InternalGateCallerEvs,
		server.InternalGateCallerTlsedge,
		server.InternalGateCallerMxedge,
	},
}

var quotaServiceRefusalCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_internal_gate_refusals_total",
	Help: "Internal gate calls api refused 403 before reading anything, by route and why: no verify file, no bearer it holds, or a class the route does not take.",
}, []string{"route", "refusal"})

func init() {
	prometheus.MustRegister(quotaServiceRefusalCounter)
}

type QuotaServiceSettings struct {
	// The most bytes a usage batch's body may hold: 4 MiB, past the largest batch a producer sends
	// (sidecar.QuotaUsageBatchMaxEventCount events, with model's most carves).
	MaxUsageBatchByteCount int64
}

func DefaultQuotaServiceSettings() *QuotaServiceSettings {
	return &QuotaServiceSettings{
		MaxUsageBatchByteCount: 4 * 1024 * 1024,
	}
}

// What the handlers run on beyond their settings. Each nil takes api's own.
type QuotaServiceDeps struct {
	// The bearers' digests. nil: server.LoadInternalGateVerifier from api's vault, read once; a vault without the
	// file refuses every call 403.
	Verifier *server.InternalGateVerifier
	// nil: server.NowUtc.
	Now func() time.Time
}

// See the file header.
type QuotaServiceHandlers struct {
	settings *QuotaServiceSettings
	verifier func() *server.InternalGateVerifier
	now      func() time.Time
}

// The handlers on settings and deps, each nil taking the defaults.
func NewQuotaServiceHandlers(settings *QuotaServiceSettings, deps *QuotaServiceDeps) *QuotaServiceHandlers {
	if settings == nil {
		settings = DefaultQuotaServiceSettings()
	}
	if deps == nil {
		deps = &QuotaServiceDeps{}
	}
	verifier := func() *server.InternalGateVerifier {
		return deps.Verifier
	}
	if deps.Verifier == nil {
		verifier = sync.OnceValue(func() *server.InternalGateVerifier {
			loaded, err := server.LoadInternalGateVerifier()
			if err != nil {
				// the error names the file and the part, never a bearer
				glog.Errorf("[quotaservice]every internal gate call is refused: %v\n", err)
				return nil
			}
			return loaded
		})
	}
	now := deps.Now
	if now == nil {
		now = server.NowUtc
	}
	return &QuotaServiceHandlers{
		settings: settings,
		verifier: verifier,
		now:      now,
	}
}

// The five routes, each pattern its quotahost path's, so the transport and api name one route.
func (self *QuotaServiceHandlers) Routes() []*router.Route {
	return []*router.Route{
		router.NewRoute("GET", quotahost.QuotaApiLimitsPath+"([^/]+)", self.GetLimits),
		router.NewRoute("GET", quotahost.QuotaApiOverLimitPath, self.GetOverLimit),
		router.NewRoute("GET", quotahost.QuotaApiDrawOrderPath+"([^/]+)", self.GetDrawOrder),
		router.NewRoute("POST", quotahost.QuotaApiSpentPath+"([^/]+)", self.PostSpent),
		router.NewRoute("POST", quotahost.QuotaApiUsageBatchPath, self.PostUsageBatch),
	}
}

// The caller when its bearer is one the verify file holds for a class the route takes; otherwise the request is
// answered 403 and counted.
func (self *QuotaServiceHandlers) authenticate(w http.ResponseWriter, r *http.Request, route string) (server.InternalGateCaller, bool) {
	refuse := func(refusal string) (server.InternalGateCaller, bool) {
		quotaServiceRefusalCounter.WithLabelValues(route, refusal).Inc()
		http.Error(w, "Forbidden.", http.StatusForbidden)
		return "", false
	}
	verifier := self.verifier()
	if verifier == nil {
		return refuse("no_verify_file")
	}
	caller, ok := verifier.Authenticate(r)
	if !ok {
		return refuse("credential")
	}
	if !slices.Contains(quotaServiceRouteCallers[route], caller) {
		if glog.V(1) {
			glog.Infof("[quotaservice]%s's bearer refused on %s\n", caller, route)
		}
		return refuse("class")
	}
	return caller, true
}

// The route's network id, its one path value; a missing, malformed or zero id is answered 400.
func quotaServiceNetworkId(w http.ResponseWriter, r *http.Request) (server.Id, bool) {
	pathValues := router.GetPathValues(r)
	if len(pathValues) != 1 {
		http.Error(w, "Invalid network id.", http.StatusBadRequest)
		return server.Id{}, false
	}
	networkId, err := server.ParseId(pathValues[0])
	if err != nil || networkId == (server.Id{}) {
		http.Error(w, "Invalid network id.", http.StatusBadRequest)
		return server.Id{}, false
	}
	return networkId, true
}

func quotaServiceWriteJson(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[quotaservice]could not write a response: %s\n", err)
	}
}

// Answers 503 for a model that could not answer, saying why in the log alone.
func quotaServiceUnavailable(w http.ResponseWriter, route string, err error) {
	if glog.V(1) {
		glog.Infof("[quotaservice]%s unavailable: %s\n", route, err)
	}
	http.Error(w, "Unavailable.", http.StatusServiceUnavailable)
}

// GET /internal/gate/limits/{network_id}: the network's sidecar.QuotaLimits.
func (self *QuotaServiceHandlers) GetLimits(w http.ResponseWriter, r *http.Request) {
	if _, ok := self.authenticate(w, r, quotaServiceRouteLimits); !ok {
		return
	}
	networkId, ok := quotaServiceNetworkId(w, r)
	if !ok {
		return
	}
	var limits *sidecar.QuotaLimits
	var loadErr error
	server.HandleError(func() {
		limits = model.NetworkQuotaLimits(r.Context(), networkId)
	}, func(err error) {
		loadErr = err
	})
	if loadErr != nil || limits == nil {
		quotaServiceUnavailable(w, quotaServiceRouteLimits, loadErr)
		return
	}
	quotaServiceWriteJson(w, limits)
}

// GET /internal/gate/over-limit: quotahost.QuotaApiOverLimit.
func (self *QuotaServiceHandlers) GetOverLimit(w http.ResponseWriter, r *http.Request) {
	if _, ok := self.authenticate(w, r, quotaServiceRouteOverLimit); !ok {
		return
	}
	var overLimit *sidecar.QuotaOverLimit
	var loadErr error
	server.HandleError(func() {
		overLimit, loadErr = model.GetQuotaOverLimit(r.Context())
	}, func(err error) {
		loadErr = err
	})
	if loadErr != nil || overLimit == nil {
		quotaServiceUnavailable(w, quotaServiceRouteOverLimit, loadErr)
		return
	}
	quotaServiceWriteJson(w, quotahost.NewQuotaApiOverLimit(overLimit))
}

// GET /internal/gate/draw-order/{network_id}: quotahost.QuotaApiDrawOrder, the network's in-window balances in
// escrowDrawLess's order, each with its credit read now.
func (self *QuotaServiceHandlers) GetDrawOrder(w http.ResponseWriter, r *http.Request) {
	if _, ok := self.authenticate(w, r, quotaServiceRouteDrawOrder); !ok {
		return
	}
	networkId, ok := quotaServiceNetworkId(w, r)
	if !ok {
		return
	}
	drawOrder := &quotahost.QuotaApiDrawOrder{Balances: []*quotahost.QuotaApiBalance{}}
	var loadErr error
	server.HandleError(func() {
		for _, balance := range model.LoadDrawOrderBalances(r.Context(), networkId, self.now()) {
			drawOrder.Balances = append(drawOrder.Balances, &quotahost.QuotaApiBalance{
				BalanceId:       balance.BalanceId,
				CreditByteCount: max(balance.BalanceByteCount, 0),
				Credit:          balance.Credit,
				EndTime:         balance.EndTime,
			})
		}
	}, func(err error) {
		loadErr = err
	})
	if loadErr != nil {
		quotaServiceUnavailable(w, quotaServiceRouteDrawOrder, loadErr)
		return
	}
	quotaServiceWriteJson(w, drawOrder)
}

// POST /internal/gate/spent/{network_id}: marks the network spent (§20.6), as a lease that reserved nothing does.
func (self *QuotaServiceHandlers) PostSpent(w http.ResponseWriter, r *http.Request) {
	if _, ok := self.authenticate(w, r, quotaServiceRouteSpent); !ok {
		return
	}
	networkId, ok := quotaServiceNetworkId(w, r)
	if !ok {
		return
	}
	var markErr error
	server.HandleError(func() {
		markErr = model.MarkNetworkSpent(r.Context(), networkId, self.now())
	}, func(err error) {
		markErr = err
	})
	if markErr != nil {
		quotaServiceUnavailable(w, quotaServiceRouteSpent, markErr)
		return
	}
	quotaServiceWriteJson(w, struct{}{})
}

// POST /internal/gate/usage-batch: applies a quotahost.QuotaApiUsageBatch once by its id, its producer the
// caller's own, answering quotahost.QuotaApiUsageBatchResult.
func (self *QuotaServiceHandlers) PostUsageBatch(w http.ResponseWriter, r *http.Request) {
	caller, ok := self.authenticate(w, r, quotaServiceRouteUsageBatch)
	if !ok {
		return
	}
	wire := &quotahost.QuotaApiUsageBatch{}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, self.settings.MaxUsageBatchByteCount))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(wire); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "Usage batch too large.", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Invalid usage batch.", http.StatusBadRequest)
		return
	}
	if wire.Producer != string(caller) {
		quotaServiceRefusalCounter.WithLabelValues(quotaServiceRouteUsageBatch, "producer").Inc()
		http.Error(w, "Forbidden.", http.StatusForbidden)
		return
	}
	batch, carves, err := wire.UsageBatch()
	if err != nil {
		http.Error(w, "Invalid usage batch.", http.StatusBadRequest)
		return
	}
	var applied bool
	var applyErr error
	server.HandleError(func() {
		applied, applyErr = model.ApplyUsageBatch(r.Context(), batch, carves, self.now())
	}, func(err error) {
		applyErr = err
	})
	switch {
	case errors.Is(applyErr, model.ErrUsageBatchRefused):
		// the model's text says why, and names no network
		http.Error(w, applyErr.Error(), http.StatusBadRequest)
		return
	case applyErr != nil:
		quotaServiceUnavailable(w, quotaServiceRouteUsageBatch, applyErr)
		return
	}
	quotaServiceWriteJson(w, &quotahost.QuotaApiUsageBatchResult{Applied: applied})
}
