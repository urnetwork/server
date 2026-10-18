package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/controller"
)

// maxProviderEgressHealthBody bounds the request body. It is larger than the
// bandwidth endpoints' 4 KiB cap because this body carries a per-class map plus
// five joined destination-name lists, each of which the prober bounds to 512
// bytes.
const maxProviderEgressHealthBody = 128 * 1024

// The wire shapes live in controller, which validates and stores the run for
// this handler and for the taskworker's in-process operator alike.
type ProviderEgressHealthClassResult = controller.ProviderEgressHealthClassResult

type SubmitProviderEgressHealthArgs = controller.SubmitProviderEgressHealthArgs

// readStrictOperatorRequestBody reads a bounded operator request body and
// rejects any field the target struct does not declare.
//
// It is a separate reader from readOperatorRequestBody rather than a flag on
// it: that one is shared with the bandwidth endpoints, which are already
// deployed and already accept whatever their probers send, and tightening a
// live endpoint's parser as a side effect of adding a new one is how a working
// fleet stops submitting overnight.
//
// Rejecting unknown fields matters here specifically because this body is a
// set of counts that must agree with each other. A misspelled field silently
// decodes to zero, and a zero count is a perfectly valid, perfectly consistent
// payload -- so the failure would be a table full of plausible rows describing
// a measurement that never happened.
func readStrictOperatorRequestBody(w http.ResponseWriter, r *http.Request, out any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProviderEgressHealthBody+1))
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return false
	}
	if maxProviderEgressHealthBody < len(body) {
		http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return false
	}
	return true
}

// ProviderEgressHealthResult ingests one egress-health run from the operator's
// prober. The route is operator-to-server, gated by the same operator secret as
// the egress-location and bandwidth ingest endpoints. There is no network jwt.
// Every rule is controller.SubmitProviderEgressHealth's, applied before
// anything is stored.
func ProviderEgressHealthResult(w http.ResponseWriter, r *http.Request) {
	if !authorizeOperator(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var args SubmitProviderEgressHealthArgs
	if !readStrictOperatorRequestBody(w, r, &args) {
		return
	}

	if err := controller.SubmitProviderEgressHealth(r.Context(), &args); err != nil {
		writeProviderEgressIngestError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{}); err != nil {
		glog.Infof("[pegh]could not write response. err = %s\n", err)
	}
}
