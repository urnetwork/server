package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const InitialTransferBalance = 32 * model.Gib

// 30 days
const InitialTransferBalanceDuration = 30 * 24 * time.Hour

// The recurring per-tier data grants come from pro.yml (model.Pro().DataAmount),
// on three separate schedules -- see FreeGrantWindow / ProGrantWindow /
// ReferralGrantWindow and the three Refresh*TransferBalances tasks below.
//
// RefreshSupporterTransferBalance is the legacy amount still used at subscription
// ACTIVATION, where the balance spans the whole subscription period alongside the
// revenue/subsidy accounting. It is not the recurring meter.
const RefreshSupporterTransferBalance = 600 * model.Gib

const SubscriptionGracePeriod = 24 * time.Hour

const SubscriptionYearDuration = 365 * 24 * time.Hour

const SpecialCompany = "company"

type Skus struct {
	Skus map[string]*Sku `yaml:"skus"`
}

type Sku struct {
	// the fees on the payment amount
	FeeFraction                   float64 `yaml:"fee_fraction"`
	PriceAmountUsd                float64 `yaml:"price_amount_usd,omitempty"`
	BalanceByteCountHumanReadable string  `yaml:"balance_byte_count"`
	Special                       string  `yaml:"special"`
	Supporter                     bool    `yaml:"supporter"`
}

func (self *Sku) BalanceByteCount() model.ByteCount {
	byteCount, err := model.ParseByteCount(self.BalanceByteCountHumanReadable)
	if err != nil {
		panic(err)
	}
	return byteCount
}

var coinbaseWebhookSharedSecret = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("coinbase.yml").Parse()
	return c["webhook"].(map[string]any)["shared_secret"].(string)
})

var coinbaseSkus = sync.OnceValue(func() map[string]*Sku {
	var skus Skus
	server.Config.RequireSimpleResource("coinbase.yml").UnmarshalYaml(&skus)
	return skus.Skus
})

var playPublisherEmail = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("google.yml").Parse()
	return c["webhook"].(map[string]any)["publisher_email"].(string)
})

var playPackageName = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("google.yml").Parse()
	return c["webhook"].(map[string]any)["package_name"].(string)
})

var playSkus = sync.OnceValue(func() map[string]*Sku {
	var skus Skus
	server.Config.RequireSimpleResource("play.yml").UnmarshalYaml(&skus)
	return skus.Skus
})

// var companySenderEmail = sync.OnceValue(func() string {
// 	c := server.Config.RequireSimpleResource("email.yml").Parse()
// 	return c["company_sender_email"].(string)
// })

var playClientId = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("google.yml").Parse()
	return c["oauth"].(map[string]any)["client_id"].(string)
})

var playClientSecret = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("google.yml").Parse()
	return c["oauth"].(map[string]any)["client_secret"].(string)
})

var playRefreshToken = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("google.yml").Parse()
	return c["oauth"].(map[string]any)["refresh_token"].(string)
})

// app initially calls "get info"
// then if no wallet, show a button to initialize wallet
// if wallet, show a button to refresh, and to withdraw

type SubscriptionBalanceResult struct {
	/*
	 * StartBalanceByteCount - The available balance the user starts the day with
	 */
	StartBalanceByteCount model.ByteCount `json:"start_balance_byte_count"`
	/**
	 * BalanceByteCount - The remaining balance the user has available
	 */
	BalanceByteCount model.ByteCount `json:"balance_byte_count"`
	/**
	 * OpenTransferByteCount - The total number of bytes tied up in open transfers
	 */
	OpenTransferByteCount model.ByteCount `json:"open_transfer_byte_count"`
	/**
	 * CurrentSubscription - ONE of the active subscriptions, or nil.
	 *
	 * Shipped apple/android/windows/linux clients read this through the sdk and
	 * treat it as the plan indicator, so it keeps its exact single-value meaning.
	 * It cannot name more than one store; use Subscriptions for the full set.
	 */
	CurrentSubscription *Subscription `json:"current_subscription,omitempty"`
	/**
	 * Subscriptions - EVERY store currently billing this network, one entry per
	 * store, so a caller can offer a cancel path for each. A user subscribed on
	 * two stores is charged by both and has to cancel in both places.
	 *
	 * Not gated on Pro: an active renewal row means a store is taking money now,
	 * and that is exactly when the cancel path must be reachable, entitlement
	 * bookkeeping notwithstanding.
	 */
	Subscriptions             []*Subscription          `json:"subscriptions,omitempty"`
	ActiveTransferBalances    []*model.TransferBalance `json:"active_transfer_balances,omitempty"`
	PendingPayoutUsdNanoCents model.NanoCents          `json:"pending_payout_usd_nano_cents"`
	UpdateTime                time.Time                `json:"update_time"`
}

type Subscription struct {
	SubscriptionId server.Id `json:"subscription_id"`
	Store          string    `json:"store"`
	Plan           string    `json:"plan"`
}

func SubscriptionBalance(session *session.ClientSession) (*SubscriptionBalanceResult, error) {
	transferBalances := model.GetActiveTransferBalances(session.Ctx, session.ByJwt.NetworkId)

	netBalanceByteCount := model.ByteCount(0)
	startBalanceByteCount := model.ByteCount(0)

	// Pro comes from pro_model, the single place it is tracked. It is NOT "has a
	// paid balance": a data code is paid but data-only, so that test would report
	// a data-code buyer as Pro.
	isPro := model.IsProNetwork(session.Ctx, session.ByJwt.NetworkId)

	for _, transferBalance := range transferBalances {

		if transferBalance.EndTime.After(server.NowUtc()) {
			netBalanceByteCount += transferBalance.BalanceByteCount
			startBalanceByteCount += transferBalance.StartBalanceByteCount
		}

	}

	openTransferByteCount := model.GetOpenTransferByteCount(session.Ctx, session.ByJwt.NetworkId)

	var currentSubscription *Subscription

	_, market := model.HasSubscriptionRenewal(session.Ctx, session.ByJwt.NetworkId, model.SubscriptionTypeSupporter)

	if isPro {
		currentSubscription = &Subscription{
			Plan: model.SubscriptionTypeSupporter,
		}

		if market != nil {
			currentSubscription.Store = *market
		}
	}

	// one entry per store billing this network. MIN(market) above can only name
	// one of them, so a second store would otherwise be invisible -- and keep
	// charging, with nowhere in the ui to go and cancel it.
	subscriptions := []*Subscription{}
	for _, market := range model.GetActiveSubscriptionRenewalMarkets(
		session.Ctx,
		session.ByJwt.NetworkId,
		model.SubscriptionTypeSupporter,
	) {
		subscriptions = append(subscriptions, &Subscription{
			Store: market,
			Plan:  model.SubscriptionTypeSupporter,
		})
	}

	// FIXME
	pendingPayout := model.ByteCount(0)

	return &SubscriptionBalanceResult{
		BalanceByteCount:          netBalanceByteCount,
		StartBalanceByteCount:     startBalanceByteCount,
		OpenTransferByteCount:     openTransferByteCount,
		CurrentSubscription:       currentSubscription,
		Subscriptions:             subscriptions,
		ActiveTransferBalances:    transferBalances,
		PendingPayoutUsdNanoCents: pendingPayout,
		UpdateTime:                server.NowUtc(),
	}, nil
}

type CoinbaseWebhookArgs struct {
	Event *CoinbaseEvent `json:"event"`
}

type CoinbaseEvent struct {
	Id   string             `json:"id"`
	Type string             `json:"type"`
	Data *CoinbaseEventData `json:"data"`
}

type CoinbaseEventData struct {
	Id          string                      `json:"id"`
	Name        string                      `json:"name"`
	Description string                      `json:"description"`
	Payments    []*CoinbaseEventDataPayment `json:"payments"`
	Checkout    *CoinbaseEventDataCheckout  `json:"checkout"`
	Metadata    *CoinbaseEventDataMetadata  `json:"metadata"`
}

type CoinbaseEventDataCheckout struct {
	Id string `json:"id"`
}

type CoinbaseEventDataMetadata struct {
	Email string `json:"email"`
}

type CoinbaseEventDataPayment struct {
	Net *CoinbaseEventDataPaymentNet `json:"net"`
}

type CoinbaseEventDataPaymentNet struct {
	Local  *CoinbaseEventDataPaymentAmount `json:"local"`
	Crypto *CoinbaseEventDataPaymentAmount `json:"crypto"`
}

type CoinbaseEventDataPaymentAmount struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type CoinbaseWebhookResult struct {
}

