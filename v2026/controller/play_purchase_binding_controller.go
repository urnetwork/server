package controller

// Google Play purchases without an account link (UPGRADE.md A1, the Play half
// of the App Store offer-code binding in apple_offer_code_binding_controller.go).
// A purchase started in the app's billing flow carries the network as its
// obfuscatedExternalAccountId. A purchase made outside that flow -- a Play
// Store promo code redemption, a purchase from the Play Store listing or the
// subscriptions center -- carries no account identifiers, and the verify
// endpoint used to credit such a token to whichever session reported it first.
// It is now bound only through the welcome offer this server issued
// (model.IssueOnboardingOffer), with the same rules as the App Store:
//
//   - only through the authenticated verify endpoint, for the session network;
//   - only a purchase made under the welcome offer: a line item whose offer
//     tags carry the configured play_offer_tag (or the tag the network was
//     issued), or whose signupPromotion names the configured custom promotion
//     code (onboarding.offer.play_promotion_code; one-time codes carry no
//     identifier and are never matched);
//   - only when the network was issued a welcome offer with a Play offer tag,
//     has not redeemed it, and Google's startTime falls inside
//     [issued_at - 5 min, expires_at);
//   - only when the purchase token is not bound yet.
//
// The binding, the offer redemption and the credit (through the Play
// purchase-token gate, playCreditSubscriptionInTx) commit together or not at
// all. A token already bound to the session network -- or whose
// linkedPurchaseToken is -- is credited as a renewal of that subscription, in
// the verify endpoint, the RTDN webhook and the reconciler. Anything else is
// `invalid`; there is no binding by email or any other guess.
//
// There is no Play code pool like the App Store one: onboarding never issues
// Play codes (the welcome offer is bought in-app with its offer token, which
// sets the obfuscated account id), and subscriptionsv2 reports a one-time
// promo code without any identifier, so an issued code could not be matched
// to its redemption anyway.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// playOfferIssueSkew allows for clock skew between Google's startTime and our
// issued_at.
const playOfferIssueSkew = 5 * time.Minute

// playUnlinkedPurchase is a verified ACTIVE subscriptionsv2 purchase with no
// account identifiers, as reported to the verify endpoint.
type playUnlinkedPurchase struct {
	packageName   string
	purchaseToken string
	sub           *PlaySubscription
	startTime     time.Time
	maxExpiryTime time.Time
}

// playPurchaseBindingTx is the transactional state the binding rules read and
// write. The production implementation is one db transaction; tests use a fake.
type playPurchaseBindingTx interface {
	// BoundNetworkId is the network the token is bound to and its chain root
	BoundNetworkId(purchaseToken string) (networkId server.Id, rootPurchaseToken string, ok bool)
	// NetworkExists locks the network row for the rest of the tx
	NetworkExists(networkId server.Id) bool
	// OfferForUpdate reads the network's welcome offer and locks it
	OfferForUpdate(networkId server.Id) *model.OnboardingOffer
	// Bind records purchaseToken -> networkId; false if already bound
	Bind(purchaseToken string, rootPurchaseToken string, networkId server.Id, offer string) bool
	// RedeemOffer stamps the offer redeemed on play at `at`; false if it was
	// not eligible at `at`
	RedeemOffer(networkId server.Id, at time.Time) bool
	// Credit is playCreditSubscriptionInTx for networkId: false when a credit
	// for this expiry already landed
	Credit(purchase *playUnlinkedPurchase, networkId server.Id) (bool, error)
}

type playPurchaseBindingResult struct {
	status string
	// reason is why a report was refused (logged)
	reason string
	// commit is false when the tx must roll back (nothing may persist)
	commit bool
	err    error
}

func playPurchaseRefused(reason string) playPurchaseBindingResult {
	return playPurchaseBindingResult{status: VerifyPurchaseStatusInvalid, reason: reason}
}

// playWelcomeOfferMatch is what identifies the welcome offer in a purchase.
type playWelcomeOfferMatch struct {
	// offerTag is onboarding.offer.play_offer_tag
	offerTag string
	// promotionCode is onboarding.offer.play_promotion_code
	promotionCode string
}

