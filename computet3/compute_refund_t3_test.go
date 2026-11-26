//go:build sidecar_pending

package computet3

// Refunds, adjustments, disputes and closure on the private stack's PostgreSQL and Redis (BAL-M12-14; DESIGN-COMPUTE
// §9.6, §9.9, §9.12, D-BAL-11, D-BAL-14; R2-08's acceptance), with a fake of the host's Stripe refunds: a dispute's
// refund racing an operator's, then retried past UTC midnight, returning the draws once at its first time; a
// self-serve refund for a Stripe lot's unspent part within 30 days alone, retried returning once; a refund netting
// into its service; an adjustment counting to the network's own line; disputes counted by network and capped, paging
// past three networks; and a closure refunding within 90 days and forfeiting the rest.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// The host's Stripe refunds standing in: each lot refunded once however often asked, every ask recorded.
type computeT3RefundFake struct {
	stateLock sync.Mutex
	asks      []*controller.ComputeStripeLotRefund
	refunded  map[operator.Id]*controller.ComputeStripeLotRefund
}

func (self *computeT3RefundFake) RefundLot(ctx context.Context, request *controller.ComputeStripeLotRefund) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.asks = append(self.asks, request)
	if _, ok := self.refunded[request.LotId]; !ok {
		self.refunded[request.LotId] = request
	}
	return nil
}

// The refund of lotId, nil where none was made, and how often it was asked.
func (self *computeT3RefundFake) of(lotId operator.Id) (*controller.ComputeStripeLotRefund, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	asked := 0
	for _, ask := range self.asks {
		if ask.LotId == lotId {
			asked += 1
		}
	}
	return self.refunded[lotId], asked
}

var (
	computeT3RefundsOnce sync.Once
	computeT3Refunds     = &computeT3RefundFake{refunded: map[operator.Id]*controller.ComputeStripeLotRefund{}}
)

// A network for refunds, its fakes registered.
func newComputeT3Refunding(t3 *computeT3) *computeT3Renewing {
	computeT3RefundsOnce.Do(func() {
		controller.RegisterComputeStripeRefunds(computeT3Refunds)
	})
	return newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
}

// A lot of kind and market credited now, its reference new.
func (self *computeT3Renewing) lot(kind model.ComputeLotKind, market model.ComputeMarket, amount int64) *model.ComputeLot {
	self.t3.t.Helper()
	request := &controller.ComputeCreditRequest{
		NetworkId: self.networkId, Kind: kind, Market: market, MarketRef: "ref_t3_" + server.NewId().String(), AmountNanoUsd: amount,
	}
	if kind == model.ComputeLotKindPromotion {
		expire := self.clock.Now().Add(self.settings.PromoLapse)
		request.ExpireTime = &expire
	}
	if market == model.ComputeMarketAppStore || market == model.ComputeMarketGooglePlay {
		request.Store = true
	}
	return self.credit(request).Lot
}

