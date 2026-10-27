package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coinbase/cdp-sdk/go/auth"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The x402 facilitator: the CDP facilitator, and the x402 v1 wire format.
//
// Stripe settles x402 mainnet payments through the Coinbase Developer Platform
// facilitator (docs.stripe.com/payments/machine/x402). Its x402 API lives
// under https://api.cdp.coinbase.com/platform/v2/x402: POST /verify, POST
// /settle and GET /supported. Every request authenticates with a CDP JWT, a
// bearer token signed by the CDP secret API key, valid for 120 s, whose `uris`
// claim scopes it to exactly one method, host and path. A token is minted for
// each request, the way the CDP SDKs do it (coinbase/x402
// go/legacy/pkg/coinbasefacilitator/cdp.go CreateAuthHeader, which calls
// github.com/coinbase/cdp-sdk/go/auth GenerateJWT, the package used here).

const (
	// verify and settle: settle waits for the chain
	x402FacilitatorTimeout = 30 * time.Second
	// /supported is fetched on the quote path, so it gets less
	x402SupportedTimeout = 10 * time.Second
	// the most of a facilitator answer that is read
	x402FacilitatorMaxBody = 1 << 20
)

// x402FacilitatorConfigured reports whether the facilitator settings are
// filled in at all. x402ConfigProblem checks them properly at load; this is
// the floor every facilitator request checks again before any http.
func x402FacilitatorConfigured(c *X402Config) bool {
	return c.Facilitator.Url != "" && c.Facilitator.ApiKeyId != "" && c.Facilitator.ApiKeySecret != ""
}

var errX402FacilitatorNotConfigured = errors.New(
	"x402 facilitator is not configured (facilitator.url, facilitator.api_key_id, facilitator.api_key_secret)",
)

// x402FacilitatorEndpoint is the url of one facilitator endpoint ("/verify",
// "/settle", "/supported") under the configured base url.
func x402FacilitatorEndpoint(c *X402Config, endpoint string) (*url.URL, error) {
	endpointUrl, err := url.Parse(strings.TrimRight(c.Facilitator.Url, "/") + endpoint)
	if err != nil || endpointUrl.Scheme == "" || endpointUrl.Host == "" {
		return nil, errors.New("facilitator.url is not an absolute url")
	}
	return endpointUrl, nil
}

// x402FacilitatorAuthorization is the Authorization header for one facilitator
// request: a fresh CDP JWT for exactly this method, host and path.
//
// The SDK accepts both kinds of CDP secret API key: Ed25519 (the default,
// base64 of the 64-byte private key) and ECDSA (an "EC PRIVATE KEY" PEM). Its
// errors name what is wrong with a key, never the key.
func x402FacilitatorAuthorization(c *X402Config, method string, endpointUrl *url.URL) (string, error) {
	token, err := auth.GenerateJWT(auth.JwtOptions{
		KeyID:         c.Facilitator.ApiKeyId,
		KeySecret:     c.Facilitator.ApiKeySecret,
		RequestMethod: method,
		RequestHost:   endpointUrl.Host,
		RequestPath:   endpointUrl.Path,
	})
	if err != nil {
		return "", fmt.Errorf("could not sign the facilitator request: %w", err)
	}
	return "Bearer " + token, nil
}

