//go:build sidecar_pending

package computet3

// Auto-renew on the private stack's PostgreSQL and Redis with a fake Stripe for auto-renew and a fake clock (BAL-M12-11;
// DESIGN-COMPUTE §9.5, §9.8, R1-31, R1-33), the stand-in for CTEST's computetest.FakeStores until it lands: threshold
// fires once below T; at most 3 a UTC day and L a month; a failure retried after 1 h and 6 h, then threshold off, each
// failure emailed; authentication_required emailing and turning threshold off; a call held at a cent's cost charging no
// card; a Billing admin's change charging no other member's card; a payer who left detached; the monthly grant credited
// once and carried over; and turning a mode off cancelling only what was not yet sent.

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// A fake Stripe for auto-renew: each network's top-ups answer its planned outcomes in order (succeeded once they run
// out), a key sent again answers what it answered first, as Stripe's idempotency does, and every new charge, detach,
// cancel and plan checkout is recorded.
type computeT3RenewFake struct {
	stateLock sync.Mutex
	plans     map[operator.Id][]controller.ComputeOffSessionOutcome
	keys      map[string]*controller.ComputeOffSessionAnswer
	charges   map[operator.Id][]*controller.ComputeOffSessionCharge
	detached  []string
	cancelled []string
	monthly   []*controller.ComputeStripeMonthlyRequest
	intents   int
}

func (self *computeT3RenewFake) CardFingerprint(ctx context.Context, paymentMethodId string) (string, error) {
	return "fp_" + paymentMethodId, nil
}

func (self *computeT3RenewFake) ChargeOffSession(ctx context.Context, charge *controller.ComputeOffSessionCharge) (*controller.ComputeOffSessionAnswer, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if answer, ok := self.keys[charge.IdempotencyKey]; ok {
		return answer, nil
	}
	outcome := controller.ComputeOffSessionSucceeded
	if plan := self.plans[charge.NetworkId]; 0 < len(plan) {
		outcome, self.plans[charge.NetworkId] = plan[0], plan[1:]
	}
	self.intents += 1
	answer := &controller.ComputeOffSessionAnswer{Outcome: outcome, PaymentIntentId: fmt.Sprintf("pi_t3_top_up_%d", self.intents)}
	self.keys[charge.IdempotencyKey] = answer
	self.charges[charge.NetworkId] = append(self.charges[charge.NetworkId], charge)
	return answer, nil
}

func (self *computeT3RenewFake) MonthlyCheckout(ctx context.Context, request *controller.ComputeStripeMonthlyRequest) (*controller.ComputeStripeCheckoutSession, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.monthly = append(self.monthly, request)
	return &controller.ComputeStripeCheckoutSession{SessionId: "cs_t3_monthly", Url: "https://checkout.stripe.com/c/pay/cs_t3_monthly", ExpireTime: time.Now().Add(time.Hour)}, nil
}

func (self *computeT3RenewFake) CancelSubscription(ctx context.Context, subscriptionId string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.cancelled = append(self.cancelled, subscriptionId)
	return nil
}

func (self *computeT3RenewFake) DetachCard(ctx context.Context, paymentMethodId string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.detached = append(self.detached, paymentMethodId)
	return nil
}

// Plans networkId's next top-ups' outcomes.
func (self *computeT3RenewFake) plan(networkId operator.Id, outcomes ...controller.ComputeOffSessionOutcome) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.plans[networkId] = append(self.plans[networkId], outcomes...)
}

// The top-ups sent for networkId, each a new key.
func (self *computeT3RenewFake) sent(networkId operator.Id) []*controller.ComputeOffSessionCharge {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*controller.ComputeOffSessionCharge{}, self.charges[networkId]...)
}

func (self *computeT3RenewFake) wasDetached(card string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Contains(self.detached, card)
}

func (self *computeT3RenewFake) wasCancelled(subscriptionId string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Contains(self.cancelled, subscriptionId)
}

// A payer check whose refusals a test sets: every member may buy but those refused.
type computeT3Payers struct {
	stateLock sync.Mutex
	refused   map[operator.Id]bool
}