// playPurchaseWelcomeOffer names the welcome offer the purchase was made
// under ("tag:<tag>" or "promotion:<code>"), or "" when it was not: a line
// item's offer tags carry the configured tag or the tag the network was
// issued, or its custom promotion code is the configured one.
func playPurchaseWelcomeOffer(sub *PlaySubscription, match playWelcomeOfferMatch, offer *model.OnboardingOffer) string {
	tags := []string{}
	if tag := strings.TrimSpace(match.offerTag); tag != "" {
		tags = append(tags, tag)
	}
	if offer != nil && offer.PlayOfferTag != nil {
		if tag := strings.TrimSpace(*offer.PlayOfferTag); tag != "" {
			tags = append(tags, tag)
		}
	}
	promotionCode := strings.TrimSpace(match.promotionCode)
	for _, item := range sub.LineItems {
		if item == nil {
			continue
		}
		if item.OfferDetails != nil {
			for _, itemTag := range item.OfferDetails.OfferTags {
				for _, tag := range tags {
					if strings.EqualFold(strings.TrimSpace(itemTag), tag) {
						return "tag:" + tag
					}
				}
			}
		}
		if promotionCode != "" && item.SignupPromotion != nil && item.SignupPromotion.VanityCode != nil {
			if strings.EqualFold(strings.TrimSpace(item.SignupPromotion.VanityCode.PromotionCode), promotionCode) {
				return "promotion:" + promotionCode
			}
		}
	}
	return ""
}

// playCreditBound credits a purchase already bound to the session network:
// the gate decides credited vs already_credited.
func playCreditBound(store playPurchaseBindingTx, purchase *playUnlinkedPurchase, networkId server.Id) playPurchaseBindingResult {
	credited, err := store.Credit(purchase, networkId)
	if err != nil {
		return playPurchaseBindingResult{err: err}
	}
	if credited {
		return playPurchaseBindingResult{status: VerifyPurchaseStatusCredited, commit: true}
	}
	return playPurchaseBindingResult{status: VerifyPurchaseStatusAlreadyCredited, commit: true}
}

// playBindUnlinkedPurchase applies the binding rules to a verified ACTIVE
// purchase that carries no account identifiers, reported by sessionNetworkId.
func playBindUnlinkedPurchase(
	store playPurchaseBindingTx,
	purchase *playUnlinkedPurchase,
	sessionNetworkId server.Id,
	match playWelcomeOfferMatch,
) playPurchaseBindingResult {
	if purchase.purchaseToken == "" || purchase.sub == nil {
		return playPurchaseRefused("no purchase token")
	}
	if !store.NetworkExists(sessionNetworkId) {
		return playPurchaseBindingResult{err: errors.New("session network does not exist")}
	}

	if boundNetworkId, _, ok := store.BoundNetworkId(purchase.purchaseToken); ok {
		if boundNetworkId != sessionNetworkId {
			return playPurchaseRefused("purchase is bound to another network")
		}
		// a renewal (or a repeated report) of a purchase bound to this network
		return playCreditBound(store, purchase, sessionNetworkId)
	}

	linkedPurchaseToken := purchase.sub.LinkedPurchaseToken
	if linkedPurchaseToken != "" && linkedPurchaseToken != purchase.purchaseToken {
		if boundNetworkId, rootPurchaseToken, ok := store.BoundNetworkId(linkedPurchaseToken); ok {
			if boundNetworkId != sessionNetworkId {
				return playPurchaseRefused("linked purchase is bound to another network")
			}
			// the same subscription continued under a new token (re-signup,
			// plan change): it inherits the binding
			if !store.Bind(purchase.purchaseToken, rootPurchaseToken, sessionNetworkId, "") {
				return playPurchaseRefused("purchase is already bound")
			}
			return playCreditBound(store, purchase, sessionNetworkId)
		}
	}

	offer := store.OfferForUpdate(sessionNetworkId)
	if offer == nil {
		return playPurchaseRefused("network was never issued a welcome offer")
	}
	if offer.PlayOfferTag == nil || strings.TrimSpace(*offer.PlayOfferTag) == "" {
		return playPurchaseRefused("network was never issued a Play welcome offer")
	}
	welcomeOffer := playPurchaseWelcomeOffer(purchase.sub, match, offer)
	if welcomeOffer == "" {
		return playPurchaseRefused("purchase is not the welcome offer")
	}
	if offer.RedeemedAt != nil {
		return playPurchaseRefused("welcome offer already redeemed")
	}
	// the purchase must fall inside the offer's window: Google's startTime,
	// not the time of the report (a report can be retried days later)
	if !purchase.startTime.Before(offer.ExpiresAt) {
		return playPurchaseRefused("welcome offer expired before the purchase")
	}
	if purchase.startTime.Before(offer.IssuedAt.Add(-playOfferIssueSkew)) {
		return playPurchaseRefused("purchase predates the welcome offer")
	}

	if !store.Bind(purchase.purchaseToken, purchase.purchaseToken, sessionNetworkId, welcomeOffer) {
		return playPurchaseRefused("purchase is already bound")
	}
	if !store.RedeemOffer(sessionNetworkId, purchase.startTime) {
		// roll back the binding
		return playPurchaseRefused("welcome offer could not be redeemed")
	}
	credited, err := store.Credit(purchase, sessionNetworkId)
	if err != nil {
		return playPurchaseBindingResult{err: err}
	}
	if !credited {
		// the token is already credited under some network: it was never an
		// unlinked purchase of this offer; roll everything back
		return playPurchaseRefused("purchase already credited")
	}
	return playPurchaseBindingResult{status: VerifyPurchaseStatusCredited, commit: true}
}

