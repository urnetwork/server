// The native transport of the lb status probe (lbalt_status.go): per attempt,
// one fresh TCP connection to the exact pinned address in the requested
// family, then plain HTTP/1.1 on 80 or HTTP/1.1 over verified TLS on 443. The
// lb name is the Host and, on 443, the SNI, and is never resolved. No proxy,
// no keepalive, no compression and no redirect followed. Every bound comes
// from the pass's LbAltStatusProbeSettings: the attempt deadline, which the
// runner's command timeout also caps on a native attempt, and the response
// header and body. Errors are fixed text, since net/http's carry the request
// URL and with it the hidden prefix.
package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// warpctl's status error rule (docker.go IsError)
var lbAltStatusErrorPattern = regexp.MustCompile(`^(?i)error(\s|:)`)

// The native adapter: one attempt on a fresh system dial, or the runner's
// test dial, verified against the system roots, within the runner's command
// timeout. It only delegates, so its bounds are observeLbAltStatusWithin's.
func (self *runner) lbAltStatus(ctx context.Context, request LbAltStatusRequest, settings *LbAltStatusProbeSettings) (LbAltStatusObservation, error) {
	dial := self.lbAltStatusDial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return observeLbAltStatusWithin(ctx, self.cfg.commandTimeout, request, settings, dial, nil)
}

// One attempt within a command timeout, when it is positive; a non-positive
// one caps nothing. The transport bounds the attempt by the settings' own
// timeout as well, so the shorter of the two applies: a tuned attempt timeout
// reaches the attempt up to the command timeout. The dial and the roots are
// the transport's seams.
func observeLbAltStatusWithin(
	ctx context.Context,
	commandTimeout time.Duration,
	request LbAltStatusRequest,
	settings *LbAltStatusProbeSettings,
	dial func(context.Context, string, string) (net.Conn, error),
	roots *x509.CertPool,
) (LbAltStatusObservation, error) {
	if 0 < commandTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, commandTimeout)
		defer cancel()
	}
	return observeLbAltStatus(ctx, request, settings, dial, roots)
}