func (self *computeT3Payers) MayBuy(env operator.Env, ctx context.Context, networkId operator.Id, memberId operator.Id) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return !self.refused[memberId], nil
}

func (self *computeT3Payers) refuse(memberId operator.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refused[memberId] = true
}

// A notifier recording each notice owed and each hint, by network.
type computeT3Notices struct {
	stateLock sync.Mutex
	notices   map[operator.Id][]*controller.ComputeNotice
	hints     map[operator.Id]int
}

func (self *computeT3Notices) NoticeInTx(env operator.Env, ctx context.Context, tx operator.DbTx, notice *controller.ComputeNotice) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, owed := range self.notices[notice.NetworkId] {
		if owed.Key == notice.Key {
			return nil
		}
	}
	self.notices[notice.NetworkId] = append(self.notices[notice.NetworkId], notice)
	return nil
}

func (self *computeT3Notices) HintDevices(env operator.Env, ctx context.Context, networkId operator.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hints[networkId] += 1
	return nil
}

// The kinds owed networkId, in order.
func (self *computeT3Notices) kinds(networkId operator.Id) []controller.ComputeNoticeKind {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	kinds := []controller.ComputeNoticeKind{}
	for _, notice := range self.notices[networkId] {
		kinds = append(kinds, notice.Kind)
	}
	return kinds
}

func (self *computeT3Notices) hintCount(networkId operator.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.hints[networkId]
}

// Registered once a test binary: the registries take one each.
var (
	computeT3RenewOnce sync.Once
	computeT3Renew     = &computeT3RenewFake{
		plans: map[operator.Id][]controller.ComputeOffSessionOutcome{}, keys: map[string]*controller.ComputeOffSessionAnswer{},
		charges: map[operator.Id][]*controller.ComputeOffSessionCharge{},
	}
	computeT3PayerCheck = &computeT3Payers{refused: map[operator.Id]bool{}}
	computeT3Notifier   = &computeT3Notices{notices: map[operator.Id][]*controller.ComputeNotice{}, hints: map[operator.Id]int{}}
)

// The fakes registered, and the ledger's keys.
func computeT3RenewRegister() {
	computeT3RenewOnce.Do(func() {
		controller.RegisterComputeStripeRenew(computeT3Renew)
		controller.RegisterComputeAutoRenewPayers(computeT3PayerCheck)
		controller.RegisterComputeNotifier(computeT3Notifier)
		controller.RegisterComputeLedgerSecrets(computeT3Secrets{})
	})
}

// One network with a fake clock, its payer and its card.
type computeT3Renewing struct {
	*computeT3Clocked
	networkId server.Id
	payerId   server.Id
	card      string
}

// A network whose first purchase, by its payer's card, was 8 days before now, so the new account's limits are behind
// it: its balance is that purchase's $10.
func newComputeT3Renewing(t3 *computeT3, now time.Time) *computeT3Renewing {
	computeT3RenewRegister()
	clocked := newComputeT3Clocked(t3, now.Add(-8*24*time.Hour))
	renewing := &computeT3Renewing{computeT3Clocked: clocked, networkId: server.NewId(), payerId: server.NewId(), card: "pm_t3_" + server.NewId().String()}
	clocked.purchase(renewing.networkId, computeT3Usd(10), clocked.payer("fp_"+renewing.card))
	clocked.clock.AdvanceTo(now)
	return renewing
}

