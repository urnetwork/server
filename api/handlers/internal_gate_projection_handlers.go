package handlers

// api's end of the internal gate's projection reads (ACL-M4-05; QD-680, QD-055's plan-SOCK-gate-data): how a gate
// service with no reach to the metadata tier reads a network's authorization log, sidecar.AclGateApiSource being the
// other end. Both routes sit under server.InternalGatePathPrefix and take only a gate host's bearer, verified by its
// hash in constant time (server.InternalGateVerifier, QD-027), answering every other request 403 before a read:
//
//	GET /internal/gate/projection/{network_id}?after=<n>  connect sockets tlsedge mxedge  ConfigLogSequencer.Read
//	    GET /network/config/projection's body (controller.NetworkConfigProjectionResult): the entries after n, a
//	    page at most and from the retention point when n is before it, the rows of each base among them and, from
//	    0, the Signed Genesis, every message verbatim in base64
//	GET /internal/gate/rows/{network_id}/{rows_sha256}    the same                        ConfigLogSequencer.ReadRows
//	    one base's ProjectionRows, verbatim, as application/octet-stream, the hash in lower-case hex; 404 when the
//	    store holds none by that hash
//
// api is a carrier and decides nothing: a gate checks every entry's chain from its own pin and holds rows to their
// hash (sidecar.AclGateCache), so a stale or short answer costs it a load and never a lower n. evs and gkm read
// proj/ themselves (AclGateDirectSource), so their bearers reach neither route. A process with no first metadata
// cluster, or a store that cannot answer, answers 503; a network with no log answers an empty page.
//
// QD-055 also has the lb refuse the prefix from outside, which warp does not do yet, so the bearer is the gate alone
// until it does. Safe for concurrent use.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// Each route by the name its refusals count under.
const (
	internalGateRouteProjection = "projection"
	internalGateRouteRows       = "rows"
)

// The classes both routes take: the gate services that read projections through api (C-09, QD-055).
var internalGateProjectionCallers = []server.InternalGateCaller{
	server.InternalGateCallerConnect,
	server.InternalGateCallerSockets,
	server.InternalGateCallerTlsedge,
	server.InternalGateCallerMxedge,
}

// What the handlers run on. Each nil takes api's own.
type InternalGateProjectionDeps struct {
	// The bearers' digests. nil: server.LoadInternalGateVerifier from api's vault, read once; a vault without the
	// file refuses every call 403.
	Verifier *server.InternalGateVerifier
	// The log's sequencer. nil: model.CurrentConfigLogSequencer, this process's, 503 while api has none.
	Sequencer func() (*model.ConfigLogSequencer, error)
}

// See the file header.
type InternalGateProjectionHandlers struct {
	verifier  func() *server.InternalGateVerifier
	sequencer func() (*model.ConfigLogSequencer, error)
}

// The handlers on deps, nil taking api's own.
func NewInternalGateProjectionHandlers(deps *InternalGateProjectionDeps) *InternalGateProjectionHandlers {
	if deps == nil {
		deps = &InternalGateProjectionDeps{}
	}
	verifier := func() *server.InternalGateVerifier {
		return deps.Verifier
	}
	if deps.Verifier == nil {
		verifier = sync.OnceValue(func() *server.InternalGateVerifier {
			loaded, err := server.LoadInternalGateVerifier()
			if err != nil {
				// the error names the file and the part, never a bearer
				glog.Errorf("[internalgate]every projection read is refused: %v\n", err)
				return nil
			}
			return loaded
		})
	}
	sequencer := deps.Sequencer
	if sequencer == nil {
		sequencer = model.CurrentConfigLogSequencer
	}
	return &InternalGateProjectionHandlers{
		verifier:  verifier,
		sequencer: sequencer,
	}
}

// The two routes, each pattern the client's path (sidecar.AclGateApiProjectionPath, AclGateApiRowsPath), so both ends
// name one route.
func (self *InternalGateProjectionHandlers) Routes() []*router.Route {
	return []*router.Route{
		router.NewRoute("GET", sidecar.AclGateApiProjectionPath+"([^/]+)", self.GetProjection),
		router.NewRoute("GET", sidecar.AclGateApiRowsPath+"([^/]+)/([^/]+)", self.GetRows),
	}
}