func CoinbaseWebhook(
	coinbaseWebhook *CoinbaseWebhookArgs,
	clientSession *session.ClientSession,
) (*CoinbaseWebhookResult, error) {
	// A signed-but-malformed event must be an ERROR, never a panic: a nil deref
	// here would 500 this delivery and every retry of it, forever, with only a
	// stack trace to find it by. The error is still a non-2xx (Coinbase retries
	// and shows the failure in its dashboard), but it says what is missing.
	if coinbaseWebhook.Event == nil {
		return nil, errors.New("Coinbase event missing.")
	}
	if coinbaseWebhook.Event.Type == "charge:confirmed" {
		if coinbaseWebhook.Event.Data == nil {
			return nil, errors.New("Coinbase event data missing.")
		}
		skuName := coinbaseWebhook.Event.Data.Name
		skus := coinbaseSkus()
		if sku, ok := skus[skuName]; ok {
			purchaseEmail := ""
			if coinbaseWebhook.Event.Data.Metadata != nil {
				purchaseEmail = coinbaseWebhook.Event.Data.Metadata.Email
			}
			if purchaseEmail == "" {
				return nil, errors.New("Missing purchase email to send balance code.")
			}

			coinbaseDataJsonBytes, err := json.Marshal(coinbaseWebhook.Event.Data)
			if err != nil {
				return nil, err
			}

			payments := coinbaseWebhook.Event.Data.Payments
			if len(payments) == 0 || payments[0] == nil || payments[0].Net == nil || payments[0].Net.Local == nil {
				return nil, errors.New("Coinbase event has no payment amount.")
			}

			paymentUsd, err := strconv.ParseFloat(payments[0].Net.Local.Amount, 64)
			if err != nil {
				return nil, err
			}
			netRevenue := model.UsdToNanoCents((1.0 - sku.FeeFraction) * paymentUsd)

			err = CreateBalanceCode(
				clientSession.Ctx,
				sku.BalanceByteCount(),
				model.Pro().DataCodeDuration,
				netRevenue,
				coinbaseWebhook.Event.Data.Id,
				string(coinbaseDataJsonBytes),
				purchaseEmail,
				// no network: a Coinbase purchase is not tied to a signed-in session, so
				// the emailed code IS the delivery mechanism
				nil,
			)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("Coinbase sku not found: %s", skuName)
		}

	}
	// else ignore

	return &CoinbaseWebhookResult{}, nil
}

// CreateBalanceCode creates the data code for a purchase and emails it.
//
// When redeemNetworkId is set, the code is ALSO redeemed into that network immediately,
// so the data simply lands. That is the case for a purchase made while SIGNED IN, where
// the Stripe checkout session told us exactly whose network it is
// (client_reference_id). The code is still emailed, as a record.
//
// Without this, a signed-in customer who buys data on the site is emailed a code they
// have to go and find and paste back into the app -- and the confirmation page sits
// there polling for a balance that never arrives, eventually telling them their purchase
// is "taking longer than usual" when in fact it worked perfectly. We know who they are.
// Make the data appear.
//
// redeemNetworkId is nil for purchases where we genuinely do not know the network (the
// Coinbase flow), which is what data codes exist for in the first place.
func CreateBalanceCode(
	ctx context.Context,
	balanceByteCount model.ByteCount,
	duration time.Duration,
	netRevenue model.NanoCents,
	purchaseEventId string,
	purchaseRecord string,
	purchaseEmail string,
	redeemNetworkId *server.Id,
) error {
	// This is a PAID path -- by the time we are here the customer's money has already
	// moved. So this one does NOT no-op like the grants do.
	//
	// A code with a zero duration expires the instant it is created: the customer pays,
	// receives a code, redeems it, and gets nothing, with no error anywhere. Refuse
	// instead. The caller is a webhook, so an error means the provider RETRIES and the
	// failure is visible in their dashboard -- an unfulfilled payment we can see and fix
	// beats a fulfilled one that is worthless.
	if duration <= 0 {
		glog.Errorf(
			"[sub]refusing to create a balance code with a zero duration "+
				"(purchase_event_id = %s). Is pro.yml present?\n",
			purchaseEventId,
		)
		return fmt.Errorf("balance code duration is not configured (pro.yml)")
	}

	// With no email AND no network there is no delivery mechanism at all -- the
	// code would exist and nobody could ever learn it. Refuse so the webhook
	// retries and the failure is visible.
	if purchaseEmail == "" && redeemNetworkId == nil {
		return fmt.Errorf("balance code needs a purchase email or a network to redeem into")
	}

	var balanceCode *model.BalanceCode

	if balanceCodeId, err := model.GetBalanceCodeIdForPurchaseEventId(ctx, purchaseEventId); err == nil {
		// the code was already created for this purchase event -- a webhook retry.
		// Re-send it, and fall through so an earlier failed redeem is retried too.
		balanceCode, err = model.GetBalanceCode(ctx, balanceCodeId)
		if err != nil {
			return err
		}
	} else {
		balanceCode, err = model.CreateBalanceCode(
			ctx,
			balanceByteCount,
			duration,
			netRevenue,
			purchaseEventId,
			purchaseRecord,
			purchaseEmail,
		)
		if err != nil {
			return err
		}
	}

	if redeemNetworkId != nil {
		_, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: *redeemNetworkId,
		}, ctx)
		if err != nil {
			// Do NOT fail the webhook here. Stripe retries a failed webhook, and every
			// retry would re-send the email -- so a transient redeem error would turn
			// into a stream of duplicate emails. The most likely "error" is simply that
			// the code is already redeemed (this IS the retry), in which case the data is
			// already where it belongs.
			//
			// The customer is not stranded either way: they hold the emailed code and can
			// redeem it by hand.
			glog.Infof(
				"[sub]balance code %s redeem into network %s: %s\n",
				balanceCode.BalanceCodeId, *redeemNetworkId, err,
			)
		} else {
			glog.Infof(
				"[sub]balance code %s redeemed into network %s (%s)\n",
				balanceCode.BalanceCodeId, *redeemNetworkId,
				model.ByteCountHumanReadable(balanceCode.BalanceByteCount),
			)
		}
	}

	// No email on the purchase: nothing to send. This only happens when
	// redeemNetworkId was set (the caller guards the both-empty case), so the
	// credit has already landed in the right network above -- delivery is done.
	if balanceCode.PurchaseEmail == "" {
		return nil
	}

	awsMessageSender := GetAWSMessageSender()

	return awsMessageSender.SendAccountMessageTemplate(
		balanceCode.PurchaseEmail,
		&SubscriptionTransferBalanceCodeTemplate{
			Secret:           balanceCode.Secret,
			BalanceByteCount: balanceCode.BalanceByteCount,
		},
	)
}

type RedeemBalanceCodeArgs struct {
	Secret string `json:"secret"`
}

func RedeemBalanceCode(
	redeemBalanceCode RedeemBalanceCodeArgs,
	session *session.ClientSession,
) (*model.RedeemBalanceCodeResult, error) {

	return model.RedeemBalanceCode(
		&model.RedeemBalanceCodeArgs{
			Secret:    redeemBalanceCode.Secret,
			NetworkId: session.ByJwt.NetworkId,
		},
		session.Ctx,
	)
}

// https://developers.google.com/android-publisher/authorization
func playAuth(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Add("grant_type", "refresh_token")
	form.Add("client_id", playClientId())
	form.Add("client_secret", playClientSecret())
	form.Add("refresh_token", playRefreshToken())

	result, err := server.HttpPostForm(
		ctx,
		"https://accounts.google.com/o/oauth2/token",
		form,
		server.NoCustomHeaders,
		server.ResponseJsonObject[map[string]any],
	)
	if err != nil {
		return "", err
	}

	tokenType := result["token_type"]
	accessToken := result["access_token"]

	if tokenType == "Bearer" {
		return fmt.Sprintf("Bearer %s", accessToken), nil
	}
	return "", errors.New("Could not auth.")
}

func playAuthHeader(ctx context.Context, header http.Header) {
	if auth, err := playAuth(ctx); err == nil {
		header.Add("Authorization", auth)
	}
}

type PlayRtdnMessage struct {
	Version                  string                        `json:"version"`
	PackageName              string                        `json:"packageName"`
	SubscriptionNotification *PlaySubscriptionNotification `json:"subscriptionNotification,omitempty"`
}

type PlaySubscriptionNotification struct {
	Version          string `json:"version"`
	NotificationType int    `json:"notificationType"`
	PurchaseToken    string `json:"purchaseToken"`
	SubscriptionId   string `json:"subscriptionId"`
}

// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2#SubscriptionPurchaseV2
type PlaySubscription struct {
	LineItems []*PlaySubscriptionPurchaseLineItem `json:"lineItems"`
	StartTime string                              `json:"startTime"`
	// values:
	// - SUBSCRIPTION_STATE_UNSPECIFIED
	// - SUBSCRIPTION_STATE_PENDING
	// - SUBSCRIPTION_STATE_ACTIVE
	// - SUBSCRIPTION_STATE_PAUSED
	// - SUBSCRIPTION_STATE_IN_GRACE_PERIOD
	// - SUBSCRIPTION_STATE_ON_HOLD
	// - SUBSCRIPTION_STATE_CANCELED
	// - SUBSCRIPTION_STATE_EXPIRED
	// - SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED
	SubscriptionState string `json:"subscriptionState"`
	// values:
	// - ACKNOWLEDGEMENT_STATE_UNSPECIFIED
	// - ACKNOWLEDGEMENT_STATE_PENDING
	// - ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED
	AcknowledgementState       string                          `json:"acknowledgementState"`
	ExternalAccountIdentifiers *PlayExternalAccountIdentifiers `json:"externalAccountIdentifiers"`
	SubscribeWithGoogleInfo    *PlaySubscribeWithGoogleInfo    `json:"subscribeWithGoogleInfo,omitempty"`
}

