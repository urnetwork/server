package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrCredentialNotReady reports that the server has not minted the prober's
// network client credential yet (404).
//
// This is a WAIT, and it is deliberately its own sentinel rather than a member
// of either family already in this package:
//
//   - It is NOT ErrDueUnsupported/ErrAttemptUnsupported ("this server is older,
//     carry on without the feature"). There is nothing to carry on without: no
//     jwt means no tunnel, so there is no degraded mode to fall back to.
//   - It is NOT ErrCredentialUnavailable ("something went wrong, try again").
//     A 404 is the server working correctly and saying "not yet" -- its
//     bootstrap task runs every 6h, so the first prober to start legitimately
//     arrives before the credential exists. Reporting that as a failure would
//     train an operator to ignore the one line that means a real fault.
//
// The caller polls on this and on the retryable sentinel below, and on nothing
// else.
var ErrCredentialNotReady = errors.New("ingest: the server has not minted the prober credential yet")

// ErrCredentialUnavailable reports that the prober credential could not be
// fetched for any reason that is not a 404 and not a rejected operator secret:
// an unreachable server, a 5xx, an undecodable body, or a 200 that carries no
// usable jwt.
//
// Every one of those is retryable, which is why they share one sentinel: the
// caller's response to all of them is the same backoff it uses for a 404. What
// must NOT share it is the 401 -- see ErrUnauthorized, returned bare below, so
// that a caller which retries on ErrCredentialUnavailable cannot end up
// retrying a wrong secret forever.
var ErrCredentialUnavailable = errors.New("ingest: could not get the prober credential from the server")

// ProberCredential is the server's answer from GET /network/prober-credential:
// the network client jwt the prober authenticates its tunnels with, and the
// client id that jwt belongs to.
//
// The field tags are the fixed contract of that endpoint's result. Note
// by_client_jwt, not by_jwt: the prober's own flag and env var are named
// -by-jwt / UR_PROBER_BY_JWT, so the wire name and the local name differ by one
// word, and getting it wrong yields a 200 that decodes cleanly into an empty
// string. That is why the method refuses an empty jwt below rather than
// returning it -- a silent empty would surface much later as an unparseable
// jwt or a refused tunnel, far from the typo that caused it.
type ProberCredential struct {
	ByClientJwt string `json:"by_client_jwt"`
	ClientId    string `json:"client_id"`
}

// ProberCredential fetches the prober's own network client credential.
//
// It authenticates with X-UR-Operator-Secret, the same header and the same
// secret as Due, Submit, ReportAttempt and GeolocationPins: one secret, one
// mechanism, one thing for a deployment to get right. That is what makes an
// unattended prober possible at all -- the operator secret is already in the
// deployment, so the jwt no longer has to be provisioned by hand and pasted
// into the environment.
//
// Four outcomes, deliberately disjoint under errors.Is:
//
//	200  -> the credential
//	404  -> ErrCredentialNotReady    (wait and ask again)
//	401  -> ErrUnauthorized          (stop; the deployment is misconfigured)
//	else -> ErrCredentialUnavailable (retry)
func (c *Client) ProberCredential(ctx context.Context) (*ProberCredential, error) {
	url := strings.TrimRight(c.ServerUrl, "/") + "/network/prober-credential"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCredentialUnavailable, err)
	}
	req.Header.Set("X-UR-Operator-Secret", c.OperatorSecret)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// %w, not %s, for the same reason GeolocationPins does it: a caller
		// triaging a shutdown needs errors.Is(err, context.Canceled) to
		// survive this wrapping. The startup poll built on this can sit for
		// hours waiting on the bootstrap task, so an interrupt landing
		// mid-request is the ordinary case here rather than a corner one.
		return nil, fmt.Errorf("%w: %w", ErrCredentialUnavailable, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrCredentialNotReady
	case http.StatusUnauthorized:
		// Bare, exactly as Due and ReportAttempt return it. Wrapping it in
		// ErrCredentialUnavailable as well (which is what GeolocationPins
		// does) would be wrong HERE specifically: this error is the one the
		// caller branches on, and a 401 that also matched the retryable
		// sentinel would be retried forever by a caller that happened to test
		// the retryable case first -- the silent misconfiguration
		// ErrUnauthorized exists to make loud.
		return nil, ErrUnauthorized
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: status %d: %s", ErrCredentialUnavailable, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var cred ProberCredential
	if err := json.NewDecoder(resp.Body).Decode(&cred); err != nil {
		return nil, fmt.Errorf("%w: decoding the response: %s", ErrCredentialUnavailable, err)
	}
	// A body of `null`, of `{}`, or one keyed by anything other than
	// by_client_jwt all decode without error into a zero-value struct, so
	// without this check every one of them would read as a successful fetch of
	// an empty jwt. Same lesson as GeolocationPins refusing a nil pin map:
	// decodable is not the same as usable, and the gap has to close here
	// rather than in the caller.
	if strings.TrimSpace(cred.ByClientJwt) == "" {
		return nil, fmt.Errorf("%w: the server answered 200 with no by_client_jwt", ErrCredentialUnavailable)
	}
	return &cred, nil
}
