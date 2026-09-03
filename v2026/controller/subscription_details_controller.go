package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The "Manage subscription" screen.
//
// GET /subscription/balance lists the stores billing a network as bare
// {store, plan} pairs (its wire shape is frozen: shipped native apps parse it),
// and the web used to send every "Manage" tap to the Stripe billing portal,
// which answers "No stripe customer found" for a network billed by the App
// Store, Google Play or Solana. This file is the smart version: one entry per
// store with the window it bought, whether the store will renew it, and the
// control that stops it -- a server-side cancel for Stripe, the store's own
// subscriptions page for Apple/Google, nothing for a one-time USDC payment.
//
// Every store lookup is best-effort: a store API that is down or slow yields
// auto_renew null for that entry, never an error result. The enriched result
// is cached per network for a minute so reopening the screen does not hammer
// Stripe/Play/Apple; cancel and resume invalidate it.

const subscriptionDetailsCacheTtl = 60 * time.Second

// per-store lookup budget; the screen waits on the slowest store
const subscriptionStoreLookupTimeout = 5 * time.Second

const appleManageSubscriptionsUrl = "https://apps.apple.com/account/subscriptions"
const googleManageSubscriptionsUrl = "https://play.google.com/store/account/subscriptions"

const SubscriptionCadenceYearly = "yearly"
const SubscriptionCadenceMonthly = "monthly"

type SubscriptionDetail struct {
	// the market: stripe | apple | google | solana | manual | x402 | "" (unknown)
	Store string `json:"store"`
	// the wire plan value ("supporter"); the ui names it Pro
	Plan string `json:"plan"`
	// yearly | monthly | "" when the store did not say and the window is ambiguous
	Cadence string `json:"cadence"`
	// the active renewal window; EndTime is the expiry, or the next renewal
	// date when the store renews
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	// nil = unknown (the store lookup failed)
	AutoRenew *bool `json:"auto_renew"`
	// the store will let the current period run out and not bill again
	CancelAtPeriodEnd bool `json:"cancel_at_period_end"`
	// POST /subscription/cancel works for this entry (Stripe only)
	CanCancel bool `json:"can_cancel"`
	// where the customer manages it when we cannot (App Store / Google Play)
	ManageUrl string `json:"manage_url"`
	// the store handle, for support: invoice id (stripe), original transaction
	// id (apple), purchase token (google), payment reference (solana)
	TransactionId string `json:"transaction_id,omitempty"`
}

type SubscriptionDetailsResult struct {
	Subscriptions []*SubscriptionDetail `json:"subscriptions"`
	// gate for the Stripe billing portal link: the portal errors without one
	HasStripeCustomer bool      `json:"has_stripe_customer"`
	UpdateTime        time.Time `json:"update_time"`
}

// subscriptionStoreState is what one store says about its subscription.
type subscriptionStoreState struct {
	AutoRenew         *bool
	CancelAtPeriodEnd bool
	// the store's expiry / next renewal, when it knows better than our row
	EndTime *time.Time
	Cadence string
	// the store's subscription object id (Stripe sub_...), for cancel/resume
	StoreSubscriptionId string
	// the store still bills this (Stripe status active/trialing/past_due)
	Active bool
}

// subscriptionStoreLookupSet are the per-store questions. Replaceable by tests;
// the defaults call the store APIs through the same base-url/token seams the
// webhooks and the payment reconciler use.
type subscriptionStoreLookupSet struct {
	// customerId may be empty (no stripe_customer row: an older checkout);
	// invoiceId is the renewal's transaction id and resolves the subscription
	// through the invoice instead
	stripe func(ctx context.Context, customerId string, invoiceId string) (*subscriptionStoreState, error)
	apple  func(ctx context.Context, originalTransactionId string) (*subscriptionStoreState, error)
	google func(ctx context.Context, purchaseToken string) (*subscriptionStoreState, error)
	solana func(ctx context.Context, reference string) (*subscriptionStoreState, error)
}

