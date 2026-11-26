//go:build sidecar_pending

package computet3

// Lots on the private stack's PostgreSQL and Redis (BAL-M12-07; DESIGN-COMPUTE §9.4, §9.5, §9.11, §9.12): a lot credited
// once by its market reference, an overrun drawn from the next lot credited with its draws written, a promotion paying
// none, a chargeback reversing its lot, suspending the network's compute and lapsing its promotions, a store's refund
// reversing its lot while another lot pays what it had paid for, a store purchase past its limits credited and
// flagged, and a Play purchase counting to its network alone.

import (
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
)

// The ledger's keys for these tests: synthetic, test only.
type computeT3Secrets struct{}

func (self computeT3Secrets) PayerHashKey() ([]byte, error) {
	return []byte("synthetic computet3 payer hash key, test only"), nil
}

func (self computeT3Secrets) PromoHashKey() ([]byte, error) {
	return []byte("synthetic computet3 promo hash key, test only"), nil
}

// The test's database and Redis with a fake clock and metrics the test reads.
type computeT3Clocked struct {
	t3       *computeT3
	env      operator.Env
	clock    *sidecartest.FakeClock
	metrics  *sidecartest.FakeMetrics
	settings *model.ComputeLedgerSettings
}

func newComputeT3Clocked(t3 *computeT3, now time.Time) *computeT3Clocked {
	clock := sidecartest.NewFakeClock(now)
	metrics := sidecartest.NewFakeMetrics(nil)
	env, err := core.NewEnv(&sidecar.ServiceHost{Redis: t3.redis, Clock: clock, Metrics: metrics}, &core.Grants{Db: true})
	if err != nil {
		t3.t.Fatal(err)
	}
	return &computeT3Clocked{t3: t3, env: env, clock: clock, metrics: metrics, settings: model.DefaultComputeLedgerSettings()}
}

// A payer's keyed hash of a synthetic card's fingerprint.
func (self *computeT3Clocked) payer(fingerprint string) []byte {
	self.t3.t.Helper()
	hash, err := controller.ComputePayerHash(computeT3Secrets{}, controller.ComputePayerStripeCard, fingerprint)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return hash
}

// Credits a lot, failing the test on an error.
func (self *computeT3Clocked) credit(request *controller.ComputeCreditRequest) *controller.ComputeCreditAnswer {
	self.t3.t.Helper()
	answer, err := controller.ComputeCreditLot(self.env, self.t3.ctx, self.settings, request)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return answer
}

// A Stripe purchase of amount, by payer where given.
func (self *computeT3Clocked) purchase(networkId server.Id, amount int64, payerHash []byte) *controller.ComputeCreditAnswer {
	self.t3.t.Helper()
	return self.credit(&controller.ComputeCreditRequest{
		NetworkId: networkId, Kind: model.ComputeLotKindPurchase, Market: model.ComputeMarketStripe,
		MarketRef: "pi_test_" + server.NewId().String(), AmountNanoUsd: amount, PayerHash: payerHash,
	})
}

// A promotion of amount, lapsing at the settings' 30 days.
func (self *computeT3Clocked) promotion(networkId server.Id, amount int64) *controller.ComputeCreditAnswer {
	self.t3.t.Helper()
	expire := self.clock.Now().Add(self.settings.PromoLapse)
	return self.credit(&controller.ComputeCreditRequest{
		NetworkId: networkId, Kind: model.ComputeLotKindPromotion, Market: model.ComputeMarketPromotion,
		MarketRef: server.NewId().String(), AmountNanoUsd: amount, ExpireTime: &expire,
	})
}

// Reverses a lot, failing the test on an error.
func (self *computeT3Clocked) reverse(market model.ComputeMarket, marketRef string, chargeback bool) *controller.ComputeReverseAnswer {
	self.t3.t.Helper()
	answer, err := controller.ComputeReverseLot(self.env, self.t3.ctx, self.settings, &controller.ComputeReverseRequest{Market: market, MarketRef: marketRef, Chargeback: chargeback})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return answer
}