// A charge of amount booked now: its id.
func (self *computeT3Renewing) charge(amount int64) server.Id {
	self.t3.t.Helper()
	before := self.t3.queryInt(`SELECT count(*) FROM compute_charge WHERE network_id = $1`, self.networkId)
	self.spend(amount)
	if self.t3.queryInt(`SELECT count(*) FROM compute_charge WHERE network_id = $1`, self.networkId) != before+1 {
		self.t3.t.Fatal("the charge was not booked")
	}
	var chargeId server.Id
	err := self.t3.db.Db(self.t3.ctx, func(conn operator.DbConn) error {
		rows, err := conn.Query(self.t3.ctx, `SELECT charge_id FROM compute_charge WHERE network_id = $1 AND amount_nano_usd = $2 ORDER BY settle_time DESC LIMIT 1`, self.networkId, amount)
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			return rows.Scan(&chargeId)
		}
		return rows.Err()
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return chargeId
}

// The subject, the call's request id, of chargeId.
func (self *computeT3Renewing) subjectOf(chargeId server.Id) server.Id {
	self.t3.t.Helper()
	var subjectId server.Id
	err := self.t3.db.Db(self.t3.ctx, func(conn operator.DbConn) error {
		rows, err := conn.Query(self.t3.ctx, `SELECT subject_ref FROM compute_charge WHERE charge_id = $1`, chargeId)
		if err != nil {
			return err
		}
		defer rows.Close()
		if !rows.Next() {
			return errors.New("no such charge")
		}
		return rows.Scan(&subjectId)
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return subjectId
}

// One booking run of the network's shard.
func (self *computeT3Renewing) book() {
	self.t3.t.Helper()
	if _, err := controller.ComputeBook(self.env, self.t3.ctx, self.settings, model.ComputeOpShard(self.settings, self.networkId)); err != nil {
		self.t3.t.Fatal(err)
	}
}

// What the draws of chargeId sum to, a refund's negative.
func (self *computeT3Renewing) drawn(chargeId server.Id) int64 {
	self.t3.t.Helper()
	return self.t3.queryInt(`SELECT coalesce(sum(amount_nano_usd), 0)::bigint FROM compute_charge_lot WHERE charge_id = $1`, chargeId)
}

// A dispute's refund racing an operator's books once; retried past UTC midnight, it meets its claim, books nothing
// more and stays counted to the UTC day it was first written, so no closed day reopens.
func TestRefundRacedAndRetriedPastMidnightReturnsTheDrawsOnce(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		refunding := newComputeT3Refunding(t3)
		promo := refunding.lot(model.ComputeLotKindPromotion, model.ComputeMarketPromotion, computeT3Usd(2))
		chargeId := refunding.charge(computeT3Usd(5))
		requestId := refunding.subjectOf(chargeId)
		if _, err := controller.ComputeDispute(refunding.env, t3.ctx, refunding.settings, &controller.ComputeDisputeRequest{
			NetworkId: refunding.networkId, RequestId: requestId, Provider: 1, ChargeId: &chargeId,
			DeviceUnits: model.ComputeUnits{InputTokens: 10}, ProviderUnits: model.ComputeUnits{InputTokens: 1000, OutputTokens: 100},
		}); err != nil {
			t.Fatal(err)
		}
		// a dispute of another request naming this charge is upheld and refunds nothing
		otherId := server.NewId()
		if _, err := controller.ComputeDispute(refunding.env, t3.ctx, refunding.settings, &controller.ComputeDisputeRequest{
			NetworkId: refunding.networkId, RequestId: otherId, Provider: 1, ChargeId: &chargeId,
			DeviceUnits: model.ComputeUnits{InputTokens: 10}, ProviderUnits: model.ComputeUnits{InputTokens: 1000},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.ComputeDecideDispute(refunding.env, t3.ctx, refunding.settings, otherId, true); !errors.Is(err, controller.ErrComputeRefundArgs) {
			t.Fatalf("another request's dispute of the charge: %v", err)
		}
		settledBefore := refunding.settled()
		start := make(chan struct{})
		var racing sync.WaitGroup
		for i := 0; i < 2; i += 1 {
			racing.Add(1)
			go func(dispute bool) {
				defer racing.Done()
				<-start
				var err error
				if dispute {
					_, err = controller.ComputeDecideDispute(refunding.env, t3.ctx, refunding.settings, requestId, true)
				} else {
					_, err = controller.ComputeRefundCharge(refunding.env, t3.ctx, refunding.settings, &controller.ComputeRefundChargeRequest{ChargeId: chargeId})
				}
				if err != nil {
					t.Error(err)
				}
			}(i == 0)
		}
		close(start)
		racing.Wait()
		refunding.book()
		refundKey := controller.ComputeOpKeyForRefund(chargeId)
		if t3.queryInt(`SELECT count(*) FROM compute_charge WHERE charge_id = $1`, refundKey) != 1 || refunding.drawn(refundKey) != -computeT3Usd(5) {
			t.Fatalf("the race booked %d refunds drawing %d", t3.queryInt(`SELECT count(*) FROM compute_charge WHERE charge_id = $1`, refundKey), refunding.drawn(refundKey))
		}
		if refunding.settled() != settledBefore+computeT3Usd(5) {
			t.Fatalf("settled moved from %d to %d", settledBefore, refunding.settled())
		}
		if remaining := refunding.remaining(promo.LotId); remaining != computeT3Usd(2) {
			t.Fatalf("the promotion got back %d", remaining)
		}
		firstDay := t3.queryInt(`SELECT extract(epoch FROM (start_time AT TIME ZONE 'UTC')::date)::bigint FROM compute_charge WHERE charge_id = $1`, refundKey)
		refunding.clock.AdvanceTo(model.ComputeUtcDay(refunding.clock.Now()).AddDate(0, 0, 1).Add(time.Minute))
		again, err := controller.ComputeRefundCharge(refunding.env, t3.ctx, refunding.settings, &controller.ComputeRefundChargeRequest{ChargeId: chargeId})
		if err != nil || again.Write.Written {
			t.Fatalf("the retry past midnight: %+v %v", again, err)
		}
		refunding.book()
		if t3.queryInt(`SELECT count(*) FROM compute_charge WHERE charge_id = $1`, refundKey) != 1 || refunding.settled() != settledBefore+computeT3Usd(5) ||
			t3.queryInt(`SELECT extract(epoch FROM (start_time AT TIME ZONE 'UTC')::date)::bigint FROM compute_charge WHERE charge_id = $1`, refundKey) != firstDay {
			t.Fatal("the retry past midnight booked again or moved its day")
		}
		if t3.queryInt(`SELECT coalesce(sum(refunded_nano_usd), 0)::bigint FROM compute_charge_day WHERE network_id = $1`, refunding.networkId) != computeT3Usd(5) {
			t.Fatal("the day rows count the refund twice")
		}
	})
}

// A self-serve refund returns a Stripe lot's unspent whole cents within its 30 days; a store's lot, a promotion and a
// Stripe lot past 30 days are not refundable.
func TestSelfServeRefundOnlyStripeUnspentWithinThirtyDays(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		refunding := newComputeT3Refunding(t3)
		refund := func(lotId server.Id) *controller.ComputeRefundLotAnswer {
			t.Helper()
			answer, err := controller.ComputeRefundLot(refunding.env, t3.ctx, refunding.settings, &controller.ComputeRefundLotRequest{NetworkId: refunding.networkId, LotId: lotId})
			if err != nil {
				t.Fatal(err)
			}
			return answer
		}
		// the network's first purchase, 8 days old and $10 unspent
		old := refunding.t3.queryInt(`SELECT count(*) FROM compute_lot WHERE network_id = $1`, refunding.networkId)
		if old != 1 {
			t.Fatalf("%d lots before", old)
		}
		store := refunding.lot(model.ComputeLotKindPurchase, model.ComputeMarketAppStore, computeT3Usd(10))
		promo := refunding.lot(model.ComputeLotKindPromotion, model.ComputeMarketPromotion, computeT3Usd(5))
		stripe := refunding.lot(model.ComputeLotKindPurchase, model.ComputeMarketStripe, computeT3Usd(25))
		for name, lotId := range map[string]server.Id{"a store's lot": store.LotId, "a promotion": promo.LotId, "no lot": server.NewId()} {
			if answer := refund(lotId); answer.Refusal != controller.ComputeRefundRefusalNotRefundable {
				t.Errorf("%s: %+v", name, answer)
			}
		}
		settledBefore := refunding.settled()
		answer := refund(stripe.LotId)
		if answer.Refusal != "" || !answer.Refunded || answer.RefundedNanoUsd != computeT3Usd(25) || refunding.remaining(stripe.LotId) != 0 ||
			refunding.settled() != settledBefore-computeT3Usd(25) {
			t.Fatalf("a Stripe lot's refund: %+v", answer)
		}
		if made, _ := computeT3Refunds.of(stripe.LotId); made == nil || made.AmountCents != 2500 || made.PaymentIntentId != stripe.MarketRef || made.Reason != controller.ComputeRefundReasonSelfServe {
			t.Fatalf("the Stripe refund asked: %+v", made)
		}
		// past its 30 days
		late := refunding.lot(model.ComputeLotKindPurchase, model.ComputeMarketStripe, computeT3Usd(10))
		refunding.clock.Advance(refunding.settings.RefundWindow + time.Hour)
		if answer := refund(late.LotId); answer.Refusal != controller.ComputeRefundRefusalNotRefundable {
			t.Fatalf("a lot past its 30 days: %+v", answer)
		}
		if made, _ := computeT3Refunds.of(late.LotId); made != nil {
			t.Fatal("a lot past its window was refunded")
		}
	})
}

// A self-serve refund retried answers its first refund: settled moved once and Stripe refunded once, though asked
// again.
func TestSelfServeRefundRetriedReturnsOnce(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		refunding := newComputeT3Refunding(t3)
		stripe := refunding.lot(model.ComputeLotKindPurchase, model.ComputeMarketStripe, computeT3Usd(25))
		refunding.spend(computeT3Usd(30.004))
		// $10 of the first lot and $25 of this one paid $30.004: what is left of this one is $4.996, whose whole cents go back
		settledBefore := refunding.settled()
		request := &controller.ComputeRefundLotRequest{NetworkId: refunding.networkId, LotId: stripe.LotId}
		first, err := controller.ComputeRefundLot(refunding.env, t3.ctx, refunding.settings, request)
		if err != nil || !first.Refunded || first.RefundedNanoUsd != computeT3Usd(4.99) || refunding.remaining(stripe.LotId) != computeT3Usd(0.006) {
			t.Fatalf("the first: %+v %v, %d left", first, err, refunding.remaining(stripe.LotId))
		}
		again, err := controller.ComputeRefundLot(refunding.env, t3.ctx, refunding.settings, request)
		if err != nil || again.Refunded || again.RefundedNanoUsd != first.RefundedNanoUsd || refunding.settled() != settledBefore-computeT3Usd(4.99) {
			t.Fatalf("the retry: %+v %v", again, err)
		}
		if made, asked := computeT3Refunds.of(stripe.LotId); made == nil || made.AmountCents != 499 || asked != 2 {
			t.Fatalf("the Stripe refund: %+v asked %d", made, asked)
		}
	})
}

