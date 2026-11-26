//go:build sidecar_pending

package computet3

// §9.9's states on the private stack's PostgreSQL and Redis with a fake clock and fakes of ENC's rates and listener
// (BAL-M12-12; DESIGN-COMPUTE §9.9, D-BAL-5, D-BAL-9, R1-33, R1-46): the states follow the balance, holds left out;
// grace refuses calls and searches through the projection's state; suspended at minus the floor, never at the floor;
// grace older than its hour suspends; a top-up returns the account to active at once and asks for a restart; each
// change notifies once with two workers racing; the floor the smaller of $5 and an hour of the enclaves' rates; burn
// the larger of the day's charges and the rates.

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// ENC's rates and listener standing in: each network's hourly rate a test sets, and the stops and restarts asked.
type computeT3Enclaves struct {
	stateLock sync.Mutex
	hourly    map[operator.Id]int64
	suspended map[operator.Id]int
	resumed   map[operator.Id]int
}

func (self *computeT3Enclaves) Rates(env operator.Env, ctx context.Context, networkId operator.Id) (*controller.ComputeEnclaveRateSet, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return &controller.ComputeEnclaveRateSet{HourlyNanoUsd: self.hourly[networkId]}, nil
}

func (self *computeT3Enclaves) Suspended(env operator.Env, ctx context.Context, networkId operator.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.suspended[networkId] += 1
	return nil
}

func (self *computeT3Enclaves) Resumed(env operator.Env, ctx context.Context, networkId operator.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.resumed[networkId] += 1
	return nil
}

func (self *computeT3Enclaves) rate(networkId operator.Id, hourly int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hourly[networkId] = hourly
}

func (self *computeT3Enclaves) counts(networkId operator.Id) (suspended int, resumed int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.suspended[networkId], self.resumed[networkId]
}

var (
	computeT3EnclavesOnce sync.Once
	computeT3EnclaveSet   = &computeT3Enclaves{hourly: map[operator.Id]int64{}, suspended: map[operator.Id]int{}, resumed: map[operator.Id]int{}}
)

// A network for the states, its fakes registered.
func newComputeT3States(t3 *computeT3) *computeT3Renewing {
	computeT3EnclavesOnce.Do(func() {
		controller.RegisterComputeEnclaveRates(computeT3EnclaveSet)
		controller.RegisterComputeSuspendListener(computeT3EnclaveSet)
	})
	return newComputeT3Renewing(t3, computeT3Midday(time.Now().UTC()))
}

// One review of the network's account.
func (self *computeT3Renewing) review() *controller.ComputeStateChange {
	self.t3.t.Helper()
	change, err := controller.ComputeSuspendEvaluate(self.env, self.t3.ctx, self.settings, self.networkId)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return change
}

// One whole pass of compute_suspend.
func (self *computeT3Renewing) suspendPass() {
	self.t3.t.Helper()
	var cursor *operator.Id
	for {
		result, err := controller.ComputeSuspendRun(self.env, self.t3.ctx, self.settings, cursor)
		if err != nil {
			self.t3.t.Fatal(err)
		}
		if result.Cursor == nil {
			return
		}
		cursor = result.Cursor
	}
}

// The account's stored state and the projection's.
func (self *computeT3Renewing) states() (model.ComputeAccountState, model.ComputeAccountState) {
	self.t3.t.Helper()
	stored, err := controller.ComputeAccountStateOf(self.env, self.t3.ctx, self.networkId)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	projection, err := model.ComputeProjectionRead(self.env, self.t3.ctx, self.networkId)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return stored, projection.State
}

// The projection rebuilt from the ledger, with §16.5's default budgets.
func (self *computeT3Renewing) rebuild() {
	self.t3.t.Helper()
	budgets := func(ctx context.Context, subjects *controller.ComputeBudgetSubjects) ([]*model.ComputeBudgetLine, error) {
		return model.ComputeDefaultBudgetLines(self.settings, &model.ComputeBudgetCaller{SidecarId: subjects.SidecarId, AgentId: subjects.AgentId})
	}
	if rebuilt, err := controller.ComputeRebuild(self.env, self.t3.ctx, self.settings, self.networkId, budgets); err != nil || !rebuilt {
		self.t3.t.Fatalf("the rebuild: %v %v", rebuilt, err)
	}
}

