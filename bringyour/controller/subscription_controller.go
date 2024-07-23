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

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/task"
)



const InitialTransferBalance = 32 * model.Gib

// 30 days
const InitialTransferBalanceDuration = 30 * 24 * time.Hour


const SubscriptionGracePeriod = 24 * time.Hour


type Sku struct {
	// the fees on the payment amount
	FeeFraction float64
	BalanceByteCount model.ByteCount
	Special string
}


const SpecialCompany = "company"


// FIXME read from yml


var stripeWebhookSigningSecret = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["webhook"].(map[string]any)["signing_secret"].(string)
})

var stripeApiToken = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["api"].(map[string]any)["token"].(string)
})

func stripeSkus() map[string]*Sku {
	playSubscriptionFeeFraction := 0.3
	// FIXME read from json
	return map[string]*Sku{
		// 300GiB
		"prod_OlUgT5brBfOBiT": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(300 * 1024 * 1024 * 1024),
		},
		// 1TiB
		"prod_Om2V4ElmxY5Civ": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(1024 * 1024 * 1024 * 1024),
		},
		// 2Tib
		"prod_Om2XiaUQlgzawz": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(2 * 1024 * 1024 * 1024 * 1024),
		},
		// 10TiB company
		"prod_PYvFxhlBrr1FAN": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(10 * 1024 * 1024 * 1024 * 1024),
			Special: SpecialCompany,
		},
		// 100TiB company
		"prod_PYvNGYsoREsTVZ": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(100 * 1024 * 1024 * 1024 * 1024),
			Special: SpecialCompany,
		},
	}
}

var coinbaseWebhookSharedSecret = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("coinbase.yml").Parse()
	return c["webhook"].(map[string]any)["shared_secret"].(string)
})

func coinbaseSkus() map[string]*Sku {
	playSubscriptionFeeFraction := 0.3
	// FIXME read from json
	return map[string]*Sku{
		"300GiB": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(300 * 1024 * 1024 * 1024),
		},
		"1TiB": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(1024 * 1024 * 1024 * 1024),
		},
		"2TiB": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(2 * 1024 * 1024 * 1024 * 1024),
		},
	}
}

var playPublisherEmail = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("google.yml").Parse()
	return c["webhook"].(map[string]any)["publisher_email"].(string)
})

var playPackageName = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("google.yml").Parse()
	return c["webhook"].(map[string]any)["package_name"].(string)
})

// FIXME eval once
func playSkus() map[string]*Sku {
	playSubscriptionFeeFraction := 0.3
	// FIXME read from json
	return map[string]*Sku{
		"monthly_transfer_300gib": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(300) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024),
		},
		"monthly_transfer_1tib": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024),
		},
		"ultimate": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(10) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024),
		},
	}
}

func companySenderEmail() string {
	return "brien@bringyour.com"
}

var playClientId = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("google.yml").Parse()
	return c["oauth"].(map[string]any)["client_id"].(string)
})

var playClientSecret = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("google.yml").Parse()
	return c["oauth"].(map[string]any)["client_secret"].(string)
})

var playRefreshToken = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("google.yml").Parse()
	return c["oauth"].(map[string]any)["refresh_token"].(string)
})


// app initially calls "get info"
// then if no wallet, show a button to initialize wallet
// if wallet, show a button to refresh, and to withdraw


type SubscriptionBalanceResult struct {
	BalanceByteCount model.ByteCount `json:"balance_byte_count"`
	CurrentSubscription *model.Subscription `json:"current_subscription,omitempty"`
	ActiveTransferBalances []*model.TransferBalance `json:"active_transfer_balances,omitempty"`
	PendingPayoutUsdNanoCents model.NanoCents `json:"pending_payout_usd_nano_cents"`
	WalletInfo *CircleWalletInfo `json:"wallet_info,omitempty"`
	UpdateTime time.Time `json:"update_time"`
}