// A lot's remaining.
func (self *computeT3Clocked) remaining(lotId server.Id) int64 {
	self.t3.t.Helper()
	return self.t3.queryInt(`SELECT remaining_nano_usd FROM compute_lot WHERE lot_id = $1`, lotId)
}

// A webhook replayed twice credits once: the replay answers the first delivery's lot, and the account moved once.
func TestComputeLotCreditedOnceByMarketRef(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, time.Now().UTC())
		networkId := server.NewId()
		request := &controller.ComputeCreditRequest{
			NetworkId: networkId, Kind: model.ComputeLotKindPurchase, Market: model.ComputeMarketStripe,
			MarketRef: "pi_test_replayed", AmountNanoUsd: computeT3Usd(25), PayerHash: clocked.payer("fp_test_replayed"),
		}
		first := clocked.credit(request)
		if !first.Credited || first.Lot.RemainingNanoUsd != computeT3Usd(25) || first.Account.SettledNanoUsd != computeT3Usd(25) {
			t.Fatalf("the first delivery: %+v", first)
		}
		for i := 0; i < 2; i += 1 {
			again := clocked.credit(request)
			if again.Credited || again.Lot.LotId != first.Lot.LotId {
				t.Fatalf("a replay: %+v", again)
			}
		}
		if t3.queryInt(`SELECT count(*) FROM compute_lot WHERE network_id = $1`, networkId) != 1 ||
			t3.queryInt(`SELECT settled_nano_usd FROM compute_account WHERE network_id = $1`, networkId) != computeT3Usd(25) ||
			t3.queryInt(`SELECT version FROM compute_account WHERE network_id = $1`, networkId) != first.Account.Version {
			t.Error("a replay credited again")
		}
		// the payer counted the purchase once
		if t3.queryInt(`SELECT amount_nano_usd FROM compute_payer_day WHERE payer_hash = $1`, request.PayerHash) != computeT3Usd(25) {
			t.Error("the payer's day counts a replay")
		}
		// the same reference in another network is no credit of it
		other := *request
		other.NetworkId = server.NewId()
		if _, err := controller.ComputeCreditLot(clocked.env, t3.ctx, clocked.settings, &other); err == nil {
			t.Error("another network's reference credited")
		}
	})
}

