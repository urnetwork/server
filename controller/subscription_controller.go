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

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/ephemeralkey"

	stripesession "github.com/stripe/stripe-go/v82/billingportal/session"
	"github.com/stripe/stripe-go/v82/subscription"
	stripewebhook "github.com/stripe/stripe-go/v82/webhook"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const InitialTransferBalance = 32 * model.Gib

// 30 days
const InitialTransferBalanceDuration = 30 * 24 * time.Hour

const RefreshTransferBalanceDuration = 30 * time.Hour

// const RefreshTransferBalanceTimeout = 24 * time.Hour

const RefreshSupporterTransferBalance = 600 * model.Gib
const RefreshFreeTransferBalance = 60 * model.Gib

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

var stripeWebhookSigningSecret = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["webhook"].(map[string]any)["signing_secret"].(string)
})

var stripeApiToken = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["api"].(map[string]any)["token"].(string)
})

var stripePublishableKey = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["api"].(map[string]any)["publishable_key"].(string)
})

var stripeSkus = sync.OnceValue(func() map[string]*Sku {
	var skus Skus
	server.Config.RequireSimpleResource("stripe.yml").UnmarshalYaml(&skus)
	return skus.Skus
})

type StripeSubscriptionPrices struct {
	Yearly  string `yaml:"yearly"`
	Monthly string `yaml:"monthly"`
}

