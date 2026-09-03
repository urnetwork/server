package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"

	"github.com/stripe/stripe-go/v82"
	stripecheckout "github.com/stripe/stripe-go/v82/checkout/session"
)

// Buy data without signing in.
//
// POST /pay/data/checkout starts a hosted checkout with Stripe or Coinbase
// Commerce for a data pack and hands back the url to send the customer to. The
// customer either names the network that should receive the data (the webhook
// then applies it on payment and emails an "it is on your network" note) or
// gives no network and receives a data code by email, exactly as today.
//
// POST /pay/data/network-lookup answers "does a network with exactly this name
// exist" for the buy-data page. /auth/network-check is a sign-up similarity
// check and reads a name within a few characters of an existing one as taken,
// so it cannot answer that question.
//
// Both are unauthenticated and per-ip rate limited.

const (
	PayDataProviderStripe   = "stripe"
	PayDataProviderCoinbase = "coinbase"
)

// The checkout metadata both webhooks read. `apply_to_network` = "1" means the
// purchase was made for a named network and the data is applied on payment
// instead of only emailing a code.
const (
	payDataMetadataApplyToNetwork = "apply_to_network"
	payDataMetadataNetworkId      = "network_id"
	payDataMetadataNetworkName    = "network_name"
	payDataMetadataEmail          = "email"
	payDataMetadataItemId         = "item_id"
	payDataMetadataApplyYes       = "1"
)

type PayDataCheckoutArgs struct {
	// "data_1tib" or "data_10tib"
	ItemId string `json:"item_id"`
	// "stripe" or "coinbase"
	Provider string `json:"provider"`
	// the network that should receive the data. Optional: without it the
	// customer gets a data code by email.
	NetworkName string `json:"network_name,omitempty"`
	// where the data code (or the applied note) is sent. Stripe collects the
	// email at checkout when it is not given here; Coinbase needs it here
	// unless a network name is given.
	Email string `json:"email,omitempty"`
}

type PayDataCheckoutResult struct {
	// the hosted checkout url to send the customer to
	Url      string `json:"url,omitempty"`
	Provider string `json:"provider,omitempty"`
	// the resolved network when a network name was given
	NetworkId *server.Id            `json:"network_id,omitempty"`
	Error     *PayDataCheckoutError `json:"error,omitempty"`
}

type PayDataCheckoutError struct {
	Message string `json:"message"`
}

func payDataCheckoutError(message string) *PayDataCheckoutResult {
	return &PayDataCheckoutResult{
		Error: &PayDataCheckoutError{Message: message},
	}
}

type PayDataNetworkLookupArgs struct {
	NetworkName string `json:"network_name"`
}

type PayDataNetworkLookupResult struct {
	Exists bool `json:"exists"`
	// the name as stored, when it exists
	NetworkName string `json:"network_name,omitempty"`
}

// payDataTarget is a validated checkout request: what is bought and who gets it.
type payDataTarget struct {
	ItemId    string
	ByteCount model.ByteCount
	// set when the purchase applies to a named network
	NetworkId   *server.Id
	NetworkName string
	Email       string
}

func (self *payDataTarget) applyToNetwork() bool {
	return self.NetworkId != nil
}

// -----------------------------------------------------------------------------
// per-ip limits
// -----------------------------------------------------------------------------

const (
	payDataCheckoutIpLimitPerMinute = 10
	payDataLookupIpLimitPerMinute   = 60
)

// payDataIpLimiter is a fixed one-minute window per client address, the same
// shape as the wallet validate limiter: cheap, in-process, and fail-open when
// the client address cannot be hashed (tests, local runs without the client
// secret).
type payDataIpLimiter struct {
	limitPerMinute int
	lock           sync.Mutex
	window         time.Time
	counts         map[[32]byte]int
}

var payDataCheckoutLimiter = &payDataIpLimiter{limitPerMinute: payDataCheckoutIpLimitPerMinute}
var payDataLookupLimiter = &payDataIpLimiter{limitPerMinute: payDataLookupIpLimitPerMinute}

