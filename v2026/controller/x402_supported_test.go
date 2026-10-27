package controller

// The mechanisms the CDP facilitator integration added: the JWT's scope for the
// real CDP host, the /supported cache's timing, the config problems the loader
// logs, and what a facilitator's rejection tells the agent. Hermetic: fake
// facilitators only, no database.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The JWT for each facilitator request names exactly that request in its uris
// claim, "<METHOD> api.cdp.coinbase.com/platform/v2/x402/<endpoint>", the form
// CDP checks (coinbase/x402 go/legacy/pkg/coinbasefacilitator CreateAuthHeader:
// host without the scheme, then the full path). A trailing slash on the
// configured url does not change it.
func TestX402FacilitatorJwtIsScopedToTheCdpRequest(t *testing.T) {
	for name, newKey := range map[string]func(testing.TB) x402CdpTestKey{
		"ed25519":     newX402CdpTestEd25519Key,
		"ecdsa p-256": newX402CdpTestEcKey,
	} {
		t.Run(name, func(t *testing.T) {
			key := newKey(t)
			// the checks a CDP-like verifier makes, without a server
			verifier := &x402CdpTestFacilitator{key: key}
			for _, baseUrl := range []string{
				"https://api.cdp.coinbase.com/platform/v2/x402",
				"https://api.cdp.coinbase.com/platform/v2/x402/",
			} {
				c := &X402Config{Facilitator: x402FacilitatorConfig{Url: baseUrl, ApiKeyId: key.id, ApiKeySecret: key.secret}}
				for _, request := range []struct {
					method   string
					endpoint string
					uri      string
				}{
					{http.MethodPost, "/verify", "POST api.cdp.coinbase.com/platform/v2/x402/verify"},
					{http.MethodPost, "/settle", "POST api.cdp.coinbase.com/platform/v2/x402/settle"},
					{http.MethodGet, "/supported", "GET api.cdp.coinbase.com/platform/v2/x402/supported"},
				} {
					endpointUrl, err := x402FacilitatorEndpoint(c, request.endpoint)
					if err != nil {
						t.Fatal(err)
					}
					authorization, err := x402FacilitatorAuthorization(c, request.method, endpointUrl)
					if err != nil {
						t.Fatal(err)
					}
					r := httptest.NewRequest(request.method, endpointUrl.String(), nil)
					r.Header.Set("Authorization", authorization)
					claims, _, err := verifier.checkJwt(r)
					if err != nil {
						t.Errorf("%s %s: %v", request.method, endpointUrl, err)
						continue
					}
					if uris, _ := claims["uris"].([]any); len(uris) != 1 || uris[0] != request.uri {
						t.Errorf("uris %v, want [%s]", claims["uris"], request.uri)
					}
				}
			}
		})
	}
}

// x402TestClock is a settable x402Now.
type x402TestClock struct {
	mutex sync.Mutex
	now   time.Time
}

func installX402TestClock(t *testing.T) *x402TestClock {
	clock := &x402TestClock{now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	previous := x402Now
	x402Now = clock.Now
	t.Cleanup(func() { x402Now = previous })
	return clock
}

func (self *x402TestClock) Now() time.Time {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.now
}

func (self *x402TestClock) advance(d time.Duration) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.now = self.now.Add(d)
}

// The fee payer is fetched again once the cached answer is 10 minutes old. A
// failed refresh keeps quoting the fee payer last fetched, for up to an hour
// after that fetch, and is retried at most every 30 s; past the hour the
// network is not quoted until a fetch succeeds again.
func TestX402SupportedCacheTiming(t *testing.T) {
	clock := installX402TestClock(t)
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "solana"))
	var working string
	facilitator.set(func(f *x402CdpTestFacilitator) { working = f.supportedBody })

	step := func(name string, advance time.Duration, wantFetches int, wantQuoted bool) {
		t.Helper()
		clock.advance(advance)
		feePayer, ok := x402FeePayer(c, "solana")
		if fetches := len(facilitator.receivedOn("/supported")); fetches != wantFetches {
			t.Errorf("%s: %d fetches, want %d", name, fetches, wantFetches)
		}
		if ok != wantQuoted || (ok && feePayer != x402CdpTestFeePayerV1) {
			t.Errorf("%s: fee payer %q known=%t, want known=%t", name, feePayer, ok, wantQuoted)
		}
	}
	fail := func() {
		facilitator.set(func(f *x402CdpTestFacilitator) { f.supportedStatus = http.StatusServiceUnavailable })
	}
	restore := func() {
		facilitator.set(func(f *x402CdpTestFacilitator) {
			f.supportedStatus = http.StatusOK
			f.supportedBody = working
		})
	}

	step("first quote fetches", 0, 1, true)
	step("9 minutes later, cached", 9*time.Minute, 1, true)
	step("10 minutes after the fetch, fetched again", time.Minute, 2, true)
	fail()
	step("a failed refresh keeps the known fee payer", 10*time.Minute, 3, true)
	step("not retried within 30 s", 10*time.Second, 3, true)
	step("retried after 30 s", 30*time.Second, 4, true)
	// the last success was 10 minutes in; an hour after it the known fee
	// payer is too old to quote
	step("an hour after the last success, not quoted", 50*time.Minute, 5, false)
	restore()
	step("a later success quotes it again", 30*time.Second, 6, true)
}

