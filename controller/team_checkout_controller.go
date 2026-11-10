package controller

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/stripe/stripe-go/v82"
	stripecheckout "github.com/stripe/stripe-go/v82/checkout/session"
	stripeprice "github.com/stripe/stripe-go/v82/price"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// POST /network/team-checkout: a team network buys the team tier (QUOTA-M6-02; DESIGN.md 20.5's
// Becoming Team, 20.9's Post-paid and its anchor row; R-QUOTA-1, R-QUOTA-6, D-QUOTA-8).
//
// One Stripe subscription per Team network, with one item per price:
//
//	base              flat, a month, team.price_usd.base, which covers included_seats
//	seats             metered on team_seats (last), graduated: nothing up to included_seats, then
//	                  team.price_usd.seat a seat
//	transfer overage  metered (sum), a GiB at overage_usd.transfer_tib / 1024; no item while
//	                  pro.yml holds no price, so that overage is withheld (D-QUOTA-2)
//	storage overage   metered (sum), a GiB-day at 12/365 of overage_usd.storage_gib_month; no item
//	                  until the owner prices it (owner decision 122)
//
// Stripe holds only the price ids, in config/<env>/stripe.yml's team_prices, never vault; the
// amounts are pro.yml's, and checkout reads each price back from Stripe and refuses one whose
// amount differs. An env with no team_prices refuses checkout and boots.
//
// The session expires an hour after it is made and anchors the billing cycle at the first
// 00:00:00 UTC after that (the pinned SDK's Checkout takes a timestamp, not
// billing_cycle_anchor_config), so every period after the first starts at 00:00:00 UTC and no UTC
// day spans two periods. The first period runs from the payment to the anchor. The subscription
// carries the network and the team mark in its metadata: a renewal invoice names no checkout
// session, so without the network a renewal resolves to no network, and without the mark the
// invoice would be credited as Pro's (team_stripe_events.go).
//
// Checkout opens only once this process has read the escrow_draw_order gate on for its reread
// (model.EscrowDrawOrderOpenForCredit), since the first invoice writes a team_credit line that
// every draw site must already order last (D-QUOTA-9). Invoicing above 25 seats is Later: such a
// team is granted by hand with market manual, and no route sells it.

// The price ids of the Team subscription's items, config/<env>/stripe.yml's team_prices:
//
//	team_prices:
//	  base: price_...
//	  seats: price_...
//	  transfer_overage: price_...
//	  storage_overage: price_...    # once team.overage_usd.storage_gib_month is priced
type StripeTeamPrices struct {
	Base            string `yaml:"base"`
	Seats           string `yaml:"seats"`
	TransferOverage string `yaml:"transfer_overage"`
	StorageOverage  string `yaml:"storage_overage"`
}

// Optional, as stripeSubscriptionPrices is: an env with no config stripe.yml, or one with no
// team_prices, has no prices, and checkout refuses.
var stripeTeamPrices = sync.OnceValue(func() StripeTeamPrices {
	var c struct {
		TeamPrices StripeTeamPrices `yaml:"team_prices"`
	}
	if resource, err := server.Config.SimpleResource("stripe.yml"); err == nil {
		resource.UnmarshalYaml(&c)
	}
	return c.TeamPrices
})

// The mark of a Team subscription in its Stripe metadata, beside network_id.
const stripeMetadataTeam = "team"
const stripeTeamMark = "1"

// Whether the metadata is a Team subscription's.
func stripeTeamMarked(metadata map[string]string) bool {
	return metadata[stripeMetadataTeam] == stripeTeamMark
}

// The items of a Team subscription.
type stripeTeamItem string

const (
	stripeTeamItemBase            stripeTeamItem = "base"
	stripeTeamItemSeats           stripeTeamItem = "seats"
	stripeTeamItemTransferOverage stripeTeamItem = "transfer_overage"
	stripeTeamItemStorageOverage  stripeTeamItem = "storage_overage"
)

// Tunables of Team checkout.
type TeamCheckoutSettings struct {
	// How long a Checkout session stays open (expires_at). The anchor is the first 00:00:00 UTC
	// after it, so the subscription always starts before its anchor. Stripe takes 30 minutes to
	// 24 hours.
	SessionTimeout time.Duration
	// Bounds the Stripe reads and the session's creation.
	StripeTimeout time.Duration
}

