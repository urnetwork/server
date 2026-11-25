//go:build sidecar_pending

package computet3

// The {cmp_<network_id>} projection's scripts on the private stack's Redis, running the real Lua (BAL-M12-02;
// DESIGN-COMPUTE §9.7, D-BAL-13): the balance down to zero and the floor kept for enclave intervals, held where open
// holds or reservations stand in the way, each budget line at its last cents and parallel holds never spending one
// room twice, grace refusing calls and searches, an unconfirmed hold past its age dropped by the next script, each
// script acting once per operation key, the drop moving settled only forward, and an absent or marked projection
// answering held. The classes no built path takes yet (searches, enclave intervals) run through scripts of this
// file's own over the exported prelude, as BAL-M13-02's and BAL-M14-01's will.

import (
	"errors"
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

const computeT3Nano = int64(1)

// Dollars and cents in nano-USD.
func computeT3Usd(dollars float64) int64 {
	return int64(math.Round(dollars*1_000_000)) * 1_000
}

var (
	// Redis TIME, in ms.
	computeT3TimeScript = &operator.RedisScript{
		Name:   "compute-t3-time",
		Source: model.ComputeHoldPrelude() + "\nreturn {cmp_str(cmp_now())}\n",
	}
	// Plants a hold as the hold script would have taken it ARGV[3] ms before Redis TIME, unconfirmed.
	// ARGV: op, amount, age ms, the line count, then per line its subject, budget, period letter and cap.
	computeT3PlantScript = &operator.RedisScript{
		Name: "compute-t3-plant",
		Source: model.ComputeHoldPrelude() + `
local at = cmp_now() - cmp_int(ARGV[3], 'the age')
local amount = cmp_int(ARGV[2], 'the amount')
cmp_take(ARGV[1], '1', amount, amount, cmp_lines(4), cmp_periods(at), at)
return {'planted'}
`,
	}
	// A hold of another class through the prelude: ARGV[3] is search or interval; budgets bind searches, never an
	// interval. ARGV: op, amount, class, the line count, then the lines.
	computeT3ClassHoldScript = &operator.RedisScript{
		Name: "compute-t3-class-hold",
		Source: model.ComputeHoldPrelude() + `
local op = ARGV[1]
local amount = cmp_int(ARGV[2], 'the amount')
local class = ARGV[3]
local lines = cmp_lines(4)
local now = cmp_now()
if not cmp_admits() then
	return {'held'}
end
local first = cmp_repeat_hold(op)
if first then
	return {first[1]}
end
local periods = cmp_periods(now)
local refused = cmp_check_balance(class, amount)
if not refused and class ~= 'interval' then
	refused = cmp_check_budgets(lines, amount, periods)
end
if refused then
	return {refused}
end
cmp_take(op, '1', amount, amount, lines, periods, now)
return {'taken'}
`,
	}
	// The unconfirmed set's members.
	computeT3UnconfirmedScript = &operator.RedisScript{
		Name:   "compute-t3-unconfirmed",
		Source: "return redis.call('ZRANGE', KEYS[1], 0, -1)\n",
	}
)

// One network's projection under test.
type computeT3Network struct {
	t3        *computeT3
	settings  *model.ComputeLedgerSettings
	networkId server.Id
	sidecarId server.Id
}

func newComputeT3Network(t3 *computeT3) *computeT3Network {
	return &computeT3Network{t3: t3, settings: model.DefaultComputeLedgerSettings(), networkId: server.NewId(), sidecarId: server.NewId()}
}

// Redis TIME.
func (self *computeT3Network) redisNow() time.Time {
	reply, err := self.t3.redis.RunScript(self.t3.ctx, computeT3TimeScript, []string{model.ComputeProjectionKey(self.networkId)}, nil)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	ms, err := strconv.ParseInt(string(reply.Replies[0].Bulk), 10, 64)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return time.UnixMilli(ms).UTC()
}

// Writes the projection from a snapshot of settled, the floor and the state, and the counters given.
func (self *computeT3Network) rebuild(version int64, settled int64, floor int64, promo int64, state model.ComputeAccountState, counters ...*model.ComputeProjectionCounter) {
	self.t3.t.Helper()
	err := model.ComputeProjectionRebuild(self.t3.env, self.t3.ctx, self.settings, &model.ComputeProjectionSnapshot{
		NetworkId: self.networkId,
		Account: &model.ComputeProjectionAccount{
			Version: version, SettledNanoUsd: settled, PromoNanoUsd: promo, FloorNanoUsd: floor, State: state,
		},
		Counters: counters,
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
}

// A TEE call's hold of amount, reserved on lines.
func (self *computeT3Network) hold(opKey server.Id, amount int64, lines []*model.ComputeBudgetLine) *model.ComputeHoldResult {
	self.t3.t.Helper()
	result, err := model.ComputeProjectionHold(self.t3.env, self.t3.ctx, self.settings, &model.ComputeHoldRequest{
		NetworkId: self.networkId, OpKey: opKey, Kind: model.ComputeChargeKindInference, Path: model.ComputeHoldPathTee,
		HoldNanoUsd: amount, ReserveNanoUsd: amount, Lines: lines,
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return result
}

// The lines' ARGV for the test scripts: the count, then subject, budget, letter and cap each.
func computeT3LineArgs(lines []*model.ComputeBudgetLine) []string {
	args := []string{strconv.Itoa(len(lines))}
	for _, line := range lines {
		letter := "d"
		if line.Period == model.ComputeBudgetPeriodMonth {
			letter = "m"
		}
		args = append(args, computeT3Hex(line.SubjectId), line.Budget, letter, strconv.FormatInt(line.CapNanoUsd, 10))
	}
	return args
}

// The keys a call over lines declares: the projection, the unconfirmed set, each subject's counters.
func (self *computeT3Network) keys(lines []*model.ComputeBudgetLine) []string {
	keys := []string{model.ComputeProjectionKey(self.networkId), model.ComputeProjectionKey(self.networkId) + ":u"}
	for _, line := range lines {
		key := model.ComputeBudgetKey(self.networkId, line.SubjectId)
		found := false
		for _, held := range keys {
			found = found || held == key
		}
		if !found {
			keys = append(keys, key)
		}
	}
	return keys
}

// A hold of class through the prelude.
func (self *computeT3Network) classHold(opKey server.Id, amount int64, class string, lines []*model.ComputeBudgetLine) string {
	self.t3.t.Helper()
	args := append([]string{computeT3Hex(opKey), strconv.FormatInt(amount, 10), class}, computeT3LineArgs(lines)...)
	reply, err := self.t3.redis.RunScript(self.t3.ctx, computeT3ClassHoldScript, self.keys(lines), args)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return string(reply.Replies[0].Bulk)
}

// Plants an unconfirmed hold of amount taken age before Redis TIME.
func (self *computeT3Network) plant(opKey server.Id, amount int64, age time.Duration, lines []*model.ComputeBudgetLine) {
	self.t3.t.Helper()
	args := append([]string{computeT3Hex(opKey), strconv.FormatInt(amount, 10), strconv.FormatInt(age.Milliseconds(), 10)}, computeT3LineArgs(lines)...)
	if _, err := self.t3.redis.RunScript(self.t3.ctx, computeT3PlantScript, self.keys(lines), args); err != nil {
		self.t3.t.Fatal(err)
	}
}

func (self *computeT3Network) read() *model.ComputeProjection {
	self.t3.t.Helper()
	projection, err := model.ComputeProjectionRead(self.t3.env, self.t3.ctx, self.networkId)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return projection
}

func (self *computeT3Network) confirm(opKey server.Id, subjectIds ...server.Id) model.ComputeScriptOutcome {
	self.t3.t.Helper()
	outcome, err := model.ComputeProjectionConfirm(self.t3.env, self.t3.ctx, self.settings, self.networkId, opKey, subjectIds)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return outcome
}

func (self *computeT3Network) settle(opKey server.Id, amount int64, start time.Time, lines []*model.ComputeBudgetLine, subjectIds ...server.Id) model.ComputeScriptOutcome {
	self.t3.t.Helper()
	outcome, err := model.ComputeProjectionSettle(self.t3.env, self.t3.ctx, self.settings, &model.ComputeSettleRequest{
		NetworkId: self.networkId, OpKey: opKey, Kind: model.ComputeChargeKindInference, Path: model.ComputeHoldPathTee,
		AmountNanoUsd: amount, ReserveNanoUsd: amount, StartTime: start, Lines: lines, SubjectIds: subjectIds,
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return outcome
}

func (self *computeT3Network) void(opKey server.Id, subjectIds ...server.Id) model.ComputeScriptOutcome {
	self.t3.t.Helper()
	outcome, err := model.ComputeProjectionVoid(self.t3.env, self.t3.ctx, self.settings, self.networkId, opKey, subjectIds)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return outcome
}

func (self *computeT3Network) drop(version int64, settled int64, ops []*model.ComputeDropOp, subjectIds ...server.Id) *model.ComputeDropResult {
	self.t3.t.Helper()
	result, err := model.ComputeProjectionDrop(self.t3.env, self.t3.ctx, self.settings, &model.ComputeDropRequest{
		NetworkId: self.networkId,
		Account:   &model.ComputeProjectionAccount{Version: version, SettledNanoUsd: settled, State: model.ComputeAccountStateActive},
		Ops:       ops, SubjectIds: subjectIds,
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return result
}

// One field of a hash as an integer, zero where it is absent.
func (self *computeT3Network) field(key string, field string) int64 {
	self.t3.t.Helper()
	value, found, err := self.t3.redis.HashGet(self.t3.ctx, key, field)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	if !found {
		return 0
	}
	parsed, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return parsed
}

// A counter of subject's budget for now's period.
func (self *computeT3Network) counter(subjectId server.Id, kind string, budget string, period model.ComputeBudgetPeriod, now time.Time) int64 {
	self.t3.t.Helper()
	field, err := model.ComputeBudgetPeriodField(period, now)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return self.field(model.ComputeBudgetKey(self.networkId, subjectId), kind+":"+budget+":"+field)
}

// One day line of budget on subject.
func computeT3DayLine(subjectId server.Id, budget string, capNanoUsd int64) *model.ComputeBudgetLine {
	return &model.ComputeBudgetLine{SubjectId: subjectId, Budget: budget, Period: model.ComputeBudgetPeriodDay, CapNanoUsd: capNanoUsd}
}

// A hold whose balance would fall below zero is refused balance, one that lands exactly on zero is taken, and holds
// stay out of the balance the states read.
func TestComputeHoldRefusesBelowZero(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		network := newComputeT3Network(t3)
		network.rebuild(1, computeT3Usd(1), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		if result := network.hold(server.NewId(), computeT3Usd(1.5), nil); result.Outcome != model.ComputeHoldBalance {
			t.Errorf("a hold past the balance: %s", result.Outcome)
		}
		first := server.NewId()
		if result := network.hold(first, computeT3Usd(1), nil); result.Outcome != model.ComputeHoldTaken || result.Time.IsZero() {
			t.Errorf("a hold to exactly zero: %+v", result)
		}
		projection := network.read()
		if projection.AvailableNanoUsd() != 0 || projection.BalanceNanoUsd() != computeT3Usd(1) {
			t.Errorf("after the hold, available %d and the balance %d", projection.AvailableNanoUsd(), projection.BalanceNanoUsd())
		}
		if result := network.hold(server.NewId(), computeT3Nano, nil); result.Outcome != model.ComputeHoldHeld {
			t.Errorf("a nano-USD while the hold stands: %s", result.Outcome)
		}
		// the call settles at 40 cents: available 60 cents
		if network.confirm(first) != model.ComputeScriptConfirmed || network.settle(first, computeT3Usd(0.4), time.Now(), nil) != model.ComputeScriptSettled {
			t.Fatal("the call did not settle")
		}
		if projection := network.read(); projection.AvailableNanoUsd() != computeT3Usd(0.6) || projection.BalanceNanoUsd() != computeT3Usd(0.6) {
			t.Errorf("after the settlement, available %d and the balance %d", projection.AvailableNanoUsd(), projection.BalanceNanoUsd())
		}
		if result := network.hold(server.NewId(), computeT3Usd(0.7), nil); result.Outcome != model.ComputeHoldBalance {
			t.Errorf("a hold past what is left: %s", result.Outcome)
		}
		if result := network.hold(server.NewId(), computeT3Usd(0.6), nil); result.Outcome != model.ComputeHoldTaken {
			t.Errorf("a hold of what is left: %s", result.Outcome)
		}
	})
}

// Calls hold down to zero, never into the floor, which only enclave intervals reach: an interval holds down to minus
// the floor and no further, and the built path refuses the interval's hold, which BAL-M14-01's script takes.
func TestComputeHoldKeepsTheFloorForEnclaveKinds(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		network := newComputeT3Network(t3)
		network.rebuild(1, computeT3Usd(1), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		if result := network.hold(server.NewId(), computeT3Usd(1.5), nil); result.Outcome != model.ComputeHoldBalance {
			t.Errorf("a call into the floor: %s", result.Outcome)
		}
		if outcome := network.classHold(server.NewId(), computeT3Usd(5.5), "interval", nil); outcome != "taken" {
			t.Errorf("an interval down to 4.50 below zero, inside the 5 floor: %s", outcome)
		}
		if result := network.hold(server.NewId(), computeT3Nano, nil); result.Outcome != model.ComputeHoldBalance && result.Outcome != model.ComputeHoldHeld {
			t.Errorf("a call below zero: %s", result.Outcome)
		}
		// past the floor: refused balance without the open interval in the way, held with it
		if outcome := network.classHold(server.NewId(), computeT3Usd(6.5), "interval", nil); outcome != "balance" {
			t.Errorf("an interval past the floor: %s", outcome)
		}
		if outcome := network.classHold(server.NewId(), computeT3Usd(1), "interval", nil); outcome != "held" {
			t.Errorf("an interval the open one stands in the way of: %s", outcome)
		}
		_, err := model.ComputeProjectionHold(t3.env, t3.ctx, network.settings, &model.ComputeHoldRequest{
			NetworkId: network.networkId, OpKey: server.NewId(), Kind: model.ComputeChargeKindEnclaveRun, Path: model.ComputeHoldPathInterval,
		})
		if !errors.Is(err, model.ErrComputeHoldPathUnbuilt) {
			t.Errorf("the interval path: %v", err)
		}
	})
}

// A hold that open holds, not the balance, stand in the way of answers held with the retry, and is taken once they go.
func TestComputeHoldAnswersHeldWhenOpenHoldsStandInTheWay(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		network := newComputeT3Network(t3)
		network.rebuild(1, computeT3Usd(1), 0, 0, model.ComputeAccountStateActive)
		open := server.NewId()
		if result := network.hold(open, computeT3Usd(0.8), nil); result.Outcome != model.ComputeHoldTaken {
			t.Fatalf("the first hold: %s", result.Outcome)
		}
		waiting := server.NewId()
		result := network.hold(waiting, computeT3Usd(0.5), nil)
		if result.Outcome != model.ComputeHoldHeld || result.RetryAfter != network.settings.HeldRetry {
			t.Errorf("a hold the open one stands in the way of: %+v", result)
		}
		if result := network.hold(server.NewId(), computeT3Usd(1.2), nil); result.Outcome != model.ComputeHoldBalance {
			t.Errorf("a hold past the balance itself: %s", result.Outcome)
		}
		if network.void(open) != model.ComputeScriptVoid {
			t.Fatal("the open hold did not void")
		}
		if result := network.hold(waiting, computeT3Usd(0.5), nil); result.Outcome != model.ComputeHoldTaken {
			t.Errorf("the waiting hold once the open one went: %s", result.Outcome)
		}
	})
}

// Each budget line at its last cents: an agent's day, a device's ceiling, a member's month and a network's month each
// admit a hold to exactly the cap and refuse one past it; open reservations answer held; and parallel holds released
// together never spend one room twice.
func TestComputeHoldRefusesPastEachBudget(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		network := newComputeT3Network(t3)
		now := network.redisNow()
		agentId := server.NewId()
		memberId := server.NewId()
		lines, err := model.ComputeDefaultBudgetLines(network.settings, &model.ComputeBudgetCaller{SidecarId: network.sidecarId, AgentId: &agentId})
		if err != nil {
			t.Fatal(err)
		}
		memberLine := &model.ComputeBudgetLine{SubjectId: memberId, Budget: "member", Period: model.ComputeBudgetPeriodMonth, CapNanoUsd: computeT3Usd(100)}
		networkLine := &model.ComputeBudgetLine{SubjectId: network.networkId, Budget: "network", Period: model.ComputeBudgetPeriodMonth, CapNanoUsd: computeT3Usd(200)}
		counter := func(subjectId server.Id, budget string, period model.ComputeBudgetPeriod, charged int64) *model.ComputeProjectionCounter {
			return &model.ComputeProjectionCounter{SubjectId: subjectId, Budget: budget, Period: period, At: now, ChargedNanoUsd: charged}
		}
		// the agent's day at $4.99 of $5
		network.rebuild(1, computeT3Usd(1000), 0, 0, model.ComputeAccountStateActive,
			counter(agentId, model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, computeT3Usd(4.99)))
		if result := network.hold(server.NewId(), computeT3Usd(0.02), lines); result.Outcome != model.ComputeHoldBudget {
			t.Errorf("the agent's day past its cap: %s", result.Outcome)
		}
		last := server.NewId()
		if result := network.hold(last, computeT3Usd(0.01), lines); result.Outcome != model.ComputeHoldTaken {
			t.Errorf("the agent's last cent: %s", result.Outcome)
		}
		if result := network.hold(server.NewId(), computeT3Usd(0.01), lines); result.Outcome != model.ComputeHoldHeld {
			t.Errorf("a cent the open reservation stands in the way of: %s", result.Outcome)
		}

		// the device's ceiling at $49.995 of $50, the agent fresh
		network.rebuild(2, computeT3Usd(1000), 0, 0, model.ComputeAccountStateActive,
			counter(network.sidecarId, model.ComputeBudgetCeiling, model.ComputeBudgetPeriodDay, computeT3Usd(49.995)))
		if result := network.hold(server.NewId(), computeT3Usd(0.006), lines); result.Outcome != model.ComputeHoldBudget {
			t.Errorf("the ceiling past its cap: %s", result.Outcome)
		}
		if result := network.hold(server.NewId(), computeT3Usd(0.005), lines); result.Outcome != model.ComputeHoldTaken {
			t.Errorf("the ceiling's last half cent: %s", result.Outcome)
		}

		// a member's month at $99.99 of $100, and a network's month spent to its cap
		network.rebuild(3, computeT3Usd(1000), 0, 0, model.ComputeAccountStateActive,
			counter(memberId, "member", model.ComputeBudgetPeriodMonth, computeT3Usd(99.99)),
			counter(network.networkId, "network", model.ComputeBudgetPeriodMonth, computeT3Usd(200)))
		if result := network.hold(server.NewId(), computeT3Usd(0.02), []*model.ComputeBudgetLine{memberLine}); result.Outcome != model.ComputeHoldBudget {
			t.Errorf("the member's month past its cap: %s", result.Outcome)
		}
		if result := network.hold(server.NewId(), computeT3Usd(0.01), []*model.ComputeBudgetLine{memberLine}); result.Outcome != model.ComputeHoldTaken {
			t.Errorf("the member's last cent: %s", result.Outcome)
		}
		if result := network.hold(server.NewId(), computeT3Nano, []*model.ComputeBudgetLine{networkLine}); result.Outcome != model.ComputeHoldBudget {
			t.Errorf("the network's spent month: %s", result.Outcome)
		}

		// twenty holds of $1 released at once against the agent's fresh $5: five taken, the rest held
		network.rebuild(4, computeT3Usd(1000), 0, 0, model.ComputeAccountStateActive)
		start := make(chan struct{})
		outcomes := make([]model.ComputeHoldOutcome, 20)
		opKeys := make([]server.Id, 20)
		errs := make([]error, 20)
		var wait sync.WaitGroup
		for i := range 20 {
			opKeys[i] = server.NewId()
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				result, err := model.ComputeProjectionHold(t3.env, t3.ctx, network.settings, &model.ComputeHoldRequest{
					NetworkId: network.networkId, OpKey: opKeys[i], Kind: model.ComputeChargeKindInference, Path: model.ComputeHoldPathTee,
					HoldNanoUsd: computeT3Usd(1), ReserveNanoUsd: computeT3Usd(1), Lines: lines,
				})
				errs[i] = err
				if result != nil {
					outcomes[i] = result.Outcome
				}
			}()
		}
		close(start)
		wait.Wait()
		taken := []server.Id{}
		for i, outcome := range outcomes {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			switch outcome {
			case model.ComputeHoldTaken:
				taken = append(taken, opKeys[i])
			case model.ComputeHoldHeld:
			default:
				t.Errorf("a parallel hold answered %s", outcome)
			}
		}
		if len(taken) != 5 {
			t.Fatalf("%d of the parallel holds were taken", len(taken))
		}
		dayField, _ := model.ComputeBudgetPeriodField(model.ComputeBudgetPeriodDay, now)
		mainKey := model.ComputeProjectionKey(network.networkId)
		if reserved := network.field(mainKey, "ur:"+computeT3Hex(agentId)+":agent:"+dayField); reserved != computeT3Usd(5) {
			t.Errorf("the agent's unconfirmed reservations are %d", reserved)
		}
		// confirmed, the reservations move into the agent's counter; one settles at 30 cents, freeing 70
		for _, opKey := range taken {
			if network.confirm(opKey, agentId, network.sidecarId) != model.ComputeScriptConfirmed {
				t.Fatal("a parallel hold did not confirm")
			}
		}
		if reserved := network.counter(agentId, "r", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now); reserved != computeT3Usd(5) {
			t.Errorf("the agent's confirmed reservations are %d", reserved)
		}
		if network.settle(taken[0], computeT3Usd(0.3), now, lines, agentId, network.sidecarId) != model.ComputeScriptSettled {
			t.Fatal("a parallel hold did not settle")
		}
		if charged := network.counter(agentId, "c", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now); charged != computeT3Usd(0.3) {
			t.Errorf("the agent's charges are %d", charged)
		}
		if result := network.hold(server.NewId(), computeT3Usd(0.71), lines); result.Outcome != model.ComputeHoldHeld {
			t.Errorf("71 cents past the 70 freed: %s", result.Outcome)
		}
		if result := network.hold(server.NewId(), computeT3Usd(0.7), lines); result.Outcome != model.ComputeHoldTaken {
			t.Errorf("the 70 cents freed: %s", result.Outcome)
		}
	})
}

// The id as the scripts write a subject: 32 lowercase hex characters.
func computeT3Hex(id server.Id) string {
	key := model.ComputeProjectionKey(id)
	return key[5 : len(key)-1]
}

// In grace, calls and searches are refused for the balance whatever they cost, while an enclave interval runs on into
// the floor; suspended, the interval is refused too; low still pays. A search never draws promotional credit.
func TestComputeGraceRefusesCallsAndSearches(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		network := newComputeT3Network(t3)
		network.rebuild(1, -computeT3Usd(0.1), computeT3Usd(5), 0, model.ComputeAccountStateGrace)
		if result := network.hold(server.NewId(), computeT3Nano, nil); result.Outcome != model.ComputeHoldBalance {
			t.Errorf("a call in grace: %s", result.Outcome)
		}
		if outcome := network.classHold(server.NewId(), computeT3Nano, "search", nil); outcome != "balance" {
			t.Errorf("a search in grace: %s", outcome)
		}
		if outcome := network.classHold(server.NewId(), computeT3Usd(1), "interval", nil); outcome != "taken" {
			t.Errorf("an interval in grace inside the floor: %s", outcome)
		}
		network.rebuild(2, -computeT3Usd(0.1), computeT3Usd(5), 0, model.ComputeAccountStateSuspended)
		if outcome := network.classHold(server.NewId(), computeT3Nano, "interval", nil); outcome != "balance" {
			t.Errorf("an interval suspended: %s", outcome)
		}
		// low pays; promotional credit pays a call and never a search
		network.rebuild(3, computeT3Usd(1), computeT3Usd(5), computeT3Usd(0.95), model.ComputeAccountStateLow)
		if outcome := network.classHold(server.NewId(), computeT3Usd(0.1), "search", nil); outcome != "balance" {
			t.Errorf("a search past the credit promotions leave: %s", outcome)
		}
		if outcome := network.classHold(server.NewId(), computeT3Usd(0.05), "search", nil); outcome != "taken" {
			t.Errorf("a search within what promotions leave: %s", outcome)
		}
		if result := network.hold(server.NewId(), computeT3Usd(0.9), nil); result.Outcome != model.ComputeHoldTaken {
			t.Errorf("a call low, promotions paying: %s", result.Outcome)
		}
	})
}

