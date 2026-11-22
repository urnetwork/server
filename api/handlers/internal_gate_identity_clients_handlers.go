package handlers

// api's end of §5.15's sidecar_id to client lookup (plan-KEYS-hint-addressing, QD-146), the route
// server.SidecarIdentityClientsResolver calls for the hint writers of hosts that keep Postgres off §6.8's hint path.
// It sits under server.InternalGatePathPrefix and takes only the gkm and mxedge classes' bearers, verified by hash in
// constant time (server.InternalGateVerifier), answering every other request 403 before anything is read:
//
//	POST /internal/gate/sidecar-identity-clients  gkm mxedge  {sidecar_ids}  ->  {clients: [{sidecar_id, client_id, generation}]}
//
// Each named sidecar api holds an active identity of answers its newest binding's client and generation, in the
// request's order; unknown and revoked ids are omitted, alike. At most the format's SidecarIdMaxCount ids, 1,000, a
// request, and a field beyond the format's is refused 400. 503 is a lookup that could not run: a binary without MG1, or
// a store that did not answer. It logs no per-target record (§2.8's api-5): no line here names a sidecar or a client.
// Safe for concurrent use.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// The route by the name its refusals count under.
const internalGateRouteIdentityClients = "sidecar-identity-clients"

// The classes the route takes: the hosts §5.15 names that resolve sidecars for their hints with no Postgres.
var internalGateIdentityClientsCallers = []server.InternalGateCaller{
	server.InternalGateCallerGkm,
	server.InternalGateCallerMxedge,
}

// Tunables of the route.
type InternalGateIdentityClientsSettings struct {
	// The most bytes a request's body may hold: 64 KiB, the format's 1,000 ids with room over.
	RequestMaxByteCount int64
	// What a 503 asks the caller to wait, as Retry-After in whole seconds: 5 s. A hint the caller then loses is
	// covered by its clients' hourly resync.
	UnavailableRetryAfter time.Duration
	// The route's format, which the resolver carries too. nil: server.DefaultSidecarIdentityClientsGateFormatSettings.
	Format *server.SidecarIdentityClientsGateFormatSettings
}

func DefaultInternalGateIdentityClientsSettings() *InternalGateIdentityClientsSettings {
	return &InternalGateIdentityClientsSettings{
		RequestMaxByteCount:   64 * 1024,
		UnavailableRetryAfter: 5 * time.Second,
		Format:                server.DefaultSidecarIdentityClientsGateFormatSettings(),
	}
}

// What the handlers run on. Each nil takes api's own.
type InternalGateIdentityClientsDeps struct {
	// The bearers' digests. nil: server.LoadInternalGateVerifier from api's vault, read once; a vault without the
	// file refuses every call 403.
	Verifier *server.InternalGateVerifier
	// The lookup. nil: model.GetSidecarCurrentClients.
	Lookup func(ctx context.Context, sidecarIds []server.Id) (map[server.Id]*model.SidecarCurrentClient, error)
	// nil: the defaults.
	Settings *InternalGateIdentityClientsSettings
}

// See the file header.
type InternalGateIdentityClientsHandlers struct {
	verifier func() *server.InternalGateVerifier
	lookup   func(ctx context.Context, sidecarIds []server.Id) (map[server.Id]*model.SidecarCurrentClient, error)
	settings *InternalGateIdentityClientsSettings
	format   *server.SidecarIdentityClientsGateFormatSettings
}

// The handlers on deps, nil taking api's own.
func NewInternalGateIdentityClientsHandlers(deps *InternalGateIdentityClientsDeps) *InternalGateIdentityClientsHandlers {
	if deps == nil {
		deps = &InternalGateIdentityClientsDeps{}
	}
	verifier := func() *server.InternalGateVerifier {
		return deps.Verifier
	}
	if deps.Verifier == nil {
		verifier = sync.OnceValue(func() *server.InternalGateVerifier {
			loaded, err := server.LoadInternalGateVerifier()
			if err != nil {
				// the error names the file and the part, never a bearer
				glog.Errorf("[internalgate]every sidecar identity clients lookup is refused: %v\n", err)
				return nil
			}
			return loaded
		})
	}
	lookup := deps.Lookup
	if lookup == nil {
		lookup = model.GetSidecarCurrentClients
	}
	settings := deps.Settings
	if settings == nil {
		settings = DefaultInternalGateIdentityClientsSettings()
	}
	format := settings.Format
	if format == nil {
		format = server.DefaultSidecarIdentityClientsGateFormatSettings()
	}
	return &InternalGateIdentityClientsHandlers{
		verifier: verifier,
		lookup:   lookup,
		settings: settings,
		format:   format,
	}
}

