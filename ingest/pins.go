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

// The certificate pins the server observed, fetched per pass.

// Reports that the server did not answer the pin endpoint
// with a usable set.
//
// There is deliberately no "the server does not implement this endpoint" error
// beside it, unlike ErrDueUnsupported and ErrAttemptUnsupported. Those two
// exist so the prober keeps working against an older server by doing its own
// scheduling; the equivalent here would be to go on without the set, and a
// pin the server chose to serve is a pin it wants enforced. The requests are
// issued through the provider under test, so for a pinned host the pin is what
// stops that provider answering with a certificate that is chain-valid but
// issued by someone else. A prober that shrugged off a failed fetch would drop
// every such pin without a word.
//
// No host is required to be pinned any more -- the geolocation sources that
// were are gone, and a host without a pin is verified by ordinary WebPKI (see
// fleetprobe.ValidatePins) -- so an empty set is a valid answer. A failed
// fetch is not.
//
// So a 404 here is an error like any other status. It is worth distinguishing
// in the message -- "this server has not deployed the endpoint" is a different
// thing for an operator to fix than "the endpoint returned 500" -- but not in
// the control flow, because there is no behaviour to branch to.
var ErrPinsUnavailable = errors.New("ingest: could not get the certificate pins from the server")

// One host's observed certificate pin as the server serves
// it: the base64 sha-256 SPKI hash of the leaf certificate and of its issuing
// intermediate, both observed by the server on a direct, WebPKI-validated
// connection with no provider in the path. The name is the endpoint's, which
// served the geolocation sources' pins first; the pins are per host, for
// whatever host the server observes.
type GeolocationPin struct {
	Leaf         string `json:"leaf"`
	Intermediate string `json:"intermediate"`
}

// Fetches the certificate pins the server observed.
//
// The response is a bare object keyed by host,
// `{"api.example": {"leaf": "...", "intermediate": "..."}}`. It carries exactly
// what the server observed: a host it has never successfully observed is
// absent, not present-and-empty. That distinction is preserved here rather
// than smoothed over, and the caller decides what a host with no pin gets
// (fleetprobe: ordinary WebPKI).
//
// Every non-200 is an error, including 404. See ErrPinsUnavailable.
func (self *Client) GeolocationPins(ctx context.Context) (map[string]GeolocationPin, error) {
	pinsUrl := strings.TrimRight(self.ServerUrl, "/") + "/network/geolocation-source-pins"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pinsUrl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-UR-Operator-Secret", self.OperatorSecret)

	resp, err := self.httpClient().Do(req)
	if err != nil {
		// %w, not %s: a caller triaging a startup or shutdown needs
		// errors.Is(err, context.Canceled / DeadlineExceeded) to survive this
		// wrapping, the same way the 401 case below preserves its sentinel.
		return nil, fmt.Errorf("%w: %w", ErrPinsUnavailable, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %w", ErrPinsUnavailable, ErrUnauthorized)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: status 404: this server has not deployed /network/geolocation-source-pins; upgrade it -- the prober will not probe unpinned", ErrPinsUnavailable)
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: status %d: %s", ErrPinsUnavailable, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var pins map[string]GeolocationPin
	if err := json.NewDecoder(resp.Body).Decode(&pins); err != nil {
		return nil, fmt.Errorf("%w: decoding the response: %s", ErrPinsUnavailable, err)
	}
	// A body of `null` decodes into a nil map without error, which would
	// otherwise reach the caller looking like a successful fetch of an empty
	// set. Both are refused upstream (an empty set covers no source host), but
	// returning nil,nil here would make that refusal depend on the caller
	// rather than on this function.
	if pins == nil {
		return nil, fmt.Errorf("%w: the server sent no pin object", ErrPinsUnavailable)
	}
	return pins, nil
}