// A hold planted unconfirmed 61 s before Redis TIME is dropped by the next script, with no wait: its amount and its
// reservations go, its operation answers void, and one planted 50 s before stays.
func TestComputeUnconfirmedHoldDropped(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		network := newComputeT3Network(t3)
		network.rebuild(1, computeT3Usd(10), 0, 0, model.ComputeAccountStateActive)
		agentId := server.NewId()
		lines := []*model.ComputeBudgetLine{computeT3DayLine(agentId, model.ComputeBudgetAgent, computeT3Usd(5))}
		stale := server.NewId()
		fresh := server.NewId()
		network.plant(stale, computeT3Usd(3), 61*time.Second, lines)
		network.plant(fresh, computeT3Usd(1), 50*time.Second, lines)
		if holds := network.read().HoldsNanoUsd; holds != computeT3Usd(4) {
			t.Fatalf("the planted holds sum to %d", holds)
		}
		next := server.NewId()
		if result := network.hold(next, computeT3Usd(0.5), lines); result.Outcome != model.ComputeHoldTaken {
			t.Fatalf("the next hold: %s", result.Outcome)
		}
		if holds := network.read().HoldsNanoUsd; holds != computeT3Usd(1.5) {
			t.Errorf("after the next script the holds sum to %d", holds)
		}
		now := network.redisNow()
		dayField, _ := model.ComputeBudgetPeriodField(model.ComputeBudgetPeriodDay, now)
		mainKey := model.ComputeProjectionKey(network.networkId)
		if reserved := network.field(mainKey, "ur:"+computeT3Hex(agentId)+":agent:"+dayField); reserved != computeT3Usd(1.5) {
			t.Errorf("the unconfirmed reservations are %d", reserved)
		}
		reply, err := t3.redis.RunScript(t3.ctx, computeT3UnconfirmedScript, []string{mainKey + ":u"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		members := []string{}
		for _, member := range reply.Replies {
			members = append(members, string(member.Bulk))
		}
		if len(members) != 2 || members[0] == computeT3Hex(stale) || members[1] == computeT3Hex(stale) {
			t.Errorf("the unconfirmed set holds %v", members)
		}
		if network.confirm(stale, agentId) != model.ComputeScriptVoid {
			t.Error("the dropped hold confirmed")
		}
		if result := network.hold(stale, computeT3Usd(3), lines); result.Outcome != model.ComputeHoldVoid {
			t.Errorf("the dropped hold's repeat: %s", result.Outcome)
		}
		if network.confirm(fresh, agentId) != model.ComputeScriptConfirmed {
			t.Error("the hold inside its age did not confirm")
		}
	})
}