var subscriptionStoreLookups = subscriptionStoreLookupSet{
	stripe: stripeLookupSubscriptionState,
	apple:  appleLookupSubscriptionState,
	google: playLookupSubscriptionState,
	solana: solanaLookupSubscriptionState,
}

func subscriptionDetailsCacheKey(networkId server.Id) string {
	return fmt.Sprintf("subscription_details:%s", networkId)
}

func getSubscriptionDetailsCached(ctx context.Context, networkId server.Id) *SubscriptionDetailsResult {
	var result *SubscriptionDetailsResult
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, subscriptionDetailsCacheKey(networkId)).Bytes()
		if err != nil {
			// a miss or a cache error: rebuild
			return
		}
		cached := &SubscriptionDetailsResult{}
		if err := json.Unmarshal(value, cached); err != nil {
			return
		}
		result = cached
	})
	return result
}

func setSubscriptionDetailsCached(ctx context.Context, networkId server.Id, result *SubscriptionDetailsResult) {
	value, err := json.Marshal(result)
	if err != nil {
		return
	}
	server.Redis(ctx, func(r server.RedisClient) {
		r.Set(ctx, subscriptionDetailsCacheKey(networkId), value, subscriptionDetailsCacheTtl)
	})
}

func clearSubscriptionDetailsCached(ctx context.Context, networkId server.Id) {
	server.Redis(ctx, func(r server.RedisClient) {
		r.Del(ctx, subscriptionDetailsCacheKey(networkId))
	})
}

// SubscriptionDetails answers GET /subscription/details for the caller's network.
func SubscriptionDetails(clientSession *session.ClientSession) (*SubscriptionDetailsResult, error) {
	networkId := clientSession.ByJwt.NetworkId
	if cached := getSubscriptionDetailsCached(clientSession.Ctx, networkId); cached != nil {
		return cached, nil
	}

	stripeCustomerId := ""
	if customerId, err := model.GetStripeCustomer(clientSession); err == nil && customerId != nil {
		stripeCustomerId = *customerId
	}
	renewals := model.GetActiveSubscriptionRenewals(
		clientSession.Ctx,
		networkId,
		model.SubscriptionTypeSupporter,
	)
	result := buildSubscriptionDetails(clientSession.Ctx, renewals, stripeCustomerId, &subscriptionStoreLookups)
	setSubscriptionDetailsCached(clientSession.Ctx, networkId, result)
	return result, nil
}

// buildSubscriptionDetails collapses the active renewal rows to one entry per
// store (the window that ends LAST is the one the customer is paid through)
// and asks each store about it. Pure apart from the lookups, so it is what
// the hermetic tests exercise.
func buildSubscriptionDetails(
	ctx context.Context,
	renewals []*model.ActiveSubscriptionRenewal,
	stripeCustomerId string,
	lookups *subscriptionStoreLookupSet,
) *SubscriptionDetailsResult {
	byStore := map[string]*model.ActiveSubscriptionRenewal{}
	for _, renewal := range renewals {
		market := strings.ToLower(strings.TrimSpace(renewal.Market))
		if current, ok := byStore[market]; !ok || current.EndTime.Before(renewal.EndTime) {
			byStore[market] = renewal
		}
	}
	stores := make([]string, 0, len(byStore))
	for store := range byStore {
		stores = append(stores, store)
	}
	sort.Strings(stores)

	details := []*SubscriptionDetail{}
	for _, store := range stores {
		renewal := byStore[store]
		detail := &SubscriptionDetail{
			Store:     store,
			Plan:      model.SubscriptionTypeSupporter,
			StartTime: renewal.StartTime,
			EndTime:   renewal.EndTime,
		}
		switch store {
		case model.SubscriptionMarketStripe:
			detail.CanCancel = true
			detail.TransactionId = renewal.TransactionId
		case model.SubscriptionMarketApple:
			detail.ManageUrl = appleManageSubscriptionsUrl
			detail.TransactionId = renewal.TransactionId
		case model.SubscriptionMarketGoogle:
			detail.ManageUrl = googleManageSubscriptionsUrl
			detail.TransactionId = renewal.PurchaseToken
		case model.SubscriptionMarketSolana:
			detail.TransactionId = renewal.TransactionId
		}

		state, err := lookupSubscriptionStoreState(ctx, store, renewal, stripeCustomerId, lookups)
		if err != nil {
			glog.Infof("[sub]details: %s lookup for %s failed: %v\n", store, renewal.TransactionId, err)
		}
		applySubscriptionStoreState(detail, state)
		details = append(details, detail)
	}

	return &SubscriptionDetailsResult{
		Subscriptions:     details,
		HasStripeCustomer: stripeCustomerId != "",
		UpdateTime:        server.NowUtc(),
	}
}

