package controller

// x402 v2 beside v1, and the $1 default client cap.
//
// x402 v2 (x402-foundation/x402 specs/x402-specification-v2.md and
// specs/transports-v2/http.md, 2025-12-09) moved the terms into the
// PAYMENT-REQUIRED header, named networks by CAIP-2 id, renamed the payment
// header to PAYMENT-SIGNATURE and the receipt to PAYMENT-RESPONSE, and has the
// payment name the terms it `accepted`. Until this change the server spoke
// v1 only: a v2 client found no PAYMENT-REQUIRED header and fell back to the
// v1 body, and a v2 payment was refused before the facilitator ("x402 payment
// is for x402 version 2; this server takes version 1").
//
// The fixtures in testdata/x402:
//   - cdp-supported.json: the CDP facilitator's GET /supported example, from
//     its API reference (docs.cdp.coinbase.com api-reference v2 rest-api
//     x402-facilitator get-supported-payment-schemes-and-networks).
//   - cdp-answers.json: the verify and settle answers of that reference and of
//     the v2 spec, set to Base mainnet.
//   - v2-payment-required-data_1tib.json: the v2 terms this server quotes for
//     data_1tib on Base and Solana with x402CdpTestDocument's synthetic pay-to
//     addresses (the golden file of TestX402QuotesV2InThePaymentRequiredHeader).
//   - v2-client-payment-signature-base.txt: the PAYMENT-SIGNATURE header the
//     official Go client (github.com/x402-foundation/x402/go/v2 v2.30.0,
//     x402http.Newx402HTTPClient over an x402 client with an ExactEvmScheme and
//     SpendControls{MaxAmountPerPayment: "$3"}) built for those terms on
//     2026-11-10, with a throwaway key generated for that run. The same client
//     with default spend controls refused the terms: "all payment requirements
//     were rejected by spendControls.maxAmountPerPayment ($1, including USDC)".
//
// Nothing here contacts a facilitator, Stripe or a database.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// x402V2TestFixture reads a testdata/x402 file.
func x402V2TestFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/x402/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// x402V2TestAnswer is one recorded facilitator answer.
type x402V2TestAnswer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func x402V2TestAnswers(t testing.TB) map[string]x402V2TestAnswer {
	t.Helper()
	answers := map[string]x402V2TestAnswer{}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(x402V2TestFixture(t, "cdp-answers.json"), &raw); err != nil {
		t.Fatal(err)
	}
	for name, value := range raw {
		if strings.HasPrefix(name, "_") {
			continue
		}
		answer := x402V2TestAnswer{}
		if err := json.Unmarshal(value, &answer); err != nil {
			t.Fatal(err)
		}
		answers[name] = answer
	}
	return answers
}

// x402V2TestSetup is a live x402 config quoting Base and Solana through a fake
// CDP facilitator that answers /supported with CDP's documented example, and
// fakes for the grants, the reconciliation events and Stripe.
type x402V2TestSetup struct {
	facilitator *x402CdpTestFacilitator
	config      *X402Config
	answers     map[string]x402V2TestAnswer
	events      *x402FakeEventTable
	grants      *x402FakeGrants
	stripe      *x402StripeFixture
}

func newX402V2TestSetup(t *testing.T, networks ...string) *x402V2TestSetup {
	t.Helper()
	if len(networks) == 0 {
		networks = []string{"base", "solana"}
	}
	events, grants := installX402SettledNotGrantedFakes(t)
	stripe := newX402StripeFixture(t)
	x402CdpTestPrices(t)
	t.Setenv("WARP_DOMAIN", "x402.example")
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	answers := x402V2TestAnswers(t)
	facilitator.set(func(f *x402CdpTestFacilitator) {
		f.supportedBody = string(x402V2TestFixture(t, "cdp-supported.json"))
		f.verifyStatus, f.verifyBody = answers["verify_valid"].Status, string(answers["verify_valid"].Body)
		f.settleStatus, f.settleBody = answers["settle_success"].Status, string(answers["settle_success"].Body)
	})
	c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, networks...))
	SetX402Config(c)
	t.Cleanup(func() { SetX402Config(nil) })
	return &x402V2TestSetup{
		facilitator: facilitator,
		config:      c,
		answers:     answers,
		events:      events,
		grants:      grants,
		stripe:      stripe,
	}
}

func (self *x402V2TestSetup) answer(path string, name string) {
	answer := self.answers[name]
	self.facilitator.set(func(f *x402CdpTestFacilitator) {
		switch path {
		case "/verify":
			f.verifyStatus, f.verifyBody = answer.Status, string(answer.Body)
		case "/settle":
			f.settleStatus, f.settleBody = answer.Status, string(answer.Body)
		}
	})
}

