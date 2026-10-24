package controller

// Paying for Pro or data with Zcash, Monero or USDC on Solana: the generic
// invoice API (mmm/ZCASH-CONTRACT.md §2, mmm/ZCASH.md §0).
//
//	GET  /pay/crypto/assets          the coins that are on, and what each sells
//	POST /pay/crypto/intent          an invoice for the signed-in caller's network
//	POST /pay/data/crypto-intent     a data pack for a named network, or a code, without signing in
//	POST /pay/crypto/status          an invoice's status, by its id and status token
//	POST /pay/crypto/requote         a fresh quote for an invoice nothing was paid to yet
//
// The server quotes and builds every payment URI; a client never names a
// price, an amount or an address. Each xmr and zec invoice gets its own receive
// address from a pool the taskworker fills, so a client request never waits on
// a wallet daemon. USDC keeps its Solana Pay intents (one merchant address, a
// reference and a unique amount): the invoice links one, and the Helius
// webhook's credit of it goes through the same credit as the other coins
// (crypto_pay_credit.go).
//
// The status token is the bearer secret of one invoice: anyone holding it may
// read the invoice's status, which is how a page or an app that is not signed
// in follows its payment. Only its SHA-256 is stored, and it travels only in
// request bodies, never in a url or a log.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mr-tron/base58"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// ----- the api shapes (mmm/ZCASH-CONTRACT.md §2; the SDK and connect/api/bringyour.yml
// carry the same names and json fields) -----

// CryptoPaymentAsset is one coin that is on, and what it sells.
type CryptoPaymentAsset struct {
	// usdc_solana | zec | xmr
	Asset string `json:"asset"`
	// the plans the coin sells (yearly, yearly_onboarding; never monthly)
	Plans []string `json:"plans"`
	// the data items the coin sells (data_1tib, data_10tib)
	Items []string `json:"items"`
	// the confirmations a payment needs before it is credited
	ConfirmationsRequired int `json:"confirmations_required"`
	// how long a quote holds
	QuoteTtlSeconds int `json:"quote_ttl_seconds"`
}

// CryptoPaymentAssetsResult is GET /pay/crypto/assets.
type CryptoPaymentAssetsResult struct {
	// only the coins that are on; the order means nothing (every UI orders
	// the methods Card, USDC, Zcash, Monero)
	Assets []*CryptoPaymentAsset `json:"assets"`
}

// CryptoTerm is the network's latest one-time crypto credit whose grace has not
// ended (SubscriptionBalanceResult.CryptoTerm).
type CryptoTerm struct {
	// solana | zcash | monero
	Market string `json:"market"`
	// when the paid term ends and the grace begins; apps show the renewal
	// notice from here until GraceEndTime, never before
	TermEndTime  time.Time `json:"term_end_time"`
	GraceEndTime time.Time `json:"grace_end_time"`
}

// CreateCryptoPaymentIntentArgs is POST /pay/crypto/intent: exactly one of
// plan and item_id.
type CreateCryptoPaymentIntentArgs struct {
	Asset  string `json:"asset"`
	Plan   string `json:"plan,omitempty"`
	ItemId string `json:"item_id,omitempty"`
	// the store's storefront country, when the app knows it (the price tier)
	StorefrontCountry string `json:"storefront_country,omitempty"`
}

// CreateDataCryptoPaymentIntentArgs is POST /pay/data/crypto-intent: a data
// pack without signing in, for a named network or as a code.
type CreateDataCryptoPaymentIntentArgs struct {
	Asset  string `json:"asset"`
	ItemId string `json:"item_id"`
	// set: the data lands on this network. Empty: a balance code, shown once
	// paid and emailed when Email is given
	NetworkName string `json:"network_name,omitempty"`
	Email       string `json:"email,omitempty"`
	// not taken here: a plan is refused (invalid_request). Kept so that
	// refusal is explicit rather than a silently ignored field.
	Plan string `json:"plan,omitempty"`
}

// CryptoPaymentStatusArgs is POST /pay/crypto/status and /pay/crypto/requote.
type CryptoPaymentStatusArgs struct {
	IntentId    string `json:"intent_id"`
	StatusToken string `json:"status_token"`
}

// CryptoPaymentError is a refusal: one of the PurchaseErrorCode* values and
// an English message to fall back to.
type CryptoPaymentError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CryptoPaymentStatusResult is an invoice's status (§2.5).
type CryptoPaymentStatusResult struct {
	IntentId string `json:"intent_id"`
	Asset    string `json:"asset"`
	Plan     string `json:"plan"`
	ItemId   string `json:"item_id"`
	// account | network | code
	Delivery string `json:"delivery"`
	// the named network (delivery network), else ""
	NetworkName string `json:"network_name"`
	// unknown | pending | seen | confirming | paid | expired | review
	Status string `json:"status"`
	// with review: underpaid | late_underpaid | extra | orphaned | voided
	ReviewReason string `json:"review_reason"`
	// exact decimal strings in coin units, and the same in atomic units
	Amount          string `json:"amount"`
	AmountAtomic    string `json:"amount_atomic"`
	Received        string `json:"received"`
	ReceivedAtomic  string `json:"received_atomic"`
	Remaining       string `json:"remaining"`
	RemainingAtomic string `json:"remaining_atomic"`
	// the lowest among the receipts that make up the payment (0 in the mempool)
	Confirmations         int        `json:"confirmations"`
	ConfirmationsRequired int        `json:"confirmations_required"`
	ExpiresAt             *time.Time `json:"expires_at"`
	CreditedAt            *time.Time `json:"credited_at"`
	// delivery code, once paid: the balance code to redeem
	BalanceCode string              `json:"balance_code"`
	Error       *CryptoPaymentError `json:"error,omitempty"`
}

// CryptoPaymentIntentResult is an invoice (§2.3), from intent, data-intent and
// requote.
type CryptoPaymentIntentResult struct {
	IntentId string `json:"intent_id"`
	// the invoice's bearer secret: keep it in memory, device storage, request
	// bodies and url fragments; never in a query string or a log
	StatusToken  string  `json:"status_token"`
	Asset        string  `json:"asset"`
	Plan         string  `json:"plan"`
	ItemId       string  `json:"item_id"`
	Delivery     string  `json:"delivery"`
	NetworkName  string  `json:"network_name"`
	Tier         string  `json:"tier"`
	OfferApplied bool    `json:"offer_applied"`
	PriceUsd     float64 `json:"price_usd"`
	// the item's regular price (the offer's full yearly price)
	RegularPriceUsd float64 `json:"regular_price_usd"`
	// the coin's USD rate the amount was quoted at (1 for USDC)
	RateUsd float64 `json:"rate_usd"`
	// where to pay, and exactly how much: at most 6 decimals, equal to
	// AmountAtomic
	Address      string `json:"address"`
	Amount       string `json:"amount"`
	AmountAtomic string `json:"amount_atomic"`
	// usdc_solana only: the reference, as the memo of a transfer sent by hand
	Memo string `json:"memo"`
	// the payment URI to show as a QR and open; never built by a client
	Uri                   string                     `json:"uri"`
	QuotedAt              *time.Time                 `json:"quoted_at"`
	ExpiresAt             *time.Time                 `json:"expires_at"`
	ConfirmationsRequired int                        `json:"confirmations_required"`
	Status                *CryptoPaymentStatusResult `json:"status,omitempty"`
	Error                 *CryptoPaymentError        `json:"error,omitempty"`
}

// The statuses of §2.5.
const (
	CryptoPaymentStatusUnknown    = "unknown"
	CryptoPaymentStatusPending    = "pending"
	CryptoPaymentStatusSeen       = "seen"
	CryptoPaymentStatusConfirming = "confirming"
	CryptoPaymentStatusPaid       = "paid"
	CryptoPaymentStatusExpired    = "expired"
	CryptoPaymentStatusReview     = "review"
)

func cryptoIntentError(code string, message string) *CryptoPaymentIntentResult {
	return &CryptoPaymentIntentResult{Error: &CryptoPaymentError{Code: code, Message: message}}
}

const cryptoAssetUnavailableMessage = "Paying with this coin is temporarily unavailable."

// One default-level report a minute per coin and cause of refused quotes: a
// rate source that is down refuses every request, and the log says so once.
var cryptoPayRefusalReports = newCryptoPayKeyedReport(time.Minute)

// ----- per-network limit of the signed-in endpoints -----