// lookupSubscriptionStoreState asks the store, within the lookup budget. A
// store we cannot ask (manual grants, x402, unknown) renews nothing: that is a
// known false, not an unknown.
func lookupSubscriptionStoreState(
	ctx context.Context,
	store string,
	renewal *model.ActiveSubscriptionRenewal,
	stripeCustomerId string,
	lookups *subscriptionStoreLookupSet,
) (*subscriptionStoreState, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, subscriptionStoreLookupTimeout)
	defer cancel()

	switch store {
	case model.SubscriptionMarketStripe:
		if lookups.stripe == nil {
			return nil, errors.New("no stripe lookup")
		}
		return lookups.stripe(lookupCtx, stripeCustomerId, renewal.TransactionId)
	case model.SubscriptionMarketApple:
		if lookups.apple == nil || renewal.TransactionId == "" {
			return nil, errors.New("no apple transaction to look up")
		}
		return lookups.apple(lookupCtx, renewal.TransactionId)
	case model.SubscriptionMarketGoogle:
		if lookups.google == nil || renewal.PurchaseToken == "" {
			return nil, errors.New("no play purchase token to look up")
		}
		return lookups.google(lookupCtx, renewal.PurchaseToken)
	case model.SubscriptionMarketSolana:
		if lookups.solana == nil {
			return &subscriptionStoreState{AutoRenew: boolPtr(false)}, nil
		}
		state, err := lookups.solana(lookupCtx, renewal.TransactionId)
		if err != nil || state == nil {
			// a one-time payment never renews, whatever the intent table says
			return &subscriptionStoreState{AutoRenew: boolPtr(false)}, err
		}
		state.AutoRenew = boolPtr(false)
		return state, nil
	default:
		return &subscriptionStoreState{AutoRenew: boolPtr(false)}, nil
	}
}

// applySubscriptionStoreState folds the store's answer into the entry. A nil
// state (lookup failed) leaves auto_renew unknown and the row's own window.
func applySubscriptionStoreState(detail *SubscriptionDetail, state *subscriptionStoreState) {
	if state != nil {
		detail.AutoRenew = state.AutoRenew
		detail.CancelAtPeriodEnd = state.CancelAtPeriodEnd
		if state.EndTime != nil && !state.EndTime.IsZero() {
			detail.EndTime = *state.EndTime
		}
		detail.Cadence = state.Cadence
	}
	if detail.Cadence == "" {
		detail.Cadence = subscriptionCadenceFromWindow(detail.StartTime, detail.EndTime)
	}
}

// subscriptionCadenceFromWindow infers yearly/monthly from the window length
// when the store did not say. Stripe adds a grace period to its windows and a
// trial can stretch the first one, so the bands are wide; a window that fits
// neither stays "".
func subscriptionCadenceFromWindow(startTime time.Time, endTime time.Time) string {
	days := endTime.Sub(startTime).Hours() / 24
	switch {
	case 180 <= days:
		return SubscriptionCadenceYearly
	case 0 < days && days <= 62:
		return SubscriptionCadenceMonthly
	}
	return ""
}

