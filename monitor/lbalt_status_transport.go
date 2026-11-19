// The native transport of the lb status probe (lbalt_status.go): per attempt,
// one fresh TCP connection to the exact pinned address in the requested
// family, then plain HTTP/1.1 on 80 or HTTP/1.1 over verified TLS on 443. The
// lb name is the Host and, on 443, the SNI, and is never resolved. No proxy,
// no keepalive, no compression and no redirect followed; the response header
// and body are bounded. Errors are fixed text, since net/http's carry the
// request URL and with it the hidden prefix.
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
)

// warpctl's status error rule (docker.go IsError)
var lbAltStatusErrorPattern = regexp.MustCompile(`^(?i)error(\s|:)`)

// The native adapter: one attempt under the attempt timeout, or the runner's
// command timeout when that is shorter.
func (self *runner) lbAltStatus(ctx context.Context, request LbAltStatusRequest, settings *LbAltStatusProbeSettings) (LbAltStatusObservation, error) {
	timeout := settings.AttemptTimeout
	if self.cfg.commandTimeout > 0 && self.cfg.commandTimeout < timeout {
		timeout = self.cfg.commandTimeout
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return observeLbAltStatus(attemptCtx, request, settings, (&net.Dialer{}).DialContext, nil)
}

// Test seams supply the dialer and the trust roots, never a different
// protocol: production dials the pin with net.Dialer and verifies against the
// system roots. The dialer receives the pin itself, in the request's family.
func observeLbAltStatus(
	ctx context.Context,
	request LbAltStatusRequest,
	settings *LbAltStatusProbeSettings,
	dial func(context.Context, string, string) (net.Conn, error),
	roots *x509.CertPool,
) (observation LbAltStatusObservation, resultErr error) {
	observation.AttemptId = request.AttemptId
	observation.RequestedTuple = lbAltStatusTuple(request)
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
		routePrefix == request.Path || !lbAltStatusRoutePrefixValid(routePrefix) ||
		settings == nil || settings.ResponseBodyByteLimit <= 0 {
		observation.Failure = LbAltStatusFailureConfiguration
		return observation, errors.New("lb status request authority invalid")
	}
	network := "tcp4"
	if request.Family == "ipv6" {
		network = "tcp6"
	}
	authority := net.JoinHostPort(request.ServerName, strconv.Itoa(request.Port))

	// Bookkeeping only: no lock is held across the dial.
	var stateLock sync.Mutex
	dials := 0
	connected := false
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
			admitted := func() bool {
				stateLock.Lock()
				defer stateLock.Unlock()
				dials += 1
				return address == authority && dials == 1
			}()
			if !admitted {
				return nil, errors.New("lb status dial outside its pin")
			}
			connection, err := dial(ctx, network, pin.String())
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
	// a broken exchange keeps its fixed failure, and only cancellation or
	// expiry of the attempt itself replaces the fixed error
	incomplete := func(err error, text string) (LbAltStatusObservation, error) {
		observation.Failure = lbAltStatusFailureOf(err, readConnection())
		if ctxErr := ctx.Err(); ctxErr != nil {
			return observation, ctxErr
		}
		return observation, errors.New(text)
	}

	statusUrl := url.URL{Scheme: scheme, Host: request.ServerName, Path: request.Path}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, statusUrl.String(), nil)
	if err != nil {
		observation.Failure = LbAltStatusFailureConfiguration
		return observation, errors.New("lb status request unavailable")
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
	observation.FrontReadiness = lbAltStatusFrontReadinessOf(response.Header)
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

// The fixed failure of an exchange that broke. connected tells a refused or
// unrouted connection apart from an exchange that broke after it was made.
func lbAltStatusFailureOf(err error, connected bool) LbAltStatusFailure {
	var certificateErr *tls.CertificateVerificationError
	var timeoutErr net.Error
	switch {
	case errors.As(err, &certificateErr):
		return LbAltStatusFailureTls
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeoutErr) && timeoutErr.Timeout():
		return LbAltStatusFailureTimeout
	case !connected && publicUdpRouteError(err):
		return LbAltStatusFailureRoute
	case !connected:
		return LbAltStatusFailureConnect
	default:
		return LbAltStatusFailureRequest
	}
}

// X-UR-Front-Ready as a fixed domain: one "1" or one "0", or absent.
func lbAltStatusFrontReadinessOf(header http.Header) LbAltStatusFrontReadiness {
	values := header.Values(lbAltStatusFrontReadyHeader)
	switch {
	case len(values) == 0:
		return LbAltStatusFrontReadinessAbsent
	case len(values) == 1 && values[0] == "1":
		return LbAltStatusFrontReadinessReady
	case len(values) == 1 && values[0] == "0":
		return LbAltStatusFrontReadinessNotReady
	default:
		return LbAltStatusFrontReadinessInvalid
	}
}