// The signed-in intent endpoints allow each network this many intents a
// minute: /pay/crypto/intent and /solana/payment-intent share the budget
// (decision 13; /solana/payment-intent had no limit before).
const cryptoPayNetworkIntentsPerMinute = 10

var cryptoPayNetworkLimiter = &payDataIpLimiter{limitPerMinute: cryptoPayNetworkIntentsPerMinute}

// allowNetworkIntent counts one intent of the network against its budget.
// The key is a hash of the network id, held in memory only.
// Code-delivery invoices (no sign-in, no network) one client address may
// start in an hour. Each draws a pool address that stays taken until its
// quote ends, so without this an anonymous caller could drain the pool.
const cryptoCodeIntentsPerAddressPerHour = 6

var cryptoCodeIntentLimiter = &cryptoWindowLimiter{limit: cryptoCodeIntentsPerAddressPerHour, window: time.Hour}

// cryptoWindowLimiter counts calls per client address in a fixed window, like
// payDataIpLimiter but with a window of its own. It keeps only the hash of
// the address, in memory: no address is ever stored (mmm/ZCASH-CONTRACT.md §3.11).
type cryptoWindowLimiter struct {
	limit     int
	window    time.Duration
	stateLock sync.Mutex
	start     time.Time
	counts    map[[32]byte]int
}

func (self *cryptoWindowLimiter) allow(clientSession *session.ClientSession) (allow bool) {
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

func (self *cryptoWindowLimiter) allowHash(clientAddressHash [32]byte, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.start.IsZero() || self.window <= now.Sub(self.start) {
		self.start = now
		self.counts = map[[32]byte]int{}
	}
	self.counts[clientAddressHash] += 1
	return self.counts[clientAddressHash] <= self.limit
}

// reset starts the window afresh (tests).
func (self *cryptoWindowLimiter) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.start = time.Time{}
	self.counts = nil
}

func allowNetworkIntent(networkId server.Id) bool {
	return cryptoPayNetworkLimiter.allowHash(sha256.Sum256(append([]byte("crypto-intent/"), networkId.Bytes()...)), server.NowUtc())
}

// ----- status tokens -----

const cryptoStatusTokenBytes = 32

// cryptoNewStatusToken is a fresh status token (32 random bytes, base64url
// without padding) and the hash that is stored.
func cryptoNewStatusToken() (token string, tokenHash []byte) {
	b := make([]byte, cryptoStatusTokenBytes)
	// crypto/rand.Read never fails as of go 1.24; a predictable token would
	// let anyone read an invoice, so there is deliberately no fallback
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("no entropy for a status token: %s", err))
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(b), sum[:]
}

// cryptoStatusTokenHash is the stored form of a presented token, or ok =
// false for a token that cannot be one.
func cryptoStatusTokenHash(token string) ([]byte, bool) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil || len(b) != cryptoStatusTokenBytes {
		return nil, false
	}
	sum := sha256.Sum256(b)
	return sum[:], true
}

// cryptoNewSolanaReference is a Solana Pay reference: 32 random bytes,
// base58, the wire form of a public key (as sdk/solana_pay.go makes them).
func cryptoNewSolanaReference() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("no entropy for a payment reference: %s", err))
	}
	return base58.Encode(b)
}

// ----- payment URIs (§3.5) -----

// cryptoItemDescription is the fixed English description a wallet shows.
func cryptoItemDescription(item string) string {
	switch item {
	case model.SolanaPlanYearly:
		return "URnetwork Pro yearly"
	case model.SolanaPlanYearlyOnboarding:
		return "URnetwork Pro yearly (welcome offer)"
	case StripeItemData1Tib:
		return "URnetwork 1 TiB data"
	case StripeItemData10Tib:
		return "URnetwork 10 TiB data"
	}
	return "URnetwork"
}

// cryptoUriEscape percent-encodes a query value per RFC 3986: everything but
// the unreserved characters (A-Z a-z 0-9 - . _ ~), a space as %20. For the
// Solana Pay URI.
func cryptoUriEscape(value string) string {
	return cryptoPercentEncode(value, func(b byte) bool {
		return !('A' <= b && b <= 'Z' || 'a' <= b && b <= 'z' || '0' <= b && b <= '9' ||
			b == '-' || b == '.' || b == '_' || b == '~')
	})
}

// cryptoZip321Escape percent-encodes a ZIP-321 message or label: every
// control byte, space, " # % & / < = > ? [ \ ] ^ ` { | } and every non-ASCII
// byte, as the reference renderer does (zakura-zip321 QCHAR_ENCODE). ' + and ~
// stay literal; Go's url.QueryEscape would write a space as '+', which ZIP-321
// reads as a plus.
func cryptoZip321Escape(value string) string {
	return cryptoPercentEncode(value, func(b byte) bool {
		return b <= 0x20 || 0x7f <= b || strings.IndexByte("\"#%&/<=>?[\\]^`{|}", b) != -1
	})
}

// cryptoMoneroEscape percent-encodes a Monero URI's tx_description as
// wallet2's conver_to_url_format does: bytes up to space, from '{' up, and
// " < > % \ ^ [ ] ` + $ , @ : ; ! # &.
func cryptoMoneroEscape(value string) string {
	return cryptoPercentEncode(value, func(b byte) bool {
		return b <= 0x20 || 0x7b <= b || strings.IndexByte("\"<>%\\^[]`+$,@:;!#&", b) != -1
	})
}

// cryptoPercentEncode writes each byte escape reports as %XX (upper-case
// hex) and every other byte as it is.
func cryptoPercentEncode(value string, escape func(byte) bool) string {
	const hexDigits = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if escape(b) {
			out.WriteByte('%')
			out.WriteByte(hexDigits[b>>4])
			out.WriteByte(hexDigits[b&0x0f])
		} else {
			out.WriteByte(b)
		}
	}
	return out.String()
}

// cryptoUriAddressOk reports whether an address can go into a URI as it is:
// the base58 and bech32 alphabets are alphanumeric.
func cryptoUriAddressOk(address string) bool {
	if address == "" || 512 < len(address) {
		return false
	}
	for _, r := range address {
		if !('0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z') {
			return false
		}
	}
	return true
}

// cryptoMoneroUri is monero:<address>?tx_amount=<amount>&tx_description=<description>.
// The amount is a plain decimal (wallet2's parser takes fewer than 12
// decimals); it is never escaped.
func cryptoMoneroUri(address string, amount string, description string) string {
	return fmt.Sprintf(
		"monero:%s?tx_amount=%s&tx_description=%s",
		address, amount, cryptoMoneroEscape(description),
	)
}

// cryptoZcashUri is the ZIP-321 request zcash:<address>?amount=<amount>&message=<description>,
// with no memo: the address alone identifies the invoice. ZIP-321 allows no
// percent-encoding in the address or the amount, and the amount has at most 8
// decimals, without a trailing point.
func cryptoZcashUri(address string, amount string, description string) string {
	return fmt.Sprintf(
		"zcash:%s?amount=%s&message=%s",
		address, amount, cryptoZip321Escape(description),
	)
}

// cryptoSolanaPayUri is the Solana Pay transfer request
// solana:<recipient>?amount=<amount>&spl-token=<mint>&reference=<reference>&label=URnetwork&message=<description>.
func cryptoSolanaPayUri(recipient string, amount string, mint string, reference string, description string) string {
	return fmt.Sprintf(
		"solana:%s?amount=%s&spl-token=%s&reference=%s&label=%s&message=%s",
		recipient,
		cryptoUriEscape(amount),
		cryptoUriEscape(mint),
		cryptoUriEscape(reference),
		cryptoUriEscape("URnetwork"),
		cryptoUriEscape(description),
	)
}

// cryptoPaymentUri is the URI for an invoice, or "" when its address cannot
// be put in one (it never is: every address comes from our own daemons).
func cryptoPaymentUri(intent *model.CryptoPaymentIntent) string {
	if !cryptoUriAddressOk(intent.Address) {
		return ""
	}
	amount := cryptoFormatAtomic(intent.AmountAtomic, cryptoAssetDecimals(intent.Asset))
	description := cryptoItemDescription(intent.Item)
	switch intent.Asset {
	case model.CryptoAssetXmr:
		return cryptoMoneroUri(intent.Address, amount, description)
	case model.CryptoAssetZec:
		return cryptoZcashUri(intent.Address, amount, description)
	case model.CryptoAssetUsdcSolana:
		if intent.SolanaReference == nil {
			return ""
		}
		return cryptoSolanaPayUri(intent.Address, amount, solanaUsdcMint, *intent.SolanaReference, description)
	}
	return ""
}