func (self *payDataIpLimiter) allow(clientSession *session.ClientSession) (allow bool) {
	defer func() {
		if r := recover(); r != nil {
			allow = true
		}
	}()
	if clientSession == nil || clientSession.ClientAddress == "" {
		return true
	}
	clientAddressHash, _, err := clientSession.ClientAddressHashPort()
	if err != nil {
		return true
	}
	return self.allowHash(clientAddressHash, server.NowUtc())
}

func (self *payDataIpLimiter) allowHash(clientAddressHash [32]byte, now time.Time) bool {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.window.IsZero() || time.Minute <= now.Sub(self.window) {
		self.window = now
		self.counts = map[[32]byte]int{}
	}
	self.counts[clientAddressHash] += 1
	return self.counts[clientAddressHash] <= self.limitPerMinute
}

// -----------------------------------------------------------------------------
// network lookup
// -----------------------------------------------------------------------------

func PayDataNetworkLookup(
	args *PayDataNetworkLookupArgs,
	clientSession *session.ClientSession,
) (*PayDataNetworkLookupResult, error) {
	networkName := strings.TrimSpace(args.NetworkName)
	if networkName == "" {
		return &PayDataNetworkLookupResult{Exists: false}, nil
	}
	if !payDataLookupLimiter.allow(clientSession) {
		return nil, errors.New("Too many lookups from this address. Try again in a minute.")
	}
	networkId, storedName := model.FindNetworkByName(clientSession.Ctx, networkName)
	if networkId == nil {
		return &PayDataNetworkLookupResult{Exists: false}, nil
	}
	return &PayDataNetworkLookupResult{
		Exists:      true,
		NetworkName: storedName,
	}, nil
}

// -----------------------------------------------------------------------------
// checkout
// -----------------------------------------------------------------------------

func PayDataCheckout(
	args *PayDataCheckoutArgs,
	clientSession *session.ClientSession,
) (*PayDataCheckoutResult, error) {
	target, provider, errMessage := payDataValidate(args)
	if errMessage != "" {
		return payDataCheckoutError(errMessage), nil
	}

	if !payDataCheckoutLimiter.allow(clientSession) {
		return payDataCheckoutError("Too many checkout attempts from this address. Try again in a minute."), nil
	}

	networkNameArg := strings.TrimSpace(args.NetworkName)
	if networkNameArg != "" {
		networkId, storedName := model.FindNetworkByName(clientSession.Ctx, networkNameArg)
		if networkId == nil {
			return payDataCheckoutError(fmt.Sprintf("No network named %s", networkNameArg)), nil
		}
		target.NetworkId = networkId
		target.NetworkName = storedName
	} else if provider == PayDataProviderCoinbase && target.Email == "" {
		// Stripe collects the email on its checkout page; Coinbase Commerce does
		// not hand the payer's email back reliably, so without a network there
		// would be nowhere to send the code
		return payDataCheckoutError("Enter the email that should receive the data code, or the network that should receive the data."), nil
	}

	var checkoutUrl string
	switch provider {
	case PayDataProviderStripe:
		checkoutUrl, errMessage = payDataStripeCheckout(target)
	case PayDataProviderCoinbase:
		checkoutUrl, errMessage = payDataCoinbaseCheckout(clientSession.Ctx, target)
	}
	if errMessage != "" {
		return payDataCheckoutError(errMessage), nil
	}

	glog.Infof(
		"[paydata]%s checkout started for %s (network %v)\n",
		provider, target.ItemId, target.NetworkId,
	)

	return &PayDataCheckoutResult{
		Url:       checkoutUrl,
		Provider:  provider,
		NetworkId: target.NetworkId,
	}, nil
}

