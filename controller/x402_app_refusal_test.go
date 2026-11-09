package controller

// A refusal an upgrade lifts reaches every caller as the JSON body with
// upgrade_required, which the apps, the browser extensions and ur.io prompt
// on. Only a caller that asks for x402 payment terms, with `X-UR-X402: 1`, is
// answered 402 with them (X402QuotesClient). Pure: no database.
//
// Support inbox 8244 (2026-11-09): a free user of the browser extension saw
// "HTTP error! status: 402" on every connect once the network held the free
// plan's 5 active hosted proxies. The published extensions (Chrome and Edge
// 0.0.3, Firefox 2026.6.25) call auth-client through @urnetwork/sdk-js
// 0.0.1-beta.5, whose request carries only Content-Type and Authorization, and
// which reads any answer that is not 2xx as "HTTP error! status: <code>". The
// server quoted every caller that named no app device type in its client info,
// so it took the extension for an agent.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// The request header that asks for x402 terms, as callers send it. Pinned as
// a literal: it is wire contract that agents copy from the docs.
const x402QuoteRequestHeader = "X-UR-X402"

// The free plan's hosted proxy cap refusal, as auth-client answers it
// (model proxyClientLimitMessage(5, false)).
func x402FreeProxyCapRefusal() *model.AuthNetworkClientResult {
	return &model.AuthNetworkClientResult{
		Error: &model.AuthNetworkClientError{
			ClientLimitExceeded: true,
			UpgradeRequired:     true,
			Message:             "Your plan's limit of 5 active proxies is reached. Remove a proxy to create another, or upgrade to UR Pro for more.",
		},
	}
}

// A POST to auth-client with exactly `header`, plus the bearer every caller
// sends.
func newX402RefusalRequest(header map[string]string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/network/auth-client", nil)
	request.Header.Set("Authorization", "Bearer x402-refusal-test")
	for name, value := range header {
		request.Header.Set(name, value)
	}
	return request
}

func x402ClientInfoHeader(deviceType string) map[string]string {
	return map[string]string{
		"Content-Type": "application/json",
		connect.ClientInfoHeader: connect.ClientInfo{
			Version:    1,
			DeviceType: deviceType,
			AppVersion: "2026.11.9",
		}.Json(),
	}
}

// Callers that did not ask for x402 terms, by what their requests carry.
func x402UnaskedCallers() []struct {
	name   string
	header map[string]string
} {
	callers := []struct {
		name   string
		header map[string]string
	}{
		{
			// sdk-js 0.0.1-beta.5's authNetworkClient, from a Chromium
			// extension service worker or popup (the browser adds its own
			// Origin and User-Agent)
			name: "the published Chrome/Edge extension",
			header: map[string]string{
				"Content-Type": "application/json",
				"Origin":       "chrome-extension://kpnklgbgjkihebbieiggfeokkddjbkfb",
				"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36",
			},
		},
		{
			name: "the published Firefox extension",
			header: map[string]string{
				"Content-Type": "application/json",
				"User-Agent":   "Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0",
			},
		},
		{
			name: "ur.io",
			header: map[string]string{
				"Content-Type": "application/json",
				"Origin":       "https://ur.io",
			},
		},
		{
			// the Go sdk before the apps sent client info on api calls
			name: "an app build that sends no client info",
			header: map[string]string{
				"Content-Type": "application/json",
				"User-Agent":   "Go-http-client/2.0",
			},
		},
		{
			// an agent following the docs, which has not asked for terms
			name: "curl",
			header: map[string]string{
				"Content-Type": "application/json",
				"User-Agent":   "curl/8.7.1",
			},
		},
		{
			name: "a malformed client info header",
			header: map[string]string{
				connect.ClientInfoHeader: `{"v":1,"device_type":"ios","device_type":"ios"}`,
			},
		},
		{
			name:   "an x402 header that is not 1",
			header: map[string]string{x402QuoteRequestHeader: "0"},
		},
	}
	for _, deviceType := range []string{"android", "ios", "macos", "windows", "linux", "web", "cli", "server", "unknown"} {
		callers = append(callers, struct {
			name   string
			header map[string]string
		}{"client info device type " + deviceType, x402ClientInfoHeader(deviceType)})
	}
	return callers
}