// x402V2TestClientPayment is the recorded official-client payment for the
// Base term of data_1tib, and its decoded PaymentPayload.
func x402V2TestClientPayment(t testing.TB) (string, map[string]any) {
	t.Helper()
	header := strings.TrimSpace(string(x402V2TestFixture(t, "v2-client-payment-signature-base.txt")))
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatal(err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return header, decoded
}

// x402V2TestPayment encodes a PaymentPayload object as the header carries it.
func x402V2TestPayment(t testing.TB, payment map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(payment)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

// x402V2TestCopy deep-copies a decoded JSON object.
func x402V2TestCopy(t testing.TB, value map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	copied := map[string]any{}
	if err := json.Unmarshal(encoded, &copied); err != nil {
		t.Fatal(err)
	}
	return copied
}

// x402V2TestDecodeHeader decodes a base64 JSON header into an object.
func x402V2TestDecodeHeader(t testing.TB, header string) map[string]any {
	t.Helper()
	if header == "" {
		t.Fatal("the header is missing")
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		t.Fatalf("the header is not base64: %q", header)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("the header is not base64 json: %v", err)
	}
	return decoded
}

// The CAIP-2 form the reference SDKs check every v2 network against
// (x402-foundation/x402 go/types/v2.go networkV2Pattern,
// typescript/packages/core/src/schemas NetworkSchemaV2).
var x402V2TestCaip2Pattern = regexp.MustCompile(`^[-a-z0-9]{3,8}:[-_a-zA-Z0-9]{1,32}$`)

// requireX402V2PaymentRequired checks a decoded PaymentRequired the way the
// reference clients validate one before they pay (typescript/packages/core
// src/schemas PaymentRequiredV2Schema; go/types/v2.go PaymentRequired).
func requireX402V2PaymentRequired(t testing.TB, paymentRequired map[string]any) {
	t.Helper()
	if paymentRequired["x402Version"] != float64(2) {
		t.Errorf("x402Version = %v, want 2", paymentRequired["x402Version"])
	}
	resource, _ := paymentRequired["resource"].(map[string]any)
	if url, _ := resource["url"].(string); url == "" {
		t.Errorf("resource.url is missing: %v", paymentRequired["resource"])
	}
	accepts, _ := paymentRequired["accepts"].([]any)
	if len(accepts) == 0 {
		t.Fatalf("no accepts: %v", paymentRequired)
	}
	for _, value := range accepts {
		accept, _ := value.(map[string]any)
		for _, field := range []string{"scheme", "network", "amount", "asset", "payTo"} {
			if text, _ := accept[field].(string); text == "" {
				t.Errorf("accept %v has no %s", accept, field)
			}
		}
		if network, _ := accept["network"].(string); !x402V2TestCaip2Pattern.MatchString(network) {
			t.Errorf("accept network %q is not CAIP-2", network)
		}
		if timeout, _ := accept["maxTimeoutSeconds"].(float64); timeout <= 0 {
			t.Errorf("accept %v has no positive maxTimeoutSeconds", accept)
		}
		if _, v1 := accept["maxAmountRequired"]; v1 {
			t.Errorf("a v2 accept carries the v1 maxAmountRequired: %v", accept)
		}
	}
}

// A 402 carries the v2 terms in the PAYMENT-REQUIRED header, as base64 JSON
// of the PaymentRequired (specs/transports-v2/http.md, "Payment Required
// Signaling"), and keeps the v1 terms as its body for v1 clients. The v2 terms
// name networks by CAIP-2 id, quote `amount`, describe the resource once, and
// carry each scheme's extra: Base USDC's EIP-712 domain, and the v2 Solana
// kind's fee payer and transaction versions from /supported. The 402 stays
// out of shared caches.
//
// Before this change the 402 had no PAYMENT-REQUIRED header: v2 clients fell
// back to the v1 body.
func TestX402QuotesV2InThePaymentRequiredHeader(t *testing.T) {
	setup := newX402V2TestSetup(t)

	terms, err := x402TermsForConfig(setup.config, x402PurchaseResource, X402SkuData1Tib, "payment required for data_1tib")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	WriteX402PaymentRequired(w, terms)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("answered %d, want 402", w.Code)
	}
	if cacheControl := w.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cacheControl)
	}
	paymentRequired := x402V2TestDecodeHeader(t, w.Header().Get("PAYMENT-REQUIRED"))
	requireX402V2PaymentRequired(t, paymentRequired)

	golden := map[string]any{}
	if err := json.Unmarshal(x402V2TestFixture(t, "v2-payment-required-data_1tib.json"), &golden); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paymentRequired, golden) {
		got, _ := json.MarshalIndent(paymentRequired, "", "  ")
		t.Errorf("PAYMENT-REQUIRED decodes to\n%s\nwant testdata/x402/v2-payment-required-data_1tib.json", got)
	}

	// the body is the v1 terms, as before
	body := X402PaymentRequired{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the body is not v1 terms: %v", err)
	}
	if body.X402Version != X402VersionV1 || len(body.Accepts) != 2 {
		t.Fatalf("the v1 body: %+v", body)
	}
	for _, accept := range body.Accepts {
		if accept.MaxAmountRequired != "3000000" || accept.Resource != "https://api.x402.example/x402/purchase" {
			t.Errorf("v1 accept %+v", accept)
		}
	}
	if body.Accepts[0].Network != "base" || body.Accepts[1].Network != "solana" {
		t.Errorf("v1 networks %s, %s, want base, solana", body.Accepts[0].Network, body.Accepts[1].Network)
	}
}