// payDataValidate checks the request shape before anything is looked up or
// counted against the rate limit. The returned target has no network yet.
func payDataValidate(args *PayDataCheckoutArgs) (target payDataTarget, provider string, errMessage string) {
	itemId := strings.TrimSpace(args.ItemId)
	byteCount, ok := stripeDataPackByteCount(itemId)
	if !ok {
		return target, "", "Unknown item."
	}

	provider = strings.ToLower(strings.TrimSpace(args.Provider))
	switch provider {
	case PayDataProviderStripe, PayDataProviderCoinbase:
	default:
		return target, "", "Unknown provider."
	}

	email := strings.TrimSpace(args.Email)
	if email != "" && !payDataValidEmail(email) {
		return target, "", "That email address does not look right."
	}

	target = payDataTarget{
		ItemId:    itemId,
		ByteCount: byteCount,
		Email:     email,
	}
	return target, provider, ""
}

// payDataValidEmail is a shape check only: the address is where a paid code is
// sent, so an obviously broken one is refused before money moves.
func payDataValidEmail(email string) bool {
	if len(email) > 254 {
		return false
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return false
	}
	if strings.ContainsAny(email, " \t\r\n") {
		return false
	}
	return strings.Contains(email[at+1:], ".")
}

// payDataSuccessUrl is where the customer lands after paying. The configured
// success url (which carries Stripe's session id placeholder) gets the item and,
// when the purchase applies to a network, the network name, so the page can say
// what happened without another lookup.
func payDataSuccessUrl(successUrl string, itemId string, networkName string) string {
	query := url.Values{}
	query.Set("item", itemId)
	if networkName != "" {
		query.Set("network", networkName)
	}
	separator := "?"
	if strings.Contains(successUrl, "?") {
		separator = "&"
	}
	return successUrl + separator + query.Encode()
}

// -----------------------------------------------------------------------------
// stripe
// -----------------------------------------------------------------------------

// payDataStripeMetadata is what checkout.session.completed carries back so the
// webhook knows the purchase was for a named network.
func payDataStripeMetadata(target payDataTarget) map[string]string {
	metadata := map[string]string{
		payDataMetadataItemId: target.ItemId,
	}
	if target.applyToNetwork() {
		metadata[payDataMetadataApplyToNetwork] = payDataMetadataApplyYes
		metadata[payDataMetadataNetworkId] = target.NetworkId.String()
		metadata[payDataMetadataNetworkName] = target.NetworkName
	}
	return metadata
}

// payDataStripeSessionParams builds the hosted checkout session. The line items
// come from the same product and price lookups the signed-in checkout uses, so
// the webhook fulfils both the same way (sku by product id).
func payDataStripeSessionParams(
	target payDataTarget,
	urls StripeCheckoutUrls,
	lineItems []*stripe.CheckoutSessionLineItemParams,
) *stripe.CheckoutSessionParams {
	params := &stripe.CheckoutSessionParams{
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		LineItems:  lineItems,
		SuccessURL: stripe.String(payDataSuccessUrl(urls.SuccessUrl, target.ItemId, target.NetworkName)),
		CancelURL:  stripe.String(urls.CancelUrl),
		Metadata:   payDataStripeMetadata(target),
	}
	if target.applyToNetwork() {
		// the fulfilment webhook resolves the network from this, exactly as it
		// does for a signed-in checkout
		params.ClientReferenceID = stripe.String(target.NetworkId.String())
	}
	if target.Email != "" {
		params.CustomerEmail = stripe.String(target.Email)
	}
	return params
}

func payDataStripeCheckout(target payDataTarget) (checkoutUrl string, errMessage string) {
	urls := stripeCheckoutUrls()
	if urls.SuccessUrl == "" || urls.CancelUrl == "" {
		// refuse rather than hand a customer to Stripe with no way back
		glog.Errorf("[paydata]stripe checkout urls are not configured\n")
		return "", "Checkout is not configured."
	}

	lineItems, errMessage := stripeDataPackLineItems(target.ItemId)
	if errMessage != "" {
		return "", errMessage
	}

	params := payDataStripeSessionParams(target, urls, lineItems)

	stripe.Key = stripeApiTokenFunc()
	checkoutSession, err := stripecheckout.New(params)
	if err != nil {
		glog.Errorf("[paydata]could not create stripe checkout session for %s: %s\n", target.ItemId, err)
		return "", "Could not start checkout. Please try again."
	}
	if checkoutSession.URL == "" {
		glog.Errorf("[paydata]stripe checkout session %s has no url\n", checkoutSession.ID)
		return "", "Could not start checkout. Please try again."
	}
	return checkoutSession.URL, ""
}

