//go:build sidecar_pending

package computet3

// Promotion codes on the private stack's PostgreSQL and Redis (BAL-M12-13; DESIGN-COMPUTE §9.11, §14.3, §14.5,
// D-BAL-12): a code redeemed once, its retry answering the same lot and a repeat past the retry window code_redeemed;
// one redemption a network a code and one a network in 30 days; a code past its cap or its last day refused; a cap above
// one asking for an unreversed paid lot; the value counted as a load against the daily load and the cap; a lot lapsing
// at 30 days by compute_promo_lapse, never as a charge, with its lapse on the lot the statement reads; five failed codes
// in a UTC day and then rate_limited until the next; and the code never kept in clear.

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// Makes a code, failing the test on an error.
func (self *computeT3Clocked) createPromo(request *controller.ComputePromoCreate) *controller.ComputePromoCreated {
	self.t3.t.Helper()
	if request.Campaign == "" {
		request.Campaign = "test campaign"
	}
	if request.CreatedBy == "" {
		request.CreatedBy = "operator.example"
	}
	if request.Reason == "" {
		request.Reason = "a test of the ledger"
	}
	if request.LastDay.IsZero() {
		request.LastDay = self.clock.Now().AddDate(0, 0, 60)
	}
	created, err := controller.ComputeCreatePromo(self.env, self.t3.ctx, self.settings, computeT3Secrets{}, request)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return created
}

// Redeems code for networkId, failing the test on an error.
func (self *computeT3Clocked) redeem(networkId server.Id, code string) *controller.ComputeRedeemAnswer {
	self.t3.t.Helper()
	sidecarId := server.NewId()
	answer, err := controller.ComputeRedeemPromo(self.env, self.t3.ctx, self.settings, computeT3Secrets{}, &controller.ComputeRedeemRequest{
		NetworkId: networkId, SidecarId: &sidecarId, Code: code,
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return answer
}

// A code redeems once: the lot of its value, a promotion lapsing 30 days on, its market_ref the redemption's id, the
// code's count one; a retry, in any case, answers the same lot and credits nothing; past the retry window the network
// is answered code_redeemed.
func TestPromoRedeemedOnceAndRetryAnswersTheSameLot(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		now := computeT3Midday(time.Now().UTC())
		clocked := newComputeT3Clocked(t3, now)
		created := clocked.createPromo(&controller.ComputePromoCreate{AmountNanoUsd: computeT3Usd(10), RedeemMaxCount: 1})
		networkId := server.NewId()
		first := clocked.redeem(networkId, created.Code)
		if !first.Redeemed || first.Refusal != "" || first.Lot.AmountNanoUsd != computeT3Usd(10) || first.Lot.Kind != model.ComputeLotKindPromotion ||
			first.Lot.Market != model.ComputeMarketPromotion || first.Lot.ExpireTime == nil || !first.Lot.ExpireTime.Equal(now.Add(clocked.settings.PromoLapse)) {
			t.Fatalf("the redemption: %+v", first)
		}
		if t3.queryInt(`SELECT count(*) FROM compute_promo_redemption WHERE network_id = $1 AND redemption_id::text = $2`, networkId, first.Lot.MarketRef) != 1 {
			t.Error("the lot's reference is not its redemption's id")
		}
		retry := clocked.redeem(networkId, strings.ToLower(created.Code))
		if retry.Redeemed || retry.Lot == nil || retry.Lot.LotId != first.Lot.LotId {
			t.Fatalf("a retry: %+v", retry)
		}
		if t3.queryInt(`SELECT count(*) FROM compute_lot WHERE network_id = $1`, networkId) != 1 ||
			t3.queryInt(`SELECT settled_nano_usd FROM compute_account WHERE network_id = $1`, networkId) != computeT3Usd(10) ||
			t3.queryInt(`SELECT redeem_count FROM compute_promo_code WHERE code_id = $1`, created.CodeId) != 1 {
			t.Error("a retry credited again")
		}
		clocked.clock.Advance(clocked.settings.PromoRetryWindow + time.Minute)
		if again := clocked.redeem(networkId, created.Code); again.Refusal != controller.ComputePromoRefusalCodeRedeemed || again.Lot != nil {
			t.Fatalf("past the retry window: %+v", again)
		}
	})
}

