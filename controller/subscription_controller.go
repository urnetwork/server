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
	OpenTransferByteCount     model.ByteCount          `json:"open_transfer_byte_count"`
	CurrentSubscription       *Subscription            `json:"current_subscription,omitempty"`
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

	// FIXME
	pendingPayout := model.ByteCount(0)

	return &SubscriptionBalanceResult{
		BalanceByteCount:          netBalanceByteCount,
		StartBalanceByteCount:     startBalanceByteCount,
		OpenTransferByteCount:     openTransferByteCount,
		CurrentSubscription:       currentSubscription,
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
	if coinbaseWebhook.Event.Type == "charge:confirmed" {
		skuName := coinbaseWebhook.Event.Data.Name
		skus := coinbaseSkus()
		if sku, ok := skus[skuName]; ok {
			purchaseEmail := coinbaseWebhook.Event.Data.Metadata.Email
			if purchaseEmail == "" {
				return nil, errors.New("Missing purchase email to send balance code.")
			}

			coinbaseDataJsonBytes, err := json.Marshal(coinbaseWebhook.Event.Data)
			if err != nil {
				return nil, err
			}

			paymentUsd, err := strconv.ParseFloat(coinbaseWebhook.Event.Data.Payments[0].Net.Local.Amount, 64)
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

	if rtdnMessage.PackageName == playPackageName() {
		// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2/get
		// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2#SubscriptionPurchaseV2
		// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptions/acknowledge
		if rtdnMessage.SubscriptionNotification != nil {
			url := fmt.Sprintf(
				"https://androidpublisher.googleapis.com/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
				rtdnMessage.PackageName,
				rtdnMessage.SubscriptionNotification.PurchaseToken,
			)
			sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
				clientSession.Ctx,
				url,
				func(header http.Header) {
					playAuthHeader(clientSession.Ctx, header)
				},
				server.ResponseJsonObject[*PlaySubscription],
			)
			if err != nil {
				if v, ok := err.(*server.HttpStatusError); ok {
					switch v.StatusCode {
					// Gone
					case 410:
						return &PlayWebhookResult{}, nil
					default:
						return nil, err
					}
				} else {
					return nil, err
				}
			}

			glog.Infof("[sub]google play sub: %v\n", sub)

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
				// Aknowledge
				url := fmt.Sprintf(
					"https://androidpublisher.googleapis.com/androidpublisher/v3/applications/%s/purchases/subscriptions/%s/tokens/%s:acknowledge",
					rtdnMessage.PackageName,
					rtdnMessage.SubscriptionNotification.SubscriptionId,
					rtdnMessage.SubscriptionNotification.PurchaseToken,
				)
				server.HttpPostRawRequireStatusOk(
					clientSession.Ctx,
					url,
					[]byte{},
					func(header http.Header) {
						playAuthHeader(clientSession.Ctx, header)
					},
				)

				// fire this immediately since we pull current plan from subscription_renewal table
				PlaySubscriptionRenewal(
					&PlaySubscriptionRenewalArgs{
						NetworkId:      networkId,
						PackageName:    rtdnMessage.PackageName,
						SubscriptionId: rtdnMessage.SubscriptionNotification.SubscriptionId,
						PurchaseToken:  rtdnMessage.SubscriptionNotification.PurchaseToken,
					},
					clientSession,
				)

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
		"https://androidpublisher.googleapis.com/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
		playSubscriptionRenewal.PackageName,
		playSubscriptionRenewal.PurchaseToken,
	)
	sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
		clientSession.Ctx,
		url,
		func(header http.Header) {
			playAuthHeader(clientSession.Ctx, header)
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
			skus := playSkus()
			skuName := playSubscriptionRenewal.SubscriptionId
			if sku, ok := skus[skuName]; ok {
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
					model.AddSubscriptionRenewal(
						clientSession.Ctx,
						renewal,
					)

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
					model.AddTransferBalance(
						clientSession.Ctx,
						transferBalance,
					)
					model.UpdateProNetwork(clientSession.Ctx, playSubscriptionRenewal.NetworkId)

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
					model.AddTransferBalance(
						clientSession.Ctx,
						transferBalance,
					)
				}
			} else {
				return nil, fmt.Errorf("Play sku not found: %s", skuName)
			}

			return &PlaySubscriptionRenewalResult{
				ExpiryTime: minExpiryTime,
				Renewed:    true,
			}, nil
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

// RefreshReferralTransferBalances grants every referrer its referral bonus for one
// period: bonus_per_referral x min(referrals, max_referrals), all from pro.yml.
// Referrals pay out every period for life. The balance is unpaid and pro = false, so
// referral data never confers Pro.
func RefreshReferralTransferBalances(
	refreshReferralTransferBalances *RefreshReferralTransferBalancesArgs,
	clientSession *session.ClientSession,
) (*RefreshReferralTransferBalancesResult, error) {
	// Nothing to grant -> grant nothing, rather than write zero-byte balance rows. With no
	// pro.yml this amount is zero. The task stays SCHEDULED, so once pro.yml lands (and
	// the process restarts) the grants resume on their normal cadence by themselves.
	if model.Pro().ReferralBonus <= 0 {
		glog.Errorf("[sub]RefreshReferralTransferBalances: no amount configured (is pro.yml present?); skipping the grant\n")
		return &RefreshReferralTransferBalancesResult{}, nil
	}

	startTime, endTime := ReferralGrantWindow(server.NowUtc())
	model.AddReferralBonusesToAllNetworks(
		clientSession.Ctx,
		startTime,
		endTime,
		model.Pro().ReferralBonus,
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
}

func HandleSubscribedApple(ctx context.Context, notification AppleNotificationDecodedPayload) {

	renewalInfo, transactionInfo, err := ParseSignedInfo(notification)

	if err != nil {
		glog.Errorf("[apple] Failed to parse signed info: %v", err)
		return
	}

	if renewalInfo != nil {
		glog.Infof("[apple] Renewal Info: %+v", renewalInfo)
	} else {
		glog.Infof("[apple] Renewal Info is nil")
	}

	if transactionInfo != nil {
		glog.Infof("[apple] Transaction Info: %+v", transactionInfo)

		// Extract key subscription details for database update
		// var originalTransactionId string // for the original subscription transaction
		var appTransactionId string // for the current subscription transaction
		var productId string
		var expiresDate time.Time
		var networkId server.Id

		// parse the network id
		if networkIdStr, ok := transactionInfo["appAccountToken"].(string); ok {
			networkId, err = server.ParseId(networkIdStr)
			if err != nil {
				glog.Errorf("[apple] Failed to parse network ID: %v", err)
				return
			}
		}

		if atid, ok := transactionInfo["appTransactionId"].(string); ok {
			appTransactionId = atid
		}

		if exp, ok := transactionInfo["expiresDate"].(float64); ok {
			expiresDate = time.Unix(int64(exp/1000), 0)
		}

		var priceNanoCents int64

		if priceFloat, ok := transactionInfo["price"].(float64); ok {

			// webhook price coming back like "4990" for $4.99
			priceUsd := priceFloat / 1000

			priceNanoCents = model.UsdToNanoCents(priceUsd)

		}

		// fixme: hardcoded fee fraction
		feeFraction := 0.2

		netRevenue := priceNanoCents - int64(float64(priceNanoCents)*feeFraction)

		if netRevenue == 0 {
			/**
			 * For users who subscribe via TestFlight or with FREE_TRIAL, the price will be 0
			 * We still want to mark them upgraded in the DB, which means they need some paid transfer_balance
			 */
			netRevenue = model.UsdToNanoCents(0.01)
		}

		glog.Infof("[apple] Subscription details - App Transaction ID: %s, Network ID: %s, Product ID: %s, Expires: %s, Net Revenue: %d",
			appTransactionId,
			networkId,
			productId,
			expiresDate.Format(time.RFC3339),
			netRevenue,
		)

		startTime := time.Now()
		endTime := expiresDate.Add(SubscriptionGracePeriod)

		subscriptionRenewal := model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          startTime,
			EndTime:            endTime,
			NetRevenue:         netRevenue,
			SubscriptionMarket: model.SubscriptionMarketApple,
			TransactionId:      appTransactionId,
		}

		model.AddSubscriptionRenewal(ctx, &subscriptionRenewal)

		// a supporter subscription -> carries the Pro entitlement
		transferBalance := &model.TransferBalance{
			NetworkId:             networkId,
			StartTime:             startTime,
			EndTime:               endTime,
			StartBalanceByteCount: RefreshSupporterTransferBalance,
			SubsidyNetRevenue:     netRevenue,
			BalanceByteCount:      RefreshSupporterTransferBalance,
			Pro:                   true,
		}
		model.AddTransferBalance(
			ctx,
			transferBalance,
		)
		model.UpdateProNetwork(ctx, networkId)

	} else {
		glog.Infof("[apple] Transaction Info: nil")
	}
}

func HandleExpiredApple(notification AppleNotificationDecodedPayload) {
	glog.Infof("[apple] Subscription expired: %+v", notification.Data)

	_, transactionInfo, err := ParseSignedInfo(notification)
	if err != nil {
		glog.Errorf("[apple] Failed to parse signed info: %v", err)
		return
	}

	if transactionInfo != nil {
		if originalTransactionId, ok := transactionInfo["originalTransactionId"].(string); ok {
			glog.Infof("[apple] Marking subscription expired for transaction: %s", originalTransactionId)
			// fixme: mark subscription expired in db
		}
	}

	// could send a follow up email to the user?

}

func HandleRenewalApple(ctx context.Context, notification AppleNotificationDecodedPayload) {
	glog.Infof("[apple] Subscription renewed: %+v", notification.Data)

	var networkId server.Id
	var appTransactionId string
	var expiresDate time.Time

	renewalInfo, transactionInfo, err := ParseSignedInfo(notification)
	if err != nil {
		glog.Errorf("[apple] Failed to parse signed info: %v", err)
		return
	}

	if renewalInfo != nil {
		glog.Infof("[apple] Renewal Info: %+v", renewalInfo)

		if atid, ok := transactionInfo["appTransactionId"].(string); ok {
			appTransactionId = atid
		}

		// for checking auto renewal status
		// if autoRenewStatus, ok := renewalInfo["autoRenewStatus"].(float64); ok {
		// 	if autoRenewStatus == 1 {
		// 		glog.Infof("[apple] Auto-renewal is enabled")
		// 	} else {
		// 		glog.Infof("[apple] Auto-renewal is disabled")
		// 	}
		// }

		if networkIdStr, ok := renewalInfo["appAccountToken"].(string); ok {
			networkId, err = server.ParseId(networkIdStr)
			if err != nil {
				glog.Errorf("[apple] Failed to parse network ID: %v", err)
				return
			}
		}

		if renewalDate, ok := renewalInfo["renewalDate"].(float64); ok {
			glog.Infof("[apple] Expiration intent: %v", renewalDate)
			expiresDate = time.Unix(int64(renewalDate/1000), 0)
		}

		var priceNanoCents int64

		if priceFloat, ok := renewalInfo["renewalPrice"].(float64); ok {

			// webhook price coming back like "4990" for $4.99
			priceUsd := priceFloat / 1000

			priceNanoCents = model.UsdToNanoCents(priceUsd)

		}

		// fixme: hardcoded fee fraction
		feeFraction := 0.2

		netRevenue := priceNanoCents - int64(float64(priceNanoCents)*feeFraction)

		glog.Infof("[apple] Subscription details - App Transaction ID: %s, Network ID: %s, Expires: %s, Net Revenue: %d",
			appTransactionId,
			networkId,
			expiresDate.Format(time.RFC3339),
			netRevenue,
		)

		subscriptionRenewal := model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          time.Now(),
			EndTime:            expiresDate.Add(SubscriptionGracePeriod),
			NetRevenue:         netRevenue,
			SubscriptionMarket: model.SubscriptionMarketApple,
			TransactionId:      appTransactionId,
		}

		model.AddSubscriptionRenewal(ctx, &subscriptionRenewal)

		AddRefreshTransferBalance(ctx, networkId)

	} else {
		glog.Infof("[apple] Renewal Info is nil")
	}

}

func DecodeJWSPayload(jwsToken string) (map[string]interface{}, error) {
	// split JWS
	parts := strings.Split(jwsToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWS format")
	}

	// decode
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to decode JWS payload: %v", err)
	}

	// parse
	var payload map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse JWS payload: %v", err)
	}

	return payload, nil
}