// ----- db implementation -----

type playPurchaseBindingDbTx struct {
	tx            server.PgTx
	clientSession *session.ClientSession
}

func (self *playPurchaseBindingDbTx) BoundNetworkId(purchaseToken string) (server.Id, string, bool) {
	return model.GetPlayPurchaseBindingInTx(self.tx, self.clientSession.Ctx, purchaseToken)
}

func (self *playPurchaseBindingDbTx) NetworkExists(networkId server.Id) bool {
	return appleNetworkExistsInTx(self.tx, self.clientSession.Ctx, networkId)
}

func (self *playPurchaseBindingDbTx) OfferForUpdate(networkId server.Id) *model.OnboardingOffer {
	return model.GetOnboardingOfferForUpdateInTx(self.tx, self.clientSession.Ctx, networkId)
}

func (self *playPurchaseBindingDbTx) Bind(purchaseToken string, rootPurchaseToken string, networkId server.Id, offer string) bool {
	return model.BindPlayPurchaseInTx(self.tx, self.clientSession.Ctx, purchaseToken, rootPurchaseToken, networkId, offer)
}

func (self *playPurchaseBindingDbTx) RedeemOffer(networkId server.Id, at time.Time) bool {
	return model.RedeemOnboardingOfferInTx(self.tx, self.clientSession.Ctx, networkId, model.OnboardingStorePlay, at)
}

func (self *playPurchaseBindingDbTx) Credit(purchase *playUnlinkedPurchase, networkId server.Id) (bool, error) {
	if len(purchase.sub.LineItems) == 0 {
		return false, errors.New("Google play subscription with zero line items")
	}
	skuName := purchase.sub.LineItems[0].ProductId
	sku, ok := playSkusFunc()[skuName]
	if !ok {
		return false, errors.New("Play sku not found: " + skuName)
	}
	return playCreditSubscriptionInTx(
		self.tx,
		self.clientSession,
		&PlaySubscriptionRenewalArgs{
			NetworkId:      networkId,
			PackageName:    purchase.packageName,
			SubscriptionId: skuName,
			PurchaseToken:  purchase.purchaseToken,
		},
		purchase.sub,
		sku,
		purchase.startTime,
		purchase.maxExpiryTime,
	)
}

type playPurchaseBindingRollback struct{}

// playPurchaseBindingRunFunc runs the rules in one ReadCommitted transaction
// (the credit gate's lock-then-recheck needs per-statement snapshots), rolled
// back unless the result says commit. Tests replace it with a fake store.
var playPurchaseBindingRunFunc = func(
	clientSession *session.ClientSession,
	callback func(store playPurchaseBindingTx) playPurchaseBindingResult,
) (result playPurchaseBindingResult) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(playPurchaseBindingRollback); ok {
				return
			}
			panic(r)
		}
	}()
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		result = callback(&playPurchaseBindingDbTx{tx: tx, clientSession: clientSession})
		if !result.commit {
			// a panic is the tx helper's rollback path
			panic(playPurchaseBindingRollback{})
		}
	}, server.TxReadCommitted)
	return
}

var playWelcomeOfferMatchFunc = func() playWelcomeOfferMatch {
	offer := model.Onboarding().Offer
	return playWelcomeOfferMatch{
		offerTag:      offer.PlayOfferTag,
		promotionCode: offer.PlayPromotionCode,
	}
}

var playPurchaseBindingProRefreshFunc = func(ctx context.Context, networkId server.Id) {
	model.UpdateProNetwork(ctx, networkId)
}

