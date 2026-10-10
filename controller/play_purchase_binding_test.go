package controller

// Root-cause tests for the Play half of UPGRADE.md A1: a purchase token with
// no account identifiers (a Play Store promo code redemption, a purchase
// outside the app's billing flow) was credited to whichever session network
// reported it first. They drive the real VerifyPlayPurchase against the
// hermetic fake Android Publisher API (newPlayWebhookTestEnv) with the db
// steps replaced by a fake transactional store and a fixed offer clock, so
// they run without the test database.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

const playBindingTestProductId = "supporter_yearly"
const playBindingTestOfferTag = "onboarding25"

type playBindingFakeBinding struct {
	networkId server.Id
	root      string
	offer     string
}

type playBindingFakeState struct {
	networks map[server.Id]bool
	offers   map[server.Id]model.OnboardingOffer
	bindings map[string]playBindingFakeBinding
	// ledger: purchase token + max expiry -> network (the overlap gate)
	ledger map[string]server.Id
}

func (self *playBindingFakeState) clone() *playBindingFakeState {
	c := &playBindingFakeState{
		networks: map[server.Id]bool{},
		offers:   map[server.Id]model.OnboardingOffer{},
		bindings: map[string]playBindingFakeBinding{},
		ledger:   map[string]server.Id{},
	}
	for k, v := range self.networks {
		c.networks[k] = v
	}
	for k, v := range self.offers {
		c.offers[k] = v
	}
	for k, v := range self.bindings {
		c.bindings[k] = v
	}
	for k, v := range self.ledger {
		c.ledger[k] = v
	}
	return c
}

func playBindingLedgerKey(purchaseToken string, maxExpiryTime time.Time) string {
	return purchaseToken + "@" + maxExpiryTime.UTC().Format(time.RFC3339)
}

// playBindingFakeStore is one fake transaction over a staged copy of the
// state; the runner publishes the copy only on commit, like a db rollback.
type playBindingFakeStore struct {
	state *playBindingFakeState
}

func (self *playBindingFakeStore) BoundNetworkId(purchaseToken string) (server.Id, string, bool) {
	binding, ok := self.state.bindings[purchaseToken]
	return binding.networkId, binding.root, ok
}

func (self *playBindingFakeStore) NetworkExists(networkId server.Id) bool {
	return self.state.networks[networkId]
}

func (self *playBindingFakeStore) OfferForUpdate(networkId server.Id) *model.OnboardingOffer {
	offer, ok := self.state.offers[networkId]
	if !ok {
		return nil
	}
	return &offer
}

func (self *playBindingFakeStore) Bind(purchaseToken string, rootPurchaseToken string, networkId server.Id, offer string) bool {
	if _, ok := self.state.bindings[purchaseToken]; ok {
		return false
	}
	self.state.bindings[purchaseToken] = playBindingFakeBinding{networkId: networkId, root: rootPurchaseToken, offer: offer}
	return true
}

func (self *playBindingFakeStore) RedeemOffer(networkId server.Id, at time.Time) bool {
	// the same predicate as model.RedeemOnboardingOfferInTx
	offer, ok := self.state.offers[networkId]
	if !ok || offer.RedeemedAt != nil || !at.Before(offer.ExpiresAt) {
		return false
	}
	store := model.OnboardingStorePlay
	offer.RedeemedAt = &at
	offer.Store = &store
	self.state.offers[networkId] = offer
	return true
}

func (self *playBindingFakeStore) Credit(purchase *playUnlinkedPurchase, networkId server.Id) (bool, error) {
	key := playBindingLedgerKey(purchase.purchaseToken, purchase.maxExpiryTime)
	if _, ok := self.state.ledger[key]; ok {
		return false, nil
	}
	self.state.ledger[key] = networkId
	return true, nil
}

type playBindingFakeEnv struct {
	play      *playWebhookTestEnv
	state     *playBindingFakeState
	match     playWelcomeOfferMatch
	refreshed []server.Id
	// renewals are the linked-purchase credits through PlaySubscriptionRenewal
	renewals []*PlaySubscriptionRenewalArgs
}

