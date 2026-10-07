package acceptance

import (
	"context"
	"net/http"
	"net/url"
)

// TCP connects to the proxy listener before net/http sends CONNECT. Record only
// whether its response arrived and the numeric status; neither headers nor
// response content belong in the acceptance diagnostic.
func newHTTPConnectTransport(proxyURL *url.URL) *http.Transport {
	return &http.Transport{
		Proxy: func(request *http.Request) (*url.URL, error) {
			if request.URL.Scheme == "https" {
				if trace, ok := request.Context().Value(httpsRequestTraceContextKey{}).(*httpsRequestTrace); ok {
					trace.stateLock.Lock()
					trace.proxyConnectStatus = -1
					trace.stateLock.Unlock()
				}
			}
			return proxyURL, nil
		},
		OnProxyConnectResponse: func(ctx context.Context, _ *url.URL, _ *http.Request, response *http.Response) error {
			if trace, ok := ctx.Value(httpsRequestTraceContextKey{}).(*httpsRequestTrace); ok {
				trace.stateLock.Lock()
				trace.proxyConnectStatus = response.StatusCode
				trace.phase = "proxy_tunnel_established"
				if response.StatusCode != http.StatusOK {
					trace.phase = "proxy_connect_rejected"
				}
				trace.stateLock.Unlock()
			}
			return nil
		},
	}
}