// Each version's Solana term carries that version's fee payer from
// /supported, and v2 also the facilitator's transactionVersions, which "the
// server MUST copy" (specs/schemes/exact/scheme_exact_svm.md 1.5). A version
// whose kind is missing, or whose transactionVersions is anything but the
// JSON integers 0 and 1, leaves Solana out of that version only.
//
// Before this change there were no v2 terms at all.
func TestX402V2SolanaQuotesTheV2KindsFeePayer(t *testing.T) {
	setup := newX402V2TestSetup(t)
	solana := func(terms *X402Terms) (v1 map[string]any, v2 map[string]any) {
		if terms.V1 != nil {
			for _, accept := range terms.V1.Accepts {
				if accept.Network == "solana" {
					v1 = accept.Extra
				}
			}
		}
		if terms.V2 != nil {
			for _, accept := range terms.V2.Accepts {
				if accept.Network == "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp" {
					v2 = accept.Extra
				}
			}
		}
		return
	}

	for name, test := range map[string]struct {
		supported string
		v1        map[string]any
		v2        map[string]any
	}{
		"distinct fee payers per version": {
			supported: fmt.Sprintf(`{"kinds":[
				{"x402Version":1,"scheme":"exact","network":"solana","extra":{"feePayer":%q}},
				{"x402Version":2,"scheme":"exact","network":"solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp","extra":{"feePayer":%q,"transactionVersions":[0,1]}}]}`,
				x402CdpTestFeePayerV1, x402CdpTestFeePayerV2),
			v1: map[string]any{"feePayer": x402CdpTestFeePayerV1},
			v2: map[string]any{"feePayer": x402CdpTestFeePayerV2, "transactionVersions": []int{0, 1}},
		},
		"no transaction versions advertised": {
			supported: fmt.Sprintf(`{"kinds":[{"x402Version":2,"scheme":"exact","network":"solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp","extra":{"feePayer":%q}}]}`, x402CdpTestFeePayerV2),
			v2:        map[string]any{"feePayer": x402CdpTestFeePayerV2},
		},
		"only a v1 kind": {
			supported: fmt.Sprintf(`{"kinds":[{"x402Version":1,"scheme":"exact","network":"solana","extra":{"feePayer":%q}}]}`, x402CdpTestFeePayerV1),
			v1:        map[string]any{"feePayer": x402CdpTestFeePayerV1},
		},
		"a string transaction version": {
			supported: fmt.Sprintf(`{"kinds":[{"x402Version":2,"scheme":"exact","network":"solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp","extra":{"feePayer":%q,"transactionVersions":["0"]}}]}`, x402CdpTestFeePayerV2),
		},
		"a transaction version that is not 0 or 1": {
			supported: fmt.Sprintf(`{"kinds":[{"x402Version":2,"scheme":"exact","network":"solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp","extra":{"feePayer":%q,"transactionVersions":[0,2]}}]}`, x402CdpTestFeePayerV2),
		},
		"a v2 kind under the v1 network name": {
			supported: fmt.Sprintf(`{"kinds":[{"x402Version":2,"scheme":"exact","network":"solana","extra":{"feePayer":%q}}]}`, x402CdpTestFeePayerV2),
		},
	} {
		t.Run(name, func(t *testing.T) {
			// a fresh facilitator url per case, so the cache starts empty
			fresh := newX402CdpTestFacilitator(t, setup.facilitator.key)
			fresh.set(func(f *x402CdpTestFacilitator) { f.supportedBody = test.supported })
			c := x402CdpTestConfig(t, x402CdpTestDocument(fresh.url, setup.facilitator.key, "base", "solana"))
			terms, err := x402TermsForConfig(c, x402PurchaseResource, X402SkuData1Tib, "")
			if err != nil {
				t.Fatal(err)
			}
			v1, v2 := solana(terms)
			want := func(version string, got map[string]any, want map[string]any) {
				gotJson, _ := json.Marshal(got)
				wantJson, _ := json.Marshal(want)
				if string(gotJson) != string(wantJson) {
					t.Errorf("%s solana extra %s, want %s", version, gotJson, wantJson)
				}
			}
			want("v1", v1, test.v1)
			want("v2", v2, test.v2)
			// Base is quoted in both versions whatever /supported says
			if terms.V1 == nil || terms.V1.Accepts[0].Network != "base" || terms.V2 == nil || terms.V2.Accepts[0].Network != "eip155:8453" {
				t.Errorf("base is not quoted in both versions: %+v %+v", terms.V1, terms.V2)
			}
		})
	}
}

