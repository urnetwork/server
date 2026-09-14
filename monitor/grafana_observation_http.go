package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

type grafanaHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Retain the established name for probes whose query crosses Grafana's
// datasource boundary. Both names deliberately express the same narrow Do
// contract so injected deterministic clients remain source-compatible.
type grafanaDatasourceHTTPClient = grafanaHTTPClient

// grafanaObservationHTTPClient keeps public Grafana observations off
// http.DefaultTransport's process-wide connection pool. A stale IPv6 HTTP/2
// connection must not couple otherwise independent signal results. When a
// retryable transport failure occurs, the logically read-only observation is
// replayed once over a fresh IPv4-only transport. Each attempt retains the
// established timeout, so a failure-only path is bounded by twice that timeout
// unless the caller's parent context ends earlier. The dedicated IPv6 signals
// continue to own IPv6 route and certificate visibility.
type grafanaObservationHTTPClient struct {
	primary grafanaHTTPClient
	ipv4    grafanaHTTPClient
}

func newGrafanaObservationHTTPClient(timeout time.Duration) *grafanaObservationHTTPClient {
	primaryTransport := newGrafanaObservationTransport()
	ipv4Transport := newGrafanaObservationTransport()
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	ipv4Transport.DialContext = forceDialNetwork("tcp4", dialer.DialContext)
	return newGrafanaObservationHTTPClientWithTransports(
		timeout,
		primaryTransport,
		ipv4Transport,
	)
}

func newGrafanaObservationHTTPClientWithTransports(
	timeout time.Duration,
	primary http.RoundTripper,
	ipv4 http.RoundTripper,
) *grafanaObservationHTTPClient {
	return newGrafanaObservationHTTPClientWithClients(
		&http.Client{Transport: primary, Timeout: timeout, CheckRedirect: checkGrafanaObservationRedirect},
		&http.Client{Transport: ipv4, Timeout: timeout, CheckRedirect: checkGrafanaObservationRedirect},
	)
}

// Only newly owned clients use this per-request policy. Preserve the standard
// redirect bound without changing injected clients, transports, or resolvers.
func checkGrafanaObservationRedirect(request *http.Request, via []*http.Request) error {
	if err := guardGrafanaObservationRequest(request); err != nil {
		return err
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

func newGrafanaObservationHTTPClientWithClients(
	primary grafanaHTTPClient,
	ipv4 grafanaHTTPClient,
) *grafanaObservationHTTPClient {
	return &grafanaObservationHTTPClient{
		primary: primary,
		ipv4:    ipv4,
	}
}

func newGrafanaObservationTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func forceDialNetwork(network string, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, _ string, address string) (net.Conn, error) {
		return dial(ctx, network, address)
	}
}

func (c *grafanaObservationHTTPClient) Do(request *http.Request) (*http.Response, error) {
	if err := guardGrafanaObservationRequest(request); err != nil {
		return nil, err
	}
	response, err := c.primary.Do(request)
	if err == nil {
		return response, nil
	}
	closeGrafanaErrorResponse(response)
	if hostScopeOnlyError(err) {
		// http.Client wraps redirect refusals in a URL-bearing error. Keep
		// intentional policy recognition, but never return that wrapper.
		return nil, &hostScopeExcludedError{}
	}
	primaryClass, retryable := classifyGrafanaTransportError(request.Context(), err)
	if !retryable || request.Context().Err() != nil {
		return nil, grafanaObservationTransportError{primary: primaryClass}
	}

	ipv4Request, replayErr := replayGrafanaRequest(request, request.Context())
	if replayErr != nil {
		return nil, grafanaObservationTransportError{
			primary: primaryClass,
			ipv4:    "request-replay-unavailable",
		}
	}
	response, err = c.ipv4.Do(ipv4Request)
	if err == nil {
		return response, nil
	}
	closeGrafanaErrorResponse(response)
	if hostScopeOnlyError(err) {
		return nil, &hostScopeExcludedError{}
	}
	ipv4Class, _ := classifyGrafanaTransportError(request.Context(), err)
	return nil, grafanaObservationTransportError{
		primary: primaryClass,
		ipv4:    ipv4Class,
	}
}

func closeGrafanaErrorResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

func replayGrafanaRequest(request *http.Request, ctx context.Context) (*http.Request, error) {
	replayed := request.Clone(ctx)
	if request.Body == nil {
		return replayed, nil
	}
	if request.GetBody == nil {
		return nil, errors.New("request body cannot be replayed")
	}
	body, err := request.GetBody()
	if err != nil {
		return nil, err
	}
	replayed.Body = body
	return replayed, nil
}

func classifyGrafanaTransportError(ctx context.Context, err error) (string, bool) {
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "timeout", false
		}
		return "context-canceled", false
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context-canceled", false
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", true
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "network-unreachable", true
	case errors.Is(err, syscall.ECONNRESET):
		return "connection-reset", true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout", true
	}
	return "transport-error", false
}

type grafanaObservationTransportError struct {
	primary string
	ipv4    string
}

func (e grafanaObservationTransportError) Error() string {
	if e.ipv4 == "" {
		return fmt.Sprintf("Grafana observation transport failed: primary=%s", e.primary)
	}
	return fmt.Sprintf(
		"Grafana observation transport failed: primary=%s ipv4_fallback=%s",
		e.primary,
		e.ipv4,
	)
}