// ----- the coins on offer -----

// cryptoPaymentAssets is the list of §2.1 for the coins that are on.
func cryptoPaymentAssets(config *CryptoPayConfig) []*CryptoPaymentAsset {
	assets := []*CryptoPaymentAsset{}
	for _, asset := range cryptoEnabledAssets(config) {
		assetConfig := config.Asset(asset)
		plans := []string{}
		for _, plan := range cryptoPayKnownPlans {
			if assetConfig.SellsPlan(plan) {
				plans = append(plans, plan)
			}
		}
		items := []string{}
		for _, item := range cryptoPayKnownItems {
			if assetConfig.SellsItem(item) {
				items = append(items, item)
			}
		}
		if len(plans) == 0 && len(items) == 0 {
			continue
		}
		assets = append(assets, &CryptoPaymentAsset{
			Asset:                 asset,
			Plans:                 plans,
			Items:                 items,
			ConfirmationsRequired: int(assetConfig.CreditConfirmations),
			QuoteTtlSeconds:       int(cryptoAssetQuoteTtl(config, asset) / time.Second),
		})
	}
	return assets
}

// CryptoPaymentAssets answers GET /pay/crypto/assets.
func CryptoPaymentAssets(clientSession *session.ClientSession) (*CryptoPaymentAssetsResult, error) {
	if !payDataLookupLimiter.allow(clientSession) {
		return nil, errors.New("429 Too many requests from this address. Try again in a minute.")
	}
	return &CryptoPaymentAssetsResult{Assets: cryptoPaymentAssets(cryptoPayConfigFunc())}, nil
}

// cryptoTermForNetwork is the network's latest one-time crypto credit (Solana,
// Monero or Zcash) whose grace has not ended, or nil.
func cryptoTermForNetwork(ctx context.Context, networkId server.Id, now time.Time) *CryptoTerm {
	market, endTime, ok := model.GetLatestOneTimeSubscriptionRenewal(
		ctx,
		networkId,
		[]string{model.SubscriptionMarketSolana, model.SubscriptionMarketMonero, model.SubscriptionMarketZcash},
		now,
	)
	if !ok {
		return nil
	}
	return &CryptoTerm{
		Market:       market,
		TermEndTime:  endTime.Add(-manualPaymentGracePeriod),
		GraceEndTime: endTime,
	}
}

// decorateCryptoPay fills the balance result's crypto fields: the coins on
// offer and the network's one-time crypto term. Never fails the balance: a
// failed read leaves them out.
func decorateCryptoPay(result *SubscriptionBalanceResult, clientSession *session.ClientSession) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[cryptopay]could not decorate the balance: %v\n", r)
		}
	}()
	if assets := cryptoPaymentAssets(cryptoPayConfigFunc()); 0 < len(assets) {
		result.CryptoAssets = assets
	}
	if clientSession.ByJwt != nil {
		result.CryptoTerm = cryptoTermForNetwork(clientSession.Ctx, clientSession.ByJwt.NetworkId, server.NowUtc())
	}
}

// ----- starting an invoice -----

// cryptoIntentRequest is a validated request for an invoice.
type cryptoIntentRequest struct {
	asset string
	// the plan or data item
	item     string
	plan     bool
	delivery string
	// nil for code delivery
	networkId   *server.Id
	networkName string
	email       string
	// the price from pro.yml for the caller's tier, never the client's
	priceUsd     float64
	regularUsd   float64
	tier         string
	offerApplied bool
	// open-invoice reuse applies (account and network delivery)
	reuse bool
}

// CreateCryptoPaymentIntent answers POST /pay/crypto/intent: an invoice for
// the signed-in caller's network. The rules, in order (§2.3): the asset, the
// plan or item it sells, the guest guard, the welcome offer, the per-network
// limit, the price, open-invoice reuse, then the quote and the address.
func CreateCryptoPaymentIntent(
	args *CreateCryptoPaymentIntentArgs,
	clientSession *session.ClientSession,
) (*CryptoPaymentIntentResult, error) {
	config := cryptoPayConfigFunc()
	asset := strings.TrimSpace(args.Asset)
	if !slices.Contains(cryptoPayAssets, asset) {
		return cryptoIntentError(PurchaseErrorCodeInvalidRequest, "Unknown asset."), nil
	}
	if !cryptoAssetEnabled(config, asset) {
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedDisabled).Inc()
		return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
	}
	assetConfig := config.Asset(asset)

	plan := strings.TrimSpace(args.Plan)
	itemId := strings.TrimSpace(args.ItemId)
	if (plan == "") == (itemId == "") {
		return cryptoIntentError(PurchaseErrorCodeInvalidRequest, "Name a plan or a data pack."), nil
	}
	if plan != "" && !assetConfig.SellsPlan(plan) {
		// monthly included: it is card only
		return cryptoIntentError(PurchaseErrorCodePlanUnavailable, "That plan is not available with this coin."), nil
	}
	if itemId != "" && !assetConfig.SellsItem(itemId) {
		return cryptoIntentError(PurchaseErrorCodeItemUnavailable, "That data pack is not available with this coin."), nil
	}

	networkId := clientSession.ByJwt.NetworkId
	request := &cryptoIntentRequest{
		asset:     asset,
		delivery:  model.CryptoDeliveryAccount,
		networkId: &networkId,
		reuse:     true,
	}
	if plan != "" {
		if refuseGuestPurchase(clientSession) {
			return cryptoIntentError(PurchaseErrorCodeGuestSignInRequired, purchaseGuestSignInRequiredMessage), nil
		}
		if plan == model.SolanaPlanYearlyOnboarding {
			if EligibleOnboardingOffer(clientSession, networkId) == nil {
				return cryptoIntentError(PurchaseErrorCodeOfferUnavailable, "The welcome offer is not available."), nil
			}
			request.offerApplied = true
		}
	}

	if !allowNetworkIntent(networkId) {
		return cryptoIntentError(PurchaseErrorCodeRateLimited, "Too many payment attempts. Try again in a minute."), nil
	}

	if plan != "" {
		// the price comes from pro.yml, keyed by the plan and the caller's
		// regional tier. It is NEVER taken from the client.
		tier := ResolvePriceTier(clientSession, args.StorefrontCountry)
		priceUsd, ok := solanaPlanPriceUsdForTier(plan, tier.Tier)
		if !ok || priceUsd <= 0 {
			return cryptoIntentError(PurchaseErrorCodePlanUnavailable, "That plan is not available."), nil
		}
		request.item = plan
		request.plan = true
		request.priceUsd = priceUsd
		request.regularUsd = priceUsd
		if request.offerApplied {
			request.regularUsd = tier.Tier.YearlyUsd
		}
		request.tier = tier.Tier.Name
	} else {
		priceUsd, ok := dataPackPriceUsd(itemId)
		if !ok {
			glog.Errorf("[cryptopay]no usable price in pro.yml for data pack %s\n", itemId)
			return cryptoIntentError(PurchaseErrorCodeItemUnavailable, "That data pack is not available."), nil
		}
		request.item = itemId
		request.priceUsd = priceUsd
		request.regularUsd = priceUsd
	}

	return cryptoStartIntent(clientSession, config, request), nil
}

