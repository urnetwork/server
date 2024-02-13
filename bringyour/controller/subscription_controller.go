package controller

import (
	"context"
	"time"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"net/http"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"strings"
	"errors"
	"sync"

	stripewebhook "github.com/stripe/stripe-go/v76/webhook"

	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/task"
	"bringyour.com/bringyour"
)



const SubscriptionGracePeriod = 24 * time.Hour



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
			BalanceByteCount: model.ByteCount(300) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024),
		},
		// 1TiB
		"prod_Om2V4ElmxY5Civ": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024),
		},
		// 2Tib
		"prod_Om2XiaUQlgzawz": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(2) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024),
		},
	}
}

var coinbaseWebhookSigningSecret = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("coinbase.yml").Parse()
	return c["webhook"].(map[string]any)["signing_secret"].(string)
})

func coinbaseSkus() map[string]*Sku {
	playSubscriptionFeeFraction := 0.3
	// FIXME read from json
	return map[string]*Sku{
		"300GiB": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(300) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024),
		},
		"1TiB": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024),
		},
		"2TiB": &Sku{
			FeeFraction: playSubscriptionFeeFraction,
			BalanceByteCount: model.ByteCount(2) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024) * model.ByteCount(1024),
		},
	}
}

var playPublisherEmail = sync.OnceValue(func()(string) {
	c := bringyour.Vault.RequireSimpleResource("google.yml").Parse()
	return c["webhook"].(map[string]any)["publisher_email"].(string)
})


type Sku struct {
	// the fees on the payment amount
	FeeFraction float64
	BalanceByteCount model.ByteCount
}

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


func playPackageName() string {
	return "com.bringyour.network"
}




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
		UpdateTime: time.Now(),
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




/*

2024/02/06 23:29:20 subscription_handlers.go:34: Stripe webhook body: 
{
    "id": "evt_1Ogy4qEqqTaiwAGPEnrlPpV1",
    "object": "event",
    "api_version": "2023-08-16",
    "created": 1707262160,
    "data": {
        "object": {
            "id": "cs_live_a1Cp5DxxhmxF2wmXbSfRvvMpX9st6AmNZghLPvzt2gJTFBZDHyFveJbLRT",
            "object": "checkout.session",
            "after_expiration": null,
            "allow_promotion_codes": false,
            "amount_subtotal": 500,
            "amount_total": 500,
            "automatic_tax": {
                "enabled": true,
                "liability": {
                    "type": "self"
                },
                "status": "complete"
            },
            "billing_address_collection": "auto",
            "cancel_url": "https://stripe.com",
            "client_reference_id": null,
            "client_secret": null,
            "consent": null,
            "consent_collection": {
                "payment_method_reuse_agreement": null,
                "promotions": "none",
                "terms_of_service": "none"
            },
            "created": 1707262105,
            "currency": "usd",
            "currency_conversion": null,
            "custom_fields": [],
            "custom_text": {
                "after_submit": null,
                "shipping_address": null,
                "submit": null,
                "terms_of_service_acceptance": null
            },
            "customer": null,
            "customer_creation": "if_required",
            "customer_details": {
                "address": {
                    "city": "Redwood City",
                    "country": "US",
                    "line1": "330 Grand Street",
                    "line2": null,
                    "postal_code": "94062",
                    "state": "CA"
                },
                "email": "xcolwell@gmail.com",
                "name": "Brien Colwell",
                "phone": null,
                "tax_exempt": "none",
                "tax_ids": []
            },
            "customer_email": null,
            "expires_at": 1707348505,
            "invoice": null,
            "invoice_creation": {
                "enabled": false,
                "invoice_data": {
                    "account_tax_ids": null,
                    "custom_fields": null,
                    "description": null,
                    "footer": null,
                    "issuer": null,
                    "metadata": {},
                    "rendering_options": null
                }
            },
            "livemode": true,
            "locale": "auto",
            "metadata": {},
            "mode": "payment",
            "payment_intent": "pi_3Ogy4nEqqTaiwAGP0TvOuvDh",
            "payment_link": "plink_1NzPpjEqqTaiwAGPktia7ARw",
            "payment_method_collection": "if_required",
            "payment_method_configuration_details": {
                "id": "pmc_1NxxhLEqqTaiwAGPaM2pA7cM",
                "parent": null
            },
            "payment_method_options": {},
            "payment_method_types": [
                "card",
                "klarna",
                "link",
                "us_bank_account",
                "wechat_pay",
                "cashapp"
            ],
            "payment_status": "paid",
            "phone_number_collection": {
                "enabled": false
            },
            "recovered_from": null,
            "setup_intent": null,
            "shipping_address_collection": null,
            "shipping_cost": null,
            "shipping_details": null,
            "shipping_options": [],
            "status": "complete",
            "submit_type": "auto",
            "subscription": null,
            "success_url": "https://stripe.com",
            "total_details": {
                "amount_discount": 0,
                "amount_shipping": 0,
                "amount_tax": 0
            },
            "ui_mode": "hosted",
            "url": null
        }
    },
    "livemode": true,
    "pending_webhooks": 1,
    "request": {
        "id": null,
        "idempotency_key": null
    },
    "type": "checkout.session.completed"
}

*/


