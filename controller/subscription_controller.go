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
	"strconv"
	"strings"
	"sync"
	"time"

	stripewebhook "github.com/stripe/stripe-go/v76/webhook"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const InitialTransferBalance = 32 * model.Gib

// 30 days
const InitialTransferBalanceDuration = 30 * 24 * time.Hour

const RefreshTransferBalanceDuration = 30 * time.Hour
const RefreshTransferBalanceTimeout = 24 * time.Hour

const RefreshSupporterTransferBalance = 600 * model.Gib
const RefreshFreeTransferBalance = 60 * model.Gib

const SubscriptionGracePeriod = 24 * time.Hour

const SpecialCompany = "company"

type Skus struct {
	Skus map[string]*Sku `yaml:"skus"`
}

type Sku struct {
	// the fees on the payment amount
	FeeFraction                   float64 `yaml:"fee_fraction"`
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

var stripeWebhookSigningSecret = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["webhook"].(map[string]any)["signing_secret"].(string)
})

var stripeApiToken = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["api"].(map[string]any)["token"].(string)
})

var stripeSkus = sync.OnceValue(func() map[string]*Sku {
	var skus Skus
	server.Config.RequireSimpleResource("stripe.yml").UnmarshalYaml(&skus)
	return skus.Skus
})

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

var companySenderEmail = sync.OnceValue(func() string {
	c := server.Config.RequireSimpleResource("email.yml").Parse()
	return c["company_sender_email"].(string)
})

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
	BalanceByteCount          model.ByteCount          `json:"balance_byte_count"`
	CurrentSubscription       *Subscription            `json:"current_subscription,omitempty"`
	ActiveTransferBalances    []*model.TransferBalance `json:"active_transfer_balances,omitempty"`
	PendingPayoutUsdNanoCents model.NanoCents          `json:"pending_payout_usd_nano_cents"`
	WalletInfo                *CircleWalletInfo        `json:"wallet_info,omitempty"`
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
	for _, transferBalance := range transferBalances {
		netBalanceByteCount += transferBalance.BalanceByteCount
	}

	var currentSubscription *Subscription
	if model.HasSubscriptionRenewal(session.Ctx, session.ByJwt.NetworkId, model.SubscriptionTypeSupporter) {
		currentSubscription = &Subscription{
			Plan: model.SubscriptionTypeSupporter,
		}
	}

	// FIXME
	pendingPayout := model.ByteCount(0)

	// ignore any error with circle,
	// since the model won't allow the wallet to enter a corrupt state
	walletInfo, _ := findMostRecentCircleWallet(session)

	return &SubscriptionBalanceResult{
		BalanceByteCount:          netBalanceByteCount,
		CurrentSubscription:       currentSubscription,
		ActiveTransferBalances:    transferBalances,
		PendingPayoutUsdNanoCents: pendingPayout,
		WalletInfo:                walletInfo,
		UpdateTime:                server.NowUtc(),
	}, nil
}

type StripeWebhookArgs struct {
	Id   string           `json:"id"`
	Type string           `json:"type"`
	Data *StripeEventData `json:"data"`
}

type StripeEventData struct {
	Object *StripeEventDataObject `json:"object"`
}

type StripeEventDataObject struct {
	Id              string                                `json:"id"`
	AmountTotal     int                                   `json:"amount_total"`
	CustomerDetails *StripeEventDataObjectCustomerDetails `json:"customer_details"`
	PaymentStatus   string                                `json:"payment_status"`
}

type StripeEventDataObjectCustomerDetails struct {
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
}

type StripeWebhookResult struct {
}

type StripeLineItems struct {
	Data []*StripeLineItem `json:"data"`
}

type StripeLineItem struct {
	Id          string                 `json:"id"`
	AmountTotal int                    `json:"amount_total"`
	Currency    string                 `json:"currency"`
	Description string                 `json:"description"`
	Price       *StripeLineItemProduct `json:"price"`
	Quantity    int                    `json:"quantity"`
}

type StripeLineItemProduct struct {
	Id         string `json:"id"`
	Product    string `json:"product"`
	UnitAmount int    `json:"unit_amount"`
}

