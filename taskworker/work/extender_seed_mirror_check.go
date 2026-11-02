package work

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/controller"
)

// The check of every configured seed mirror (connect/EXTENDER-UPDATE2.md §2.3).
//
// Each pass fetches the bundle from every route of every mirror in the seed
// block -- the mirror's url resolved as usual, and each dial address, dialed
// directly with the url's host as the TLS server name, which is the
// DNS-independent route a client takes -- with the certificate verified under
// the roots a client verifies mirrors with (extenderSeedMirrorRootCAs), and
// verifies the bundle exactly as a client does (connect.VerifyExtenderSeed)
// under the root keys and hosts of `extender.yml`. A client without a dial
// address resolves the url with the device's system resolver, not the
// network's DoH, so the address routes are the ones that matter where the
// seed matters. A route reports up only for a bundle
// that verified and is fresh; its sequence, issue and expiry are reported for
// any bundle that verified, so an expired or lagging mirror shows how far
// behind it is. The comparison with the stored bundle -- stale, rolled back,
// expiring -- is the monitor's (SIGNALS.md §27.1) over these series.
//
// A fetch follows no redirect: a first-party mirror never sends one, and a
// client bounds its own redirects to configured destinations. The body is read
// to at most the envelope cap plus one byte. Nothing a mirror returns reaches a
// log beyond a fixed outcome code; the url and the addresses stay in the
// configuration.

const (
	// one route's whole fetch
	extenderSeedMirrorFetchTimeout = 10 * time.Second
	// one route's connect, TLS handshake and response header
	extenderSeedMirrorStepTimeout = 5 * time.Second
	// routes fetched at once
	extenderSeedMirrorConcurrency = 8
)

// Fetch outcomes before verification. A verification refusal reports the
// refusal's code (connect.ExtenderSeedCode*), and a fresh verified bundle "ok".
const (
	extenderSeedMirrorOutcomeOk          = "ok"
	extenderSeedMirrorOutcomeUnreachable = "unreachable"
	extenderSeedMirrorOutcomeTls         = "tls"
	extenderSeedMirrorOutcomeRedirect    = "redirect"
	extenderSeedMirrorOutcomeHttpStatus  = "http_status"
	extenderSeedMirrorOutcomeTooLarge    = connect.ExtenderSeedCodeTooLarge
	extenderSeedMirrorOutcomeMalformed   = connect.ExtenderSeedCodeMalformed
)

// The verification codes a route can report; any other refusal reports
// malformed, so the outcome label stays a fixed set.
var extenderSeedMirrorVerifyOutcomes = map[string]bool{
	connect.ExtenderSeedCodeMalformed:         true,
	connect.ExtenderSeedCodeTooLarge:          true,
	connect.ExtenderSeedCodeTooMany:           true,
	connect.ExtenderSeedCodeNoRootKeys:        true,
	connect.ExtenderSeedCodeSignature:         true,
	connect.ExtenderSeedCodeForeignNetwork:    true,
	connect.ExtenderSeedCodeRecordInvalid:     true,
	connect.ExtenderSeedCodeGatedRecord:       true,
	connect.ExtenderSeedCodeRevocationInvalid: true,
	connect.ExtenderSeedCodeNotYetValid:       true,
	connect.ExtenderSeedCodeExpired:           true,
}

// The roots a mirror's certificate is verified against: the roots a client
// verifies a mirror with, connect's default TLS roots (the pinned Let's
// Encrypt set, connect/net_tls.go). A mirror whose certificate the system
// roots accept but the pinned set does not is refused by every client, so the
// check refuses it too rather than report it up. A variable so a test can
// trust its own httptest certificate.
var extenderSeedMirrorRootCAs func() *x509.CertPool = extenderSeedMirrorClientRootCAs

// connect's default client roots, or an empty pool when they cannot be built
// (a malformed extra root file): every route then reports tls, never a check
// that trusts more than a client does.
func extenderSeedMirrorClientRootCAs() *x509.CertPool {
	tlsConfig, err := connect.DefaultTlsConfig()
	if err != nil || tlsConfig.RootCAs == nil {
		return x509.NewCertPool()
	}
	return tlsConfig.RootCAs
}

// What one route's check found.
type extenderSeedMirrorResult struct {
	route       *controller.ExtenderSeedMirrorRoute
	outcome     string
	observation *extenderSeedMirrorObservation
}

// checkExtenderSeedMirrors checks every route at now, records each result, and
// returns them in configuration order.
func checkExtenderSeedMirrors(
	ctx context.Context,
	routes []*controller.ExtenderSeedMirrorRoute,
	rootKeys *connect.ExtenderRootKeySet,
	networkHosts []string,
	now time.Time,
) []*extenderSeedMirrorResult {
	results := make([]*extenderSeedMirrorResult, len(routes))
	semaphore := make(chan struct{}, extenderSeedMirrorConcurrency)
	var wait sync.WaitGroup
	for i, route := range routes {
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[i] = &extenderSeedMirrorResult{
					route:       route,
					outcome:     extenderSeedMirrorOutcomeUnreachable,
					observation: &extenderSeedMirrorObservation{observedAt: now},
				}
				return
			}
			results[i] = checkExtenderSeedMirrorRoute(ctx, route, rootKeys, networkHosts, now)
		}()
	}
	wait.Wait()
	for _, result := range results {
		extenderSeedMirrorChecksCounter.WithLabelValues(result.route.Mirror, result.route.Route, result.outcome).Inc()
		extenderSeedObservations.observeMirror(result.route.Mirror, result.route.Route, result.observation)
		if result.outcome != extenderSeedMirrorOutcomeOk {
			glog.V(1).Infof("[extenderseed]mirror %s route %s: %s\n", result.route.Mirror, result.route.Route, result.outcome)
		}
	}
	return results
}