func SubscriptionBalance(session *session.ClientSession) (*SubscriptionBalanceResult, error) {
	transferBalances := model.GetActiveTransferBalances(session.Ctx, session.ByJwt.NetworkId)

	netBalanceByteCount := model.ByteCount(0)
	for _, transferBalance := range transferBalances {
		netBalanceByteCount += transferBalance.BalanceByteCount
	}

	currentSubscription := model.CurrentSubscription(session.Ctx, session.ByJwt.NetworkId)

	pendingPayout := model.GetNetPendingPayout(session.Ctx, session.ByJwt.NetworkId)

	// ignore any error with circle, 
	// since the model won't allow the wallet to enter a corrupt state
	walletInfo, _ := findMostRecentCircleWallet(session)

	return &SubscriptionBalanceResult{
		BalanceByteCount: netBalanceByteCount,
		CurrentSubscription: currentSubscription,
		ActiveTransferBalances: transferBalances,
		PendingPayoutUsdNanoCents: pendingPayout,
		WalletInfo: walletInfo,
		UpdateTime: bringyour.NowUtc(),
	}, nil
}


// run this every 15 minutes
// circle.yml
func AutoPayout() {
	// auto accept payout as long as it is below an amount
	// otherwise require manual processing
	// FIXME use circle
}


// call from api
func SetPayoutWallet(ctx context.Context, networkId bringyour.Id, walletId bringyour.Id) {
	
}


// notification_count
// next_notify_time
func BalanceCodeNotify() {
	// in a loop get all unredeemed balance codes where next notify time is null or >= now
	// send a reminder that the customer has a balance code, and embed the code in the email
	// 1 day, 7 days, 30 days, 90 days (final reminder)
}

func notifyBalanceCode(balanceCodeId bringyour.Id) {

}


type StripeWebhookArgs struct {
	Id string `json:"id"`
	Type string `json:"type"`
	Data *StripeEventData `json:"data"`
}

type StripeEventData struct {
	Object *StripeEventDataObject `json:"object"`
}

type StripeEventDataObject struct {
	Id string `json:"id"`
	AmountTotal int `json:"amount_total"`
	CustomerDetails *StripeEventDataObjectCustomerDetails `json:"customer_details"`
	PaymentStatus string `json:"payment_status"`
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
	Id string `json:"id"`
	AmountTotal int `json:"amount_total"`
	Currency string `json:"currency"`
	Description string `json:"description"`
	Price *StripeLineItemProduct `json:"price"`
	Quantity int `json:"quantity"`
}