// The one route, its pattern the client's path (server.SidecarIdentityClientsGatePath), so both ends name one route.
func (self *InternalGateIdentityClientsHandlers) Routes() []*router.Route {
	return []*router.Route{
		router.NewRoute("POST", server.SidecarIdentityClientsGatePath, self.PostIdentityClients),
	}
}

// Whether the request carries the bearer of a class the route takes; otherwise it is answered 403 and counted, before
// anything is read.
func (self *InternalGateIdentityClientsHandlers) authenticate(w http.ResponseWriter, r *http.Request) bool {
	refuse := func(refusal string) bool {
		quotaServiceRefusalCounter.WithLabelValues(internalGateRouteIdentityClients, refusal).Inc()
		http.Error(w, "Forbidden.", http.StatusForbidden)
		return false
	}
	verifier := self.verifier()
	if verifier == nil {
		return refuse("no_verify_file")
	}
	caller, ok := verifier.Authenticate(r)
	if !ok {
		return refuse("credential")
	}
	if !slices.Contains(internalGateIdentityClientsCallers, caller) {
		if glog.V(1) {
			glog.Infof("[internalgate]%s's bearer refused on %s\n", caller, internalGateRouteIdentityClients)
		}
		return refuse("class")
	}
	return true
}

// Refuses a request the route's format does not take, saying which part in the log alone.
func (self *InternalGateIdentityClientsHandlers) invalid(w http.ResponseWriter, reason string) {
	if glog.V(1) {
		glog.Infof("[internalgate]%s refused: %s\n", internalGateRouteIdentityClients, reason)
	}
	http.Error(w, "Invalid request.", http.StatusBadRequest)
}

// POST /internal/gate/sidecar-identity-clients: the named sidecars' current clients.
func (self *InternalGateIdentityClientsHandlers) PostIdentityClients(w http.ResponseWriter, r *http.Request) {
	if !self.authenticate(w, r) {
		return
	}
	args := &server.SidecarIdentityClientsGateArgs{}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, self.settings.RequestMaxByteCount))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(args); err != nil {
		self.invalid(w, "a body that is not the route's format")
		return
	}
	if err := decoder.Decode(&json.RawMessage{}); err != io.EOF {
		self.invalid(w, "a second value after the request")
		return
	}
	if err := args.Validate(self.format); err != nil {
		// the error names the part, never an id
		self.invalid(w, err.Error())
		return
	}
	clients, err := self.lookup(r.Context(), args.SidecarIds)
	if err != nil {
		reason := "the store did not answer"
		if errors.Is(err, model.ErrSidecarIdentityClientsUnlinked) {
			reason = "this binary does not link MG1"
		}
		glog.Infof("[internalgate]%s unavailable: %s\n", internalGateRouteIdentityClients, reason)
		w.Header().Set("Retry-After", strconv.Itoa(int(self.settings.UnavailableRetryAfter/time.Second)))
		http.Error(w, "Unavailable.", http.StatusServiceUnavailable)
		return
	}
	result := &server.SidecarIdentityClientsGateResult{Clients: []*server.SidecarIdentityClientsGateClient{}}
	answered := map[server.Id]bool{}
	for _, sidecarId := range args.SidecarIds {
		client, ok := clients[sidecarId]
		if !ok || answered[sidecarId] {
			continue
		}
		answered[sidecarId] = true
		result.Clients = append(result.Clients, &server.SidecarIdentityClientsGateClient{
			SidecarId:  sidecarId,
			ClientId:   client.ClientId,
			Generation: client.Generation,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[internalgate]could not write a sidecar identity clients answer\n")
	}
}
