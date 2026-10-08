package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// A fake Stripe backend, through the stripeApiBaseUrl seam. Nothing here
// contacts Stripe, and nothing needs the vault.
type x402StripeFixture struct {
	server *httptest.Server
	mutex  sync.Mutex
	calls  []x402StripeCall
	status int
}

type x402StripeCall struct {
	form           url.Values
	apiVersion     string
	idempotencyKey string
	authorization  string
}

func newX402StripeFixture(t *testing.T) *x402StripeFixture {
	t.Helper()
	fixture := &x402StripeFixture{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/payment_intents", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fixture.mutex.Lock()
		fixture.calls = append(fixture.calls, x402StripeCall{
			form:           r.PostForm,
			apiVersion:     r.Header.Get("Stripe-Version"),
			idempotencyKey: r.Header.Get("Idempotency-Key"),
			authorization:  r.Header.Get("Authorization"),
		})
		status := fixture.status
		fixture.mutex.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"synthetic stripe refusal"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "pi_synthetic", "status": "succeeded", "amount": 300,
		})
	})
	fixture.server = httptest.NewServer(mux)

	previousBaseUrl := stripeApiBaseUrl
	previousTokenFunc := stripeApiTokenFunc
	previousEnabled := x402StripeRecordEnabledFunc
	stripeApiBaseUrl = fixture.server.URL
	stripeApiTokenFunc = func() string { return "synthetic-stripe-token" }
	x402StripeRecordEnabledFunc = func() bool { return true }
	t.Cleanup(func() {
		stripeApiBaseUrl = previousBaseUrl
		stripeApiTokenFunc = previousTokenFunc
		x402StripeRecordEnabledFunc = previousEnabled
		fixture.server.Close()
	})
	return fixture
}

func (f *x402StripeFixture) recorded() []x402StripeCall {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]x402StripeCall(nil), f.calls...)
}

func x402TestSku() *X402Sku {
	return &X402Sku{SkuId: X402SkuData1Tib, PriceUsd: 3.00, Description: "1 TiB"}
}

func x402TestTerms(network string, amount string) *X402Accept {
	return &X402Accept{
		Scheme:            "exact",
		Network:           network,
		MaxAmountRequired: amount,
		PayTo:             "0xmerchant",
		Asset:             "usdc",
	}
}

// The whole point of the file: a settled transfer becomes a Stripe payment.
func TestX402SettledPaymentIsRecordedInStripe(t *testing.T) {
	fixture := newX402StripeFixture(t)

	x402RecordStripePayment(
		context.Background(),
		x402TestSku(),
		x402TestTerms("solana", "3000000"),
		&X402SettleResponse{Success: true, Transaction: "tx-abc", Network: "solana"},
	)

	calls := fixture.recorded()
	connect.AssertEqual(t, len(calls), 1)
	form := calls[0].form
	connect.AssertEqual(t, form.Get("amount"), "300")
	connect.AssertEqual(t, form.Get("currency"), "usd")
	connect.AssertEqual(t, form.Get("confirm"), "true")
	connect.AssertEqual(t, form.Get("payment_method_data[type]"), "crypto")
	connect.AssertEqual(t, form.Get("allowed_payment_method_types[0]"), "crypto")
	connect.AssertEqual(t, form.Get("payment_method_options[crypto][mode]"), "transaction_verification")
	connect.AssertEqual(t,
		form.Get("payment_method_options[crypto][transaction_verification_options][network]"),
		"solana")
	connect.AssertEqual(t,
		form.Get("payment_method_options[crypto][transaction_verification_options][transaction_hash]"),
		"tx-abc")
	connect.AssertEqual(t, form.Get("metadata[x402_sku]"), X402SkuData1Tib)

	// The feature only exists on the preview version; without the header Stripe
	// rejects the call.
	connect.AssertEqual(t, calls[0].apiVersion, stripePreviewApiVersion)
	// One transfer books once.
	connect.AssertEqual(t, calls[0].idempotencyKey, "x402-record-tx-abc")
	connect.AssertEqual(t, calls[0].authorization, "Bearer synthetic-stripe-token")
}

// The chain the money is actually on is the one that can verify it, so the
// facilitator's answer wins over the terms we quoted.
func TestX402StripeRecordPrefersTheSettledNetwork(t *testing.T) {
	fixture := newX402StripeFixture(t)

	x402RecordStripePayment(
		context.Background(),
		x402TestSku(),
		x402TestTerms("solana", "3000000"),
		&X402SettleResponse{Success: true, Transaction: "tx-base", Network: "base"},
	)

	calls := fixture.recorded()
	connect.AssertEqual(t, len(calls), 1)
	connect.AssertEqual(t,
		calls[0].form.Get("payment_method_options[crypto][transaction_verification_options][network]"),
		"base")
}

// A settle response with no network falls back to what we quoted.
func TestX402StripeRecordFallsBackToTheQuotedNetwork(t *testing.T) {
	fixture := newX402StripeFixture(t)

	x402RecordStripePayment(
		context.Background(),
		x402TestSku(),
		x402TestTerms("tempo", "3000000"),
		&X402SettleResponse{Success: true, Transaction: "tx-tempo"},
	)

	calls := fixture.recorded()
	connect.AssertEqual(t, len(calls), 1)
	connect.AssertEqual(t,
		calls[0].form.Get("payment_method_options[crypto][transaction_verification_options][network]"),
		"tempo")
}