// Each script acts once per operation key and a repeat answers its first outcome: a hold, a confirm, a settle, a drop
// and a void repeated change nothing more; an operation voided or booked answers so to a later hold or settle; a
// settled charge is never voided; and a settle with no hold still counts its charge, at its start's period.
func TestComputeScriptsActOncePerOperationKey(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		network := newComputeT3Network(t3)
		network.rebuild(1, computeT3Usd(10), 0, 0, model.ComputeAccountStateActive)
		now := network.redisNow()
		agentId := server.NewId()
		lines := []*model.ComputeBudgetLine{computeT3DayLine(agentId, model.ComputeBudgetAgent, computeT3Usd(5))}

		call := server.NewId()
		first := network.hold(call, computeT3Usd(1), lines)
		second := network.hold(call, computeT3Usd(1), lines)
		if first.Outcome != model.ComputeHoldTaken || second.Outcome != model.ComputeHoldTaken || !second.Time.Equal(first.Time) {
			t.Errorf("a hold repeated: %+v then %+v", first, second)
		}
		if holds := network.read().HoldsNanoUsd; holds != computeT3Usd(1) {
			t.Errorf("a repeated hold holds %d", holds)
		}
		for range 2 {
			if network.confirm(call, agentId) != model.ComputeScriptConfirmed {
				t.Error("a confirm did not confirm")
			}
		}
		if reserved := network.counter(agentId, "r", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now); reserved != computeT3Usd(1) {
			t.Errorf("a repeated confirm reserved %d", reserved)
		}
		for range 2 {
			if network.settle(call, computeT3Usd(0.4), now, lines, agentId) != model.ComputeScriptSettled {
				t.Error("a settle did not settle")
			}
		}
		projection := network.read()
		if projection.UnsettledNanoUsd != computeT3Usd(0.4) || projection.HoldsNanoUsd != 0 {
			t.Errorf("a repeated settle left unsettled %d and holds %d", projection.UnsettledNanoUsd, projection.HoldsNanoUsd)
		}
		if charged := network.counter(agentId, "c", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now); charged != computeT3Usd(0.4) {
			t.Errorf("a repeated settle charged %d", charged)
		}
		if reserved := network.counter(agentId, "r", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now); reserved != 0 {
			t.Errorf("a settled hold still reserves %d", reserved)
		}
		if network.void(call, agentId) != model.ComputeScriptSettled {
			t.Error("a settled charge was voided")
		}
		ops := []*model.ComputeDropOp{{OpKey: call, AmountNanoUsd: computeT3Usd(0.4)}}
		if result := network.drop(2, computeT3Usd(9.6), ops, agentId); result.DroppedCount != 1 || !result.Applied {
			t.Errorf("the drop: %+v", result)
		}
		if result := network.drop(2, computeT3Usd(9.6), ops, agentId); result.DroppedCount != 0 || result.Applied {
			t.Errorf("a repeated drop: %+v", result)
		}
		projection = network.read()
		if projection.UnsettledNanoUsd != 0 || projection.SettledNanoUsd != computeT3Usd(9.6) || projection.AvailableNanoUsd() != computeT3Usd(9.6) {
			t.Errorf("after the drop: %+v", projection)
		}
		if network.settle(call, computeT3Usd(0.4), now, lines, agentId) != model.ComputeScriptDropped {
			t.Error("a settle after the drop")
		}
		if result := network.hold(call, computeT3Usd(1), lines); result.Outcome != model.ComputeHoldDropped {
			t.Errorf("a hold after the drop: %s", result.Outcome)
		}
		if charged := network.counter(agentId, "c", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now); charged != computeT3Usd(0.4) {
			t.Errorf("after the drop the agent's charges are %d", charged)
		}

		released := server.NewId()
		network.hold(released, computeT3Usd(2), lines)
		for range 2 {
			if network.void(released, agentId) != model.ComputeScriptVoid {
				t.Error("a void did not void")
			}
		}
		if holds := network.read().HoldsNanoUsd; holds != 0 {
			t.Errorf("after the void the holds are %d", holds)
		}
		if result := network.hold(released, computeT3Usd(2), lines); result.Outcome != model.ComputeHoldVoid {
			t.Errorf("a hold after its void: %s", result.Outcome)
		}
		if network.confirm(released, agentId) != model.ComputeScriptVoid || network.settle(released, computeT3Usd(1), now, lines, agentId) != model.ComputeScriptVoid {
			t.Error("a voided hold confirmed or settled")
		}

		// a settle whose hold is gone still counts its charge, at the periods of its start
		lost := server.NewId()
		if network.settle(lost, computeT3Usd(0.25), now, lines, agentId) != model.ComputeScriptSettled {
			t.Error("a settle with no hold")
		}
		if charged := network.counter(agentId, "c", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now); charged != computeT3Usd(0.65) {
			t.Errorf("a settle with no hold charged the agent to %d", charged)
		}
		if unsettled := network.read().UnsettledNanoUsd; unsettled != computeT3Usd(0.25) {
			t.Errorf("a settle with no hold left unsettled %d", unsettled)
		}
	})
}