// A TEE call's hold of amount on a sidecar of the network's own.
func (self *computeT3Renewing) holdCall(amount int64) *controller.ComputeHoldAnswer {
	self.t3.t.Helper()
	catalogId := "chutes-qwen3.8-27b-tee-high"
	sidecarId := server.NewId()
	lines, err := model.ComputeDefaultBudgetLines(self.settings, &model.ComputeBudgetCaller{SidecarId: sidecarId})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	answer, err := controller.ComputeHold(self.env, self.t3.ctx, self.settings, &controller.ComputeHoldRequest{
		NetworkId: self.networkId, Kind: model.ComputeChargeKindInference, Path: model.ComputeHoldPathTee, SubjectRef: server.NewId(),
		SidecarId: sidecarId, CatalogId: &catalogId, Provider: 1, HoldNanoUsd: amount, ReserveNanoUsd: amount, Lines: lines,
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return answer
}

// An open hold of nearly the whole balance moves no state; spending below T moves it to low, into the projection,
// emailed once and hinted; a pass of compute_suspend over an unmoved account changes nothing more.
func TestStatesFollowTheBalanceNotHolds(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		states := newComputeT3States(t3)
		states.rebuild()
		if answer := states.holdCall(computeT3Usd(9.5)); !answer.Admitted {
			t.Fatalf("the hold: %+v", answer.Refusal)
		}
		if change := states.review(); change.Changed || change.To != model.ComputeAccountStateActive || change.BalanceNanoUsd != computeT3Usd(10) {
			t.Fatalf("a hold moved the state: %+v", change)
		}
		states.spend(computeT3Usd(6))
		states.suspendPass()
		if stored, projected := states.states(); stored != model.ComputeAccountStateLow || projected != model.ComputeAccountStateLow {
			t.Fatalf("below T: stored %s, projected %s", stored, projected)
		}
		if kinds := computeT3Notifier.kinds(states.networkId); !slices.Equal(kinds, []controller.ComputeNoticeKind{controller.ComputeNoticeLow}) {
			t.Fatalf("the notices: %v", kinds)
		}
		if computeT3Notifier.hintCount(states.networkId) != 1 {
			t.Fatalf("%d hints", computeT3Notifier.hintCount(states.networkId))
		}
		states.suspendPass()
		if len(computeT3Notifier.kinds(states.networkId)) != 1 || computeT3Notifier.hintCount(states.networkId) != 1 {
			t.Fatal("an unmoved account notified again")
		}
	})
}

// At or under zero with a floor, an account is in grace: the projection's state refuses new calls and searches,
// while an enclave's interval still holds down to minus the floor.
func TestGraceRefusesCallsAndSearches(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		states := newComputeT3States(t3)
		computeT3EnclaveSet.rate(states.networkId, computeT3Usd(3))
		states.spend(computeT3Usd(11))
		states.rebuild()
		if change := states.review(); change.To != model.ComputeAccountStateGrace || change.FloorNanoUsd != computeT3Usd(3) {
			t.Fatalf("at -$1 with a $3 floor: %+v", change)
		}
		if answer := states.holdCall(computeT3Usd(0.01)); answer.Admitted || answer.Refusal == nil || answer.Refusal.Reason != controller.ComputeRefusalBalance {
			t.Fatalf("a call in grace: %+v", answer)
		}
		network := &computeT3Network{t3: t3, settings: states.settings, networkId: states.networkId, sidecarId: server.NewId()}
		if outcome := network.classHold(server.NewId(), computeT3Usd(0.01), "search", nil); outcome != "balance" {
			t.Fatalf("a search in grace: %s", outcome)
		}
		if outcome := network.classHold(server.NewId(), computeT3Usd(1), "interval", nil); outcome != "taken" {
			t.Fatalf("an interval in grace: %s", outcome)
		}
		if kinds := computeT3Notifier.kinds(states.networkId); !slices.Contains(kinds, controller.ComputeNoticeGrace) {
			t.Fatalf("the notices: %v", kinds)
		}
	})
}

// With a $3 floor, a balance of $3 is no suspension, one of -$3 is, and its enclaves are stopped once.
func TestSuspendedAtMinusFloorNotAtTheFloor(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		states := newComputeT3States(t3)
		computeT3EnclaveSet.rate(states.networkId, computeT3Usd(3))
		states.spend(computeT3Usd(7))
		if change := states.review(); change.To == model.ComputeAccountStateSuspended || change.BalanceNanoUsd != computeT3Usd(3) {
			t.Fatalf("at the floor: %+v", change)
		}
		states.spend(computeT3Usd(6))
		if change := states.review(); change.To != model.ComputeAccountStateSuspended {
			t.Fatalf("at minus the floor: %+v", change)
		}
		states.review()
		if suspended, _ := computeT3EnclaveSet.counts(states.networkId); suspended != 1 {
			t.Fatalf("the enclaves stopped %d times", suspended)
		}
	})
}

// Grace an hour old suspends, a minute short of it does not.
func TestGraceOlderThanAnHourSuspends(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		states := newComputeT3States(t3)
		computeT3EnclaveSet.rate(states.networkId, computeT3Usd(3))
		states.spend(computeT3Usd(11))
		if change := states.review(); change.To != model.ComputeAccountStateGrace {
			t.Fatalf("at -$1: %+v", change)
		}
		states.clock.Advance(states.settings.GraceMaxAge - time.Minute)
		if change := states.review(); change.To != model.ComputeAccountStateGrace {
			t.Fatalf("grace a minute short of its hour: %+v", change)
		}
		states.clock.Advance(time.Minute)
		if change := states.review(); change.To != model.ComputeAccountStateSuspended {
			t.Fatalf("grace an hour old: %+v", change)
		}
	})
}