func DefaultTeamCheckoutSettings() *TeamCheckoutSettings {
	return &TeamCheckoutSettings{
		SessionTimeout: 1 * time.Hour,
		StripeTimeout:  15 * time.Second,
	}
}

// The settings and the clock Team checkout runs with. Replaceable only by tests.
var teamCheckoutSettingsFunc = DefaultTeamCheckoutSettings
var teamCheckoutNow = server.NowUtc

type TeamCheckoutArgs struct {
	// "hosted" (default) or "embedded", as POST /stripe/create-checkout-session takes them
	UiMode string `json:"ui_mode,omitempty"`
	// "never" keeps an embedded checkout inline
	RedirectOnCompletion string `json:"redirect_on_completion,omitempty"`
}

// Why team-checkout refused, beside the PurchaseErrorCode* it shares with Pro's checkout.
const (
	// the network is not one POST /network/team-create made
	TeamCheckoutErrorCodeNotTeam = "not_team"
)

type TeamCheckoutError struct {
	// TeamCheckoutErrorCodeNotTeam or a PurchaseErrorCode*
	Code    string `json:"code"`
	Message string `json:"message"`
}

type TeamCheckoutResult struct {
	// which of the two shapes below is set
	UiMode string `json:"ui_mode,omitempty"`
	// hosted: Stripe's checkout url
	CheckoutUrl string `json:"checkout_url,omitempty"`
	// embedded: what Stripe.js mounts Embedded Checkout with
	ClientSecret   string `json:"client_secret,omitempty"`
	PublishableKey string `json:"publishable_key,omitempty"`
	SessionId      string `json:"session_id,omitempty"`

	Error *TeamCheckoutError `json:"error,omitempty"`
}

func teamCheckoutRefusal(code string, message string) (*TeamCheckoutResult, error) {
	return &TeamCheckoutResult{
		Error: &TeamCheckoutError{Code: code, Message: message},
	}, nil
}

const teamCheckoutUnavailableMessage = "Teams are not available."
const teamCheckoutClientMessage = "A client credential cannot buy a team plan."
const teamCheckoutNotOpenMessage = "Team checkout is not open yet."
const teamCheckoutNotConfiguredMessage = "The team plan is not available."
const teamCheckoutAlreadySubscribedMessage = "This team already has an active subscription, so no new payment was made."

// The first 00:00:00 UTC after expireTime, never expireTime itself: a session paid in its last
// second still starts before its anchor.
func teamCheckoutAnchorTime(expireTime time.Time) time.Time {
	expireTime = expireTime.UTC()
	return time.Date(expireTime.Year(), expireTime.Month(), expireTime.Day()+1, 0, 0, 0, 0, time.UTC)
}

// A Team subscription's item: its price id and what pro.yml says the price is.
type stripeTeamCheckoutItem struct {
	item    stripeTeamItem
	priceId string
}

// The items a Team subscription is sold with now, or false when pro.yml or stripe.yml cannot
// sell one: no base or seat price, or a priced item with no price id. An overage pro.yml does not
// price has no item, so it is withheld, never sold at $0.
func stripeTeamCheckoutItems(prices StripeTeamPrices, config *model.ProTeamConfig) ([]*stripeTeamCheckoutItem, bool) {
	if config.PriceBaseUsd <= 0 || config.PriceSeatUsd <= 0 {
		return nil, false
	}
	if prices.Base == "" || prices.Seats == "" {
		return nil, false
	}
	items := []*stripeTeamCheckoutItem{
		{item: stripeTeamItemBase, priceId: prices.Base},
		{item: stripeTeamItemSeats, priceId: prices.Seats},
	}
	if _, ok := config.OverageTransferTibUsd(); ok {
		if prices.TransferOverage == "" {
			return nil, false
		}
		items = append(items, &stripeTeamCheckoutItem{item: stripeTeamItemTransferOverage, priceId: prices.TransferOverage})
	}
	if _, ok := config.OverageStorageGibMonthUsd(); ok {
		if prices.StorageOverage == "" {
			return nil, false
		}
		items = append(items, &stripeTeamCheckoutItem{item: stripeTeamItemStorageOverage, priceId: prices.StorageOverage})
	}
	return items, true
}