// x402FacilitatorDo sends one request to the facilitator and returns the
// status and body of its answer. Only a failure to get an answer is an error
// here; the caller judges the status.
func x402FacilitatorDo(
	ctx context.Context,
	c *X402Config,
	method string,
	endpoint string,
	request any,
	timeout time.Duration,
) (int, []byte, error) {
	// Refuse before any http. Every entry point is behind X402Enabled(), and
	// the loader turns x402 off when these are blank, but this is the function
	// that VERIFIES AND SETTLES MONEY, so it does not rely on a caller having
	// checked. With a blank url the request would go out as a relative path
	// and fail with "unsupported protocol scheme", a confusing way to learn
	// that a payment was never settled.
	if !x402FacilitatorConfigured(c) {
		return 0, nil, errX402FacilitatorNotConfigured
	}
	endpointUrl, err := x402FacilitatorEndpoint(c, endpoint)
	if err != nil {
		return 0, nil, err
	}

	var body io.Reader
	if request != nil {
		requestBody, err := json.Marshal(request)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(requestBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpointUrl.String(), body)
	if err != nil {
		return 0, nil, err
	}
	if request != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	authorization, err := x402FacilitatorAuthorization(c, method, endpointUrl)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", authorization)

	client := server.NewHttpClient(timeout)
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()

	resBody, err := io.ReadAll(io.LimitReader(res.Body, x402FacilitatorMaxBody))
	if err != nil {
		return 0, nil, err
	}
	return res.StatusCode, resBody, nil
}

// x402FacilitatorAnswer is a decoded /verify or /settle answer.
type x402FacilitatorAnswer interface {
	// rejected reports whether the answer is the scheme's structured
	// rejection of the payment
	rejected() bool
}

func (self *X402VerifyResponse) rejected() bool {
	return !self.IsValid && self.InvalidReason != ""
}

func (self *X402SettleResponse) rejected() bool {
	return !self.Success && self.ErrorReason != ""
}

// x402FacilitatorPost posts a verify or settle request and decodes the answer.
//
// A 2xx is the answer. So is a 400 that carries the scheme's rejection: CDP
// answers a payment it finds invalid with 400 and {isValid: false,
// invalidReason} from /verify, {success: false, errorReason} from /settle,
// which is a verdict on the payment rather than a failure to reach one.
// Anything else is an error quoting the facilitator's answer, for the log and
// never for the agent.
func x402FacilitatorPost(ctx context.Context, path string, request any, answer x402FacilitatorAnswer) error {
	status, body, err := x402FacilitatorDo(ctx, X402(), http.MethodPost, path, request, x402FacilitatorTimeout)
	if err != nil {
		return err
	}
	switch {
	case 200 <= status && status < 300:
		return json.Unmarshal(body, answer)
	case status == http.StatusBadRequest:
		if json.Unmarshal(body, answer) == nil && answer.rejected() {
			return nil
		}
	}
	return fmt.Errorf("facilitator %s: %d %s", path, status, string(body))
}

// ----- the payment the agent signed -----

// x402PaymentPayload is the x402 v1 PaymentPayload. The X-PAYMENT header
// carries it as base64 of its JSON (specs/x402-specification-v1.md 5.2,
// specs/transports-v1/http.md), and /verify and /settle take it as a JSON
// object (spec 7.1). Payload is the scheme's signed data (an EIP-3009
// authorization and its signature on EVM, a partially signed transaction on
// Solana), passed through as the client sent it.
type x402PaymentPayload struct {
	X402Version int             `json:"x402Version"`
	Scheme      string          `json:"scheme"`
	Network     string          `json:"network"`
	Payload     json.RawMessage `json:"payload"`
}

// x402DecodePayment reads an X-PAYMENT header: an x402 v1 exact payment on a
// named network, with a payload object. Anything else is refused here, before
// it can reach the facilitator.
func x402DecodePayment(payment string) (*x402PaymentPayload, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payment))
	if err != nil {
		return nil, errors.New("x402 payment is not base64")
	}
	paymentPayload := &x402PaymentPayload{}
	if err := json.Unmarshal(raw, paymentPayload); err != nil {
		return nil, errors.New("x402 payment is not an x402 PaymentPayload")
	}
	if paymentPayload.X402Version != X402Version {
		return nil, fmt.Errorf(
			"x402 payment is for x402 version %d; this server takes version %d",
			paymentPayload.X402Version, X402Version,
		)
	}
	if paymentPayload.Scheme != "exact" {
		return nil, errors.New("x402 payment scheme is not exact")
	}
	if paymentPayload.Network == "" {
		return nil, errors.New("x402 payment names no network")
	}
	if !bytes.HasPrefix(bytes.TrimSpace(paymentPayload.Payload), []byte("{")) {
		return nil, errors.New("x402 payment carries no payload")
	}
	return paymentPayload, nil
}

// x402DecodePaymentFor decodes a payment and checks it was made for these
// terms' scheme and network.
func x402DecodePaymentFor(payment string, requirements X402Accept) (*x402PaymentPayload, error) {
	paymentPayload, err := x402DecodePayment(payment)
	if err != nil {
		return nil, err
	}
	if paymentPayload.Scheme != requirements.Scheme || paymentPayload.Network != requirements.Network {
		return nil, fmt.Errorf(
			"x402 payment is for %s on %s, not the quoted %s on %s",
			paymentPayload.Scheme, paymentPayload.Network, requirements.Scheme, requirements.Network,
		)
	}
	return paymentPayload, nil
}

// ----- the Solana fee payer, from /supported -----

// The SVM exact scheme quotes the facilitator's fee payer as extra.feePayer
// (specs/schemes/exact/scheme_exact_svm.md): the client builds its
// transaction with that account paying the network fee, and the facilitator
// co-signs as it at settlement. CDP advertises it in GET /supported, on the
// v1 exact solana kind. It changes rarely, so the answer is cached per
// facilitator, at most one fetch is in flight, and a network whose fee payer
// is not known is not quoted.
const (
	// how long a fetched answer is used before it is fetched again
	x402SupportedTtl = 10 * time.Minute
	// after a failed fetch, how long before another is tried
	x402SupportedRetryInterval = 30 * time.Second
	// A known fee payer keeps being quoted while refreshes fail, up to this
	// age. If it rotated meanwhile, the facilitator refuses the payment at
	// verify and nothing moves.
	x402SupportedMaxAge = time.Hour
)