type StripeWebhookArgs struct {
	Id string `json:"id"`
	Type string `json:"data"`
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


/*
{
  "object": "list",
  "data": [
    {
      "id": "li_1Ogy3xEqqTaiwAGPfugdUKei",
      "object": "item",
      "amount_discount": 0,
      "amount_subtotal": 500,
      "amount_tax": 0,
      "amount_total": 500,
      "currency": "usd",
      "description": "300Gib Global Data Pack",
      "price": {
        "id": "price_1NzPpJEqqTaiwAGPl0uHkOKK",
        "object": "price",
        "active": true,
        "billing_scheme": "per_unit",
        "created": 1696882397,
        "currency": "usd",
        "custom_unit_amount": null,
        "livemode": true,
        "lookup_key": null,
        "metadata": {},
        "nickname": null,
        "product": "prod_OlUgT5brBfOBiT",
        "recurring": null,
        "tax_behavior": "unspecified",
        "tiers_mode": null,
        "transform_quantity": null,
        "type": "one_time",
        "unit_amount": 500,
        "unit_amount_decimal": "500"
      },
      "quantity": 1
    }
  ],
  "has_more": false,
  "url": "/v1/checkout/sessions/cs_live_a1Cp5DxxhmxF2wmXbSfRvvMpX9st6AmNZghLPvzt2gJTFBZDHyFveJbLRT/line_items"
}
*/


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

// FIXME need to make a second call to get the line items for the order
// https://stripe.com/docs/api/checkout/sessions/line_items

// https://stripe.com/docs/webhooks
// https://stripe.com/docs/webhooks#verify-official-libraries
// https://github.com/stripe/stripe-go
func StripeWebhook(
	stripeWebhook *StripeWebhookArgs,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {
	if stripeWebhook.Type == "checkout.session.completed" {
		stripeSessionId := stripeWebhook.Data.Object.Id

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
			} else {
				return nil, fmt.Errorf("Stripe sku not found: %s", stripeSku)
			}
		}
	}
	// else ignore the event

	return &StripeWebhookResult{}, nil
}