// The payer saves the card for threshold with T, P and L, as a pack's checkout with threshold on does.
func (self *computeT3Renewing) saveCard(belowNanoUsd int64, packCredits int64, monthLimitNanoUsd int64) {
	self.t3.t.Helper()
	err := controller.ComputeCardSavedForAutoRenew(self.env, self.t3.ctx, &controller.ComputeCardSaved{
		NetworkId: self.networkId, PayerMemberId: self.payerId, Mode: model.ComputeAutoRenewModeThreshold,
		StripeCustomerId: "cus_t3_" + self.payerId.String(), StripePaymentMethodId: self.card, PaymentIntentId: "pi_t3_saved",
		BelowNanoUsd: belowNanoUsd, PackCredits: packCredits, MonthLimitNanoUsd: monthLimitNanoUsd,
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
}

// Spends amount: a settled charge, booked, so the account's settled falls by it.
func (self *computeT3Renewing) spend(amount int64) {
	self.t3.t.Helper()
	now := self.clock.Now()
	catalogId := "chutes-qwen3.8-27b-tee-high"
	op := &model.ComputeOp{
		OpKey: server.NewId(), NetworkId: self.networkId, State: model.ComputeOpStateSettled, Kind: model.ComputeChargeKindInference,
		SubjectRef: server.NewId(), SidecarId: server.NewId(), CatalogId: &catalogId, Provider: 1, StartTime: now,
		HoldNanoUsd: amount, ReserveNanoUsd: amount,
		Outcome: &model.ComputeOpOutcome{Units: model.ComputeUnits{InputTokens: 1000, OutputTokens: 100}, AmountNanoUsd: amount, CostNanoUsd: amount * 5 / 6, EndTime: now, SettleTime: now},
	}
	if inserted, _, err := model.ComputeOpInsertSettled(self.env, self.t3.ctx, self.settings, op); err != nil || !inserted {
		self.t3.t.Fatalf("the spend's row: %v %v", inserted, err)
	}
	if _, err := controller.ComputeBook(self.env, self.t3.ctx, self.settings, model.ComputeOpShard(self.settings, self.networkId)); err != nil {
		self.t3.t.Fatal(err)
	}
}

// One run of compute_auto_renew, the whole pass.
func (self *computeT3Renewing) run() {
	self.t3.t.Helper()
	var cursor *operator.Id
	for {
		result, err := controller.ComputeAutoRenewRun(self.env, self.t3.ctx, self.settings, cursor)
		if err != nil {
			self.t3.t.Fatal(err)
		}
		if result.Cursor == nil {
			return
		}
		cursor = result.Cursor
	}
}

// The account's settled balance.
func (self *computeT3Renewing) settled() int64 {
	self.t3.t.Helper()
	return self.t3.queryInt(`SELECT settled_nano_usd FROM compute_account WHERE network_id = $1`, self.networkId)
}

// Both modes as they stand.
func (self *computeT3Renewing) state() *controller.ComputeAutoRenewState {
	self.t3.t.Helper()
	state, err := controller.ComputeAutoRenew(self.env, self.t3.ctx, self.settings, self.networkId)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return state
}

// The network's Stripe lots of a top-up's PaymentIntents.
func (self *computeT3Renewing) topUpLots() int64 {
	self.t3.t.Helper()
	return self.t3.queryInt(`SELECT count(*) FROM compute_lot WHERE network_id = $1 AND market_ref LIKE 'pi_t3_top_up_%'`, self.networkId)
}

// A chargeback's suspension charges no saved card: a network below T with threshold on, one of its Stripe lots charged
// back, is passed over, its threshold left on for the review that lifts the suspension (§9.9, §9.11).
func TestChargebackChargesNoCard(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		renewing := newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
		renewing.saveCard(0, 0, 0)
		disputed := "pi_test_disputed_" + renewing.networkId.String()
		renewing.credit(&controller.ComputeCreditRequest{
			NetworkId: renewing.networkId, Kind: model.ComputeLotKindPurchase, Market: model.ComputeMarketStripe,
			MarketRef: disputed, AmountNanoUsd: computeT3Usd(10), PayerHash: renewing.payer("fp_" + renewing.card),
		})
		if reversed := renewing.reverse(model.ComputeMarketStripe, disputed, true); !reversed.Reversed {
			t.Fatalf("the chargeback: %+v", reversed)
		}
		renewing.spend(computeT3Usd(6))
		if stored, _ := renewing.states(); stored != model.ComputeAccountStateSuspended || renewing.settled() != computeT3Usd(4) {
			t.Fatalf("after the chargeback: %s, %d settled", stored, renewing.settled())
		}
		renewing.run()
		if sent := computeT3Renew.sent(renewing.networkId); len(sent) != 0 {
			t.Fatalf("a charged-back network's card was charged: %+v", sent)
		}
		if renewing.settled() != computeT3Usd(4) || renewing.topUpLots() != 0 || !renewing.state().Threshold.On {
			t.Fatalf("after the run: %d settled, %d lots, %+v", renewing.settled(), renewing.topUpLots(), renewing.state().Threshold)
		}
	})
}