// installPlayBindingFakes stands up the fake Publisher API and swaps every db
// step of VerifyPlayPurchase for fakes, restored when the test ends.
func installPlayBindingFakes(t *testing.T) *playBindingFakeEnv {
	env := &playBindingFakeEnv{
		play: newPlayWebhookTestEnv(t, map[string]*Sku{
			playBindingTestProductId: {
				FeeFraction:    0.15,
				PriceAmountUsd: 40.0,
				Supporter:      true,
			},
		}),
		state: (&playBindingFakeState{}).clone(),
		match: playWelcomeOfferMatch{offerTag: playBindingTestOfferTag},
	}
	previousRateLimit := playVerifyRateLimitFunc
	previousRenewal := playVerifyRenewalFunc
	previousPaymentId := playPaymentIdNetworkIdFunc
	previousRun := playPurchaseBindingRunFunc
	previousMatch := playWelcomeOfferMatchFunc
	previousRefresh := playPurchaseBindingProRefreshFunc
	previousLookup := playPurchaseBindingLookupFunc
	t.Cleanup(func() {
		playVerifyRateLimitFunc = previousRateLimit
		playVerifyRenewalFunc = previousRenewal
		playPaymentIdNetworkIdFunc = previousPaymentId
		playPurchaseBindingRunFunc = previousRun
		playWelcomeOfferMatchFunc = previousMatch
		playPurchaseBindingProRefreshFunc = previousRefresh
		playPurchaseBindingLookupFunc = previousLookup
	})
	playVerifyRateLimitFunc = func(clientSession *session.ClientSession) error {
		return nil
	}
	// the linked (in-app) credit path: the real gate needs the db; record the
	// credit in the same fake ledger
	playVerifyRenewalFunc = func(args *PlaySubscriptionRenewalArgs, clientSession *session.ClientSession) (*PlaySubscriptionRenewalResult, error) {
		env.renewals = append(env.renewals, args)
		sub := env.play.subscriptions[args.PurchaseToken]
		maxExpiryTime, minExpiryTime, err := playLineItemExpiryRange(sub)
		if err != nil {
			return nil, err
		}
		key := playBindingLedgerKey(args.PurchaseToken, maxExpiryTime)
		if _, ok := env.state.ledger[key]; ok {
			return &PlaySubscriptionRenewalResult{ExpiryTime: minExpiryTime}, nil
		}
		env.state.ledger[key] = args.NetworkId
		return &PlaySubscriptionRenewalResult{ExpiryTime: minExpiryTime, Renewed: true}, nil
	}
	playPaymentIdNetworkIdFunc = func(_ server.PgConn, ctx context.Context, subscriptionPaymentId server.Id) (server.Id, error) {
		// no subscription payment ids: the obfuscated id is a network id
		return server.Id{}, errors.New("not a subscription payment id")
	}
	playPurchaseBindingRunFunc = func(
		clientSession *session.ClientSession,
		callback func(store playPurchaseBindingTx) playPurchaseBindingResult,
	) playPurchaseBindingResult {
		staged := env.state.clone()
		result := callback(&playBindingFakeStore{state: staged})
		if result.commit {
			env.state = staged
		}
		return result
	}
	playWelcomeOfferMatchFunc = func() playWelcomeOfferMatch {
		return env.match
	}
	playPurchaseBindingProRefreshFunc = func(ctx context.Context, networkId server.Id) {
		env.refreshed = append(env.refreshed, networkId)
	}
	playPurchaseBindingLookupFunc = func(connOwner server.PgConn, ctx context.Context, purchaseToken string, linkedPurchaseToken string, inherit bool) (server.Id, bool) {
		if binding, ok := env.state.bindings[purchaseToken]; ok {
			return binding.networkId, true
		}
		if binding, ok := env.state.bindings[linkedPurchaseToken]; ok && linkedPurchaseToken != "" {
			if inherit {
				env.state.bindings[purchaseToken] = playBindingFakeBinding{networkId: binding.networkId, root: binding.root}
			}
			return binding.networkId, true
		}
		return server.Id{}, false
	}
	return env
}

