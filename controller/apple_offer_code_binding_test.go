package controller

// Root-cause tests for UPGRADE.md A1 (offer-code redemptions carry no
// appAccountToken, so the verify endpoint answered `invalid` and the app
// finished the transaction without any network being credited). They drive
// VerifyAppleTransactionClaims -- the exact input the api/handlers pinned-root
// verifier hands it -- against a fake transactional store (no db, fixed
// clock), so they run without the test database.

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

const offerCodeTestProductId = "supporter_yearly_26"

// offerCodeTestNow is fixed; purchase dates in the claims are relative to it.
var offerCodeTestNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

type offerCodeFakeState struct {
	networks map[server.Id]bool
	offers   map[server.Id]model.OnboardingOffer
	bindings map[string]server.Id
	// ledger: transaction id -> network
	ledger map[string]server.Id
}

func (self *offerCodeFakeState) clone() *offerCodeFakeState {
	c := &offerCodeFakeState{
		networks: map[server.Id]bool{},
		offers:   map[server.Id]model.OnboardingOffer{},
		bindings: map[string]server.Id{},
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

// offerCodeFakeStore is one fake transaction over a staged copy of the state;
// the runner publishes the copy only on commit, like a db rollback.
type offerCodeFakeStore struct {
	state *offerCodeFakeState
}

func (self *offerCodeFakeStore) BoundNetworkId(originalTransactionId string) (server.Id, bool) {
	networkId, ok := self.state.bindings[originalTransactionId]
	return networkId, ok
}

func (self *offerCodeFakeStore) NetworkExists(networkId server.Id) bool {
	return self.state.networks[networkId]
}

func (self *offerCodeFakeStore) OfferForUpdate(networkId server.Id) *model.OnboardingOffer {
	offer, ok := self.state.offers[networkId]
	if !ok {
		return nil
	}
	return &offer
}

func (self *offerCodeFakeStore) Bind(transaction *validatedAppleTransaction, networkId server.Id) bool {
	if _, ok := self.state.bindings[transaction.originalTransactionId]; ok {
		return false
	}
	self.state.bindings[transaction.originalTransactionId] = networkId
	return true
}

func (self *offerCodeFakeStore) RedeemOffer(networkId server.Id, at time.Time) bool {
	// the same predicate as model.RedeemOnboardingOfferInTx
	offer, ok := self.state.offers[networkId]
	if !ok || offer.RedeemedAt != nil || !at.Before(offer.ExpiresAt) {
		return false
	}
	store := model.OnboardingStoreApple
	offer.RedeemedAt = &at
	offer.Store = &store
	self.state.offers[networkId] = offer
	return true
}

func (self *offerCodeFakeStore) Credit(transaction *validatedAppleTransaction) bool {
	if _, ok := self.state.ledger[transaction.transactionId]; ok {
		return false
	}
	self.state.ledger[transaction.transactionId] = transaction.networkId
	return true
}

type offerCodeFakeEnv struct {
	state     *offerCodeFakeState
	refreshed []server.Id
}

// installOfferCodeFakes swaps the binding seams for the fake store and
// restores them when the test ends.
func installOfferCodeFakes(t *testing.T) *offerCodeFakeEnv {
	env := &offerCodeFakeEnv{
		state: (&offerCodeFakeState{}).clone(),
	}
	previousRun := appleOfferCodeBindingRunFunc
	previousName := appleOfferReferenceNameFunc
	previousRefresh := appleOfferCodeProRefreshFunc
	previousLookup := appleOfferCodeBindingLookupFunc
	previousNow := appleVerifyNowFunc
	t.Cleanup(func() {
		appleOfferCodeBindingRunFunc = previousRun
		appleOfferReferenceNameFunc = previousName
		appleOfferCodeProRefreshFunc = previousRefresh
		appleOfferCodeBindingLookupFunc = previousLookup
		appleVerifyNowFunc = previousNow
	})
	appleOfferCodeBindingRunFunc = func(
		ctx context.Context,
		callback func(store appleOfferCodeBindingTx) appleOfferCodeBindingResult,
	) appleOfferCodeBindingResult {
		staged := env.state.clone()
		result := callback(&offerCodeFakeStore{state: staged})
		if result.commit {
			env.state = staged
		}
		return result
	}
	appleOfferReferenceNameFunc = func() string {
		return model.OnboardingAppleOfferReferenceNameDefault
	}
	appleOfferCodeProRefreshFunc = func(ctx context.Context, networkId server.Id) {
		env.refreshed = append(env.refreshed, networkId)
	}
	appleOfferCodeBindingLookupFunc = func(ctx context.Context, originalTransactionId string) (server.Id, bool) {
		networkId, ok := env.state.bindings[originalTransactionId]
		return networkId, ok
	}
	appleVerifyNowFunc = func() time.Time {
		return offerCodeTestNow
	}
	return env
}

// addNetwork adds a network; with an offer issued `issuedAgo` before now,
// valid for 5 days, carrying a one-time App Store code.
func (self *offerCodeFakeEnv) addNetwork(withOffer bool, issuedAgo time.Duration) (*session.ClientSession, server.Id) {
	networkId := server.NewId()
	self.state.networks[networkId] = true
	if withOffer {
		code := "ONETIME" + networkId.String()[:8]
		issuedAt := offerCodeTestNow.Add(-issuedAgo)
		self.state.offers[networkId] = model.OnboardingOffer{
			NetworkId:      networkId,
			IssuedAt:       issuedAt,
			ExpiresAt:      issuedAt.Add(5 * 24 * time.Hour),
			IssuedBy:       model.OnboardingOfferIssuedByInApp,
			PercentOff:     25,
			AppleOfferCode: &code,
		}
	}
	clientSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{
		NetworkId: networkId,
		UserId:    server.NewId(),
	})
	return clientSession, networkId
}

// offerCodeTestClaims is a redemption as the redeem sheet produces it: no
// appAccountToken, offerType 3 (offer code), the offer's reference name.
func offerCodeTestClaims(originalTransactionId string, transactionId string, purchaseAgo time.Duration) map[string]any {
	purchase := offerCodeTestNow.Add(-purchaseAgo)
	return map[string]any{
		"transactionId":         transactionId,
		"originalTransactionId": originalTransactionId,
		"productId":             offerCodeTestProductId,
		"purchaseDate":          float64(purchase.UnixMilli()),
		"expiresDate":           float64(purchase.Add(365 * 24 * time.Hour).UnixMilli()),
		"price":                 float64(29990),
		"offerType":             float64(appleOfferTypeOfferCode),
		"offerIdentifier":       model.OnboardingAppleOfferReferenceNameDefault,
	}
}

func verifyOfferCode(t *testing.T, claims map[string]any, clientSession *session.ClientSession) string {
	t.Helper()
	result, err := VerifyAppleTransactionClaims(claims, []string{offerCodeTestProductId}, clientSession)
	connect.AssertEqual(t, err, nil)
	return result.Status
}

// TestVerifyAppleOfferCodeRedemptionCreditsIssuedNetwork is the A1 defect: a
// redemption of the welcome offer code, reported by the network that was
// issued the code, must credit that network, redeem its offer and bind the
// subscription -- not answer `invalid` (which made the app finish the
// transaction with nobody credited).
func TestVerifyAppleOfferCodeRedemptionCreditsIssuedNetwork(t *testing.T) {
	env := installOfferCodeFakes(t)
	clientSession, networkId := env.addNetwork(true, 24*time.Hour)

	claims := offerCodeTestClaims("orig-1", "tx-1", time.Hour)
	connect.AssertEqual(t, verifyOfferCode(t, claims, clientSession), VerifyPurchaseStatusCredited)

	connect.AssertEqual(t, env.state.ledger["tx-1"], networkId)
	connect.AssertEqual(t, env.state.bindings["orig-1"], networkId)
	offer := env.state.offers[networkId]
	connect.AssertEqual(t, offer.RedeemedAt != nil, true)
	connect.AssertEqual(t, offer.RedeemedAt.Equal(offerCodeTestNow.Add(-time.Hour)), true)
	connect.AssertEqual(t, *offer.Store, model.OnboardingStoreApple)
	connect.AssertEqual(t, env.refreshed, []server.Id{networkId})

	// the client retries the same proof: terminal, nothing credited twice
	connect.AssertEqual(t, verifyOfferCode(t, claims, clientSession), VerifyPurchaseStatusAlreadyCredited)
	connect.AssertEqual(t, len(env.state.ledger), 1)
}

// The offerIdentifier may carry the issued code itself rather than the
// reference name; both name the welcome offer.
func TestVerifyAppleOfferCodeMatchesIssuedCode(t *testing.T) {
	env := installOfferCodeFakes(t)
	clientSession, networkId := env.addNetwork(true, 24*time.Hour)

	claims := offerCodeTestClaims("orig-1", "tx-1", time.Hour)
	claims["offerIdentifier"] = *env.state.offers[networkId].AppleOfferCode
	connect.AssertEqual(t, verifyOfferCode(t, claims, clientSession), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, env.state.ledger["tx-1"], networkId)
}

// Renewals of a bound subscription credit the bound network, through the
// verify endpoint and through the webhook/reconciler validation, and nobody
// else.
func TestAppleOfferCodeRenewalsCreditBoundNetwork(t *testing.T) {
	env := installOfferCodeFakes(t)
	clientSession, networkId := env.addNetwork(true, 24*time.Hour)
	otherSession, _ := env.addNetwork(true, 24*time.Hour)

	connect.AssertEqual(t, verifyOfferCode(t, offerCodeTestClaims("orig-1", "tx-1", time.Hour), clientSession), VerifyPurchaseStatusCredited)

	// the renewal: a regular (non-offer) transaction of the same subscription,
	// purchased long after the offer expired
	renewal := offerCodeTestClaims("orig-1", "tx-2", time.Minute)
	delete(renewal, "offerType")
	delete(renewal, "offerIdentifier")
	connect.AssertEqual(t, verifyOfferCode(t, renewal, otherSession), VerifyPurchaseStatusInvalid)
	connect.AssertEqual(t, verifyOfferCode(t, renewal, clientSession), VerifyPurchaseStatusCredited)
	connect.AssertEqual(t, env.state.ledger["tx-2"], networkId)

	// the DID_RENEW webhook for the next renewal resolves the same network
	transaction, err := validateAppleTransactionBound(
		context.Background(),
		AppleNotificationDecodedPayload{
			SignedDate:      offerCodeTestNow.UnixMilli(),
			TransactionInfo: offerCodeTestClaims("orig-1", "tx-3", time.Minute),
		},
		[]string{offerCodeTestProductId},
		true,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, transaction.networkId, networkId)

	// a webhook for a subscription that was never bound stays invalid
	_, err = validateAppleTransactionBound(
		context.Background(),
		AppleNotificationDecodedPayload{
			SignedDate:      offerCodeTestNow.UnixMilli(),
			TransactionInfo: offerCodeTestClaims("orig-unbound", "tx-9", time.Minute),
		},
		[]string{offerCodeTestProductId},
		true,
	)
	connect.AssertEqual(t, err, errors.New("invalid App Store account token"))
}

// Every refusal answers `invalid` and persists nothing (no binding, no
// redemption, no credit).
func TestVerifyAppleOfferCodeRefusals(t *testing.T) {
	type testCase struct {
		name    string
		prepare func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any)
	}
	cases := []testCase{
		{"never issued an offer", func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any) {
			clientSession, _ := env.addNetwork(false, 0)
			return clientSession, offerCodeTestClaims("orig-1", "tx-1", time.Hour)
		}},
		{"offer issued without an App Store code", func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any) {
			clientSession, networkId := env.addNetwork(true, 24*time.Hour)
			offer := env.state.offers[networkId]
			offer.AppleOfferCode = nil
			env.state.offers[networkId] = offer
			return clientSession, offerCodeTestClaims("orig-1", "tx-1", time.Hour)
		}},
		{"offer already redeemed", func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any) {
			clientSession, networkId := env.addNetwork(true, 24*time.Hour)
			offer := env.state.offers[networkId]
			redeemedAt := offerCodeTestNow.Add(-2 * time.Hour)
			store := model.OnboardingStoreStripe
			offer.RedeemedAt = &redeemedAt
			offer.Store = &store
			env.state.offers[networkId] = offer
			return clientSession, offerCodeTestClaims("orig-1", "tx-1", time.Hour)
		}},
		{"offer expired before the purchase", func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any) {
			// issued 6 days ago, valid 5: expired a day ago
			clientSession, _ := env.addNetwork(true, 6*24*time.Hour)
			return clientSession, offerCodeTestClaims("orig-1", "tx-1", time.Hour)
		}},
		{"purchase before the offer was issued", func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any) {
			clientSession, _ := env.addNetwork(true, time.Hour)
			return clientSession, offerCodeTestClaims("orig-1", "tx-1", 3*time.Hour)
		}},
		{"subscription bound to another network", func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any) {
			_, otherNetworkId := env.addNetwork(true, 24*time.Hour)
			env.state.bindings["orig-1"] = otherNetworkId
			clientSession, _ := env.addNetwork(true, 24*time.Hour)
			return clientSession, offerCodeTestClaims("orig-1", "tx-1", time.Hour)
		}},
		{"not an offer-code purchase", func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any) {
			clientSession, _ := env.addNetwork(true, 24*time.Hour)
			claims := offerCodeTestClaims("orig-1", "tx-1", time.Hour)
			claims["offerType"] = float64(appleOfferTypeIntroductory)
			return clientSession, claims
		}},
		{"a different offer code", func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any) {
			clientSession, _ := env.addNetwork(true, 24*time.Hour)
			claims := offerCodeTestClaims("orig-1", "tx-1", time.Hour)
			claims["offerIdentifier"] = "someone-elses-offer"
			return clientSession, claims
		}},
		{"no original transaction id", func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any) {
			clientSession, _ := env.addNetwork(true, 24*time.Hour)
			claims := offerCodeTestClaims("", "tx-1", time.Hour)
			delete(claims, "originalTransactionId")
			return clientSession, claims
		}},
		{"transaction already credited elsewhere", func(env *offerCodeFakeEnv) (*session.ClientSession, map[string]any) {
			_, otherNetworkId := env.addNetwork(false, 0)
			env.state.ledger["tx-1"] = otherNetworkId
			clientSession, _ := env.addNetwork(true, 24*time.Hour)
			return clientSession, offerCodeTestClaims("orig-1", "tx-1", time.Hour)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := installOfferCodeFakes(t)
			clientSession, claims := c.prepare(env)
			before := env.state.clone()

			connect.AssertEqual(t, verifyOfferCode(t, claims, clientSession), VerifyPurchaseStatusInvalid)
			connect.AssertEqual(t, env.state.bindings, before.bindings)
			connect.AssertEqual(t, env.state.ledger, before.ledger)
			connect.AssertEqual(t, env.state.offers, before.offers)
			connect.AssertEqual(t, len(env.refreshed), 0)
		})
	}
}

// A present but malformed appAccountToken is never treated as an unbound
// redemption.
func TestVerifyAppleMalformedAccountTokenStaysInvalid(t *testing.T) {
	env := installOfferCodeFakes(t)
	clientSession, _ := env.addNetwork(true, 24*time.Hour)
	claims := offerCodeTestClaims("orig-1", "tx-1", time.Hour)
	claims["appAccountToken"] = "not-a-uuid"
	connect.AssertEqual(t, verifyOfferCode(t, claims, clientSession), VerifyPurchaseStatusInvalid)
	connect.AssertEqual(t, len(env.state.bindings), 0)
	connect.AssertEqual(t, len(env.state.ledger), 0)
}