// A network below T with threshold on is charged P once: the lot credited, the day's count one; the next run, above
// T, charges nothing, and a run's retry sends the same key, which Stripe charges once.
func TestThresholdFiresOnceBelowT(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		renewing := newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
		renewing.saveCard(0, 0, 0)
		renewing.spend(computeT3Usd(6))
		if renewing.settled() != computeT3Usd(4) {
			t.Fatalf("the balance before: %d", renewing.settled())
		}
		renewing.run()
		sent := computeT3Renew.sent(renewing.networkId)
		if len(sent) != 1 || sent[0].PackCredits != renewing.settings.ThresholdPackCredits || sent[0].StripePaymentMethodId != renewing.card ||
			sent[0].PayerMemberId != renewing.payerId {
			t.Fatalf("the top-ups sent: %+v", sent)
		}
		if renewing.settled() != computeT3Usd(24) || renewing.topUpLots() != 1 {
			t.Fatalf("after the top-up: %d settled, %d lots", renewing.settled(), renewing.topUpLots())
		}
		if state := renewing.state(); state.Threshold.TopUpsToday != 1 || state.Threshold.MonthNanoUsd != computeT3Usd(20) {
			t.Fatalf("the day's counts: %+v", state.Threshold)
		}
		renewing.run()
		if len(computeT3Renew.sent(renewing.networkId)) != 1 {
			t.Fatal("a run above T charged")
		}
		// the webhook's success for the same top-up credits nothing again
		answer, err := controller.ComputeTopUpCredit(renewing.env, t3.ctx, renewing.settings, &controller.ComputeTopUpCreditRequest{
			NetworkId: renewing.networkId, PaymentIntentId: computeT3Renew.keys[computeT3TopUpKeyOf(sent[0])].PaymentIntentId, AmountNanoUsd: computeT3Usd(20),
		})
		if err != nil || answer.Credited || renewing.settled() != computeT3Usd(24) || renewing.state().Threshold.TopUpsToday != 1 {
			t.Fatalf("the webhook's repeat: %+v %v", answer, err)
		}
	})
}

// A sent charge's idempotency key, as the fake recorded it.
func computeT3TopUpKeyOf(charge *controller.ComputeOffSessionCharge) string {
	return charge.IdempotencyKey
}

// With T above what three top-ups reach, a UTC day takes 3 and no more; the next day takes more until the month's L.
func TestThresholdAtMostThreeADayAndLAMonth(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		// noon on the 2nd of next month, so the next day is in the same month
		now := time.Now().UTC()
		second := time.Date(now.Year(), now.Month(), 2, 12, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		renewing := newComputeT3Renewing(t3, second)
		renewing.saveCard(computeT3Usd(1_000), 60, computeT3Usd(200))
		for i := 0; i < 4; i += 1 {
			renewing.run()
		}
		if sent := computeT3Renew.sent(renewing.networkId); len(sent) != renewing.settings.ThresholdDayMaxCount {
			t.Fatalf("one day sent %d top-ups", len(sent))
		}
		if state := renewing.state(); state.Threshold.TopUpsToday != 3 || state.Threshold.MonthNanoUsd != computeT3Usd(180) {
			t.Fatalf("the day's counts: %+v", state.Threshold)
		}
		renewing.clock.Advance(24 * time.Hour)
		renewing.run()
		if sent := computeT3Renew.sent(renewing.networkId); len(sent) != 3 {
			t.Fatalf("a top-up past L was sent: %d in all", len(sent))
		}
		if state := renewing.state(); state.Threshold.TopUpsToday != 0 || state.Threshold.MonthNanoUsd != computeT3Usd(180) {
			t.Fatalf("the next day's counts: %+v", state.Threshold)
		}
	})
}