// A refund nets into its charge's service: the day and lifetime rows of the charge's kind show it as net and refunded,
// with neither the count nor the units moved, and Adjustments hold nothing of it.
func TestRefundNetsIntoItsService(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		refunding := newComputeT3Refunding(t3)
		chargeId := refunding.charge(computeT3Usd(4))
		if _, err := controller.ComputeRefundCharge(refunding.env, t3.ctx, refunding.settings, &controller.ComputeRefundChargeRequest{
			ChargeId: chargeId, AmountNanoUsd: computeT3Usd(1.5),
		}); err != nil {
			t.Fatal(err)
		}
		refunding.book()
		kind := int16(model.ComputeChargeKindInference)
		for _, table := range []string{"compute_charge_day", "compute_charge_total"} {
			if amount := t3.queryInt(`SELECT sum(amount_nano_usd)::bigint FROM `+table+` WHERE network_id = $1 AND kind = $2`, refunding.networkId, kind); amount != computeT3Usd(2.5) {
				t.Errorf("%s's net: %d", table, amount)
			}
			if refunded := t3.queryInt(`SELECT sum(refunded_nano_usd)::bigint FROM `+table+` WHERE network_id = $1 AND kind = $2`, refunding.networkId, kind); refunded != computeT3Usd(1.5) {
				t.Errorf("%s's refunded: %d", table, refunded)
			}
			if count := t3.queryInt(`SELECT sum(count)::bigint FROM `+table+` WHERE network_id = $1 AND kind = $2`, refunding.networkId, kind); count != 1 {
				t.Errorf("%s's count: %d", table, count)
			}
			if adjusted := t3.queryInt(`SELECT count(*) FROM `+table+` WHERE network_id = $1 AND kind = $2`, refunding.networkId, int16(model.ComputeChargeKindAdjustment)); adjusted != 0 {
				t.Errorf("%s holds %d adjustment rows", table, adjusted)
			}
		}
		// a charge whose row is gone is answered gone: a support credit refunds it instead
		if gone, err := controller.ComputeRefundCharge(refunding.env, t3.ctx, refunding.settings, &controller.ComputeRefundChargeRequest{ChargeId: server.NewId()}); err != nil || !gone.Gone {
			t.Fatalf("a charge with no row: %+v %v", gone, err)
		}
	})
}