// CreateDataCryptoPaymentIntent answers POST /pay/data/crypto-intent: a data
// pack without signing in, for the named network, or as a balance code when
// no network is named. Per client address rate limited.
func CreateDataCryptoPaymentIntent(
	args *CreateDataCryptoPaymentIntentArgs,
	clientSession *session.ClientSession,
) (*CryptoPaymentIntentResult, error) {
	config := cryptoPayConfigFunc()
	asset := strings.TrimSpace(args.Asset)
	if !slices.Contains(cryptoPayAssets, asset) {
		return cryptoIntentError(PurchaseErrorCodeInvalidRequest, "Unknown asset."), nil
	}
	if !cryptoAssetEnabled(config, asset) {
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedDisabled).Inc()
		return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
	}
	assetConfig := config.Asset(asset)

	itemId := strings.TrimSpace(args.ItemId)
	if strings.TrimSpace(args.Plan) != "" || itemId == "" {
		// plans need a signed-in account (the guest guard and the offer)
		return cryptoIntentError(PurchaseErrorCodeInvalidRequest, "Name a data pack."), nil
	}
	if !assetConfig.SellsItem(itemId) {
		return cryptoIntentError(PurchaseErrorCodeItemUnavailable, "That data pack is not available with this coin."), nil
	}
	email := strings.TrimSpace(args.Email)
	if email != "" && !payDataValidEmail(email) {
		return cryptoIntentError(PurchaseErrorCodeInvalidEmail, "That email address does not look right."), nil
	}
	networkName := strings.TrimSpace(args.NetworkName)
	codeDelivery := networkName == ""
	if codeDelivery && !assetConfig.CodeDeliveryEnabled() {
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedNoCodes).Inc()
		return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
	}

	if !payDataCheckoutLimiter.allow(clientSession) {
		return cryptoIntentError(
			PurchaseErrorCodeRateLimited,
			"Too many payment attempts from this address. Try again in a minute.",
		), nil
	}
	if codeDelivery && !cryptoCodeIntentLimiter.allow(clientSession) {
		return cryptoIntentError(
			PurchaseErrorCodeRateLimited,
			"Too many payment attempts from this address. Try again later.",
		), nil
	}

	priceUsd, ok := dataPackPriceUsd(itemId)
	if !ok {
		glog.Errorf("[cryptopay]no usable price in pro.yml for data pack %s\n", itemId)
		return cryptoIntentError(PurchaseErrorCodeItemUnavailable, "That data pack is not available."), nil
	}
	request := &cryptoIntentRequest{
		asset:      asset,
		item:       itemId,
		priceUsd:   priceUsd,
		regularUsd: priceUsd,
	}

	if !codeDelivery {
		networkId, storedName := model.FindNetworkByName(clientSession.Ctx, networkName)
		if networkId == nil {
			return cryptoIntentError(PurchaseErrorCodeNetworkNotFound, fmt.Sprintf("No network named %s", networkName)), nil
		}
		request.delivery = model.CryptoDeliveryNetwork
		request.networkId = networkId
		request.networkName = storedName
		// the data lands on the network: no code, and nothing to email
		request.reuse = true
	} else {
		request.delivery = model.CryptoDeliveryCode
		request.email = email
	}

	return cryptoStartIntent(clientSession, config, request), nil
}

// cryptoStartIntent applies the last rules of §2.3: the per-invoice cap,
// open-invoice reuse, then the quote and the address, and returns the new
// invoice.
func cryptoStartIntent(
	clientSession *session.ClientSession,
	config *CryptoPayConfig,
	request *cryptoIntentRequest,
) *CryptoPaymentIntentResult {
	ctx := clientSession.Ctx
	asset := request.asset
	if config.Quote.MaxInvoiceUsd < request.priceUsd {
		glog.Errorf("[cryptopay]refusing to invoice %s with %s: the price is over the per-invoice cap\n", request.item, asset)
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedOverMax).Inc()
		return cryptoIntentError(PurchaseErrorCodeItemUnavailable, "This purchase is not available with this coin.")
	}

	now := server.NowUtc()
	if request.reuse && request.networkId != nil {
		// per delivery: the buy-data page never gets a token for the signed-in
		// account's own invoice, and the reverse (mmm/ZCASH-CONTRACT.md §3.13.5)
		if open := model.GetOpenCryptoPaymentIntentForReuse(ctx, *request.networkId, asset, request.item, request.delivery, now); open != nil {
			token, tokenHash := cryptoNewStatusToken()
			if err := model.AddCryptoPaymentIntentStatusToken(ctx, open.IntentId, tokenHash); err != nil {
				glog.Errorf("[cryptopay]could not hand out invoice %s again: %s\n", open.IntentId, err)
				return cryptoIntentError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage)
			}
			if glog.V(1) {
				glog.Infof("[cryptopay]reused open invoice %s (%s, %s)\n", open.IntentId, asset, request.item)
			}
			return cryptoIntentResult(ctx, config, open, token, request.networkName, now)
		}
	}

	if request.delivery == model.CryptoDeliveryCode &&
		config.MaxOpenCodeIntentsPerAsset <= model.CountOpenCryptoPaymentIntents(ctx, asset, model.CryptoDeliveryCode, now) {
		cryptoPayRefusalReports.report("too_many_codes/"+asset, func() {
			glog.Errorf("[cryptopay]%s has %d open code invoices or more; refusing new ones\n", asset, config.MaxOpenCodeIntentsPerAsset)
		})
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedTooManyCodes).Inc()
		return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage)
	}

	if asset == model.CryptoAssetUsdcSolana {
		return cryptoStartUsdcIntent(clientSession, config, request, now)
	}

	if available, reason := cryptoAssetAvailable(ctx, config, asset); !available {
		cryptoPayQuoteResults.WithLabelValues(asset, reason).Inc()
		return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage)
	}
	if config.MaxOpenIntentsPerAsset <= model.CountOpenCryptoPaymentIntents(ctx, asset, "", now) {
		cryptoPayRefusalReports.report("too_many/"+asset, func() {
			glog.Errorf("[cryptopay]%s has %d open invoices or more; refusing new ones\n", asset, config.MaxOpenIntentsPerAsset)
		})
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedTooMany).Inc()
		return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage)
	}

	rateQuote, reason := cryptoQuoteRate(ctx, config, asset)
	if rateQuote == nil {
		cryptoPayQuoteResults.WithLabelValues(asset, reason).Inc()
		cryptoPayRefusalReports.report("rate/"+asset, func() {
			glog.Warningf("[cryptopay]%s cannot be quoted now: %s\n", asset, reason)
		})
		return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage)
	}
	cryptoPayRateSpread.WithLabelValues(asset).Set(rateQuote.SpreadPercent)
	amountAtomic, ok := cryptoQuoteAmountAtomic(request.priceUsd, rateQuote.RateUsd, config.Quote.BufferPercent, cryptoAssetDecimals(asset))
	if !ok {
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedBadAmount).Inc()
		return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage)
	}

	token, tokenHash := cryptoNewStatusToken()
	intent := &model.CryptoPaymentIntent{
		IntentId:                 server.NewId(),
		StatusTokenHash:          tokenHash,
		Asset:                    asset,
		Delivery:                 request.delivery,
		NetworkId:                request.networkId,
		Item:                     request.item,
		Tier:                     request.tier,
		OfferApplied:             request.offerApplied,
		PriceUsdNanoCents:        model.UsdToNanoCents(request.priceUsd),
		RegularPriceUsdNanoCents: model.UsdToNanoCents(request.regularUsd),
		Email:                    request.email,
		AmountAtomic:             amountAtomic,
		RateUsd:                  rateQuote.RateUsd,
		RateSourcesJson:          cryptoRateSourcesJson(rateQuote),
		QuotedAt:                 now,
		ExpiresAt:                now.Add(cryptoAssetQuoteTtl(config, asset)),
		CreateTime:               now,
	}
	err := cryptoCreateIntentWithAddress(ctx, intent)
	if errors.Is(err, model.ErrCryptoAddressPoolEmpty) {
		cryptoPayRefusalReports.report("pool/"+asset, func() {
			glog.Errorf("[cryptopay]the %s address pool is empty\n", asset)
		})
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedPoolEmpty).Inc()
		return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage)
	}
	if err != nil {
		glog.Errorf("[cryptopay]could not create a %s invoice: %s\n", asset, err)
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedDatabase).Inc()
		return cryptoIntentError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage)
	}
	cryptoPayQuoteResults.WithLabelValues(asset, "quoted").Inc()
	cryptoPayIntentsCreated.WithLabelValues(asset, request.delivery).Inc()
	glog.Infof("[cryptopay]invoice %s: %s %s, delivery %s\n", intent.IntentId, asset, request.item, request.delivery)
	return cryptoIntentResult(ctx, config, intent, token, request.networkName, now)
}

// The invoice insert with its pool address. Replaceable only by tests.
var cryptoCreateIntentWithAddress = func(ctx context.Context, intent *model.CryptoPaymentIntent) error {
	return model.CreateCryptoPaymentIntentWithAddress(ctx, intent, true)
}