// Test seams supply the dialer and the trust roots, never a different
// protocol: production dials the pin with net.Dialer and verifies against the
// system roots. The dialer receives the pin itself, in the request's family,
// under the request's own context.
//
// The settings bound the whole attempt. Its deadline is the attempt timeout,
// whatever the caller's context allows beyond it, and it covers the connect,
// the TLS handshake on 443, the request and the whole response. Settings that
// bound nothing are refused before the context is read, since a non-positive
// timeout would expire the attempt at its start, which is a configuration
// fact and never a timeout.
func observeLbAltStatus(
	ctx context.Context,
	request LbAltStatusRequest,
	settings *LbAltStatusProbeSettings,
	dial func(context.Context, string, string) (net.Conn, error),
	roots *x509.CertPool,
) (observation LbAltStatusObservation, resultErr error) {
	observation.AttemptId = request.AttemptId
	observation.RequestedTuple = lbAltStatusTuple(request)
	if settings == nil || settings.AttemptTimeout <= 0 || settings.ResponseBodyByteLimit <= 0 {
		observation.Failure = LbAltStatusFailureConfiguration
		return observation, errors.New("lb status probe settings invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, settings.AttemptTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	pin, err := netip.ParseAddrPort(observation.RequestedTuple)
	scheme := ""
	routePrefix := ""
	switch {
	case request.Route == lbAltStatusRouteLb && request.Port == lbAltStatusHttpPort && request.Service == "" && request.Block == "":
		scheme = "http"
		routePrefix = strings.TrimSuffix(request.Path, lbAltStatusLbPath(""))
	case request.Route == lbAltStatusRouteBlock && request.Port == lbAltStatusHttpsPort && validDNSLabel(request.Service) && validDNSLabel(request.Block):
		scheme = "https"
		routePrefix = strings.TrimSuffix(request.Path, lbAltStatusBlockPath("", request.Service, request.Block))
	}
	if err != nil || scheme == "" || request.AttemptId == "" || !publicUdpServerName(request.ServerName) ||
		(request.Family != "ipv4" && request.Family != "ipv6") || (request.Family == "ipv4") != pin.Addr().Is4() ||
		routePrefix == request.Path || !lbAltStatusRoutePrefixValid(routePrefix) {
		observation.Failure = LbAltStatusFailureConfiguration
		return observation, errors.New("lb status request authority invalid")
	}
	network := "tcp4"
	if request.Family == "ipv6" {
		network = "tcp6"
	}
	authority := net.JoinHostPort(request.ServerName, strconv.Itoa(request.Port))
	statusUrl := url.URL{Scheme: scheme, Host: request.ServerName, Path: request.Path}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, statusUrl.String(), nil)
	if err != nil {
		observation.Failure = LbAltStatusFailureConfiguration
		return observation, errors.New("lb status request unavailable")
	}

	// Bookkeeping only: no lock is held across the dial.
	var stateLock sync.Mutex
	dials := 0
	connected := false
	transport := &http.Transport{
		Proxy: nil,
		// net/http dials under a context detached from the request's deadline
		// and cancellation (transport.go getConn), so that a later request
		// could take the connection. None follows here, so the dial runs under
		// the request's own context: the attempt deadline bounds the connect
		// itself, and the connect ends with the attempt.
		DialContext: func(_ context.Context, _ string, address string) (net.Conn, error) {
			admitted := func() bool {
				stateLock.Lock()
				defer stateLock.Unlock()
				dials += 1
				return address == authority && dials == 1
			}()
			if !admitted {
				return nil, errors.New("lb status dial outside its pin")
			}
			connection, err := dial(httpRequest.Context(), network, pin.String())
			if err == nil {
				func() {
					stateLock.Lock()
					defer stateLock.Unlock()
					connected = true
				}()
			}
			return connection, err
		},
		TLSClientConfig:        &tls.Config{ServerName: request.ServerName, RootCAs: roots, MinVersion: tls.VersionTLS12},
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: int64(settings.ResponseHeaderByteLimit),
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		// Route53 counts a redirect healthy, so the probe reads the redirect
		// itself rather than where it leads
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// records the dial evidence, and whether any dial connected
	readConnection := func() bool {
		stateLock.Lock()
		defer stateLock.Unlock()
		observation.Dialed = 0 < dials
		observation.FreshConnection = connected && dials == 1
		return connected
	}
	// A broken exchange keeps its fixed failure, and only cancellation or
	// expiry of the attempt itself replaces the fixed error. Whether a dial
	// connected tells a refused or unrouted connection apart from an exchange
	// that broke after it was made.
	incomplete := func(err error, text string) (LbAltStatusObservation, error) {
		connected := readConnection()
		var certificateErr *tls.CertificateVerificationError
		var timeoutErr net.Error
		switch {
		case errors.As(err, &certificateErr):
			observation.Failure = LbAltStatusFailureTls
		case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeoutErr) && timeoutErr.Timeout():
			observation.Failure = LbAltStatusFailureTimeout
		case connected:
			observation.Failure = LbAltStatusFailureRequest
		case errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT):
			// The monitor host has no route or address in the pin's family.
			// EHOSTUNREACH is not one of these: a TCP connect also reports a
			// router's ICMP host unreachable with it, as for a down edge
			// interface, so it stays a connect failure on the path.
			observation.Failure = LbAltStatusFailureRoute
		default:
			observation.Failure = LbAltStatusFailureConnect
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return observation, ctxErr
		}
		return observation, errors.New(text)
	}

	response, err := client.Do(httpRequest)
	if err != nil {
		return incomplete(err, "lb status request incomplete")
	}
	defer response.Body.Close()
	observation.StatusCode = response.StatusCode
	if request.Port == lbAltStatusHttpsPort {
		observation.TlsVerified = response.TLS != nil && response.TLS.HandshakeComplete && 0 < len(response.TLS.VerifiedChains)
	}
	observation.JsonContentType = publicUdpJsonContentType(response.Header)
	// X-UR-Front-Ready as a fixed domain: one "1" or one "0", or absent
	frontReadyValues := response.Header.Values(lbAltStatusFrontReadyHeader)
	switch {
	case len(frontReadyValues) == 0:
		observation.FrontReadiness = LbAltStatusFrontReadinessAbsent
	case len(frontReadyValues) == 1 && frontReadyValues[0] == "1":
		observation.FrontReadiness = LbAltStatusFrontReadinessReady
	case len(frontReadyValues) == 1 && frontReadyValues[0] == "0":
		observation.FrontReadiness = LbAltStatusFrontReadinessNotReady
	default:
		observation.FrontReadiness = LbAltStatusFrontReadinessInvalid
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(settings.ResponseBodyByteLimit)+1))
	if err != nil {
		return incomplete(err, "lb status response incomplete")
	}
	readConnection()
	if settings.ResponseBodyByteLimit < len(body) {
		observation.ResponseBytes = settings.ResponseBodyByteLimit
		observation.Failure = LbAltStatusFailureBudget
		return observation, nil
	}
	observation.ResponseBytes = len(body)

	// One JSON object, with string values wherever the routes name a field.
	// Only fixed facts leave the body; client_address is never read.
	fields := map[string]json.RawMessage{}
	statusBody := struct {
		Status  *string `json:"status"`
		Front   *string `json:"front"`
		Service *string `json:"service"`
		Block   *string `json:"block"`
	}{}
	if json.Unmarshal(body, &fields) != nil || fields == nil || json.Unmarshal(body, &statusBody) != nil {
		return observation, nil
	}
	observation.BodyJson = true
	switch {
	case statusBody.Status == nil:
		observation.Status = LbAltStatusStateAbsent
	case *statusBody.Status == string(LbAltStatusStateOk):
		observation.Status = LbAltStatusStateOk
	case *statusBody.Status == string(LbAltStatusStateDraining):
		observation.Status = LbAltStatusStateDraining
	case lbAltStatusErrorPattern.MatchString(*statusBody.Status):
		observation.Status = LbAltStatusStateError
	default:
		observation.Status = LbAltStatusStateOther
	}
	observation.LbAltFront = request.Route == lbAltStatusRouteLb && statusBody.Front != nil && *statusBody.Front == lbAltStatusFrontName
	observation.BlockMatched = request.Route == lbAltStatusRouteBlock &&
		statusBody.Service != nil && *statusBody.Service == request.Service &&
		statusBody.Block != nil && *statusBody.Block == request.Block
	return observation, nil
}