// Whether a Stripe amount in cents, to 12 decimal places, is the amount pro.yml gives.
func stripeTeamAmountEqual(stripeCents float64, configCents float64) bool {
	return math.Abs(stripeCents-configCents) <= 1e-11+1e-9*math.Abs(configCents)
}

// Why the Stripe price is not the item pro.yml sells, or "" when it is: a live monthly USD
// price of the item's shape at pro.yml's amount.
func stripeTeamPriceMismatch(item stripeTeamItem, price *stripe.Price, config *model.ProTeamConfig) string {
	if price == nil {
		return "no price"
	}
	if !price.Active {
		return "the price is archived"
	}
	if price.Currency != stripe.CurrencyUSD {
		return fmt.Sprintf("the currency is %s", price.Currency)
	}
	if price.Recurring == nil || price.Recurring.Interval != stripe.PriceRecurringIntervalMonth || price.Recurring.IntervalCount != 1 {
		return "the price does not recur monthly"
	}
	if price.TransformQuantity != nil {
		return "the price transforms its quantity"
	}
	metered := price.Recurring.UsageType == stripe.PriceRecurringUsageTypeMetered
	perUnit := func(configCents float64) string {
		if price.BillingScheme != stripe.PriceBillingSchemePerUnit {
			return "the price is not per unit"
		}
		if !stripeTeamAmountEqual(price.UnitAmountDecimal, configCents) {
			return fmt.Sprintf("the amount is %v cents, pro.yml's is %v", price.UnitAmountDecimal, configCents)
		}
		return ""
	}
	switch item {
	case stripeTeamItemBase:
		if metered {
			return "the base price is metered"
		}
		return perUnit(config.PriceBaseUsd * 100)
	case stripeTeamItemSeats:
		if !metered || price.Recurring.Meter == "" {
			return "the seat price is not metered"
		}
		seatCents := config.PriceSeatUsd * 100
		if config.IncludedSeats <= 0 {
			return perUnit(seatCents)
		}
		if price.BillingScheme != stripe.PriceBillingSchemeTiered || price.TiersMode != stripe.PriceTiersModeGraduated {
			return "the seat price is not graduated"
		}
		if len(price.Tiers) != 2 || price.Tiers[0] == nil || price.Tiers[1] == nil {
			return "the seat price does not have two tiers"
		}
		included := price.Tiers[0]
		above := price.Tiers[1]
		if included.UpTo != config.IncludedSeats {
			return fmt.Sprintf("the first tier ends at %d seats, pro.yml includes %d", included.UpTo, config.IncludedSeats)
		}
		if included.UnitAmountDecimal != 0 || included.FlatAmountDecimal != 0 || included.UnitAmount != 0 || included.FlatAmount != 0 {
			return "the included seats are not free"
		}
		// the last tier has no end, which reads zero
		if above.UpTo != 0 || above.FlatAmountDecimal != 0 || above.FlatAmount != 0 {
			return "the second tier is not open-ended per seat"
		}
		if !stripeTeamAmountEqual(above.UnitAmountDecimal, seatCents) {
			return fmt.Sprintf("a seat is %v cents, pro.yml's is %v", above.UnitAmountDecimal, seatCents)
		}
		return ""
	case stripeTeamItemTransferOverage:
		priceUsd, ok := config.OverageTransferTibUsd()
		if !ok {
			return "pro.yml does not price transfer overage"
		}
		if !metered || price.Recurring.Meter == "" {
			return "the transfer overage price is not metered"
		}
		// a GiB, the meter's unit
		return perUnit(priceUsd * 100 * float64(model.Gib) / float64(model.Tib))
	case stripeTeamItemStorageOverage:
		priceUsd, ok := config.OverageStorageGibMonthUsd()
		if !ok {
			return "pro.yml does not price storage overage"
		}
		if !metered || price.Recurring.Meter == "" {
			return "the storage overage price is not metered"
		}
		// a GiB-day, at 12/365 of the GiB-month, to Stripe's 12 decimal places
		return perUnit(math.Round(priceUsd*100*12/365*1e12) / 1e12)
	}
	return "an unknown item"
}