// A support credit counts to the network's own line: a kind-5 charge naming the case, no sidecar, member or provider,
// raising settled; its number run again adds nothing.
func TestAdjustmentCountsToTheNetworksOwnLine(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		refunding := newComputeT3Refunding(t3)
		caseId := server.NewId()
		settledBefore := refunding.settled()
		for i := 0; i < 2; i += 1 {
			if _, err := controller.ComputeAdjust(refunding.env, t3.ctx, refunding.settings, &controller.ComputeAdjustRequest{
				NetworkId: refunding.networkId, CaseId: caseId, Number: 1, AmountNanoUsd: -computeT3Usd(5),
			}); err != nil {
				t.Fatal(err)
			}
		}
		refunding.book()
		key := controller.ComputeOpKeyForAdjustment(caseId, 1)
		if t3.queryInt(`SELECT count(*) FROM compute_charge WHERE charge_id = $1 AND kind = $2 AND subject_ref = $3 AND sidecar_id = $4 AND member_id IS NULL AND provider = 0`,
			key, int16(model.ComputeChargeKindAdjustment), caseId, server.Id{}) != 1 {
			t.Fatal("the credit is not one charge on the network's own line")
		}
		if refunding.settled() != settledBefore+computeT3Usd(5) {
			t.Fatalf("settled moved from %d to %d", settledBefore, refunding.settled())
		}
		// a debit, the case's second
		if _, err := controller.ComputeAdjust(refunding.env, t3.ctx, refunding.settings, &controller.ComputeAdjustRequest{
			NetworkId: refunding.networkId, CaseId: caseId, Number: 2, AmountNanoUsd: computeT3Usd(2),
		}); err != nil {
			t.Fatal(err)
		}
		refunding.book()
		if refunding.settled() != settledBefore+computeT3Usd(3) {
			t.Fatalf("after the debit: %d", refunding.settled())
		}
	})
}

