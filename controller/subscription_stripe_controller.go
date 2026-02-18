package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/ephemeralkey"

	stripesession "github.com/stripe/stripe-go/v82/billingportal/session"
	"github.com/stripe/stripe-go/v82/subscription"
	stripewebhook "github.com/stripe/stripe-go/v82/webhook"
)

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

		transferBalance := &model.TransferBalance{
			NetworkId:             *networkId,
			StartTime:             startTime,
			EndTime:               endTime,
			StartBalanceByteCount: RefreshSupporterTransferBalance,
			SubsidyNetRevenue:     netRevenue,
			BalanceByteCount:      RefreshSupporterTransferBalance,
		}

		model.AddTransferBalanceInTx(
			clientSession.Ctx,
			tx,
			transferBalance,
		)

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

func UnsubscribeStripe(session *session.ClientSession) error {

	var subscriptionRenewals []struct {
		TransactionId string
		EndTime       time.Time
	}
	var queryErr error

	server.Tx(session.Ctx, func(tx server.PgTx) {

		// query if network has active stripe subscriptions
		result, err := tx.Query(
			session.Ctx,
			`
			SELECT transaction_id, end_time
			FROM subscription_renewal
			WHERE
				network_id = $1
				AND market = $2
				AND end_time > $3
			ORDER BY end_time DESC
			`,
			session.ByJwt.NetworkId,
			model.SubscriptionMarketStripe,
			server.NowUtc(),
		)

		if err != nil {
			glog.Errorf("[unsubscribe] Failed to query subscription_renewal: %v", err)
			queryErr = err
			return
		}

		server.WithPgResult(result, err, func() {
			for result.Next() {

				var txId string
				var endTime time.Time

				err := result.Scan(&txId, &endTime)
				if err != nil {
					glog.Errorf("[unsubscribe] Failed to scan subscription renewal: %v", err)
					queryErr = err
					continue
				}

				subscriptionRenewals = append(subscriptionRenewals, struct {
					TransactionId string
					EndTime       time.Time
				}{
					TransactionId: txId,
					EndTime:       endTime,
				})

			}
		})

	})

	if queryErr != nil {
		return fmt.Errorf("[unsubscribe] failed to query subscription renewals: %w", queryErr)
	}

	if len(subscriptionRenewals) == 0 {
		return nil
	}

	stripe.Key = stripeApiToken()

	for _, renewal := range subscriptionRenewals {

		invoiceId := renewal.TransactionId
		if invoiceId == "" {
			glog.Infof("[unsubscribe] Subscription renewal with empty transaction id, skipping")
			continue
		}

		// Get invoice details with expanded subscription info
		url := fmt.Sprintf("https://api.stripe.com/v1/invoices/%s?expand[]=subscription&expand[]=customer", invoiceId)
		fullInvoice, err := server.HttpGetRequireStatusOk[*StripeInvoiceExpanded](
			session.Ctx,
			url,
			func(header http.Header) {
				header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiToken()))
			},
			server.ResponseJsonObject[*StripeInvoiceExpanded],
		)

		if err != nil {
			glog.Errorf("[unsubscribe] Failed to fetch invoice %s: %v", invoiceId, err)
			continue
		}

		// Extract subscription ID
		var subscriptionId string
		if fullInvoice.Subscription != nil && fullInvoice.Subscription.ID != "" {
			subscriptionId = fullInvoice.Subscription.ID
		}

		if subscriptionId == "" {
			glog.Errorf("[unsubscribe] No subscription ID found for invoice %s", invoiceId)
			continue
		}

		// Cancel the subscription
		glog.Infof("[unsubscribe] Canceling Stripe subscription %s for network %s", subscriptionId, session.ByJwt.NetworkId)

		cancelUrl := fmt.Sprintf("https://api.stripe.com/v1/subscriptions/%s", subscriptionId)

		req, err := http.NewRequestWithContext(session.Ctx, "DELETE", cancelUrl, nil)
		if err != nil {
			glog.Errorf("[unsubscribe] Failed to create cancel request for subscription %s: %v", subscriptionId, err)
			continue
		}

		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiToken()))

		httpClient := server.DefaultHttpClient()
		resp, err := httpClient.Do(req)
		if err != nil {
			glog.Errorf("[unsubscribe] Failed to cancel Stripe subscription %s: %v", subscriptionId, err)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			glog.Errorf("[unsubscribe] Stripe API returned error status %d for subscription %s", resp.StatusCode, subscriptionId)
			continue
		}

		glog.Infof("[unsubscribe] Successfully canceled Stripe subscription %s", subscriptionId)

	}

	// set subscription_renewal end time as now for all active subscriptions
	// to prevent transfer balance from being unnecessarily added
	server.Tx(session.Ctx, func(tx server.PgTx) {

		_, err := tx.Exec(
			session.Ctx,
			`
			UPDATE subscription_renewal
			SET end_time = $1
			WHERE
				network_id = $2
				AND market = $3
				AND end_time > $4
			`,
			server.NowUtc(),
			session.ByJwt.NetworkId,
			model.SubscriptionMarketStripe,
			server.NowUtc(),
		)

		if err != nil {
			glog.Errorf("[unsubscribe] Failed to update subscription_renewal end times: %v", err)
		}

	})

	return nil
}
