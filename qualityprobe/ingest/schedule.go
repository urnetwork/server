package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/urnetwork/server/qualityprobe/controlplane"
)

// The due queue and attempt reports, and the bounded string fields they
// send.

// Reports that the server has no due endpoint (404). The
// caller falls back to enumerating providers itself, so the prober still works
// against a server that has not deployed it.
var ErrDueUnsupported = errors.New("ingest: the server does not implement /network/provider-egress-due")

// Reports that the server has no attempt endpoint (404),
// for the same reason as ErrDueUnsupported. Probing continues; only the
// server-side backoff for failing providers is unavailable.
var ErrAttemptUnsupported = errors.New("ingest: the server does not implement /network/provider-egress-attempt")

// Reports that the server rejected the operator secret.
//
// This is deliberately not folded into ErrDueUnsupported. A 401 is a
// misconfigured deployment, not an old server: treating it as "fall back to
// enumeration" would hide the fault while every submission the prober went on
// to make was rejected by the same bad secret.
var ErrUnauthorized = errors.New("ingest: the server rejected the operator secret")

// The width of the server's probe_failure column
// (varchar(64)); controller.RecordProviderEgressProbeAttempt rejects anything
// longer with a 400. A rejected report is a lost report, which puts the
// provider straight back at the head of the due queue -- the starvation the
// endpoint exists to prevent -- so a long class is truncated rather than sent.
const MaxProbeFailureLen = 64

// Bounds each of the two failure-name lists in an
// egress-health submission. Unlike probe_failure the server's column width
// here is unconfirmed, so this is not a mirror of a known limit: it is the
// same defensive posture applied to the same kind of field. A heavy-failure
// run names ~26 destinations (400+ characters), and a submission rejected
// for length is a health signal silently dropped after one deduplicated log
// line, because the prober submits these fire-and-forget.
const MaxNameListLen = 512

// Cuts s to at most max bytes without splitting a rune.
// Truncating on a byte boundary can leave a partial encoding that json
// marshals as U+FFFD; every current caller passes ASCII, which is exactly
// why the failure would be silent when one eventually does not.
func truncateUtf8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	for 0 < maxBytes && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// Cuts a comma-separated list to at most max bytes on an
// element boundary, and appends a count of what it dropped.
//
// Cutting mid-element is worse than cutting fewer elements: a list ending
// "...,kernel-org-mirror" names a destination that does not exist, and is
// indistinguishable from one that does, so a query for providers failing that
// destination silently returns nothing. The dropped count matters for the
// same reason -- a blackholing provider under -egress-health-all names ~131
// destinations in ~1.4 KB, so most of the list is dropped, and a reader must
// be able to tell a short list from a truncated one.
func truncateNameList(names []string, maxBytes int) string {
	joined := strings.Join(names, ",")
	if len(joined) <= maxBytes {
		return joined
	}
	kept, used := 0, 0
	for _, name := range names {
		width := len(name)
		if 0 < kept {
			width++ // the separating comma
		}
		// Leave room for the "+N more" marker, which is what tells a reader
		// the list is partial.
		if maxBytes-len("…+999 more") < used+width {
			break
		}
		used += width
		kept++
	}
	if kept == 0 {
		// One name alone exceeds the budget: keep a rune-safe prefix rather
		// than nothing, and let the marker say the rest was dropped.
		return truncateUtf8(joined, maxBytes-len("…+999 more")) + fmt.Sprintf("…+%d more", len(names))
	}
	return strings.Join(names[:kept], ",") + fmt.Sprintf("…+%d more", len(names)-kept)
}

// Resolves the due endpoint: the explicit DueUrl when set, otherwise
// derived from ServerUrl.
func (self *Client) dueUrl() string {
	if self.DueUrl != "" {
		return self.DueUrl
	}
	return strings.TrimRight(self.ServerUrl, "/") + "/network/provider-egress-due"
}

// Returns the configured client, or the shared default when there is none.
func (self *Client) httpClient() *http.Client {
	if self.Http != nil {
		return self.Http
	}
	return defaultHttpClient
}

// One shared transport preserves connection pooling while making the default
// safe for every operator endpoint, including call sites that omit Client.Http.
var defaultHttpClient = controlplane.NewHTTPClient(0)

