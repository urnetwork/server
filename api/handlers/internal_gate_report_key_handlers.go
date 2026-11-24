package handlers

// api's end of the report intake's seal key (plan-SMTP-report-seal-key, decided as its option 1, QD-075; §14.8, §15.8;
// §8.9's gate read surface): the route the platform listener's intake reads a network's mail key from, under
// server.InternalGatePathPrefix, taking only the platform listener's class's bearer
// (server.InternalGateCallerMxedgePlatform), verified by its hash in constant time (server.InternalGateVerifier), and
// answering every other request 403 before anything is read: no bearer, one api does not hold, another class's, a
// client's token or a staff member's.
//
//	GET /internal/gate/report-key/{network_id}  mxedge-platform  ->  smtpedgeservice.SmtpReportKeyReply
//
// The answer is controller.SmtpReportKeySource's: the drop key of the network mail key's newest generation, which api
// verified against the network's rows (smtpedgeservice.CheckSmtpReportSealKey), with its generation and expiry, or no
// field when the network has none to seal to. 503 with Retry-After is a key api cannot tell now, the network's state
// not loaded or gkm not answering, which the listener answers 451 so the reporter sends the report again; so is every
// request before api's projection has installed the source. The answer holds a public key and no repo id, and nothing
// here logs a bearer or a key. Safe for concurrent use.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/router"
)

// The route by the name its refusals count under.
const internalGateRouteReportKey = "report-key"

// Tunables of the route.
type InternalGateReportKeySettings struct {
	// What a 503 asks the listener to wait, as Retry-After in whole seconds: 60 s. The listener answers 451 and the
	// reporter's MTA keeps its own schedule, so this is a hint.
	UnavailableRetryAfter time.Duration
}

func DefaultInternalGateReportKeySettings() *InternalGateReportKeySettings {
	return &InternalGateReportKeySettings{
		UnavailableRetryAfter: 60 * time.Second,
	}
}

// What the handlers run on. Each nil takes api's own.
type InternalGateReportKeyDeps struct {
	// The bearers' digests. nil: server.LoadInternalGateVerifier from api's vault, read once; a vault without the
	// file refuses every call 403.
	Verifier *server.InternalGateVerifier
	// A network's key, nil for none. nil: controller.CurrentSmtpReportKeySource's, unavailable until api's projection
	// installs one.
	Key func(ctx context.Context, networkId sidecar.Id) (*smtpedgeservice.SmtpReportSealKey, error)
	// nil: the defaults.
	Settings *InternalGateReportKeySettings
}

// See the file header.
type InternalGateReportKeyHandlers struct {
	verifier func() *server.InternalGateVerifier
	key      func(ctx context.Context, networkId sidecar.Id) (*smtpedgeservice.SmtpReportSealKey, error)
	settings *InternalGateReportKeySettings
}

// The handlers on deps, nil taking api's own.
func NewInternalGateReportKeyHandlers(deps *InternalGateReportKeyDeps) *InternalGateReportKeyHandlers {
	if deps == nil {
		deps = &InternalGateReportKeyDeps{}
	}
	verifier := func() *server.InternalGateVerifier {
		return deps.Verifier
	}
	if deps.Verifier == nil {
		verifier = sync.OnceValue(func() *server.InternalGateVerifier {
			loaded, err := server.LoadInternalGateVerifier()
			if err != nil {
				// the error names the file and the part, never a bearer
				glog.Errorf("[internalgate]every report key read is refused: %v\n", err)
				return nil
			}
			return loaded
		})
	}
	key := deps.Key
	if key == nil {
		key = func(ctx context.Context, networkId sidecar.Id) (*smtpedgeservice.SmtpReportSealKey, error) {
			source := controller.CurrentSmtpReportKeySource()
			if source == nil {
				return nil, fmt.Errorf("%w: api's projection has not installed the source", smtpedgeservice.ErrSmtpReportKeyUnavailable)
			}
			return source.ReportSealKey(ctx, networkId)
		}
	}
	settings := deps.Settings
	if settings == nil {
		settings = DefaultInternalGateReportKeySettings()
	}
	return &InternalGateReportKeyHandlers{
		verifier: verifier,
		key:      key,
		settings: settings,
	}
}

// The one route, its pattern the source's path (smtpedgeservice.SmtpReportKeyGatePath), so both ends name one route.
func (self *InternalGateReportKeyHandlers) Routes() []*router.Route {
	return []*router.Route{
		router.NewRoute("GET", smtpedgeservice.SmtpReportKeyGatePath+"([^/]+)", self.GetReportKey),
	}
}

// Whether the request carries the platform listener's bearer; otherwise it is answered 403 and counted, before
// anything is read.
func (self *InternalGateReportKeyHandlers) authenticate(w http.ResponseWriter, r *http.Request) bool {
	refuse := func(refusal string) bool {
		quotaServiceRefusalCounter.WithLabelValues(internalGateRouteReportKey, refusal).Inc()
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
	if caller != server.InternalGateCallerMxedgePlatform {
		if glog.V(1) {
			glog.Infof("[internalgate]%s's bearer refused on %s\n", caller, internalGateRouteReportKey)
		}
		return refuse("class")
	}
	return true
}

// GET /internal/gate/report-key/{network_id}: the network's key to seal its reports to, or none.
func (self *InternalGateReportKeyHandlers) GetReportKey(w http.ResponseWriter, r *http.Request) {
	if !self.authenticate(w, r) {
		return
	}
	networkId, ok := internalGateProjectionNetworkId(w, router.GetPathValues(r))
	if !ok {
		return
	}
	key, err := self.key(r.Context(), sidecar.Id(networkId))
	if err != nil {
		// the source's error names ids and positions only; every error is a key not told now, its own kind or not
		glog.Infof("[internalgate]a network's report key was not told now, so the report is asked for again: %v\n", err)
		w.Header().Set("Retry-After", strconv.Itoa(int(self.settings.UnavailableRetryAfter/time.Second)))
		http.Error(w, "Unavailable.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(smtpedgeservice.NewSmtpReportKeyReply(key)); err != nil {
		glog.Infof("[internalgate]could not write a report key answer\n")
	}
}