// While a /supported fetch is in flight, other quotes do not start another:
// they quote what is known (here nothing, so the network sits out), and the
// fetch's answer serves the quotes after it.
func TestX402SupportedFetchesOneAtATime(t *testing.T) {
	key := newX402CdpTestEd25519Key(t)
	var fetches atomic.Int32
	arrived := make(chan struct{})
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fetches.Add(1) == 1 {
			close(arrived)
		}
		<-release
		w.Write([]byte(`{"kinds":[{"x402Version":1,"scheme":"exact","network":"solana","extra":{"feePayer":"` + x402CdpTestFeePayerV1 + `"}}]}`))
	}))
	defer slow.Close()
	c := x402CdpTestConfig(t, x402CdpTestDocument(slow.URL+"/one-at-a-time"+x402CdpTestBasePath, key, "solana"))

	first := make(chan bool)
	go func() {
		_, ok := x402FeePayer(c, "solana")
		first <- ok
	}()
	<-arrived
	for i := 0; i < 5; i += 1 {
		if _, ok := x402FeePayer(c, "solana"); ok {
			t.Error("a fee payer was known before the first fetch answered")
		}
	}
	close(release)
	if !<-first {
		t.Error("the fetch's own quote did not get the fee payer")
	}
	if feePayer, ok := x402FeePayer(c, "solana"); !ok || feePayer != x402CdpTestFeePayerV1 {
		t.Errorf("after the fetch: %q %t", feePayer, ok)
	}
	if fetches.Load() != 1 {
		t.Errorf("%d fetches, want 1", fetches.Load())
	}
}

// The loader logs x402ConfigProblem when it turns x402 off, so the reason
// names what to fix, and never a value from the config.
func TestX402ConfigProblemSaysWhatToFix(t *testing.T) {
	key := newX402CdpTestEd25519Key(t)
	const facilitatorUrl = "https://api.cdp.coinbase.com/platform/v2/x402"
	for name, want := range map[string]struct {
		document map[string]any
		says     string
	}{
		"tempo": {
			x402CdpTestDocument(facilitatorUrl, key, "base", "tempo"),
			"network tempo is not an x402 network: Stripe takes Tempo payments through MPP",
		},
		"ethereum": {
			x402CdpTestDocument(facilitatorUrl, key, "ethereum"),
			"network ethereum is not an x402 network this server can quote (base, solana)",
		},
		"only the static api_key": {
			withX402CdpTestFacilitator(withX402CdpTestFacilitator(
				x402CdpTestDocument(facilitatorUrl, key, "base"), "api_key_id", nil), "api_key_secret", nil),
			"facilitator.api_key_id and facilitator.api_key_secret",
		},
		"a secret that is not a key": {
			withX402CdpTestFacilitator(x402CdpTestDocument(facilitatorUrl, key, "base"), "api_key_secret", "c2stdGVzdA=="),
			"facilitator.api_key_secret is not a CDP API key secret",
		},
		"an http facilitator": {
			withX402CdpTestFacilitator(x402CdpTestDocument(facilitatorUrl, key, "base"), "url", "http://api.cdp.coinbase.com/platform/v2/x402"),
			"facilitator.url must be the facilitator's https url",
		},
		"a solana address under base": {
			withX402CdpTestPayTo(x402CdpTestDocument(facilitatorUrl, key, "base"), "base", x402CdpTestPayToSolana),
			"pay_to.base is not an address of the base network's form",
		},
		"no deposit address": {
			withX402CdpTestPayTo(x402CdpTestDocument(facilitatorUrl, key, "base", "solana"), "solana", ""),
			"pay_to.solana (the Stripe deposit address for solana) is missing",
		},
	} {
		err := x402ConfigProblem(x402CdpTestConfig(t, want.document))
		if err == nil {
			t.Errorf("%s: no problem", name)
			continue
		}
		if !strings.Contains(err.Error(), want.says) {
			t.Errorf("%s: %q does not say %q", name, err, want.says)
		}
		for _, value := range []string{key.secret, x402CdpTestPayToBase, x402CdpTestPayToSolana, x402CdpTestStaticBearer} {
			if strings.Contains(err.Error(), value) {
				t.Errorf("%s: the reason quotes a config value", name)
			}
		}
	}
}