// A charge that overran its network's lots leaves settled below zero; the next lot credited pays it at once, its draw
// written for the charge after the charge's own, so the lots' remaining and draws still sum to what was credited. A
// promotion credited on an overrun pays none of it.
func TestComputeOverrunDrawnFromTheNextLot(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		clocked := newComputeT3Clocked(t3, time.Now().UTC())
		firstLot := clocked.purchase(ledger.networkId, computeT3Usd(1), nil).Lot
		chargeId := ledger.settledRow(model.ComputeChargeKindInference, computeT3Usd(1.50), time.Now().UTC().Add(-time.Minute))
		ledger.book()
		if settled := ledger.account().SettledNanoUsd; settled != -computeT3Usd(0.50) {
			t.Fatalf("settled after the overrun: %d", settled)
		}
		ledger.rebuildFromLedger()
		// a promotion leaves the overrun where it stands
		promotion := clocked.promotion(ledger.networkId, computeT3Usd(5))
		if promotion.OverrunDrawnNanoUsd != 0 || clocked.remaining(promotion.Lot.LotId) != computeT3Usd(5) || promotion.Account.SettledNanoUsd != computeT3Usd(4.50) {
			t.Fatalf("a promotion on the overrun: %+v", promotion)
		}
		next := clocked.purchase(ledger.networkId, computeT3Usd(10), nil)
		if next.OverrunDrawnNanoUsd != computeT3Usd(0.50) || clocked.remaining(next.Lot.LotId) != computeT3Usd(9.50) {
			t.Fatalf("the next lot: drawn %d, remaining %d", next.OverrunDrawnNanoUsd, clocked.remaining(next.Lot.LotId))
		}
		if t3.queryInt(`SELECT amount_nano_usd FROM compute_charge_lot WHERE charge_id = $1 AND draw_index = 0 AND lot_id = $2`, chargeId, firstLot.LotId) != computeT3Usd(1) ||
			t3.queryInt(`SELECT amount_nano_usd FROM compute_charge_lot WHERE charge_id = $1 AND draw_index = 1 AND lot_id = $2`, chargeId, next.Lot.LotId) != computeT3Usd(0.50) {
			t.Error("the charge's draws are not its first lot's dollar and the next lot's fifty cents")
		}
		account := ledger.account()
		credited := t3.queryInt(`SELECT sum(amount_nano_usd)::bigint FROM compute_lot WHERE network_id = $1`, ledger.networkId)
		remaining := t3.queryInt(`SELECT sum(remaining_nano_usd)::bigint FROM compute_lot WHERE network_id = $1`, ledger.networkId)
		drawn := t3.queryInt(`SELECT coalesce(sum(d.amount_nano_usd), 0)::bigint FROM compute_charge_lot d JOIN compute_lot l USING (lot_id) WHERE l.network_id = $1`, ledger.networkId)
		charged := t3.queryInt(`SELECT sum(amount_nano_usd)::bigint FROM compute_charge WHERE network_id = $1`, ledger.networkId)
		if credited != remaining+drawn || account.SettledNanoUsd != remaining-(charged-drawn) || account.SettledNanoUsd != computeT3Usd(14.50) {
			t.Errorf("credited %d, remaining %d, drawn %d, charged %d, settled %d", credited, remaining, drawn, charged, account.SettledNanoUsd)
		}
		// the projection took the account through the version guard
		if projection := ledger.read(); !next.Projected || projection.SettledNanoUsd != account.SettledNanoUsd {
			t.Errorf("the projection reads %d, the account %d", projection.SettledNanoUsd, account.SettledNanoUsd)
		}
	})
}

// A chargeback reverses its lot, what the lot paid for staying charged so settled goes negative, suspends the network's
// compute, which the projection then refuses calls by, and lapses its promotions at once; the reversal replayed changes
// nothing.
func TestComputeChargebackReversesSuspendsAndLapsesPromotions(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		clocked := newComputeT3Clocked(t3, time.Now().UTC())
		purchase := clocked.purchase(ledger.networkId, computeT3Usd(10), nil)
		clocked.promotion(ledger.networkId, computeT3Usd(5))
		// $7 draws the promotion's $5 first, then the purchase's $2
		ledger.settledRow(model.ComputeChargeKindInference, computeT3Usd(7), time.Now().UTC().Add(-time.Minute))
		ledger.book()
		later := clocked.promotion(ledger.networkId, computeT3Usd(3))
		ledger.rebuildFromLedger()
		if suspended, err := controller.ComputeChargebackSuspended(clocked.env, t3.ctx, ledger.networkId); err != nil || suspended {
			t.Fatalf("suspended before the chargeback: %v, %v", suspended, err)
		}
		answer := clocked.reverse(model.ComputeMarketStripe, purchase.Lot.MarketRef, true)
		if !answer.Reversed || answer.RemovedNanoUsd != computeT3Usd(8) || answer.OwedNanoUsd != computeT3Usd(2) || answer.LapsedNanoUsd != computeT3Usd(3) {
			t.Fatalf("the chargeback: %+v", answer)
		}
		account := ledger.account()
		if account.SettledNanoUsd != -computeT3Usd(2) || account.State != model.ComputeAccountStateSuspended {
			t.Fatalf("the account after the chargeback: settled %d, state %s", account.SettledNanoUsd, account.State)
		}
		if t3.queryInt(`SELECT coalesce(lapsed_nano_usd, 0) FROM compute_lot WHERE lot_id = $1`, later.Lot.LotId) != computeT3Usd(3) ||
			clocked.remaining(later.Lot.LotId) != 0 || clocked.remaining(purchase.Lot.LotId) != 0 ||
			t3.queryInt(`SELECT (reversed_time IS NOT NULL)::int FROM compute_lot WHERE lot_id = $1`, purchase.Lot.LotId) != 1 {
			t.Error("the lots after the chargeback")
		}
		if suspended, err := controller.ComputeChargebackSuspended(clocked.env, t3.ctx, ledger.networkId); err != nil || !suspended {
			t.Fatalf("not suspended after the chargeback: %v, %v", suspended, err)
		}
		// the projection holds the suspension, so a call is refused balance
		if projection := ledger.read(); projection.State != model.ComputeAccountStateSuspended || projection.SettledNanoUsd != -computeT3Usd(2) {
			t.Fatalf("the projection: %+v", projection)
		}
		if refused := ledger.holdCall(ledger.teeRequest(computeT3Usd(0.10))); refused.Admitted || refused.Refusal == nil || refused.Refusal.Reason != controller.ComputeRefusalBalance {
			t.Fatalf("a call after the chargeback: %+v", refused)
		}
		version := ledger.account().Version
		if again := clocked.reverse(model.ComputeMarketStripe, purchase.Lot.MarketRef, true); again.Reversed || ledger.account().Version != version {
			t.Errorf("a replayed chargeback: %+v", again)
		}
	})
}