// -----------------------------------------------------------------------------
// coinbase commerce
// -----------------------------------------------------------------------------

// coinbaseCommerceApiBase is the seam for tests; the api key comes from the
// vault (coinbase.yml `commerce.api_key`).
var coinbaseCommerceApiBase = "https://api.commerce.coinbase.com"

const coinbaseCommerceApiVersion = "2018-03-22"

// coinbaseCommerceApiKey reads the Commerce api key. Empty when the vault has
// no `commerce` section: crypto checkout is then reported as not configured
// instead of panicking on a missing secret.
var coinbaseCommerceApiKey = sync.OnceValue(func() (apiKey string) {
	defer func() {
		if r := recover(); r != nil {
			apiKey = ""
		}
	}()
	c := server.Vault.RequireSimpleResource("coinbase.yml").Parse()
	commerce, ok := c["commerce"].(map[string]any)
	if !ok {
		return ""
	}
	apiKey, _ = commerce["api_key"].(string)
	return strings.TrimSpace(apiKey)
})

var coinbaseCommerceApiKeyFunc = func() string { return coinbaseCommerceApiKey() }

// coinbaseSkuForItem maps a data item to the sku name in config coinbase.yml.
// The charge is created with this as its `name`, which is how the webhook
// looks the sku up again on charge:confirmed.
func coinbaseSkuForItem(itemId string) (string, bool) {
	switch itemId {
	case StripeItemData1Tib:
		return "1TiB", true
	case StripeItemData10Tib:
		return "10TiB", true
	}
	return "", false
}

type CoinbaseChargeRequest struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	PricingType string                   `json:"pricing_type"`
	LocalPrice  CoinbaseChargeLocalPrice `json:"local_price"`
	Metadata    map[string]string        `json:"metadata"`
	RedirectUrl string                   `json:"redirect_url,omitempty"`
	CancelUrl   string                   `json:"cancel_url,omitempty"`
}

type CoinbaseChargeLocalPrice struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type CoinbaseChargeResponse struct {
	Data *CoinbaseChargeResponseData `json:"data"`
}

type CoinbaseChargeResponseData struct {
	Id        string `json:"id"`
	Code      string `json:"code"`
	HostedUrl string `json:"hosted_url"`
}

// coinbaseChargeRequest builds the Commerce charge for a data pack. The price is
// the same pro.yml price Stripe charges, and the metadata is what the webhook
// reads back (`CoinbaseEventDataMetadata`).
func coinbaseChargeRequest(
	target payDataTarget,
	skuName string,
	priceUsd float64,
	urls StripeCheckoutUrls,
) *CoinbaseChargeRequest {
	metadata := map[string]string{
		payDataMetadataItemId: target.ItemId,
	}
	if target.Email != "" {
		metadata[payDataMetadataEmail] = target.Email
	}
	description := fmt.Sprintf("%s of URnetwork data, valid for one year", payDataAmountLabel(target.ByteCount))
	if target.applyToNetwork() {
		metadata[payDataMetadataApplyToNetwork] = payDataMetadataApplyYes
		metadata[payDataMetadataNetworkId] = target.NetworkId.String()
		metadata[payDataMetadataNetworkName] = target.NetworkName
		description = fmt.Sprintf("%s, applied to network %s", description, target.NetworkName)
	} else {
		description = fmt.Sprintf("%s, delivered as a code by email", description)
	}
	request := &CoinbaseChargeRequest{
		Name:        skuName,
		Description: description,
		PricingType: "fixed_price",
		LocalPrice: CoinbaseChargeLocalPrice{
			Amount:   fmt.Sprintf("%.2f", priceUsd),
			Currency: "USD",
		},
		Metadata: metadata,
	}
	if urls.SuccessUrl != "" {
		request.RedirectUrl = payDataSuccessUrl(coinbaseRedirectBase(urls.SuccessUrl), target.ItemId, target.NetworkName)
	}
	if urls.CancelUrl != "" {
		request.CancelUrl = urls.CancelUrl
	}
	return request
}

