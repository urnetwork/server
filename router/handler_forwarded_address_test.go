// HTTP-boundary tests verify the client identity handed to route implementations.
package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

// Builds the shape delivered by the main Warp ingress.
func forwardedAddressRequest(headers map[string]string) *http.Request {
	req := httptest.NewRequest("POST", "/auth/network-create", strings.NewReader("{}"))
	req.RemoteAddr = "65.49.70.82:52344"
	for header, value := range headers {
		req.Header.Set(header, value)
	}
	return req
}

// Drives the no-input route wrapper and captures its resolved address.
func runWrapped(req *http.Request) (statusCode int, body string, clientAddress string) {
	recorder := httptest.NewRecorder()
	wrap(
		func(clientSession *session.ClientSession) (string, error) {
			clientAddress = clientSession.ClientAddress
			return "ok", nil
		},
		recorder,
		req,
	)
	return recorder.Code, strings.TrimRight(recorder.Body.String(), "\n"), clientAddress
}

// Drives the input route wrapper and captures its resolved address.
func runWrappedWithInput(req *http.Request) (statusCode int, body string, clientAddress string) {
	recorder := httptest.NewRecorder()
	wrapWithInput(
		RequestBodyFormatter,
		func(input map[string]any, clientSession *session.ClientSession) (string, error) {
			clientAddress = clientSession.ClientAddress
			return "ok", nil
		},
		recorder,
		req,
	)
	return recorder.Code, strings.TrimRight(recorder.Body.String(), "\n"), clientAddress
}

// Reproduces the wrong-ip response at the boundary shared by all API handlers.
func TestUrForwardedAddressReachesEveryHandlerWithoutProxyCIDRs(t *testing.T) {
	runners := []struct {
		name string
		run  func(*http.Request) (int, string, string)
	}{
		{name: "wrap", run: runWrapped},
		{name: "wrapWithInput", run: runWrappedWithInput},
	}
	for _, runner := range runners {
		statusCode, body, clientAddress := runner.run(forwardedAddressRequest(map[string]string{
			"X-UR-Forwarded-For": "173.25.160.143:41001",
		}))
		if statusCode != http.StatusOK {
			t.Errorf("%s answered HTTP %d (%q), want 200", runner.name, statusCode, body)
			continue
		}
		if clientAddress != "173.25.160.143:41001" {
			t.Errorf("%s handed the implementation %q, want the ingress-owned client address", runner.name, clientAddress)
		}
	}
}

// Pins removal of the alternate header and source-port companion.
func TestLegacyForwardedAddressDoesNotReachHandler(t *testing.T) {
	statusCode, body, clientAddress := runWrapped(forwardedAddressRequest(map[string]string{
		"X-Forwarded-For":         "203.0.113.9",
		"X-Forwarded-Source-Port": "41001",
	}))
	if statusCode != http.StatusOK {
		t.Fatalf("legacy headers answered HTTP %d (%q), want 200", statusCode, body)
	}
	if clientAddress != "65.49.70.82:52344" {
		t.Fatalf("legacy headers handed the implementation %q, want the socket peer", clientAddress)
	}
}

// A malformed ingress value remains a degraded attribution signal, not an API outage.
func TestMalformedUrForwardedAddressDoesNotReturn500(t *testing.T) {
	statusCode, body, clientAddress := runWrapped(forwardedAddressRequest(map[string]string{
		"X-UR-Forwarded-For": "not-an-address",
	}))
	if statusCode != http.StatusOK {
		t.Fatalf("malformed UR header answered HTTP %d (%q), want 200", statusCode, body)
	}
	if clientAddress != "65.49.70.82:52344" {
		t.Fatalf("malformed UR header handed the implementation %q, want the socket peer", clientAddress)
	}
}