// A payment the official v2 client built for our terms (recorded) is
// verified and settled in v2: /verify and /settle get x402Version 2, the
// client's PaymentPayload as it was signed, and OUR v2 term as the
// requirements. The facilitator's CAIP-2 network becomes our name for the
// grant and the Stripe record, and the receipt is in v2.
//
// Before this change the payment was refused before the facilitator: "x402
// payment is for x402 version 2; this server takes version 1".
func TestX402V2PaymentIsVerifiedAndSettledInV2(t *testing.T) {
	setup := newX402V2TestSetup(t)
	payment, decoded := x402V2TestClientPayment(t)

	networkId := server.NewId()
	result, err := X402Purchase(context.Background(), networkId, payment, &X402PurchaseArgs{SkuId: X402SkuData1Tib}, x402PurchaseResource)
	if err != nil {
		t.Fatalf("the purchase failed: %v (facilitator saw %s)", err, x402CdpTestDescribe(setup.facilitator.received()))
	}

	golden := map[string]any{}
	if err := json.Unmarshal(x402V2TestFixture(t, "v2-payment-required-data_1tib.json"), &golden); err != nil {
		t.Fatal(err)
	}
	baseTerm := golden["accepts"].([]any)[0]
	for _, path := range []string{"/verify", "/settle"} {
		requests := setup.facilitator.receivedOn(path)
		if len(requests) != 1 {
			t.Fatalf("%s requested %d times", path, len(requests))
		}
		if requests[0].jwtErr != nil {
			t.Errorf("%s: %v", path, requests[0].jwtErr)
		}
		body := map[string]any{}
		if err := json.Unmarshal(requests[0].body, &body); err != nil {
			t.Fatal(err)
		}
		if body["x402Version"] != float64(2) {
			t.Errorf("%s x402Version = %v, want 2", path, body["x402Version"])
		}
		if !reflect.DeepEqual(body["paymentPayload"], decoded) {
			t.Errorf("%s paymentPayload = %v, want the client's payload %v", path, body["paymentPayload"], decoded)
		}
		if !reflect.DeepEqual(body["paymentRequirements"], baseTerm) {
			t.Errorf("%s paymentRequirements = %v, want our v2 Base term %v", path, body["paymentRequirements"], baseTerm)
		}
	}

	settled := setup.answers["settle_success"].Body
	transaction := struct {
		Transaction string `json:"transaction"`
		Payer       string `json:"payer"`
	}{}
	json.Unmarshal(settled, &transaction)
	if result.X402Version != X402VersionV2 || result.Network != "base" || result.Transaction != transaction.Transaction {
		t.Errorf("result %+v", result)
	}
	if granted := setup.grants.granted[transaction.Transaction]; granted == nil || granted.SkuId != X402SkuData1Tib {
		t.Errorf("the settled transaction was not granted data_1tib: %v", setup.grants.granted)
	}
	recorded := setup.stripe.recorded()
	if len(recorded) != 1 {
		t.Fatalf("stripe recorded %d payments, want 1", len(recorded))
	}
	if network := recorded[0].form.Get("payment_method_options[crypto][transaction_verification_options][network]"); network != "base" {
		t.Errorf("stripe was asked to verify on %q, want base", network)
	}
	if amount := recorded[0].form.Get("amount"); amount != "300" {
		t.Errorf("stripe amount %s, want 300 cents", amount)
	}

	settlement, _ := json.Marshal(result.SettlementResponse())
	want := fmt.Sprintf(`{"success":true,"transaction":%q,"network":"eip155:8453","payer":%q}`, transaction.Transaction, transaction.Payer)
	if string(settlement) != want {
		t.Errorf("settlement response %s, want %s", settlement, want)
	}
}

