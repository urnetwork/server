// Bounded public api reads for the §22.7 root reproduction and the §22.8
// artifact, evidence and history readers. Only enrolled bare https origins
// are contacted, through the actual typed public routes. Requests carry no
// credentials; redirects, environment proxies, cookies and automatic
// decompression are disabled; headers and bodies are bounded before decoding.
package monitor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
)

// One bounded response. Header values are the single values the reader
// requires; any repeated required header is refused.
type subnetReplayApiResponse struct {
	status        int
	contentType   string
	etag          string
	cacheControl  string
	body          []byte
	contentLength int64
}

// The native client for one family run; the caller closes idle connections.
func newSubnetReplayHttpClient(settings *SubnetReplaySettings) (*http.Client, *http.Transport) {
	dialer := &net.Dialer{Timeout: settings.ApiReadTimeout}
	transport := &http.Transport{
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    settings.ApiReadTimeout,
		ResponseHeaderTimeout:  settings.ApiReadTimeout,
		DisableCompression:     true,
		MaxResponseHeaderBytes: int64(settings.ApiMaximumHeaderBytes),
		MaxIdleConns:           2 * settings.ApiMaximumConnectionsPerOrigin,
		MaxIdleConnsPerHost:    settings.ApiMaximumConnectionsPerOrigin,
		MaxConnsPerHost:        settings.ApiMaximumConnectionsPerOrigin,
		IdleConnTimeout:        settings.ApiReadTimeout,
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       settings.ApiReadTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, transport
}

// The family's api client: an injected synthetic seam when present, else the
// native client. A synthetic source without the seam never reaches a socket.
func subnetReplayApiClient(env *probeEnv, settings *SubnetReplaySettings) (grafanaHTTPClient, func()) {
	if env.subnetArtifactClient != nil {
		return env.subnetArtifactClient, func() {}
	}
	client, transport := newSubnetReplayHttpClient(settings)
	return client, transport.CloseIdleConnections
}

// Reads one public object with its own deadline under the run owner. A
// canceled run returns the context error; every other failure is a gap.
func readSubnetReplayApi(ctx context.Context, client grafanaHTTPClient, origin string, path string, query url.Values, settings *SubnetReplaySettings) (response subnetReplayApiResponse, resultErr error) {
	select {
	case <-ctx.Done():
		return response, ctx.Err()
	default:
	}
	readCtx, cancel := context.WithTimeout(ctx, settings.ApiReadTimeout)
	defer cancel()
	defer func() {
		select {
		case <-ctx.Done():
			response, resultErr = subnetReplayApiResponse{}, errors.Join(ctx.Err(), resultErr)
		default:
		}
	}()
	endpoint := origin + path
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(readCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return response, newSubnetReplayUnknown("api-request-invalid", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Cache-Control", "no-cache, no-store")
	answer, err := doScopedGrafanaRequest(client, request)
	if answer == nil || answer.Body == nil {
		return response, newSubnetReplayUnknown("api-unavailable", err)
	}
	if err != nil {
		return response, newSubnetReplayUnknown("api-unavailable", errors.Join(err, answer.Body.Close()))
	}
	if answer.Uncompressed || answer.Header.Get("Content-Encoding") != "" || 1 < len(answer.Header.Values("Content-Type")) || 1 < len(answer.Header.Values("ETag")) || answer.ContentLength < -1 || int64(settings.ApiMaximumBodyBytes) < answer.ContentLength || answer.Request != nil && (answer.Request.URL == nil || answer.Request.URL.String() != endpoint) {
		return response, newSubnetReplayUnknown("api-response-contract-invalid", answer.Body.Close())
	}
	body, readErr := io.ReadAll(io.LimitReader(answer.Body, int64(settings.ApiMaximumBodyBytes)+1))
	closeErr := answer.Body.Close()
	if readErr != nil || closeErr != nil || readCtx.Err() != nil {
		return response, newSubnetReplayUnknown("api-body-unavailable", errors.Join(readErr, closeErr, readCtx.Err()))
	}
	if settings.ApiMaximumBodyBytes < len(body) || 0 <= answer.ContentLength && answer.ContentLength != int64(len(body)) {
		return response, newSubnetReplayUnknown("api-body-bound-invalid", nil)
	}
	return subnetReplayApiResponse{
		status: answer.StatusCode, contentType: answer.Header.Get("Content-Type"),
		etag: answer.Header.Get("ETag"), cacheControl: answer.Header.Get("Cache-Control"),
		body: body, contentLength: answer.ContentLength,
	}, nil
}