// A network redeems a code once and one code in 30 days: a second code within 30 days is redeemed_recently, and
// redeems once the 30 days are past.
func TestPromoOneANetworkAndOneIn30Days(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		first := clocked.createPromo(&controller.ComputePromoCreate{AmountNanoUsd: computeT3Usd(5), RedeemMaxCount: 10, PaidLotRequired: true})
		second := clocked.createPromo(&controller.ComputePromoCreate{AmountNanoUsd: computeT3Usd(5), RedeemMaxCount: 10, PaidLotRequired: true, LastDay: clocked.clock.Now().AddDate(0, 0, 90)})
		networkId := server.NewId()
		clocked.purchase(networkId, computeT3Usd(20), nil)
		if answer := clocked.redeem(networkId, first.Code); !answer.Redeemed {
			t.Fatalf("the first code: %+v", answer)
		}
		if answer := clocked.redeem(networkId, second.Code); answer.Refusal != controller.ComputePromoRefusalRedeemedRecently {
			t.Fatalf("a second code in 30 days: %+v", answer)
		}
		clocked.clock.Advance(clocked.settings.PromoNetworkInterval - time.Minute)
		if answer := clocked.redeem(networkId, second.Code); answer.Refusal != controller.ComputePromoRefusalRedeemedRecently {
			t.Fatalf("a minute short of 30 days: %+v", answer)
		}
		clocked.clock.Advance(time.Minute)
		if answer := clocked.redeem(networkId, second.Code); !answer.Redeemed {
			t.Fatalf("30 days on: %+v", answer)
		}
		// the first code a second time for the network, long past its retry window
		if answer := clocked.redeem(networkId, first.Code); answer.Refusal != controller.ComputePromoRefusalCodeRedeemed {
			t.Fatalf("the first code again: %+v", answer)
		}
	})
}

// A code past its cap, or past its last UTC day, is code_ended.
func TestPromoPastCapOrLastDayRefused(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		capped := clocked.createPromo(&controller.ComputePromoCreate{AmountNanoUsd: computeT3Usd(2), RedeemMaxCount: 2, PaidLotRequired: true})
		networkIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		for _, networkId := range networkIds {
			clocked.purchase(networkId, computeT3Usd(10), nil)
		}
		for _, networkId := range networkIds[:2] {
			if answer := clocked.redeem(networkId, capped.Code); !answer.Redeemed {
				t.Fatalf("under the cap: %+v", answer)
			}
		}
		if answer := clocked.redeem(networkIds[2], capped.Code); answer.Refusal != controller.ComputePromoRefusalCodeEnded {
			t.Fatalf("past the cap: %+v", answer)
		}
		today := clocked.createPromo(&controller.ComputePromoCreate{AmountNanoUsd: computeT3Usd(2), RedeemMaxCount: 1, LastDay: clocked.clock.Now()})
		clocked.clock.Advance(24 * time.Hour)
		if answer := clocked.redeem(server.NewId(), today.Code); answer.Refusal != controller.ComputePromoRefusalCodeEnded {
			t.Fatalf("past its last day: %+v", answer)
		}
		// a last day before today is refused at its making
		if _, err := controller.ComputeCreatePromo(clocked.env, t3.ctx, clocked.settings, computeT3Secrets{}, &controller.ComputePromoCreate{
			Campaign: "late", AmountNanoUsd: computeT3Usd(1), RedeemMaxCount: 1, LastDay: clocked.clock.Now().AddDate(0, 0, -1),
			CreatedBy: "operator.example", Reason: "a test",
		}); err == nil {
			t.Error("a code ended before it was made")
		}
	})
}

// A code whose cap is above one asks for an unreversed paid lot: made without the rule it is refused; a network with
// no paid lot, or only a reversed one, is paid_lot_required; one with a paid lot redeems. A value past $10 is refused.
func TestPromoCapAboveOneNeedsAPaidLot(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		for label, request := range map[string]*controller.ComputePromoCreate{
			"a cap above one, no paid lot": {AmountNanoUsd: computeT3Usd(5), RedeemMaxCount: 2},
			"past $10":                     {AmountNanoUsd: computeT3Usd(10.01), RedeemMaxCount: 1},
		} {
			request.Campaign, request.CreatedBy, request.Reason, request.LastDay = "c", "operator.example", "a test", clocked.clock.Now()
			if _, err := controller.ComputeCreatePromo(clocked.env, t3.ctx, clocked.settings, computeT3Secrets{}, request); err == nil {
				t.Errorf("%s was made", label)
			}
		}
		created := clocked.createPromo(&controller.ComputePromoCreate{AmountNanoUsd: computeT3Usd(5), RedeemMaxCount: 100, PaidLotRequired: true})
		unpaid := server.NewId()
		clocked.promotion(unpaid, computeT3Usd(1))
		if answer := clocked.redeem(unpaid, created.Code); answer.Refusal != controller.ComputePromoRefusalPaidLotRequired {
			t.Fatalf("no paid lot: %+v", answer)
		}
		refunded := server.NewId()
		clocked.credit(&controller.ComputeCreditRequest{
			NetworkId: refunded, Kind: model.ComputeLotKindPurchase, Market: model.ComputeMarketAppStore,
			MarketRef: "2000000000000999", AmountNanoUsd: computeT3Usd(10), Store: true,
		})
		clocked.reverse(model.ComputeMarketAppStore, "2000000000000999", false)
		if answer := clocked.redeem(refunded, created.Code); answer.Refusal != controller.ComputePromoRefusalPaidLotRequired {
			t.Fatalf("only a reversed paid lot: %+v", answer)
		}
		paid := server.NewId()
		clocked.purchase(paid, computeT3Usd(10), nil)
		if answer := clocked.redeem(paid, created.Code); !answer.Redeemed {
			t.Fatalf("a paid lot: %+v", answer)
		}
	})
}