// What a refused payment tells the agent: an x402 error code from the
// facilitator's rejection passes through, anything else it said does not, and
// a payment that is malformed or signed for another network than the one
// named is refused before the facilitator hears of it.
func TestX402PurchaseReportsTheRejection(t *testing.T) {
	x402CdpTestPrices(t)
	t.Setenv("WARP_DOMAIN", "x402.example")
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base"))
	SetX402Config(c)
	t.Cleanup(func() { SetX402Config(nil) })

	quote, err := X402PaymentRequiredFor(x402PurchaseResource, X402SkuData1Tib, "")
	if err != nil {
		t.Fatal(err)
	}
	payment, _ := x402CdpTestEvmPayment(t, "base", x402CdpTestPayToBase, quote.Accepts[0].MaxAmountRequired)
	purchase := func(payment string, network string) string {
		_, err := X402Purchase(context.Background(), server.NewId(), payment, &X402PurchaseArgs{SkuId: X402SkuData1Tib, Network: network})
		if err == nil {
			t.Fatal("the purchase succeeded")
		}
		return err.Error()
	}

	for _, answer := range []struct {
		endpoint string
		status   int
		body     string
		says     string
	}{
		{"verify", http.StatusBadRequest, `{"isValid":false,"invalidReason":"insufficient_funds"}`, "x402 payment invalid: insufficient_funds"},
		{"verify", http.StatusOK, `{"isValid":false,"invalidReason":"invalid_exact_evm_payload_signature"}`, "x402 payment invalid: invalid_exact_evm_payload_signature"},
		{"verify", http.StatusBadRequest, `{"isValid":false,"invalidReason":"synthetic facilitator detail <b>"}`, "x402 payment invalid: rejected"},
		{"settle", http.StatusBadRequest, `{"success":false,"errorReason":"invalid_exact_evm_payload_authorization_valid_before"}`, "x402 settlement failed: invalid_exact_evm_payload_authorization_valid_before"},
		{"settle", http.StatusInternalServerError, `{"success":false,"errorReason":"settlement_pending","transaction":"0x01"}`, "x402 settle failed"},
	} {
		facilitator.set(func(f *x402CdpTestFacilitator) {
			f.verifyStatus, f.verifyBody = http.StatusOK, `{"isValid":true}`
			if answer.endpoint == "verify" {
				f.verifyStatus, f.verifyBody = answer.status, answer.body
			} else {
				f.settleStatus, f.settleBody = answer.status, answer.body
			}
		})
		if says := purchase(payment, ""); says != answer.says {
			t.Errorf("%s %d %s: the agent was told %q, want %q", answer.endpoint, answer.status, answer.body, says, answer.says)
		}
	}

	before := len(facilitator.received())
	solanaPayment, _ := x402CdpTestPayment(t, map[string]any{
		"x402Version": 1, "scheme": "exact", "network": "solana",
		"payload": map[string]any{"transaction": "AQID"},
	})
	if says := purchase(solanaPayment, "base"); !strings.Contains(says, "x402 payment is for network solana, not base") {
		t.Errorf("a payment signed for another network: %q", says)
	}
	if says := purchase("not base64 at all!", ""); !strings.Contains(says, "x402 payment is not base64") {
		t.Errorf("a malformed payment: %q", says)
	}
	if after := len(facilitator.received()); after != before {
		t.Errorf("refused payments reached the facilitator: %s", x402CdpTestDescribe(facilitator.received()[before:]))
	}
}