// The drop sets settled from the committed account only when its version is newer, compared as decimal strings by
// length then bytes, while it drops its own charges whatever the version.
func TestComputeDropMovesSettledOnlyForward(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		network := newComputeT3Network(t3)
		network.rebuild(5, computeT3Usd(10), 0, 0, model.ComputeAccountStateActive)
		call := server.NewId()
		network.hold(call, computeT3Usd(1), nil)
		network.confirm(call)
		network.settle(call, computeT3Usd(1), time.Now(), nil)
		if result := network.drop(4, computeT3Usd(3), []*model.ComputeDropOp{{OpKey: call, AmountNanoUsd: computeT3Usd(1)}}); result.DroppedCount != 1 || result.Applied {
			t.Errorf("an older version's drop: %+v", result)
		}
		if projection := network.read(); projection.SettledNanoUsd != computeT3Usd(10) || projection.UnsettledNanoUsd != 0 || projection.Version != "5" {
			t.Errorf("after an older version: %+v", projection)
		}
		// 10 is newer than 5 though "1" sorts before "5"
		if result := network.drop(10, computeT3Usd(9), nil); !result.Applied {
			t.Error("version 10 was not taken over 5")
		}
		if result := network.drop(9, computeT3Usd(1), nil); result.Applied {
			t.Error("version 9 was taken over 10")
		}
		if result := network.drop(10, computeT3Usd(1), nil); result.Applied {
			t.Error("version 10 was taken twice")
		}
		if projection := network.read(); projection.SettledNanoUsd != computeT3Usd(9) || projection.Version != "10" {
			t.Errorf("after version 10: %+v", projection)
		}
	})
}