// subscriptionCadenceFromProductId reads the cadence off a store product or
// price id ("supporter_monthly_26", "pro_yearly"), or "" when it is not named.
func subscriptionCadenceFromProductId(productId string) string {
	id := strings.ToLower(productId)
	switch {
	case strings.Contains(id, "year") || strings.Contains(id, "annual"):
		return SubscriptionCadenceYearly
	case strings.Contains(id, "month"):
		return SubscriptionCadenceMonthly
	}
	return ""
}

func boolPtr(b bool) *bool {
	return &b
}

// ----- stripe -----

// the raw subscription object (the fields we read)
type stripeCustomerSubscription struct {
	Id                string `json:"id"`
	Status            string `json:"status"`
	CancelAtPeriodEnd bool   `json:"cancel_at_period_end"`
	// on the subscription before API version 2025-03-31, on the items since
	CurrentPeriodEnd int64 `json:"current_period_end"`
	Created          int64 `json:"created"`
	Items            struct {
		Data []struct {
			CurrentPeriodEnd int64 `json:"current_period_end"`
			Price            struct {
				Id        string `json:"id"`
				Recurring *struct {
					Interval string `json:"interval"`
				} `json:"recurring"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

type stripeCustomerSubscriptionList struct {
	Data []*stripeCustomerSubscription `json:"data"`
}

type stripeInvoiceWithSubscription struct {
	Subscription *stripeCustomerSubscription `json:"subscription"`
}

func (self *stripeCustomerSubscription) periodEnd() *time.Time {
	end := self.CurrentPeriodEnd
	for _, item := range self.Items.Data {
		if end < item.CurrentPeriodEnd {
			end = item.CurrentPeriodEnd
		}
	}
	if end <= 0 {
		return nil
	}
	t := time.Unix(end, 0).UTC()
	return &t
}

func (self *stripeCustomerSubscription) cadence() string {
	for _, item := range self.Items.Data {
		if item.Price.Recurring != nil {
			switch item.Price.Recurring.Interval {
			case "year":
				return SubscriptionCadenceYearly
			case "month":
				return SubscriptionCadenceMonthly
			}
		}
		if cadence := subscriptionCadenceFromProductId(item.Price.Id); cadence != "" {
			return cadence
		}
	}
	return ""
}

// stripeSubscriptionBilling reports whether Stripe still bills this
// subscription: the statuses with a live period. past_due is still a
// subscription being collected (the reconciler's cancelled ≠ expired rule).
func stripeSubscriptionBilling(status string) bool {
	switch status {
	case "active", "trialing", "past_due":
		return true
	}
	return false
}

// stripeSubscriptionState maps a raw subscription to the store state.
func stripeSubscriptionState(sub *stripeCustomerSubscription) *subscriptionStoreState {
	active := stripeSubscriptionBilling(sub.Status)
	autoRenew := active && !sub.CancelAtPeriodEnd
	return &subscriptionStoreState{
		AutoRenew:           &autoRenew,
		CancelAtPeriodEnd:   active && sub.CancelAtPeriodEnd,
		EndTime:             sub.periodEnd(),
		Cadence:             sub.cadence(),
		StoreSubscriptionId: sub.Id,
		Active:              active,
	}
}

func stripeAuthHeader(header http.Header) {
	header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
}

// stripeListCustomerSubscriptions returns the customer's subscriptions, the
// ones Stripe still bills first (latest period end first), then the rest
// newest first.
func stripeListCustomerSubscriptions(ctx context.Context, customerId string) ([]*stripeCustomerSubscription, error) {
	listUrl := fmt.Sprintf(
		"%s/v1/subscriptions?%s",
		stripeApiBaseUrl,
		url.Values{
			"customer": []string{customerId},
			"status":   []string{"all"},
			"limit":    []string{"20"},
		}.Encode(),
	)
	list, err := server.HttpGetRequireStatusOk[*stripeCustomerSubscriptionList](
		ctx,
		listUrl,
		stripeAuthHeader,
		server.ResponseJsonObject[*stripeCustomerSubscriptionList],
	)
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}
	subs := append([]*stripeCustomerSubscription{}, list.Data...)
	sort.SliceStable(subs, func(i int, j int) bool {
		a, b := subs[i], subs[j]
		aBilling, bBilling := stripeSubscriptionBilling(a.Status), stripeSubscriptionBilling(b.Status)
		if aBilling != bBilling {
			return aBilling
		}
		aEnd, bEnd := a.CurrentPeriodEnd, b.CurrentPeriodEnd
		if end := a.periodEnd(); end != nil {
			aEnd = end.Unix()
		}
		if end := b.periodEnd(); end != nil {
			bEnd = end.Unix()
		}
		if aEnd != bEnd {
			return bEnd < aEnd
		}
		return b.Created < a.Created
	})
	return subs, nil
}

// stripeSubscriptionFromInvoice resolves the subscription an invoice billed
// (the renewal's transaction id is the invoice id).
func stripeSubscriptionFromInvoice(ctx context.Context, invoiceId string) (*stripeCustomerSubscription, error) {
	invoiceUrl := fmt.Sprintf(
		"%s/v1/invoices/%s?expand[]=subscription",
		stripeApiBaseUrl,
		url.PathEscape(invoiceId),
	)
	invoice, err := server.HttpGetRequireStatusOk[*stripeInvoiceWithSubscription](
		ctx,
		invoiceUrl,
		stripeAuthHeader,
		server.ResponseJsonObject[*stripeInvoiceWithSubscription],
	)
	if err != nil {
		return nil, fmt.Errorf("invoice %s: %w", invoiceId, err)
	}
	if invoice.Subscription == nil || invoice.Subscription.Id == "" {
		return nil, fmt.Errorf("invoice %s has no subscription", invoiceId)
	}
	return invoice.Subscription, nil
}

// stripeFindSubscription finds the subscription that bills the network: the
// customer's billing subscription when there is a customer, else the one
// behind the renewal's invoice.
func stripeFindSubscription(ctx context.Context, customerId string, invoiceId string) (*stripeCustomerSubscription, error) {
	if customerId != "" {
		subs, err := stripeListCustomerSubscriptions(ctx, customerId)
		if err != nil {
			return nil, err
		}
		if 0 < len(subs) {
			return subs[0], nil
		}
	}
	if invoiceId != "" {
		return stripeSubscriptionFromInvoice(ctx, invoiceId)
	}
	return nil, errors.New("no stripe subscription to look up")
}

func stripeLookupSubscriptionState(ctx context.Context, customerId string, invoiceId string) (*subscriptionStoreState, error) {
	sub, err := stripeFindSubscription(ctx, customerId, invoiceId)
	if err != nil {
		return nil, err
	}
	return stripeSubscriptionState(sub), nil
}

// stripeSetCancelAtPeriodEnd flips cancel_at_period_end on the subscription
// and returns Stripe's view of it afterwards.
func stripeSetCancelAtPeriodEnd(ctx context.Context, subscriptionId string, cancelAtPeriodEnd bool) (*stripeCustomerSubscription, error) {
	updateUrl := fmt.Sprintf("%s/v1/subscriptions/%s", stripeApiBaseUrl, url.PathEscape(subscriptionId))
	value := "false"
	if cancelAtPeriodEnd {
		value = "true"
	}
	sub, err := server.HttpPostForm[*stripeCustomerSubscription](
		ctx,
		updateUrl,
		url.Values{"cancel_at_period_end": []string{value}},
		stripeAuthHeader,
		server.HttpResponseRequireStatusOk[*stripeCustomerSubscription](
			server.ResponseJsonObject[*stripeCustomerSubscription],
		),
	)
	if err != nil {
		return nil, fmt.Errorf("update subscription %s: %w", subscriptionId, err)
	}
	return sub, nil
}

// ----- apple -----

func appleLookupSubscriptionState(ctx context.Context, originalTransactionId string) (*subscriptionStoreState, error) {
	if appleReconcileCredentialsFunc() == nil {
		return nil, errors.New("no App Store Server API credentials")
	}
	statusesUrl := fmt.Sprintf(
		"%s/inApps/v1/subscriptions/%s",
		appleAppStoreServerApiBaseUrl,
		url.PathEscape(originalTransactionId),
	)
	statuses, err := server.HttpGetRequireStatusOk[*appleSubscriptionStatusesResponse](
		ctx,
		statusesUrl,
		func(header http.Header) {
			appleServerApiAuthHeaderFunc(ctx, header)
		},
		server.ResponseJsonObject[*appleSubscriptionStatusesResponse],
	)
	if err != nil {
		return nil, fmt.Errorf("subscription statuses: %w", err)
	}
	state := appleSubscriptionState(statuses)
	if state == nil {
		return nil, errors.New("no transactions in the subscription statuses")
	}
	return state, nil
}

// appleSubscriptionState reads the store's latest transaction: an entitled
// one (active / in grace) if any, else the last one reported. The renewal
// info's autoRenewStatus is the customer's auto-renew switch; a status of 1
// with the switch off is "cancel at period end".
func appleSubscriptionState(statuses *appleSubscriptionStatusesResponse) *subscriptionStoreState {
	var transaction *appleLastTransaction
	for _, group := range statuses.Data {
		for _, lastTransaction := range group.LastTransactions {
			if transaction == nil || lastTransaction.entitled() {
				transaction = lastTransaction
			}
			if lastTransaction.entitled() {
				break
			}
		}
		if transaction != nil && transaction.entitled() {
			break
		}
	}
	if transaction == nil {
		return nil
	}

	state := &subscriptionStoreState{
		Active: transaction.entitled(),
	}
	autoRenewSwitch := -1
	if transaction.SignedRenewalInfo != "" {
		if claims, err := appleDecodeJwsPayload(transaction.SignedRenewalInfo); err == nil {
			if v, ok := claims["autoRenewStatus"].(float64); ok {
				autoRenewSwitch = int(v)
			}
			if productId, _ := appleControllerStringClaim(claims, "autoRenewProductId"); productId != "" {
				state.Cadence = subscriptionCadenceFromProductId(productId)
			}
		}
	}
	if transaction.SignedTransactionInfo != "" {
		if claims, err := appleDecodeJwsPayload(transaction.SignedTransactionInfo); err == nil {
			if v, ok := claims["expiresDate"].(float64); ok && 0 < v {
				t := time.UnixMilli(int64(v)).UTC()
				state.EndTime = &t
			}
			if state.Cadence == "" {
				if productId, _ := appleControllerStringClaim(claims, "productId"); productId != "" {
					state.Cadence = subscriptionCadenceFromProductId(productId)
				}
			}
		}
	}
	switch {
	case !transaction.entitled():
		state.AutoRenew = boolPtr(false)
	case autoRenewSwitch == 1:
		state.AutoRenew = boolPtr(true)
	case autoRenewSwitch == 0:
		state.AutoRenew = boolPtr(false)
		state.CancelAtPeriodEnd = true
	default:
		// entitled, renewal info missing: unknown
		state.AutoRenew = nil
	}
	return state
}

// ----- google -----

func playLookupSubscriptionState(ctx context.Context, purchaseToken string) (*subscriptionStoreState, error) {
	if !playReconcileHasCredentials() {
		return nil, errors.New("no Play credentials")
	}
	subUrl := fmt.Sprintf(
		"%s/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
		playPublisherApiBaseUrl,
		playPackageNameFunc(),
		purchaseToken,
	)
	sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
		ctx,
		subUrl,
		func(header http.Header) {
			playAuthHeaderFunc(ctx, header)
		},
		server.ResponseJsonObject[*PlaySubscription],
	)
	if err != nil {
		return nil, fmt.Errorf("subscriptionsv2: %w", err)
	}
	return playSubscriptionState(sub), nil
}

// playSubscriptionState maps a subscriptionsv2 purchase to the store state.
// The line items' autoRenewingPlan.autoRenewEnabled is the customer's switch;
// SUBSCRIPTION_STATE_CANCELED with time remaining is "cancel at period end".
func playSubscriptionState(sub *PlaySubscription) *subscriptionStoreState {
	state := &subscriptionStoreState{}
	var expiry *time.Time
	autoRenewEnabled := false
	sawAutoRenewPlan := false
	for _, item := range sub.LineItems {
		if t, err := item.ParseExpiryTime(); err == nil {
			if expiry == nil || expiry.Before(t) {
				tt := t.UTC()
				expiry = &tt
			}
		}
		if item.AutoRenewingPlan != nil {
			sawAutoRenewPlan = true
			if item.AutoRenewingPlan.AutoRenewEnabled {
				autoRenewEnabled = true
			}
		}
		if state.Cadence == "" {
			state.Cadence = subscriptionCadenceFromProductId(item.ProductId)
		}
	}
	state.EndTime = expiry
	now := server.NowUtc()
	switch sub.SubscriptionState {
	case "SUBSCRIPTION_STATE_ACTIVE", "SUBSCRIPTION_STATE_IN_GRACE_PERIOD":
		state.Active = true
		if sawAutoRenewPlan {
			state.AutoRenew = boolPtr(autoRenewEnabled)
			state.CancelAtPeriodEnd = !autoRenewEnabled
		} else {
			// a prepaid plan, or a response with no plan block: the store did
			// not say
			state.AutoRenew = nil
		}
	case "SUBSCRIPTION_STATE_CANCELED":
		state.Active = expiry != nil && expiry.After(now)
		state.AutoRenew = boolPtr(false)
		state.CancelAtPeriodEnd = state.Active
	case "SUBSCRIPTION_STATE_ON_HOLD", "SUBSCRIPTION_STATE_PAUSED", "SUBSCRIPTION_STATE_PENDING":
		state.AutoRenew = boolPtr(autoRenewEnabled && sawAutoRenewPlan)
	default:
		// expired, cancelled purchase, unspecified
		state.AutoRenew = boolPtr(false)
	}
	return state
}

// ----- solana -----

// A USDC payment buys one window and never renews. The intent names the plan
// the customer paid for, which is the cadence.
func solanaLookupSubscriptionState(ctx context.Context, reference string) (*subscriptionStoreState, error) {
	state := &subscriptionStoreState{AutoRenew: boolPtr(false)}
	if reference == "" {
		return state, nil
	}
	intent := model.GetSolanaPaymentIntent(ctx, reference)
	if intent == nil {
		return state, nil
	}
	switch intent.SubscriptionPlan {
	case model.SolanaPlanYearly:
		state.Cadence = SubscriptionCadenceYearly
	case model.SolanaPlanMonthly:
		state.Cadence = SubscriptionCadenceMonthly
	}
	return state, nil
}

// ----- cancel / resume -----

type SubscriptionCancelArgs struct {
	// the store to cancel on: "stripe" (the only one the server can cancel)
	Store string `json:"store"`
}

type SubscriptionCancelError struct {
	Message string `json:"message"`
}

type SubscriptionCancelResult struct {
	Store string `json:"store,omitempty"`
	// the date the current period ends: the customer keeps Pro until then
	EndTime   *time.Time `json:"end_time,omitempty"`
	AutoRenew *bool      `json:"auto_renew,omitempty"`
	// for a store the server cannot act on: where the customer does it
	ManageUrl string                   `json:"manage_url,omitempty"`
	Error     *SubscriptionCancelError `json:"error,omitempty"`
}

func subscriptionCancelError(message string, manageUrl string) *SubscriptionCancelResult {
	return &SubscriptionCancelResult{
		ManageUrl: manageUrl,
		Error:     &SubscriptionCancelError{Message: message},
	}
}

// SubscriptionCancel answers POST /subscription/cancel: the Stripe
// subscription runs out at the end of the paid period and is not billed again.
func SubscriptionCancel(args *SubscriptionCancelArgs, clientSession *session.ClientSession) (*SubscriptionCancelResult, error) {
	return stripeSetSubscriptionCancelAtPeriodEnd(args, clientSession, true)
}

// SubscriptionResume answers POST /subscription/resume: undoes a cancel while
// the paid period is still running.
func SubscriptionResume(args *SubscriptionCancelArgs, clientSession *session.ClientSession) (*SubscriptionCancelResult, error) {
	return stripeSetSubscriptionCancelAtPeriodEnd(args, clientSession, false)
}

// subscriptionCancelRefusal is the answer for a store the server cannot act
// on, or nil when the store is one it can.
func subscriptionCancelRefusal(store string) *SubscriptionCancelResult {
	switch store {
	case model.SubscriptionMarketStripe:
		return nil
	case model.SubscriptionMarketApple:
		return subscriptionCancelError("Cancel this subscription in the App Store.", appleManageSubscriptionsUrl)
	case model.SubscriptionMarketGoogle:
		return subscriptionCancelError("Cancel this subscription in Google Play.", googleManageSubscriptionsUrl)
	case model.SubscriptionMarketSolana:
		return subscriptionCancelError("A USDC payment covers one period and does not renew. There is nothing to cancel.", "")
	default:
		return subscriptionCancelError("This subscription cannot be cancelled here.", "")
	}
}

func stripeSetSubscriptionCancelAtPeriodEnd(
	args *SubscriptionCancelArgs,
	clientSession *session.ClientSession,
	cancelAtPeriodEnd bool,
) (*SubscriptionCancelResult, error) {
	store := strings.ToLower(strings.TrimSpace(args.Store))
	if store == "" {
		store = model.SubscriptionMarketStripe
	}
	if refusal := subscriptionCancelRefusal(store); refusal != nil {
		return refusal, nil
	}

	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId

	customerId := ""
	if id, err := model.GetStripeCustomer(clientSession); err == nil && id != nil {
		customerId = *id
	}
	invoiceId := ""
	for _, renewal := range model.GetActiveSubscriptionRenewals(ctx, networkId, model.SubscriptionTypeSupporter) {
		if strings.EqualFold(renewal.Market, model.SubscriptionMarketStripe) && renewal.TransactionId != "" {
			invoiceId = renewal.TransactionId
			break
		}
	}
	if customerId == "" && invoiceId == "" {
		return subscriptionCancelError("No Stripe subscription is billing this network.", ""), nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, subscriptionStoreLookupTimeout)
	defer cancel()
	sub, err := stripeFindSubscription(lookupCtx, customerId, invoiceId)
	if err != nil {
		glog.Infof("[sub]cancel: find stripe subscription for %s: %v\n", networkId, err)
		return subscriptionCancelError("Could not reach Stripe. Please try again.", ""), nil
	}
	if !stripeSubscriptionBilling(sub.Status) {
		return subscriptionCancelError("No Stripe subscription is billing this network.", ""), nil
	}
	if sub.CancelAtPeriodEnd != cancelAtPeriodEnd {
		updated, err := stripeSetCancelAtPeriodEnd(lookupCtx, sub.Id, cancelAtPeriodEnd)
		if err != nil {
			glog.Infof("[sub]cancel: update stripe subscription %s: %v\n", sub.Id, err)
			return subscriptionCancelError("Could not update the subscription with Stripe. Please try again.", ""), nil
		}
		sub = updated
	}
	clearSubscriptionDetailsCached(ctx, networkId)

	state := stripeSubscriptionState(sub)
	glog.Infof("[sub]stripe subscription %s for %s: cancel_at_period_end=%t\n", sub.Id, networkId, sub.CancelAtPeriodEnd)
	return &SubscriptionCancelResult{
		Store:     model.SubscriptionMarketStripe,
		EndTime:   state.EndTime,
		AutoRenew: state.AutoRenew,
	}, nil
}