func TestX402QuotesOnlyCallersThatAskForTerms(t *testing.T) {
	for _, caller := range x402UnaskedCallers() {
		if X402QuotesClient(newX402RefusalRequest(caller.header).Header) {
			t.Errorf("%s is quoted x402 terms without asking for them", caller.name)
		}
	}

	for _, value := range []string{"1", " 1 "} {
		asks := newX402RefusalRequest(map[string]string{x402QuoteRequestHeader: value})
		if !X402QuotesClient(asks.Header) {
			t.Errorf("a caller that sends %s: %q is not quoted", x402QuoteRequestHeader, value)
		}
	}
	// the header is the caller's own claim, whatever else it names
	appAsks := x402ClientInfoHeader("ios")
	appAsks[x402QuoteRequestHeader] = "1"
	if !X402QuotesClient(newX402RefusalRequest(appAsks).Header) {
		t.Errorf("a caller that asks is not quoted because its client info names an app")
	}

	// the paid retry: the payment was settled before the auth ran, so a refusal
	// on it never asks for a second payment
	paid := newX402RefusalRequest(map[string]string{
		x402QuoteRequestHeader: "1",
		"X-PAYMENT":            "eyJ4NDAyVmVyc2lvbiI6MX0=",
	})
	if X402QuotesClient(paid.Header) {
		t.Error("a request that already carries a payment is quoted again")
	}
}

// The root cause of 8244: the free plan's hosted proxy cap refused the
// published extension's connect, and the handler answered it 402 with x402
// terms, which the extension showed as "HTTP error! status: 402". Nothing may
// be written here for a caller that did not ask, so the handler writes the JSON
// body with upgrade_required, which every client reads.
func TestWriteX402UpgradeRequiredAnswersCallersThatDidNotAskWithTheJsonRefusal(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "x402.example")
	x402ErrorTextConfig(t, "https://facilitator.x402.example")

	for _, caller := range x402UnaskedCallers() {
		w := httptest.NewRecorder()
		if WriteX402UpgradeRequired(w, newX402RefusalRequest(caller.header), x402FreeProxyCapRefusal()) {
			t.Errorf("%s was answered %d with x402 terms: %s", caller.name, w.Code, w.Body.String())
			continue
		}
		if w.Body.Len() != 0 {
			t.Errorf("%s's answer was written here: %d %q", caller.name, w.Code, w.Body.String())
		}
	}
}

// An agent that asks is quoted the terms for a Pro month, with the refusal's
// message as the terms' error. A refusal no upgrade lifts is quoted to no one,
// and neither is a refusal while x402 is off.
func TestWriteX402UpgradeRequiredQuotesAnAgentThatAsks(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "x402.example")
	x402ErrorTextConfig(t, "https://facilitator.x402.example")
	asks := map[string]string{"Content-Type": "application/json", x402QuoteRequestHeader: "1"}

	refusal := x402FreeProxyCapRefusal()
	w := httptest.NewRecorder()
	if !WriteX402UpgradeRequired(w, newX402RefusalRequest(asks), refusal) {
		t.Fatal("an agent that asked was not answered with x402 terms")
	}
	connect.AssertEqual(t, w.Code, http.StatusPaymentRequired)
	var paymentRequired X402PaymentRequired
	if err := json.Unmarshal(w.Body.Bytes(), &paymentRequired); err != nil {
		t.Fatalf("the agent's 402 body is not payment terms: %v", err)
	}
	connect.AssertEqual(t, paymentRequired.X402Version, X402Version)
	connect.AssertEqual(t, paymentRequired.Error, refusal.Error.Message)
	connect.AssertEqual(t, len(paymentRequired.Accepts), 1)

	noUpgrade := &model.AuthNetworkClientResult{
		Error: &model.AuthNetworkClientError{
			ClientLimitExceeded: true,
			Message:             "Your plan's limit of 1000 active proxies is reached. Remove a proxy to create another.",
		},
	}
	w = httptest.NewRecorder()
	if WriteX402UpgradeRequired(w, newX402RefusalRequest(asks), noUpgrade) {
		t.Fatal("a refusal no upgrade lifts was answered with x402 terms")
	}
	connect.AssertEqual(t, w.Body.Len(), 0)

	SetX402Config(&X402Config{})
	w = httptest.NewRecorder()
	if WriteX402UpgradeRequired(w, newX402RefusalRequest(asks), x402FreeProxyCapRefusal()) {
		t.Fatal("a refusal was answered with x402 terms while x402 is off")
	}
	connect.AssertEqual(t, w.Body.Len(), 0)
}