// A v2 payment is checked against OUR terms before anything reaches the
// facilitator: every field of `accepted` but extra must equal a quoted term,
// and extra must hold the quoted extra (x402-foundation/x402 go/server.go
// FindMatchingRequirements). A client may add keys to extra, but not choose a
// transfer method or payment flow we did not quote (spec 6.1). A mismatch is
// answered 402 with the terms quoted again.
//
// Before this change no v2 payment was accepted at all, so the cases a client
// may legitimately send failed too.
func TestX402V2PaymentMustMatchTheQuotedTerms(t *testing.T) {
	setup := newX402V2TestSetup(t)
	_, decoded := x402V2TestClientPayment(t)

	mutated := func(mutate func(accepted map[string]any)) string {
		payment := x402V2TestCopy(t, decoded)
		mutate(payment["accepted"].(map[string]any))
		return x402V2TestPayment(t, payment)
	}
	extra := func(accepted map[string]any) map[string]any {
		return accepted["extra"].(map[string]any)
	}

	for name, payment := range map[string]string{
		"a lower amount":           mutated(func(a map[string]any) { a["amount"] = "1" }),
		"another pay-to":           mutated(func(a map[string]any) { a["payTo"] = "0x" + strings.Repeat("d4", 20) }),
		"another asset":            mutated(func(a map[string]any) { a["asset"] = "0x" + strings.Repeat("e5", 20) }),
		"another network":          mutated(func(a map[string]any) { a["network"] = "eip155:84532" }),
		"the v1 network name":      mutated(func(a map[string]any) { a["network"] = "base" }),
		"a shorter timeout":        mutated(func(a map[string]any) { a["maxTimeoutSeconds"] = 60 }),
		"another EIP-712 version":  mutated(func(a map[string]any) { extra(a)["version"] = "1" }),
		"no EIP-712 name":          mutated(func(a map[string]any) { delete(extra(a), "name") }),
		"no extra":                 mutated(func(a map[string]any) { delete(a, "extra") }),
		"permit2 transfers":        mutated(func(a map[string]any) { extra(a)["assetTransferMethod"] = "permit2" }),
		"an upfront payment flow":  mutated(func(a map[string]any) { extra(a)["paymentFlow"] = "upfront" }),
		"a sku price it was not":   mutated(func(a map[string]any) { a["amount"] = "5000000" }),
		"an amount with a decimal": mutated(func(a map[string]any) { a["amount"] = "3.00" }),
	} {
		paymentRequests := func() int {
			return len(setup.facilitator.receivedOn("/verify")) + len(setup.facilitator.receivedOn("/settle"))
		}
		before := paymentRequests()
		_, err := X402Purchase(context.Background(), server.NewId(), payment, &X402PurchaseArgs{SkuId: X402SkuData1Tib}, x402PurchaseResource)
		paymentError := &X402PaymentError{}
		if err == nil || !errorsAsX402PaymentError(err, &paymentError) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if paymentError.Status != http.StatusPaymentRequired || paymentError.QuotedTerms() == nil {
			t.Errorf("%s: answered %d with terms %v, want 402 with the terms", name, paymentError.Status, paymentError.QuotedTerms())
		}
		if after := paymentRequests(); after != before {
			t.Errorf("%s reached the facilitator's /verify or /settle", name)
		}
	}

	// keys a client may add, and the default method and flow named outright
	for name, payment := range map[string]string{
		"the default transfer method named": mutated(func(a map[string]any) { extra(a)["assetTransferMethod"] = "eip3009" }),
		"the default payment flow named":    mutated(func(a map[string]any) { extra(a)["paymentFlow"] = "authorization" }),
		"another key":                       mutated(func(a map[string]any) { extra(a)["note"] = "client-side" }),
	} {
		// each a distinct settlement, so none is a retry of another
		transaction := "0x" + strings.Repeat(fmt.Sprintf("%02x", len(name)), 32)
		setup.facilitator.set(func(f *x402CdpTestFacilitator) {
			f.settleBody = fmt.Sprintf(`{"success":true,"transaction":%q,"network":"eip155:8453"}`, transaction)
		})
		if _, err := X402Purchase(context.Background(), server.NewId(), payment, &X402PurchaseArgs{SkuId: X402SkuData1Tib}, x402PurchaseResource); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// the network an agent names must be the one it paid on
	payment, _ := x402V2TestClientPayment(t)
	if _, err := X402Purchase(context.Background(), server.NewId(), payment, &X402PurchaseArgs{SkuId: X402SkuData1Tib, Network: "solana"}, x402PurchaseResource); err == nil ||
		!strings.Contains(err.Error(), "x402 payment is for network eip155:8453, not solana") {
		t.Errorf("a v2 payment on Base with network solana named: %v", err)
	}
}

func errorsAsX402PaymentError(err error, target **X402PaymentError) bool {
	paymentError, ok := err.(*X402PaymentError)
	if ok {
		*target = paymentError
	}
	return ok
}

// The x402 HTTP transport's error handling (specs/transports-v2/http.md and
// specs/transports-v1/http.md, "Error Handling"): a malformed payment is 400;
// a payment the facilitator refuses is 402 with the terms quoted again, the
// reason as their error (the x402 code in v2), and a refused settlement's
// SettlementResponse in the payment response header of the payment's
// version.
//
// Before this change every one of these was a 502 with a text body.
func TestX402PaymentErrorsFollowTheHttpTransport(t *testing.T) {
	setup := newX402V2TestSetup(t, "base")
	paymentV2, _ := x402V2TestClientPayment(t)
	quote, err := X402PaymentRequiredFor(x402PurchaseResource, X402SkuData1Tib, "")
	if err != nil {
		t.Fatal(err)
	}
	paymentV1, _ := x402CdpTestEvmPayment(t, "base", x402CdpTestPayToBase, quote.Accepts[0].MaxAmountRequired)

	answer := func(payment string) *httptest.ResponseRecorder {
		t.Helper()
		_, err := X402Purchase(context.Background(), server.NewId(), payment, &X402PurchaseArgs{SkuId: X402SkuData1Tib}, x402PurchaseResource)
		if err == nil {
			t.Fatal("the purchase succeeded")
		}
		w := httptest.NewRecorder()
		writeX402PurchaseError(w, err)
		return w
	}

	// malformed: 400
	for _, payment := range []string{"not base64 at all!", base64.StdEncoding.EncodeToString([]byte(`{"x402Version":3}`))} {
		if w := answer(payment); w.Code != http.StatusBadRequest || w.Header().Get("PAYMENT-REQUIRED") != "" {
			t.Errorf("malformed %q: %d %q", payment, w.Code, w.Body.String())
		}
	}

	// refused at verify: 402, the reason as the terms' error, no receipt
	setup.answer("/verify", "verify_insufficient_funds")
	w := answer(paymentV2)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("verify refusal answered %d, want 402", w.Code)
	}
	if terms := x402V2TestDecodeHeader(t, w.Header().Get("PAYMENT-REQUIRED")); terms["error"] != "insufficient_funds" {
		t.Errorf("the v2 terms' error = %v, want insufficient_funds", terms["error"])
	}
	body := X402PaymentRequired{}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.X402Version != X402VersionV1 || body.Error != "x402 payment invalid: insufficient_funds" || len(body.Accepts) != 1 {
		t.Errorf("the v1 body %+v", body)
	}
	if w.Header().Get("PAYMENT-RESPONSE") != "" || w.Header().Get("X-PAYMENT-RESPONSE") != "" {
		t.Error("a verify refusal carried a settlement response")
	}

	// refused at settle: 402, and the failed settlement in the payment's version
	setup.answer("/verify", "verify_valid")
	setup.answer("/settle", "settle_insufficient_funds")
	for _, payment := range []struct {
		version  string
		value    string
		header   string
		other    string
		network  string
		errorMsg string
	}{
		{"v2", paymentV2, "PAYMENT-RESPONSE", "X-PAYMENT-RESPONSE", "eip155:8453", "insufficient_funds"},
		{"v1", paymentV1, "X-PAYMENT-RESPONSE", "PAYMENT-RESPONSE", "base", "x402 settlement failed: insufficient_funds"},
	} {
		w := answer(payment.value)
		if w.Code != http.StatusPaymentRequired {
			t.Fatalf("%s settle refusal answered %d, want 402", payment.version, w.Code)
		}
		settlement := x402V2TestDecodeHeader(t, w.Header().Get(payment.header))
		if settlement["success"] != false || settlement["errorReason"] != "insufficient_funds" || settlement["network"] != payment.network {
			t.Errorf("%s %s = %v", payment.version, payment.header, settlement)
		}
		if w.Header().Get(payment.other) != "" {
			t.Errorf("%s payment answered in %s too", payment.version, payment.other)
		}
		terms := x402V2TestDecodeHeader(t, w.Header().Get("PAYMENT-REQUIRED"))
		body := X402PaymentRequired{}
		json.Unmarshal(w.Body.Bytes(), &body)
		if payment.version == "v2" && terms["error"] != payment.errorMsg || payment.version == "v1" && body.Error != payment.errorMsg {
			t.Errorf("%s terms error: v2 %v, v1 %q", payment.version, terms["error"], body.Error)
		}
	}
	if len(setup.grants.granted) != 0 {
		t.Errorf("a refused payment was granted: %v", setup.grants.granted)
	}
}