// Three failures in a row: the first retried after 1 h, the second after 6 h, the third turning threshold off, each
// emailed; a run before a retry's time sends nothing.
func TestThresholdRetriesAtOneAndSixHoursThenTurnsOff(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		renewing := newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
		renewing.saveCard(0, 0, 0)
		renewing.spend(computeT3Usd(6))
		failed := controller.ComputeOffSessionFailed
		computeT3Renew.plan(renewing.networkId, failed, failed, failed)
		renewing.run()
		state := renewing.state().Threshold
		if !state.On || state.FailuresToday != 1 || state.NextRetryTime == nil || !state.NextRetryTime.Equal(renewing.clock.Now().Add(time.Hour)) {
			t.Fatalf("after the first failure: %+v", state)
		}
		renewing.clock.Advance(30 * time.Minute)
		renewing.run()
		if len(computeT3Renew.sent(renewing.networkId)) != 1 {
			t.Fatal("a run before the retry's time charged")
		}
		renewing.clock.Advance(30 * time.Minute)
		renewing.run()
		state = renewing.state().Threshold
		if len(computeT3Renew.sent(renewing.networkId)) != 2 || state.NextRetryTime == nil || !state.NextRetryTime.Equal(renewing.clock.Now().Add(6*time.Hour)) {
			t.Fatalf("after the second failure: %+v", state)
		}
		renewing.clock.Advance(6 * time.Hour)
		renewing.run()
		state = renewing.state().Threshold
		if len(computeT3Renew.sent(renewing.networkId)) != 3 || state.On || state.OffReason == nil || *state.OffReason != model.ComputeAutoRenewOffFailed {
			t.Fatalf("after the third failure: %+v", state)
		}
		kinds := computeT3Notifier.kinds(renewing.networkId)
		want := []controller.ComputeNoticeKind{controller.ComputeNoticeTopUpFailed, controller.ComputeNoticeTopUpFailed, controller.ComputeNoticeTopUpOff}
		if !slices.Equal(kinds, want) {
			t.Fatalf("the notices: %v", kinds)
		}
		renewing.clock.Advance(24 * time.Hour)
		renewing.run()
		if len(computeT3Renew.sent(renewing.networkId)) != 3 {
			t.Fatal("threshold off still charged")
		}
	})
}

// The bank's authentication_required answer emails its notice and turns threshold off at once, until its holder buys
// again with it on.
func TestAuthenticationRequiredEmailsAndTurnsThresholdOff(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		renewing := newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
		renewing.saveCard(0, 0, 0)
		renewing.spend(computeT3Usd(6))
		computeT3Renew.plan(renewing.networkId, controller.ComputeOffSessionAuthenticationRequired)
		renewing.run()
		state := renewing.state().Threshold
		if state.On || state.OffReason == nil || *state.OffReason != model.ComputeAutoRenewOffAuthenticationRequired {
			t.Fatalf("after authentication_required: %+v", state)
		}
		if kinds := computeT3Notifier.kinds(renewing.networkId); !slices.Equal(kinds, []controller.ComputeNoticeKind{controller.ComputeNoticeAuthenticationRequired}) {
			t.Fatalf("the notices: %v", kinds)
		}
		renewing.clock.Advance(2 * time.Hour)
		renewing.run()
		if len(computeT3Renew.sent(renewing.networkId)) != 1 {
			t.Fatal("threshold off charged again")
		}
		// its holder turns it on again with the card it saved
		answer, err := controller.ComputeSetAutoRenew(renewing.env, t3.ctx, renewing.settings, &controller.ComputeAutoRenewChange{
			NetworkId: renewing.networkId, MemberId: &renewing.payerId, Mode: model.ComputeAutoRenewModeThreshold, On: true,
		})
		if err != nil || answer.Refusal != "" || !answer.State.Threshold.On || answer.State.Threshold.OffReason != nil {
			t.Fatalf("its holder turning it on: %+v %v", answer, err)
		}
	})
}