// cryptoStartUsdcIntent is the USDC adapter: a Solana Pay intent (one merchant
// address, a reference and a unique amount, exactly as /solana/payment-intent
// makes them) and the invoice linked to it, in one transaction.
func cryptoStartUsdcIntent(
	clientSession *session.ClientSession,
	config *CryptoPayConfig,
	request *cryptoIntentRequest,
	now time.Time,
) *CryptoPaymentIntentResult {
	ctx := clientSession.Ctx
	asset := model.CryptoAssetUsdcSolana
	reference := cryptoNewSolanaReference()
	token, tokenHash := cryptoNewStatusToken()
	intent := &model.CryptoPaymentIntent{
		IntentId:                 server.NewId(),
		StatusTokenHash:          tokenHash,
		Asset:                    asset,
		Delivery:                 request.delivery,
		NetworkId:                request.networkId,
		Item:                     request.item,
		Tier:                     request.tier,
		OfferApplied:             request.offerApplied,
		PriceUsdNanoCents:        model.UsdToNanoCents(request.priceUsd),
		RegularPriceUsdNanoCents: model.UsdToNanoCents(request.regularUsd),
		Email:                    request.email,
		Address:                  solanaPaymentRecipient(),
		SolanaReference:          &reference,
		RateUsd:                  1,
		QuotedAt:                 now,
		ExpiresAt:                now.Add(cryptoAssetQuoteTtl(config, asset)),
		CreateTime:               now,
	}
	if err := cryptoCreateLinkedSolanaIntent(ctx, intent, request.priceUsd); err != nil {
		glog.Errorf("[cryptopay]could not create a USDC invoice: %s\n", err)
		cryptoPayQuoteResults.WithLabelValues(asset, cryptoQuoteRefusedSolanaFail).Inc()
		return cryptoIntentError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage)
	}
	cryptoPayQuoteResults.WithLabelValues(asset, "quoted").Inc()
	cryptoPayIntentsCreated.WithLabelValues(asset, request.delivery).Inc()
	glog.Infof("[cryptopay]invoice %s: %s %s, delivery %s\n", intent.IntentId, asset, request.item, request.delivery)
	return cryptoIntentResult(ctx, config, intent, token, request.networkName, now)
}

// The linked USDC insert. Replaceable only by tests.
var cryptoCreateLinkedSolanaIntent = func(ctx context.Context, intent *model.CryptoPaymentIntent, priceUsd float64) error {
	return model.CreateLinkedSolanaCryptoPaymentIntent(ctx, intent, priceUsd, solanaPickAmountSuffixMicro)
}

// cryptoIntentResult is the invoice as the client sees it, with its current
// status.
func cryptoIntentResult(
	ctx context.Context,
	config *CryptoPayConfig,
	intent *model.CryptoPaymentIntent,
	token string,
	networkName string,
	now time.Time,
) *CryptoPaymentIntentResult {
	decimals := cryptoAssetDecimals(intent.Asset)
	plan, itemId := cryptoSplitItem(intent.Item)
	quotedAt := intent.QuotedAt
	expiresAt := intent.ExpiresAt
	result := &CryptoPaymentIntentResult{
		IntentId:              intent.IntentId.String(),
		StatusToken:           token,
		Asset:                 intent.Asset,
		Plan:                  plan,
		ItemId:                itemId,
		Delivery:              intent.Delivery,
		NetworkName:           networkName,
		Tier:                  intent.Tier,
		OfferApplied:          intent.OfferApplied,
		PriceUsd:              model.NanoCentsToUsd(intent.PriceUsdNanoCents),
		RegularPriceUsd:       model.NanoCentsToUsd(intent.RegularPriceUsdNanoCents),
		RateUsd:               intent.RateUsd,
		Address:               intent.Address,
		Amount:                cryptoFormatAtomic(intent.AmountAtomic, decimals),
		AmountAtomic:          fmt.Sprintf("%d", intent.AmountAtomic),
		Uri:                   cryptoPaymentUri(intent),
		QuotedAt:              &quotedAt,
		ExpiresAt:             &expiresAt,
		ConfirmationsRequired: cryptoConfirmationsRequired(config, intent.Asset),
	}
	if intent.SolanaReference != nil {
		result.Memo = *intent.SolanaReference
	}
	receipts := []*model.CryptoPaymentReceipt{}
	if intent.Asset != model.CryptoAssetUsdcSolana && intent.CreditedAt == nil {
		receipts = model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
	}
	result.Status = cryptoStatusResult(ctx, config, intent, receipts, networkName, now)
	return result
}

// cryptoSplitItem is the plan or the item id of an invoice's item.
func cryptoSplitItem(item string) (plan string, itemId string) {
	if slices.Contains(cryptoPayKnownPlans, item) {
		return item, ""
	}
	return "", item
}

// cryptoConfirmationsRequired is the depth a coin's payment is credited at.
func cryptoConfirmationsRequired(config *CryptoPayConfig, asset string) int {
	if assetConfig := config.Asset(asset); assetConfig != nil {
		return int(assetConfig.CreditConfirmations)
	}
	return 1
}

// ----- scanner health, as the quote path sees it -----

// The quote path re-reads a coin's cursor at most this often per process.
const cryptoAvailabilityCacheTtl = 10 * time.Second

type cryptoAvailability struct {
	checkedAt time.Time
	available bool
	reason    string
}

var cryptoAvailabilityLock sync.Mutex
var cryptoAvailabilityByAsset = map[string]*cryptoAvailability{}

// The cursor read behind the availability. Replaceable only by tests.
var cryptoGetScanCursor = model.GetCryptoScanCursor

// cryptoAssetAvailable reports whether a scanned coin may be quoted now: its
// scanner succeeded within three scan intervals, it knows the network's
// height, and its wallet is within max_lag_blocks of it. A coin whose
// payments would not be seen in time must not be sold.
func cryptoAssetAvailable(ctx context.Context, config *CryptoPayConfig, asset string) (bool, string) {
	if !slices.Contains(cryptoPayScannedAssets, asset) {
		return true, ""
	}
	now := server.NowUtc()
	cryptoAvailabilityLock.Lock()
	cached, ok := cryptoAvailabilityByAsset[asset]
	cryptoAvailabilityLock.Unlock()
	if ok && now.Sub(cached.checkedAt) < cryptoAvailabilityCacheTtl {
		return cached.available, cached.reason
	}
	available, reason := cryptoDecideAvailability(cryptoGetScanCursor(ctx, asset), config, asset, now)
	cryptoAvailabilityLock.Lock()
	cryptoAvailabilityByAsset[asset] = &cryptoAvailability{checkedAt: now, available: available, reason: reason}
	cryptoAvailabilityLock.Unlock()
	return available, reason
}

// cryptoDecideAvailability is the rule of cryptoAssetAvailable on a cursor
// in hand.
func cryptoDecideAvailability(cursor *model.CryptoScanCursor, config *CryptoPayConfig, asset string, now time.Time) (bool, string) {
	if !cryptoCursorFresh(cursor, config, now) {
		return false, cryptoQuoteRefusedLagging
	}
	if cursor.WalletHeight == nil || cursor.NetworkHeight == nil || *cursor.NetworkHeight <= 0 {
		// no node, or one that answered no height: the lag cannot be judged
		return false, cryptoQuoteRefusedNoHeight
	}
	if !cryptoWalletCaughtUp(config, asset, *cursor.WalletHeight, *cursor.NetworkHeight) {
		return false, cryptoQuoteRefusedLagging
	}
	return true, ""
}

// cryptoCursorFresh reports a scanner that succeeded within three scan
// intervals.
func cryptoCursorFresh(cursor *model.CryptoScanCursor, config *CryptoPayConfig, now time.Time) bool {
	return cursor != nil && cursor.LastSuccessAt != nil && now.Sub(*cursor.LastSuccessAt) <= 3*config.ScanInterval
}

// cryptoWalletCaughtUp reports a wallet within the coin's max lag of a known
// network height. The network height is the node's tip (Monero: or the height
// it syncs to), never an extrapolation from block times, so a slow block
// cannot make it trip (crypto_pay_zcash.go Heights).
func cryptoWalletCaughtUp(config *CryptoPayConfig, asset string, walletHeight int64, networkHeight int64) bool {
	return 0 < networkHeight && networkHeight-walletHeight <= config.MaxLagBlocksAt(asset, walletHeight)
}

// resetCryptoAvailability forgets the cached availability (tests).
func resetCryptoAvailability() {
	cryptoAvailabilityLock.Lock()
	defer cryptoAvailabilityLock.Unlock()
	cryptoAvailabilityByAsset = map[string]*cryptoAvailability{}
}