// payDataAmountLabel is the customer-facing amount on the Coinbase charge:
// "1 TiB", "10 TiB".
func payDataAmountLabel(byteCount model.ByteCount) string {
	if 0 < byteCount && byteCount%model.Tib == 0 {
		return fmt.Sprintf("%d TiB", byteCount/model.Tib)
	}
	return model.ByteCountHumanReadable(byteCount)
}

// coinbaseRedirectBase strips Stripe's `session_id={CHECKOUT_SESSION_ID}`
// placeholder from the shared success url: Coinbase does not fill it in.
func coinbaseRedirectBase(successUrl string) string {
	parsed, err := url.Parse(successUrl)
	if err != nil {
		return successUrl
	}
	query := parsed.Query()
	for key, values := range query {
		for _, value := range values {
			if strings.Contains(value, "{") {
				query.Del(key)
				break
			}
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// coinbaseCreateCharge posts the charge and returns the hosted checkout url.
func coinbaseCreateCharge(
	ctx context.Context,
	apiKey string,
	request *CoinbaseChargeRequest,
) (*CoinbaseChargeResponseData, error) {
	// server.HttpPost json-encodes the body and sets the json content type
	responseBytes, err := server.HttpPost[[]byte](
		ctx,
		fmt.Sprintf("%s/charges", coinbaseCommerceApiBase),
		request,
		func(header http.Header) {
			header.Set("Accept", "application/json")
			header.Set("X-CC-Api-Key", apiKey)
			header.Set("X-CC-Version", coinbaseCommerceApiVersion)
		},
		server.HttpResponseRequireStatusOk[[]byte](func(response *http.Response, responseBodyBytes []byte) ([]byte, error) {
			return responseBodyBytes, nil
		}),
	)
	if err != nil {
		return nil, err
	}
	var response CoinbaseChargeResponse
	if err := json.Unmarshal(responseBytes, &response); err != nil {
		return nil, err
	}
	if response.Data == nil || response.Data.HostedUrl == "" {
		return nil, errors.New("coinbase charge response has no hosted url")
	}
	return response.Data, nil
}

func payDataCoinbaseCheckout(ctx context.Context, target payDataTarget) (checkoutUrl string, errMessage string) {
	apiKey := coinbaseCommerceApiKeyFunc()
	if apiKey == "" {
		glog.Errorf("[paydata]coinbase commerce api key is not configured\n")
		return "", "Crypto checkout is not configured"
	}

	skuName, ok := coinbaseSkuForItem(target.ItemId)
	if !ok {
		return "", "That data pack is not available."
	}
	if _, ok := coinbaseSkus()[skuName]; !ok {
		// the webhook would not recognize the charge: refuse rather than take
		// money for nothing
		glog.Errorf("[paydata]coinbase sku %s is not configured\n", skuName)
		return "", "That data pack is not available."
	}

	priceUsd, ok := stripeDataPackPriceUsd(target.ByteCount)
	if !ok || priceUsd <= 0 {
		glog.Errorf("[paydata]no price in pro.yml for data pack %s\n", target.ItemId)
		return "", "That data pack is not available."
	}

	request := coinbaseChargeRequest(target, skuName, priceUsd, stripeCheckoutUrls())
	charge, err := coinbaseCreateCharge(ctx, apiKey, request)
	if err != nil {
		glog.Errorf("[paydata]could not create coinbase charge for %s: %s\n", target.ItemId, err)
		return "", "Could not start crypto checkout. Please try again."
	}
	glog.Infof("[paydata]coinbase charge %s created for %s\n", charge.Id, target.ItemId)
	return charge.HostedUrl, ""
}