// verifyPlayUnlinkedPurchase is the verify endpoint's path for an ACTIVE
// purchase without account identifiers.
func verifyPlayUnlinkedPurchase(
	clientSession *session.ClientSession,
	packageName string,
	purchaseToken string,
	sub *PlaySubscription,
) (*VerifyStorePurchaseResult, error) {
	sessionNetworkId := clientSession.ByJwt.NetworkId
	startTime, err := sub.ParseStartTime()
	if err != nil {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	maxExpiryTime, minExpiryTime, err := playLineItemExpiryRange(sub)
	if err != nil {
		return NewVerifyStorePurchaseInvalid(), nil
	}
	purchase := &playUnlinkedPurchase{
		packageName:   packageName,
		purchaseToken: purchaseToken,
		sub:           sub,
		startTime:     startTime,
		maxExpiryTime: maxExpiryTime,
	}
	match := playWelcomeOfferMatchFunc()
	result := playPurchaseBindingRunFunc(clientSession, func(store playPurchaseBindingTx) playPurchaseBindingResult {
		return playBindUnlinkedPurchase(store, purchase, sessionNetworkId, match)
	})
	if result.err != nil {
		return nil, result.err
	}
	if result.status == VerifyPurchaseStatusInvalid || result.status == "" {
		glog.Infof(
			"[sub]verify play unlinked purchase for network %s refused: %s\n",
			sessionNetworkId,
			result.reason,
		)
		return NewVerifyStorePurchaseInvalid(), nil
	}
	if result.status == VerifyPurchaseStatusCredited {
		glog.Infof("[sub]verify play unlinked purchase credited to network %s\n", sessionNetworkId)
		playPurchaseBindingProRefreshFunc(clientSession.Ctx, sessionNetworkId)
	}
	return &VerifyStorePurchaseResult{Status: result.status, ExpiryTime: &minExpiryTime}, nil
}

// playLineItemExpiryRange is the latest and earliest line item expiry.
func playLineItemExpiryRange(sub *PlaySubscription) (maxExpiryTime time.Time, minExpiryTime time.Time, err error) {
	if len(sub.LineItems) == 0 {
		err = errors.New("no line items")
		return
	}
	for i, item := range sub.LineItems {
		if item == nil {
			err = errors.New("empty line item")
			return
		}
		var expiryTime time.Time
		expiryTime, err = item.ParseExpiryTime()
		if err != nil {
			return
		}
		if i == 0 || maxExpiryTime.Before(expiryTime) {
			maxExpiryTime = expiryTime
		}
		if i == 0 || expiryTime.Before(minExpiryTime) {
			minExpiryTime = expiryTime
		}
	}
	return
}

// playPurchaseBindingLookupFunc resolves an unlinked token through its binding
// or its linkedPurchaseToken's (model.ResolvePlayPurchaseBinding). Tests
// replace it.
var playPurchaseBindingLookupFunc = func(
	connOwner server.PgConn,
	ctx context.Context,
	purchaseToken string,
	linkedPurchaseToken string,
	inherit bool,
) (server.Id, bool) {
	return model.ResolvePlayPurchaseBindingInConn(connOwner, ctx, purchaseToken, linkedPurchaseToken, inherit)
}

// playResolveNetworkId is the network a purchase belongs to, for the paths
// that cannot bind (the RTDN webhook, the reconciler): the account link when
// present, else the binding. ok=false when the link is present but malformed;
// a nil network (with ok) when the purchase is unlinked and was never bound.
func playResolveNetworkId(
	clientSession *session.ClientSession,
	sub *PlaySubscription,
	purchaseToken string,
	inherit bool,
) (*server.Id, bool) {
	return playResolveNetworkIdInConn(nil, clientSession, sub, purchaseToken, inherit)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func playResolveNetworkIdInConn(connOwner server.PgConn,
	clientSession *session.ClientSession,
	sub *PlaySubscription,
	purchaseToken string,
	inherit bool,
) (*server.Id, bool) {
	linkedNetworkId, ok := playLinkedNetworkIdInConn(connOwner, clientSession, sub)
	if !ok {
		return nil, false
	}
	if linkedNetworkId != nil {
		return linkedNetworkId, true
	}
	if networkId, bound := playPurchaseBindingLookupFunc(connOwner, clientSession.Ctx, purchaseToken, sub.LinkedPurchaseToken, inherit); bound {
		return &networkId, true
	}
	return nil, true
}