// ----- status -----

// cryptoLoadIntentForToken is the invoice whose status token this is, or nil.
// The token is compared in constant time against every hash the invoice
// keeps, and against a dummy when there is no such invoice, so the time it
// takes says nothing about which part failed.
func cryptoLoadIntentForToken(ctx context.Context, intentIdText string, token string) *model.CryptoPaymentIntent {
	tokenHash, tokenOk := cryptoStatusTokenHash(token)
	intentId, err := server.ParseId(strings.TrimSpace(intentIdText))
	if err != nil || !tokenOk {
		return nil
	}
	intent := model.GetCryptoPaymentIntent(ctx, intentId)
	if intent == nil {
		dummy := &model.CryptoPaymentIntent{StatusTokenHash: make([]byte, sha256.Size)}
		dummy.MatchesStatusToken(tokenHash)
		return nil
	}
	if !intent.MatchesStatusToken(tokenHash) {
		return nil
	}
	return intent
}

// CryptoPaymentStatus answers POST /pay/crypto/status. It reads Postgres
// only: the scanner keeps the receipts current.
func CryptoPaymentStatus(
	args *CryptoPaymentStatusArgs,
	clientSession *session.ClientSession,
) (*CryptoPaymentStatusResult, error) {
	if !payDataLookupLimiter.allow(clientSession) {
		return nil, errors.New("429 Too many status checks from this address. Try again in a minute.")
	}
	ctx := clientSession.Ctx
	intent := cryptoLoadIntentForToken(ctx, args.IntentId, args.StatusToken)
	if intent == nil {
		return &CryptoPaymentStatusResult{Status: CryptoPaymentStatusUnknown}, nil
	}
	config := cryptoPayConfigFunc()
	receipts := []*model.CryptoPaymentReceipt{}
	if intent.Asset != model.CryptoAssetUsdcSolana {
		receipts = model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
	}
	return cryptoStatusResult(ctx, config, intent, receipts, cryptoIntentNetworkName(ctx, intent), server.NowUtc()), nil
}

// cryptoIntentNetworkName is the name of the network an invoice delivers to by
// name, "" for the other deliveries.
func cryptoIntentNetworkName(ctx context.Context, intent *model.CryptoPaymentIntent) string {
	if intent.Delivery != model.CryptoDeliveryNetwork || intent.NetworkId == nil {
		return ""
	}
	if networkName, _, ok := model.GetNetworkAdminUserAuth(ctx, *intent.NetworkId); ok {
		return networkName
	}
	return ""
}

// cryptoStatusResult is the status of §2.5 for an invoice and its receipts.
func cryptoStatusResult(
	ctx context.Context,
	config *CryptoPayConfig,
	intent *model.CryptoPaymentIntent,
	receipts []*model.CryptoPaymentReceipt,
	networkName string,
	now time.Time,
) *CryptoPaymentStatusResult {
	decimals := cryptoAssetDecimals(intent.Asset)
	plan, itemId := cryptoSplitItem(intent.Item)
	expiresAt := intent.ExpiresAt
	view := cryptoEvaluateIntent(intent, receipts, cryptoIntentPolicyFor(config, intent.Asset), now)
	result := &CryptoPaymentStatusResult{
		IntentId:              intent.IntentId.String(),
		Asset:                 intent.Asset,
		Plan:                  plan,
		ItemId:                itemId,
		Delivery:              intent.Delivery,
		NetworkName:           networkName,
		Status:                view.Status,
		ReviewReason:          view.ReviewReason,
		Amount:                cryptoFormatAtomic(intent.AmountAtomic, decimals),
		AmountAtomic:          fmt.Sprintf("%d", intent.AmountAtomic),
		Received:              cryptoFormatAtomic(view.ReceivedAtomic, decimals),
		ReceivedAtomic:        fmt.Sprintf("%d", view.ReceivedAtomic),
		Remaining:             cryptoFormatAtomic(view.RemainingAtomic, decimals),
		RemainingAtomic:       fmt.Sprintf("%d", view.RemainingAtomic),
		Confirmations:         view.Confirmations,
		ConfirmationsRequired: cryptoConfirmationsRequired(config, intent.Asset),
		ExpiresAt:             &expiresAt,
		CreditedAt:            intent.CreditedAt,
	}
	switch view.Status {
	case CryptoPaymentStatusExpired:
		result.Error = &CryptoPaymentError{Code: PurchaseErrorCodeQuoteExpired, Message: "The quote expired with nothing received."}
	case CryptoPaymentStatusReview:
		if view.ReviewReason == model.CryptoReviewReasonUnderpaid || view.ReviewReason == model.CryptoReviewReasonLateUnderpaid {
			result.Error = &CryptoPaymentError{Code: PurchaseErrorCodeUnderpaid, Message: "The payment is short of the quote. Contact support."}
		}
	case CryptoPaymentStatusPaid:
		if intent.Delivery == model.CryptoDeliveryCode && intent.BalanceCodeId != nil {
			result.BalanceCode = model.GetBalanceCodeSecretInConn(nil, ctx, *intent.BalanceCodeId)
		}
	}
	return result
}

// ----- requote -----

// CryptoPaymentRequote answers POST /pay/crypto/requote: a fresh quote for an
// invoice nothing was received for yet. XMR and ZEC get a new rate, amount and
// expiry for the same address (the old quote stays in the history); USDC's
// amount is already in dollars, so only its expiry moves.
func CryptoPaymentRequote(
	args *CryptoPaymentStatusArgs,
	clientSession *session.ClientSession,
) (*CryptoPaymentIntentResult, error) {
	if !payDataCheckoutLimiter.allow(clientSession) {
		return cryptoIntentError(
			PurchaseErrorCodeRateLimited,
			"Too many payment attempts from this address. Try again in a minute.",
		), nil
	}
	ctx := clientSession.Ctx
	intent := cryptoLoadIntentForToken(ctx, args.IntentId, args.StatusToken)
	if intent == nil {
		return cryptoIntentError(PurchaseErrorCodeInvalidRequest, "Unknown invoice."), nil
	}
	config := cryptoPayConfigFunc()
	now := server.NowUtc()
	receipts := []*model.CryptoPaymentReceipt{}
	if intent.Asset != model.CryptoAssetUsdcSolana {
		receipts = model.GetCryptoPaymentReceiptsForIntent(ctx, intent.IntentId)
	}
	view := cryptoEvaluateIntent(intent, receipts, cryptoIntentPolicyFor(config, intent.Asset), now)
	if view.Status != CryptoPaymentStatusPending && view.Status != CryptoPaymentStatusExpired {
		return cryptoIntentError(PurchaseErrorCodeInvalidRequest, "This invoice can no longer be re-quoted."), nil
	}
	if intent.VoidedAt != nil {
		return cryptoIntentError(PurchaseErrorCodeInvalidRequest, "This invoice is closed."), nil
	}
	if !cryptoAssetEnabled(config, intent.Asset) {
		cryptoPayQuoteResults.WithLabelValues(intent.Asset, cryptoQuoteRefusedDisabled).Inc()
		return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
	}
	networkName := cryptoIntentNetworkName(ctx, intent)
	expiresAt := now.Add(cryptoAssetQuoteTtl(config, intent.Asset))

	if intent.Asset == model.CryptoAssetUsdcSolana {
		if intent.SolanaReference == nil || !model.ExtendSolanaCryptoPaymentIntent(ctx, intent.IntentId, *intent.SolanaReference, now, expiresAt) {
			return cryptoIntentError(PurchaseErrorCodeInvalidRequest, "This invoice can no longer be re-quoted."), nil
		}
	} else {
		if available, reason := cryptoAssetAvailable(ctx, config, intent.Asset); !available {
			cryptoPayQuoteResults.WithLabelValues(intent.Asset, reason).Inc()
			return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
		}
		rateQuote, reason := cryptoQuoteRate(ctx, config, intent.Asset)
		if rateQuote == nil {
			cryptoPayQuoteResults.WithLabelValues(intent.Asset, reason).Inc()
			return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
		}
		cryptoPayRateSpread.WithLabelValues(intent.Asset).Set(rateQuote.SpreadPercent)
		priceUsd := model.NanoCentsToUsd(intent.PriceUsdNanoCents)
		amountAtomic, ok := cryptoQuoteAmountAtomic(priceUsd, rateQuote.RateUsd, config.Quote.BufferPercent, cryptoAssetDecimals(intent.Asset))
		if !ok {
			cryptoPayQuoteResults.WithLabelValues(intent.Asset, cryptoQuoteRefusedBadAmount).Inc()
			return cryptoIntentError(PurchaseErrorCodeAssetUnavailable, cryptoAssetUnavailableMessage), nil
		}
		if !model.RequoteCryptoPaymentIntent(ctx, intent.IntentId, &model.CryptoPaymentQuote{
			QuotedAt:        now,
			AmountAtomic:    amountAtomic,
			RateUsd:         rateQuote.RateUsd,
			RateSourcesJson: cryptoRateSourcesJson(rateQuote),
			ExpiresAt:       expiresAt,
		}) {
			// a payment arrived or a credit landed since the status was read
			return cryptoIntentError(PurchaseErrorCodeInvalidRequest, "This invoice can no longer be re-quoted."), nil
		}
		cryptoPayQuoteResults.WithLabelValues(intent.Asset, "requoted").Inc()
	}

	requoted := model.GetCryptoPaymentIntent(ctx, intent.IntentId)
	if requoted == nil {
		return cryptoIntentError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage), nil
	}
	if glog.V(1) {
		glog.Infof("[cryptopay]requoted invoice %s\n", intent.IntentId)
	}
	// the caller's own token: re-quoting does not change who may read it
	return cryptoIntentResult(ctx, config, requoted, strings.TrimSpace(args.StatusToken), networkName, now), nil
}

