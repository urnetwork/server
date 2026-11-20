package handlers

// api's end of a Team direct MX's probe outcomes (QD-1103, owner decision 639; MAIL-M7-23; §15.4's Team paragraph,
// §12.8's direct MX): the route mxedge's direct-MX prober posts each outcome on (server.MailDirectMxProbeGatePath),
// one of the edge class's routes beside its client mint (internal_gate_edge_client_handlers.go), taking only that
// class's bearer and answering every other request 403 before anything is read:
//
//	POST /internal/gate/mail-direct-mx-probe  edge  {network_id, receiver_sidecar_id, address, passed}
//	  -> {recorded}
//
// The prober probes from the edge, since owner decision 33's check of public TCP 25, STARTTLS and the receiver's
// mail_host_key SPKI has to run there, and api records what it found, so mxedge's restricted role writes neither
// mail_direct_mx nor the zone, whose writers stay api and the dns block (D-OPS-1). api believes the outcome it is
// sent, so what the edge bearer can do here, stolen from any edge block that mounts it, is bounded by what the rows
// hold: it can mark a row a Team's Owner or Admin named verified or failing, so mx points only at an address that
// admin named and §12.7's A rules take, never at one the caller chooses, and only DELETE or a removal takes a row.
//
// The body is the route's format (server.MailDirectMxProbeGateArgs) and nothing more: a field beyond it, a second
// value, a zero id or an address not in its canonical text is refused 400 with nothing written. The handler records
// the outcome in api's own dns root (model.RecordMailDirectMxProbe): the row's state and, on a pass, §12.8's records
// and the network's routes, only while the row still stands. A row gone in between answers recorded false, a no-op
// success; a network api has no row of answers 404; a zone that refuses the records now, the network no longer a
// Team, its mail off, the address past §12.7's A rules or the zone suspended, answers 409; and a process that cannot
// record now, the mail routes dark (no MG2, MG5 and MG7, or no dns root), the dns restoring or a record that failed,
// answers 503 with Retry-After. Each refusal leaves the row as it was, for the prober's next pass, 5 minutes on.
//
// Nothing here logs a bearer, a network, a receiver or an address; a refusal's log line names the part refused. Safe
// for concurrent use.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The route by the name its refusals count under.
const internalGateRouteMailDirectMxProbe = "mail-direct-mx-probe"

// Tunables of the direct-MX probe route.
type InternalGateMailDirectMxProbeSettings struct {
	// The most bytes a request's body may hold: 4 KiB, two ids, an address and a flag many times over.
	RequestMaxByteCount int64
	// What a 503 asks the prober to wait, as Retry-After in whole seconds: 60 s. The prober keeps its own 5-minute
	// pass, so this is a hint.
	UnavailableRetryAfter time.Duration
}

func DefaultInternalGateMailDirectMxProbeSettings() *InternalGateMailDirectMxProbeSettings {
	return &InternalGateMailDirectMxProbeSettings{
		RequestMaxByteCount:   4 * 1024,
		UnavailableRetryAfter: 60 * time.Second,
	}
}

// Refuses a request the route's format does not take, saying which part in the log alone.
func (self *InternalGateEdgeClientHandlers) mailDirectMxProbeInvalid(w http.ResponseWriter, reason string) {
	if glog.V(1) {
		glog.Infof("[internalgate]%s refused: %s\n", internalGateRouteMailDirectMxProbe, reason)
	}
	http.Error(w, "Invalid request.", http.StatusBadRequest)
}

// Answers status, saying why in the log alone, with Retry-After on a 503.
func (self *InternalGateEdgeClientHandlers) mailDirectMxProbeRefused(w http.ResponseWriter, status int, reason string) {
	if glog.V(1) {
		glog.Infof("[internalgate]%s answered %d: %s\n", internalGateRouteMailDirectMxProbe, status, reason)
	}
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", strconv.Itoa(int(self.mailDirectMxProbe.UnavailableRetryAfter/time.Second)))
	}
	http.Error(w, http.StatusText(status)+".", status)
}

// POST /internal/gate/mail-direct-mx-probe: one probe's outcome, recorded or found gone.
func (self *InternalGateEdgeClientHandlers) PostMailDirectMxProbe(w http.ResponseWriter, r *http.Request) {
	if !self.authenticate(w, r, internalGateRouteMailDirectMxProbe) {
		return
	}
	if !self.mailOpen() {
		self.mailDirectMxProbeRefused(w, http.StatusServiceUnavailable, "the mail routes are dark in this process")
		return
	}
	args := &server.MailDirectMxProbeGateArgs{}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, self.mailDirectMxProbe.RequestMaxByteCount))
	// a field beyond the format's is refused, not ignored
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(args); err != nil {
		self.mailDirectMxProbeInvalid(w, "a body that is not the route's format")
		return
	}
	if err := decoder.Decode(&json.RawMessage{}); err != io.EOF {
		self.mailDirectMxProbeInvalid(w, "a second value after the request")
		return
	}
	address, err := args.Validate()
	if err != nil {
		// the error names the part, never its value
		self.mailDirectMxProbeInvalid(w, err.Error())
		return
	}
	recorded, err := self.recordMailDirectMxProbe(r.Context(), &model.MailDirectMxProbe{
		NetworkId:         args.NetworkId,
		ReceiverSidecarId: args.ReceiverSidecarId,
		Address:           address,
		Passed:            args.Passed,
	})
	var quota *model.QuotaRefusalError
	switch {
	case err == nil:
	case errors.Is(err, model.ErrMailDirectMxProbeNetworkUnknown):
		self.mailDirectMxProbeRefused(w, http.StatusNotFound, "no such network")
		return
	case errors.As(err, &quota), errors.Is(err, model.ErrDnsDirectMxMailOff), errors.Is(err, model.ErrMailDirectMxMailOff),
		errors.Is(err, dnsservice.ErrDnsDirectMxRefused), errors.Is(err, model.ErrDnsZoneSuspended), errors.Is(err, model.ErrMailDirectMxInvalid):
		// the zone's rules refuse its records now: no longer a Team, mail off, the address past §12.7's A rules, or the
		// zone suspended
		self.mailDirectMxProbeRefused(w, http.StatusConflict, "the zone refuses the records now")
		return
	case errors.Is(err, model.ErrMailUnlinked), errors.Is(err, model.ErrDnsUnlinked), errors.Is(err, dnsservice.ErrDnsRestoring):
		self.mailDirectMxProbeRefused(w, http.StatusServiceUnavailable, "the outcome cannot be recorded now")
		return
	default:
		// a store error is said by its kind alone
		self.mailDirectMxProbeRefused(w, http.StatusServiceUnavailable, "the record failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(&server.MailDirectMxProbeGateResult{Recorded: recorded}); err != nil {
		glog.Infof("[internalgate]could not write a direct-MX probe's answer\n")
	}
}