func StripeWebhook(
	stripeWebhook *StripeWebhookArgs,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {
	if stripeWebhook.Type == "checkout.session.completed" {
		stripeSessionId := stripeWebhook.Data.Object.Id

		// need to make a second call to get the line items for the order
		// https://stripe.com/docs/api/checkout/sessions/line_items
		url := fmt.Sprintf(
			"https://api.stripe.com/v1/checkout/sessions/%s/line_items",
			stripeSessionId,
		)
		lineItems, err := server.HttpGetRequireStatusOk[*StripeLineItems](
			url,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiToken()))
			},
			server.ResponseJsonObject[*StripeLineItems],
		)
		if err != nil {
			return nil, err
		}

		purchaseEmail := stripeWebhook.Data.Object.CustomerDetails.Email
		if purchaseEmail == "" {
			return nil, errors.New("Missing purchase email to send balance code.")
		}

		skus := stripeSkus()
		for _, lineItem := range lineItems.Data {
			stripeSku := lineItem.Price.Product
			if sku, ok := skus[stripeSku]; ok {
				stripeItemJsonBytes, err := json.Marshal(lineItem)
				if err != nil {
					return nil, err
				}

				netRevenue := model.UsdToNanoCents((1.0 - sku.FeeFraction) * float64(lineItem.AmountTotal) / 100.0)

				glog.Infof("[sub]create balance code: %s %s\n", purchaseEmail, string(stripeItemJsonBytes))

				if sku.Special == "" {
					err = CreateBalanceCode(
						clientSession.Ctx,
						sku.BalanceByteCount(),
						netRevenue,
						stripeSessionId,
						string(stripeItemJsonBytes),
						purchaseEmail,
					)
					if err != nil {
						return nil, err
					}
				} else if sku.Special == SpecialCompany {
					awsMessageSender := GetAWSMessageSender()
					// company shared data
					err := awsMessageSender.SendAccountMessageTemplate(
						purchaseEmail,
						&SubscriptionTransferBalanceCompanyTemplate{
							BalanceByteCount: sku.BalanceByteCount(),
						},
						SenderEmail(companySenderEmail()),
					)
					if err != nil {
						return nil, err
					}
				} else {
					return nil, fmt.Errorf("Stripe unknown special (%s) for sku: %s", sku.Special, stripeSku)
				}
			} else {
				return nil, fmt.Errorf("Stripe sku not found: %s", stripeSku)
			}
		}
	}
	// else ignore the event

	return &StripeWebhookResult{}, nil
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
				netRevenue,
				coinbaseWebhook.Event.Data.Id,
				string(coinbaseDataJsonBytes),
				purchaseEmail,
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