// $7.50 with threshold on and one call held at a $2.58 worst case: no card is charged, since the balance leaves holds
// out, and no state moves (R1-33).
func TestOneCentCallChargesNoCard(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		renewing := newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
		renewing.saveCard(0, 0, 0)
		renewing.spend(computeT3Usd(2.5))
		budgets := func(ctx context.Context, subjects *controller.ComputeBudgetSubjects) ([]*model.ComputeBudgetLine, error) {
			return model.ComputeDefaultBudgetLines(renewing.settings, &model.ComputeBudgetCaller{SidecarId: subjects.SidecarId, AgentId: subjects.AgentId})
		}
		if rebuilt, err := controller.ComputeRebuild(renewing.env, t3.ctx, renewing.settings, renewing.networkId, budgets); err != nil || !rebuilt {
			t.Fatalf("the rebuild: %v %v", rebuilt, err)
		}
		catalogId := "chutes-qwen3.8-27b-tee-high"
		sidecarId := server.NewId()
		lines, err := model.ComputeDefaultBudgetLines(renewing.settings, &model.ComputeBudgetCaller{SidecarId: sidecarId})
		if err != nil {
			t.Fatal(err)
		}
		hold := computeT3Usd(2.58)
		answer, err := controller.ComputeHold(renewing.env, t3.ctx, renewing.settings, &controller.ComputeHoldRequest{
			NetworkId: renewing.networkId, Kind: model.ComputeChargeKindInference, Path: model.ComputeHoldPathTee, SubjectRef: server.NewId(),
			SidecarId: sidecarId, CatalogId: &catalogId, Provider: 1, HoldNanoUsd: hold, ReserveNanoUsd: hold, Lines: lines,
		})
		if err != nil || !answer.Admitted {
			t.Fatalf("the call's hold: %+v %v", answer, err)
		}
		stateBefore := renewing.state()
		renewing.run()
		if len(computeT3Renew.sent(renewing.networkId)) != 0 {
			t.Fatal("a held call charged the card")
		}
		change, err := controller.ComputeSuspendEvaluate(renewing.env, t3.ctx, renewing.settings, renewing.networkId)
		if err != nil || change.Changed || change.To != model.ComputeAccountStateActive || change.BalanceNanoUsd != computeT3Usd(7.5) {
			t.Fatalf("the state with a hold open: %+v %v", change, err)
		}
		if after := renewing.state(); after.Threshold.TopUpsToday != stateBefore.Threshold.TopUpsToday {
			t.Fatal("a held call moved threshold's counts")
		}
	})
}

// A Billing admin's change of the owner's threshold, turning it on or changing P, is refused card_required and charges
// nothing of the owner's card at the admin's figures; the admin may turn it off; only the card's holder turns it on.
func TestBillingAdminChangeChargesNoOtherMembersCard(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		renewing := newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
		renewing.saveCard(0, 0, 0)
		admin := server.NewId()
		set := func(memberId server.Id, on bool, pack int64) *controller.ComputeAutoRenewChangeAnswer {
			t.Helper()
			answer, err := controller.ComputeSetAutoRenew(renewing.env, t3.ctx, renewing.settings, &controller.ComputeAutoRenewChange{
				NetworkId: renewing.networkId, MemberId: &memberId, Mode: model.ComputeAutoRenewModeThreshold, On: on, PackCredits: pack,
			})
			if err != nil {
				t.Fatal(err)
			}
			return answer
		}
		if answer := set(admin, true, 100); answer.Refusal != controller.ComputeAutoRenewRefusalCardRequired {
			t.Fatalf("the admin raising P: %+v", answer)
		}
		if answer := set(admin, false, 0); answer.Refusal != "" || answer.State.Threshold.On {
			t.Fatalf("the admin turning it off: %+v", answer)
		}
		if answer := set(admin, true, 0); answer.Refusal != controller.ComputeAutoRenewRefusalCardRequired {
			t.Fatalf("the admin turning it on: %+v", answer)
		}
		if answer := set(renewing.payerId, true, 0); answer.Refusal != "" || !answer.State.Threshold.On || answer.State.Threshold.PackCredits != renewing.settings.ThresholdPackCredits {
			t.Fatalf("its holder turning it on: %+v", answer)
		}
		renewing.spend(computeT3Usd(6))
		renewing.run()
		sent := computeT3Renew.sent(renewing.networkId)
		if len(sent) != 1 || sent[0].PackCredits != renewing.settings.ThresholdPackCredits || sent[0].PayerMemberId != renewing.payerId {
			t.Fatalf("the top-ups sent: %+v", sent)
		}
	})
}