var stripeSubscriptionPrices = sync.OnceValue(func() StripeSubscriptionPrices {
	c := server.Vault.RequireSimpleResource("stripe.yml").Parse()
	return c["subscription_prices"].(StripeSubscriptionPrices)
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
	isPro := false

	for _, transferBalance := range transferBalances {
		netBalanceByteCount += transferBalance.BalanceByteCount
		startBalanceByteCount += transferBalance.StartBalanceByteCount

		if !isPro && transferBalance.Paid {
			// check if any of the transfer balances are from a pro subscription
			isPro = true
		}

	}

	openTransferByteCount := model.GetOpenTransferByteCount(session.Ctx, session.ByJwt.NetworkId)

	var currentSubscription *Subscription

	_, market := model.HasSubscriptionRenewal(session.Ctx, session.ByJwt.NetworkId, model.SubscriptionTypeSupporter)

	if isPro {
		currentSubscription = &Subscription{
			Plan: model.SubscriptionTypeSupporter,
		}
	}

	if market != nil {
		currentSubscription.Store = *market
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

type StripeWebhookArgs struct {
	Id   string           `json:"id"`
	Type string           `json:"type"`
	Data *StripeEventData `json:"data"`
}

type StripeEventData struct {
	Object json.RawMessage `json:"object"`
}

type StripeEventCheckoutCompleteObject struct {
	Id                string                                `json:"id"`
	AmountTotal       int                                   `json:"amount_total"`
	Customer          string                                `json:"customer"`
	CustomerDetails   *StripeEventDataObjectCustomerDetails `json:"customer_details"`
	PaymentStatus     string                                `json:"payment_status"`
	ClientReferenceId string                                `json:"client_reference_id"`
	Subscription      string                                `json:"subscription"`
}

type StripeEventInvoiceObject struct {
	Id          string `json:"id"`
	Total       int    `json:"total"`
	PeriodStart int    `json:"period_start"`
	PeriodEnd   int    `json:"period_end"`
	Customer    string `json:"customer"`
}

type StripeEventDataObjectCustomerDetails struct {
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"`
}

type StripeWebhookResultErr struct {
	Message string `json:"message"`
}

type StripeWebhookResult struct {
	Error *StripeWebhookResultErr `json:"message,omitempty"`
}

type StripeLineItems struct {
	Data []*StripeLineItem `json:"data"`
}

type StripeSubscription struct {
	Id   string                    `json:"id"`
	Data []*StripeSubscriptionItem `json:"data"`
}

type StripeSubscriptionItem struct {
	Id                 string `json:"id"`
	CurrentPeriodStart int64  `json:"current_period_start"`
	CurrentPeriodEnd   int64  `json:"current_period_end"`
}

type StripeLineItem struct {
	Id           string                 `json:"id"`
	AmountTotal  int                    `json:"amount_total"`
	Currency     string                 `json:"currency"`
	Description  string                 `json:"description"`
	Price        *StripeLineItemProduct `json:"price"`
	Quantity     int                    `json:"quantity"`
	Type         string                 `json:"type"`
	Subscription *string                `json:"subscription,omitempty"`
	Period       *StripeLineItemPeriod  `json:"period,omitempty"`
}

type StripeLineItemPeriod struct {
	End   int64 `json:"end"`
	Start int64 `json:"start"`
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

	glog.Infof("Stripe webhook hit: %s", stripeWebhook.Id)

	if stripeWebhook.Type == "checkout.session.completed" {

		/**
		 * need to check, but I think this is deprecated?
		 * used for topping up individual transfer balances, which we don't have in the app atm
		 */

		glog.Infof("type: checkout.session.completed")

		var checkoutCompleteObject StripeEventCheckoutCompleteObject
		if err := json.Unmarshal(stripeWebhook.Data.Object, &checkoutCompleteObject); err != nil {
			glog.Errorf("Failed to parse checkout session: %v", err)
			return nil, fmt.Errorf("failed to parse invoice: %v", err)
		}

		return stripeHandleCheckoutSessionCompleted(
			&checkoutCompleteObject,
			clientSession,
		)

	} else if stripeWebhook.Type == "invoice.paid" {

		/**
		 * new user subscription
		 */

		glog.Infof("type: invoice.paid")

		var invoiceObject StripeEventInvoiceObject
		if err := json.Unmarshal(stripeWebhook.Data.Object, &invoiceObject); err != nil {
			return nil, fmt.Errorf("failed to parse invoice: %v", err)
		}

		return stripeHandleInvoicePaid(
			&invoiceObject,
			clientSession,
		)

	}
	// else ignore the event

	return &StripeWebhookResult{}, nil
}

func stripeHandleCheckoutSessionCompleted(
	checkoutComplete *StripeEventCheckoutCompleteObject,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {

	glog.Infof("Stripe stripeHandleCheckoutSessionCompleted")

	stripeSessionId := checkoutComplete.Id

	// need to make a second call to get the line items for the order
	// https://stripe.com/docs/api/checkout/sessions/line_items
	url := fmt.Sprintf(
		"https://api.stripe.com/v1/checkout/sessions/%s/line_items",
		stripeSessionId,
	)
	lineItems, err := server.HttpGetRequireStatusOk[*StripeLineItems](
		clientSession.Ctx,
		url,
		func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiToken()))
		},
		server.ResponseJsonObject[*StripeLineItems],
	)
	if err != nil {
		glog.Errorf("Failed to fetch line items: %v", err)
		return nil, err
	}

	purchaseEmail := checkoutComplete.CustomerDetails.Email
	if purchaseEmail == "" {
		glog.Infof("missing purchase email")
		return nil, errors.New("missing purchase email to send balance code")
	}

	skus := stripeSkus()
	for _, lineItem := range lineItems.Data {
		stripeSku := lineItem.Price.Product
		if sku, ok := skus[stripeSku]; ok {

			glog.Infof("Stripe stripeHandleCheckoutSessionCompleted")

			if sku.Supporter {

				// do nothing
				// handle subscription in `invoid.paid` event

			} else {

				/**
				 * Otherwise user is purchasing balance
				 */

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
						SubscriptionYearDuration,
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
						SenderEmail(EnvEmailConfig().CompanySenderEmail),
					)
					if err != nil {
						return nil, err
					}
				} else {
					return nil, fmt.Errorf("Stripe unknown special (%s) for sku: %s", sku.Special, stripeSku)
				}

			}

		} else {
			glog.Infof("Stripe sku not found: %s", stripeSku)
			return nil, fmt.Errorf("Stripe sku not found: %s", stripeSku)
		}
	}

	return &StripeWebhookResult{}, nil

}

type StripeCustomerExpanded struct {
	Id    string `json:"id"`
	Email string `json:"email"`
}

type StripeInvoiceExpanded struct {
	Customer     *StripeCustomerExpanded `json:"customer"`
	Lines        *StripeLineItems        `json:"lines"`
	Subscription *struct {
		ID       string            `json:"id"`
		Metadata map[string]string `json:"metadata"`
	} `json:"subscription"`
}