type StripeLineItemProduct struct {
	Id string `json:"id"`
	Product string `json:"product"`
	UnitAmount int `json:"unit_amount"`
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
		lineItems, err := bringyour.HttpGetRequireStatusOk[*StripeLineItems](
			url,
			func (header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiToken()))
			},
			bringyour.ResponseJsonObject[*StripeLineItems],
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

				bringyour.Logger().Printf("Create balance code: %s %s\n", purchaseEmail, string(stripeItemJsonBytes))

				if sku.Special == "" {
					err = CreateBalanceCode(
						clientSession.Ctx,
						sku.BalanceByteCount,
						netRevenue,
						stripeSessionId,
						string(stripeItemJsonBytes),
						purchaseEmail,
					)
					if err != nil {
						return nil, err
					}
				} else if sku.Special == SpecialCompany {
					// company shared data
					err := SendAccountMessageTemplate(
				        purchaseEmail,
				        &SubscriptionTransferBalanceCompanyTemplate{
				        	BalanceByteCount: sku.BalanceByteCount,
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
	Id string `json:"id"`
	Type string `json:"type"`
	Data *CoinbaseEventData `json:"data"`
}

type CoinbaseEventData struct {
	Id string `json:"id"`
	Name string `json:"name"`
	Description string `json:"description"`
	Payments []*CoinbaseEventDataPayment `json:"payments"`
	Checkout *CoinbaseEventDataCheckout `json:"checkout"`
	Metadata *CoinbaseEventDataMetadata `json:"metadata"`
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
	Local *CoinbaseEventDataPaymentAmount `json:"local"`
	Crypto *CoinbaseEventDataPaymentAmount `json:"crypto"`
}

type CoinbaseEventDataPaymentAmount struct {
	Amount string `json:"amount"`
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
				sku.BalanceByteCount,
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

		return SendAccountMessageTemplate(
	        balanceCode.PurchaseEmail,
	        &SubscriptionTransferBalanceCodeTemplate{
	        	Secret: balanceCode.Secret,
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

		return SendAccountMessageTemplate(
	        balanceCode.PurchaseEmail,
	        &SubscriptionTransferBalanceCodeTemplate{
	        	Secret: balanceCode.Secret,
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

	result, err := bringyour.HttpPostForm(
		"https://accounts.google.com/o/oauth2/token",
		form,
		bringyour.NoCustomHeaders,
		bringyour.ResponseJsonObject[map[string]any],
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
	Version string `json:"version"`
	PackageName string  `json:"packageName"`
	SubscriptionNotification *PlaySubscriptionNotification `json:"subscriptionNotification,omitempty"`
}

type PlaySubscriptionNotification struct {
	Version string `json:"version"`
	NotificationType int `json:"notificationType"`
	PurchaseToken string `json:"purchaseToken"`
	SubscriptionId string `json:"subscriptionId"`
}

// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptions
type PlaySubscription struct {
	StartTimeMillis string `json:"startTimeMillis"`
	ExpiryTimeMillis string `json:"expiryTimeMillis"`
	AutoRenewing bool `json:"autoRenewing"`
	PriceCurrencyCode string `json:"priceCurrencyCode"`
	PriceAmountMicros string `json:"priceAmountMicros"`
	CountryCode string `json:"countryCode"`
	DeveloperPayload string `json:"developerPayload"`
	PaymentState int `json:"paymentState"`
	OrderId string `json:"orderId"`
	AcknowledgementState int `json:"acknowledgementState"`
	Kind string `json:"kind"`
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
			sub, err := bringyour.HttpGetRequireStatusOk[*PlaySubscription](
				url,
				playAuthHeaders,
				bringyour.ResponseJsonObject[*PlaySubscription],
			)
			if err != nil {
				return nil, err
			}

			bringyour.Logger().Printf("Got Google Play sub: %v\n", sub)

			subscriptionPaymentId, err := bringyour.ParseId(sub.ObfuscatedExternalAccountId)
			if err != nil {
				return nil, err
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
				bringyour.HttpPostRawRequireStatusOk(
					url,
					[]byte{},
					playAuthHeaders,
				)

				// continually renew as long as the expiry time keeps getting pushed forward
				// note RTDN messages for renewal may unreliably delivered, so Google
				// recommends polling their system around the expiry time
				task.ScheduleTask(
					PlaySubscriptionRenewal,
					&PlaySubscriptionRenewalArgs{
						NetworkId: networkId,
						PackageName: rtdnMessage.PackageName,
						SubscriptionId: rtdnMessage.SubscriptionNotification.SubscriptionId,
						PurchaseToken: rtdnMessage.SubscriptionNotification.PurchaseToken,
						CheckTime: time.UnixMilli(sub.requireExpiryTimeMillis()),
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
	NetworkId bringyour.Id `json:"network_id"`
	PackageName string `json:"package_name"`
	SubscriptionId string `json:"subscription_id"`
	PurchaseToken string `json:"purchase_token"`
	CheckTime time.Time `json:"check_time"`
	// ExpiryTime time.Time `json:"expiry_time"`
}

type PlaySubscriptionRenewalResult struct {
	ExpiryTime time.Time `json:"expiry_time"`
	Renewed bool `json:"renewed"`
}

func SchedulePlaySubscriptionRenewal(
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
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
	sub, err := bringyour.HttpGetRequireStatusOk[*PlaySubscription](
		url,
		playAuthHeaders,
		bringyour.ResponseJsonObject[*PlaySubscription],
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
			transferBalance := &model.TransferBalance{
				NetworkId: playSubscriptionRenewal.NetworkId,
				StartTime: startTime,
				EndTime: expiryTime.Add(SubscriptionGracePeriod),
				StartBalanceByteCount: sku.BalanceByteCount,
				NetRevenue: model.UsdToNanoCents((1.0 - sku.FeeFraction) * priceAmountMicros / float64(1000 * 1000)),
				BalanceByteCount: sku.BalanceByteCount,
				PurchaseToken: playSubscriptionRenewal.PurchaseToken,
			}
			model.AddTransferBalance(
				clientSession.Ctx,
				transferBalance,
			)
		} else {
			return nil, fmt.Errorf("Play sku not found: %s", skuName)
		}

		return &PlaySubscriptionRenewalResult{
			ExpiryTime: expiryTime,
			Renewed: true,
		}, nil
	} else {
		// a transfer balance was already for the current expiry time
		// hence, the subscription has not been extended/renewed
		return &PlaySubscriptionRenewalResult{
			ExpiryTime: expiryTime,
			Renewed: false,
		}, nil
	}
}

func PlaySubscriptionRenewalPost(
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	playSubscriptionRenewalResult *PlaySubscriptionRenewalResult,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
) error {
	if playSubscriptionRenewalResult.Renewed {
		playSubscriptionRenewal.CheckTime = playSubscriptionRenewalResult.ExpiryTime
		SchedulePlaySubscriptionRenewal(
			clientSession,
			tx,
			playSubscriptionRenewal,
		)
	} else if bringyour.NowUtc().Before(playSubscriptionRenewalResult.ExpiryTime.Add(SubscriptionGracePeriod)) {
		// check again in 30 minutes
		playSubscriptionRenewal.CheckTime = bringyour.NowUtc().Add(30 * time.Minute)
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
		SendAccountMessageTemplate(
            userAuth,
            &SubscriptionEndedTemplate{},
        )
	}
	return nil
}


func VerifyStripeBody(req *http.Request)(io.Reader, error) {
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


func VerifyCoinbaseBody(req *http.Request)(io.Reader, error) {
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


func VerifyPlayBody(req *http.Request)(io.Reader, error) {
	// see https://cloud.google.com/pubsub/docs/authenticate-push-subscriptions?hl=en#protocol
	err := verifyPlayAuth(req.Header.Get("Authorization"))
	if err != nil {
		return nil, err
	}

	return req.Body, nil
}

func verifyPlayAuth(auth string) error {
	bearerPrefix := "Bearer "
	if strings.HasPrefix(auth, bearerPrefix) {
		jwt := auth[len(bearerPrefix):len(auth)]
		url := fmt.Sprintf("https://oauth2.googleapis.com/tokeninfo?id_token=%s", jwt)

		claimBytes, err := bringyour.HttpGetRawRequireStatusOk(url, bringyour.NoCustomHeaders)
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

func AddInitialTransferBalance(ctx context.Context, networkId bringyour.Id) bool {
	startTime := bringyour.NowUtc()
	endTime := startTime.Add(InitialTransferBalanceDuration)
	return model.AddBasicTransferBalance(
		ctx,
		networkId,
		InitialTransferBalance,
		startTime,
		endTime,
	)
}


// BACKFILL INITIAL TRANSFER BALANCE

type BackfillInitialTransferBalanceArgs struct {
}

type BackfillInitialTransferBalanceResult struct {
}

func ScheduleBackfillInitialTransferBalance(clientSession *session.ClientSession, tx bringyour.PgTx) {
    task.ScheduleTaskInTx(
        tx,
        BackfillInitialTransferBalance,
        &BackfillInitialTransferBalanceArgs{},
        clientSession,
        task.RunOnce("backfill_initial_transfer_balance"),
        task.RunAt(bringyour.NowUtc().Add(15 * time.Minute)),
    )
}

func BackfillInitialTransferBalance(
    backfillInitialTransferBalance *BackfillInitialTransferBalanceArgs,
    clientSession *session.ClientSession,
) (*BackfillInitialTransferBalanceResult, error) {
    networkIds := model.FindNetworksWithoutTransferBalance(clientSession.Ctx)
	for _, networkId := range networkIds {
		// add initial transfer balance
		AddInitialTransferBalance(clientSession.Ctx, networkId)
	}
	return &BackfillInitialTransferBalanceResult{}, nil
}

func BackfillInitialTransferBalancePost(
    backfillInitialTransferBalance *BackfillInitialTransferBalanceArgs,
    backfillInitialTransferBalanceResult *BackfillInitialTransferBalanceResult,
    clientSession *session.ClientSession,
    tx bringyour.PgTx,
) error {
    ScheduleBackfillInitialTransferBalance(clientSession, tx)
    return nil
}


// FIXME
// FIXME
// FIXME PlanPayments and payment loop


