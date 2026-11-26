//go:build sidecar_pending

package computet3

// The checkout's ledger half on the private stack's PostgreSQL and Redis (BAL-M12-08; DESIGN-COMPUTE §9.5, §9.11,
// §14.3, §21.9, Q-BAL-13): sales closed for a network nothing can spend credit in, and open where a service says it
// may; a purchase past the network's new-account day or the $10,000 cap refused before any session; 3-D Secure asked
// of the network's first purchase above $50 alone; the card saved only where threshold top-up is turned on; and the
// compute terms recorded with a purchase once. A stand-in for the host's Stripe takes the sessions; Stripe's own
// requests are server/controller's compute_stripe_test.go.

import (
	"context"
	"sync"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// A service that can spend credit for the networks a test opens.
type computeT3Spendable struct {
	stateLock  sync.Mutex
	networkIds map[operator.Id]bool
}

func (self *computeT3Spendable) Spendable(env operator.Env, ctx context.Context, networkId operator.Id) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.networkIds[networkId], nil
}

func (self *computeT3Spendable) open(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networkIds[networkId] = true
}

// Auto-renew's taker of saved cards, which takes none here: its registration is what lets threshold check out.
type computeT3Cards struct{}

func (self computeT3Cards) CardSaved(env operator.Env, ctx context.Context, saved *controller.ComputeCardSaved) error {
	return nil
}

var (
	computeT3SpendableOnce sync.Once
	computeT3SpendableSale = &computeT3Spendable{networkIds: map[operator.Id]bool{}}
)

// The host's Stripe, standing in: it records each session asked of it and answers one.
type computeT3Stripe struct {
	stateLock sync.Mutex
	requests  []*controller.ComputeStripeCheckoutRequest
}

func (self *computeT3Stripe) CreateCheckout(ctx context.Context, request *controller.ComputeStripeCheckoutRequest) (*controller.ComputeStripeCheckoutSession, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requests = append(self.requests, request)
	return &controller.ComputeStripeCheckoutSession{
		SessionId:  "cs_synthetic_t3",
		Url:        "https://checkout.stripe.com/c/pay/cs_synthetic_t3",
		ExpireTime: time.Now().Add(time.Hour),
	}, nil
}

func (self *computeT3Stripe) last() *controller.ComputeStripeCheckoutRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.requests) == 0 {
		return nil
	}
	return self.requests[len(self.requests)-1]
}

func (self *computeT3Stripe) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.requests)
}

// Opens a checkout, failing the test on an error.
func (self *computeT3Clocked) checkout(stripe *computeT3Stripe, request *controller.ComputeCheckoutRequest) *controller.ComputeCheckoutAnswer {
	self.t3.t.Helper()
	answer, err := controller.ComputeCheckout(self.env, self.t3.ctx, self.settings, computeT3Secrets{}, stripe, request)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return answer
}

func newComputeT3Checkout(t3 *computeT3) (*computeT3Clocked, *computeT3Stripe) {
	computeT3SpendableOnce.Do(func() {
		controller.RegisterComputeSpendableService("computet3_test_spendable", computeT3SpendableSale)
		controller.RegisterComputeAutoRenewCards(computeT3Cards{})
	})
	return newComputeT3Clocked(t3, time.Now().UTC()), &computeT3Stripe{}
}

// A network nothing can spend credit in is refused sales_closed, with no session; once a service says it may, the
// same checkout opens.
func TestComputeCheckoutT3ClosedUntilSalesOpen(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		clocked, stripe := newComputeT3Checkout(t3)
		networkId := server.NewId()
		request := &controller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: server.NewId(), PackCredits: 25}
		if answer := clocked.checkout(stripe, request); answer.Refusal != controller.ComputeCheckoutRefusalSalesClosed || stripe.count() != 0 {
			t3.t.Fatalf("closed sales: %+v, %d sessions", answer, stripe.count())
		}
		computeT3SpendableSale.open(networkId)
		answer := clocked.checkout(stripe, request)
		if answer.Refusal != "" || answer.Session == nil || stripe.count() != 1 {
			t3.t.Fatalf("open sales: %+v, %d sessions", answer, stripe.count())
		}
		made := stripe.last()
		if made.AmountNanoUsd != computeT3Usd(25) || made.PackCredits != 25 || made.SaveCard || made.AutoRenew != 0 || made.MemberId != request.MemberId {
			t3.t.Fatalf("the session asked: %+v", made)
		}
	})
}