// A network's disputes are capped at 10 a UTC day, a repeat of one answering it; disputes of a provider from three
// networks past 0.5% of its requests page, and the catalog is never touched.
func TestDisputesCountedByNetworkAndCapped(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		refunding := newComputeT3Refunding(t3)
		provider := int16(7)
		dispute := func(networkId server.Id, requestId server.Id) *controller.ComputeDisputeAnswer {
			t.Helper()
			answer, err := controller.ComputeDispute(refunding.env, t3.ctx, refunding.settings, &controller.ComputeDisputeRequest{
				NetworkId: networkId, RequestId: requestId, Provider: provider,
				DeviceUnits: model.ComputeUnits{InputTokens: 100}, ProviderUnits: model.ComputeUnits{InputTokens: 200},
			})
			if err != nil {
				t.Fatal(err)
			}
			return answer
		}
		requestIds := []server.Id{}
		for i := 0; i < refunding.settings.DisputeNetworkDayMaxCount; i += 1 {
			requestIds = append(requestIds, server.NewId())
			if answer := dispute(refunding.networkId, requestIds[i]); answer.Refusal != "" || !answer.Recorded {
				t.Fatalf("dispute %d: %+v", i, answer)
			}
		}
		if answer := dispute(refunding.networkId, server.NewId()); answer.Refusal != controller.ComputeDisputeRefusalLimit {
			t.Fatalf("the 11th: %+v", answer)
		}
		if answer := dispute(refunding.networkId, requestIds[3]); answer.Refusal != "" || answer.Recorded {
			t.Fatalf("a repeat: %+v", answer)
		}
		if answer := dispute(server.NewId(), server.NewId()); answer.Paging {
			t.Fatalf("two networks paged: %+v", answer)
		}
		third := dispute(server.NewId(), server.NewId())
		if !third.Paging || third.ProviderNetworks != 3 || third.ProviderDisputes != 12 {
			t.Fatalf("three networks: %+v", third)
		}
		if got := refunding.metrics.Value("urnetwork_compute_dispute_networks", strconv.Itoa(int(provider))); got != 3 {
			t.Fatalf("the networks gauge: %v", got)
		}
	})
}