// settlement_pending is not terminal: the facilitator broadcast the
// transaction and could not confirm it, so the money may still move (v2 spec
// 9). The settle is asked once more with the same request, as the reference
// resource servers do; a confirmation then grants as usual. Still pending, it
// grants nothing, is recorded for the operator as settlement_pending keyed by
// the transaction, and tells the agent not to pay again, with the transaction
// in the receipt header. Never a third settle.
//
// Before this change the first pending answer was "x402 settlement failed:
// settlement_pending" (or, on a non-2xx, "x402 settle failed"), with no second
// ask and no record of a transaction that may have moved the money.
func TestX402SettlementPendingIsAskedOnceMoreAndRecorded(t *testing.T) {
	setup := newX402V2TestSetup(t, "base")
	payment, _ := x402V2TestClientPayment(t)
	pending := setup.answers["settle_pending"]
	confirmed := setup.answers["settle_success_after_pending"]
	pendingTransaction := "0x89c91c789e57059b17285e7ba1716a1f5ff4c5dace0ea5a5135f26158d0421b9"

	// pending, then confirmed: granted once
	before := len(setup.facilitator.receivedOn("/settle"))
	result, err := x402V2TestPurchaseWithSettleSequence(t, setup, payment, []x402V2TestAnswer{pending, confirmed})
	if err != nil {
		t.Fatalf("pending then confirmed: %v", err)
	}
	settles := len(setup.facilitator.receivedOn("/settle")) - before
	if settles != 2 || result.Transaction != pendingTransaction || setup.grants.granted[pendingTransaction] == nil {
		t.Errorf("pending then confirmed: %d settles, result %+v, granted %v", settles, result, setup.grants.granted)
	}

	// pending twice: not granted, recorded, the agent told not to pay again
	networkId := server.NewId()
	delete(setup.grants.granted, pendingTransaction)
	before = len(setup.facilitator.receivedOn("/settle"))
	_, err = x402V2TestPurchaseWithSettleSequenceFor(t, setup, networkId, payment, []x402V2TestAnswer{pending, pending, confirmed})
	if err == nil {
		t.Fatal("a settlement still pending was granted")
	}
	if settles := len(setup.facilitator.receivedOn("/settle")) - before; settles != 2 {
		t.Errorf("still pending: %d settles, want 2", settles)
	}
	if setup.grants.granted[pendingTransaction] != nil {
		t.Error("a pending settlement was granted")
	}
	if n := setup.events.count(model.PaymentReconcileActionSettlementPending, pendingTransaction, false); n != 1 {
		t.Errorf("%d settlement_pending events for the transaction, want 1", n)
	}
	for _, event := range setup.events.events {
		if event.Action == model.PaymentReconcileActionSettlementPending {
			if event.NetworkId == nil || *event.NetworkId != networkId || event.Details["sku_id"] != X402SkuData1Tib || event.Details["network"] != "base" {
				t.Errorf("settlement_pending event %+v", event)
			}
		}
	}
	if n := setup.events.count(model.PaymentReconcileActionSettledNotGranted, pendingTransaction, false); n != 0 {
		t.Error("a pending settlement was queued for the reconciler to grant")
	}
	w := httptest.NewRecorder()
	writeX402PurchaseError(w, err)
	if w.Code != http.StatusPaymentRequired {
		t.Errorf("answered %d, want 402", w.Code)
	}
	settlement := x402V2TestDecodeHeader(t, w.Header().Get("PAYMENT-RESPONSE"))
	if settlement["errorReason"] != "settlement_pending" || settlement["transaction"] != pendingTransaction || settlement["network"] != "eip155:8453" {
		t.Errorf("PAYMENT-RESPONSE %v", settlement)
	}
	if !strings.Contains(err.Error(), "Do not sign another payment") || !strings.Contains(err.Error(), pendingTransaction) {
		t.Errorf("the agent was told %q", err)
	}
}

