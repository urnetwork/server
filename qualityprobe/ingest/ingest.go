// Package ingest submits what the prober measured -- the exit address, the
// egress-health runs, the blackhole checks, bandwidth -- to the operator's
// server, and asks it which providers are due.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Returned when a location submission carries no parseable
// exit address. It is refused before any request is made: the address is the
// whole submission now, and the server rejects one without it, so a doomed
// POST would only be a slower way to the same answer.
var ErrMissingExitIp = errors.New("ingest: the location submission needs an independently observed exit address")

// Returned when the server rejects a submission.
var ErrRejected = errors.New("ingest: server rejected the submission")

// Returned when the observation time is zero. Submit
// never fabricates an "observed now" timestamp: doing so would defeat the
// server's age check and could permanently pin a stale or wrong location,
// since the server's monotonic upsert would then reject later genuine
// probes and its expiry sweep would never remove it.
var ErrMissingProbedAt = errors.New("ingest: the exit observation time is zero")

// Talks to the server's operator endpoints: it posts probed locations,
// reports probe attempts, and asks which providers are due. All three
// authenticate with the same X-UR-Operator-Secret header -- one secret, one
// mechanism, one thing for a deployment to get right.
type Client struct {
	ServerUrl      string
	OperatorSecret string
	Http           *http.Client
	// Overrides the due endpoint derived from ServerUrl. Empty means
	// "<ServerUrl>/network/provider-egress-due".
	DueUrl string
	// Select, with ShardCount, one slice of the due queue for this worker.
	//
	// The queue hands work out but does not claim it: the server's dedupe only
	// bites once an attempt row lands, which is at submit time, minutes after a
	// batch went out. So every worker polling inside that window receives the
	// same rows, and N workers repeat one slice's work instead of dividing it.
	// Sharding on the server's hash of client_id gives each task or standalone
	// prober a disjoint slice; scheduling and host ownership stay with callers.
	//
	// A zero ShardCount (or 1) is the single-prober case: the parameters are
	// omitted from the request entirely, so this is also safe against a server
	// that predates them.
	ShardIndex int
	ShardCount int
}

// The wire body of a location submission, and the arguments an in-process
// operator hands the same controller.
type LocationSubmission struct {
	ClientId string `json:"client_id"`
	// The independently observed public address from an already-sampled HTTPS
	// IP-text response through the provider. The server places it with GeoLite2;
	// DNS answers and connection-control IPs are not substituted for this evidence.
	ExitIp string `json:"exit_ip"`
	// The fields below are what the prober's vendor consensus used to fill.
	// They stay on the wire for one release so a server that still declares
	// them keeps decoding the body, and are always sent empty -- the prober no
	// longer knows any of them, and country_confident false says so.
	CountryCode      string    `json:"country_code"`
	Country          string    `json:"country"`
	Region           string    `json:"region,omitempty"`
	City             string    `json:"city,omitempty"`
	Asn              int       `json:"asn,omitempty"`
	Org              string    `json:"org,omitempty"`
	Hosting          bool      `json:"hosting,omitempty"`
	Proxy            bool      `json:"proxy,omitempty"`
	Mobile           bool      `json:"mobile,omitempty"`
	CountryConfident bool      `json:"country_confident"`
	CityConfident    bool      `json:"city_confident,omitempty"`
	ObservedAt       time.Time `json:"observed_at"`
}

// The client-side half of Submit, shared by every transport: an address that
// does not parse or a zero observation time is refused before any request
// (ErrMissingExitIp, ErrMissingProbedAt) -- the server would reject both, and
// the scheduler would only retry the same doomed submission -- and otherwise
// the canonical address is what is submitted.
func NewLocationSubmission(providerClientId string, exitIp string, observedAt time.Time) (*LocationSubmission, error) {
	ip := net.ParseIP(strings.TrimSpace(exitIp))
	if ip == nil {
		return nil, fmt.Errorf("%w (got %q)", ErrMissingExitIp, exitIp)
	}
	if observedAt.IsZero() {
		return nil, ErrMissingProbedAt
	}
	return &LocationSubmission{
		ClientId:         providerClientId,
		ExitIp:           ip.String(),
		CountryConfident: false,
		ObservedAt:       observedAt,
	}, nil
}

// The error a refused request returns from either transport: ErrRejected with
// the status the operator API answers it with and that answer's message. The
// in-process operator builds the same error from the same controller refusal,
// so a caller's errors.Is and its logs cannot tell the transports apart.
func RejectedError(status int, message string) error {
	return fmt.Errorf("%w: status %d: %s", ErrRejected, status, strings.TrimSpace(message))
}

// Posts one provider's exit address. The body shape is the contract of
// the server's controller.SubmitProviderEgressLocationArgs (POST
// /network/provider-egress-location, X-UR-Operator-Secret header): exit_ip
// and observed_at, with the old consensus fields present and empty for one
// release.
//
// Submit refuses to contact the server at all when the address does not parse
// or the observation time is zero (ErrMissingExitIp, ErrMissingProbedAt): the
// server would reject both, and the scheduler would only retry the same doomed
// POST.
func (self *Client) Submit(ctx context.Context, providerClientId string, exitIp string, observedAt time.Time) error {
	body, err := NewLocationSubmission(providerClientId, exitIp, observedAt)
	if err != nil {
		return err
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	locationUrl := strings.TrimRight(self.ServerUrl, "/") + "/network/provider-egress-location"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, locationUrl, bytes.NewReader(buf))
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
	// 401 is mapped like every other method's, so a wrong -operator-secret
	// reaches the caller as ErrUnauthorized with its remediation advice. This
	// was the one method that did not: against a server without the due
	// endpoint the prober falls back to enumeration, which authenticates with
	// the byJwt rather than the operator secret, so the pass proceeds and a
	// wrong secret surfaced only as a per-provider "status 401" classified
	// submit_failed -- the quiet degradation ErrUnauthorized exists to
	// prevent.
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("%w: %w", ErrRejected, ErrUnauthorized)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return RejectedError(resp.StatusCode, string(msg))
	}
	return nil
}