// An absent projection answers held with a one-second retry and a drop writes nothing into it; a rebuild admits; a
// mark stops admission until the next rebuild.
func TestComputeAbsentProjectionAnswersHeld(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		network := newComputeT3Network(t3)
		result := network.hold(server.NewId(), computeT3Usd(0.01), nil)
		if result.Outcome != model.ComputeHoldHeld || result.RetryAfter != time.Second {
			t.Errorf("an absent projection: %+v", result)
		}
		if result := network.drop(3, computeT3Usd(5), nil); !result.Absent || result.Applied {
			t.Errorf("a drop on an absent projection: %+v", result)
		}
		if projection := network.read(); projection.Present {
			t.Errorf("a drop wrote an absent projection: %+v", projection)
		}
		network.rebuild(3, computeT3Usd(5), 0, 0, model.ComputeAccountStateActive)
		if result := network.hold(server.NewId(), computeT3Usd(0.01), nil); result.Outcome != model.ComputeHoldTaken {
			t.Errorf("a rebuilt projection: %s", result.Outcome)
		}
		if err := model.ComputeProjectionMarkRebuild(t3.env, t3.ctx, network.networkId); err != nil {
			t.Fatal(err)
		}
		if projection := network.read(); !projection.Marked {
			t.Error("the mark does not read")
		}
		result = network.hold(server.NewId(), computeT3Usd(0.01), nil)
		if result.Outcome != model.ComputeHoldHeld || result.RetryAfter != time.Second {
			t.Errorf("a marked projection: %+v", result)
		}
		network.rebuild(3, computeT3Usd(5), 0, 0, model.ComputeAccountStateActive)
		if projection := network.read(); projection.Marked || projection.HoldsNanoUsd != 0 {
			t.Errorf("the rebuild left %+v", projection)
		}
		if result := network.hold(server.NewId(), computeT3Usd(0.01), nil); result.Outcome != model.ComputeHoldTaken {
			t.Errorf("after the rebuild: %s", result.Outcome)
		}
	})
}