func stripeHandleInvoicePaid(
	invoice *StripeEventInvoiceObject,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {

	invoiceId := invoice.Id
	total := invoice.Total

	url := fmt.Sprintf("https://api.stripe.com/v1/invoices/%s?expand[]=subscription&expand[]=customer", invoiceId)
	fullInvoice, err := server.HttpGetRequireStatusOk[*StripeInvoiceExpanded](
		clientSession.Ctx,
		url,
		func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiToken()))
		},
		server.ResponseJsonObject[*StripeInvoiceExpanded],
	)
	if err != nil {
		glog.Errorf("Failed to fetch invoice details: %v", err)
		return nil, fmt.Errorf("failed to fetch invoice details: %v", err)
	}

	var subscriptionId string
	var periodStart int64
	var periodEnd int64
	isSubscription := false

	if fullInvoice.Lines != nil {
		if len(fullInvoice.Lines.Data) > 0 {

			if lineItem := fullInvoice.Lines.Data[0]; lineItem != nil {
				// Check if this is a subscription line item
				// if itemType, ok := lineItem["type"].(string); ok && itemType == "subscription" {
				if itemType := lineItem.Type; itemType == "subscription" {
					isSubscription = true

					if lineItem.Subscription != nil && *lineItem.Subscription != "" {
						subscriptionId = *lineItem.Subscription
					}

					// Extract period information
					if period := lineItem.Period; period != nil {
						periodStart = period.Start
						periodEnd = period.End
					}
				} else {
					glog.Infof("Line item is not a subscription type: %s", itemType)
				}
			}
		} else {
			glog.Infof("No line items found in invoice")
		}
	} else {
		glog.Infof("No lines object found in invoice")
	}

	if !isSubscription {
		glog.Infof("Invoice is not for a subscription")
		return &StripeWebhookResult{}, nil
	}

	if subscriptionId == "" {
		glog.Infof("Invoice does not have a subscription in its line items")
		return &StripeWebhookResult{}, nil
	}

	var networkId *server.Id

	// check subscription metadata for network id
	if fullInvoice.Subscription != nil {

		glog.Infof("checking subscription metadata for network id")

		glog.Infof("subscription metadata: %v", fullInvoice.Subscription.Metadata)

		if subNetworkId := fullInvoice.Subscription.Metadata["network_id"]; subNetworkId != "" {

			glog.Infof("found network id in subscription metadata: %s", subNetworkId)

			id, err := server.ParseId(subNetworkId)
			if err != nil {
				glog.Infof("Invalid network id format in subscription metadata: %v", err)

				// todo - we should store in the DB webhook errors table
				return nil, fmt.Errorf("failed to parse id err=%v id=%s", err, subNetworkId)

			} else {
				glog.Infof("found network id in subscription metadata: %s", id.String())
				networkId = &id
			}
		} else {
			glog.Infof("no network id in subscription metadata")
		}
	}

	// next, try and associate networkId by email
	if networkId == nil && fullInvoice != nil && fullInvoice.Customer != nil && fullInvoice.Customer.Email != "" {

		glog.Infof("trying to find network by email: %s", fullInvoice.Customer.Email)

		// search network by email
		foundId, err := model.FindNetworkIdByEmail(clientSession.Ctx, fullInvoice.Customer.Email)

		if err != nil {
			return nil, fmt.Errorf("failed to find network by email: %v", err)
		}

		if foundId != nil {
			networkId = foundId
		}

	}

	if networkId == nil {

		glog.Infof("could not find network by email, checking checkout session")

		// could not find network by email
		// check for client_reference_id in the checkout session

		// Get the checkout session using the subscription ID
		url = fmt.Sprintf("https://api.stripe.com/v1/checkout/sessions?subscription=%s", subscriptionId)
		sessionsResp, err := server.HttpGetRequireStatusOk[map[string]interface{}](
			clientSession.Ctx,
			url,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiToken()))
			},
			server.ResponseJsonObject[map[string]interface{}],
		)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch checkout session: %v", err)
		}

		if dataArray, ok := sessionsResp["data"].([]interface{}); ok && len(dataArray) > 0 {
			if session, ok := dataArray[0].(map[string]interface{}); ok {
				if clientRefId, ok := session["client_reference_id"].(string); ok {
					id, err := server.ParseId(clientRefId)
					if err != nil {
						return nil, fmt.Errorf("failed to parse id err=%v id=%s", err, clientRefId)
					}
					networkId = &id
				}
			}
		}
	}

	feeFraction := 0.3
	netRevenue := model.UsdToNanoCents((1.0 - feeFraction) * float64(total) / 100.0)

	startTime := time.Unix(periodStart, 0)
	endTime := time.Unix(periodEnd, 0).Add(SubscriptionGracePeriod)

	if networkId == nil {
		return nil, fmt.Errorf("could not find network for invoice %s", invoiceId)
	}

	subscriptionRenewal := model.SubscriptionRenewal{
		NetworkId:          *networkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          startTime,
		EndTime:            endTime,
		NetRevenue:         netRevenue,
		SubscriptionMarket: model.SubscriptionMarketStripe,
		TransactionId:      invoiceId,
	}

	/**
	 * all db inserts should be in a single a transaction
	 * if any part fails, return an error response to stripe
	 * this way stripe will retry the webhook later and we can roll back partial data
	 */
	var insertErr error

	server.Tx(clientSession.Ctx, func(tx server.PgTx) {

		err := model.AddSubscriptionRenewalInTx(tx, clientSession.Ctx, &subscriptionRenewal)
		if err != nil {
			glog.Infof("Error adding subscription renewal: %v", err)
			insertErr = err
			return
		}

		err = AddRefreshTransferBalanceInTx(tx, clientSession.Ctx, *networkId)
		if err != nil {
			glog.Infof("Error adding refresh transfer balance task: %v", err)
			insertErr = err
		}

	})

	if insertErr != nil {
		glog.Infof("Error processing invoice paid: %v", insertErr)
		return nil, insertErr
	}

	return &StripeWebhookResult{}, nil

}