// A store's refund reverses its lot and suspends nothing; what the lot had paid for is drawn from the network's other
// unreversed lot at once, so that lot's remaining says what the network has.
func TestComputeStoreRefundReversesAndAnotherLotPays(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		clocked := newComputeT3Clocked(t3, time.Now().UTC().Add(-time.Hour))
		store := clocked.credit(&controller.ComputeCreditRequest{
			NetworkId: ledger.networkId, Kind: model.ComputeLotKindPurchase, Market: model.ComputeMarketAppStore,
			MarketRef: "2000000000000001", AmountNanoUsd: computeT3Usd(10), Store: true,
		})
		clocked.clock.Advance(time.Minute)
		stripe := clocked.purchase(ledger.networkId, computeT3Usd(20), nil)
		chargeId := ledger.settledRow(model.ComputeChargeKindInference, computeT3Usd(4), time.Now().UTC().Add(-time.Minute))
		ledger.book()
		answer := clocked.reverse(model.ComputeMarketAppStore, "2000000000000001", false)
		if !answer.Reversed || answer.OwedNanoUsd != computeT3Usd(4) || answer.OverrunDrawnNanoUsd != computeT3Usd(4) || answer.LapsedNanoUsd != 0 {
			t.Fatalf("the refund: %+v", answer)
		}
		account := ledger.account()
		if account.SettledNanoUsd != computeT3Usd(16) || account.State != model.ComputeAccountStateActive || clocked.remaining(stripe.Lot.LotId) != computeT3Usd(16) {
			t.Fatalf("after the refund: settled %d, state %s, the other lot %d", account.SettledNanoUsd, account.State, clocked.remaining(stripe.Lot.LotId))
		}
		if t3.queryInt(`SELECT count(*) FROM compute_charge_lot WHERE charge_id = $1 AND lot_id = $2`, chargeId, stripe.Lot.LotId) != 1 ||
			t3.queryInt(`SELECT count(*) FROM compute_charge_lot WHERE charge_id = $1 AND lot_id = $2`, chargeId, store.Lot.LotId) != 1 {
			t.Error("the charge's draws do not name both lots")
		}
		if suspended, err := controller.ComputeChargebackSuspended(clocked.env, t3.ctx, ledger.networkId); err != nil || suspended {
			t.Errorf("a store's refund suspended the network: %v, %v", suspended, err)
		}
	})
}