// x402V2TestPurchaseWithSettleSequence purchases data_1tib while the fake
// facilitator answers its settles in sequence (the last answer repeats).
func x402V2TestPurchaseWithSettleSequence(t *testing.T, setup *x402V2TestSetup, payment string, answers []x402V2TestAnswer) (*X402PurchaseResult, error) {
	return x402V2TestPurchaseWithSettleSequenceFor(t, setup, server.NewId(), payment, answers)
}

func x402V2TestPurchaseWithSettleSequenceFor(t *testing.T, setup *x402V2TestSetup, networkId server.Id, payment string, answers []x402V2TestAnswer) (*X402PurchaseResult, error) {
	t.Helper()
	sequence := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setup.facilitator.serve(w, r)
		if strings.HasSuffix(r.URL.Path, "/settle") {
			// after each settle, the next answer
			settled := len(setup.facilitator.receivedOn("/settle"))
			setup.facilitator.set(func(f *x402CdpTestFacilitator) {
				next := answers[len(answers)-1]
				if settled < len(answers) {
					next = answers[settled]
				}
				f.settleStatus, f.settleBody = next.Status, string(next.Body)
			})
		}
	}))
	t.Cleanup(sequence.Close)
	previous := *setup.config
	c := *setup.config
	c.Facilitator.Url = sequence.URL + setup.facilitator.basePath
	SetX402Config(&c)
	defer SetX402Config(&previous)

	// the first settle of this purchase gets the first answer
	already := len(setup.facilitator.receivedOn("/settle"))
	shifted := make([]x402V2TestAnswer, already+len(answers))
	copy(shifted[already:], answers)
	answers = shifted
	setup.facilitator.set(func(f *x402CdpTestFacilitator) {
		f.requireJwt = false
		f.settleStatus, f.settleBody = answers[already].Status, string(answers[already].Body)
	})
	return X402Purchase(context.Background(), networkId, payment, &X402PurchaseArgs{SkuId: X402SkuData1Tib}, x402PurchaseResource)
}

// A request presents its payment in PAYMENT-SIGNATURE (v2) or X-PAYMENT (v1),
// not both. A paid retry in either header is never quoted terms again.
//
// Before this change PAYMENT-SIGNATURE was not read: a v2 client's paid retry
// looked unpaid, and on auth-client it was quoted a second payment.
func TestX402PresentedPaymentHeaders(t *testing.T) {
	for name, test := range map[string]struct {
		header  map[string]string
		payment string
		refused bool
	}{
		"v2":      {map[string]string{"PAYMENT-SIGNATURE": "djI="}, "djI=", false},
		"v1":      {map[string]string{"X-PAYMENT": "djE="}, "djE=", false},
		"v2 case": {map[string]string{"payment-signature": "djI="}, "djI=", false},
		"none":    {map[string]string{}, "", false},
		"both":    {map[string]string{"PAYMENT-SIGNATURE": "djI=", "X-PAYMENT": "djE="}, "", true},
	} {
		header := http.Header{}
		for key, value := range test.header {
			header.Set(key, value)
		}
		payment, err := x402PresentedPayment(header)
		if (err != nil) != test.refused || payment != test.payment {
			t.Errorf("%s: payment %q err %v", name, payment, err)
		}
	}

	paidV2 := newX402RefusalRequest(map[string]string{x402QuoteRequestHeader: "1", "PAYMENT-SIGNATURE": "eyJ4NDAyVmVyc2lvbiI6Mn0="})
	if X402QuotesClient(paidV2.Header) {
		t.Error("a request that already carries a v2 payment is quoted again")
	}
}