// One entry of a due list: a provider to probe, and the place
// it is published under. The place decides which destinations the provider's
// sample may draw from (egresshealth.Options.ProviderPlace): a site known not
// to work from there is never loaded, so it never counts against the place's
// exits (GEOMAP §11.3). Both place fields are optional; an entry without them
// excludes nothing.
type DueProvider struct {
	ClientId string `json:"client_id"`
	// The lower-case ISO 3166-1 alpha-2 country.
	CountryCode string `json:"country_code,omitempty"`
	Region      string `json:"region,omitempty"`
}

// Mirrors the server's due results. A server that knows where its
// providers are sends providers, each with its place; one that predates that
// sends client_ids, which read as providers with no place.
type dueResult struct {
	Providers []DueProvider `json:"providers,omitempty"`
	ClientIds []string      `json:"client_ids,omitempty"`
}

// The list the result carries, in the server's order.
func (self dueResult) due() []DueProvider {
	if 0 < len(self.Providers) {
		return self.Providers
	}
	due := make([]DueProvider, 0, len(self.ClientIds))
	for _, clientId := range self.ClientIds {
		due = append(due, DueProvider{ClientId: clientId})
	}
	return due
}

// Asks the server which providers to probe next: those whose stored egress
// location has gone stale, those never probed, and those not attempted within
// the server's backoff, oldest first.
//
// This moves the probe schedule out of the prober's memory and into the
// database, where it survives a restart. limit must be positive: the server
// answers 400 to limit<1 precisely because an empty list is indistinguishable
// from "nothing is due", and it clamps the value to its own maximum.
//
// When ShardCount is above 1 the shard parameters are sent, so the server hands
// this worker only its own slice of the queue. Below that they are omitted
// entirely, which is both the single-prober case and what keeps this working
// against a server that predates them.
func (self *Client) Due(ctx context.Context, limit int) ([]DueProvider, error) {
	if limit < 1 {
		return nil, fmt.Errorf("ingest: due limit must be positive (got %d)", limit)
	}
	// Caught here rather than sent. The server answers 400, but an operator
	// reading an empty result as "nothing is due" is exactly the confusion the
	// limit<1 check above already exists to prevent.
	if 1 < self.ShardCount && (self.ShardIndex < 0 || self.ShardCount <= self.ShardIndex) {
		return nil, fmt.Errorf(
			"ingest: shard index %d is out of range for shard count %d",
			self.ShardIndex, self.ShardCount,
		)
	}

	u, err := url.Parse(self.dueUrl())
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("limit", strconv.Itoa(limit))
	if 1 < self.ShardCount {
		q.Set("shard_count", strconv.Itoa(self.ShardCount))
		q.Set("shard_index", strconv.Itoa(self.ShardIndex))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-UR-Operator-Secret", self.OperatorSecret)

	resp, err := self.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrDueUnsupported
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: status %d: %s", ErrRejected, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var out dueResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.due(), nil
}

// The wire body of an attempt report.
type attemptBody struct {
	ClientId string `json:"client_id"`
	// Omitted on success, otherwise a short failure class.
	ProbeFailure string `json:"probe_failure,omitempty"`
}

// Records that the prober tried this provider, whether or not the
// try produced a location. probeFailure is "" on success, otherwise a short
// class such as tunnel_failed, health_not_run or submit_failed.
//
// Every attempt must be reported, including successes. A provider that can
// never be probed successfully never gets a provider_egress_location row, so
// its observed_at stays NULL and the server's due query -- which sorts NULLs
// first -- hands it back on every poll, forever, starving every healthy
// provider. It fails silently, because the endpoint keeps returning a full and
// plausible batch. Reporting a success here is redundant (the location row
// defers the provider for far longer than the attempt backoff) but harmless,
// and reporting unconditionally means there is no path through the prober that
// forgets.
func (self *Client) ReportAttempt(ctx context.Context, providerClientId string, probeFailure string) error {
	probeFailure = truncateUtf8(probeFailure, MaxProbeFailureLen)

	buf, err := json.Marshal(attemptBody{ClientId: providerClientId, ProbeFailure: probeFailure})
	if err != nil {
		return err
	}

	attemptUrl := strings.TrimRight(self.ServerUrl, "/") + "/network/provider-egress-attempt"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, attemptUrl, bytes.NewReader(buf))
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
		return ErrAttemptUnsupported
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%w: status %d: %s", ErrRejected, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}