// A store purchase past its payer's and its network's limits is credited, since the store took the money, and flagged
// for review, its alarm raised once; a replay flags nothing again.
func TestComputeStorePurchasePastLimitsCreditedAndFlagged(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, time.Now().UTC())
		networkId := server.NewId()
		payerHash, err := controller.ComputePayerHash(computeT3Secrets{}, controller.ComputePayerAppStoreAccount, "app-transaction-test-0001")
		if err != nil {
			t.Fatal(err)
		}
		request := &controller.ComputeCreditRequest{
			NetworkId: networkId, Kind: model.ComputeLotKindPurchase, Market: model.ComputeMarketAppStore,
			MarketRef: "2000000000000150", AmountNanoUsd: computeT3Usd(150), PayerHash: payerHash, Store: true,
		}
		answer := clocked.credit(request)
		if !answer.Credited || !answer.Flagged || answer.Account.SettledNanoUsd != computeT3Usd(150) {
			t.Fatalf("the store purchase: %+v", answer)
		}
		reasons := map[bool][]controller.ComputeLimitReason{}
		for _, breach := range answer.Breaches {
			reasons[breach.Payer] = append(reasons[breach.Payer], breach.Reason)
		}
		if len(reasons[false]) != 1 || reasons[false][0] != controller.ComputeLimitNewAccountDay ||
			len(reasons[true]) != 1 || reasons[true][0] != controller.ComputeLimitNewAccountDay {
			t.Fatalf("the limits passed: %+v", reasons)
		}
		if clocked.metrics.Value("urnetwork_compute_alarms_total", controller.ComputeAlarmPurchaseFlagged) != 1 {
			t.Error("the flag's alarm")
		}
		if again := clocked.credit(request); again.Credited || again.Flagged {
			t.Errorf("a replay: %+v", again)
		}
		if clocked.metrics.Value("urnetwork_compute_alarms_total", controller.ComputeAlarmPurchaseFlagged) != 1 {
			t.Error("a replay raised the alarm again")
		}
	})
}

// A Play purchase names no payer: it counts to its network's limits alone, and no payer row holds it.
func TestComputePlayCountsToTheNetworkAlone(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, time.Now().UTC())
		played, other := server.NewId(), server.NewId()
		answer := clocked.credit(&controller.ComputeCreditRequest{
			NetworkId: played, Kind: model.ComputeLotKindPurchase, Market: model.ComputeMarketGooglePlay,
			MarketRef: "GPA.3300-0000-0000-00001", AmountNanoUsd: computeT3Usd(80), Store: true,
		})
		if !answer.Credited || answer.Flagged {
			t.Fatalf("the Play purchase: %+v", answer)
		}
		if t3.queryInt(`SELECT count(*) FROM compute_payer`) != 0 || t3.queryInt(`SELECT count(*) FROM compute_payer_day`) != 0 {
			t.Error("a Play purchase counted to a payer")
		}
		check := func(networkId server.Id, amount int64, payerHash []byte) *controller.ComputePurchaseAnswer {
			answer, err := controller.ComputePurchaseCheck(clocked.env, t3.ctx, clocked.settings, computeT3Secrets{}, &controller.ComputePurchaseRequest{
				NetworkId: networkId, PayerHash: payerHash, AmountNanoUsd: amount,
			})
			if err != nil {
				t.Fatal(err)
			}
			return answer
		}
		// another network's card is no payer of the Play purchase
		if answer := check(other, computeT3Usd(80), clocked.payer("fp_test_play_other")); !answer.Allowed {
			t.Errorf("another network's purchase: %+v", answer.Breaches)
		}
		// the Play purchase's network has its $100 day less the $80
		answer2 := check(played, computeT3Usd(30), nil)
		if answer2.Allowed || len(answer2.Breaches) != 1 || answer2.Breaches[0].Reason != controller.ComputeLimitNewAccountDay || answer2.Breaches[0].Payer || answer2.Code() != "daily_limit" {
			t.Errorf("the Play network's next purchase: %+v", answer2.Breaches)
		}
	})
}