// playBindingNow is fixed; offer and purchase times are relative to it. The
// binding rules compare only Google's startTime with the offer window; the
// credit gate's own clock check is part of the faked credit.
var playBindingNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// addNetwork adds a network; with an offer issued `issuedAgo` before now,
// valid for 5 days, carrying the Play offer tag.
func (self *playBindingFakeEnv) addNetwork(withOffer bool, issuedAgo time.Duration) (*session.ClientSession, server.Id) {
	networkId := server.NewId()
	self.state.networks[networkId] = true
	if withOffer {
		tag := playBindingTestOfferTag
		issuedAt := playBindingNow.Add(-issuedAgo)
		self.state.offers[networkId] = model.OnboardingOffer{
			NetworkId:    networkId,
			IssuedAt:     issuedAt,
			ExpiresAt:    issuedAt.Add(5 * 24 * time.Hour),
			IssuedBy:     model.OnboardingOfferIssuedByInApp,
			PercentOff:   25,
			PlayOfferTag: &tag,
		}
	}
	clientSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{
		NetworkId: networkId,
		UserId:    server.NewId(),
	})
	return clientSession, networkId
}

// unlinkedPurchase puts an ACTIVE purchase with no account identifiers,
// bought `startAgo` before now under the welcome offer tag, into the fake
// Publisher API.
func (self *playBindingFakeEnv) unlinkedPurchase(purchaseToken string, startAgo time.Duration) *PlaySubscription {
	startTime := playBindingNow.Add(-startAgo)
	sub := &PlaySubscription{
		LineItems: []*PlaySubscriptionPurchaseLineItem{
			{
				ProductId:  playBindingTestProductId,
				ExpiryTime: startTime.Add(365 * 24 * time.Hour).Format(time.RFC3339),
				OfferDetails: &PlayOfferDetails{
					BasePlanId: "yearly",
					OfferId:    "onboarding-25",
					OfferTags:  []string{playBindingTestOfferTag},
				},
			},
		},
		StartTime:            startTime.Format(time.RFC3339),
		SubscriptionState:    "SUBSCRIPTION_STATE_ACTIVE",
		AcknowledgementState: "ACKNOWLEDGEMENT_STATE_PENDING",
	}
	self.play.subscriptions[purchaseToken] = sub
	return sub
}

func (self *playBindingFakeEnv) verify(t *testing.T, clientSession *session.ClientSession, purchaseToken string) string {
	t.Helper()
	result, err := VerifyPlayPurchase(
		&VerifyPlayPurchaseArgs{
			ProductId:     playBindingTestProductId,
			PurchaseToken: purchaseToken,
		},
		clientSession,
	)
	connect.AssertEqual(t, err, nil)
	return result.Status
}

func (self *playBindingFakeEnv) creditedNetworks() []server.Id {
	networkIds := []server.Id{}
	for _, networkId := range self.state.ledger {
		networkIds = append(networkIds, networkId)
	}
	return networkIds
}

// TestVerifyPlayUnlinkedPurchaseIsNotCreditedToFirstReporter is the defect: a
// purchase token with no account identifiers, reported by a network that was
// never issued the welcome offer, was credited to that network (whoever
// reported it first). It must answer invalid and credit nobody.
func TestVerifyPlayUnlinkedPurchaseIsNotCreditedToFirstReporter(t *testing.T) {
	env := installPlayBindingFakes(t)
	strangerSession, _ := env.addNetwork(false, 0)
	env.unlinkedPurchase("promo-token-1", time.Hour)

	connect.AssertEqual(t, env.verify(t, strangerSession, "promo-token-1"), VerifyPurchaseStatusInvalid)
	connect.AssertEqual(t, env.creditedNetworks(), []server.Id{})
	connect.AssertEqual(t, len(env.state.bindings), 0)
	connect.AssertEqual(t, len(env.renewals), 0)
}

// An unlinked purchase of the welcome offer, reported by the network that was
// issued it, binds the token, redeems the offer at Google's startTime and
// credits that network, once.
func TestVerifyPlayUnlinkedWelcomeOfferCreditsIssuedNetwork(t *testing.T) {
	env := installPlayBindingFakes(t)
	clientSession, networkId := env.addNetwork(true, 24*time.Hour)
	env.unlinkedPurchase("promo-token-1", time.Hour)

	connect.AssertEqual(t, env.verify(t, clientSession, "promo-token-1"), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, env.creditedNetworks(), []server.Id{networkId})
	connect.AssertEqual(t, env.state.bindings["promo-token-1"], playBindingFakeBinding{
		networkId: networkId,
		root:      "promo-token-1",
		offer:     "tag:" + playBindingTestOfferTag,
	})
	offer := env.state.offers[networkId]
	connect.AssertEqual(t, offer.RedeemedAt != nil, true)
	connect.AssertEqual(t, offer.RedeemedAt.Equal(playBindingNow.Add(-time.Hour)), true)
	connect.AssertEqual(t, *offer.Store, model.OnboardingStorePlay)
	connect.AssertEqual(t, env.refreshed, []server.Id{networkId})
	// the in-app credit path is not involved
	connect.AssertEqual(t, len(env.renewals), 0)

	// the client retries the same proof: terminal, nothing credited twice
	connect.AssertEqual(t, env.verify(t, clientSession, "promo-token-1"), VerifyPurchaseStatusAlreadyCredited)
	connect.AssertEqual(t, len(env.state.ledger), 1)
	connect.AssertEqual(t, env.refreshed, []server.Id{networkId})
}