// A closure refunds each Stripe lot's unspent whole cents within 90 days of its purchase, forfeits the rest with
// notice, closes the account so nothing credits it, turns auto-renew off and cancels the plan; closed again, it asks
// the same refunds once more and owes no second notice.
func TestClosureRefundsWithinNinetyDaysAndForfeitsTheRest(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		refunding := newComputeT3Refunding(t3)
		// the first purchase, 8 days old with $10 left; one past 90 days, a store's, and a promotion
		old := refunding.lot(model.ComputeLotKindPurchase, model.ComputeMarketStripe, computeT3Usd(7))
		t3.mustExec(`UPDATE compute_lot SET create_time = create_time - interval '100 days' WHERE lot_id = $1`, old.LotId)
		recent := refunding.lot(model.ComputeLotKindPurchase, model.ComputeMarketStripe, computeT3Usd(30))
		store := refunding.lot(model.ComputeLotKindPurchase, model.ComputeMarketAppStore, computeT3Usd(5))
		refunding.lot(model.ComputeLotKindPromotion, model.ComputeMarketPromotion, computeT3Usd(3))
		refunding.saveCard(0, 0, 0)
		subscriptionId := "sub_t3_close_" + refunding.networkId.String()
		if err := controller.ComputeMonthlySubscribed(refunding.env, t3.ctx, refunding.settings, &controller.ComputeMonthlySubscription{
			NetworkId: refunding.networkId, PayerMemberId: refunding.payerId, Credits: 10,
			StripeCustomerId: "cus_t3_" + refunding.payerId.String(), StripeSubscriptionId: subscriptionId,
		}); err != nil {
			t.Fatal(err)
		}
		// the projection stands, as for any network that has run a call, so the closure reaches it
		refunding.rebuild()
		answer, err := controller.ComputeCloseNetwork(refunding.env, t3.ctx, refunding.settings, refunding.networkId)
		if err != nil || !answer.ClosedNow {
			t.Fatalf("the closure: %+v %v", answer, err)
		}
		// the first purchase ($10, 8 days) and the recent one ($30) back; the old one, the store's and the promotion lost
		if answer.RefundedNanoUsd != computeT3Usd(40) || answer.ForfeitedNanoUsd != computeT3Usd(15) || len(answer.RefundedLots) != 2 {
			t.Fatalf("refunded %d, forfeited %d, %d lots", answer.RefundedNanoUsd, answer.ForfeitedNanoUsd, len(answer.RefundedLots))
		}
		if made, _ := computeT3Refunds.of(recent.LotId); made == nil || made.AmountCents != 3000 || made.Reason != controller.ComputeRefundReasonClosure {
			t.Fatalf("the recent lot's refund: %+v", made)
		}
		for _, lotId := range []server.Id{old.LotId, store.LotId} {
			if made, _ := computeT3Refunds.of(lotId); made != nil {
				t.Errorf("lot %s was refunded", lotId)
			}
		}
		if stored, projected := refunding.states(); stored != model.ComputeAccountStateClosed || projected != model.ComputeAccountStateClosed {
			t.Fatalf("closed: stored %s, projected %s", stored, projected)
		}
		state := refunding.state()
		if state.Threshold.On || state.Monthly.On || !computeT3Renew.wasCancelled(subscriptionId) || !computeT3Renew.wasDetached(refunding.card) {
			t.Fatalf("auto-renew after the closure: %+v", state)
		}
		if _, err := controller.ComputeCreditLot(refunding.env, t3.ctx, refunding.settings, &controller.ComputeCreditRequest{
			NetworkId: refunding.networkId, Kind: model.ComputeLotKindPurchase, Market: model.ComputeMarketStripe,
			MarketRef: "pi_t3_after_close_" + refunding.networkId.String(), AmountNanoUsd: computeT3Usd(10),
		}); !errors.Is(err, controller.ErrComputeAccountClosed) {
			t.Fatalf("a credit after the closure: %v", err)
		}
		again, err := controller.ComputeCloseNetwork(refunding.env, t3.ctx, refunding.settings, refunding.networkId)
		if err != nil || again.ClosedNow || !again.Closed {
			t.Fatalf("closed again: %+v %v", again, err)
		}
		if made, asked := computeT3Refunds.of(recent.LotId); made == nil || asked != 2 {
			t.Fatalf("the recent lot's refund asked %d times", asked)
		}
		closed := 0
		for _, kind := range computeT3Notifier.kinds(refunding.networkId) {
			if kind == controller.ComputeNoticeClosed {
				closed += 1
			}
		}
		if closed != 1 {
			t.Fatalf("%d closure notices", closed)
		}
		if !slices.Contains(computeT3Notifier.kinds(refunding.networkId), controller.ComputeNoticeClosed) {
			t.Fatal("no closure notice")
		}
	})
}
