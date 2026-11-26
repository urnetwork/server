//go:build sidecar_pending

package computet3

// §9.11's limits on the private stack's PostgreSQL (BAL-M12-07; DESIGN-COMPUTE §9.5, §9.11): a new payer limited across
// the fresh networks it funds, the daily load stopping a network and a payer with a promotion counted as a load, the
// balance cap refused before payment, the search cap by the oldest unreversed paid lot's age and its override, the
// enclave gates by the first payment and lifetime purchases, the payer rows holding no network, and the payer rows
// pruned past their retention. Time moves on a fake clock, in UTC days.

import (
	"testing"
	"time"

	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// Checks a purchase, failing the test on an error.
func (self *computeT3Clocked) check(networkId server.Id, amount int64, payerHash []byte) *controller.ComputePurchaseAnswer {
	self.t3.t.Helper()
	answer, err := controller.ComputePurchaseCheck(self.env, self.t3.ctx, self.settings, computeT3Secrets{}, &controller.ComputePurchaseRequest{
		NetworkId: networkId, PayerHash: payerHash, AmountNanoUsd: amount,
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return answer
}

// Whether answer refused for reason, by the network's limit or the payer's.
func computeT3Breached(answer *controller.ComputePurchaseAnswer, reason controller.ComputeLimitReason, payer bool) bool {
	for _, breach := range answer.Breaches {
		if breach.Reason == reason && breach.Payer == payer {
			return true
		}
	}
	return false
}

// A day's start on the clock: the midday of the UTC day after now's, so a test's days never straddle midnight.
func computeT3Midday(now time.Time) time.Time {
	return model.ComputeUtcDay(now).AddDate(0, 0, 1).Add(12 * time.Hour)
}

// One card funding fresh networks meets its own $100 a day and $200 in its first 7 days, whichever network it funds;
// past its window, only the daily load binds it.
func TestComputeNewPayerLimitedAcrossFreshNetworks(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		payer := clocked.payer("fp_test_new_payer")
		first, second, third := server.NewId(), server.NewId(), server.NewId()
		if answer := clocked.check(first, computeT3Usd(90), payer); !answer.Allowed {
			t.Fatalf("the first purchase: %+v", answer.Breaches)
		}
		clocked.purchase(first, computeT3Usd(90), payer)
		answer := clocked.check(second, computeT3Usd(20), payer)
		if answer.Allowed || !computeT3Breached(answer, controller.ComputeLimitNewAccountDay, true) || computeT3Breached(answer, controller.ComputeLimitNewAccountDay, false) {
			t.Fatalf("a fresh network on the payer's day: %+v", answer.Breaches)
		}
		clocked.clock.Advance(24 * time.Hour)
		if answer := clocked.check(second, computeT3Usd(90), payer); !answer.Allowed {
			t.Fatalf("the next day: %+v", answer.Breaches)
		}
		clocked.purchase(second, computeT3Usd(90), payer)
		clocked.clock.Advance(24 * time.Hour)
		answer = clocked.check(third, computeT3Usd(30), payer)
		if answer.Allowed || !computeT3Breached(answer, controller.ComputeLimitNewAccountWindow, true) || len(answer.Breaches) != 1 {
			t.Fatalf("the payer's first 7 days: %+v", answer.Breaches)
		}
		// past its first 7 days the payer may load past $200, its new third network still under its own day's $100
		clocked.clock.Advance(6 * 24 * time.Hour)
		if answer := clocked.check(third, computeT3Usd(100), payer); !answer.Allowed {
			t.Fatalf("past the payer's window: %+v", answer.Breaches)
		}
	})
}

// $1,500 a UTC day stops a network and a payer across markets and networks; a promotion counts as the network's load
// and as no payer's; the next UTC day starts afresh.
func TestComputeDailyLoadStopsNetworkAndPayer(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		networkId := server.NewId()
		payer := clocked.payer("fp_test_daily_load")
		clocked.purchase(networkId, computeT3Usd(50), payer)
		// past both new-account windows
		clocked.clock.Advance(8 * 24 * time.Hour)
		clocked.purchase(networkId, computeT3Usd(1_400), payer)
		if answer := clocked.check(networkId, computeT3Usd(95), payer); !answer.Allowed {
			t.Fatalf("$1,495 in a day: %+v", answer.Breaches)
		}
		answer := clocked.check(networkId, computeT3Usd(200), payer)
		if answer.Allowed || !computeT3Breached(answer, controller.ComputeLimitDailyLoad, false) || !computeT3Breached(answer, controller.ComputeLimitDailyLoad, true) || answer.Code() != "daily_limit" {
			t.Fatalf("$1,600 in a day: %+v", answer.Breaches)
		}
		clocked.promotion(networkId, computeT3Usd(10))
		answer = clocked.check(networkId, computeT3Usd(95), payer)
		if answer.Allowed || !computeT3Breached(answer, controller.ComputeLimitDailyLoad, false) || computeT3Breached(answer, controller.ComputeLimitDailyLoad, true) {
			t.Fatalf("a promotion beside $1,495: %+v", answer.Breaches)
		}
		// the payer's day spans its networks: another network's purchase by the card meets it
		otherNetwork := server.NewId()
		answer = clocked.check(otherNetwork, computeT3Usd(101), payer)
		if !computeT3Breached(answer, controller.ComputeLimitDailyLoad, true) {
			t.Fatalf("the payer on another network: %+v", answer.Breaches)
		}
		clocked.clock.Advance(24 * time.Hour)
		if answer := clocked.check(networkId, computeT3Usd(1_000), payer); !answer.Allowed {
			t.Fatalf("the next UTC day: %+v", answer.Breaches)
		}
	})
}