// ----- the state of an invoice (§2.5, ZCASH.md §4.10) -----

// cryptoIntentPolicy is what judging an invoice needs from the config.
type cryptoIntentPolicy struct {
	decimals            int
	acceptPercent       float64
	creditConfirmations int64
}

func cryptoIntentPolicyFor(config *CryptoPayConfig, asset string) *cryptoIntentPolicy {
	policy := &cryptoIntentPolicy{
		decimals:            cryptoAssetDecimals(asset),
		acceptPercent:       config.Quote.AcceptPercent,
		creditConfirmations: 1,
	}
	if assetConfig := config.Asset(asset); assetConfig != nil && 0 < assetConfig.CreditConfirmations {
		policy.creditConfirmations = assetConfig.CreditConfirmations
	}
	return policy
}

// What the scanner does with an invoice.
type cryptoIntentAction int

const (
	cryptoIntentActionNone cryptoIntentAction = iota
	// credit it (crypto_pay_credit.go) with CreditUsd and CreditReceipts
	cryptoIntentActionCredit
	// move ReviewReceipts to review with ActionReason
	cryptoIntentActionReview
)

// cryptoIntentView is an invoice judged at a moment: the status the buyer
// sees and what the scanner does about it.
type cryptoIntentView struct {
	Status          string
	ReviewReason    string
	ReceivedAtomic  int64
	RemainingAtomic int64
	Confirmations   int

	Action       cryptoIntentAction
	ActionReason string
	// the credit's USD value: the price on time, the receipts' first-seen
	// value capped at the price when late
	CreditUsd model.NanoCents
	Late      bool
	// the receipts that become credited, with each one's share of the value
	CreditReceipts  []model.CryptoPaymentReceiptKey
	ReceiptUsdValue map[model.CryptoPaymentReceiptKey]model.NanoCents
	// the receipts that go to review
	ReviewReceipts []model.CryptoPaymentReceiptKey
	// a late receipt has no first-seen rate yet, so the late payment cannot be
	// valued until the scanner records one
	NeedsRate bool
}

// cryptoReceiptCounts reports whether a receipt is part of what was paid
// toward an open invoice: in the mempool (unless the wallet flags it as a
// double spend: it counts once mined, mmm/ZCASH-CONTRACT.md §3.13.3) or a block, or
// held for an underpayment (a top-up can still make the invoice whole).
func cryptoReceiptCounts(receipt *model.CryptoPaymentReceipt) bool {
	switch receipt.State {
	case model.CryptoReceiptStateSeen:
		return receipt.ReviewReason != model.CryptoReviewReasonDoubleSpend
	case model.CryptoReceiptStateConfirmed:
		return true
	case model.CryptoReceiptStateReview:
		return receipt.ReviewReason == model.CryptoReviewReasonUnderpaid ||
			receipt.ReviewReason == model.CryptoReviewReasonLateUnderpaid
	}
	return false
}

// The review reasons only an operator clears, in the order the buyer is told
// about them when several apply: a credited payment that left the chain
// (its grant was ended), a credit that kept failing, a payment to a voided
// invoice, a second payment to a paid one.
var cryptoHeldReviewReasons = []string{
	model.CryptoReviewReasonOrphaned,
	model.CryptoReviewReasonCreditFailed,
	model.CryptoReviewReasonVoided,
	model.CryptoReviewReasonExtra,
}

// cryptoReceiptHeld reports a receipt an operator holds: in review for a
// reason no payment can clear.
func cryptoReceiptHeld(receipt *model.CryptoPaymentReceipt) bool {
	return receipt.State == model.CryptoReceiptStateReview && slices.Contains(cryptoHeldReviewReasons, receipt.ReviewReason)
}