/*


2024/02/06 23:45:21 subscription_handlers.go:53: Coinbase webhook body: 
{
    "attempt_number": 1,
    "event": {
        "api_version": "2018-03-22",
        "created_at": "2024-02-06T23:45:21Z",
        "data": {
            "id": "4347b2bb-0f06-4d78-a6b9-db24f717bed3",
            "code": "QDJE3YLL",
            "name": "300GiB",
            "utxo": false,
            "pricing": {
                "dai": {
                    "amount": "5.000750115000000000",
                    "currency": "DAI"
                },
                "usdc": {
                    "amount": "5.000000",
                    "currency": "USDC"
                },
                "local": {
                    "amount": "5.00",
                    "currency": "USD"
                },
                "pusdc": {
                    "amount": "5.000000",
                    "currency": "PUSDC"
                },
                "pweth": {
                    "amount": "0.002105914883132254",
                    "currency": "PWETH"
                },
                "tether": {
                    "amount": "5.001075",
                    "currency": "USDT"
                },
                "bitcoin": {
                    "amount": "0.00011598",
                    "currency": "BTC"
                },
                "polusdc": {
                    "amount": "5.000000",
                    "currency": "POLUSDC"
                },
                "polygon": {
                    "amount": "6.189268000",
                    "currency": "PMATIC"
                },
                "ethereum": {
                    "amount": "0.002106000",
                    "currency": "ETH"
                },
                "litecoin": {
                    "amount": "0.07312080",
                    "currency": "LTC"
                },
                "bitcoincash": {
                    "amount": "0.02125037",
                    "currency": "BCH"
                }
            },
            "checkout": {
                "id": "033b1385-3a39-444f-9ad0-04ff3567f706"
            },
            "fee_rate": 0.01,
            "logo_url": "https://res.cloudinary.com/commerce/image/upload/v1696659986/zudcooimudbgiae7qcbm.png",
            "metadata": {
                "name": "Brien",
                "email": "xcolwell@gmail.com"
            },
            "payments": [
                {
                    "net": {
                        "local": {
                            "amount": "5.00",
                            "currency": "USD"
                        },
                        "crypto": {
                            "amount": "5.000000",
                            "currency": "POLUSDC"
                        }
                    },
                    "block": {
                        "hash": "0x14c39283efef15822dce65ab561cda19e1cc46c3c931011071d9cccc2558f4e4",
                        "height": 53210854,
                        "confirmations": 0,
                        "confirmations_required": 128
                    },
                    "value": {
                        "local": {
                            "amount": "5.00",
                            "currency": "USD"
                        },
                        "crypto": {
                            "amount": "5.000000",
                            "currency": "POLUSDC"
                        }
                    },
                    "status": "PENDING",
                    "network": "polygon",
                    "deposited": {
                        "amount": {
                            "net": {
                                "local": null,
                                "crypto": {
                                    "amount": "5.000000",
                                    "currency": "POLUSDC"
                                }
                            },
                            "gross": {
                                "local": null,
                                "crypto": {
                                    "amount": "5.000000",
                                    "currency": "POLUSDC"
                                }
                            },
                            "coinbase_fee": {
                                "local": null,
                                "crypto": {
                                    "amount": "0.000000",
                                    "currency": "POLUSDC"
                                }
                            }
                        },
                        "status": "COMPLETED",
                        "destination": "brien@bringyour.com",
                        "exchange_rate": null,
                        "autoconversion_status": "PENDING",
                        "autoconversion_enabled": false
                    },
                    "payment_id": "1c4627c8-e596-434e-8846-9b2c7b9307e0",
                    "detected_at": "2024-02-06T23:45:21Z",
                    "transaction_id": "0x12b7ccd855a653f0f622b1afa75f3a8fdf8cc5222760eb6ae464bec0edb49b7a",
                    "payer_addresses": null,
                    "coinbase_processing_fee": {
                        "local": {
                            "amount": "0.00",
                            "currency": "USD"
                        },
                        "crypto": {
                            "amount": "0.000000",
                            "currency": "POLUSDC"
                        }
                    }
                }
            ],
            "resource": "charge",
            "timeline": [
                {
                    "time": "2024-02-06T23:43:01Z",
                    "status": "NEW"
                },
                {
                    "time": "2024-02-06T23:45:21Z",
                    "status": "PENDING",
                    "payment": {
                        "value": {
                            "amount": "5.000000",
                            "currency": "POLUSDC"
                        },
                        "network": "polusdc",
                        "transaction_id": "0x12b7ccd855a653f0f622b1afa75f3a8fdf8cc5222760eb6ae464bec0edb49b7a"
                    }
                }
            ],
            "addresses": {
                "dai": "0x823fdae3219277292c8d4ca2fd128860a443e8ed",
                "usdc": "0x823fdae3219277292c8d4ca2fd128860a443e8ed",
                "pusdc": "0x823fdae3219277292c8d4ca2fd128860a443e8ed",
                "pweth": "0x823fdae3219277292c8d4ca2fd128860a443e8ed",
                "tether": "0x823fdae3219277292c8d4ca2fd128860a443e8ed",
                "bitcoin": "3FMDRioVC8VGMQPVVkEKSki4u8oU7KVa7y",
                "polusdc": "0x823fdae3219277292c8d4ca2fd128860a443e8ed",
                "polygon": "0x823fdae3219277292c8d4ca2fd128860a443e8ed",
                "ethereum": "0x823fdae3219277292c8d4ca2fd128860a443e8ed",
                "litecoin": "M95JhAnnTzhmdXEuZW36hXQKmN4XHrnkAw",
                "bitcoincash": "qq6gw8yspv6lzcmghlzduaayks437jfd2y2s2h4zuz"
            },
            "pwcb_only": false,
            "created_at": "2024-02-06T23:43:01Z",
            "expires_at": "2024-02-07T00:43:01Z",
            "hosted_url": "https://commerce.coinbase.com/charges/QDJE3YLL",
            "brand_color": "#1D3150",
            "description": "300GiB Global Data Pack",
            "fees_settled": true,
            "pricing_type": "fixed_price",
            "support_email": "support@bringyour.com",
            "brand_logo_url": "https://res.cloudinary.com/commerce/image/upload/v1696663323/ibjdpinxui4w5588159t.png",
            "exchange_rates": {
                "BCH-USD": "235.29",
                "BTC-USD": "43112.455",
                "DAI-USD": "0.99985",
                "ETH-USD": "2373.97",
                "LTC-USD": "68.38",
                "USDC-USD": "1.0",
                "USDT-USD": "0.999785",
                "PUSDC-USD": "1.0",
                "PWETH-USD": "2374.265",
                "PMATIC-USD": "0.80785",
                "POLUSDC-USD": "1.0"
            },
            "collected_email": true,
            "offchain_eligible": false,
            "organization_name": "BringYour",
            "payment_threshold": {
                "overpayment_absolute_threshold": {
                    "amount": "0.25",
                    "currency": "USD"
                },
                "overpayment_relative_threshold": "0.005",
                "underpayment_absolute_threshold": {
                    "amount": "0.25",
                    "currency": "USD"
                },
                "underpayment_relative_threshold": "0.005"
            },
            "local_exchange_rates": {
                "BCH-USD": "235.29",
                "BTC-USD": "43112.455",
                "DAI-USD": "0.99985",
                "ETH-USD": "2373.97",
                "LTC-USD": "68.38",
                "USDC-USD": "1.0",
                "USDT-USD": "0.999785",
                "PUSDC-USD": "1.0",
                "PWETH-USD": "2374.265",
                "PMATIC-USD": "0.80785",
                "POLUSDC-USD": "1.0"
            },
            "coinbase_managed_merchant": false
        },
        "id": "e878e829-df04-4871-b539-675e086b9444",
        "resource": "event",
        "type": "charge:pending"
    },
    "id": "066940c8-9a5b-4262-a1be-fdf50cafd611",
    "scheduled_for": "2024-02-06T23:45:21Z"
}


*/