// Whether the request carries the bearer of a class the routes take; otherwise it is answered 403 and counted, before
// anything is read.
func (self *InternalGateProjectionHandlers) authenticate(w http.ResponseWriter, r *http.Request, route string) bool {
	refuse := func(refusal string) bool {
		quotaServiceRefusalCounter.WithLabelValues(route, refusal).Inc()
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
	if !slices.Contains(internalGateProjectionCallers, caller) {
		if glog.V(1) {
			glog.Infof("[internalgate]%s's bearer refused on %s\n", caller, route)
		}
		return refuse("class")
	}
	return true
}

// The route's network id, its first path value; a malformed or zero id is answered 400.
func internalGateProjectionNetworkId(w http.ResponseWriter, pathValues []string) (server.Id, bool) {
	if len(pathValues) == 0 {
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

// Answers 503 for a sequencer or store that could not answer, saying why in the log alone.
func internalGateProjectionUnavailable(w http.ResponseWriter, route string, err error) {
	if glog.V(1) {
		glog.Infof("[internalgate]%s unavailable: %s\n", route, err)
	}
	http.Error(w, "Unavailable.", http.StatusServiceUnavailable)
}

// GET /internal/gate/projection/{network_id}?after=<n>: controller.NetworkConfigProjectionResult, a thin wrap of
// ConfigLogSequencer.Read. A missing after reads from 0, and one that is no decimal position is answered 400.
func (self *InternalGateProjectionHandlers) GetProjection(w http.ResponseWriter, r *http.Request) {
	if !self.authenticate(w, r, internalGateRouteProjection) {
		return
	}
	networkId, ok := internalGateProjectionNetworkId(w, router.GetPathValues(r))
	if !ok {
		return
	}
	after := uint64(0)
	if afterText := r.URL.Query().Get("after"); afterText != "" {
		var err error
		if after, err = strconv.ParseUint(afterText, 10, 64); err != nil {
			http.Error(w, "Invalid after.", http.StatusBadRequest)
			return
		}
	}
	sequencer, err := self.sequencer()
	if err != nil {
		internalGateProjectionUnavailable(w, internalGateRouteProjection, err)
		return
	}
	read, err := sequencer.Read(r.Context(), networkId, after)
	if err != nil {
		if errors.Is(err, model.ErrConfigLogGenesisMissing) {
			err = errors.New("the log's genesis is not written yet")
		}
		internalGateProjectionUnavailable(w, internalGateRouteProjection, err)
		return
	}
	result := &controller.NetworkConfigProjectionResult{
		Genesis: read.SignedGenesis,
		Entries: read.Entries,
		Rows:    read.Rows,
		More:    read.More,
	}
	if read.Tip != nil {
		result.Tip = &controller.NetworkConfigPinResult{N: read.Tip.GetVersion(), Chain: read.Tip.GetChain()}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		glog.Infof("[internalgate]could not write a projection page: %s\n", err)
	}
}

// GET /internal/gate/rows/{network_id}/{rows_sha256}: one base's ProjectionRows, verbatim.
func (self *InternalGateProjectionHandlers) GetRows(w http.ResponseWriter, r *http.Request) {
	if !self.authenticate(w, r, internalGateRouteRows) {
		return
	}
	pathValues := router.GetPathValues(r)
	networkId, ok := internalGateProjectionNetworkId(w, pathValues)
	if !ok {
		return
	}
	if len(pathValues) != 2 {
		http.Error(w, "Invalid rows hash.", http.StatusBadRequest)
		return
	}
	// a SHA-256 in lower-case hex, the one form the client sends
	rowsSha256, err := hex.DecodeString(pathValues[1])
	if err != nil || len(rowsSha256) != sha256.Size || hex.EncodeToString(rowsSha256) != pathValues[1] {
		http.Error(w, "Invalid rows hash.", http.StatusBadRequest)
		return
	}
	sequencer, err := self.sequencer()
	if err != nil {
		internalGateProjectionUnavailable(w, internalGateRouteRows, err)
		return
	}
	rows, found, err := sequencer.ReadRows(r.Context(), networkId, rowsSha256)
	if err != nil {
		internalGateProjectionUnavailable(w, internalGateRouteRows, err)
		return
	}
	if !found {
		http.Error(w, "Not found.", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := w.Write(rows); err != nil {
		glog.Infof("[internalgate]could not write rows: %s\n", err)
	}
}