// Before a charge the payer is checked: one who may no longer buy has their card detached, threshold off with
// payer_left, the billing contacts told, and nothing charged.
func TestPayerWhoLeftIsDetached(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		renewing := newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
		renewing.saveCard(0, 0, 0)
		renewing.spend(computeT3Usd(6))
		computeT3PayerCheck.refuse(renewing.payerId)
		renewing.run()
		state := renewing.state().Threshold
		if state.On || state.OffReason == nil || *state.OffReason != model.ComputeAutoRenewOffPayerLeft || state.PayerMemberId != nil {
			t.Fatalf("after the payer left: %+v", state)
		}
		if !computeT3Renew.wasDetached(renewing.card) || len(computeT3Renew.sent(renewing.networkId)) != 0 {
			t.Fatal("the card was not detached, or was charged")
		}
		if kinds := computeT3Notifier.kinds(renewing.networkId); !slices.Contains(kinds, controller.ComputeNoticeCardDetached) {
			t.Fatalf("the notices: %v", kinds)
		}
		// its holder cannot turn threshold on again without saving a card
		answer, err := controller.ComputeSetAutoRenew(renewing.env, t3.ctx, renewing.settings, &controller.ComputeAutoRenewChange{
			NetworkId: renewing.networkId, MemberId: &renewing.payerId, Mode: model.ComputeAutoRenewModeThreshold, On: true,
		})
		if err != nil || answer.Refusal != controller.ComputeAutoRenewRefusalCardRequired {
			t.Fatalf("on again with no card: %+v %v", answer, err)
		}
	})
}

// A month's grant is credited once by its invoice's PaymentIntent however often it is delivered, and the next month's
// adds to what is left: unused credit carries over and never lapses.
func TestMonthlyGrantCreditedOnceAndCarriedOver(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		renewing := newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
		subscriptionId := "sub_t3_" + renewing.networkId.String()
		subscribe := &controller.ComputeMonthlySubscription{
			NetworkId: renewing.networkId, PayerMemberId: renewing.payerId, Credits: 25,
			StripeCustomerId: "cus_t3_" + renewing.payerId.String(), StripeSubscriptionId: subscriptionId,
		}
		for i := 0; i < 2; i += 1 {
			if err := controller.ComputeMonthlySubscribed(renewing.env, t3.ctx, renewing.settings, subscribe); err != nil {
				t.Fatal(err)
			}
		}
		if state := renewing.state().Monthly; !state.On || state.Credits != 25 || state.Market != model.ComputeMarketStripe {
			t.Fatalf("the plan: %+v", state)
		}
		grant := func(intentId string) *controller.ComputeCreditAnswer {
			t.Helper()
			answer, err := controller.ComputeMonthlyGrant(renewing.env, t3.ctx, renewing.settings, &controller.ComputeMonthlyGrantRequest{
				NetworkId: renewing.networkId, PaymentIntentId: intentId, Credits: 25,
			})
			if err != nil {
				t.Fatal(err)
			}
			return answer
		}
		first := grant("pi_t3_month_1")
		if !first.Credited || first.Lot.Kind != model.ComputeLotKindSubscription || first.Lot.ExpireTime != nil {
			t.Fatalf("the first month: %+v", first)
		}
		if again := grant("pi_t3_month_1"); again.Credited {
			t.Fatal("a month delivered twice credited twice")
		}
		renewing.clock.Advance(31 * 24 * time.Hour)
		if next := grant("pi_t3_month_2"); !next.Credited {
			t.Fatal("the next month was not credited")
		}
		if renewing.settled() != computeT3Usd(60) {
			t.Fatalf("the balance after two months: %d", renewing.settled())
		}
	})
}