// Everything that must produce NO call, because there is nothing Stripe could
// verify. Each of these would otherwise send a request that can only be
// refused, after the customer already paid.
func TestX402StripeRecordRefusesWhatCannotBeVerified(t *testing.T) {
	for name, record := range map[string]func(){
		"no transaction id": func() {
			x402RecordStripePayment(context.Background(), x402TestSku(),
				x402TestTerms("solana", "3000000"),
				&X402SettleResponse{Success: true, Network: "solana"})
		},
		"unmapped network": func() {
			x402RecordStripePayment(context.Background(), x402TestSku(),
				x402TestTerms("dogecoin", "3000000"),
				&X402SettleResponse{Success: true, Transaction: "tx", Network: "dogecoin"})
		},
		"below one cent": func() {
			// Rounding this up would book revenue never collected.
			x402RecordStripePayment(context.Background(), x402TestSku(),
				x402TestTerms("solana", "4000"),
				&X402SettleResponse{Success: true, Transaction: "tx", Network: "solana"})
		},
		"non-integer amount": func() {
			x402RecordStripePayment(context.Background(), x402TestSku(),
				x402TestTerms("solana", "3.00"),
				&X402SettleResponse{Success: true, Transaction: "tx", Network: "solana"})
		},
		"no amount": func() {
			x402RecordStripePayment(context.Background(), x402TestSku(),
				x402TestTerms("solana", ""),
				&X402SettleResponse{Success: true, Transaction: "tx", Network: "solana"})
		},
		"nil sku": func() {
			x402RecordStripePayment(context.Background(), nil,
				x402TestTerms("solana", "3000000"),
				&X402SettleResponse{Success: true, Transaction: "tx"})
		},
		"nil terms": func() {
			x402RecordStripePayment(context.Background(), x402TestSku(), nil,
				&X402SettleResponse{Success: true, Transaction: "tx"})
		},
		"nil settlement": func() {
			x402RecordStripePayment(context.Background(), x402TestSku(),
				x402TestTerms("solana", "3000000"), nil)
		},
	} {
		fixture := newX402StripeFixture(t)
		record()
		if len(fixture.recorded()) != 0 {
			t.Errorf("%s: expected no Stripe call, got %d", name, len(fixture.recorded()))
		}
	}
}

// While x402 is off nothing is recorded, and nothing reaches out.
func TestX402StripeRecordIsInertWhenDisabled(t *testing.T) {
	fixture := newX402StripeFixture(t)
	x402StripeRecordEnabledFunc = func() bool { return false }

	x402RecordStripePayment(
		context.Background(),
		x402TestSku(),
		x402TestTerms("solana", "3000000"),
		&X402SettleResponse{Success: true, Transaction: "tx", Network: "solana"},
	)
	connect.AssertEqual(t, len(fixture.recorded()), 0)
}

// The rule the whole file is built around: the money moved and the entitlement
// landed, so a Stripe failure is a logged bookkeeping hole and NOT an error the
// purchase can see. This function returns nothing; the test pins that it also
// does not panic and does not retry into a storm.
func TestX402StripeRecordSwallowsAStripeFailure(t *testing.T) {
	fixture := newX402StripeFixture(t)
	fixture.status = http.StatusPaymentRequired

	x402RecordStripePayment(
		context.Background(),
		x402TestSku(),
		x402TestTerms("solana", "3000000"),
		&X402SettleResponse{Success: true, Transaction: "tx-refused", Network: "solana"},
	)

	// Attempted once, refused, and the caller is none the wiser.
	connect.AssertEqual(t, len(fixture.recorded()), 1)
}

// Cents come from the integer the payment was bound to, not from a float price.
func TestX402StripeAmountCents(t *testing.T) {
	for atomic, want := range map[string]int64{
		"10000":    1,     // $0.01, the x402 floor
		"3000000":  300,   // $3.00
		"5000000":  500,   // $5.00
		"20000000": 2000,  // $20.00
		"39990000": 3999,  // $39.99
		"1999999":  200,   // rounds to $2.00 rather than truncating to $1.99
		"4999":     0,     // below a cent
		"0":        0,     //
		"1000000":  100,   // $1.00
		"99999999": 10000, // $100.00
	} {
		got, err := x402StripeAmountCents(atomic)
		if err != nil {
			t.Errorf("%s: %v", atomic, err)
			continue
		}
		if got != want {
			t.Errorf("x402StripeAmountCents(%q) = %d, want %d", atomic, got, want)
		}
	}

	for _, bad := range []string{"", "3.00", "abc", "-1", "$5"} {
		if _, err := x402StripeAmountCents(bad); err == nil {
			t.Errorf("expected %q to be refused", bad)
		}
	}
}

// Every network we quote has to be recordable, or a purchase on it settles and
// then silently fails to book.
func TestEveryQuotedNetworkIsRecordable(t *testing.T) {
	for _, network := range []string{"base", "solana", "tempo"} {
		if _, ok := x402StripeNetworks[network]; !ok {
			t.Errorf("network %q is quoted but has no Stripe mapping", network)
		}
	}
}