// A code's value counts as a load with the day's purchases: a network that bought $1,500 today redeems nothing until
// tomorrow, and one at its $10,000 cap nothing at all; a chargeback's suspension refuses too.
func TestPromoCountsAsALoad(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		created := clocked.createPromo(&controller.ComputePromoCreate{AmountNanoUsd: computeT3Usd(10), RedeemMaxCount: 10, PaidLotRequired: true})
		loaded := server.NewId()
		clocked.purchase(loaded, computeT3Usd(1_500), nil)
		if answer := clocked.redeem(loaded, created.Code); answer.Refusal != controller.ComputePromoRefusalDailyLimit {
			t.Fatalf("on a $1,500 day: %+v", answer)
		}
		clocked.clock.Advance(24 * time.Hour)
		if answer := clocked.redeem(loaded, created.Code); !answer.Redeemed {
			t.Fatalf("the next day: %+v", answer)
		}
		capped := server.NewId()
		clocked.purchase(capped, computeT3Usd(5), nil)
		clocked.credit(&controller.ComputeCreditRequest{
			NetworkId: capped, Kind: model.ComputeLotKindAdjustment, Market: model.ComputeMarketManual,
			MarketRef: "case-test-cap", AmountNanoUsd: computeT3Usd(9_990),
		})
		if answer := clocked.redeem(capped, created.Code); answer.Refusal != controller.ComputePromoRefusalBalanceCap {
			t.Fatalf("at the cap: %+v", answer)
		}
		charged := server.NewId()
		purchase := clocked.purchase(charged, computeT3Usd(10), nil)
		clocked.purchase(charged, computeT3Usd(10), nil)
		clocked.reverse(model.ComputeMarketStripe, purchase.Lot.MarketRef, true)
		if answer := clocked.redeem(charged, created.Code); answer.Refusal != controller.ComputePromoRefusalSuspended {
			t.Fatalf("after a chargeback: %+v", answer)
		}
	})
}

// At its expiry what is left of a promotional lot lapses: compute_promo_lapse sets its lapsed_nano_usd, which the
// month's statement reads, and lowers settled and the projection's settled and promo by as much, writing no charge; a
// second run lapses nothing.
func TestPromoLapsesAt30DaysAndShowsInTheStatement(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		clocked := newComputeT3Clocked(t3, time.Now().UTC())
		created := clocked.createPromo(&controller.ComputePromoCreate{AmountNanoUsd: computeT3Usd(10), RedeemMaxCount: 1})
		redeemed := clocked.redeem(ledger.networkId, created.Code)
		if !redeemed.Redeemed {
			t.Fatalf("the redemption: %+v", redeemed)
		}
		ledger.settledRow(model.ComputeChargeKindInference, computeT3Usd(3), time.Now().UTC().Add(-time.Minute))
		ledger.book()
		ledger.rebuildFromLedger()
		if projection := ledger.read(); projection.PromoNanoUsd != computeT3Usd(7) {
			t.Fatalf("the projection's promo before the lapse: %+v", projection)
		}
		charges := t3.queryInt(`SELECT count(*) FROM compute_charge WHERE network_id = $1`, ledger.networkId)
		lapse := func() *controller.ComputeLapseResult {
			result, err := controller.ComputeLapsePromos(clocked.env, t3.ctx, clocked.settings)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		clocked.clock.Advance(clocked.settings.PromoLapse - time.Minute)
		if result := lapse(); result.LotCount != 0 {
			t.Fatalf("a minute before its expiry: %+v", result)
		}
		clocked.clock.Advance(time.Minute)
		result := lapse()
		if result.LotCount != 1 || result.LapsedNanoUsd != computeT3Usd(7) || result.UnprojectedCount != 0 {
			t.Fatalf("at its expiry: %+v", result)
		}
		if t3.queryInt(`SELECT coalesce(lapsed_nano_usd, 0) FROM compute_lot WHERE lot_id = $1`, redeemed.Lot.LotId) != computeT3Usd(7) ||
			t3.queryInt(`SELECT remaining_nano_usd FROM compute_lot WHERE lot_id = $1`, redeemed.Lot.LotId) != 0 {
			t.Error("the lot's lapse")
		}
		if account := ledger.account(); account.SettledNanoUsd != 0 {
			t.Errorf("settled after the lapse: %d", account.SettledNanoUsd)
		}
		if projection := ledger.read(); projection.SettledNanoUsd != 0 || projection.PromoNanoUsd != 0 {
			t.Errorf("the projection after the lapse: %+v", projection)
		}
		if t3.queryInt(`SELECT count(*) FROM compute_charge WHERE network_id = $1`, ledger.networkId) != charges {
			t.Error("the lapse wrote a charge")
		}
		if again := lapse(); again.LotCount != 0 {
			t.Errorf("a second run: %+v", again)
		}
	})
}