// x402Now is the cache's clock; tests move it.
var x402Now = time.Now

type x402SupportedEntry struct {
	// v1 exact network -> fee payer
	feePayers map[string]string
	fetchTime time.Time
	failTime  time.Time
	fetching  bool
}

type x402SupportedCache struct {
	mutex sync.Mutex
	// by facilitator url
	entries map[string]*x402SupportedEntry
}

var x402Supported = &x402SupportedCache{
	entries: map[string]*x402SupportedEntry{},
}

// x402FeePayer is the facilitator's fee payer for a network, when one is
// known.
func x402FeePayer(c *X402Config, network string) (string, bool) {
	return x402Supported.feePayer(c, network)
}

func (self *x402SupportedCache) feePayer(c *X402Config, network string) (string, bool) {
	entry, fetch := self.claimFetch(c)
	if fetch {
		// fetched without the lock: concurrent quotes keep using what is known
		feePayers, err := x402FetchFeePayersRecovered(c)
		self.finishFetch(entry, feePayers, err)
	}
	return self.lookup(entry, network)
}

// claimFetch returns the facilitator's entry, and whether the caller should
// fetch it now: it is stale, the last failure is old enough to retry, and no
// fetch is already in flight.
func (self *x402SupportedCache) claimFetch(c *X402Config) (*x402SupportedEntry, bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	entry, ok := self.entries[c.Facilitator.Url]
	if !ok {
		entry = &x402SupportedEntry{}
		self.entries[c.Facilitator.Url] = entry
	}
	now := x402Now()
	stale := entry.fetchTime.IsZero() || x402SupportedTtl <= now.Sub(entry.fetchTime)
	retryable := entry.failTime.IsZero() || x402SupportedRetryInterval <= now.Sub(entry.failTime)
	if !stale || !retryable || entry.fetching {
		return entry, false
	}
	entry.fetching = true
	return entry, true
}

func (self *x402SupportedCache) finishFetch(entry *x402SupportedEntry, feePayers map[string]string, err error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	entry.fetching = false
	if err != nil {
		entry.failTime = x402Now()
		glog.Errorf(
			"[x402]facilitator /supported failed; a network that needs its fee payer is quoted only while a known one is under %s old: %s\n",
			x402SupportedMaxAge, err,
		)
		return
	}
	entry.feePayers = feePayers
	entry.fetchTime = x402Now()
	entry.failTime = time.Time{}
}

func (self *x402SupportedCache) lookup(entry *x402SupportedEntry, network string) (string, bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if entry.fetchTime.IsZero() || x402SupportedMaxAge <= x402Now().Sub(entry.fetchTime) {
		return "", false
	}
	feePayer := entry.feePayers[network]
	return feePayer, feePayer != ""
}

// x402FetchFeePayersRecovered is x402FetchFeePayers on the quote path, where
// a panic must not take the quote down or leave the fetch claimed forever.
func x402FetchFeePayersRecovered(c *X402Config) (feePayers map[string]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			feePayers, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return x402FetchFeePayers(c)
}

// x402SupportedKind is one entry of the facilitator's GET /supported answer.
type x402SupportedKind struct {
	X402Version int            `json:"x402Version"`
	Scheme      string         `json:"scheme"`
	Network     string         `json:"network"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// x402FetchFeePayers asks the facilitator for the fee payer of each x402 v1
// exact network that has one. Only Solana networks do, so a fee payer that is
// not a Solana address is ignored.
func x402FetchFeePayers(c *X402Config) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), x402SupportedTimeout)
	defer cancel()

	status, body, err := x402FacilitatorDo(ctx, c, http.MethodGet, "/supported", nil, x402SupportedTimeout)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("facilitator /supported: %d %s", status, string(body))
	}
	supported := struct {
		Kinds []x402SupportedKind `json:"kinds"`
	}{}
	if err := json.Unmarshal(body, &supported); err != nil {
		return nil, fmt.Errorf("facilitator /supported answered something other than kinds: %w", err)
	}

	feePayers := map[string]string{}
	for _, kind := range supported.Kinds {
		if kind.X402Version != X402Version || kind.Scheme != "exact" {
			continue
		}
		feePayer, _ := kind.Extra["feePayer"].(string)
		if feePayer != "" && x402AddressValid(x402AddressSolana, feePayer) {
			feePayers[kind.Network] = feePayer
		}
	}

	// a configured network the facilitator gave no fee payer for is not
	// quoted; say so once per fetch rather than once per quote
	for _, network := range c.Networks {
		if token, err := x402TokenFor(network, c.Asset); err == nil && token.feePayerRequired && feePayers[network] == "" {
			glog.Errorf("[x402]facilitator /supported names no x402 v1 fee payer for %s; %s is not quoted\n", network, network)
		}
	}
	return feePayers, nil
}