type SubscriptionType string

const (
	SubscriptionTypeMonthly SubscriptionType = "monthly"
	SubscriptionTypeYearly  SubscriptionType = "yearly"
)

type StripeCreatePaymentIntentArgs struct{}

type StripeCreatePaymentIntentArgsErr struct {
	Message string `json:"message"`
}

type StripePaymentIntent struct {
	SubscriptionType SubscriptionType `json:"subscription_type"`
	ClientSecret     string           `json:"client_secret"`
}

type StripeCreatePaymentIntentResult struct {
	PaymentIntents []StripePaymentIntent             `json:"payment_intents,omitempty"`
	EphemeralKey   *string                           `json:"ephemeral_key,omitempty"`
	CustomerId     *string                           `json:"customer_id,omitempty"`
	PublishableKey *string                           `json:"publishable_key,omitempty"`
	Error          *StripeCreatePaymentIntentArgsErr `json:"error,omitempty"`
}

func StripeCreatePaymentIntent(
	args *StripeCreatePaymentIntentArgs,
	session *session.ClientSession,
) (*StripeCreatePaymentIntentResult, error) {

	// check if user has a stripe customer id
	stripeCustomerId, _ := model.GetStripeCustomer(session)

	stripe.Key = stripeApiToken()

	if stripeCustomerId == nil {
		// create a new stripe customer
		params := &stripe.CustomerParams{}
		result, err := customer.New(params)
		if err != nil {
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: fmt.Sprintf("Failed to create stripe customer: %v", err)},
			}, nil
		}

		err = model.CreateStripeCustomer(result.ID, session)
		if err != nil {
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: fmt.Sprintf("Failed to save stripe customer: %v", err)},
			}, nil
		}

		stripeCustomerId = &result.ID
	}

	ekparams := &stripe.EphemeralKeyParams{
		Customer:      stripeCustomerId,
		StripeVersion: stripe.String("2023-08-16"),
	}

	ek, err := ephemeralkey.New(ekparams)
	if err != nil {
		return &StripeCreatePaymentIntentResult{
			Error: &StripeCreatePaymentIntentArgsErr{Message: fmt.Sprintf("Failed to create stripe ephemeral key: %v", err)},
		}, nil
	}

	// prices := stripeSubscriptionPrices()

	type SubPrice struct {
		PriceId string
		Type    SubscriptionType
	}

	var subPrices []SubPrice = []SubPrice{
		// stripeSubscriptionPrices().Monthly,
		// stripeSubscriptionPrices().Yearly,
		SubPrice{
			PriceId: "price_1RGDllEqqTaiwAGPOOvPrHUX",
			Type:    SubscriptionTypeMonthly,
		},
		SubPrice{
			PriceId: "price_1S5Bd2EqqTaiwAGPuCdDFm6a",
			Type:    SubscriptionTypeYearly,
		},
	}

	var intents []StripePaymentIntent

	for _, subPrice := range subPrices {

		params := &stripe.SubscriptionParams{
			Customer: stripeCustomerId,
			Items: []*stripe.SubscriptionItemsParams{
				{Price: &subPrice.PriceId},
			},
			PaymentBehavior: stripe.String("default_incomplete"),
			// Save payment method for future renewals
			PaymentSettings: &stripe.SubscriptionPaymentSettingsParams{
				SaveDefaultPaymentMethod: stripe.String("on_subscription"),
			},
			Metadata: map[string]string{
				"network_id": session.ByJwt.NetworkId.String(),
			},
			Expand: []*string{
				stripe.String("latest_invoice"),
			},
		}

		sub, err := subscription.New(params)
		if err != nil {
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: fmt.Sprintf("Failed to create stripe subscription: %v", err)},
			}, nil
		}

		if sub.LatestInvoice == nil || sub.LatestInvoice.ID == "" {
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: "No latest invoice found on the subscription"},
			}, nil
		}

		// Define helper structs to unmarshal the invoice with PaymentIntent
		type PaymentIntent struct {
			ID           string `json:"id"`
			ClientSecret string `json:"client_secret"`
		}

		type InvoiceWithPI struct {
			PaymentIntent *PaymentIntent `json:"payment_intent"`
		}

		// Fetch the invoice with PaymentIntent expanded
		url := fmt.Sprintf(
			"https://api.stripe.com/v1/invoices/%s?expand[]=payment_intent",
			sub.LatestInvoice.ID,
		)

		invoice, err := server.HttpGetRequireStatusOk[*InvoiceWithPI](
			session.Ctx,
			url,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiToken()))
			},
			server.ResponseJsonObject[*InvoiceWithPI],
		)

		if err != nil {
			glog.Infof("Failed to fetch invoice details: %v", err)
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: fmt.Sprintf("Failed to fetch invoice details: %v", err)},
			}, nil
		}

		if invoice.PaymentIntent == nil || invoice.PaymentIntent.ClientSecret == "" {
			glog.Infof("No payment intent found on the latest invoice")
			return &StripeCreatePaymentIntentResult{
				Error: &StripeCreatePaymentIntentArgsErr{Message: "No payment intent found on the latest invoice"},
			}, nil
		}

		intents = append(intents, StripePaymentIntent{
			ClientSecret:     invoice.PaymentIntent.ClientSecret,
			SubscriptionType: subPrice.Type,
		})
	}

	pk := stripePublishableKey()

	return &StripeCreatePaymentIntentResult{
		PaymentIntents: intents,
		EphemeralKey:   &ek.Secret,
		CustomerId:     stripeCustomerId,
		PublishableKey: &pk,
	}, nil
}