// The terms say what the price is and how to let a default x402 client pay
// it. The x402 SDK clients refuse a payment over $1 unless their owner raises
// the cap (docs.x402.org quickstart for buyers, "Spend Controls"), and the
// cheapest sku is $3, so a 402 that does not say so is one a default client
// can never pay. A price of $1 or less fits the default and says nothing of
// caps.
//
// Before this change the terms' error was only "payment required for <sku>",
// and the refusal's message on auth-client.
func TestX402TermsSayHowToAllowThePrice(t *testing.T) {
	setup := newX402V2TestSetup(t, "base")

	for _, test := range []struct {
		skuId string
		price string
	}{
		{X402SkuData1Tib, "$3.00"},
		{X402SkuProMonth, "$5.00"},
	} {
		terms, err := x402TermsForConfig(setup.config, x402PurchaseResource, test.skuId, "payment required for "+test.skuId)
		if err != nil {
			t.Fatal(err)
		}
		for version, text := range map[string]string{"v1": terms.V1.Error, "v2": terms.V2.Error} {
			for _, says := range []string{
				"payment required for " + test.skuId + ".",
				"costs " + test.price + " in USDC",
				"x402 clients refuse a payment over $1 by default",
				"raise your client's per-payment cap to " + test.price + " or more",
			} {
				if !strings.Contains(text, says) {
					t.Errorf("%s %s terms' error %q does not say %q", test.skuId, version, text, says)
				}
			}
		}
	}

	// a price within the default cap
	t.Cleanup(model.Testing_SetSkuPrices(5, 600*model.Gib, []model.ProDataCodeSku{{Data: model.Tib, PriceUsd: 1}}))
	terms, err := x402TermsForConfig(setup.config, x402PurchaseResource, X402SkuData1Tib, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(terms.V2.Error, "cap") || !strings.Contains(terms.V2.Error, "costs $1.00 in USDC") {
		t.Errorf("a $1 sku's terms say %q", terms.V2.Error)
	}
}

// GET /x402/skus says which protocol versions are spoken, each network's v2
// id, and each sku's exact atomic amount, so an agent can set its client's
// cap and pick its version without asking for a 402 first.
//
// Before this change it said x402_version 1 and gave only USD prices.
func TestX402SkusListsBothVersionsAndAtomicAmounts(t *testing.T) {
	newX402V2TestSetup(t)
	w := httptest.NewRecorder()
	X402SkusHandler(w, httptest.NewRequest(http.MethodGet, "/x402/skus", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("answered %d", w.Code)
	}
	catalog := struct {
		X402Version  int               `json:"x402_version"`
		X402Versions []int             `json:"x402_versions"`
		Networks     []string          `json:"networks"`
		NetworkIds   map[string]string `json:"network_ids"`
		Skus         []struct {
			SkuId    string  `json:"sku_id"`
			PriceUsd float64 `json:"price_usd"`
			Amount   string  `json:"amount"`
		} `json:"skus"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.X402Version != 2 || !reflect.DeepEqual(catalog.X402Versions, []int{2, 1}) {
		t.Errorf("versions %d %v, want 2 [2 1]", catalog.X402Version, catalog.X402Versions)
	}
	if !reflect.DeepEqual(catalog.NetworkIds, map[string]string{
		"base":   "eip155:8453",
		"solana": "solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp",
	}) {
		t.Errorf("network ids %v", catalog.NetworkIds)
	}
	amounts := map[string]string{}
	for _, sku := range catalog.Skus {
		amounts[sku.SkuId] = sku.Amount
	}
	if !reflect.DeepEqual(amounts, map[string]string{X402SkuProMonth: "5000000", X402SkuData1Tib: "3000000"}) {
		t.Errorf("amounts %v", amounts)
	}
}

// Every network x402 quotes has a v2 (CAIP-2) id, and the names map both
// ways: a v2 id from a client or facilitator becomes our name, and anything
// else is left as it is.
func TestEveryX402NetworkHasACaip2Id(t *testing.T) {
	for network := range x402Tokens {
		caip2, ok := x402Caip2Network(network)
		if !ok || !x402V2TestCaip2Pattern.MatchString(caip2) {
			t.Errorf("network %s has no CAIP-2 id (%q)", network, caip2)
			continue
		}
		if name, ok := x402NetworkForCaip2(caip2); !ok || name != network {
			t.Errorf("%s maps back to %q", caip2, name)
		}
		if x402NetworkName(caip2) != network || x402NetworkName(network) != network {
			t.Errorf("x402NetworkName(%s) = %s, x402NetworkName(%s) = %s", caip2, x402NetworkName(caip2), network, x402NetworkName(network))
		}
	}
	for _, unknown := range []string{"eip155:84532", "ethereum", ""} {
		if x402NetworkName(unknown) != unknown {
			t.Errorf("x402NetworkName(%q) = %q", unknown, x402NetworkName(unknown))
		}
	}
}