// A purchase that would take the balance past $10,000 is refused before payment, balance_cap, and one that stays
// within it is not.
func TestComputeBalanceCapRefusedBeforePayment(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		networkId := server.NewId()
		clocked.credit(&controller.ComputeCreditRequest{
			NetworkId: networkId, Kind: model.ComputeLotKindAdjustment, Market: model.ComputeMarketManual,
			MarketRef: "case-test-0001", AmountNanoUsd: computeT3Usd(9_950),
		})
		answer := clocked.check(networkId, computeT3Usd(100), nil)
		if answer.Allowed || len(answer.Breaches) != 1 || answer.Breaches[0].Reason != controller.ComputeLimitBalanceCap || answer.Code() != "balance_cap" {
			t.Fatalf("past the cap: %+v", answer.Breaches)
		}
		if answer := clocked.check(networkId, computeT3Usd(50), nil); !answer.Allowed {
			t.Fatalf("at the cap: %+v", answer.Breaches)
		}
		// the check wrote nothing
		if t3.queryInt(`SELECT count(*) FROM compute_lot WHERE network_id = $1`, networkId) != 1 {
			t.Error("a check wrote a lot")
		}
	})
}

// The search cap: 200 a day with no paid lot and until 7 days after the oldest unreversed one, then 2,000; a
// reversed lot no longer counts, and URnetwork's override stands over both.
func TestComputeSearchCapByPaidLotAge(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		networkId := server.NewId()
		searchCap := func() int64 {
			value, err := controller.ComputeSearchCap(clocked.env, t3.ctx, clocked.settings, networkId)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		if searchCap() != 200 {
			t.Fatalf("no paid lot: %d", searchCap())
		}
		clocked.promotion(networkId, computeT3Usd(10))
		clocked.credit(&controller.ComputeCreditRequest{
			NetworkId: networkId, Kind: model.ComputeLotKindPurchase, Market: model.ComputeMarketAppStore,
			MarketRef: "2000000000000777", AmountNanoUsd: computeT3Usd(20), Store: true,
		})
		if searchCap() != 200 {
			t.Fatalf("a new paid lot: %d", searchCap())
		}
		clocked.clock.Advance(7*24*time.Hour - time.Minute)
		if searchCap() != 200 {
			t.Fatalf("a minute short of 7 days: %d", searchCap())
		}
		clocked.clock.Advance(time.Minute)
		if searchCap() != 2_000 {
			t.Fatalf("7 days on: %d", searchCap())
		}
		clocked.reverse(model.ComputeMarketAppStore, "2000000000000777", false)
		if searchCap() != 200 {
			t.Fatalf("the paid lot reversed: %d", searchCap())
		}
		t3.mustExec(`UPDATE compute_account SET search_cap_override = 5000 WHERE network_id = $1`, networkId)
		if searchCap() != 5_000 {
			t.Fatalf("the override: %d", searchCap())
		}
	})
}