// A custom (vanity) Play promotion code configured as the welcome offer is
// recognized from signupPromotion; a one-time code carries no identifier and
// is not.
func TestVerifyPlayUnlinkedPromotionCode(t *testing.T) {
	env := installPlayBindingFakes(t)
	env.match.promotionCode = "WELCOME25"
	clientSession, networkId := env.addNetwork(true, 24*time.Hour)
	sub := env.unlinkedPurchase("promo-token-1", time.Hour)
	// a promo code replaces the offer: the base plan, no offer tags
	sub.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "yearly"}
	sub.LineItems[0].SignupPromotion = &PlaySignupPromotion{
		VanityCode: &PlayVanityCode{PromotionCode: "welcome25"},
	}

	connect.AssertEqual(t, env.verify(t, clientSession, "promo-token-1"), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, env.state.bindings["promo-token-1"].offer, "promotion:WELCOME25")
	connect.AssertEqual(t, env.creditedNetworks(), []server.Id{networkId})

	otherSession, _ := env.addNetwork(true, 24*time.Hour)
	oneTime := env.unlinkedPurchase("promo-token-2", time.Hour)
	oneTime.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "yearly"}
	oneTime.LineItems[0].SignupPromotion = &PlaySignupPromotion{OneTimeCode: &struct{}{}}
	connect.AssertEqual(t, env.verify(t, otherSession, "promo-token-2"), VerifyPurchaseStatusInvalid)
	connect.AssertEqual(t, len(env.state.ledger), 1)
}

// Renewals of a bound purchase credit the bound network, through the verify
// endpoint (the same token's next period, and a new token linked to it) and
// the webhook/reconciler resolution, and nobody else.
func TestPlayUnlinkedRenewalsCreditBoundNetwork(t *testing.T) {
	env := installPlayBindingFakes(t)
	clientSession, networkId := env.addNetwork(true, 24*time.Hour)
	otherSession, _ := env.addNetwork(true, 24*time.Hour)
	sub := env.unlinkedPurchase("promo-token-1", time.Hour)

	connect.AssertEqual(t, env.verify(t, clientSession, "promo-token-1"), VerifyPurchaseStatusCredited)

	// the next period: same token, later expiry, the offer long used up
	sub.LineItems[0].ExpiryTime = playBindingNow.Add(2 * 365 * 24 * time.Hour).Format(time.RFC3339)
	sub.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "yearly"}
	connect.AssertEqual(t, env.verify(t, otherSession, "promo-token-1"), VerifyPurchaseStatusInvalid)
	connect.AssertEqual(t, env.verify(t, clientSession, "promo-token-1"), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, len(env.state.ledger), 2)

	// a re-signup under a new token names the old one as linkedPurchaseToken
	resignup := env.unlinkedPurchase("promo-token-1b", time.Minute)
	resignup.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "yearly"}
	resignup.LinkedPurchaseToken = "promo-token-1"
	connect.AssertEqual(t, env.verify(t, otherSession, "promo-token-1b"), VerifyPurchaseStatusInvalid)
	connect.AssertEqual(t, env.verify(t, clientSession, "promo-token-1b"), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, env.state.bindings["promo-token-1b"], playBindingFakeBinding{
		networkId: networkId,
		root:      "promo-token-1",
	})
	for _, creditedNetworkId := range env.creditedNetworks() {
		connect.AssertEqual(t, creditedNetworkId, networkId)
	}

	// the RTDN webhook and the reconciler resolve the chain to the same network
	webhookSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()})
	next := &PlaySubscription{LinkedPurchaseToken: "promo-token-1b"}
	resolved, ok := playResolveNetworkId(webhookSession, next, "promo-token-1c", true)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, *resolved, networkId)
	connect.AssertEqual(t, env.state.bindings["promo-token-1c"].networkId, networkId)

	// a purchase that was never bound stays unresolved
	resolved, ok = playResolveNetworkId(webhookSession, &PlaySubscription{}, "never-bound", true)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, resolved == nil, true)

	// the reconciler never credits a renewal row of another network for a
	// bound purchase, and keeps rows of never-bound purchases
	connect.AssertEqual(t, playUnlinkedBoundElsewhere(webhookSession, sub, &model.ReconcileSubscriptionRenewal{
		NetworkId:     networkId,
		PurchaseToken: "promo-token-1",
	}), false)
	connect.AssertEqual(t, playUnlinkedBoundElsewhere(webhookSession, sub, &model.ReconcileSubscriptionRenewal{
		NetworkId:     server.NewId(),
		PurchaseToken: "promo-token-1",
	}), true)
	connect.AssertEqual(t, playUnlinkedBoundElsewhere(webhookSession, &PlaySubscription{}, &model.ReconcileSubscriptionRenewal{
		NetworkId:     server.NewId(),
		PurchaseToken: "never-bound",
	}), false)
}