// cryptoEvaluateIntent judges an invoice and its receipts at now. Pure: the
// status endpoint and the scanner share it, and the tests drive it directly.
//
//	credited                  paid (review/orphaned if a credited payment left the chain)
//	voided                    review/voided with anything received or held, else expired
//	held by an operator       review with the reason (orphaned: its credit was taken back
//	                          because the payment left the chain; credit_failed; extra)
//	nothing received          pending, or expired past the quote
//	received on time >= 99.5% confirming until the payment is at credit depth, then credit at the price
//	past the quote, short     the late value at first-seen rates covers the price: credit
//	                          at that value capped at the price; else review (underpaid
//	                          when anything arrived on time, late_underpaid otherwise)
//	before expiry, short      seen, with what remains to be sent
func cryptoEvaluateIntent(
	intent *model.CryptoPaymentIntent,
	receipts []*model.CryptoPaymentReceipt,
	policy *cryptoIntentPolicy,
	now time.Time,
) *cryptoIntentView {
	view := &cryptoIntentView{ReceiptUsdValue: map[model.CryptoPaymentReceiptKey]model.NanoCents{}}

	if intent.Asset == model.CryptoAssetUsdcSolana {
		// Helius reports confirmed transfers only, and the Solana intent is
		// credited in one step: pending, paid or expired
		switch {
		case intent.CreditedAt != nil:
			view.Status = CryptoPaymentStatusPaid
			view.ReceivedAtomic = intent.AmountAtomic
			view.Confirmations = 1
		case intent.VoidedAt != nil || intent.ExpiresAt.Before(now):
			// a voided invoice is closed: a payment to it is recorded as an
			// unfulfilled Solana payment for an operator
			view.Status = CryptoPaymentStatusExpired
			view.RemainingAtomic = intent.AmountAtomic
		default:
			view.Status = CryptoPaymentStatusPending
			view.RemainingAtomic = intent.AmountAtomic
		}
		return view
	}

	if intent.CreditedAt != nil {
		view.Status = CryptoPaymentStatusPaid
		for _, receipt := range receipts {
			switch {
			case receipt.State == model.CryptoReceiptStateCredited:
				view.ReceivedAtomic += receipt.AmountAtomic
			case receipt.State == model.CryptoReceiptStateReview && receipt.ReviewReason == model.CryptoReviewReasonOrphaned:
				// the credited payment left the chain and its entitlement was
				// ended: the buyer must not see "paid"
				view.Status = CryptoPaymentStatusReview
				view.ReviewReason = model.CryptoReviewReasonOrphaned
			case (receipt.State == model.CryptoReceiptStateSeen || receipt.State == model.CryptoReceiptStateConfirmed) &&
				cryptoReceiptCounts(receipt):
				// a second payment to a paid invoice: never a second credit
				view.Action = cryptoIntentActionReview
				view.ActionReason = model.CryptoReviewReasonExtra
				view.ReviewReceipts = append(view.ReviewReceipts, receipt.Key())
			}
		}
		if view.Status == CryptoPaymentStatusPaid {
			view.Confirmations = int(policy.creditConfirmations)
			for _, receipt := range receipts {
				if receipt.State == model.CryptoReceiptStateCredited && receipt.Confirmations < view.Confirmations {
					view.Confirmations = receipt.Confirmations
				}
			}
		}
		return view
	}

	counted := []*model.CryptoPaymentReceipt{}
	held := []*model.CryptoPaymentReceipt{}
	for _, receipt := range receipts {
		switch {
		case cryptoReceiptCounts(receipt):
			counted = append(counted, receipt)
		case cryptoReceiptHeld(receipt):
			held = append(held, receipt)
		}
	}
	for _, receipt := range counted {
		view.ReceivedAtomic += receipt.AmountAtomic
	}
	for _, receipt := range held {
		view.ReceivedAtomic += receipt.AmountAtomic
	}

	if intent.VoidedAt != nil {
		// what was paid to a voided invoice is held for an operator, and stays
		// review/voided once the scanner has moved it there (mmm/ZCASH-CONTRACT.md
		// §3.13.12): a voided invoice is never credited
		view.RemainingAtomic = 0
		if 0 < len(counted)+len(held) {
			view.Status = CryptoPaymentStatusReview
			view.ReviewReason = model.CryptoReviewReasonVoided
			for _, receipt := range counted {
				view.ReviewReceipts = append(view.ReviewReceipts, receipt.Key())
			}
			if 0 < len(view.ReviewReceipts) {
				view.Action = cryptoIntentActionReview
				view.ActionReason = model.CryptoReviewReasonVoided
			}
		} else {
			view.Status = CryptoPaymentStatusExpired
		}
		return view
	}

	if 0 < len(held) {
		// an operator holds part of it: no credit and no new judgment until
		// they act (bringyourctl crypto credit)
		view.Status = CryptoPaymentStatusReview
		view.RemainingAtomic = 0
		for _, reason := range cryptoHeldReviewReasons {
			if slices.ContainsFunc(held, func(receipt *model.CryptoPaymentReceipt) bool { return receipt.ReviewReason == reason }) {
				view.ReviewReason = reason
				break
			}
		}
		return view
	}

	threshold := cryptoAcceptThresholdAtomic(intent.AmountAtomic, policy.acceptPercent)
	expired := intent.ExpiresAt.Before(now)
	view.RemainingAtomic = max(0, intent.AmountAtomic-view.ReceivedAtomic)

	if len(counted) == 0 {
		if expired {
			view.Status = CryptoPaymentStatusExpired
		} else {
			view.Status = CryptoPaymentStatusPending
		}
		return view
	}

	onTime := []*model.CryptoPaymentReceipt{}
	var onTimeAtomic int64
	for _, receipt := range counted {
		if !receipt.FirstSeenAt.After(intent.ExpiresAt) {
			onTime = append(onTime, receipt)
			onTimeAtomic += receipt.AmountAtomic
		}
	}

	if threshold <= onTimeAtomic {
		// paid in time: credit at the price once the receipts that make up
		// the payment are deep enough
		confirmations := cryptoCoveringConfirmations(onTime, func(receipt *model.CryptoPaymentReceipt) int64 {
			return receipt.AmountAtomic
		}, threshold)
		view.Confirmations = confirmations
		view.RemainingAtomic = 0
		if int64(confirmations) < policy.creditConfirmations {
			view.Status = CryptoPaymentStatusConfirming
			return view
		}
		view.Status = CryptoPaymentStatusConfirming
		view.Action = cryptoIntentActionCredit
		view.CreditUsd = intent.PriceUsdNanoCents
		for _, receipt := range counted {
			view.CreditReceipts = append(view.CreditReceipts, receipt.Key())
			view.ReceiptUsdValue[receipt.Key()] = cryptoOnTimeShareNanoCents(intent, receipt.AmountAtomic)
		}
		return view
	}

	if !expired {
		// short before the quote ends: the buyer can still top up
		view.Status = CryptoPaymentStatusSeen
		return view
	}

	// Past the quote and short of it on time. A late payment is valued at the
	// rate when it was first seen (late is not fraudulent); a receipt seen on
	// time keeps its share of the quoted price.
	values := map[model.CryptoPaymentReceiptKey]model.NanoCents{}
	var lateValue model.NanoCents
	for _, receipt := range counted {
		var value model.NanoCents
		if !receipt.FirstSeenAt.After(intent.ExpiresAt) {
			value = cryptoOnTimeShareNanoCents(intent, receipt.AmountAtomic)
		} else {
			if receipt.FirstSeenRateUsd == nil {
				view.NeedsRate = true
				continue
			}
			value = cryptoAtomicUsdValueNanoCents(receipt.AmountAtomic, policy.decimals, *receipt.FirstSeenRateUsd)
		}
		values[receipt.Key()] = value
		lateValue += value
	}
	acceptValue := cryptoAcceptValueNanoCents(intent.PriceUsdNanoCents, policy.acceptPercent)
	if acceptValue <= lateValue && !view.NeedsRate {
		valued := []*model.CryptoPaymentReceipt{}
		for _, receipt := range counted {
			if _, ok := values[receipt.Key()]; ok {
				valued = append(valued, receipt)
			}
		}
		confirmations := cryptoCoveringConfirmations(valued, func(receipt *model.CryptoPaymentReceipt) int64 {
			return values[receipt.Key()]
		}, acceptValue)
		view.Confirmations = confirmations
		view.RemainingAtomic = 0
		view.Status = CryptoPaymentStatusConfirming
		view.Late = true
		if int64(confirmations) < policy.creditConfirmations {
			return view
		}
		view.Action = cryptoIntentActionCredit
		view.CreditUsd = min(lateValue, intent.PriceUsdNanoCents)
		for _, receipt := range counted {
			view.CreditReceipts = append(view.CreditReceipts, receipt.Key())
			view.ReceiptUsdValue[receipt.Key()] = values[receipt.Key()]
		}
		return view
	}
	if view.NeedsRate {
		// cannot value the late part yet; the scanner records its rate
		view.Status = CryptoPaymentStatusSeen
		return view
	}

	reason := model.CryptoReviewReasonLateUnderpaid
	if 0 < len(onTime) {
		reason = model.CryptoReviewReasonUnderpaid
	}
	view.Status = CryptoPaymentStatusReview
	view.ReviewReason = reason
	for _, receipt := range counted {
		if receipt.State != model.CryptoReceiptStateReview || receipt.ReviewReason != reason {
			view.ReviewReceipts = append(view.ReviewReceipts, receipt.Key())
		}
	}
	if 0 < len(view.ReviewReceipts) {
		view.Action = cryptoIntentActionReview
		view.ActionReason = reason
	}
	return view
}

// cryptoCoveringConfirmations is the confirmations a payment has: the lowest
// among the deepest receipts that together reach target (0 for the mempool).
func cryptoCoveringConfirmations(
	receipts []*model.CryptoPaymentReceipt,
	weight func(*model.CryptoPaymentReceipt) int64,
	target int64,
) int {
	sorted := slices.Clone(receipts)
	slices.SortStableFunc(sorted, func(a *model.CryptoPaymentReceipt, b *model.CryptoPaymentReceipt) int {
		// deepest first, then the first seen
		if a.Confirmations != b.Confirmations {
			return b.Confirmations - a.Confirmations
		}
		return a.FirstSeenAt.Compare(b.FirstSeenAt)
	})
	var total int64
	confirmations := 0
	for i, receipt := range sorted {
		if i == 0 || receipt.Confirmations < confirmations {
			confirmations = receipt.Confirmations
		}
		total += weight(receipt)
		if target <= total {
			break
		}
	}
	return confirmations
}

// cryptoOnTimeShareNanoCents is the share of the quoted price an on-time
// receipt carries: the price times its part of the quoted amount.
func cryptoOnTimeShareNanoCents(intent *model.CryptoPaymentIntent, amountAtomic int64) model.NanoCents {
	if intent.AmountAtomic <= 0 {
		return 0
	}
	share := new(big.Rat).SetFrac64(amountAtomic, intent.AmountAtomic)
	share.Mul(share, new(big.Rat).SetInt64(intent.PriceUsdNanoCents))
	floor := new(big.Int).Quo(share.Num(), share.Denom())
	if !floor.IsInt64() {
		return intent.PriceUsdNanoCents
	}
	return floor.Int64()
}

// cryptoAcceptValueNanoCents is the least USD value that pays a price: the
// accept percent of it.
func cryptoAcceptValueNanoCents(priceNanoCents model.NanoCents, acceptPercent float64) model.NanoCents {
	return cryptoAcceptThresholdAtomic(priceNanoCents, acceptPercent)
}