func (self *PlaySubscription) ParseStartTime() (time.Time, error) {
	return time.Parse(time.RFC3339, self.StartTime)
}

func (self *PlaySubscription) RequireStartTime() time.Time {
	t, err := self.ParseStartTime()
	if err != nil {
		panic(err)
	}
	return t
}

type PlayExternalAccountIdentifiers struct {
	ExternalAccountId           string `json:"externalAccountId,omitempty"`
	ObfuscatedExternalAccountId string `json:"obfuscatedExternalAccountId,omitempty"`
	ObfuscatedExternalProfileId string `json:"obfuscatedExternalProfileId,omitempty"`
}

type PlaySubscribeWithGoogleInfo struct {
	EmailAddress string `json:"emailAddress,omitempty"`
}

type PlaySubscriptionPurchaseLineItem struct {
	ProductId  string `json:"productId"`
	ExpiryTime string `json:"expiryTime"`
}

func (self *PlaySubscriptionPurchaseLineItem) ParseExpiryTime() (time.Time, error) {
	return time.Parse(time.RFC3339, self.ExpiryTime)
}

func (self *PlaySubscriptionPurchaseLineItem) RequireExpiryTime() time.Time {
	t, err := self.ParseExpiryTime()
	if err != nil {
		panic(err)
	}
	return t
}

type PlayWebhookArgs struct {
	Message *PlayWebhookMessage `json:"message"`
}

type PlayWebhookMessage struct {
	Data string `json:"data"`
}

type PlayWebhookResultMessage struct {
	Message string `json:"message"`
}

type PlayWebhookResult struct {
	Message *PlayWebhookResultMessage
}

// SUBSCRIPTION_REVOKED (RTDN notificationType 12): the user was refunded and
// access ends NOW (UPGRADE.md §2 S7). Distinct from SUBSCRIPTION_EXPIRED (13),
// the normal end of a paid period, which never claws anything back.
const playRtdnNotificationTypeRevoked = 12

// Replaceable only by the hermetic Play webhook tests, which stand up a fake
// Android Publisher API (this test env has no google vault/config to read the
// real package name, skus or oauth credentials from). Production never mutates
// these.
var playPublisherApiBaseUrl = "https://androidpublisher.googleapis.com"
var playPackageNameFunc = playPackageName
var playSkusFunc = func() map[string]*Sku { return playSkus() }
var playAuthHeaderFunc = playAuthHeader

// https://developer.android.com/google/play/billing/getting-ready#configure-rtdn
// https://developer.android.com/google/play/billing/rtdn-reference
func PlayWebhook(
	webhookArgs *PlayWebhookArgs,
	clientSession *session.ClientSession,
) (*PlayWebhookResult, error) {

	data, err := base64.StdEncoding.DecodeString(webhookArgs.Message.Data)
	if err != nil {
		return nil, err
	}
	var rtdnMessage *PlayRtdnMessage
	err = json.Unmarshal(data, &rtdnMessage)
	if err != nil {
		return nil, err
	}

	if rtdnMessage.PackageName == playPackageNameFunc() {
		// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2/get
		// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2#SubscriptionPurchaseV2
		// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptions/acknowledge
		if rtdnMessage.SubscriptionNotification != nil {
			url := fmt.Sprintf(
				"%s/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
				playPublisherApiBaseUrl,
				rtdnMessage.PackageName,
				rtdnMessage.SubscriptionNotification.PurchaseToken,
			)
			sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
				clientSession.Ctx,
				url,
				func(header http.Header) {
					playAuthHeaderFunc(clientSession.Ctx, header)
				},
				server.ResponseJsonObject[*PlaySubscription],
			)
			if err != nil {
				if v, ok := err.(*server.HttpStatusError); ok {
					switch v.StatusCode {
					// Gone
					case 410:
						if rtdnMessage.SubscriptionNotification.NotificationType == playRtdnNotificationTypeRevoked {
							// a revoked purchase can be gone from the store
							// entirely; the renewal rows still map the token to
							// its network
							return playHandleRevoked(
								clientSession,
								rtdnMessage.SubscriptionNotification.PurchaseToken,
								"GONE",
							)
						}
						return &PlayWebhookResult{}, nil
					default:
						return nil, err
					}
				} else {
					return nil, err
				}
			}

			glog.Infof("[sub]google play sub: %v\n", sub)

			if rtdnMessage.SubscriptionNotification.NotificationType == playRtdnNotificationTypeRevoked {
				// verified against Google before clawing back: the RTDN push is
				// only as trusted as the state fetch above confirms. A revoked
				// subscription reports EXPIRED (or is 410, handled above); if
				// Play still says ACTIVE, do nothing -- later signals will tell.
				if sub.SubscriptionState == "SUBSCRIPTION_STATE_ACTIVE" {
					glog.Warningf(
						"[sub]play REVOKED notification for token %s but Play reports %s; ignoring\n",
						rtdnMessage.SubscriptionNotification.PurchaseToken,
						sub.SubscriptionState,
					)
					return &PlayWebhookResult{}, nil
				}
				return playHandleRevoked(
					clientSession,
					rtdnMessage.SubscriptionNotification.PurchaseToken,
					sub.SubscriptionState,
				)
			}

			if len(sub.LineItems) == 0 {
				glog.Infof("Google play cannot not renew subscription with zero line items (%s)", rtdnMessage.SubscriptionNotification.PurchaseToken)
				return &PlayWebhookResult{
					Message: &PlayWebhookResultMessage{
						Message: fmt.Sprintf(
							"Google play cannot not renew subscription with zero line items (%s), sub state: (%s), sub aknowledgement: (%s)",
							rtdnMessage.SubscriptionNotification.PurchaseToken,
							sub.SubscriptionState,
							sub.AcknowledgementState,
						),
					},
				}, nil
			}

			var networkId server.Id
			if sub.ExternalAccountIdentifiers != nil {
				if sub.ExternalAccountIdentifiers.ExternalAccountId != "" {
					networkId, err = server.ParseId(sub.ExternalAccountIdentifiers.ExternalAccountId)
					if err != nil {
						return nil, fmt.Errorf("Google Play subscription malformed external account id: \"%s\" = %s", sub.ExternalAccountIdentifiers.ExternalAccountId, err)
					}
				} else if sub.ExternalAccountIdentifiers.ObfuscatedExternalAccountId != "" {
					networkIdOrSubscriptionPaymentId, err := server.ParseId(sub.ExternalAccountIdentifiers.ObfuscatedExternalAccountId)
					if err != nil {
						return nil, fmt.Errorf("Google Play subscription malformed obfuscated external account id: \"%s\" = %s", sub.ExternalAccountIdentifiers.ObfuscatedExternalAccountId, err)
					}
					networkId, err = model.SubscriptionGetNetworkIdForPaymentId(clientSession.Ctx, networkIdOrSubscriptionPaymentId)
					if err != nil {
						// the obfuscated account id is just a plain network id
						networkId = networkIdOrSubscriptionPaymentId
					}
				} else {
					return nil, fmt.Errorf("Google Play subscription missing external account id and obfuscated external account id")
				}
			} else {
				return &PlayWebhookResult{
					Message: &PlayWebhookResultMessage{
						Message: fmt.Sprintf(
							"Google Play subscription no external account information: sub state: (%s), sub aknowledgement: (%s)",
							sub.SubscriptionState,
							sub.AcknowledgementState,
						),
					},
				}, nil
			}

			minExpiryTime := sub.LineItems[0].RequireExpiryTime()
			for _, item := range sub.LineItems[1:] {
				if item.RequireExpiryTime().Before(minExpiryTime) {
					minExpiryTime = item.RequireExpiryTime()
				}
			}

			acknowledgeAndCheckRenewal := true
			switch sub.SubscriptionState {
			case "SUBSCRIPTION_STATE_CANCELED",
				"SUBSCRIPTION_STATE_EXPIRED",
				"SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED":
				acknowledgeAndCheckRenewal = false
			}

			if acknowledgeAndCheckRenewal {
				// Aknowledge. The result MATTERS: a purchase Google never sees
				// acknowledged is auto-refunded after 3 days while any granted
				// balance would stand. A failure here is a non-2xx so Pub/Sub
				// redelivers and the acknowledge is attempted again.
				url := fmt.Sprintf(
					"%s/androidpublisher/v3/applications/%s/purchases/subscriptions/%s/tokens/%s:acknowledge",
					playPublisherApiBaseUrl,
					rtdnMessage.PackageName,
					rtdnMessage.SubscriptionNotification.SubscriptionId,
					rtdnMessage.SubscriptionNotification.PurchaseToken,
				)
				_, err := server.HttpPostRawRequireStatusOk(
					clientSession.Ctx,
					url,
					[]byte{},
					func(header http.Header) {
						playAuthHeaderFunc(clientSession.Ctx, header)
					},
				)
				if err != nil {
					glog.Errorf(
						"[sub]play acknowledge failed for token %s: %s\n",
						rtdnMessage.SubscriptionNotification.PurchaseToken, err,
					)
					return nil, fmt.Errorf("could not acknowledge play purchase: %w", err)
				}

				// fire this immediately since we pull current plan from subscription_renewal table.
				// A credit failure (sku missing from play.yml, Google API failure) is a
				// non-2xx so Pub/Sub RETRIES the delivery -- otherwise the entitlement
				// would arrive only via the task scheduled at the END of the paid
				// period, or never.
				_, err = PlaySubscriptionRenewal(
					&PlaySubscriptionRenewalArgs{
						NetworkId:      networkId,
						PackageName:    rtdnMessage.PackageName,
						SubscriptionId: rtdnMessage.SubscriptionNotification.SubscriptionId,
						PurchaseToken:  rtdnMessage.SubscriptionNotification.PurchaseToken,
					},
					clientSession,
				)
				if err != nil {
					glog.Errorf(
						"[sub]play inline renewal failed for token %s: %s\n",
						rtdnMessage.SubscriptionNotification.PurchaseToken, err,
					)
					return nil, err
				}

				// continually renew as long as the expiry time keeps getting pushed forward
				// note RTDN messages for renewal may unreliably delivered, so Google
				// recommends polling their system around the expiry time
				server.Tx(clientSession.Ctx, func(tx server.PgTx) {
					SchedulePlaySubscriptionRenewal(
						clientSession,
						tx,
						&PlaySubscriptionRenewalArgs{
							NetworkId:      networkId,
							PackageName:    rtdnMessage.PackageName,
							SubscriptionId: rtdnMessage.SubscriptionNotification.SubscriptionId,
							PurchaseToken:  rtdnMessage.SubscriptionNotification.PurchaseToken,
							CheckTime:      minExpiryTime,
						},
					)
				})
			}
		}
	}
	// else unknown package, ignore the message

	return &PlayWebhookResult{}, nil
}