// Five failed codes in a UTC day, unknown or ended, and the network's attempts answer rate_limited until the next UTC
// day, a good code among them; a refusal of a good code for the network's own state is no failure; and the next day
// starts afresh.
func TestPromoRateLimitedAfterFiveFailures(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		good := clocked.createPromo(&controller.ComputePromoCreate{AmountNanoUsd: computeT3Usd(1), RedeemMaxCount: 10, PaidLotRequired: true})
		networkId := server.NewId()
		// a good code refused for want of a paid lot counts no failure
		for i := 0; i < 6; i += 1 {
			if answer := clocked.redeem(networkId, good.Code); answer.Refusal != controller.ComputePromoRefusalPaidLotRequired {
				t.Fatalf("a good code without a paid lot: %+v", answer)
			}
		}
		for i := 0; i < clocked.settings.PromoFailureDayMaxCount; i += 1 {
			if answer := clocked.redeem(networkId, "UNKNOWNCODE000"+string(rune('A'+i))); answer.Refusal != controller.ComputePromoRefusalCodeUnknown {
				t.Fatalf("an unknown code: %+v", answer)
			}
		}
		clocked.purchase(networkId, computeT3Usd(10), nil)
		limited := clocked.redeem(networkId, good.Code)
		if limited.Refusal != controller.ComputePromoRefusalRateLimited || limited.RetryAfter <= 0 ||
			!clocked.clock.Now().Add(limited.RetryAfter).Equal(model.ComputeUtcDay(clocked.clock.Now()).AddDate(0, 0, 1)) {
			t.Fatalf("after five failures: %+v", limited)
		}
		clocked.clock.Advance(limited.RetryAfter)
		if answer := clocked.redeem(networkId, good.Code); !answer.Redeemed {
			t.Fatalf("the next UTC day: %+v", answer)
		}
		// §14.7's count, by outcome alone
		for outcome, want := range map[string]float64{"paid_lot_required": 6, "code_unknown": 5, "rate_limited": 1, "redeemed": 1} {
			if got := clocked.metrics.Value("urnetwork_compute_promo_redemptions_total", outcome); got != want {
				t.Errorf("the %s count is %v, want %v", outcome, got, want)
			}
		}
	})
}

// The code is never kept in clear: compute_promo_code holds a keyed hash of it, its case folded, and no column of the
// ledger holds its text, in either case.
func TestPromoCodeNeverStoredInClear(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		created := clocked.createPromo(&controller.ComputePromoCreate{AmountNanoUsd: computeT3Usd(3), RedeemMaxCount: 1, Code: "ClearTextCode2026"})
		clocked.redeem(server.NewId(), created.Code)
		for _, form := range []string{"ClearTextCode2026", "cleartextcode2026", "CLEARTEXTCODE2026"} {
			for _, table := range []string{"compute_promo_code", "compute_promo_redemption", "compute_lot"} {
				if t3.queryInt(`SELECT count(*) FROM `+table+` WHERE position($1 IN row_to_json(`+table+`)::text) > 0`, form) != 0 {
					t.Errorf("%s holds the code as %s", table, form)
				}
			}
			if t3.queryInt(`SELECT count(*) FROM compute_promo_code WHERE position($1::bytea IN code_hash) > 0`, []byte(form)) != 0 {
				t.Errorf("the hash holds %s", form)
			}
		}
		if t3.queryInt(`SELECT length(code_hash) FROM compute_promo_code WHERE code_id = $1`, created.CodeId) != 32 {
			t.Error("the code's hash is not a SHA-256 MAC")
		}
		// one code in any case is one code: its other case is taken
		if _, err := controller.ComputeCreatePromo(clocked.env, t3.ctx, clocked.settings, computeT3Secrets{}, &controller.ComputePromoCreate{
			Campaign: "again", AmountNanoUsd: computeT3Usd(1), RedeemMaxCount: 1, LastDay: clocked.clock.Now(), Code: "CLEARTEXTCODE2026",
			CreatedBy: "operator.example", Reason: "a test",
		}); err == nil {
			t.Error("the code's other case was made again")
		}
	})
}
