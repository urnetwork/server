package controller

// A refusal an upgrade lifts reaches the URnetwork apps as the JSON body with
// upgrade_required, which they prompt on, and an agent as x402 payment terms
// (X402QuotesClient). Pure: no database.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// A request to auth-client with the client info header of deviceType, or
// with no client info for "".
func newX402AppRefusalRequest(deviceType string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/network/auth-client", nil)
	if deviceType != "" {
		request.Header.Set(
			connect.ClientInfoHeader,
			connect.ClientInfo{Version: 1, DeviceType: deviceType, AppVersion: "2026.11.7"}.Json(),
		)
	}
	return request
}

func TestX402QuotesAgentsAndNotTheApps(t *testing.T) {
	for _, deviceType := range []string{"android", "ios", "macos", "windows", "linux", "web"} {
		if X402QuotesClient(newX402AppRefusalRequest(deviceType).Header) {
			t.Errorf("the %s app is quoted x402 terms", deviceType)
		}
	}
	for _, deviceType := range []string{"", "cli", "server", "unknown"} {
		if !X402QuotesClient(newX402AppRefusalRequest(deviceType).Header) {
			t.Errorf("an agent with client info device type %q is not quoted x402 terms", deviceType)
		}
	}
	// a header that does not parse is no app's
	malformed := http.Header{}
	malformed.Set(connect.ClientInfoHeader, `{"v":1,"device_type":"ios","device_type":"ios"}`)
	if !X402QuotesClient(malformed) {
		t.Error("a malformed client info header is not quoted x402 terms")
	}
}

// x402 has been on in production since 2026-10-27, and auth-client answered
// every upgrade_required refusal with a 402 and payment terms. The apps'
// request layer reads a 402 as a failed request, so an app over the free
// tier's concurrent client limit showed the raw terms or a generic error,
// never its upgrade prompt. An app now gets nothing written here, so the
// handler writes the JSON body; an agent is still quoted.
func TestWriteX402UpgradeRequiredAnswersTheAppsWithTheJsonRefusal(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "x402.example")
	x402ErrorTextConfig(t, "https://facilitator.x402.example")
	message := "Your plan's concurrent client limit is reached. Upgrade to UR Pro to connect more clients."
	upgrade := &model.AuthNetworkClientResult{
		Error: &model.AuthNetworkClientError{
			ClientLimitExceeded: true,
			UpgradeRequired:     true,
			Message:             message,
		},
	}

	for _, deviceType := range []string{"android", "ios", "macos"} {
		w := httptest.NewRecorder()
		if WriteX402UpgradeRequired(w, newX402AppRefusalRequest(deviceType), upgrade) {
			t.Fatalf("the %s app was answered with x402 terms", deviceType)
		}
		if w.Body.Len() != 0 {
			t.Fatalf("the %s app's answer was written here: %d %q", deviceType, w.Code, w.Body.String())
		}
	}

	w := httptest.NewRecorder()
	if !WriteX402UpgradeRequired(w, newX402AppRefusalRequest(""), upgrade) {
		t.Fatal("an agent was not answered with x402 terms")
	}
	connect.AssertEqual(t, w.Code, http.StatusPaymentRequired)
	var paymentRequired X402PaymentRequired
	if err := json.Unmarshal(w.Body.Bytes(), &paymentRequired); err != nil {
		t.Fatalf("the agent's 402 body is not payment terms: %v", err)
	}
	connect.AssertEqual(t, paymentRequired.X402Version, X402Version)
	connect.AssertEqual(t, paymentRequired.Error, message)
	connect.AssertEqual(t, len(paymentRequired.Accepts), 1)

	// a refusal no upgrade lifts is quoted to no one
	noUpgrade := &model.AuthNetworkClientResult{
		Error: &model.AuthNetworkClientError{
			ClientLimitExceeded: true,
			Message:             "Your plan's concurrent client limit is reached. Remove or disconnect another client to connect this one.",
		},
	}
	w = httptest.NewRecorder()
	if WriteX402UpgradeRequired(w, newX402AppRefusalRequest(""), noUpgrade) {
		t.Fatal("a refusal no upgrade lifts was answered with x402 terms")
	}
	connect.AssertEqual(t, w.Body.Len(), 0)
}