func CreateBalanceCode(
	ctx context.Context,
	balanceByteCount model.ByteCount,
	netRevenue model.NanoCents,
	purchaseEventId string,
	purchaseRecord string,
	purchaseEmail string,
) error {
	if balanceCodeId, err := model.GetBalanceCodeIdForPurchaseEventId(ctx, purchaseEventId); err == nil {
		// the code was already created for the purchase event
		// send a reminder email

		balanceCode, err := model.GetBalanceCode(ctx, balanceCodeId)
		if err != nil {
			return err
		}

		awsMessageSender := GetAWSMessageSender()

		return awsMessageSender.SendAccountMessageTemplate(
			balanceCode.PurchaseEmail,
			&SubscriptionTransferBalanceCodeTemplate{
				Secret:           balanceCode.Secret,
				BalanceByteCount: balanceCode.BalanceByteCount,
			},
		)
	} else {
		// new code

		balanceCode, err := model.CreateBalanceCode(
			ctx,
			balanceByteCount,
			netRevenue,
			purchaseEventId,
			purchaseRecord,
			purchaseEmail,
		)
		if err != nil {
			return err
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
}

// https://developers.google.com/android-publisher/authorization
func playAuth() (string, error) {
	form := url.Values{}
	form.Add("grant_type", "refresh_token")
	form.Add("client_id", playClientId())
	form.Add("client_secret", playClientSecret())
	form.Add("refresh_token", playRefreshToken())

	result, err := server.HttpPostForm(
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

func playAuthHeaders(header http.Header) {
	if auth, err := playAuth(); err == nil {
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

// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptions
type PlaySubscription struct {
	StartTimeMillis             string `json:"startTimeMillis"`
	ExpiryTimeMillis            string `json:"expiryTimeMillis"`
	AutoRenewing                bool   `json:"autoRenewing"`
	PriceCurrencyCode           string `json:"priceCurrencyCode"`
	PriceAmountMicros           string `json:"priceAmountMicros"`
	CountryCode                 string `json:"countryCode"`
	DeveloperPayload            string `json:"developerPayload"`
	PaymentState                int    `json:"paymentState"`
	OrderId                     string `json:"orderId"`
	AcknowledgementState        int    `json:"acknowledgementState"`
	Kind                        string `json:"kind"`
	ObfuscatedExternalAccountId string `json:"obfuscatedExternalAccountId"`
}

func (self *PlaySubscription) requireStartTimeMillis() int64 {
	i, err := strconv.ParseInt(self.StartTimeMillis, 10, 64)
	if err != nil {
		panic(err)
	}
	return i
}

func (self *PlaySubscription) requireExpiryTimeMillis() int64 {
	i, err := strconv.ParseInt(self.ExpiryTimeMillis, 10, 64)
	if err != nil {
		panic(err)
	}
	return i
}

type PlayWebhookArgs struct {
	Message *PlayWebhookMessage `json:"message"`
}

type PlayWebhookMessage struct {
	Data string `json:"data"`
}

type PlayWebhookResult struct {
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
		if rtdnMessage.SubscriptionNotification != nil {
			url := fmt.Sprintf(
				"https://androidpublisher.googleapis.com/androidpublisher/v3/applications/%s/purchases/subscriptions/%s/tokens/%s",
				rtdnMessage.PackageName,
				rtdnMessage.SubscriptionNotification.SubscriptionId,
				rtdnMessage.SubscriptionNotification.PurchaseToken,
			)
			sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
				url,
				playAuthHeaders,
				server.ResponseJsonObject[*PlaySubscription],
			)
			if err != nil {
				return nil, err
			}

			glog.Infof("[sub]google play sub: %v\n", sub)

			subscriptionPaymentId, err := server.ParseId(sub.ObfuscatedExternalAccountId)
			if err != nil {
				return nil, fmt.Errorf("Google Play subscription malformed obfuscated external account id: \"%s\" = %s", sub.ObfuscatedExternalAccountId, err)
			}

			networkId, err := model.SubscriptionGetNetworkIdForPaymentId(clientSession.Ctx, subscriptionPaymentId)
			if err != nil {
				return nil, err
			}

			if sub.PaymentState == 1 && sub.AcknowledgementState == 0 {
				// Aknowledge
				url := fmt.Sprintf(
					"https://androidpublisher.googleapis.com/androidpublisher/v3/applications/%s/purchases/subscriptions/%s/tokens/%s:acknowledge",
					rtdnMessage.PackageName,
					rtdnMessage.SubscriptionNotification.SubscriptionId,
					rtdnMessage.SubscriptionNotification.PurchaseToken,
				)
				server.HttpPostRawRequireStatusOk(
					url,
					[]byte{},
					playAuthHeaders,
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
				task.ScheduleTask(
					PlaySubscriptionRenewal,
					&PlaySubscriptionRenewalArgs{
						NetworkId:      networkId,
						PackageName:    rtdnMessage.PackageName,
						SubscriptionId: rtdnMessage.SubscriptionNotification.SubscriptionId,
						PurchaseToken:  rtdnMessage.SubscriptionNotification.PurchaseToken,
						CheckTime:      time.UnixMilli(sub.requireExpiryTimeMillis()),
					},
					clientSession,
				)
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
		"https://androidpublisher.googleapis.com/androidpublisher/v3/applications/%s/purchases/subscriptions/%s/tokens/%s",
		playSubscriptionRenewal.PackageName,
		playSubscriptionRenewal.SubscriptionId,
		playSubscriptionRenewal.PurchaseToken,
	)
	sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
		url,
		playAuthHeaders,
		server.ResponseJsonObject[*PlaySubscription],
	)
	if err != nil {
		return nil, err
	}

	expiryTime := time.UnixMilli(sub.requireExpiryTimeMillis())
	startTime := time.UnixMilli(sub.requireStartTimeMillis())

	priceAmountMicros, err := strconv.ParseFloat(sub.PriceAmountMicros, 64)
	if err != nil {
		return nil, err
	}

	if _, err := model.GetOverlappingTransferBalance(clientSession.Ctx, playSubscriptionRenewal.PurchaseToken, expiryTime); err != nil {
		skus := playSkus()
		skuName := playSubscriptionRenewal.SubscriptionId
		if sku, ok := skus[skuName]; ok {
			if sku.Supporter {
				renewal := &model.SubscriptionRenewal{
					NetworkId:          playSubscriptionRenewal.NetworkId,
					StartTime:          startTime,
					EndTime:            expiryTime.Add(SubscriptionGracePeriod),
					NetRevenue:         model.UsdToNanoCents((1.0 - sku.FeeFraction) * priceAmountMicros / float64(1000*1000)),
					PurchaseToken:      playSubscriptionRenewal.PurchaseToken,
					SubscriptionType:   model.SubscriptionTypeSupporter,
					SubscriptionMarket: model.SubscriptionMarketGoogle,
				}
				model.AddSubscriptionRenewal(
					clientSession.Ctx,
					renewal,
				)
				AddRefreshTransferBalance(clientSession.Ctx, playSubscriptionRenewal.NetworkId)

			} else {
				transferBalance := &model.TransferBalance{
					NetworkId:             playSubscriptionRenewal.NetworkId,
					StartTime:             startTime,
					EndTime:               expiryTime.Add(SubscriptionGracePeriod),
					StartBalanceByteCount: sku.BalanceByteCount(),
					NetRevenue:            model.UsdToNanoCents((1.0 - sku.FeeFraction) * priceAmountMicros / float64(1000*1000)),
					BalanceByteCount:      sku.BalanceByteCount(),
					PurchaseToken:         playSubscriptionRenewal.PurchaseToken,
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
			ExpiryTime: expiryTime,
			Renewed:    true,
		}, nil
	} else {
		// a transfer balance was already for the current expiry time
		// hence, the subscription has not been extended/renewed
		return &PlaySubscriptionRenewalResult{
			ExpiryTime: expiryTime,
			Renewed:    false,
		}, nil
	}
}

func PlaySubscriptionRenewalPost(
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	playSubscriptionRenewalResult *PlaySubscriptionRenewalResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if playSubscriptionRenewalResult.Renewed {
		playSubscriptionRenewal.CheckTime = playSubscriptionRenewalResult.ExpiryTime
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

func VerifyStripeBody(req *http.Request) (io.Reader, error) {
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	_, err = stripewebhook.ConstructEventWithOptions(
		bodyBytes,
		req.Header.Get("Stripe-Signature"),
		stripeWebhookSigningSecret(),
		stripewebhook.ConstructEventOptions{IgnoreAPIVersionMismatch: true},
	)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(bodyBytes), nil
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
	err = verifyPlayAuth(authHeader)
	if err != nil {
		glog.Infof("verifyPlayAuth failed: %v", err)
		return nil, err
	}

	return bytes.NewReader(bodyBytes), nil
}

func verifyPlayAuth(auth string) error {
	bearerPrefix := "Bearer "

	if strings.HasPrefix(auth, bearerPrefix) {
		jwt := auth[len(bearerPrefix):len(auth)]
		url := fmt.Sprintf("https://oauth2.googleapis.com/tokeninfo?id_token=%s", jwt)

		claimBytes, err := server.HttpGetRawRequireStatusOk(url, server.NoCustomHeaders)
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

func AddRefreshTransferBalance(ctx context.Context, networkId server.Id) {
	startTime := server.NowUtc()
	endTime := startTime.Add(RefreshTransferBalanceDuration)
	var transferBalance model.ByteCount
	if model.HasSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeSupporter) {
		transferBalance = RefreshSupporterTransferBalance
	} else {
		transferBalance = RefreshFreeTransferBalance
	}
	model.AddBasicTransferBalance(
		ctx,
		networkId,
		transferBalance,
		startTime,
		endTime,
	)
}

// Refresh transfer balances

type RefreshTransferBalancesArgs struct {
}

type RefreshTransferBalancesResult struct {
}

func ScheduleRefreshTransferBalances(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RefreshTransferBalances,
		&RefreshTransferBalancesArgs{},
		clientSession,
		task.RunOnce("refresh_transfer_balances"),
		task.RunAt(server.NowUtc().Add(RefreshTransferBalanceTimeout)),
	)
}

func RefreshTransferBalances(
	refreshTransferBalances *RefreshTransferBalancesArgs,
	clientSession *session.ClientSession,
) (*RefreshTransferBalancesResult, error) {
	startTime := server.NowUtc()
	endTime := startTime.Add(RefreshTransferBalanceDuration)
	model.AddRefreshTransferBalanceToAllNetworks(
		clientSession.Ctx,
		startTime,
		endTime,
		map[bool]model.ByteCount{
			false: RefreshFreeTransferBalance,
			true:  RefreshSupporterTransferBalance,
		},
	)
	return &RefreshTransferBalancesResult{}, nil
}

func RefreshTransferBalancesPost(
	refreshTransferBalances *RefreshTransferBalancesArgs,
	refreshTransferBalancesResult *RefreshTransferBalancesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRefreshTransferBalances(clientSession, tx)
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
	glog.Infof("[apple] New subscription: %+v", notification.Data)

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

		AddRefreshTransferBalance(ctx, networkId)

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
