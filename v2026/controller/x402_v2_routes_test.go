package controller

// The two HTTP entry points that settle (POST /x402/purchase and the inline
// Pro upgrade on a gated request) speak x402 v2 to a v2 payment and v1 to a
// v1 one, and answer failures per the x402 HTTP transport. The networks are
// created in the test database; the facilitator, the grants' audit table and
// Stripe are fakes.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A signed-in request to target with the given headers, from a new network.
func newX402V2RouteRequest(tb testing.TB, target string, body string, header map[string]string) *http.Request {
	tb.Helper()
	networkId, userId := server.NewId(), server.NewId()
	// the id's tail: its head is a time prefix shared by ids made together
	id := strings.ReplaceAll(networkId.String(), "-", "")
	networkName := "x402v2route" + strings.ToLower(id[len(id)-12:])
	model.Testing_CreateNetwork(tb.Context(), networkId, networkName, userId)
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	request.RemoteAddr = "192.0.2.54:4500"
	request.Header.Set("Authorization", "Bearer "+session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign())
	for name, value := range header {
		request.Header.Set(name, value)
	}
	return request
}

// A v2 payment for one of our v2 terms, shaped as the official client builds
// it (x402V2TestClientPayment). The fake facilitator does not check the
// signature.
func x402V2RouteTestPayment(tb testing.TB, term X402AcceptV2, resource X402ResourceInfo) string {
	tb.Helper()
	payment := map[string]any{
		"x402Version": 2,
		"resource":    resource,
		"accepted":    term,
		"payload": map[string]any{
			"signature": "0x" + strings.Repeat("ab", 65),
			"authorization": map[string]any{
				"from":        "0x2222222222222222222222222222222222222222",
				"to":          term.PayTo,
				"value":       term.Amount,
				"validAfter":  "0",
				"validBefore": "1794356194",
				"nonce":       "0x" + strings.Repeat("3c", 32),
			},
		},
	}
	encoded, err := json.Marshal(payment)
	if err != nil {
		tb.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

// Both settling entry points answer a v2 payment (PAYMENT-SIGNATURE) with a
// PAYMENT-RESPONSE in v2, a v1 payment (X-PAYMENT) with X-PAYMENT-RESPONSE in
// v1, never both; a refused payment 402 with the terms again; a malformed
// one, or one presented in both headers, 400.
//
// Before this change PAYMENT-SIGNATURE was not read (the request went on
// unpaid), and every failure was a 502.
func TestX402V2RoutesAnswerInTheirVersion(t *testing.T) {
	_, grants := installX402SettledNotGrantedFakes(t)
	newX402StripeFixture(t)
	answers := x402V2TestAnswers(t)
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		x402CdpTestPrices(tb)
		key := newX402CdpTestEd25519Key(tb)
		facilitator := newX402CdpTestFacilitator(tb, key)
		SetX402Config(x402CdpTestConfig(tb, x402CdpTestDocument(facilitator.url, key, "base")))
		tb.Cleanup(func() { SetX402Config(nil) })
		setup := &x402V2TestSetup{facilitator: facilitator, answers: answers, grants: grants}

		transactions := 0
		nextSettlement := func() string {
			transactions += 1
			transaction := "0x" + strings.Repeat(fmt.Sprintf("%02x", 0x70+transactions), 32)
			setup.facilitator.set(func(f *x402CdpTestFacilitator) {
				f.verifyStatus, f.verifyBody = http.StatusOK, `{"isValid":true,"payer":"0x2222222222222222222222222222222222222222"}`
				f.settleStatus, f.settleBody = http.StatusOK, fmt.Sprintf(
					`{"success":true,"transaction":%q,"network":"eip155:8453","payer":"0x2222222222222222222222222222222222222222"}`,
					transaction,
				)
			})
			return transaction
		}

		for _, entry := range []struct {
			name     string
			target   string
			body     string
			skuId    string
			resource string
			serve    func(http.ResponseWriter, *http.Request) bool
		}{
			{"purchase", "/x402/purchase", `{"sku_id":"data_1tib"}`, X402SkuData1Tib, x402PurchaseResource, func(w http.ResponseWriter, r *http.Request) bool {
				X402PurchaseHandler(w, r)
				return true
			}},
			{"inline upgrade", "/network/auth-client", `{}`, X402SkuProMonth, x402AuthClientResource, X402SettleInlineUpgrade},
		} {
			terms, err := X402TermsFor(entry.resource, entry.skuId, "")
			if err != nil {
				tb.Fatal(err)
			}

			// v2
			transaction := nextSettlement()
			paymentV2 := x402V2RouteTestPayment(tb, terms.V2.Accepts[0], terms.V2.Resource)
			w := httptest.NewRecorder()
			if !entry.serve(w, newX402V2RouteRequest(tb, entry.target, entry.body, map[string]string{"PAYMENT-SIGNATURE": paymentV2})) || w.Code != http.StatusOK {
				tb.Fatalf("%s v2: answered %d %q", entry.name, w.Code, w.Body.String())
			}
			settlement := x402V2TestDecodeHeader(tb, w.Header().Get("PAYMENT-RESPONSE"))
			if settlement["success"] != true || settlement["transaction"] != transaction || settlement["network"] != "eip155:8453" {
				tb.Errorf("%s v2 PAYMENT-RESPONSE %v", entry.name, settlement)
			}
			if w.Header().Get("X-PAYMENT-RESPONSE") != "" {
				tb.Errorf("%s v2 also answered X-PAYMENT-RESPONSE", entry.name)
			}
			if setup.grants.granted[transaction] == nil {
				tb.Errorf("%s v2: %s was not granted", entry.name, transaction)
			}

			// v1, on the same route
			transaction = nextSettlement()
			paymentV1, _ := x402CdpTestEvmPayment(tb, "base", x402CdpTestPayToBase, terms.V1.Accepts[0].MaxAmountRequired)
			w = httptest.NewRecorder()
			if !entry.serve(w, newX402V2RouteRequest(tb, entry.target, entry.body, map[string]string{"X-PAYMENT": paymentV1})) || w.Code != http.StatusOK {
				tb.Fatalf("%s v1: answered %d %q", entry.name, w.Code, w.Body.String())
			}
			settlement = x402V2TestDecodeHeader(tb, w.Header().Get("X-PAYMENT-RESPONSE"))
			if settlement["success"] != true || settlement["transaction"] != transaction || settlement["network"] != "base" {
				tb.Errorf("%s v1 X-PAYMENT-RESPONSE %v", entry.name, settlement)
			}
			if w.Header().Get("PAYMENT-RESPONSE") != "" {
				tb.Errorf("%s v1 also answered PAYMENT-RESPONSE", entry.name)
			}

			// refused at verify: 402 with the terms again
			setup.facilitator.set(func(f *x402CdpTestFacilitator) {
				answer := setup.answers["verify_insufficient_funds"]
				f.verifyStatus, f.verifyBody = answer.Status, string(answer.Body)
			})
			w = httptest.NewRecorder()
			if entry.serve(w, newX402V2RouteRequest(tb, entry.target, entry.body, map[string]string{"PAYMENT-SIGNATURE": paymentV2})) && entry.name == "inline upgrade" {
				tb.Errorf("%s: a refused payment continued the request", entry.name)
			}
			if w.Code != http.StatusPaymentRequired {
				tb.Fatalf("%s refused: answered %d %q, want 402", entry.name, w.Code, w.Body.String())
			}
			if terms := x402V2TestDecodeHeader(tb, w.Header().Get("PAYMENT-REQUIRED")); terms["error"] != "insufficient_funds" {
				tb.Errorf("%s refused: terms' error %v", entry.name, terms["error"])
			}

			// malformed, and both headers: 400, and nothing settled
			settlesBefore := len(setup.facilitator.receivedOn("/settle"))
			for name, header := range map[string]map[string]string{
				"malformed":    {"PAYMENT-SIGNATURE": "not base64 at all!"},
				"both headers": {"PAYMENT-SIGNATURE": paymentV2, "X-PAYMENT": paymentV1},
			} {
				w := httptest.NewRecorder()
				entry.serve(w, newX402V2RouteRequest(tb, entry.target, entry.body, header))
				if w.Code != http.StatusBadRequest {
					tb.Errorf("%s %s: answered %d %q, want 400", entry.name, name, w.Code, w.Body.String())
				}
			}
			if settles := len(setup.facilitator.receivedOn("/settle")); settles != settlesBefore {
				tb.Errorf("%s: a malformed payment was settled", entry.name)
			}
		}
	})
}
