package egresshealth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// The warm-up: the operator's /ip echo that opens every run and every
// blackhole check, and the parsing of its answer into the exit address.

// Where the operator's public /ip surface answers: GET
// /my-ip-info on the api host (GEOMAP §3.3). It is unauthenticated and answers
// with the address the platform saw the request come from, which through a
// provider tunnel is that provider's exit.
const IpEchoPath = "/my-ip-info"

// Caps the warm-up's read. The answer is one small JSON
// document -- the address and its GeoLite2 place, a few hundred bytes -- and
// it has to be read whole to parse, which is why it is not held to
// MaxBodyBytes like a load.
const maxIpEchoBytes = 4 * 1024

// The part of the operator's /ip answer the prober reads:
//
//	{"info": {"ip": "203.0.113.7", "location": {...}}, "connected_to_network": false}
//
// Only info.ip. The place beside it is the server's own GeoLite2 lookup of the
// same address, which the server repeats when the prober submits the address
// (GEOMAP §11.3); reading it here would make the prober a second place where
// the lookup happens, which is the second source of truth D24 removed.
type ipEchoAnswer struct {
	Info struct {
		Ip string `json:"ip"`
	} `json:"info"`
}

// Reads the exit address out of an /ip answer, in canonical form.
// Anything that does not carry a parseable address in info.ip is an error: a
// captive portal, an interception box or a misrouted request all answer 200
// with a body, and a wrong address is worse than none, since the server would
// place the provider where that address is.
func parseIpEcho(body []byte) (string, error) {
	var answer ipEchoAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return "", fmt.Errorf("the /ip answer is not the expected json (%w): %q", err, truncate(strings.TrimSpace(string(body)), 64))
	}
	ip := net.ParseIP(strings.TrimSpace(answer.Info.Ip))
	if ip == nil {
		return "", fmt.Errorf("the /ip answer carries no address in info.ip (got %q)", truncate(answer.Info.Ip, 64))
	}
	return ip.String(), nil
}

// The first fetch of every run and of every blackhole check: the operator's
// /ip echo, with its own timeout, over the path's first tunnel.
//
// It exists because the tunnel's open returns before any path to the provider
// exists. The first request through a fresh tunnel pays the provider window,
// the contract, the in-tunnel DNS resolution and the TLS handshake, and a
// scored load that has to absorb all that on its own clock is how half the
// fleet came to read dark (GEOMAP §11.2). The warm-up pays it instead, on a
// budget sized for it, so the path is up before any scored load starts its
// clock. It is never scored and never counts as the traffic a check is
// looking for: a provider could carry the one well-known operator host and
// nothing else.
//
// Its answer is the exit address, which is the probed location's only input
// now: the server places it with its own GeoLite2 at ingest.
func (self *run) warmUp(ctx context.Context) {
	client, signal := self.path.path.Current()
	if client != nil && !lost(signal) {
		ctx, stop := bound(ctx, signal)
		defer stop()
		self.warmClient(ctx, client)
	}
}

// Fetches the operator's /ip echo through client -- the run's first tunnel,
// or one the path re-created -- and records what it saw. A re-created tunnel
// is as cold as the first was, and its cold start is the warm-up's to pay, not
// the next scored load's.
func (self *run) warmClient(ctx context.Context, client *http.Client) {
	if self.opts.IpEchoUrl == "" {
		return
	}
	fetchExitIp := func() (string, error) {
		ctx, cancel := context.WithTimeout(ctx, self.opts.ipEchoTimeout())
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, self.opts.IpEchoUrl, nil)
		if err != nil {
			return "", &echoStageError{stage: "request_build", err: err}
		}
		// The profile like every request, with the navigation's Accept
		// replaced: this one asks an api for json.
		applyHeaders(req, self.profile, map[string]string{"Accept": "application/json"})

		resp, err := client.Do(req)
		if err != nil {
			return "", &echoStageError{stage: echoRequestStage(err), err: err}
		}
		defer resp.Body.Close()

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxIpEchoBytes))
		if resp.StatusCode != http.StatusOK {
			return "", &echoStageError{stage: "response_status", err: fmt.Errorf("the /ip echo answered status %d", resp.StatusCode)}
		}
		if readErr != nil {
			return "", &echoStageError{stage: "response_body", err: readErr}
		}
		ip, err := parseIpEcho(body)
		if err != nil {
			return "", &echoStageError{stage: "response_schema", err: err}
		}
		return ip, nil
	}
	exitIp, err := fetchExitIp()
	self.exit.record(exitIp, self.opts.now(), err)
}