// A credit that lifts a suspended balance above zero returns the account to active at once, into the projection, and
// asks the listener to restart what the suspension stopped.
func TestTopUpReturnsToActiveAndAsksForRestart(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		states := newComputeT3States(t3)
		states.spend(computeT3Usd(12))
		states.rebuild()
		if change := states.review(); change.To != model.ComputeAccountStateSuspended {
			t.Fatalf("at -$2 with no floor: %+v", change)
		}
		states.purchase(states.networkId, computeT3Usd(50), nil)
		if stored, projected := states.states(); stored != model.ComputeAccountStateActive || projected != model.ComputeAccountStateActive {
			t.Fatalf("after the top-up: stored %s, projected %s", stored, projected)
		}
		if _, resumed := computeT3EnclaveSet.counts(states.networkId); resumed != 1 {
			t.Fatalf("the restart was asked %d times", resumed)
		}
		if answer := states.holdCall(computeT3Usd(1)); !answer.Admitted {
			t.Fatalf("a call after the top-up: %+v", answer.Refusal)
		}
	})
}

// Two workers reviewing one account at once make its change once: one move, one notice.
func TestEachStateChangeNotifiesOnce(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		states := newComputeT3States(t3)
		states.spend(computeT3Usd(6))
		changes := make(chan *controller.ComputeStateChange, 2)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for i := 0; i < 2; i += 1 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				change, err := controller.ComputeSuspendEvaluate(states.env, t3.ctx, states.settings, states.networkId)
				if err != nil {
					t.Error(err)
					return
				}
				changes <- change
			}()
		}
		close(start)
		workers.Wait()
		close(changes)
		changed := 0
		for change := range changes {
			if change.Changed {
				changed += 1
			}
		}
		if changed != 1 {
			t.Fatalf("%d workers made the change", changed)
		}
		if kinds := computeT3Notifier.kinds(states.networkId); !slices.Equal(kinds, []controller.ComputeNoticeKind{controller.ComputeNoticeLow}) {
			t.Fatalf("the notices: %v", kinds)
		}
	})
}

// The floor written to the account and the projection is the smaller of $5 and an hour of the running enclaves' rates.
func TestFloorIsTheSmallerOfFiveDollarsAndAnHour(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		states := newComputeT3States(t3)
		states.rebuild()
		computeT3EnclaveSet.rate(states.networkId, computeT3Usd(3))
		if change := states.review(); change.FloorNanoUsd != computeT3Usd(3) {
			t.Fatalf("an hour of $3: %+v", change)
		}
		if projection, err := model.ComputeProjectionRead(states.env, t3.ctx, states.networkId); err != nil || projection.FloorNanoUsd != computeT3Usd(3) {
			t.Fatalf("the projection's floor: %+v %v", projection, err)
		}
		computeT3EnclaveSet.rate(states.networkId, computeT3Usd(19))
		if change := states.review(); change.FloorNanoUsd != states.settings.FloorMaxNanoUsd {
			t.Fatalf("an hour of $19: %+v", change)
		}
		if floor, err := controller.ComputeFloor(states.env, t3.ctx, states.settings, states.networkId); err != nil || floor != computeT3Usd(5) {
			t.Fatalf("ComputeFloor: %d %v", floor, err)
		}
	})
}

// Burn is the trailing day's charges, or the running enclaves' rates over a day where that is larger.
func TestBurnTakesTheLarger(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		states := newComputeT3States(t3)
		states.spend(computeT3Usd(3))
		computeT3EnclaveSet.rate(states.networkId, computeT3Usd(0.1))
		if burn, err := controller.ComputeBurn(states.env, t3.ctx, states.settings, states.networkId); err != nil || burn != computeT3Usd(3) {
			t.Fatalf("charges above the rates: %d %v", burn, err)
		}
		computeT3EnclaveSet.rate(states.networkId, computeT3Usd(1))
		if burn, err := controller.ComputeBurn(states.env, t3.ctx, states.settings, states.networkId); err != nil || burn != computeT3Usd(24) {
			t.Fatalf("rates above the charges: %d %v", burn, err)
		}
		// a day later the charges have left the window
		computeT3EnclaveSet.rate(states.networkId, 0)
		states.clock.Advance(states.settings.BurnWindow + time.Minute)
		if burn, err := controller.ComputeBurn(states.env, t3.ctx, states.settings, states.networkId); err != nil || burn != 0 {
			t.Fatalf("a day later: %d %v", burn, err)
		}
	})
}