// A token with an obfuscated account id is unchanged: credited to the session
// network through the in-app credit path when it matches, wrong_network
// otherwise, and never bound.
func TestVerifyPlayLinkedPurchaseUnchanged(t *testing.T) {
	env := installPlayBindingFakes(t)
	clientSession, networkId := env.addNetwork(true, 24*time.Hour)
	otherSession, _ := env.addNetwork(true, 24*time.Hour)
	sub := env.unlinkedPurchase("inapp-token-1", time.Hour)
	sub.ExternalAccountIdentifiers = &PlayExternalAccountIdentifiers{
		ObfuscatedExternalAccountId: networkId.String(),
	}

	connect.AssertEqual(t, env.verify(t, otherSession, "inapp-token-1"), VerifyPurchaseStatusWrongNetwork)
	connect.AssertEqual(t, env.verify(t, clientSession, "inapp-token-1"), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, len(env.renewals), 1)
	connect.AssertEqual(t, env.renewals[0].NetworkId, networkId)
	connect.AssertEqual(t, len(env.state.bindings), 0)
}

// A pending unlinked purchase stays pending and binds nothing.
func TestVerifyPlayUnlinkedPendingDoesNotBind(t *testing.T) {
	env := installPlayBindingFakes(t)
	clientSession, _ := env.addNetwork(true, 24*time.Hour)
	sub := env.unlinkedPurchase("promo-token-1", time.Hour)
	sub.SubscriptionState = "SUBSCRIPTION_STATE_PENDING"

	connect.AssertEqual(t, env.verify(t, clientSession, "promo-token-1"), VerifyPurchaseStatusPending)
	connect.AssertEqual(t, len(env.state.bindings), 0)
	connect.AssertEqual(t, len(env.state.ledger), 0)
}