// A purchase past the network's limits is refused before any session: the new account's $100 a UTC day, then the
// $10,000 cap.
func TestComputeCheckoutT3RefusedPastTheNetworksLimits(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		clocked, stripe := newComputeT3Checkout(t3)
		networkId := server.NewId()
		computeT3SpendableSale.open(networkId)
		clocked.purchase(networkId, computeT3Usd(90), nil)
		answer := clocked.checkout(stripe, &controller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: server.NewId(), PackCredits: 25})
		if answer.Refusal != controller.ComputeCheckoutRefusalDailyLimit || stripe.count() != 0 {
			t3.t.Fatalf("past the new account's day: %+v", answer)
		}
		// an established network near its cap
		capped := server.NewId()
		computeT3SpendableSale.open(capped)
		for i := 0; i < 7; i += 1 {
			clocked.purchase(capped, computeT3Usd(1_400), nil)
			clocked.clock.Advance(24 * time.Hour)
		}
		answer = clocked.checkout(stripe, &controller.ComputeCheckoutRequest{NetworkId: capped, MemberId: server.NewId(), PackCredits: 100})
		if answer.Refusal != "" {
			t3.t.Fatalf("an established network under its cap: %+v", answer)
		}
		clocked.purchase(capped, computeT3Usd(150), nil)
		answer = clocked.checkout(stripe, &controller.ComputeCheckoutRequest{NetworkId: capped, MemberId: server.NewId(), PackCredits: 100})
		if answer.Refusal != controller.ComputeCheckoutRefusalBalanceCap {
			t3.t.Fatalf("past the cap: %+v", answer)
		}
	})
}

// 3-D Secure is asked of the network's first purchase above $50 alone, and the card saved only where threshold
// top-up is turned on in the act.
func TestComputeCheckoutT3AsksThreeDSecureAndSavesCardsOnlyWhenTold(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		clocked, stripe := newComputeT3Checkout(t3)
		networkId := server.NewId()
		memberId := server.NewId()
		computeT3SpendableSale.open(networkId)
		for _, c := range []struct {
			pack         int64
			threshold    bool
			threeDSecure bool
		}{
			{pack: 100, threeDSecure: true},
			{pack: 50, threshold: true},
			{pack: 10},
		} {
			request := &controller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: c.pack}
			if c.threshold {
				request.AutoRenew = model.ComputeAutoRenewModeThreshold
			}
			if answer := clocked.checkout(stripe, request); answer.Refusal != "" {
				t3.t.Fatalf("pack %d: %+v", c.pack, answer)
			}
			made := stripe.last()
			if made.ThreeDSecure != c.threeDSecure || made.SaveCard != c.threshold {
				t3.t.Errorf("pack %d: 3-D Secure %t, card saved %t", c.pack, made.ThreeDSecure, made.SaveCard)
			}
		}
		clocked.purchase(networkId, computeT3Usd(10), nil)
		clocked.clock.Advance(clocked.settings.NewAccountWindow + 24*time.Hour)
		if answer := clocked.checkout(stripe, &controller.ComputeCheckoutRequest{NetworkId: networkId, MemberId: memberId, PackCredits: 100}); answer.Refusal != "" || stripe.last().ThreeDSecure {
			t3.t.Fatalf("a later purchase of $100: %+v, 3-D Secure %t", answer, stripe.last().ThreeDSecure)
		}
	})
}

// A purchase records the compute terms' version with its buyer once; a second records nothing.
func TestComputePurchaseTermsRecordedOnce(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		clocked, _ := newComputeT3Checkout(t3)
		networkId := server.NewId()
		buyer := server.NewId()
		written, err := controller.ComputeRecordPurchaseTerms(clocked.env, t3.ctx, networkId, buyer)
		if err != nil || !written {
			t3.t.Fatalf("the first purchase: %t %v", written, err)
		}
		clocked.clock.Advance(time.Hour)
		written, err = controller.ComputeRecordPurchaseTerms(clocked.env, t3.ctx, networkId, server.NewId())
		if err != nil || written {
			t3.t.Fatalf("a later purchase: %t %v", written, err)
		}
		if t3.queryInt(`SELECT count(*) FROM compute_terms WHERE network_id = $1 AND clause = 'compute' AND terms_version = $2 AND user_id = $3`, networkId, model.ComputeTermsVersion, buyer) != 1 {
			t3.t.Fatal("the terms record names another buyer or version")
		}
	})
}