// Turning threshold off before a run sends nothing; a top-up already sent and not final is still credited when its
// money is; turning the plan off cancels its subscription, and an invoice paid before still grants its month.
func TestTurningOffCancelsOnlyUnsentCharges(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		renewing := newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
		renewing.saveCard(0, 0, 0)
		renewing.spend(computeT3Usd(6))
		computeT3Renew.plan(renewing.networkId, controller.ComputeOffSessionProcessing)
		renewing.run()
		sent := computeT3Renew.sent(renewing.networkId)
		if len(sent) != 1 || renewing.topUpLots() != 0 {
			t.Fatalf("a processing top-up: %d sent, %d lots", len(sent), renewing.topUpLots())
		}
		off, err := controller.ComputeSetAutoRenew(renewing.env, t3.ctx, renewing.settings, &controller.ComputeAutoRenewChange{
			NetworkId: renewing.networkId, MemberId: &renewing.payerId, Mode: model.ComputeAutoRenewModeThreshold, On: false,
		})
		if err != nil || off.State.Threshold.On {
			t.Fatalf("turning threshold off: %+v %v", off, err)
		}
		renewing.clock.Advance(2 * time.Hour)
		renewing.run()
		if len(computeT3Renew.sent(renewing.networkId)) != 1 {
			t.Fatal("threshold off sent a charge")
		}
		// the sent one's money is final: the webhook credits it
		intentId := computeT3Renew.keys[sent[0].IdempotencyKey].PaymentIntentId
		if answer, err := controller.ComputeTopUpCredit(renewing.env, t3.ctx, renewing.settings, &controller.ComputeTopUpCreditRequest{
			NetworkId: renewing.networkId, PaymentIntentId: intentId, AmountNanoUsd: computeT3Usd(20),
		}); err != nil || !answer.Credited {
			t.Fatalf("the sent top-up's money: %+v %v", answer, err)
		}
		// the plan
		subscriptionId := "sub_t3_off_" + renewing.networkId.String()
		if err := controller.ComputeMonthlySubscribed(renewing.env, t3.ctx, renewing.settings, &controller.ComputeMonthlySubscription{
			NetworkId: renewing.networkId, PayerMemberId: renewing.payerId, Credits: 10,
			StripeCustomerId: "cus_t3_" + renewing.payerId.String(), StripeSubscriptionId: subscriptionId,
		}); err != nil {
			t.Fatal(err)
		}
		monthlyOff, err := controller.ComputeSetAutoRenew(renewing.env, t3.ctx, renewing.settings, &controller.ComputeAutoRenewChange{
			NetworkId: renewing.networkId, MemberId: &renewing.payerId, Mode: model.ComputeAutoRenewModeMonthly, On: false,
		})
		if err != nil || monthlyOff.State.Monthly.On || !computeT3Renew.wasCancelled(subscriptionId) {
			t.Fatalf("turning the plan off: %+v %v", monthlyOff, err)
		}
		if answer, err := controller.ComputeMonthlyGrant(renewing.env, t3.ctx, renewing.settings, &controller.ComputeMonthlyGrantRequest{
			NetworkId: renewing.networkId, PaymentIntentId: "pi_t3_paid_before_off", Credits: 10,
		}); err != nil || !answer.Credited {
			t.Fatalf("the month paid before: %+v %v", answer, err)
		}
		// the plan on again only through its checkout
		if on, err := controller.ComputeSetAutoRenew(renewing.env, t3.ctx, renewing.settings, &controller.ComputeAutoRenewChange{
			NetworkId: renewing.networkId, MemberId: &renewing.payerId, Mode: model.ComputeAutoRenewModeMonthly, On: true,
		}); err != nil || on.Refusal != controller.ComputeAutoRenewRefusalCardRequired {
			t.Fatalf("the plan on again: %+v %v", on, err)
		}
	})
}