func ParseSignedInfo(notification AppleNotificationDecodedPayload) (map[string]interface{}, map[string]interface{}, error) {
	var renewalInfo map[string]interface{}
	var transactionInfo map[string]interface{}
	var err error

	var signedRenewalInfo string
	if notification.SignedRenewalInfo != "" {
		signedRenewalInfo = notification.SignedRenewalInfo
		glog.Infoln("Using top-level SignedRenewalInfo")
	} else if dataField, ok := notification.Data["signedRenewalInfo"].(string); ok && dataField != "" {
		signedRenewalInfo = dataField
		glog.Infoln("Using data.signedRenewalInfo")
	}

	if signedRenewalInfo != "" {
		renewalInfo, err = DecodeJWSPayload(signedRenewalInfo)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to decode signedRenewalInfo: %v", err)
		}
	}

	var signedTransactionInfo string
	if notification.SignedTransactionInfo != "" {
		signedTransactionInfo = notification.SignedTransactionInfo
		glog.Infoln("Using top-level SignedTransactionInfo")
	} else if dataField, ok := notification.Data["signedTransactionInfo"].(string); ok && dataField != "" {
		signedTransactionInfo = dataField
		glog.Infoln("Using data.signedTransactionInfo")
	}

	if signedTransactionInfo != "" {
		transactionInfo, err = DecodeJWSPayload(signedTransactionInfo)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to decode signedTransactionInfo: %v", err)
		}
	}

	return renewalInfo, transactionInfo, nil
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

	var matched int
	for _, transaction := range transactions {

		if transaction.Type != "TRANSFER" {
			glog.Infof("HeliusWebhook: ignoring non-transfer transaction: %s of type %s", transaction.Signature, transaction.Type)
			return &HeliusWebhookResult{
				Message: fmt.Sprintf("Ignoring non-transfer transaction of type %s", transaction.Type),
			}, nil
		}

		if len(transaction.TokenTransfers) == 0 {
			glog.Infof("HeliusWebhook: no token transfers found for transaction: %s", transaction.Signature)
			return &HeliusWebhookResult{
				Message: "Ignoring transaction with no token transfers",
			}, nil
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
			return &HeliusWebhookResult{
				Message: "Ignoring transaction with no matching USDC payment",
			}, nil
		}

		// array of accounts to use to search for payment intents
		accounts := make([]string, len(transaction.AccountData))
		for i, accountData := range transaction.AccountData {
			accounts[i] = accountData.Account
		}

		paymentSearchResult, err := model.SearchPaymentIntents(accounts, clientSession)

		if err != nil {
			glog.Infof("HeliusWebhook: error searching payment intents: %v", err)
			return nil, err
		}

		if paymentSearchResult == nil {
			glog.Infof("HeliusWebhook: no payment intent found for transaction: %s", transaction.Signature)
			return &HeliusWebhookResult{
				Message: "No payment intent found for this network ID",
			}, nil
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
			return &HeliusWebhookResult{
				Message: "Payment is less than the quoted price",
			}, nil
		}

		// Grant the plan they actually bought. This used to be a YEAR every time, whatever
		// they had chosen and whatever they had paid.
		startTime := server.NowUtc()
		endTime := startTime.Add(solanaPlanDuration(paymentSearchResult.SubscriptionPlan) + SubscriptionGracePeriod)

		netRevenue := model.UsdToNanoCents(tokenAmountReceived)

		var insertErr error

		server.Tx(clientSession.Ctx, func(tx server.PgTx) {

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
				glog.Infof("HeliusWebhook: error adding subscription renewal: %v", err)
				insertErr = err
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

			err = model.MarkPaymentIntentCompletedInTx(
				tx,
				paymentSearchResult.PaymentReference,
				transaction.Signature,
				clientSession,
			)

			if err != nil {
				glog.Infof("HeliusWebhook: error marking payment intent completed: %v", err)
				insertErr = err
			}
		})

		if insertErr != nil {
			glog.Infof("HeliusWebhook: error inserting payment data: %v", insertErr)
			return nil, insertErr
		}

		// the pro balance is committed -- refresh the entitlement so the upgrade is
		// visible immediately rather than after ProCacheTtl
		model.UpdateProNetwork(clientSession.Ctx, *paymentSearchResult.NetworkId)

		matched++
	}

	if matched == 0 {
		glog.Infof("HeliusWebhook: no matching payments found for %v", transactions)
		return &HeliusWebhookResult{Message: "No matching payments"}, nil
	}
	return &HeliusWebhookResult{Message: fmt.Sprintf("Processed %d matching payments", matched)}, nil
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