type StripeCreateCustomerPortalError struct {
	Message string `json:"message"`
}

type StripeCreateCustomerPortalResult struct {
	Url   string                           `json:"url,omitempty"`
	Error *StripeCreateCustomerPortalError `json:"error,omitempty"`
}

type StripeCreateCustomerPortalArgs struct{}

/**
 * Used to create a Stripe URL for the customer to manage their subscription
 */
func StripeCreateCustomerPortal(
	args *StripeCreateCustomerPortalArgs,
	session *session.ClientSession,
) (*StripeCreateCustomerPortalResult, error) {

	stripeCustomerId, err := model.GetStripeCustomer(session)
	if err != nil || stripeCustomerId == nil {
		return &StripeCreateCustomerPortalResult{
			Error: &StripeCreateCustomerPortalError{Message: "No stripe customer found"},
		}, nil
	}

	stripe.Key = stripeApiToken()

	params := &stripe.BillingPortalSessionParams{
		Customer: stripeCustomerId,
		// ReturnURL: stripe.String("https://example.com/account"),
	}

	result, err := stripesession.New(params)

	if err != nil {
		return &StripeCreateCustomerPortalResult{
			Error: &StripeCreateCustomerPortalError{Message: fmt.Sprintf("Failed to create stripe customer portal session: %v", err)},
		}, nil
	}

	return &StripeCreateCustomerPortalResult{
		Url: result.URL,
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
				SubscriptionYearDuration,
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
	duration time.Duration,
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
			duration,
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
					renewal := &model.SubscriptionRenewal{
						NetworkId:          playSubscriptionRenewal.NetworkId,
						StartTime:          startTime,
						EndTime:            maxExpiryTime.Add(SubscriptionGracePeriod),
						NetRevenue:         model.UsdToNanoCents((1.0 - sku.FeeFraction) * sku.PriceAmountUsd),
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
						EndTime:               maxExpiryTime.Add(SubscriptionGracePeriod),
						StartBalanceByteCount: sku.BalanceByteCount(),
						NetRevenue:            model.UsdToNanoCents((1.0 - sku.FeeFraction) * sku.PriceAmountUsd),
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

func AddRefreshTransferBalance(ctx context.Context, networkId server.Id) error {
	startTime := server.NowUtc()
	endTime := startTime.Add(RefreshTransferBalanceDuration)
	var transferBalance model.ByteCount

	active, _ := model.HasSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeSupporter)

	if active {
		transferBalance = RefreshSupporterTransferBalance
	} else {
		transferBalance = RefreshFreeTransferBalance
	}
	return model.AddBasicTransferBalance(
		ctx,
		networkId,
		transferBalance,
		startTime,
		endTime,
	)

}

func AddRefreshTransferBalanceInTx(tx server.PgTx, ctx context.Context, networkId server.Id) error {
	startTime := server.NowUtc()
	endTime := startTime.Add(RefreshTransferBalanceDuration)
	var transferBalance model.ByteCount

	active, _ := model.HasSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeSupporter)

	if active {
		transferBalance = RefreshSupporterTransferBalance
	} else {
		transferBalance = RefreshFreeTransferBalance
	}
	err := model.AddBasicTransferBalanceInTx(
		tx,
		ctx,
		networkId,
		transferBalance,
		startTime,
		endTime,
	)
	if err != nil {
		return err
	}

	return nil
}

// Refresh transfer balances

type RefreshTransferBalancesArgs struct {
}

type RefreshTransferBalancesResult struct {
}

func ScheduleRefreshTransferBalances(clientSession *session.ClientSession, tx server.PgTx) {
	year, month, day := server.NowUtc().Date()
	runAt := time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
	task.ScheduleTaskInTx(
		tx,
		RefreshTransferBalances,
		&RefreshTransferBalancesArgs{},
		clientSession,
		task.RunOnce("refresh_transfer_balances"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
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
			return &HeliusWebhookResult{
				Message: fmt.Sprintf("Ignoring non-transfer transaction of type %s", transaction.Type),
			}, nil
		}

		if len(transaction.TokenTransfers) == 0 {
			return &HeliusWebhookResult{
				Message: "Ignoring transaction with no token transfers",
			}, nil
		}

		paymentReceived := false
		var tokenAmountReceived float64

		for _, tokenTransfer := range transaction.TokenTransfers {

			if tokenTransfer.Mint == solanaUsdcMint &&
				slices.Contains(solanaReceiverAddresses, tokenTransfer.ToUserAccount) &&
				tokenTransfer.TokenAmount >= 40 {
				paymentReceived = true
				tokenAmountReceived = tokenTransfer.TokenAmount
			}

		}

		if !paymentReceived {
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
			return &HeliusWebhookResult{
				Message: "Server err calling SearchPaymentIntents",
			}, nil
		}

		if paymentSearchResult == nil {
			return &HeliusWebhookResult{
				Message: "No payment intent found for this network ID",
			}, nil
		}

		// good to go, create transfer balance + year plan
		startTime := server.NowUtc()
		endTime := startTime.Add(SubscriptionYearDuration + SubscriptionGracePeriod)

		subscriptionRenewal := model.SubscriptionRenewal{
			NetworkId:          *paymentSearchResult.NetworkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          startTime,
			EndTime:            endTime,
			NetRevenue:         model.UsdToNanoCents(tokenAmountReceived),
			SubscriptionMarket: model.SubscriptionMarketSolana,
			TransactionId:      paymentSearchResult.PaymentReference,
		}

		model.AddSubscriptionRenewal(clientSession.Ctx, &subscriptionRenewal)

		AddRefreshTransferBalance(clientSession.Ctx, *paymentSearchResult.NetworkId)

		model.MarkPaymentIntentCompleted(
			paymentSearchResult.PaymentReference,
			transaction.Signature,
			clientSession,
		)

		matched++
	}

	if matched == 0 {
		return &HeliusWebhookResult{Message: "No matching payments"}, nil
	}
	return &HeliusWebhookResult{Message: fmt.Sprintf("Processed %d matching payments", matched)}, nil
}

/**
 * Solana Payment intents
 * We create a reference for each payment intent and map it to the network ID
 */

type SolanaPaymentIntentArgs struct {
	Reference string `json:"reference"`
}

type SolanaPaymentIntentResult struct {
	Error *SolanaPaymentIntentError `json:"error,omitempty"`
}

type SolanaPaymentIntentError struct {
	Message string `json:"message"`
}

func CreateSolanaPaymentIntent(
	intent *SolanaPaymentIntentArgs,
	clientSession *session.ClientSession,
) (*SolanaPaymentIntentResult, error) {
	model.CreateSolanaPaymentIntent(intent.Reference, clientSession)
	return &SolanaPaymentIntentResult{}, nil
}