// playHandleRevoked ends a revoked purchase's entitlement: the renewals the
// purchase token bought and the pro balances they granted, with the network
// derived from the renewal rows themselves (a revoked token can be 410-Gone
// at the store). Idempotent: a Pub/Sub redelivery finds nothing left to end
// and records nothing. Always 200 -- a retry cannot do more.
func playHandleRevoked(
	clientSession *session.ClientSession,
	purchaseToken string,
	subscriptionState string,
) (*PlayWebhookResult, error) {
	endedNetworkIds := model.EndReconciledEntitlementForPurchaseToken(
		clientSession.Ctx,
		model.SubscriptionMarketGoogle,
		purchaseToken,
		server.NowUtc(),
	)
	for _, networkId := range endedNetworkIds {
		if err := model.AddPaymentReconciliationEvent(clientSession.Ctx, &model.PaymentReconciliationEvent{
			RunId:     server.NewId(),
			Store:     model.SubscriptionMarketGoogle,
			NetworkId: &networkId,
			Action:    model.PaymentReconcileActionRevoked,
			Evidence:  purchaseToken,
			Details: map[string]any{
				"subscription_state": subscriptionState,
			},
		}); err != nil {
			// the audit trail must never turn a completed clawback into a
			// failed delivery (Pub/Sub would redeliver into a no-op)
			glog.Errorf("[sub]play revoked token %s: could not record event: %s\n", purchaseToken, err)
		}
	}
	glog.Infof(
		"[sub]play revoked token %s (%s): ended %d network(s)\n",
		purchaseToken, subscriptionState, len(endedNetworkIds),
	)
	return &PlayWebhookResult{}, nil
}

type PlaySubscriptionRenewalArgs struct {
	NetworkId      server.Id `json:"network_id"`
	PackageName    string    `json:"package_name"`
	SubscriptionId string    `json:"subscription_id"`
	PurchaseToken  string    `json:"purchase_token"`
	CheckTime      time.Time `json:"check_time"`
	// ExpiryTime time.Time `json:"expiry_time"`
}

type PlaySubscriptionRenewalResult struct {
	Canceled   bool      `json:"canceled"`
	ExpiryTime time.Time `json:"expiry_time"`
	Renewed    bool      `json:"renewed"`
}

func SchedulePlaySubscriptionRenewal(
	clientSession *session.ClientSession,
	tx server.PgTx,
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
) {
	task.ScheduleTaskInTx(
		tx,
		PlaySubscriptionRenewal,
		playSubscriptionRenewal,
		clientSession,
		task.RunOnce("play_subscription_renewal", playSubscriptionRenewal.PurchaseToken),
		task.RunAt(playSubscriptionRenewal.CheckTime),
	)
}