// One route's fetch and verification.
func checkExtenderSeedMirrorRoute(
	ctx context.Context,
	route *controller.ExtenderSeedMirrorRoute,
	rootKeys *connect.ExtenderRootKeySet,
	networkHosts []string,
	now time.Time,
) *extenderSeedMirrorResult {
	result := &extenderSeedMirrorResult{
		route:       route,
		observation: &extenderSeedMirrorObservation{observedAt: now},
	}
	artifact, outcome := fetchExtenderSeedMirrorRoute(ctx, route)
	if outcome != "" {
		result.outcome = outcome
		return result
	}
	envelope, err := connect.ParseExtenderSeedEnvelope(artifact)
	if err != nil {
		result.outcome = extenderSeedMirrorVerifyOutcome(err)
		return result
	}
	bundle, err := connect.VerifyExtenderSeed(envelope, rootKeys, networkHosts, now)
	if err != nil {
		result.outcome = extenderSeedMirrorVerifyOutcome(err)
		if !connect.ExtenderSeedFreshnessError(err) {
			return result
		}
		// stale or future-dated but otherwise valid: report what it is, so
		// the monitor can tell an expired mirror from a broken one
		stale, staleErr := extenderSeedVerifyAtIssue(envelope, rootKeys, networkHosts)
		if staleErr != nil {
			return result
		}
		bundle = stale
	} else {
		result.outcome = extenderSeedMirrorOutcomeOk
		result.observation.up = true
	}
	result.observation.verified = true
	result.observation.sequence = bundle.Sequence
	result.observation.issueTime = bundle.IssueTime
	result.observation.expireTime = bundle.ExpireTime
	return result
}

// The outcome label of a parse or verification refusal.
func extenderSeedMirrorVerifyOutcome(err error) string {
	code := connect.ExtenderSeedErrorCode(err)
	if extenderSeedMirrorVerifyOutcomes[code] {
		return code
	}
	return extenderSeedMirrorOutcomeMalformed
}

// Verifies an envelope as of its own issue time: every check but freshness.
func extenderSeedVerifyAtIssue(
	envelope *protocol.ExtenderSeedEnvelope,
	rootKeys *connect.ExtenderRootKeySet,
	networkHosts []string,
) (*connect.ExtenderSeedBundle, error) {
	body := &protocol.ExtenderSeedBody{}
	if err := proto.Unmarshal(envelope.Body, body); err != nil {
		return nil, err
	}
	return connect.VerifyExtenderSeed(envelope, rootKeys, networkHosts, time.UnixMilli(int64(body.IssuedAtUnixMilli)))
}

// Fetches one route's body, or the fetch outcome that stopped it.
func fetchExtenderSeedMirrorRoute(ctx context.Context, route *controller.ExtenderSeedMirrorRoute) ([]byte, string) {
	dialer := &net.Dialer{Timeout: extenderSeedMirrorStepTimeout}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: extenderSeedMirrorRootCAs()},
		TLSHandshakeTimeout:    extenderSeedMirrorStepTimeout,
		ResponseHeaderTimeout:  extenderSeedMirrorStepTimeout,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 16 * 1024,
	}
	defer transport.CloseIdleConnections()
	if route.DialAddress.IsValid() {
		// the DNS-independent route: the configured address, with the url's
		// host still the server name the certificate must name
		address := route.DialAddress
		transport.DialContext = func(ctx context.Context, network string, hostPort string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(hostPort)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(address.String(), port))
		}
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   extenderSeedMirrorFetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, route.Url.String(), nil)
	if err != nil {
		return nil, extenderSeedMirrorOutcomeUnreachable
	}
	request.Header.Set("Accept", controller.ExtenderSeedContentType)
	response, err := client.Do(request)
	if err != nil {
		return nil, extenderSeedMirrorFetchErrorOutcome(err)
	}
	defer response.Body.Close()
	switch {
	case 300 <= response.StatusCode && response.StatusCode < 400:
		return nil, extenderSeedMirrorOutcomeRedirect
	case response.StatusCode != http.StatusOK:
		return nil, extenderSeedMirrorOutcomeHttpStatus
	}
	artifact, err := io.ReadAll(io.LimitReader(response.Body, connect.ExtenderSeedMaxEnvelopeByteCount+1))
	if err != nil {
		return nil, extenderSeedMirrorOutcomeUnreachable
	}
	if connect.ExtenderSeedMaxEnvelopeByteCount < len(artifact) {
		return nil, extenderSeedMirrorOutcomeTooLarge
	}
	return artifact, ""
}

// A transport error's outcome: a certificate or handshake refusal is tls,
// everything else unreachable.
func extenderSeedMirrorFetchErrorOutcome(err error) string {
	var certificateErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var alert tls.AlertError
	var record tls.RecordHeaderError
	switch {
	case errors.As(err, &certificateErr), errors.As(err, &unknownAuthority), errors.As(err, &hostname),
		errors.As(err, &invalid), errors.As(err, &alert), errors.As(err, &record):
		return extenderSeedMirrorOutcomeTls
	}
	return extenderSeedMirrorOutcomeUnreachable
}
