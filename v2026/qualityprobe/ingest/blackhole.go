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
	"time"
)

// The blackhole endpoints: which providers are due a check, and the batched
// submission of a pass.

// Reports that the server has no blackhole endpoints
// (404), for the same reason ErrDueUnsupported exists: the prober still works
// against a server that has not deployed them. The sweep is simply skipped, and
// says so once rather than every pass.
var ErrBlackholeUnsupported = errors.New("ingest: the server does not implement the provider-blackhole endpoints")

// Only a due-list HTTP 503 carries this retryable read failure. It does not
// apply to submissions, malformed successful bodies, or other 4xx/5xx errors.
var ErrBlackholeDueUnavailable = errors.New("ingest: blackhole due service temporarily unavailable")

// The width of the server's failure column
// (varchar(64)). The server rejects an oversized value rather than truncating
// it, and a rejected batch is a lost sweep, so truncate before sending.
const MaxBlackholeFailureLen = 64

// Resolves the due endpoint from ServerUrl.
func (self *Client) blackholeDueUrl() string {
	return strings.TrimRight(self.ServerUrl, "/") + "/network/provider-blackhole-due"
}

// Asks which providers to check next: never checked first, then
// least recently checked.
//
// Unlike Due there is no attempt backoff on the server side, by design: a
// provider that failed last hour must be re-checked this hour, because that is
// how it returns to the public list once it recovers.
func (self *Client) BlackholeDue(ctx context.Context, limit int) ([]DueProvider, error) {
	if _, _, err := PrepareDueRequest("blackhole due", limit, self.ShardIndex, self.ShardCount); err != nil {
		return nil, err
	}

	u, err := url.Parse(self.blackholeDueUrl())
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
		return nil, ErrBlackholeUnsupported
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case http.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: %w", ErrRejected, ErrBlackholeDueUnavailable)
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, RejectedError(resp.StatusCode, string(msg))
	}

	var out dueResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.due(), nil
}

// One provider's result, as submitted.
type BlackholeCheck struct {
	ClientId string `json:"client_id"`
	Ok       bool   `json:"ok"`
	Failure  string `json:"failure,omitempty"`
	// Marks a check none of whose loads could be measured: its
	// tunnel was gone and could not be re-created within their attempts. It
	// is not a verdict either way -- OK is false and Failure is
	// "not_measured" only because the fields must say something -- and the
	// server stores it as not measured and only reschedules: it neither counts
	// a failure against the provider nor clears one.
	NotMeasured bool      `json:"not_measured,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

// The wire body of a blackhole pass.
type BlackholeChecksSubmission struct {
	Checks []BlackholeCheck `json:"checks"`
}

// The client-side half of SubmitBlackholeChecks, shared by every transport:
// nil for an empty pass, which sends nothing. The server validates the entire
// batch before writing any of it, so a single malformed entry loses the whole
// pass; everything that can be fixed locally is fixed here rather than sent
// and rejected: a zero CheckedAt is refused (never fabricated: an "as of now"
// timestamp would defeat the server's freshness bound and could pin a stale
// verdict), a failed check must name its class, a passing check carries none,
// and an over-long class is cut to the column width.
func NewBlackholeChecksSubmission(checks []BlackholeCheck) (*BlackholeChecksSubmission, error) {
	if len(checks) == 0 {
		return nil, nil
	}

	body := &BlackholeChecksSubmission{Checks: make([]BlackholeCheck, 0, len(checks))}
	for _, check := range checks {
		if check.CheckedAt.IsZero() {
			return nil, fmt.Errorf("ingest: blackhole check for %s has a zero CheckedAt", check.ClientId)
		}
		if !check.Ok && strings.TrimSpace(check.Failure) == "" {
			// the server rejects this, and a rejected batch is a lost sweep
			return nil, fmt.Errorf("ingest: failed blackhole check for %s names no failure class", check.ClientId)
		}
		if check.Ok {
			check.Failure = ""
			check.NotMeasured = false
		}
		check.Failure = truncateUtf8(check.Failure, MaxBlackholeFailureLen)
		body.Checks = append(body.Checks, check)
	}
	return body, nil
}

// Reports a whole pass in one request.
//
// Batched because a sweep produces hundreds of one-bit answers and a request
// each would spend more on http than on the checks themselves.
//
// The server validates the entire batch before writing any of it, so a single
// malformed entry loses the whole pass. Everything that can be fixed locally is
// fixed here rather than sent and rejected: a zero CheckedAt is refused, and an
// over-long failure class is truncated to the column width.
func (self *Client) SubmitBlackholeChecks(ctx context.Context, checks []BlackholeCheck) error {
	body, err := NewBlackholeChecksSubmission(checks)
	if err != nil || body == nil {
		return err
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}

	checksUrl := strings.TrimRight(self.ServerUrl, "/") + "/network/provider-blackhole-checks"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, checksUrl, bytes.NewReader(buf))
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
		return ErrBlackholeUnsupported
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %w", ErrRejected, ErrUnauthorized)
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return RejectedError(resp.StatusCode, string(msg))
	}
}