type CoinbaseWebhookArgs struct {
	Event *CoinbaseEvent
}


type CoinbaseEvent struct {
	Id string
	Type string
	Data *CoinbaseEventData
}

type CoinbaseEventData struct {
	Id string
	Name string
	Description string
	Payments []*CoinbaseEventDataPayment
	Checkout *CoinbaseEventDataCheckout
	Metadata *CoinbaseEventDataMetadata
}

type CoinbaseEventDataCheckout struct {
	Id string
}

type CoinbaseEventDataMetadata struct {
	Email string
}


type CoinbaseEventDataPayment struct {
	Net *CoinbaseEventDataPaymentNet
}

type CoinbaseEventDataPaymentNet struct {
	Local *CoinbaseEventDataPaymentAmount
	Crypto *CoinbaseEventDataPaymentAmount
}

type CoinbaseEventDataPaymentAmount struct {
	Amount string
	Current string
}



type CoinbaseWebhookResult struct {

}

// https://api.bringyour.com/pay/coinbase
// https://docs.cloud.coinbase.com/commerce/docs/webhooks#subscribing-to-a-webhook
// The signature is included as a X-CC-Webhook-Signature header. This header contains the SHA256 HMAC signature of the raw request payload, computed using your webhook shared secret as the key.
func CoinbaseWebhook(
	coinbaseWebhook *CoinbaseWebhookArgs,
	clientSession *session.ClientSession,
) (*CoinbaseWebhookResult, error) {

	if coinbaseWebhook.Event.Type == "charge:pending" {
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
        purchaseEmail,
        &SubscriptionTransferBalanceCodeTemplate{
        	Secret: balanceCode.Secret,
        },
    )
}