func PlaySubscriptionRenewal(
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	clientSession *session.ClientSession,
) (*PlaySubscriptionRenewalResult, error) {

	url := fmt.Sprintf(
		"%s/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
		playPublisherApiBaseUrl,
		playSubscriptionRenewal.PackageName,
		playSubscriptionRenewal.PurchaseToken,
	)
	sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
		clientSession.Ctx,
		url,
		func(header http.Header) {
			playAuthHeaderFunc(clientSession.Ctx, header)
		},
		server.ResponseJsonObject[*PlaySubscription],
	)
	if err != nil {
		if v, ok := err.(*server.HttpStatusError); ok {
			switch v.StatusCode {
			// Gone
			case 410:
				return &PlaySubscriptionRenewalResult{
					Canceled: true,
				}, nil
			default:
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	if len(sub.LineItems) == 0 {
		return nil, fmt.Errorf("Google play cannot not renew subscription with zero line items (%s)", playSubscriptionRenewal.PurchaseToken)
	}

	maxExpiryTime := sub.LineItems[0].RequireExpiryTime()
	minExpiryTime := maxExpiryTime
	for _, item := range sub.LineItems[1:] {
		if maxExpiryTime.Before(item.RequireExpiryTime()) {
			maxExpiryTime = item.RequireExpiryTime()
		} else if item.RequireExpiryTime().Before(minExpiryTime) {
			minExpiryTime = item.RequireExpiryTime()
		}
	}
	for _, item := range sub.LineItems[1:] {
		if maxExpiryTime.Before(item.RequireExpiryTime()) {
			maxExpiryTime = item.RequireExpiryTime()
		}
	}
	startTime, err := sub.ParseStartTime()
	if err != nil {
		return nil, err
	}

	active := false
	canceled := false
	switch sub.SubscriptionState {
	case "SUBSCRIPTION_STATE_ACTIVE":
		active = true
	case "SUBSCRIPTION_STATE_CANCELED",
		"SUBSCRIPTION_STATE_EXPIRED":
		canceled = true
	}

	if canceled {
		return &PlaySubscriptionRenewalResult{
			Canceled: true,
		}, nil
	}

	if active {
		if _, err := model.GetOverlappingTransferBalance(clientSession.Ctx, playSubscriptionRenewal.PurchaseToken, maxExpiryTime); err != nil {
			skus := playSkusFunc()
			skuName := playSubscriptionRenewal.SubscriptionId
			sku, ok := skus[skuName]
			if !ok {
				return nil, fmt.Errorf("Play sku not found: %s", skuName)
			}

			// The overlap check above is only a fast path: the inline webhook call
			// and the scheduled renewal task can both pass it for the same token at
			// once, and then both credit. So the credit happens in ONE tx that
			// serializes on the purchase token (advisory xact lock, released at
			// commit/rollback) and RE-CHECKS the overlap inside -- whichever of the
			// two racers gets the lock second sees the balance the first one wrote
			// and adds nothing.
			renewed := false
			var creditErr error
			// ReadCommitted, NOT the default RepeatableRead: the gate is
			// lock-then-recheck, and under RepeatableRead the second racer's
			// snapshot is pinned by the lock statement itself, taken BEFORE it
			// blocks -- after the winner commits, the loser re-checks against
			// the pre-winner snapshot (sees no credit) and then aborts with a
			// serialization failure (40001) on the rows the winner wrote.
			// Per-statement snapshots make the post-lock re-check see the
			// winner's commit, which is the entire point of the re-check.
			server.Tx(clientSession.Ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					clientSession.Ctx,
					`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
					playSubscriptionRenewal.PurchaseToken,
				))

				if _, err := model.GetOverlappingTransferBalanceInTx(tx, clientSession.Ctx, playSubscriptionRenewal.PurchaseToken, maxExpiryTime); err == nil {
					// a concurrent credit for this expiry already landed
					return
				}

				if sku.Supporter {

					endTime := maxExpiryTime.Add(SubscriptionGracePeriod)
					netRevenue := model.UsdToNanoCents((1.0 - sku.FeeFraction) * sku.PriceAmountUsd)

					renewal := &model.SubscriptionRenewal{
						NetworkId:          playSubscriptionRenewal.NetworkId,
						StartTime:          startTime,
						EndTime:            endTime,
						NetRevenue:         netRevenue,
						PurchaseToken:      playSubscriptionRenewal.PurchaseToken,
						SubscriptionType:   model.SubscriptionTypeSupporter,
						SubscriptionMarket: model.SubscriptionMarketGoogle,
					}
					if err := model.AddSubscriptionRenewalInTx(tx, clientSession.Ctx, renewal); err != nil {
						creditErr = err
						return
					}

					// a supporter subscription -> carries the Pro entitlement
					transferBalance := &model.TransferBalance{
						NetworkId:             playSubscriptionRenewal.NetworkId,
						StartTime:             startTime,
						EndTime:               endTime,
						StartBalanceByteCount: RefreshSupporterTransferBalance,
						SubsidyNetRevenue:     netRevenue,
						BalanceByteCount:      RefreshSupporterTransferBalance,
						PurchaseToken:         playSubscriptionRenewal.PurchaseToken,
						Pro:                   true,
					}
					model.AddTransferBalanceInTx(
						clientSession.Ctx,
						tx,
						transferBalance,
					)

				} else {
					// a data pack, NOT a subscription -> data only, never Pro
					transferBalance := &model.TransferBalance{
						NetworkId:             playSubscriptionRenewal.NetworkId,
						StartTime:             startTime,
						EndTime:               maxExpiryTime.Add(SubscriptionGracePeriod),
						StartBalanceByteCount: sku.BalanceByteCount(),
						SubsidyNetRevenue:     model.UsdToNanoCents((1.0 - sku.FeeFraction) * sku.PriceAmountUsd),
						BalanceByteCount:      sku.BalanceByteCount(),
						PurchaseToken:         playSubscriptionRenewal.PurchaseToken,
						Pro:                   false,
					}
					model.AddTransferBalanceInTx(
						clientSession.Ctx,
						tx,
						transferBalance,
					)
				}

				renewed = true
			}, server.TxReadCommitted)
			if creditErr != nil {
				return nil, creditErr
			}

			if renewed {
				if sku.Supporter {
					// the pro balance is committed -- refresh the entitlement so the
					// upgrade is visible immediately rather than after ProCacheTtl
					model.UpdateProNetwork(clientSession.Ctx, playSubscriptionRenewal.NetworkId)
				}

				return &PlaySubscriptionRenewalResult{
					ExpiryTime: minExpiryTime,
					Renewed:    true,
				}, nil
			}
		}
	}

	// not active or
	// a transfer balance was already for the current expiry time
	// hence, the subscription has not been extended/renewed
	return &PlaySubscriptionRenewalResult{
		ExpiryTime: minExpiryTime,
		Renewed:    false,
	}, nil
}

func PlaySubscriptionRenewalPost(
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	playSubscriptionRenewalResult *PlaySubscriptionRenewalResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if playSubscriptionRenewalResult.Canceled {
		return nil
	}

	if playSubscriptionRenewalResult.Renewed {
		// FIXME is the expiry time messed up sometimes?
		playSubscriptionRenewal.CheckTime = server.MaxTime(
			playSubscriptionRenewalResult.ExpiryTime,
			server.NowUtc().Add(1*time.Hour),
		)
		SchedulePlaySubscriptionRenewal(
			clientSession,
			tx,
			playSubscriptionRenewal,
		)
	} else if now := server.NowUtc(); playSubscriptionRenewalResult.ExpiryTime.Before(now) && now.Before(playSubscriptionRenewalResult.ExpiryTime.Add(SubscriptionGracePeriod)) {
		// check again in an hour
		playSubscriptionRenewal.CheckTime = now.Add(1 * time.Hour)
		SchedulePlaySubscriptionRenewal(
			clientSession,
			tx,
			playSubscriptionRenewal,
		)
	} else {
		// else not renewed, stop trying
		userAuth, err := model.GetUserAuth(clientSession.Ctx, playSubscriptionRenewal.NetworkId)
		if err != nil {
			return err
		}

		awsMessageSender := GetAWSMessageSender()
		awsMessageSender.SendAccountMessageTemplate(
			userAuth,
			&SubscriptionEndedTemplate{},
		)
	}

	return nil
}

func VerifyCoinbaseBody(req *http.Request) (io.Reader, error) {
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	// see https://docs.cloud.coinbase.com/commerce-onchain/docs/webhooks-security
	err = coinbaseSignature(bodyBytes, req.Header.Get("X-CC-Webhook-Signature"), coinbaseWebhookSharedSecret())
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(bodyBytes), nil
}

func coinbaseSignature(bodyBytes []byte, header string, secret string) error {
	// see https://docs.cloud.coinbase.com/commerce-onchain/docs/webhooks-security
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(bodyBytes)
	computedSignature := mac.Sum(nil)
	headerSignature, err := hex.DecodeString(header)
	if err != nil {
		return err
	}
	if hmac.Equal(computedSignature, headerSignature) {
		return nil
	}

	return errors.New("Invalid authentication.")
}

func VerifyPlayBody(req *http.Request) (io.Reader, error) {

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	authHeader := req.Header.Get("Authorization")

	if authHeader == "" {
		return nil, errors.New("missing authorization header")
	}

	// see https://cloud.google.com/pubsub/docs/authenticate-push-subscriptions?hl=en#protocol
	err = verifyPlayAuth(req.Context(), authHeader)
	if err != nil {
		glog.Infof("verifyPlayAuth failed: %v", err)
		return nil, err
	}

	return bytes.NewReader(bodyBytes), nil
}

func verifyPlayAuth(ctx context.Context, auth string) error {
	bearerPrefix := "Bearer "

	if strings.HasPrefix(auth, bearerPrefix) {
		jwt := auth[len(bearerPrefix):len(auth)]
		url := fmt.Sprintf("https://oauth2.googleapis.com/tokeninfo?id_token=%s", jwt)

		claimBytes, err := server.HttpGetRawRequireStatusOk(ctx, url, server.NoCustomHeaders)
		if err != nil {
			return err
		}

		// parse the body as a claim map
		var claims map[string]any
		err = json.Unmarshal(claimBytes, &claims)
		if err != nil {
			return err
		}

		if claims["email"] == playPublisherEmail() {
			return nil
		}
	}
	return errors.New("Missing authorization.")
}

// ----- grant windows -----
//
// The three grants run on three different schedules, and each balance's window
// extends a little past the end of its period so consecutive grants overlap and a
// client never sees a gap at the boundary.

// FreeGrantGrace is how long past the end of the day a daily free balance stays
// valid.
const FreeGrantGrace = 1 * time.Hour

// ProGrantGrace is how long past the end of the month a monthly Pro balance stays
// valid. It is also the window in which a lapsed subscriber is still Pro, because
// the Pro entitlement is exactly "has an in-window pro balance" (see pro_model.go).
const ProGrantGrace = 24 * time.Hour

// FreeGrantWindow is the window for the daily free grant covering `now`:
// [start of day, start of next day + 1 hour).
func FreeGrantWindow(now time.Time) (startTime time.Time, endTime time.Time) {
	year, month, day := now.UTC().Date()
	startTime = time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	endTime = startTime.AddDate(0, 0, 1).Add(FreeGrantGrace)
	return
}

// ProGrantWindow is the window for the monthly Pro grant covering `now`:
// [start of month, start of next month + 1 day).
func ProGrantWindow(now time.Time) (startTime time.Time, endTime time.Time) {
	year, month, _ := now.UTC().Date()
	startTime = time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	endTime = startTime.AddDate(0, 1, 0).Add(ProGrantGrace)
	return
}

// ReferralGrantWindow is the window for one referral grant period, from `now`.
func ReferralGrantWindow(now time.Time) (startTime time.Time, endTime time.Time) {
	startTime = now.UTC()
	endTime = startTime.Add(model.Pro().ReferralGrantPeriod()).Add(FreeGrantGrace)
	return
}

// AddRefreshTransferBalance grants one network the data allowance for its CURRENT
// tier and period: a Pro network gets the monthly Pro amount, everyone else gets the
// daily free amount. Used when a network is created and when a subscription changes,
// so the network does not have to wait for the next scheduled grant.
func AddRefreshTransferBalance(ctx context.Context, networkId server.Id) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = AddRefreshTransferBalanceInTx(tx, ctx, networkId)
	})
	return
}

func AddRefreshTransferBalanceInTx(tx server.PgTx, ctx context.Context, networkId server.Id) error {
	pro, _ := model.HasSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeSupporter)

	// Nothing to grant -> grant nothing. With no pro.yml the amount is ZERO, and granting
	// zero is not a no-op: it writes a real transfer_balance row with nothing in it.
	// Keyed off the amount rather than a "was pro.yml loaded" flag, so a pro.yml that is
	// present but says `data: 0` is handled the same way.
	if model.Pro().DataAmount(pro) <= 0 {
		glog.Errorf("[sub]no data amount configured for pro = %t; skipping the grant\n", pro)
		return nil
	}

	if pro {
		// the Pro grant carries pro = true, which is what confers the entitlement
		startTime, endTime := ProGrantWindow(server.NowUtc())
		err := model.AddProTransferBalanceInTx(
			tx,
			ctx,
			networkId,
			model.Pro().DataAmount(true),
			startTime,
			endTime,
		)
		if err != nil {
			return err
		}
		model.UpdateProNetwork(ctx, networkId)
		return nil
	}

	startTime, endTime := FreeGrantWindow(server.NowUtc())
	return model.AddBasicTransferBalanceInTx(
		tx,
		ctx,
		networkId,
		model.Pro().DataAmount(false),
		startTime,
		endTime,
	)
}

// ----- Free grant: runs every day -----

type RefreshFreeTransferBalancesArgs struct {
}

type RefreshFreeTransferBalancesResult struct {
}

func ScheduleRefreshFreeTransferBalances(clientSession *session.ClientSession, tx server.PgTx) {
	// the start of the next day
	year, month, day := server.NowUtc().Date()
	runAt := time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
	task.ScheduleTaskInTx(
		tx,
		RefreshFreeTransferBalances,
		&RefreshFreeTransferBalancesArgs{},
		clientSession,
		task.RunOnce("refresh_free_transfer_balances"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

// RefreshFreeTransferBalances grants the daily free allowance (pro.yml free.data) to
// every network without an active subscription.
func RefreshFreeTransferBalances(
	refreshFreeTransferBalances *RefreshFreeTransferBalancesArgs,
	clientSession *session.ClientSession,
) (*RefreshFreeTransferBalancesResult, error) {
	// Nothing to grant -> grant nothing, rather than write zero-byte balance rows. With no
	// pro.yml this amount is zero. The task stays SCHEDULED, so once pro.yml lands (and
	// the process restarts) the grants resume on their normal cadence by themselves.
	if model.Pro().DataAmount(false) <= 0 {
		glog.Errorf("[sub]RefreshFreeTransferBalances: no amount configured (is pro.yml present?); skipping the grant\n")
		return &RefreshFreeTransferBalancesResult{}, nil
	}

	startTime, endTime := FreeGrantWindow(server.NowUtc())
	model.AddFreeTransferBalanceToAllNetworks(
		clientSession.Ctx,
		startTime,
		endTime,
		model.Pro().DataAmount(false),
	)
	return &RefreshFreeTransferBalancesResult{}, nil
}

func RefreshFreeTransferBalancesPost(
	refreshFreeTransferBalances *RefreshFreeTransferBalancesArgs,
	refreshFreeTransferBalancesResult *RefreshFreeTransferBalancesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRefreshFreeTransferBalances(clientSession, tx)
	return nil
}

// ----- Pro grant: runs every month -----

type RefreshProTransferBalancesArgs struct {
}

type RefreshProTransferBalancesResult struct {
}

func ScheduleRefreshProTransferBalances(clientSession *session.ClientSession, tx server.PgTx) {
	// the start of the next month (time.Date normalizes month 13 to January)
	year, month, _ := server.NowUtc().Date()
	runAt := time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)
	task.ScheduleTaskInTx(
		tx,
		RefreshProTransferBalances,
		&RefreshProTransferBalancesArgs{},
		clientSession,
		task.RunOnce("refresh_pro_transfer_balances"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

// RefreshProTransferBalances grants the FULL monthly Pro allowance (pro.yml pro.data)
// to every network with an active subscription, at the start of the month. The
// balance is not rationed per-day: a Pro network gets the whole 10 TiB up front and
// can spend it however it likes over the month.
func RefreshProTransferBalances(
	refreshProTransferBalances *RefreshProTransferBalancesArgs,
	clientSession *session.ClientSession,
) (*RefreshProTransferBalancesResult, error) {
	// Nothing to grant -> grant nothing, rather than write zero-byte balance rows. With no
	// pro.yml this amount is zero. The task stays SCHEDULED, so once pro.yml lands (and
	// the process restarts) the grants resume on their normal cadence by themselves.
	if model.Pro().DataAmount(true) <= 0 {
		glog.Errorf("[sub]RefreshProTransferBalances: no amount configured (is pro.yml present?); skipping the grant\n")
		return &RefreshProTransferBalancesResult{}, nil
	}

	startTime, endTime := ProGrantWindow(server.NowUtc())
	model.AddProTransferBalanceToAllNetworks(
		clientSession.Ctx,
		startTime,
		endTime,
		model.Pro().DataAmount(true),
	)
	return &RefreshProTransferBalancesResult{}, nil
}

func RefreshProTransferBalancesPost(
	refreshProTransferBalances *RefreshProTransferBalancesArgs,
	refreshProTransferBalancesResult *RefreshProTransferBalancesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRefreshProTransferBalances(clientSession, tx)
	return nil
}

// ----- Referral grant: runs every referral period -----

type RefreshReferralTransferBalancesArgs struct {
}

type RefreshReferralTransferBalancesResult struct {
}

func ScheduleRefreshReferralTransferBalances(clientSession *session.ClientSession, tx server.PgTx) {
	// ReferralGrantPeriod, never the raw ReferralPeriod: a zero period here schedules the
	// task for NOW, and its Post hook reschedules it for now again -- a hot loop.
	runAt := server.NowUtc().Add(model.Pro().ReferralGrantPeriod())
	task.ScheduleTaskInTx(
		tx,
		RefreshReferralTransferBalances,
		&RefreshReferralTransferBalancesArgs{},
		clientSession,
		task.RunOnce("refresh_referral_transfer_balances"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

// RefreshReferralTransferBalances grants the referral bonus to both sides for one
// period, all from pro.yml: the referrer earns bonus_per_referral x min(referrals,
// max_referrals), and each referred network earns referred_bonus. Referrals pay out
// every period for life. The balances are unpaid and pro = false, so referral data
// never confers Pro.
func RefreshReferralTransferBalances(
	refreshReferralTransferBalances *RefreshReferralTransferBalancesArgs,
	clientSession *session.ClientSession,
) (*RefreshReferralTransferBalancesResult, error) {
	// Nothing to grant -> grant nothing, rather than write zero-byte balance rows. With no
	// pro.yml this amount is zero. The task stays SCHEDULED, so once pro.yml lands (and
	// the process restarts) the grants resume on their normal cadence by themselves.
	if model.Pro().ReferralBonus <= 0 && model.Pro().ReferredBonus <= 0 {
		glog.Errorf("[sub]RefreshReferralTransferBalances: no amount configured (is pro.yml present?); skipping the grant\n")
		return &RefreshReferralTransferBalancesResult{}, nil
	}

	startTime, endTime := ReferralGrantWindow(server.NowUtc())
	model.AddReferralBonusesToAllNetworks(
		clientSession.Ctx,
		startTime,
		endTime,
		model.Pro().ReferralBonus,
		model.Pro().ReferredBonus,
	)
	return &RefreshReferralTransferBalancesResult{}, nil
}

func RefreshReferralTransferBalancesPost(
	refreshReferralTransferBalances *RefreshReferralTransferBalancesArgs,
	refreshReferralTransferBalancesResult *RefreshReferralTransferBalancesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRefreshReferralTransferBalances(clientSession, tx)
	return nil
}

/**
 * Apple App Store Webhooks
 */

type AppleNotificationPayload struct {
	SignedPayload string `json:"signedPayload"`
}

type AppleNotificationDecodedPayload struct {
	NotificationType      string                 `json:"notificationType"`
	Subtype               string                 `json:"subtype"`
	NotificationUUID      string                 `json:"notificationUUID"`
	NotificationVersion   string                 `json:"version"`
	SignedDate            int64                  `json:"signedDate"`
	Data                  map[string]interface{} `json:"data"` // need to parse this depending on the notification type
	AppAppleId            int64                  `json:"appAppleId"`
	BundleId              string                 `json:"bundleId"`
	BundleVersion         string                 `json:"bundleVersion"`
	Environment           string                 `json:"environment"`
	Status                int                    `json:"status"`
	SignedRenewalInfo     string                 `json:"signedRenewalInfo"`
	SignedTransactionInfo string                 `json:"signedTransactionInfo"`
	// Populated only by the App Store JWS verifier. Controller code must never
	// decode SignedRenewalInfo or SignedTransactionInfo itself.
	RenewalInfo     map[string]any `json:"-"`
	TransactionInfo map[string]any `json:"-"`
}

/**
 * Helius Webhooks for Solana payments
 */

var heliusAuthSecret = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("helius.yml").Parse()
	return c["helius"].(map[string]any)["webhook_auth_header"].(string)
})

func VerifyHeliusBody(req *http.Request) (io.Reader, error) {
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	secret := req.Header.Get("Authorization")

	if secret != heliusAuthSecret() {
		glog.Infof("[helius] Invalid authentication; dumping all headers")
		return nil, errors.New("Invalid authentication.")
	}

	return bytes.NewReader(bodyBytes), nil
}

type SolanaTransaction struct {
	AccountData      []AccountData          `json:"accountData"`
	Description      string                 `json:"description"`
	Events           map[string]interface{} `json:"events"`
	Fee              int64                  `json:"fee"`
	FeePayer         string                 `json:"feePayer"`
	Instructions     []Instruction          `json:"instructions"`
	NativeTransfers  []NativeTransfer       `json:"nativeTransfers"`
	Signature        string                 `json:"signature"`
	Slot             int64                  `json:"slot"`
	Source           string                 `json:"source"`
	Timestamp        int64                  `json:"timestamp"`
	TokenTransfers   []TokenTransfer        `json:"tokenTransfers"`
	TransactionError interface{}            `json:"transactionError"`
	Type             string                 `json:"type"`
}

type AccountData struct {
	Account             string               `json:"account"`
	NativeBalanceChange int64                `json:"nativeBalanceChange"`
	TokenBalanceChanges []TokenBalanceChange `json:"tokenBalanceChanges"`
}

type TokenBalanceChange struct {
	Mint           string         `json:"mint"`
	RawTokenAmount RawTokenAmount `json:"rawTokenAmount"`
	TokenAccount   string         `json:"tokenAccount"`
	UserAccount    string         `json:"userAccount"`
}

type RawTokenAmount struct {
	Decimals    int    `json:"decimals"`
	TokenAmount string `json:"tokenAmount"`
}

type Instruction struct {
	Accounts          []string           `json:"accounts"`
	Data              string             `json:"data"`
	InnerInstructions []InnerInstruction `json:"innerInstructions"`
	ProgramId         string             `json:"programId"`
}

type InnerInstruction struct {
	Accounts  []string `json:"accounts"`
	Data      string   `json:"data"`
	ProgramId string   `json:"programId"`
}

type NativeTransfer struct {
	Amount          int64  `json:"amount"`
	FromUserAccount string `json:"fromUserAccount"`
	ToUserAccount   string `json:"toUserAccount"`
}

type TokenTransfer struct {
	FromTokenAccount string  `json:"fromTokenAccount"`
	FromUserAccount  string  `json:"fromUserAccount"`
	Mint             string  `json:"mint"`
	ToTokenAccount   string  `json:"toTokenAccount"`
	ToUserAccount    string  `json:"toUserAccount"`
	TokenAmount      float64 `json:"tokenAmount"`
	TokenStandard    string  `json:"tokenStandard"`
}

type HeliusWebhookArgs struct{}

type HeliusWebhookResult struct {
	Message string `json:"message,omitempty"`
}

const solanaUsdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

var solanaReceiverAddresses = []string{
	"4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM", // coinbase account address
	"74UNdYRpvakSABaYHSZMQNaXBVtA6eY9Nt8chcqocKe7", // deprecating this
}

func HeliusWebhook(
	transactions []*SolanaTransaction,
	clientSession *session.ClientSession,
) (*HeliusWebhookResult, error) {

	if len(transactions) == 0 {
		return &HeliusWebhookResult{Message: "No transactions"}, nil
	}

	// One Helius delivery carries a BATCH of transactions, and a customer's payment can
	// arrive behind any number of unrelated transfers in the same batch. So a
	// transaction that does not match is SKIPPED (continue), never returned on: the
	// early returns this loop used to take meant a valid payment behind any unrelated
	// transfer was never examined at all -- Helius got its 200 and never retried, and
	// the money bought nothing.
	//
	// A per-transaction DB failure is remembered and returned AFTER the whole batch is
	// examined: the non-2xx makes Helius redeliver everything, and the consumed intents
	// (tx_signature set) make the already-credited ones no-ops on the retry.
	var matched int
	var firstErr error
	// the reason the transaction was skipped -- reported as the result message for a
	// single-transaction delivery, so a caller (and the existing tests) can see WHY
	skipMessage := ""
	for _, transaction := range transactions {

		if transaction.Type != "TRANSFER" {
			glog.Infof("HeliusWebhook: ignoring non-transfer transaction: %s of type %s", transaction.Signature, transaction.Type)
			skipMessage = fmt.Sprintf("Ignoring non-transfer transaction of type %s", transaction.Type)
			continue
		}

		if len(transaction.TokenTransfers) == 0 {
			glog.Infof("HeliusWebhook: no token transfers found for transaction: %s", transaction.Signature)
			skipMessage = "Ignoring transaction with no token transfers"
			continue
		}

		// Take the largest USDC transfer to one of our receiving addresses, WHATEVER its
		// size. It used to require `>= 40` here -- a hardcoded stand-in for the yearly
		// price -- which meant a customer who chose the $5 monthly plan on the site had
		// their payment ignored entirely as "no matching USDC payment". They paid and got
		// nothing.
		//
		// The amount is checked below, against what they were actually QUOTED.
		paymentReceived := false
		var tokenAmountReceived float64

		for _, tokenTransfer := range transaction.TokenTransfers {

			if tokenTransfer.Mint == solanaUsdcMint &&
				slices.Contains(solanaReceiverAddresses, tokenTransfer.ToUserAccount) &&
				0 < tokenTransfer.TokenAmount {
				paymentReceived = true
				if tokenAmountReceived < tokenTransfer.TokenAmount {
					tokenAmountReceived = tokenTransfer.TokenAmount
				}
			}

		}

		if !paymentReceived {
			glog.Infof("HeliusWebhook: no USDC payment found for transaction: %s", transaction.Signature)
			skipMessage = "Ignoring transaction with no matching USDC payment"
			continue
		}

		// array of accounts to use to search for payment intents
		accounts := make([]string, len(transaction.AccountData))
		for i, accountData := range transaction.AccountData {
			accounts[i] = accountData.Account
		}

		paymentSearchResult, err := model.SearchPaymentIntents(accounts, clientSession)

		if err != nil {
			glog.Infof("HeliusWebhook: error searching payment intents: %v", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		// on-chain time, when the webhook carried one, for the unfulfilled record
		var transactionTime *time.Time
		if transaction.Timestamp != 0 {
			t := time.Unix(transaction.Timestamp, 0).UTC()
			transactionTime = &t
		}

		if paymentSearchResult == nil {
			skipMessage = "No payment intent found for this network ID"

			// a REDELIVERY of a payment that already consumed its intent finds no
			// open intent either -- that one is credited and done, not unfulfilled
			if model.IsSolanaPaymentCompleted(clientSession.Ctx, transaction.Signature) {
				glog.Infof("HeliusWebhook: transaction %s already credited; ignoring redelivery", transaction.Signature)
				continue
			}

			// Money arrived at our address and no open intent matched -- a payment
			// after the intent was swept, or an unknown reference. Helius is still
			// acked 200 (it never re-examines a delivered tx), so record it where an
			// operator can see and repair it, with the account keys the reference was
			// searched among. A late payment whose intent has merely EXPIRED but not
			// yet been swept never lands here: the search ignores expires_at on
			// purpose, so it still resolves and is credited below -- late is not
			// fraudulent.
			glog.Errorf("HeliusWebhook: no payment intent found for transaction: %s; recording as unfulfilled\n", transaction.Signature)
			if err := model.RecordUnfulfilledSolanaPayment(clientSession.Ctx, &model.UnfulfilledSolanaPayment{
				TxSignature:         transaction.Signature,
				Reason:              model.SolanaUnfulfilledReasonNoIntent,
				TokenAmountUsd:      tokenAmountReceived,
				ReferenceCandidates: accounts,
				TransactionTime:     transactionTime,
			}); err != nil {
				glog.Errorf("HeliusWebhook: could not record unfulfilled payment %s: %v\n", transaction.Signature, err)
			}
			continue
		}

		// Verify the payment against what the customer was QUOTED. Underpaying must not
		// buy a plan; overpaying is their choice and is honored.
		//
		// The tolerance absorbs float dust in the token amount (it arrives as a float64
		// from the chain), not a real discount.
		if solanaAmountTolerance < paymentSearchResult.ExpectedAmountUsd-tokenAmountReceived {
			glog.Errorf(
				"HeliusWebhook: underpaid %s: received %.2f USDC, quoted %.2f (reference %s)\n",
				transaction.Signature,
				tokenAmountReceived,
				paymentSearchResult.ExpectedAmountUsd,
				paymentSearchResult.PaymentReference,
			)
			// funds were kept and the intent stays open -- record the shortfall where
			// an operator can see it, with the quote it was checked against
			if err := model.RecordUnfulfilledSolanaPayment(clientSession.Ctx, &model.UnfulfilledSolanaPayment{
				TxSignature:       transaction.Signature,
				Reason:            model.SolanaUnfulfilledReasonUnderpaid,
				TokenAmountUsd:    tokenAmountReceived,
				ExpectedAmountUsd: &paymentSearchResult.ExpectedAmountUsd,
				PaymentReference:  &paymentSearchResult.PaymentReference,
				NetworkId:         paymentSearchResult.NetworkId,
				TransactionTime:   transactionTime,
			}); err != nil {
				glog.Errorf("HeliusWebhook: could not record unfulfilled payment %s: %v\n", transaction.Signature, err)
			}
			skipMessage = "Payment is less than the quoted price"
			continue
		}

		credited, insertErr := solanaCreditPaymentIntent(
			clientSession,
			paymentSearchResult,
			transaction.Signature,
			tokenAmountReceived,
		)

		if insertErr != nil {
			glog.Infof("HeliusWebhook: error inserting payment data: %v", insertErr)
			if firstErr == nil {
				firstErr = insertErr
			}
			continue
		}

		if !credited {
			// lost the race to a concurrent delivery of the same transaction, which
			// already consumed the intent and credited
			skipMessage = "No payment intent found for this network ID"
			continue
		}

		matched++
	}

	// a per-transaction DB failure surfaces as a non-2xx AFTER the whole batch was
	// examined, so Helius redelivers everything; the consumed intents make the
	// already-credited transactions no-ops on the retry
	if firstErr != nil {
		return nil, firstErr
	}

	if matched == 0 {
		glog.Infof("HeliusWebhook: no matching payments found for %v", transactions)
		// a single-transaction delivery keeps its specific reason; a batch with
		// nothing matched can only be summarized
		if len(transactions) == 1 && skipMessage != "" {
			return &HeliusWebhookResult{Message: skipMessage}, nil
		}
		return &HeliusWebhookResult{Message: "No matching payments"}, nil
	}
	return &HeliusWebhookResult{Message: fmt.Sprintf("Processed %d matching payments", matched)}, nil
}

// solanaCreditPaymentIntent consumes an open intent and grants the plan the
// customer was quoted, in ONE tx. Shared by the Helius webhook and the payment
// reconciler (which sweeps recorded unfulfilled payments whose reference now
// resolves), so a reconcile credit racing a webhook redelivery of the same
// transaction produces exactly one credit.
//
// The intent is consumed FIRST, in the same tx as the credit. The guarded
// UPDATE (tx_signature IS NULL, rows-affected checked) is the concurrency
// gate: two callers with the same transaction set the same signature, so only
// rows-affected can tell them apart -- exactly one proceeds to credit, the
// other sees a consumed intent and adds nothing (credited = false, no error).
func solanaCreditPaymentIntent(
	clientSession *session.ClientSession,
	paymentSearchResult *model.PaymentIntentSearchResult,
	signature string,
	tokenAmountReceivedUsd float64,
) (credited bool, returnErr error) {
	// Grant the plan they actually bought. This used to be a YEAR every time,
	// whatever they had chosen and whatever they had paid.
	startTime := server.NowUtc()
	endTime := startTime.Add(solanaPlanDuration(paymentSearchResult.SubscriptionPlan) + SubscriptionGracePeriod)

	netRevenue := model.UsdToNanoCents(tokenAmountReceivedUsd)

	server.Tx(clientSession.Ctx, func(tx server.PgTx) {

		completed, err := model.MarkPaymentIntentCompletedInTx(
			tx,
			paymentSearchResult.PaymentReference,
			signature,
			clientSession,
		)
		if err != nil {
			glog.Infof("[sub]solana credit: error marking payment intent completed: %v", err)
			returnErr = err
			return
		}
		if !completed {
			glog.Infof("[sub]solana credit: payment intent %s already completed; not crediting again", paymentSearchResult.PaymentReference)
			return
		}

		subscriptionRenewal := model.SubscriptionRenewal{
			NetworkId:          *paymentSearchResult.NetworkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          startTime,
			EndTime:            endTime,
			NetRevenue:         netRevenue,
			SubscriptionMarket: model.SubscriptionMarketSolana,
			TransactionId:      paymentSearchResult.PaymentReference,
		}

		err = model.AddSubscriptionRenewalInTx(tx, clientSession.Ctx, &subscriptionRenewal)

		if err != nil {
			glog.Infof("[sub]solana credit: error adding subscription renewal: %v", err)
			returnErr = err
			return
		}

		// a supporter subscription -> carries the Pro entitlement
		transferBalance := &model.TransferBalance{
			NetworkId:             *paymentSearchResult.NetworkId,
			StartTime:             startTime,
			EndTime:               endTime,
			StartBalanceByteCount: RefreshSupporterTransferBalance,
			SubsidyNetRevenue:     netRevenue,
			BalanceByteCount:      RefreshSupporterTransferBalance,
			Pro:                   true,
		}
		model.AddTransferBalanceInTx(
			clientSession.Ctx,
			tx,
			transferBalance,
		)

		credited = true
	})

	if returnErr != nil {
		return false, returnErr
	}

	if credited {
		// the pro balance is committed -- refresh the entitlement so the upgrade
		// is visible immediately rather than after ProCacheTtl
		model.UpdateProNetwork(clientSession.Ctx, *paymentSearchResult.NetworkId)
	}

	return credited, nil
}

/**
 * Solana Payment intents
 * We create a reference for each payment intent and map it to the network ID
 */

// solanaAmountTolerance absorbs float dust in the chain-reported token amount. It is not
// a discount: anything more than a cent short of the quoted price is an underpayment.
const solanaAmountTolerance = 0.01

// solanaPlanDuration is how long the plan the customer bought lasts. An empty plan means
// an intent created before the plan was recorded, which was always treated as yearly --
// so that is what those legacy intents still get.
func solanaPlanDuration(subscriptionPlan string) time.Duration {
	switch subscriptionPlan {
	case model.SolanaPlanMonthly:
		return 30 * 24 * time.Hour
	default:
		return SubscriptionYearDuration
	}
}

type SolanaPaymentIntentArgs struct {
	Reference string `json:"reference"`
	// The plan the customer picked. The PRICE is never taken from the client -- the
	// server derives it from pro.yml. A client-supplied amount would let anyone quote
	// themselves a year for a cent.
	Plan string `json:"plan"`
}

type SolanaPaymentIntentResult struct {
	// the price the SERVER quoted -- the client must pay exactly this
	AmountUsd float64                   `json:"amount_usd,omitempty"`
	Error     *SolanaPaymentIntentError `json:"error,omitempty"`
}

type SolanaPaymentIntentError struct {
	Message string `json:"message"`
}

// solanaPlanPriceUsd is the quoted price for a plan, from pro.yml. Server-side, always.
// solanaPlanPriceUsd is the price we QUOTE for a plan, and the price the webhook then
// checks the payment against. ok = false means we will not sell the plan at all.
//
// A price of zero is never sellable. With no pro.yml (or a mis-specified price: 0) this
// would otherwise quote UR Pro at $0.00 -- and the webhook's check is
// `amount >= price - tolerance`, which at price 0 is `amount >= -0.01`: satisfied by
// ANY payment, including none. We would hand out a year of Pro for nothing. Refuse.
func solanaPlanPriceUsd(subscriptionPlan string) (float64, bool) {
	var priceUsd float64
	switch subscriptionPlan {
	case model.SolanaPlanMonthly:
		priceUsd = model.Pro().PriceMonthlyUsd()
	case model.SolanaPlanYearly:
		priceUsd = model.Pro().PriceYearlyUsd()
	default:
		return 0, false
	}
	if priceUsd <= 0 {
		glog.Errorf(
			"[sub]refusing to quote %s: no price is configured (is pro.yml present?)\n",
			subscriptionPlan,
		)
		return 0, false
	}
	return priceUsd, true
}

func CreateSolanaPaymentIntent(
	intent *SolanaPaymentIntentArgs,
	clientSession *session.ClientSession,
) (*SolanaPaymentIntentResult, error) {

	// The price comes from pro.yml, keyed by the plan. It is NEVER taken from the client.
	priceUsd, ok := solanaPlanPriceUsd(intent.Plan)
	if !ok || priceUsd <= 0 {
		return &SolanaPaymentIntentResult{
			Error: &SolanaPaymentIntentError{Message: "Unknown plan."},
		}, nil
	}

	// The error used to be discarded here, so a duplicate or failed intent looked exactly
	// like a successful one -- and the customer was sent off to pay against an intent
	// that did not exist.
	err := model.CreateSolanaPaymentIntent(intent.Reference, priceUsd, intent.Plan, clientSession)
	if err != nil {
		glog.Errorf("[sub]could not create solana payment intent: %s\n", err)
		return &SolanaPaymentIntentResult{
			Error: &SolanaPaymentIntentError{Message: "Could not start the payment. Please try again."},
		}, nil
	}

	// Hand the quoted price back so the payment url the client builds and the intent the
	// webhook checks against cannot disagree.
	return &SolanaPaymentIntentResult{AmountUsd: priceUsd}, nil
}