// Every refusal answers invalid and persists nothing (no binding, no
// redemption, no credit).
func TestVerifyPlayUnlinkedRefusals(t *testing.T) {
	type testCase struct {
		name    string
		prepare func(env *playBindingFakeEnv) *session.ClientSession
	}
	cases := []testCase{
		{"never issued an offer", func(env *playBindingFakeEnv) *session.ClientSession {
			clientSession, _ := env.addNetwork(false, 0)
			env.unlinkedPurchase("promo-token-1", time.Hour)
			return clientSession
		}},
		{"offer issued without a Play offer tag", func(env *playBindingFakeEnv) *session.ClientSession {
			clientSession, networkId := env.addNetwork(true, 24*time.Hour)
			offer := env.state.offers[networkId]
			offer.PlayOfferTag = nil
			env.state.offers[networkId] = offer
			env.unlinkedPurchase("promo-token-1", time.Hour)
			return clientSession
		}},
		{"offer already redeemed", func(env *playBindingFakeEnv) *session.ClientSession {
			clientSession, networkId := env.addNetwork(true, 24*time.Hour)
			offer := env.state.offers[networkId]
			redeemedAt := playBindingNow.Add(-2 * time.Hour)
			store := model.OnboardingStoreStripe
			offer.RedeemedAt = &redeemedAt
			offer.Store = &store
			env.state.offers[networkId] = offer
			env.unlinkedPurchase("promo-token-1", time.Hour)
			return clientSession
		}},
		{"offer expired before the purchase", func(env *playBindingFakeEnv) *session.ClientSession {
			// issued 6 days ago, valid 5: expired a day ago
			clientSession, _ := env.addNetwork(true, 6*24*time.Hour)
			env.unlinkedPurchase("promo-token-1", time.Hour)
			return clientSession
		}},
		{"purchase before the offer was issued", func(env *playBindingFakeEnv) *session.ClientSession {
			clientSession, _ := env.addNetwork(true, time.Hour)
			env.unlinkedPurchase("promo-token-1", time.Hour+6*time.Minute)
			return clientSession
		}},
		{"purchase bound to another network", func(env *playBindingFakeEnv) *session.ClientSession {
			_, otherNetworkId := env.addNetwork(true, 24*time.Hour)
			env.state.bindings["promo-token-1"] = playBindingFakeBinding{networkId: otherNetworkId, root: "promo-token-1"}
			clientSession, _ := env.addNetwork(true, 24*time.Hour)
			env.unlinkedPurchase("promo-token-1", time.Hour)
			return clientSession
		}},
		{"linked purchase bound to another network", func(env *playBindingFakeEnv) *session.ClientSession {
			_, otherNetworkId := env.addNetwork(true, 24*time.Hour)
			env.state.bindings["promo-token-0"] = playBindingFakeBinding{networkId: otherNetworkId, root: "promo-token-0"}
			clientSession, _ := env.addNetwork(true, 24*time.Hour)
			sub := env.unlinkedPurchase("promo-token-1", time.Hour)
			sub.LinkedPurchaseToken = "promo-token-0"
			return clientSession
		}},
		{"not the welcome offer", func(env *playBindingFakeEnv) *session.ClientSession {
			clientSession, _ := env.addNetwork(true, 24*time.Hour)
			sub := env.unlinkedPurchase("promo-token-1", time.Hour)
			sub.LineItems[0].OfferDetails = &PlayOfferDetails{
				BasePlanId: "yearly",
				OfferId:    "someone-elses-offer",
				OfferTags:  []string{"spring-sale"},
			}
			return clientSession
		}},
		{"an unconfigured promotion code", func(env *playBindingFakeEnv) *session.ClientSession {
			clientSession, _ := env.addNetwork(true, 24*time.Hour)
			sub := env.unlinkedPurchase("promo-token-1", time.Hour)
			sub.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "yearly"}
			sub.LineItems[0].SignupPromotion = &PlaySignupPromotion{
				VanityCode: &PlayVanityCode{PromotionCode: "WELCOME25"},
			}
			return clientSession
		}},
		{"purchase already credited elsewhere", func(env *playBindingFakeEnv) *session.ClientSession {
			_, otherNetworkId := env.addNetwork(false, 0)
			sub := env.unlinkedPurchase("promo-token-1", time.Hour)
			maxExpiryTime, _, _ := playLineItemExpiryRange(sub)
			env.state.ledger[playBindingLedgerKey("promo-token-1", maxExpiryTime)] = otherNetworkId
			clientSession, _ := env.addNetwork(true, 24*time.Hour)
			return clientSession
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := installPlayBindingFakes(t)
			clientSession := c.prepare(env)
			before := env.state.clone()

			connect.AssertEqual(t, env.verify(t, clientSession, "promo-token-1"), VerifyPurchaseStatusInvalid)
			connect.AssertEqual(t, env.state.bindings, before.bindings)
			connect.AssertEqual(t, env.state.ledger, before.ledger)
			connect.AssertEqual(t, env.state.offers, before.offers)
			connect.AssertEqual(t, len(env.refreshed), 0)
			connect.AssertEqual(t, len(env.renewals), 0)
		})
	}
}

// The issue skew: a purchase up to 5 minutes before issued_at is inside the
// window (Google's clock vs ours).
func TestVerifyPlayUnlinkedIssueSkew(t *testing.T) {
	env := installPlayBindingFakes(t)
	clientSession, networkId := env.addNetwork(true, time.Hour)
	env.unlinkedPurchase("promo-token-1", time.Hour+4*time.Minute)

	connect.AssertEqual(t, env.verify(t, clientSession, "promo-token-1"), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, env.creditedNetworks(), []server.Id{networkId})
}
