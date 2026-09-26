package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The egress-health submission: one run's tallies, names and flags in the
// server's fixed wire format.

// One class's ok/total tally over the destinations
// this run sampled.
type egressHealthClassBody struct {
	Ok    int `json:"ok"`
	Total int `json:"total"`
}

// The fixed contract of the server's
// handlers.SubmitProviderEgressHealthArgs
// (POST /network/provider-egress-health, X-UR-Operator-Secret header).
//
// The server rejects unknown fields, so this struct's json tags are not a
// convention here -- they are the wire format, and a rename on either side
// makes every submission 400. There is deliberately no measured_at: the server
// stamps arrival time, so a skewed prober clock cannot write a row that looks
// stale or future-dated.
type submitEgressHealthBody struct {
	ClientId string `json:"client_id"`
	// With TotalCount, covers every class over the loads this run sampled and
	// measured, after their retries: ok is a load that passed on some attempt,
	// total - ok a load that failed every one, which is what the index and the
	// one-in-ten rule count. The server checks that ClassResults sums to
	// exactly these two. A load that was not measured, and a canary, is in
	// neither -- see below.
	OkCount      int                              `json:"ok_count"`
	TotalCount   int                              `json:"total_count"`
	ClassResults map[string]egressHealthClassBody `json:"class_results"`
	// The tallies and failed names of the dissolved reputation class, which
	// is scored with the sites now (GEOMAP §11.3).
	// They stay on the wire, always zero and empty, for one release, because a
	// server that still declares them keeps decoding the body; the server
	// accepts them as zero and ignores them.
	ReputationOk          int    `json:"reputation_ok"`
	ReputationTotal       int    `json:"reputation_total"`
	FailedNames           string `json:"failed_names"`
	ReputationFailedNames string `json:"reputation_failed_names"`
	// Deliberately outside all score fields. One
	// unauthenticated destination is a hard provider failure even when the
	// remaining sampled destinations make the percentage look healthy.
	TlsAuthenticationFailure bool `json:"tls_authentication_failure"`
	// The count and names of the loads whose tunnel was gone
	// for their last attempt and could not be re-created in time: neither
	// passed nor failed, and already out of ok/total, so the server can tell
	// a short run from a thin one and never counts them against the exit.
	NotMeasuredCount int    `json:"not_measured_count"`
	NotMeasuredNames string `json:"not_measured_names"`
	// The passed and failed destinations loaded as
	// canaries from a place they are marked incompatible with: unscored, out
	// of every count, and what the server reads to unmark a place once a site
	// works there again (GEOMAP §11.4).
	CanaryPassedNames string `json:"canary_passed_names"`
	CanaryFailedNames string `json:"canary_failed_names"`
	// The classes too thin, for the provider's place, to
	// fill their sample: the pool's signal, comma-joined in class order.
	ShortClasses string `json:"short_classes"`
}

// Records one egress-health run for one provider.
//
// Until this existed the run was a log line and nothing else, so the one
// signal that says whether a provider carries traffic at all rolled off with
// the container logs.
//
// A server with no health endpoint (404) answers egresshealth.ErrUnsupported,
// which is a clean skip rather than a failure: the prober keeps working
// against a deployment that has not shipped this endpoint yet, it just records
// no health. This mirrors the bandwidth path exactly.
//
// The run is submitted over the loads it measured: a run whose tunnel died
// and could not be re-created for some of its loads is still a measurement of
// the rest, and those loads are named apart. A run that measured nothing is
// not submitted at all -- the prober holds it back (see prober) -- because
// 0/0 is not evidence of anything.
func (self *Client) SubmitEgressHealth(
	ctx context.Context,
	providerClientId string,
	res *egresshealth.Result,
) error {
	if res == nil {
		// nothing measured. Submitting a zero here would be indistinguishable
		// from a total blackhole, which is a false accusation against a
		// provider whose check simply did not run.
		return nil
	}

	classResults := map[string]egressHealthClassBody{}
	for class, summary := range res.ByClass {
		classResults[string(class)] = egressHealthClassBody{Ok: summary.Ok, Total: summary.Total}
	}
	shortClasses := make([]string, 0, len(res.ShortClasses))
	for _, class := range res.ShortClasses {
		shortClasses = append(shortClasses, string(class))
	}

	buf, err := json.Marshal(submitEgressHealthBody{
		ClientId:                 providerClientId,
		OkCount:                  res.OkCount,
		TotalCount:               res.Total,
		ClassResults:             classResults,
		TlsAuthenticationFailure: res.TlsAuthenticationFailure,
		NotMeasuredCount:         res.NotMeasured,
		// Bounded like probe_failure, and for the same reason: a run with
		// many failures names dozens of destinations under
		// -egress-health-all, and a submission the server rejects for length
		// is a health signal dropped silently, since the prober submits these
		// fire-and-forget with deduplicated error logging. Cut on element
		// boundaries with a dropped count -- see truncateNameList and
		// MaxNameListLen.
		FailedNames:       truncateNameList(res.FailedNames(), MaxNameListLen),
		NotMeasuredNames:  truncateNameList(res.NotMeasuredNames(), MaxNameListLen),
		CanaryPassedNames: truncateNameList(res.CanaryPassedNames(), MaxNameListLen),
		CanaryFailedNames: truncateNameList(res.CanaryFailedNames(), MaxNameListLen),
		ShortClasses:      strings.Join(shortClasses, ","),
	})
	if err != nil {
		return err
	}

	healthUrl := strings.TrimRight(self.ServerUrl, "/") + "/network/provider-egress-health"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, healthUrl, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-UR-Operator-Secret", self.OperatorSecret)

	resp, err := self.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return egresshealth.ErrUnsupported
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%w: status %d: %s", ErrRejected, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}