// Reads each item's price from Stripe and returns why the first that differs from pro.yml does,
// "" when none does. An error means Stripe could not be read.
func stripeTeamVerifyPrices(ctx context.Context, items []*stripeTeamCheckoutItem, config *model.ProTeamConfig) (string, error) {
	for _, item := range items {
		params := &stripe.PriceParams{}
		params.Context = ctx
		params.AddExpand("tiers")
		price, err := stripeprice.Get(item.priceId, params)
		if err != nil {
			return "", fmt.Errorf("price %s: %w", item.priceId, err)
		}
		if mismatch := stripeTeamPriceMismatch(item.item, price, config); mismatch != "" {
			return fmt.Sprintf("%s price %s: %s", item.item, item.priceId, mismatch), nil
		}
	}
	return "", nil
}

// Whether Stripe still bills the subscription behind the network's last Team renewal. False
// when it never had one. An error means Stripe could not say, and checkout refuses: a second
// subscription would bill the team twice.
func stripeTeamSubscriptionBilling(ctx context.Context, networkId server.Id) (bool, error) {
	renewal := model.GetLastSubscriptionRenewal(ctx, networkId, model.SubscriptionTypeTeam, model.SubscriptionMarketStripe)
	if renewal == nil || renewal.TransactionId == "" {
		return false, nil
	}
	sub, err := stripeSubscriptionFromInvoice(ctx, renewal.TransactionId)
	if err != nil {
		return false, err
	}
	return stripeSubscriptionBilling(sub.Status), nil
}