// One CPU enclave until 24 hours after the first payment, none of a GPU shape before $50 of lifetime purchases.
func TestComputeEnclaveAllowance(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		networkId := server.NewId()
		allowance := func() *controller.ComputeEnclaveAllowance {
			value, err := controller.ComputeEnclaveAllowanceOf(clocked.env, t3.ctx, clocked.settings, networkId)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		if a := allowance(); !a.CpuBounded || a.CpuEnclaveMaxCount != 1 || a.GpuShapes {
			t.Fatalf("no payment: %+v", a)
		}
		clocked.promotion(networkId, computeT3Usd(10))
		clocked.purchase(networkId, computeT3Usd(20), nil)
		if a := allowance(); !a.CpuBounded || a.CpuEnclaveMaxCount != 1 || a.GpuShapes {
			t.Fatalf("the first payment: %+v", a)
		}
		clocked.clock.Advance(24 * time.Hour)
		if a := allowance(); a.CpuBounded || a.GpuShapes {
			t.Fatalf("24 hours on: %+v", a)
		}
		clocked.purchase(networkId, computeT3Usd(30), nil)
		if a := allowance(); !a.GpuShapes {
			t.Fatalf("$50 of purchases: %+v", a)
		}
	})
}

// The payer's rows hold its keyed hash, never its card's fingerprint, and no network: one card funding two networks is
// one payer row and one day row, and no table joins them to either network.
func TestComputePayerRowsHoldNoNetwork(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		clocked := newComputeT3Clocked(t3, computeT3Midday(time.Now().UTC()))
		payer := clocked.payer("fp_test_two_networks")
		first, second := server.NewId(), server.NewId()
		clocked.purchase(first, computeT3Usd(30), payer)
		clocked.purchase(second, computeT3Usd(40), payer)
		if t3.queryInt(`SELECT count(*) FROM compute_payer`) != 1 || t3.queryInt(`SELECT count(*) FROM compute_payer_day`) != 1 ||
			t3.queryInt(`SELECT amount_nano_usd FROM compute_payer_day WHERE payer_hash = $1`, payer) != computeT3Usd(70) {
			t.Fatal("one card's rows")
		}
		for _, table := range []string{"compute_payer", "compute_payer_day"} {
			if t3.queryInt(`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name LIKE '%network%'`, table) != 0 {
				t.Errorf("%s has a network column", table)
			}
		}
		if t3.queryInt(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'compute_lot' AND column_name LIKE '%payer%'`) != 0 {
			t.Error("compute_lot names a payer")
		}
		if t3.queryInt(`SELECT count(*) FROM compute_payer WHERE position($1::bytea IN payer_hash) > 0`, []byte("fp_test_two_networks")) != 0 ||
			t3.queryInt(`SELECT length(payer_hash) FROM compute_payer`) != 32 {
			t.Error("the payer row holds its fingerprint")
		}
	})
}

// A payer's day sums go past 30 days and a payer past 2 years, with compute_rollup_day's pass; the rows inside their
// retention stay.
func TestComputePayerRowsPrunedPastTheirRetention(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		now := time.Now().UTC()
		today := model.ComputeUtcDay(now)
		for i, day := range []time.Time{today.AddDate(0, 0, -31), today.AddDate(0, 0, -29)} {
			t3.mustExec(`INSERT INTO compute_payer_day (payer_hash, day, amount_nano_usd) VALUES ($1, $2::date, 1)`, []byte{byte(i + 1)}, day.Format(time.DateOnly))
		}
		for i, day := range []time.Time{today.AddDate(0, 0, -731), today.AddDate(0, 0, -100)} {
			t3.mustExec(`INSERT INTO compute_payer (payer_hash, first_purchase_day) VALUES ($1, $2::date)`, []byte{byte(i + 10)}, day.Format(time.DateOnly))
		}
		rollup := newComputeT3Rollup(t3, now)
		result := rollup.pass(nil)
		if result.PrunedPayerDayCount != 1 || result.PrunedPayerCount != 1 {
			t.Fatalf("pruned %d day sums and %d payers", result.PrunedPayerDayCount, result.PrunedPayerCount)
		}
		if t3.queryInt(`SELECT count(*) FROM compute_payer_day`) != 1 || t3.queryInt(`SELECT count(*) FROM compute_payer`) != 1 {
			t.Error("the rows inside their retention")
		}
	})
}
