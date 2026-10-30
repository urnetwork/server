package controller

// x402 against the CDP facilitator and the x402 v1 wire format.
//
// Stripe settles x402 mainnet payments through the Coinbase Developer Platform
// facilitator (docs.stripe.com/payments/machine/x402). Each test here pins one
// thing a real payment needs, against a fake facilitator that checks requests
// the way CDP does:
//   - every request carries a CDP JWT, minted per request for that method,
//     host and path (coinbase/x402 go/legacy/pkg/coinbasefacilitator);
//   - the terms quote the token address and the scheme's `extra`
//     (specs/x402-specification-v1.md 5.1, specs/schemes/exact/);
//   - verify and settle send the payment payload as a JSON object (spec 7.1);
//   - X-PAYMENT-RESPONSE is base64 JSON of the SettlementResponse
//     (specs/transports-v1/http.md).
//
// The configs are parsed from x402.yml documents, the shape the vault holds.
// Nothing here contacts CDP or Stripe, and every address is synthetic except
// the public USDC token addresses.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The public USDC token addresses the terms must quote.
const (
	x402CdpTestUsdcBase   = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	x402CdpTestUsdcSolana = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)

// Synthetic deposit addresses, in the form each network uses.
var (
	x402CdpTestPayToBase   = "0x" + strings.Repeat("b1", 20)
	x402CdpTestPayToSolana = "SynthPayTo" + strings.Repeat("1", 32)
	// what /supported advertises as the v1 and v2 Solana fee payers
	x402CdpTestFeePayerV1 = "SynthFeePayerVone" + strings.Repeat("1", 26)
	x402CdpTestFeePayerV2 = "SynthFeePayerVtwo" + strings.Repeat("1", 26)
)

// The static bearer the server used to send. x402.yml documents written for
// the old shape still carry it; it must never reach the facilitator.
const x402CdpTestStaticBearer = "synthetic-static-bearer"

// A CDP secret API key: the secret in the form the CDP portal issues it, and
// the public half a facilitator verifies with.
type x402CdpTestKey struct {
	id     string
	secret string
	public crypto.PublicKey
	alg    string
}

// An Ed25519 key, CDP's default: the secret is base64 of the 64-byte private
// key.
func newX402CdpTestEd25519Key(t testing.TB) x402CdpTestKey {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return x402CdpTestKey{
		id:     "00000000-0000-4000-8000-0000000000e1",
		secret: base64.StdEncoding.EncodeToString(private),
		public: public,
		alg:    jwt.SigningMethodEdDSA.Alg(),
	}
}

// An ECDSA P-256 key: the secret is a SEC1 "EC PRIVATE KEY" PEM.
func newX402CdpTestEcKey(t testing.TB) x402CdpTestKey {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return x402CdpTestKey{
		id:     "organizations/00000000-0000-4000-8000-0000000000a1/apiKeys/00000000-0000-4000-8000-0000000000e2",
		secret: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})),
		public: &private.PublicKey,
		alg:    jwt.SigningMethodES256.Alg(),
	}
}

// One request the fake facilitator received, and what its JWT check found.
type x402CdpTestRequest struct {
	method        string
	path          string
	authorization string
	jwtErr        error
	claims        jwt.MapClaims
	header        map[string]any
	body          []byte
}

// A fake CDP facilitator. It checks each request's JWT against the key's
// public half and the request's own method, host and path, and answers from
// its fields.
type x402CdpTestFacilitator struct {
	t      testing.TB
	key    x402CdpTestKey
	server *httptest.Server
	// The facilitator lives under a path, as CDP's does. The path starts with
	// a random segment so no two fakes share a url, even when the OS reuses
	// a port: the server caches /supported per facilitator url.
	basePath string
	// the base url to configure
	url string

	// guards requests and the answer fields below; tests change the answers
	// between requests with set
	mutex    sync.Mutex
	requests []x402CdpTestRequest

	// when set, a request whose JWT does not check out is answered 401, as
	// CDP answers it
	requireJwt bool

	supportedStatus int
	supportedBody   string
	verifyStatus    int
	verifyBody      string
	settleStatus    int
	settleBody      string
}

const x402CdpTestBasePath = "/platform/v2/x402"

func newX402CdpTestFacilitator(t testing.TB, key x402CdpTestKey) *x402CdpTestFacilitator {
	t.Helper()
	f := &x402CdpTestFacilitator{
		t:               t,
		key:             key,
		requireJwt:      true,
		supportedStatus: http.StatusOK,
		supportedBody: fmt.Sprintf(`{"kinds":[
			{"x402Version":2,"scheme":"exact","network":"eip155:8453"},
			{"x402Version":2,"scheme":"exact","network":"solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp","extra":{"feePayer":%q}},
			{"x402Version":1,"scheme":"exact","network":"base"},
			{"x402Version":1,"scheme":"exact","network":"solana","extra":{"feePayer":%q}}
		],"extensions":[],"signers":{}}`, x402CdpTestFeePayerV2, x402CdpTestFeePayerV1),
		verifyStatus: http.StatusOK,
		verifyBody:   `{"isValid":true,"payer":"0x2222222222222222222222222222222222222222"}`,
		settleStatus: http.StatusOK,
		settleBody: `{"success":true,"transaction":"0x` + strings.Repeat("5e", 32) +
			`","network":"base","payer":"0x2222222222222222222222222222222222222222"}`,
	}
	unique := make([]byte, 8)
	if _, err := rand.Read(unique); err != nil {
		t.Fatal(err)
	}
	f.basePath = "/fake-" + hex.EncodeToString(unique) + x402CdpTestBasePath
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.url = f.server.URL + f.basePath
	return f
}