// POST /network/team-checkout: a Stripe Checkout session for the caller's team network, the
// Owner's or a Billing admin's by the role gate (api/route_permission.go). Each refusal is a 200
// with `error`; 503 while the binary does not link MG6.
func TeamCheckout(
	args *TeamCheckoutArgs,
	clientSession *session.ClientSession,
) (*TeamCheckoutResult, error) {
	if !server.SidecarMg6Linked() {
		return nil, fmt.Errorf("%d %s", http.StatusServiceUnavailable, teamCheckoutUnavailableMessage)
	}
	byJwt := clientSession.ByJwt
	// the router refuses client tokens on this Network route; this holds without it
	if byJwt == nil || byJwt.ClientId != nil {
		return nil, fmt.Errorf("%d %s", http.StatusForbidden, teamCheckoutClientMessage)
	}
	networkId := byJwt.NetworkId
	settings := teamCheckoutSettingsFunc()

	uiMode, ok := stripeCheckoutUiMode(args.UiMode)
	if !ok {
		return teamCheckoutRefusal(PurchaseErrorCodeInvalidRequest, "Unknown ui mode.")
	}
	redirectNever, redirectOk := stripeCheckoutRedirectNever(uiMode, args.RedirectOnCompletion)
	if !redirectOk {
		return teamCheckoutRefusal(
			PurchaseErrorCodeInvalidRequest,
			"redirect_on_completion \"never\" requires ui_mode \"embedded\".",
		)
	}

	if model.GetNetworkTeam(clientSession.Ctx, networkId) == nil {
		return teamCheckoutRefusal(TeamCheckoutErrorCodeNotTeam, "Create a team before buying a team plan.")
	}

	// the first invoice writes a team_credit line, which every draw site must already order last
	if !model.EscrowDrawOrderOpenForCredit() {
		glog.Infof("[stripe][team]checkout for network %s refused: the escrow_draw_order gate is not open\n", networkId)
		return teamCheckoutRefusal(PurchaseErrorCodeCheckoutUnavailable, teamCheckoutNotOpenMessage)
	}

	config := &model.Pro().Team
	items, ok := stripeTeamCheckoutItems(stripeTeamPrices(), config)
	if !ok {
		glog.Errorf("[stripe][team]no team prices configured\n")
		return teamCheckoutRefusal(PurchaseErrorCodePlanUnavailable, teamCheckoutNotConfiguredMessage)
	}

	params := &stripe.CheckoutSessionParams{
		// names the network to the first invoice, beside the subscription's metadata
		ClientReferenceID: stripe.String(networkId.String()),
	}
	// before any vault read, so an env with no Stripe config refuses
	if !stripeCheckoutApplyUiMode(params, uiMode, redirectNever, stripeCheckoutUrls()) {
		glog.Errorf("[stripe][team]checkout urls are not configured for ui mode %s\n", uiMode)
		return teamCheckoutRefusal(PurchaseErrorCodeCheckoutUnavailable, "Checkout is not configured.")
	}

	stripe.Key = stripeApiToken()

	// a network already paying for Pro keeps Pro's refusal, and one Stripe still bills for
	// Team is never sold a second subscription
	customerId := ""
	if existing, _ := model.GetStripeCustomer(clientSession); existing != nil {
		customerId = *existing
	}
	if stripeNetworkHasActiveSubscription(clientSession, customerId) {
		glog.Infof("[stripe][team]checkout: network %s already has an active subscription\n", networkId)
		return teamCheckoutRefusal(PurchaseErrorCodeAlreadySubscribed, stripeAlreadySubscribedMessage)
	}
	stripeCtx, cancel := context.WithTimeout(clientSession.Ctx, settings.StripeTimeout)
	defer cancel()
	billing, err := stripeTeamSubscriptionBilling(stripeCtx, networkId)
	if err != nil {
		glog.Errorf("[stripe][team]checkout: could not read the subscription of network %s: %s\n", networkId, err)
		return teamCheckoutRefusal(PurchaseErrorCodeStartFailed, "Could not start checkout. Please try again.")
	}
	if billing {
		glog.Infof("[stripe][team]checkout: network %s already has a team subscription\n", networkId)
		return teamCheckoutRefusal(PurchaseErrorCodeAlreadySubscribed, teamCheckoutAlreadySubscribedMessage)
	}

	mismatch, err := stripeTeamVerifyPrices(stripeCtx, items, config)
	if err != nil {
		glog.Errorf("[stripe][team]checkout: could not read the team prices: %s\n", err)
		return teamCheckoutRefusal(PurchaseErrorCodeStartFailed, "Could not start checkout. Please try again.")
	}
	if mismatch != "" {
		// never sell at an amount pro.yml does not name
		glog.Errorf("[stripe][team]checkout refused: %s\n", mismatch)
		return teamCheckoutRefusal(PurchaseErrorCodePlanUnavailable, teamCheckoutNotConfiguredMessage)
	}

	expireTime := teamCheckoutNow().UTC().Add(settings.SessionTimeout)
	anchorTime := teamCheckoutAnchorTime(expireTime)

	params.Context = stripeCtx
	params.Mode = stripe.String(string(stripe.CheckoutSessionModeSubscription))
	params.ExpiresAt = stripe.Int64(expireTime.Unix())
	if customerId != "" {
		params.Customer = stripe.String(customerId)
	}
	for _, item := range items {
		lineItem := &stripe.CheckoutSessionLineItemParams{
			Price: stripe.String(item.priceId),
		}
		if item.item == stripeTeamItemBase {
			// a metered item takes no quantity: its meter gives it
			lineItem.Quantity = stripe.Int64(1)
		}
		params.LineItems = append(params.LineItems, lineItem)
	}
	params.SubscriptionData = &stripe.CheckoutSessionSubscriptionDataParams{
		BillingCycleAnchor: stripe.Int64(anchorTime.Unix()),
		Metadata: map[string]string{
			stripeMetadataNetworkId: networkId.String(),
			stripeMetadataTeam:      stripeTeamMark,
		},
	}

	checkoutSession, err := stripecheckout.New(params)
	if err != nil {
		glog.Errorf("[stripe][team]could not create a checkout session for network %s: %s\n", networkId, err)
		return teamCheckoutRefusal(PurchaseErrorCodeStartFailed, "Could not start checkout. Please try again.")
	}
	glog.Infof(
		"[stripe][team]checkout session %s created for network %s (%s), anchored at %s\n",
		checkoutSession.ID, networkId, uiMode, anchorTime.Format(time.RFC3339),
	)

	result := &TeamCheckoutResult{
		UiMode:    uiMode,
		SessionId: checkoutSession.ID,
	}
	switch uiMode {
	case StripeUiModeEmbedded:
		result.ClientSecret = checkoutSession.ClientSecret
		result.PublishableKey = stripePublishableKey()
	case StripeUiModeHosted:
		result.CheckoutUrl = checkoutSession.URL
	}
	return result, nil
}
