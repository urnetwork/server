package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// maxProviderEgressHealthBody bounds the request body. It is larger than the
// bandwidth endpoints' 4 KiB cap because this body carries a per-class map plus
// five joined destination-name lists, each of which the prober bounds to 512
// bytes.
const maxProviderEgressHealthBody = 128 * 1024

// providerEgressHealthReputationClass is the class name that must never appear
// inside class_results. See ProviderEgressHealthResult for why this is checked
// rather than tolerated.
const providerEgressHealthReputationClass = "reputation"

// ProviderEgressHealthClassResult is one class's ok/total tally over the
// destinations the run SAMPLED.
type ProviderEgressHealthClassResult struct {
	OK    int `json:"ok"`
	Total int `json:"total"`
}

// One immutable URL outcome, or an older aggregate retained only for audit.
// Validated URL evidence preserves its measurement time rather than arrival
// time, so delayed publication cannot refresh rolling eligibility. Timestamps
// more than one minute in the future are rejected. Legacy evidence without a
// policy timestamp uses arrival time but cannot satisfy the URL-policy gate.
type SubmitProviderEgressHealthArgs struct {
	ClientId         server.Id                      `json:"client_id"`
	RunId            server.Id                      `json:"run_id"`
	CycleStartedAt   time.Time                      `json:"cycle_started_at,omitempty"`
	UrlProbeEvidence *egresshealth.UrlProbeEvidence `json:"url_probe_evidence,omitempty"`
	// OKCount/TotalCount cover every class over the measured scored loads. A
	// load that was not measured, and a canary, is in neither.
	OKCount    int `json:"ok_count"`
	TotalCount int `json:"total_count"`
	// ClassResults is the per-class tally. Its ok and total must sum to
	// exactly OKCount and TotalCount.
	ClassResults map[string]ProviderEgressHealthClassResult `json:"class_results"`
	// ReputationOK/ReputationTotal/ReputationFailedNames are the dissolved
	// reputation class's: its sites are scored with the rest now. They are
	// accepted, always zero from a current prober, and ignored.
	ReputationOK          int    `json:"reputation_ok"`
	ReputationTotal       int    `json:"reputation_total"`
	FailedNames           string `json:"failed_names"`
	ReputationFailedNames string `json:"reputation_failed_names"`
	// TLSAuthenticationFailure is separate from the score. A single peer that
	// cannot authenticate the requested HTTPS host is a hard integrity failure,
	// even when unrelated destinations make ok_count/total_count look healthy.
	TLSAuthenticationFailure bool `json:"tls_authentication_failure"`
	// NotMeasuredCount/NotMeasuredNames are the loads whose tunnel was gone and
	// could not be re-created in time: already out of ok_count and
	// total_count, and never counted against the exit.
	NotMeasuredCount int    `json:"not_measured_count"`
	NotMeasuredNames string `json:"not_measured_names"`
	// CanaryPassedNames/CanaryFailedNames are the unscored canaries of the
	// run, loaded from a place their site is marked incompatible with.
	CanaryPassedNames string `json:"canary_passed_names"`
	CanaryFailedNames string `json:"canary_failed_names"`
	// ShortClasses is the comma-joined classes too thin, for the provider's
	// place, to fill their sample.
	ShortClasses string `json:"short_classes"`
}

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
//
// # Everything is validated before anything is stored
//
// The row is an upsert keyed on client_id, so a bad submission does not sit
// beside the good one waiting to be noticed -- it DESTROYS the last good
// measurement for that provider. That is why every rule below returns 400
// before the store, and why none of them is a stored-then-flagged warning.
//
// # What counts
//
// The counts are the prober's, over the loads it measured after their
// retries, with one server-side cut: a failed load of a site the pool has on
// probation, or of one marked incompatible with the provider's place, leaves
// the counts and is named in unscored_failed_names (model.
// ScoreProviderEgressHealth). The probe task submits runs already cut this way,
// passes included; the cut here is what the wire allows for any other
// submitter, since a run names its failures and not its passes.
//
// A "reputation" key inside class_results is still rejected outright. The
// class dissolved into the sites, so no current prober sends one; a body that
// carries one is from a prober that would have scored the class twice, and the
// sum check below would otherwise be "fixed" by relaxing it.
func ProviderEgressHealthResult(w http.ResponseWriter, r *http.Request) {
	if !authorizeOperator(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var args SubmitProviderEgressHealthArgs
	if !readStrictOperatorRequestBody(w, r, &args) {
		return
	}

	if args.ClientId == (server.Id{}) {
		http.Error(w, "Missing client id.", http.StatusBadRequest)
		return
	}
	if err := args.UrlProbeEvidence.ValidateOutcome(args.OKCount, args.TotalCount, args.TLSAuthenticationFailure); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if args.UrlProbeEvidence != nil && args.UrlProbeEvidence.MeasuredAt.After(server.NowUtc().Add(time.Minute)) {
		http.Error(w, "URL evidence timestamp is in the future.", http.StatusBadRequest)
		return
	}
	if args.UrlProbeEvidence != nil && args.RunId == (server.Id{}) {
		http.Error(w, "A versioned URL result requires an immutable run id.", http.StatusBadRequest)
		return
	}
	securityOnly := args.TotalCount == 0 && args.UrlProbeEvidence != nil && len(args.UrlProbeEvidence.Security) > 0
	if !args.CycleStartedAt.IsZero() && (args.RunId == (server.Id{}) || args.UrlProbeEvidence == nil || (args.TotalCount != 1 && !securityOnly)) {
		http.Error(w, "A scheduled URL probe requires a run id and versioned evidence for one measured outcome or independent TLS security evidence.", http.StatusBadRequest)
		return
	}
	if args.UrlProbeEvidence != nil {
		tlsFailure := false
		for _, event := range args.UrlProbeEvidence.Security {
			tlsFailure = tlsFailure || event.TlsFailure
		}
		if tlsFailure != args.TLSAuthenticationFailure {
			http.Error(w, "URL security evidence disagrees with the aggregate TLS flag.", http.StatusBadRequest)
			return
		}
	}
	if args.OKCount < 0 || args.TotalCount < 0 {
		http.Error(w, "ok_count and total_count must be non-negative.", http.StatusBadRequest)
		return
	}
	if args.ReputationOK < 0 || args.ReputationTotal < 0 {
		http.Error(w, "reputation_ok and reputation_total must be non-negative.", http.StatusBadRequest)
		return
	}
	if args.NotMeasuredCount < 0 {
		http.Error(w, "not_measured_count must be non-negative.", http.StatusBadRequest)
		return
	}
	if args.TotalCount < args.OKCount {
		// more destinations passed than were attempted: the submitter is not
		// measuring what it thinks it is, and storing this would overwrite a
		// real measurement with an impossible one
		http.Error(w, "ok_count must not exceed total_count.", http.StatusBadRequest)
		return
	}
	if args.ReputationTotal < args.ReputationOK {
		http.Error(w, "reputation_ok must not exceed reputation_total.", http.StatusBadRequest)
		return
	}

	sumOK, sumTotal := 0, 0
	for class, tally := range args.ClassResults {
		if class == providerEgressHealthReputationClass {
			http.Error(w, fmt.Sprintf(
				"%q is not a scored class: it is reported in reputation_ok/reputation_total and must never be part of ok_count/total_count.",
				providerEgressHealthReputationClass,
			), http.StatusBadRequest)
			return
		}
		if tally.OK < 0 || tally.Total < 0 {
			http.Error(w, fmt.Sprintf("class %q: ok and total must be non-negative.", class), http.StatusBadRequest)
			return
		}
		if tally.Total < tally.OK {
			// checked per class as well as in aggregate: {ok:5,total:2} and
			// {ok:0,total:3} sum to a consistent 5/5 while describing a class
			// where more destinations passed than ran
			http.Error(w, fmt.Sprintf("class %q: ok must not exceed total.", class), http.StatusBadRequest)
			return
		}
		sumOK += tally.OK
		sumTotal += tally.Total
	}
	// exact equality, not <=: the classes ARE the score. A total that does not
	// decompose into its classes means the two halves of the payload were
	// produced by different runs, or that something not in class_results was
	// counted into the score -- which is precisely how reputation would get in.
	if sumOK != args.OKCount || sumTotal != args.TotalCount {
		http.Error(w, fmt.Sprintf(
			"class_results sum to %d/%d but ok_count/total_count are %d/%d.",
			sumOK, sumTotal, args.OKCount, args.TotalCount,
		), http.StatusBadRequest)
		return
	}

	classResults := map[string]model.ProviderEgressHealthClassResult{}
	for class, tally := range args.ClassResults {
		classResults[class] = model.ProviderEgressHealthClassResult{
			OK:    tally.OK,
			Total: tally.Total,
		}
	}

	health := &model.ProviderEgressHealth{
		ClientId:         args.ClientId,
		RunId:            args.RunId,
		CycleStartedAt:   args.CycleStartedAt,
		MeasuredAt:       server.NowUtc(),
		OKCount:          args.OKCount,
		Total:            args.TotalCount,
		ClassResults:     classResults,
		UrlProbeEvidence: args.UrlProbeEvidence,
		// the reputation class dissolved into the sites (GEOMAP §11.3):
		// whatever an older prober sends here is not a measurement anything
		// reads, and is stored as none
		ReputationOK:             0,
		ReputationTotal:          0,
		FailedNames:              args.FailedNames,
		ReputationFailedNames:    "",
		TLSAuthenticationFailure: args.TLSAuthenticationFailure,
		NotMeasuredCount:         args.NotMeasuredCount,
		NotMeasuredNames:         args.NotMeasuredNames,
		CanaryPassedNames:        args.CanaryPassedNames,
		CanaryFailedNames:        args.CanaryFailedNames,
		ShortClasses:             args.ShortClasses,
	}
	if args.UrlProbeEvidence != nil {
		health.MeasuredAt = args.UrlProbeEvidence.MeasuredAt
	}
	// Preserve legacy scoring semantics only for legacy diagnostic reports.
	// Every versioned URL error remains in its policy-version denominator.
	if args.UrlProbeEvidence == nil {
		place := model.GetProviderEgressPlaces(r.Context(), []server.Id{args.ClientId})[args.ClientId]
		health = model.ScoreProviderEgressHealth(health, place, model.GetProviderEgressHealthScoring(r.Context()))
	}

	model.SetProviderEgressHealth(r.Context(), health)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{}); err != nil {
		glog.Infof("[pegh]could not write response. err = %s\n", err)
	}
}
