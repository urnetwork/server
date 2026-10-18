package controller

// The x402 routes answer a facilitator, grant or session failure with a fixed
// message. A facilitator's failure quotes its response body, a grant's quotes
// its storage error, and a session's quotes the peer address; each stays in
// the server log.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// A live x402 config whose facilitator is the given synthetic endpoint, with
// priced skus.
func x402ErrorTextConfig(t testing.TB, facilitatorUrl string) {
	t.Helper()
	SetX402Config(&X402Config{
		Enabled:     true,
		Facilitator: x402FacilitatorConfig{Url: facilitatorUrl, ApiKey: "synthetic-facilitator-key"},
		PayTo:       map[string]string{"base": "0xsynthetic"},
		Networks:    []string{"base"},
		Asset:       "usdc",
		Skus:        map[string]x402SkuConfig{X402SkuProMonth: {}, X402SkuData1Tib: {}},
	})
	t.Cleanup(func() { SetX402Config(nil) })
	t.Cleanup(model.Testing_SetSkuPrices(5, 600*model.Gib, []model.ProDataCodeSku{{Data: model.Tib, PriceUsd: 10}}))
}

// A facilitator that verifies and settles every payment, except that the
// failing path answers 500 with private detail in its body.
func newX402ErrorTextFacilitator(t testing.TB, failing string) string {
	t.Helper()
	facilitator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case failing:
			http.Error(w, "synthetic-facilitator-detail", http.StatusInternalServerError)
		case "/verify":
			w.Write([]byte(`{"isValid":true}`))
		default:
			w.Write([]byte(`{"success":true,"transaction":"0x5e771e","network":"base"}`))
		}
	}))
	t.Cleanup(facilitator.Close)
	return facilitator.URL
}

// A signed-in request presenting a payment, from a new network.
func newX402ErrorTextRequest(tb testing.TB, target string, body string) *http.Request {
	tb.Helper()
	networkId, userId := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(tb.Context(), networkId, "x402errortext", userId)
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader([]byte(body)))
	request.RemoteAddr = "192.0.2.53:4500"
	request.Header.Set("Authorization", "Bearer "+session.NewByJwt(networkId, userId, "x402errortext", false, false).Testing_Sign())
	request.Header.Set(X402PaymentHeader, base64.StdEncoding.EncodeToString([]byte(`{"network":"base"}`)))
	return request
}

func requireX402Answer(tb testing.TB, w *httptest.ResponseRecorder, status int, message string) {
	tb.Helper()
	if strings.Contains(w.Body.String(), "synthetic") || w.Code != status || w.Body.String() != message+"\n" {
		tb.Fatalf("want %d %q, got %d %q", status, message, w.Code, w.Body.String())
	}
}

// The facilitator's verification fails. Its status and body were appended to
// the purchase's 502.
func TestX402PurchaseHidesFacilitatorVerifyFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		x402ErrorTextConfig(tb, newX402ErrorTextFacilitator(tb, "/verify"))
		w := httptest.NewRecorder()
		X402PurchaseHandler(w, newX402ErrorTextRequest(tb, "/x402/purchase", `{"sku_id":"data_1tib"}`))
		requireX402Answer(tb, w, http.StatusBadGateway, "x402 verify failed")
	})
}

// The facilitator verifies the payment and its settlement fails.
func TestX402PurchaseHidesFacilitatorSettleFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		x402ErrorTextConfig(tb, newX402ErrorTextFacilitator(tb, "/settle"))
		w := httptest.NewRecorder()
		X402PurchaseHandler(w, newX402ErrorTextRequest(tb, "/x402/purchase", `{"sku_id":"data_1tib"}`))
		requireX402Answer(tb, w, http.StatusBadGateway, "x402 settle failed")
	})
}

// The payment settles and its grant fails; the grant's error was appended to
// the purchase's 502. The reconciler holds the cause.
func TestX402PurchaseHidesGrantFailure(t *testing.T) {
	_, grants := installX402SettledNotGrantedFakes(t)
	grants.err = errors.New("synthetic-grant-detail")
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		x402ErrorTextConfig(tb, newX402ErrorTextFacilitator(tb, ""))
		w := httptest.NewRecorder()
		X402PurchaseHandler(w, newX402ErrorTextRequest(tb, "/x402/purchase", `{"sku_id":"data_1tib"}`))
		requireX402Answer(tb, w, http.StatusBadGateway, "x402 grant failed after settlement")
	})
}

// The inline Pro upgrade on a gated request answers a facilitator failure the
// same way.
func TestX402InlineUpgradeHidesFacilitatorFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		x402ErrorTextConfig(tb, newX402ErrorTextFacilitator(tb, "/verify"))
		w := httptest.NewRecorder()
		if X402SettleInlineUpgrade(w, newX402ErrorTextRequest(tb, "/network/auth-client", `{}`)) {
			tb.Fatal("a failed upgrade continued the request")
		}
		requireX402Answer(tb, w, http.StatusBadGateway, "x402 verify failed")
	})
}

// A peer address that does not parse makes no session; the session's error,
// which quotes the address, was the 500 body of both x402 entry points.
func TestX402HandlersHideSessionFailure(t *testing.T) {
	x402ErrorTextConfig(t, "https://facilitator.example")
	for _, serve := range []func(http.ResponseWriter, *http.Request){
		X402PurchaseHandler,
		func(w http.ResponseWriter, r *http.Request) { X402SettleInlineUpgrade(w, r) },
	} {
		request := httptest.NewRequest(http.MethodPost, "/x402/purchase", bytes.NewReader([]byte(`{"sku_id":"pro_1month"}`)))
		request.RemoteAddr = "synthetic-peer"
		request.Header.Set(X402PaymentHeader, "synthetic-payment")
		w := httptest.NewRecorder()
		serve(w, request)
		requireX402Answer(t, w, http.StatusInternalServerError, router.UnexpectedErrorMessage)
	}
}
