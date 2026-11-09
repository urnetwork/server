package controller

// A sidecar limit's refusal over HTTP (DESIGN.md 20.7, §22.5's "402 and 429"): the router's
// answer to model.QuotaRefusalError, and WriteX402QuotaRefusal's terms with x402 on, off
// and unable to quote. No database: these run anywhere.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// The router's answer to a refusal raised by a handler.
func testingQuotaRefusalAnswer(t *testing.T, refusal *sidecar.QuotaRefusal) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	router.RaiseHttpError(model.NewQuotaRefusalError(refusal), w)
	return w
}

func testingDecodeQuotaRefusal(t *testing.T, w *httptest.ResponseRecorder) *sidecar.QuotaRefusal {
	t.Helper()
	refusal := &sidecar.QuotaRefusal{}
	if err := json.Unmarshal(w.Body.Bytes(), refusal); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return refusal
}

// A window that resets: 429 with Retry-After, whole seconds rounded up, and the refusal.
func TestQuotaRefusalAnswers429WithRetryAfter(t *testing.T) {
	refusal := &sidecar.QuotaRefusal{LimitKey: "evs_drops_per_day", Limit: 20, Used: 20, RetryAfterMs: 1500, Upgrade: true}
	w := testingQuotaRefusalAnswer(t, refusal)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "2" {
		t.Fatalf("status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if got := testingDecodeQuotaRefusal(t, w); !reflect.DeepEqual(got, refusal) {
		t.Errorf("body %+v, want %+v", got, refusal)
	}
}

// A purchase lifts it: 402 with the refusal, and no Retry-After.
func TestQuotaRefusalAnswers402(t *testing.T) {
	refusal := &sidecar.QuotaRefusal{LimitKey: "sidecar_clients", Limit: 20, Used: 20, Upgrade: true}
	w := testingQuotaRefusalAnswer(t, refusal)
	if w.Code != http.StatusPaymentRequired || w.Header().Get("Retry-After") != "" {
		t.Fatalf("status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if got := testingDecodeQuotaRefusal(t, w); !reflect.DeepEqual(got, refusal) {
		t.Errorf("body %+v, want %+v", got, refusal)
	}
}

// Neither: RESOURCE_EXHAUSTED over HTTP, 429 with the refusal and no Retry-After
// (plan-QUOTA-small-readings #13).
func TestQuotaRefusalAnswersResourceExhausted(t *testing.T) {
	refusal := &sidecar.QuotaRefusal{LimitKey: "directory_target_pending", Limit: 100, Used: 100}
	w := testingQuotaRefusalAnswer(t, refusal)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "" {
		t.Fatalf("status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
	if got := testingDecodeQuotaRefusal(t, w); !reflect.DeepEqual(got, refusal) {
		t.Errorf("body %+v, want %+v", got, refusal)
	}
}

// With x402 on, a Community network's refusal that a Pro month lifts is quoted: 402 with
// x402's terms and the refusal's fields beside them.
func TestWriteX402QuotaRefusalQuotesAProMonth(t *testing.T) {
	x402CdpTestPrices(t)
	t.Setenv("WARP_DOMAIN", "x402.example")
	t.Cleanup(model.Testing_SetSidecarLimits(model.NetworkTierPro, map[string]int64{"sidecar_clients": 200}))
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	SetX402Config(x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base")))
	t.Cleanup(func() { SetX402Config(nil) })

	refusal := &sidecar.QuotaRefusal{LimitKey: "sidecar_clients", Limit: 20, Used: 20, Upgrade: true}
	w := httptest.NewRecorder()
	if !WriteX402QuotaRefusal(w, "/network/enroll", model.NetworkTierFree, refusal) {
		t.Fatal("no terms were written")
	}
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d", w.Code)
	}
	body := map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	accepts, _ := body["accepts"].([]any)
	if body["x402Version"] != float64(X402Version) || len(accepts) != 1 {
		t.Fatalf("terms %v", body)
	}
	accept, _ := accepts[0].(map[string]any)
	if accept["resource"] != "https://api.x402.example/network/enroll" || accept["maxAmountRequired"] != "5000000" {
		t.Errorf("term %v", accept)
	}
	if got := testingDecodeQuotaRefusal(t, w); !reflect.DeepEqual(got, refusal) {
		t.Errorf("refusal %+v, want %+v", got, refusal)
	}
}

// Every refusal x402 cannot sell a way past writes nothing, and the caller answers the
// plain body: x402 off; a Pro network, which only Team lifts; a refusal Pro's own limit
// does not lift; no upgrade; a window that resets; and no Pro price to quote.
func TestWriteX402QuotaRefusalFallsBack(t *testing.T) {
	t.Cleanup(model.Testing_SetSidecarLimits(model.NetworkTierPro, map[string]int64{
		"sidecar_clients":   200,
		"acl_approvers_two": 0,
	}))
	lifted := &sidecar.QuotaRefusal{LimitKey: "sidecar_clients", Limit: 20, Used: 20, Upgrade: true}

	// x402 off
	SetX402Config(nil)
	w := httptest.NewRecorder()
	if WriteX402QuotaRefusal(w, "/network/enroll", model.NetworkTierFree, lifted) || w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("x402 off wrote %d %q", w.Code, w.Body.String())
	}

	t.Setenv("WARP_DOMAIN", "x402.example")
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	SetX402Config(x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base")))
	t.Cleanup(func() { SetX402Config(nil) })

	// no Pro price: cannot quote
	t.Cleanup(model.Testing_SetSkuPrices(0, 0, nil))
	w = httptest.NewRecorder()
	if WriteX402QuotaRefusal(w, "/network/enroll", model.NetworkTierFree, lifted) || w.Body.Len() != 0 {
		t.Fatal("quoted with no price")
	}
	x402CdpTestPrices(t)

	for _, c := range []struct {
		tier    model.NetworkTier
		refusal *sidecar.QuotaRefusal
	}{
		{tier: model.NetworkTierPro, refusal: lifted},
		{tier: model.NetworkTierFree, refusal: &sidecar.QuotaRefusal{LimitKey: "acl_approvers_two", Limit: 0, Used: 0, Upgrade: true}},
		{tier: model.NetworkTierFree, refusal: &sidecar.QuotaRefusal{LimitKey: "sidecar_clients", Limit: 20, Used: 20}},
		{tier: model.NetworkTierFree, refusal: &sidecar.QuotaRefusal{LimitKey: "sidecar_clients", Limit: 20, Used: 20, RetryAfterMs: 1000, Upgrade: true}},
		{tier: model.NetworkTierFree, refusal: &sidecar.QuotaRefusal{LimitKey: "directory_target_pending", Limit: 100, Used: 100, Upgrade: true}},
	} {
		w := httptest.NewRecorder()
		if WriteX402QuotaRefusal(w, "/network/enroll", c.tier, c.refusal) || w.Body.Len() != 0 {
			t.Errorf("%s %+v: terms written", c.tier, c.refusal)
		}
	}
}