// set changes how the fake answers, between requests.
func (self *x402CdpTestFacilitator) set(update func(*x402CdpTestFacilitator)) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	update(self)
}

func (self *x402CdpTestFacilitator) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	claims, header, jwtErr := self.checkJwt(r)

	self.mutex.Lock()
	self.requests = append(self.requests, x402CdpTestRequest{
		method:        r.Method,
		path:          r.URL.Path,
		authorization: r.Header.Get("Authorization"),
		jwtErr:        jwtErr,
		claims:        claims,
		header:        header,
		body:          body,
	})
	requireJwt := self.requireJwt
	status, answer := http.StatusNotFound, `{"errorType":"not_found"}`
	switch {
	case r.Method == http.MethodGet && r.URL.Path == self.basePath+"/supported":
		status, answer = self.supportedStatus, self.supportedBody
	case r.Method == http.MethodPost && r.URL.Path == self.basePath+"/verify":
		status, answer = self.verifyStatus, self.verifyBody
	case r.Method == http.MethodPost && r.URL.Path == self.basePath+"/settle":
		status, answer = self.settleStatus, self.settleBody
	}
	self.mutex.Unlock()

	if requireJwt && jwtErr != nil {
		http.Error(w, `{"errorType":"unauthorized","errorMessage":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(answer))
}

// checkJwt verifies the bearer token as CDP does: signed by the API key,
// issued by "cdp" for the key id, unexpired, and scoped by its uris claim to
// exactly this request's method, host and path.
func (self *x402CdpTestFacilitator) checkJwt(r *http.Request) (jwt.MapClaims, map[string]any, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, nil, fmt.Errorf("no bearer token")
	}
	claims := jwt.MapClaims{}
	token, err := jwt.ParseWithClaims(
		raw,
		claims,
		func(*jwt.Token) (any, error) { return self.key.public, nil },
		jwt.WithValidMethods([]string{self.key.alg}),
		jwt.WithIssuer("cdp"),
		jwt.WithSubject(self.key.id),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("bearer is not a valid CDP JWT: %w", err)
	}
	wantUri := fmt.Sprintf("%s %s%s", r.Method, r.Host, r.URL.Path)
	if uris, _ := claims["uris"].([]any); len(uris) != 1 || uris[0] != wantUri {
		return claims, token.Header, fmt.Errorf("uris claim %v, want [%s]", claims["uris"], wantUri)
	}
	iat, _ := claims["iat"].(float64)
	nbf, _ := claims["nbf"].(float64)
	exp, _ := claims["exp"].(float64)
	if nbf != iat || exp-iat != 120 {
		return claims, token.Header, fmt.Errorf("iat %v nbf %v exp %v: want nbf = iat and a 120 s lifetime", iat, nbf, exp)
	}
	if token.Header["kid"] != self.key.id {
		return claims, token.Header, fmt.Errorf("kid %v, want the key id", token.Header["kid"])
	}
	nonce, _ := token.Header["nonce"].(string)
	if nonceBytes, err := hex.DecodeString(nonce); err != nil || len(nonceBytes) != 16 {
		return claims, token.Header, fmt.Errorf("nonce %q is not 16 random bytes in hex", nonce)
	}
	return claims, token.Header, nil
}

func (self *x402CdpTestFacilitator) received() []x402CdpTestRequest {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]x402CdpTestRequest(nil), self.requests...)
}

func (self *x402CdpTestFacilitator) receivedOn(path string) []x402CdpTestRequest {
	matching := []x402CdpTestRequest{}
	for _, request := range self.received() {
		if request.path == self.basePath+path {
			matching = append(matching, request)
		}
	}
	return matching
}

// x402CdpTestConfig parses an x402.yml document, as the vault loader does.
func x402CdpTestConfig(t testing.TB, document map[string]any) *X402Config {
	t.Helper()
	encoded, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	c := &X402Config{}
	if err := yaml.Unmarshal(encoded, c); err != nil {
		t.Fatal(err)
	}
	return c
}

// x402CdpTestDocument is a complete x402.yml for a CDP facilitator, quoting
// the given networks.
func x402CdpTestDocument(facilitatorUrl string, key x402CdpTestKey, networks ...string) map[string]any {
	payTo := map[string]any{}
	for _, network := range networks {
		switch network {
		case "base":
			payTo[network] = x402CdpTestPayToBase
		case "solana":
			payTo[network] = x402CdpTestPayToSolana
		default:
			payTo[network] = "0x" + strings.Repeat("c3", 20)
		}
	}
	networkList := []any{}
	for _, network := range networks {
		networkList = append(networkList, network)
	}
	return map[string]any{
		"enabled": true,
		"facilitator": map[string]any{
			"url":            facilitatorUrl,
			"api_key":        x402CdpTestStaticBearer,
			"api_key_id":     key.id,
			"api_key_secret": key.secret,
		},
		"pay_to":          payTo,
		"networks":        networkList,
		"asset":           "usdc",
		"skus":            map[string]any{"pro_1month": map[string]any{}, "data_1tib": map[string]any{}},
		"max_payment_usd": 100.0,
		"receipt":         map[string]any{"enabled": false},
	}
}

// x402CdpTestPrices prices the skus whatever this environment's pro.yml says.
func x402CdpTestPrices(t testing.TB) {
	t.Cleanup(model.Testing_SetSkuPrices(5, 600*model.Gib, []model.ProDataCodeSku{{Data: model.Tib, PriceUsd: 3}}))
}

// A signed x402 v1 exact payment on an EVM network, as the X-PAYMENT header
// carries it (base64 of the PaymentPayload JSON), and its decoded JSON.
func x402CdpTestEvmPayment(t testing.TB, network string, payTo string, value string) (string, map[string]any) {
	t.Helper()
	return x402CdpTestPayment(t, map[string]any{
		"x402Version": 1,
		"scheme":      "exact",
		"network":     network,
		"payload": map[string]any{
			"signature": "0x" + strings.Repeat("ab", 65),
			"authorization": map[string]any{
				"from":        "0x2222222222222222222222222222222222222222",
				"to":          payTo,
				"value":       value,
				"validAfter":  "1740672089",
				"validBefore": "1740672154",
				"nonce":       "0x" + strings.Repeat("3c", 32),
			},
		},
	})
}

func x402CdpTestPayment(t testing.TB, payment map[string]any) (string, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payment)
	if err != nil {
		t.Fatal(err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded), decoded
}

// x402CdpTestTerms renders quoted terms as JSON objects, the form an agent
// reads them in.
func x402CdpTestTerms(t testing.TB, paymentRequired *X402PaymentRequired) map[string]map[string]any {
	t.Helper()
	terms := map[string]map[string]any{}
	for _, accept := range paymentRequired.Accepts {
		encoded, err := json.Marshal(accept)
		if err != nil {
			t.Fatal(err)
		}
		term := map[string]any{}
		if err := json.Unmarshal(encoded, &term); err != nil {
			t.Fatal(err)
		}
		terms[accept.Network] = term
	}
	return terms
}

// Every facilitator request carries a CDP JWT that the facilitator accepts:
// signed by the API key (both key types CDP issues), issued by "cdp" for the
// key id, valid for 120 s, and scoped to exactly that request's method, host
// and path. A fresh JWT is minted per request, so verify's token cannot be
// replayed against settle. The static bearer of the old config shape is never
// sent, and the key secret never leaves the process.
//
// Before this change every request carried `Bearer <api_key>`, which CDP
// rejects, so no payment could be verified.
func TestX402FacilitatorRequestsCarryCdpJwt(t *testing.T) {
	x402CdpTestPrices(t)
	for name, newKey := range map[string]func(testing.TB) x402CdpTestKey{
		"ed25519":     newX402CdpTestEd25519Key,
		"ecdsa p-256": newX402CdpTestEcKey,
	} {
		t.Run(name, func(t *testing.T) {
			key := newKey(t)
			facilitator := newX402CdpTestFacilitator(t, key)
			c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base"))
			SetX402Config(c)
			t.Cleanup(func() { SetX402Config(nil) })

			t.Setenv("WARP_DOMAIN", "x402.example")
			paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
			if err != nil {
				t.Fatalf("quote: %v", err)
			}
			payment, _ := x402CdpTestEvmPayment(t, "base", x402CdpTestPayToBase, paymentRequired.Accepts[0].MaxAmountRequired)

			verifyResponse, err := X402Verify(context.Background(), payment, paymentRequired.Accepts[0])
			if err != nil {
				t.Fatalf("verify: %v (facilitator saw %s)", err, x402CdpTestDescribe(facilitator.received()))
			}
			if !verifyResponse.IsValid {
				t.Fatalf("verify answered invalid: %+v", verifyResponse)
			}
			settleResponse, err := X402Settle(context.Background(), payment, paymentRequired.Accepts[0])
			if err != nil {
				t.Fatalf("settle: %v (facilitator saw %s)", err, x402CdpTestDescribe(facilitator.received()))
			}
			if !settleResponse.Success {
				t.Fatalf("settle answered failure: %+v", settleResponse)
			}

			requests := facilitator.received()
			if len(requests) != 2 {
				t.Fatalf("facilitator saw %d requests, want verify and settle: %s", len(requests), x402CdpTestDescribe(requests))
			}
			for _, request := range requests {
				if request.jwtErr != nil {
					t.Errorf("%s %s: %v", request.method, request.path, request.jwtErr)
				}
				if strings.Contains(request.authorization, x402CdpTestStaticBearer) {
					t.Errorf("%s %s sent the static api_key", request.method, request.path)
				}
				if strings.Contains(request.authorization+string(request.body), strings.TrimSpace(key.secret)) {
					t.Errorf("%s %s carried the key secret", request.method, request.path)
				}
			}
			if requests[0].authorization == requests[1].authorization {
				t.Error("verify and settle carried the same JWT; each request needs its own")
			}
			if requests[0].header["nonce"] == requests[1].header["nonce"] {
				t.Error("verify and settle JWTs share a nonce")
			}
		})
	}
}

func x402CdpTestDescribe(requests []x402CdpTestRequest) string {
	parts := []string{}
	for _, request := range requests {
		bearer := request.authorization
		if 24 < len(bearer) {
			bearer = bearer[:24] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s %s auth=%q jwt=%v", request.method, request.path, bearer, request.jwtErr))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// The terms quote the token's address, never its symbol, and the EVM exact
// scheme's extra: the EIP-712 domain the client signs the EIP-3009
// authorization against. Base mainnet USDC's domain is {"USD Coin", "2"}.
//
// Before this change the terms quoted asset "usdc" and no extra: a client
// derives the EIP-712 domain's verifyingContract from `asset` and its name and
// version from `extra`, so no signature could have verified.
func TestX402QuotesTheTokenAddressAndEip712Domain(t *testing.T) {
	x402CdpTestPrices(t)
	t.Setenv("WARP_DOMAIN", "x402.example")
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base"))

	paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
	if err != nil {
		t.Fatal(err)
	}
	terms := x402CdpTestTerms(t, paymentRequired)
	base := terms["base"]
	if base == nil {
		t.Fatalf("base was not quoted: %v", terms)
	}
	for field, want := range map[string]any{
		"scheme":            "exact",
		"network":           "base",
		"asset":             x402CdpTestUsdcBase,
		"payTo":             x402CdpTestPayToBase,
		"maxAmountRequired": "3000000",
	} {
		if base[field] != want {
			t.Errorf("base %s = %v, want %v", field, base[field], want)
		}
	}
	if extra := base["extra"]; !reflect.DeepEqual(extra, map[string]any{"name": "USD Coin", "version": "2"}) {
		t.Errorf("base extra = %v, want the USDC EIP-712 domain {name: USD Coin, version: 2}", extra)
	}
	// quoting base needs nothing from the facilitator
	if requests := facilitator.received(); len(requests) != 0 {
		t.Errorf("quoting base called the facilitator: %s", x402CdpTestDescribe(requests))
	}
}

// x402 v1 requires `resource` to be a URL, and v1 clients validate the terms
// before paying (the legacy fetch and axios clients parse each term with a zod
// schema whose resource is z.string().url(), which refuses a bare path). A
// path served by this API is quoted under the API's public origin; a resource
// that is already a URL is left alone. Without a domain to put in front of the
// path, nothing is quoted.
//
// Before this change the terms quoted "/x402/purchase", which a v1 client
// refuses before it pays.
func TestX402QuotesAnAbsoluteResource(t *testing.T) {
	x402CdpTestPrices(t)
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base"))

	t.Setenv("WARP_DOMAIN", "x402.example")
	for resource, want := range map[string]string{
		"/x402/purchase":       "https://api.x402.example/x402/purchase",
		"/network/auth-client": "https://api.x402.example/network/auth-client",
		"mcp:fetch":            "mcp:fetch",
	} {
		paymentRequired, err := x402PaymentRequiredForConfig(c, resource, X402SkuData1Tib, "")
		if err != nil {
			t.Errorf("%s: %v", resource, err)
			continue
		}
		if got := paymentRequired.Accepts[0].Resource; got != want {
			t.Errorf("resource %q quoted as %q, want %q", resource, got, want)
		}
	}

	t.Setenv("WARP_DOMAIN", "")
	if paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, ""); err == nil {
		t.Errorf("quoted %q with no domain to make it a URL", paymentRequired.Accepts[0].Resource)
	}
}

// Solana's exact scheme needs the facilitator's fee payer in extra.feePayer:
// the client builds the transaction with that account paying the fee, and
// the facilitator co-signs as it. The fee payer comes from the facilitator's
// GET /supported (the v1 exact solana kind, not the v2 one), requested with
// its own JWT and cached between quotes. The asset is the USDC mint.
//
// Before this change Solana was quoted with no extra and the symbol "usdc",
// which no Solana client can pay.
func TestX402QuotesSolanaWithTheFacilitatorFeePayer(t *testing.T) {
	x402CdpTestPrices(t)
	t.Setenv("WARP_DOMAIN", "x402.example")
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base", "solana"))

	for i := 0; i < 3; i += 1 {
		paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
		if err != nil {
			t.Fatal(err)
		}
		terms := x402CdpTestTerms(t, paymentRequired)
		solana := terms["solana"]
		if solana == nil {
			t.Fatalf("solana was not quoted: %v", terms)
		}
		if solana["asset"] != x402CdpTestUsdcSolana || solana["payTo"] != x402CdpTestPayToSolana {
			t.Errorf("solana asset %v payTo %v, want the USDC mint and the deposit address", solana["asset"], solana["payTo"])
		}
		if extra := solana["extra"]; !reflect.DeepEqual(extra, map[string]any{"feePayer": x402CdpTestFeePayerV1}) {
			t.Errorf("solana extra = %v, want the v1 fee payer from /supported", extra)
		}
		if terms["base"] == nil {
			t.Errorf("base was not quoted beside solana: %v", terms)
		}
	}

	supported := facilitator.receivedOn("/supported")
	if len(supported) != 1 {
		t.Fatalf("/supported was requested %d times for three quotes, want once (cached)", len(supported))
	}
	if supported[0].method != http.MethodGet || supported[0].jwtErr != nil {
		t.Errorf("/supported request %s: %v", supported[0].method, supported[0].jwtErr)
	}
}

// Solana is quoted only with a fee payer the facilitator advertised. When
// /supported fails, or names no v1 Solana fee payer, the terms leave Solana
// out (Base is still quoted), and a config that quotes only Solana quotes
// nothing at all rather than a term no client can pay.
//
// Before this change Solana was always quoted, without a fee payer.
func TestX402DoesNotQuoteSolanaWithoutAFeePayer(t *testing.T) {
	x402CdpTestPrices(t)
	t.Setenv("WARP_DOMAIN", "x402.example")
	for name, breakIt := range map[string]func(*x402CdpTestFacilitator){
		"supported fails": func(f *x402CdpTestFacilitator) {
			f.supportedStatus = http.StatusInternalServerError
			f.supportedBody = `{"errorType":"internal_server_error"}`
		},
		"only a v2 solana kind": func(f *x402CdpTestFacilitator) {
			f.supportedBody = fmt.Sprintf(`{"kinds":[{"x402Version":1,"scheme":"exact","network":"base"},
				{"x402Version":2,"scheme":"exact","network":"solana:5eykt4UsFv8P8NJdTREpY1vzqKqZKvdp","extra":{"feePayer":%q}}]}`,
				x402CdpTestFeePayerV2)
		},
		"a v1 solana kind with no fee payer": func(f *x402CdpTestFacilitator) {
			f.supportedBody = `{"kinds":[{"x402Version":1,"scheme":"exact","network":"solana"}]}`
		},
		"a fee payer that is not an address": func(f *x402CdpTestFacilitator) {
			f.supportedBody = `{"kinds":[{"x402Version":1,"scheme":"exact","network":"solana","extra":{"feePayer":"0x0"}}]}`
		},
		"supported is not json": func(f *x402CdpTestFacilitator) {
			f.supportedBody = `<html>maintenance</html>`
		},
	} {
		t.Run(name, func(t *testing.T) {
			key := newX402CdpTestEd25519Key(t)
			facilitator := newX402CdpTestFacilitator(t, key)
			facilitator.set(breakIt)

			c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base", "solana"))
			paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
			if err != nil {
				t.Fatal(err)
			}
			terms := x402CdpTestTerms(t, paymentRequired)
			if terms["solana"] != nil {
				t.Errorf("solana was quoted with no fee payer: %v", terms["solana"])
			}
			if terms["base"] == nil {
				t.Errorf("base was not quoted: %v", terms)
			}

			solanaOnly := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "solana"))
			if paymentRequired, err := x402PaymentRequiredForConfig(solanaOnly, "/x402/purchase", X402SkuData1Tib, ""); err == nil {
				t.Errorf("a solana-only config quoted %v with no fee payer", x402CdpTestTerms(t, paymentRequired))
			}
		})
	}
}

// Verify and settle send the x402 v1 request body: x402Version 1, the decoded
// PaymentPayload as a JSON object, and the terms we quoted
// (specs/x402-specification-v1.md 7.1). The payload's own fields reach the
// facilitator exactly as the client signed them.
//
// Before this change paymentPayload was the X-PAYMENT header string, which
// the facilitator's schema (an object) refuses.
func TestX402VerifyAndSettleSendThePaymentPayloadAsAnObject(t *testing.T) {
	x402CdpTestPrices(t)
	t.Setenv("WARP_DOMAIN", "x402.example")
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	// this test is about the body, whatever the auth
	facilitator.set(func(f *x402CdpTestFacilitator) { f.requireJwt = false })
	c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base"))
	SetX402Config(c)
	t.Cleanup(func() { SetX402Config(nil) })

	paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
	if err != nil {
		t.Fatal(err)
	}
	requirements := paymentRequired.Accepts[0]
	payment, decoded := x402CdpTestEvmPayment(t, "base", x402CdpTestPayToBase, requirements.MaxAmountRequired)

	if _, err := X402Verify(context.Background(), payment, requirements); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := X402Settle(context.Background(), payment, requirements); err != nil {
		t.Fatalf("settle: %v", err)
	}

	wantRequirements := x402CdpTestTerms(t, paymentRequired)["base"]
	for _, path := range []string{"/verify", "/settle"} {
		requests := facilitator.receivedOn(path)
		if len(requests) != 1 {
			t.Fatalf("%s requested %d times", path, len(requests))
		}
		body := map[string]any{}
		if err := json.Unmarshal(requests[0].body, &body); err != nil {
			t.Fatalf("%s body is not json: %v", path, err)
		}
		if body["x402Version"] != float64(1) {
			t.Errorf("%s x402Version = %v, want 1", path, body["x402Version"])
		}
		if !reflect.DeepEqual(body["paymentPayload"], decoded) {
			t.Errorf("%s paymentPayload = %#v, want the decoded payment object %#v", path, body["paymentPayload"], decoded)
		}
		if !reflect.DeepEqual(body["paymentRequirements"], wantRequirements) {
			t.Errorf("%s paymentRequirements = %v, want the quoted terms %v", path, body["paymentRequirements"], wantRequirements)
		}
	}
}

// A payment that is not an x402 v1 exact PaymentPayload is refused before
// anything reaches the facilitator: not base64, not JSON, another protocol
// version or scheme, no inner payload, or a network other than the terms'.
//
// Before this change any header string was forwarded as the payload.
func TestX402RefusesAMalformedPaymentBeforeTheFacilitator(t *testing.T) {
	x402CdpTestPrices(t)
	t.Setenv("WARP_DOMAIN", "x402.example")
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	facilitator.set(func(f *x402CdpTestFacilitator) { f.requireJwt = false })
	c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base"))
	SetX402Config(c)
	t.Cleanup(func() { SetX402Config(nil) })

	paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
	if err != nil {
		t.Fatal(err)
	}
	requirements := paymentRequired.Accepts[0]

	valid := func() map[string]any {
		_, decoded := x402CdpTestEvmPayment(t, "base", x402CdpTestPayToBase, requirements.MaxAmountRequired)
		return decoded
	}
	encode := func(payment map[string]any) string {
		encoded, _ := x402CdpTestPayment(t, payment)
		return encoded
	}
	for name, payment := range map[string]string{
		"not base64":           "not base64 at all!",
		"not json":             base64.StdEncoding.EncodeToString([]byte("not json")),
		"a json string":        base64.StdEncoding.EncodeToString([]byte(`"a string"`)),
		"x402 version 2":       encode(func() map[string]any { p := valid(); p["x402Version"] = 2; return p }()),
		"no version":           encode(func() map[string]any { p := valid(); delete(p, "x402Version"); return p }()),
		"scheme upto":          encode(func() map[string]any { p := valid(); p["scheme"] = "upto"; return p }()),
		"no inner payload":     encode(func() map[string]any { p := valid(); delete(p, "payload"); return p }()),
		"a null inner payload": encode(func() map[string]any { p := valid(); p["payload"] = nil; return p }()),
		"another network":      encode(func() map[string]any { p := valid(); p["network"] = "solana"; return p }()),
	} {
		if _, err := X402Verify(context.Background(), payment, requirements); err == nil {
			t.Errorf("%s: verified", name)
		}
		if _, err := X402Settle(context.Background(), payment, requirements); err == nil {
			t.Errorf("%s: settled", name)
		}
	}
	if requests := facilitator.received(); len(requests) != 0 {
		t.Errorf("malformed payments reached the facilitator: %s", x402CdpTestDescribe(requests))
	}
}

// CDP answers a payment it finds invalid with HTTP 400 and the scheme's
// structured rejection ({isValid: false, invalidReason} from verify,
// {success: false, errorReason} from settle). That is an answer about the
// payment, not a facilitator failure, so it comes back as one and the reason
// reaches the agent. Any other non-2xx answer stays an error, whose body is
// kept out of the response.
//
// Before this change every 400 was a facilitator error, and the agent was told
// only "x402 verify failed".
func TestX402FacilitatorRejectionIsAnAnswer(t *testing.T) {
	x402CdpTestPrices(t)
	t.Setenv("WARP_DOMAIN", "x402.example")
	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	facilitator.set(func(f *x402CdpTestFacilitator) { f.requireJwt = false })
	c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base"))
	SetX402Config(c)
	t.Cleanup(func() { SetX402Config(nil) })

	paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
	if err != nil {
		t.Fatal(err)
	}
	requirements := paymentRequired.Accepts[0]
	payment, _ := x402CdpTestEvmPayment(t, "base", x402CdpTestPayToBase, requirements.MaxAmountRequired)

	facilitator.set(func(f *x402CdpTestFacilitator) {
		f.verifyStatus = http.StatusBadRequest
		f.verifyBody = `{"isValid":false,"invalidReason":"insufficient_funds","invalidMessage":"Insufficient funds","payer":"0x2222222222222222222222222222222222222222"}`
	})
	verifyResponse, err := X402Verify(context.Background(), payment, requirements)
	if err != nil {
		t.Fatalf("a 400 rejection came back as an error: %v", err)
	}
	if verifyResponse.IsValid || verifyResponse.InvalidReason != "insufficient_funds" {
		t.Errorf("verify rejection = %+v, want invalid with insufficient_funds", verifyResponse)
	}

	facilitator.set(func(f *x402CdpTestFacilitator) {
		f.settleStatus = http.StatusBadRequest
		f.settleBody = `{"success":false,"errorReason":"invalid_exact_evm_payload_authorization_valid_before","errorMessage":"expired","payer":"0x2222222222222222222222222222222222222222"}`
	})
	settleResponse, err := X402Settle(context.Background(), payment, requirements)
	if err != nil {
		t.Fatalf("a 400 rejection came back as an error: %v", err)
	}
	if settleResponse.Success || settleResponse.ErrorReason != "invalid_exact_evm_payload_authorization_valid_before" {
		t.Errorf("settle rejection = %+v", settleResponse)
	}

	// a 400 that is not a rejection, and a 500, are facilitator errors
	for _, answer := range []struct {
		status int
		body   string
	}{
		{http.StatusBadRequest, `{"errorType":"invalid_request","errorMessage":"synthetic-facilitator-detail"}`},
		{http.StatusBadRequest, `{"isValid":true}`},
		{http.StatusInternalServerError, `{"isValid":false,"invalidReason":"synthetic-facilitator-detail"}`},
		{http.StatusUnauthorized, `synthetic-facilitator-detail`},
	} {
		facilitator.set(func(f *x402CdpTestFacilitator) { f.verifyStatus, f.verifyBody = answer.status, answer.body })
		if verifyResponse, err := X402Verify(context.Background(), payment, requirements); err == nil {
			t.Errorf("%d %s came back as an answer: %+v", answer.status, answer.body, verifyResponse)
		}
	}
}

// The config is usable only when every piece a real payment needs is there:
// an https facilitator url, a CDP API key id and a secret CDP would accept,
// and for every listed network a token address for the asset and a deposit
// address of that network's form. Tempo, which Stripe takes through MPP
// rather than x402, and networks with no token address make the whole config
// unusable rather than being dropped. The static api_key of the old shape no
// longer authenticates anything.
//
// Before this change a static api_key was enough, any network name was
// accepted, and a deposit address was never checked against its network.
func TestX402ConfigUsableForCdp(t *testing.T) {
	ed25519Key := newX402CdpTestEd25519Key(t)
	ecKey := newX402CdpTestEcKey(t)
	const facilitatorUrl = "https://api.cdp.coinbase.com/platform/v2/x402"

	usable := map[string]map[string]any{
		"base, ed25519 key":         x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"),
		"base, ecdsa key":           x402CdpTestDocument(facilitatorUrl, ecKey, "base"),
		"base and solana":           x402CdpTestDocument(facilitatorUrl, ed25519Key, "base", "solana"),
		"solana":                    x402CdpTestDocument(facilitatorUrl, ed25519Key, "solana"),
		"a url with a trailing /":   withX402CdpTestFacilitator(x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "url", facilitatorUrl+"/"),
		"no static api_key at all":  withX402CdpTestFacilitator(x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "api_key", nil),
		"an unused pay_to network":  withX402CdpTestPayTo(x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "solana", x402CdpTestPayToSolana),
		"an unused pay_to of tempo": withX402CdpTestPayTo(x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "tempo", "0x"+strings.Repeat("c3", 20)),
	}
	for name, document := range usable {
		if !x402ConfigUsable(x402CdpTestConfig(t, document)) {
			t.Errorf("%s: not usable", name)
		}
	}

	unusable := map[string]map[string]any{
		"only the static api_key": withX402CdpTestFacilitator(withX402CdpTestFacilitator(
			x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "api_key_id", nil), "api_key_secret", nil),
		"no api_key_id":     withX402CdpTestFacilitator(x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "api_key_id", ""),
		"no api_key_secret": withX402CdpTestFacilitator(x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "api_key_secret", ""),
		"a secret that is not a key": withX402CdpTestFacilitator(
			x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "api_key_secret", "c2stdGVzdA=="),
		"an EC key with escaped newlines": withX402CdpTestFacilitator(
			x402CdpTestDocument(facilitatorUrl, ecKey, "base"), "api_key_secret", strings.ReplaceAll(ecKey.secret, "\n", `\n`)),
		"an http facilitator": withX402CdpTestFacilitator(
			x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "url", "http://api.cdp.coinbase.com/platform/v2/x402"),
		"a facilitator url with no host": withX402CdpTestFacilitator(
			x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "url", "https:///platform/v2/x402"),
		"no facilitator url":              withX402CdpTestFacilitator(x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "url", ""),
		"tempo":                           x402CdpTestDocument(facilitatorUrl, ed25519Key, "base", "tempo"),
		"a network with no token address": x402CdpTestDocument(facilitatorUrl, ed25519Key, "base", "ethereum"),
		"a testnet":                       x402CdpTestDocument(facilitatorUrl, ed25519Key, "base-sepolia"),
		"an asset with no token address": func() map[string]any {
			document := x402CdpTestDocument(facilitatorUrl, ed25519Key, "base")
			document["asset"] = "usdt"
			return document
		}(),
		"no asset": func() map[string]any {
			document := x402CdpTestDocument(facilitatorUrl, ed25519Key, "base")
			delete(document, "asset")
			return document
		}(),
		"a solana address under base": withX402CdpTestPayTo(
			x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "base", x402CdpTestPayToSolana),
		"an evm address under solana": withX402CdpTestPayTo(
			x402CdpTestDocument(facilitatorUrl, ed25519Key, "solana"), "solana", x402CdpTestPayToBase),
		"a short evm address": withX402CdpTestPayTo(
			x402CdpTestDocument(facilitatorUrl, ed25519Key, "base"), "base", "0x1234"),
		"no deposit address for a listed network": withX402CdpTestPayTo(
			x402CdpTestDocument(facilitatorUrl, ed25519Key, "base", "solana"), "solana", ""),
	}
	for name, document := range unusable {
		if x402ConfigUsable(x402CdpTestConfig(t, document)) {
			t.Errorf("%s: usable", name)
		}
	}
}

func withX402CdpTestFacilitator(document map[string]any, field string, value any) map[string]any {
	facilitator := document["facilitator"].(map[string]any)
	if value == nil {
		delete(facilitator, field)
	} else {
		facilitator[field] = value
	}
	return document
}

func withX402CdpTestPayTo(document map[string]any, network string, address string) map[string]any {
	document["pay_to"].(map[string]any)[network] = address
	return document
}

// After a payment settles, X-PAYMENT-RESPONSE carries the SettlementResponse
// as base64 JSON, {success, transaction, network, payer}, on both entry points
// that settle: POST /x402/purchase and the inline upgrade on a gated request
// (specs/transports-v1/http.md, "Settlement Response Delivery"). v1 clients
// read it with base64 decode and JSON.parse.
//
// Before this change the header was our purchase result as raw JSON, which a
// client's base64 decode refuses.
func TestX402PaymentResponseHeaderIsTheSettlementResponse(t *testing.T) {
	_, grants := installX402SettledNotGrantedFakes(t)
	stripe := newX402StripeFixture(t)
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		x402CdpTestPrices(tb)
		key := newX402CdpTestEd25519Key(tb)
		facilitator := newX402CdpTestFacilitator(tb, key)
		// this test is about the answer to the agent, whatever the auth
		facilitator.set(func(f *x402CdpTestFacilitator) { f.requireJwt = false })
		c := x402CdpTestConfig(tb, x402CdpTestDocument(facilitator.url, key, "base"))
		SetX402Config(c)
		tb.Cleanup(func() { SetX402Config(nil) })

		for _, entry := range []struct {
			name        string
			skuId       string
			transaction string
			// reports whether the request went on to be served
			serve func(http.ResponseWriter, *http.Request) bool
		}{
			{"purchase", X402SkuData1Tib, "0x" + strings.Repeat("a1", 32), func(w http.ResponseWriter, r *http.Request) bool {
				X402PurchaseHandler(w, r)
				return true
			}},
			{"inline upgrade", X402SkuProMonth, "0x" + strings.Repeat("b2", 32), X402SettleInlineUpgrade},
		} {
			quote, err := X402PaymentRequiredFor("/x402/purchase", entry.skuId, "")
			if err != nil {
				tb.Fatal(err)
			}
			// a distinct settle transaction per entry point, so the second is
			// a new purchase and not a retry of the first
			transaction := entry.transaction
			facilitator.set(func(f *x402CdpTestFacilitator) {
				f.settleBody = `{"success":true,"transaction":"` + transaction +
					`","network":"base","payer":"0x2222222222222222222222222222222222222222"}`
			})
			payment, _ := x402CdpTestEvmPayment(tb, "base", x402CdpTestPayToBase, quote.Accepts[0].MaxAmountRequired)

			// a network per entry point; network names are unique
			networkId, userId := server.NewId(), server.NewId()
			networkName := "x402cdpheader" + strings.ReplaceAll(entry.name, " ", "")
			model.Testing_CreateNetwork(tb.Context(), networkId, networkName, userId)
			request := httptest.NewRequest(http.MethodPost, "/x402/purchase", strings.NewReader(fmt.Sprintf(`{"sku_id":%q}`, entry.skuId)))
			request.RemoteAddr = "192.0.2.53:4500"
			request.Header.Set("Authorization", "Bearer "+session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign())
			request.Header.Set(X402PaymentHeader, payment)
			w := httptest.NewRecorder()
			if !entry.serve(w, request) || w.Code != http.StatusOK {
				tb.Fatalf("%s: answered %d %q", entry.name, w.Code, w.Body.String())
			}

			header := w.Header().Get(X402PaymentResponseHeader)
			decoded, err := base64.StdEncoding.DecodeString(header)
			if err != nil {
				tb.Fatalf("%s: %s is not base64: %q", entry.name, X402PaymentResponseHeader, header)
			}
			settlement := map[string]any{}
			if err := json.Unmarshal(decoded, &settlement); err != nil {
				tb.Fatalf("%s: %s is not base64 json: %v", entry.name, X402PaymentResponseHeader, err)
			}
			want := map[string]any{
				"success":     true,
				"transaction": transaction,
				"network":     "base",
				"payer":       "0x2222222222222222222222222222222222222222",
			}
			if !reflect.DeepEqual(settlement, want) {
				tb.Errorf("%s: settlement response %v, want %v", entry.name, settlement, want)
			}
			if _, granted := grants.granted[transaction]; !granted {
				tb.Errorf("%s: settled transaction %s was not granted", entry.name, transaction)
			}
		}
		if len(stripe.recorded()) != 2 {
			tb.Errorf("stripe recorded %d payments, want 2", len(stripe.recorded()))
		}
	})
}