/*
"message": {
        "data": "eyJ2ZXJzaW9uIjoiMS4wIiwicGFja2FnZU5hbWUiOiJjb20uYnJpbmd5b3VyLm5ldHdvcmsiLCJldmVudFRpbWVNaWxsaXMiOiIxNzA3NzY4NjY2MTQwIiwidGVzdE5vdGlmaWNhdGlvbiI6eyJ2ZXJzaW9uIjoiMS4wIn19",
        "messageId": "10457656034958578",
        "message_id": "10457656034958578",
        "publishTime": "2024-02-12T20:11:06.166Z",
        "publish_time": "2024-02-12T20:11:06.166Z"
    },
    "subscription": "projects/bringyour/subscriptions/webhook"
*/
// data is base64 encoded json
// 



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


    /*
    {
  "startTimeMillis": "1707348607902",
  "expiryTimeMillis": "1709854187177",
  "autoRenewing": true,
  "priceCurrencyCode": "USD",
  "priceAmountMicros": "5990000",
  "countryCode": "US",
  "developerPayload": "",
  "paymentState": 1,
  "orderId": "GPA.3341-2551-4829-22419",
  "acknowledgementState": 1,
  "kind": "androidpublisher#subscriptionPurchase"
}
*/

    /*
    {
  "countryCode":"CZ",
  "developerPayload":"",
  "kind":"androidpublisher#subscriptionPurchase",
  "orderId":"GPA.3399-3829-9285-87596..0",
  "priceCurrencyCode":"CZK",
  "autoRenewing":true,
  "expiryTimeMillis":1584703770967, // "2020-03-20T11:29:30.967Z"
  "startTimeMillis":1584702935111, // "2020-03-20T11:15:35.111Z"
  "priceAmountMicros":159990000,
  "paymentState":1,
  "purchaseType":0
}
*/
// https://stackoverflow.com/questions/60850840/late-subscription-renewal-real-time-developer-notification-with-google-play-bill

type PlaySubscription struct {
	StartTimeMillis int64 `json:"startTimeMillis"`
	ExpiryTimeMillis int64 `json:"expiryTimeMillis"`
	AutoRenewing bool `json:"autoRenewing"`
	PriceCurrencyCode string `json:"priceCurrencyCode"`
	PriceAmountMicros string `json:"priceAmountMicros"`
	CountryCode string `json:"countryCode"`
	DeveloperPayload string `json:"developerPayload"`
	PaymentState int `json:"paymentState"`
	OrderId string `json:"orderId"`
	AcknowledgementState int `json:"acknowledgementState"`
	Kind int `json:"kind"`

	// FIXME How is this actually passed?
	ObfuscatedAccountId string
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
				bringyour.NoCustomHeaders,
				bringyour.ResponseJsonObject[*PlaySubscription],
			)
			if err != nil {
				return nil, err
			}

		    // FIXME ANDROID APP setObfuscatedAccountId use the network name
			// the obfuscated account id should be a subscription payment id
			subscriptionPaymentId, err := bringyour.ParseId(sub.ObfuscatedAccountId)

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
					bringyour.NoCustomHeaders,
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
						ExpiryTime: time.UnixMilli(sub.ExpiryTimeMillis),
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
	NetworkId bringyour.Id
	PackageName string
	SubscriptionId string
	PurchaseToken string
	CheckTime time.Time
	ExpiryTime time.Time
}

type PlaySubscriptionRenewalResult struct {
	ExpiryTime time.Time
	Renewed bool
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
		bringyour.NoCustomHeaders,
		bringyour.ResponseJsonObject[*PlaySubscription],
	)
	if err != nil {
		return nil, err
	}

	expiryTime := time.UnixMilli(sub.ExpiryTimeMillis)
	startTime := time.UnixMilli(sub.StartTimeMillis)

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
		playSubscriptionRenewal.CheckTime = playSubscriptionRenewal.ExpiryTime
		SchedulePlaySubscriptionRenewal(
			clientSession,
			tx,
			playSubscriptionRenewal,
		)
	} else if time.Now().Before(playSubscriptionRenewalResult.ExpiryTime.Add(SubscriptionGracePeriod)) {
		// check again in 30 minutes
		playSubscriptionRenewal.CheckTime = time.Now().Add(30 * time.Minute)
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

	_, err = stripewebhook.ConstructEvent(bodyBytes, req.Header.Get("Stripe-Signature"), stripeWebhookSigningSecret())
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

	err = coinbaseSignature(bodyBytes, req.Header.Get("X-CC-Webhook-Signature"), coinbaseWebhookSigningSecret())
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
	err := playAuth(req.Header.Get("Authorization"))
	if err != nil {
		return nil, err
	}

	return req.Body, nil
}

func playAuth(auth string) error {
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

