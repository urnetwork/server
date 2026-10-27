// Once this process has refused a payer as exhausted, the payer's next refusal
// costs one discovery read and one probe of each grant: no reservation,
// compensation or second query, and the census's own outcome. Every other
// state leaves the request to the census.
package model

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// A payer stays listed for its hold and leaves when funded. A proof that
// decided nothing keeps the payer with the census for its hold, which a
// refusing census does not cut short. A full list neither grows nor scans
// more than once per hold.
func TestRedisExhaustedPayersHoldForgetAndBound(t *testing.T) {
	start := time.Unix(1_000_000, 0)
	payers := newRedisExhaustedPayers(5*time.Second, 2)
	first, second, third := server.NewId(), server.NewId(), server.NewId()
	if payers.recent(first, start) {
		t.Fatal("an unlisted payer was recent")
	}
	payers.note(first, start)
	if !payers.recent(first, start.Add(5*time.Second-time.Millisecond)) {
		t.Fatal("a listed payer was not recent within its hold")
	}
	if payers.recent(first, start.Add(5*time.Second)) {
		t.Fatal("a listed payer outlived its hold")
	}
	if len(payers.payerIdHolds) != 0 {
		t.Fatal("an expired read kept its payer", len(payers.payerIdHolds))
	}
	payers.note(first, start)
	payers.forget(first)
	if payers.recent(first, start) {
		t.Fatal("a funded payer stayed listed")
	}

	payers.note(first, start)
	payers.noteUnproven(first, start.Add(time.Second))
	payers.note(first, start.Add(2*time.Second))
	if payers.recent(first, start.Add(2*time.Second)) || payers.recent(first, start.Add(6*time.Second-time.Millisecond)) {
		t.Fatal("a payer the proof could not decide tried it again within its hold")
	}
	payers.note(first, start.Add(6*time.Second))
	if !payers.recent(first, start.Add(6*time.Second)) {
		t.Fatal("a refusing census did not list the payer after its unproven hold")
	}
	payers.noteUnproven(first, start.Add(7*time.Second))
	payers.forget(first)
	payers.note(first, start.Add(7*time.Second))
	if !payers.recent(first, start.Add(7*time.Second)) {
		t.Fatal("funding did not end the unproven hold")
	}
	payers.forget(first)

	payers.note(first, start)
	payers.note(second, start)
	payers.note(third, start.Add(time.Second))
	if payers.recent(third, start.Add(time.Second)) {
		t.Fatal("a full list grew")
	}
	// a listed payer renews its hold in a full list
	payers.note(first, start.Add(2*time.Second))
	// the second payer has expired, but the list scanned at 1s
	payers.note(third, start.Add(5500*time.Millisecond))
	if payers.recent(third, start.Add(5500*time.Millisecond)) {
		t.Fatal("a full list scanned twice within one hold")
	}
	payers.note(third, start.Add(6*time.Second))
	if !payers.recent(third, start.Add(6*time.Second)) || !payers.recent(first, start.Add(6*time.Second)) {
		t.Fatal("a full list did not drop its expired payer for a new one")
	}
	payers.clear()
	if payers.recent(first, start.Add(6*time.Second)) || payers.recent(third, start.Add(6*time.Second)) {
		t.Fatal("a cleared list kept a payer")
	}
}

// Gives the test its own list, with a hold no request on a loaded test host
// outlives, so a payer a test lists stays listed for its next request.
func withRedisExhaustedPayersForTest(t testing.TB) {
	t.Helper()
	previous := exhaustedRedisPayers
	exhaustedRedisPayers = newRedisExhaustedPayers(10*time.Minute, redisExhaustedPayerCapacity)
	t.Cleanup(func() { exhaustedRedisPayers = previous })
}

// Every counter an exhausted request can move.
func redisExhaustionCounts() map[string]float64 {
	counts := redisGrantProbeCounts()
	for _, counter := range []struct{ operation, result string }{
		{"release-pending", "accepted"}, {"release-pending", "error"},
		{"compensation", "completed"}, {"compensation", "pending"}, {"compensation", "deferred"},
		{"recovery", "completed"}, {"recovery", "pending"},
	} {
		counts[counter.operation+" "+counter.result] = testutil.ToFloat64(redisContractReservationResults.WithLabelValues(counter.operation, counter.result))
	}
	return counts
}

func redisExhaustionCountsSince(before map[string]float64) map[string]float64 {
	delta := map[string]float64{}
	for name, value := range redisExhaustionCounts() {
		if difference := value - before[name]; difference != 0 {
			delta[name] = difference
		}
	}
	return delta
}

// One request with an observable admission owner, counters and discovery reads.
func redisExhaustionCreate(t testing.TB, ctx context.Context, clients escrowSelectionTestClients, amount ByteCount) (*TransferEscrow, *redisContractAdmission, map[string]float64, *redisSelectionTestReads, error) {
	t.Helper()
	reads := &redisSelectionTestReads{}
	request := withRedisContractAdmission(context.WithValue(ctx, redisGrantReadContextKey{}, reads.observe))
	owner := redisAdmissionFromContext(request)
	before := redisExhaustionCounts()
	escrow, err := CreateTransferEscrow(request, clients.payerNetworkId, clients.payerId,
		clients.providerNetworkId, clients.providerId, amount)
	drainRedisContractCompensations(t, ctx)
	return escrow, owner, redisExhaustionCountsSince(before), reads, err
}

func redisExhaustionHeld(ctx context.Context, ids []server.Id) []ByteCount {
	held := make([]ByteCount, len(ids))
	for index, id := range ids {
		held[index] = Testing_NetEscrowByteCount(ctx, id)
	}
	return held
}

// Production shape: every grant is held, two with a little room left. The
// first request runs the census, which reserves that room and releases it with
// the refusal. The next one is refused by one read of the grants and one probe
// pipeline, with the same error, no reservation, no compensation and no
// recovery page for grants that hold no marker.
func TestRedisExhaustedPayerRefusalReservesNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		withRedisExhaustedPayersForTest(t)
		for _, spread := range []bool{false, true} {
			clients := newEscrowSelectionTestClients(t, ctx)
			ids := redisSelectionTestGrants(t, ctx, clients, 6, 1000, spread)
			for index, id := range ids {
				reserved := ByteCount(1000)
				if index == 2 || index == 4 {
					reserved = 900
				}
				redisGrantProbeToken(t, ctx, id, 1000, reserved)
			}
			held := redisExhaustionHeld(ctx, ids)

			escrow, _, counts, _, censusErr := redisExhaustionCreate(t, ctx, clients, 512)
			if escrow != nil || !errors.Is(censusErr, ErrContractBalanceExhausted) {
				t.Fatal("the census did not refuse the exhausted payer", spread, escrow, censusErr)
			}
			if counts["reserve-pending accepted"] != 2 || counts["release-pending accepted"] != 2 {
				t.Fatal("the census no longer reserves and releases the room left", spread, counts)
			}

			escrow, owner, counts, reads, err := redisExhaustionCreate(t, ctx, clients, 512)
			if escrow != nil || !errors.Is(err, ErrContractBalanceExhausted) || err.Error() != censusErr.Error() {
				t.Fatal("the next refusal differs from the census", spread, escrow, err, censusErr)
			}
			redisGrantProbeRequireCounts(t, "exhausted payer", counts, map[string]float64{
				"reserve-probe empty":     4,
				"reserve-probe available": 2,
				"exhaustion-proof proven": 1,
			})
			for _, name := range []string{"release-pending accepted", "compensation completed", "compensation deferred", "recovery completed", "recovery pending"} {
				if counts[name] != 0 {
					t.Error("the proven refusal still did", name, spread, counts)
				}
			}
			if reads.grantQueries != 1 || reads.grantRows != len(ids) {
				t.Error("the proven refusal read the grants more than once", spread, reads.grantQueries, reads.grantRows)
			}
			if len(owner.attemptedBalanceIds) != 0 || len(owner.compensationIds()) != 0 {
				t.Error("unmarked grants joined recovery or compensation", spread, len(owner.attemptedBalanceIds))
			}
			if got := redisExhaustionHeld(ctx, ids); !slices.Equal(got, held) {
				t.Fatal("a refusal changed the held bytes", spread, got, held)
			}
			if !exhaustedRedisPayers.recent(clients.payerNetworkId, time.Now()) {
				t.Fatal("the proven refusal did not keep its payer listed", spread)
			}
		}
	})
}

// One payer state, set up identically for a twin.
type redisExhaustionScenario struct {
	name    string
	spread  bool
	grants  int
	credit  ByteCount
	request ByteCount
	limits  *redisGrantSelectionLimits
	// the room left on a grant, by grant index; every other grant is held
	room map[int]ByteCount
	// a custom state instead of room
	setup func(t testing.TB, ctx context.Context, ids []server.Id)
	// whether the listed twin is decided by the proof
	proven bool
	// when set, the census's outcome: refused, funded or capacity
	outcome string
	// when set, the bytes the census funds from each grant index
	funded map[int]ByteCount
}

// One request for each twin of the scenario, in identical states: the listed
// twin's process has just refused its payer as exhausted, the other's has not.
// Requires the same outcome: the same refusal or capacity hold, or the same
// grants, amounts and priority, and the same bytes held afterwards. The
// listed twin is decided by the proof exactly when the scenario says so.
func requireRedisExhaustionTwinsAgree(t testing.TB, ctx context.Context, scenario redisExhaustionScenario) {
	t.Helper()
	type outcome struct {
		escrow *TransferEscrow
		err    error
		held   []ByteCount
		// funded grant index -> bytes
		funded map[int]ByteCount
		counts map[string]float64
	}
	run := func(listed bool) outcome {
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := []server.Id{}
		if 0 < scenario.grants {
			ids = redisSelectionTestGrants(t, ctx, clients, scenario.grants, scenario.credit, scenario.spread)
		}
		if scenario.setup != nil {
			scenario.setup(t, ctx, ids)
		} else {
			for index, id := range ids {
				if amount := scenario.credit - scenario.room[index]; 0 < amount {
					redisGrantProbeToken(t, ctx, id, scenario.credit, amount)
				}
			}
		}
		if listed {
			exhaustedRedisPayers.note(clients.payerNetworkId, time.Now())
		}
		request := ctx
		if scenario.limits != nil {
			request = context.WithValue(ctx, redisGrantSelectionContextKey{}, *scenario.limits)
		}
		var result outcome
		if value := server.HandleError(func() {
			result.escrow, _, result.counts, _, result.err = redisExhaustionCreate(t, request, clients, scenario.request)
		}); value != nil {
			t.Fatal(scenario.name, "admission panicked", value)
		}
		result.held = redisExhaustionHeld(ctx, ids)
		if result.escrow != nil {
			result.funded = map[int]ByteCount{}
			for _, balance := range result.escrow.Balances {
				result.funded[slices.Index(ids, balance.BalanceId)] = balance.BalanceByteCount
			}
		}
		return result
	}
	census, listed := run(false), run(true)

	switch scenario.outcome {
	case "":
	case "refused":
		if !errors.Is(census.err, ErrContractBalanceExhausted) {
			t.Fatalf("%s: the census did not refuse: %v", scenario.name, census.err)
		}
	case "funded":
		if census.err != nil || census.escrow == nil {
			t.Fatalf("%s: the census did not fund: %v", scenario.name, census.err)
		}
	case "capacity":
		if !errors.Is(census.err, errRedisGrantSelectionCapacity) {
			t.Fatalf("%s: the census did not stop at a selection limit: %v", scenario.name, census.err)
		}
	default:
		t.Fatalf("%s: unknown outcome %q", scenario.name, scenario.outcome)
	}
	if scenario.funded != nil && !maps.Equal(census.funded, scenario.funded) {
		t.Fatalf("%s: the census funded %v, want %v", scenario.name, census.funded, scenario.funded)
	}

	if (census.err == nil) != (listed.err == nil) ||
		errors.Is(census.err, ErrContractBalanceExhausted) != errors.Is(listed.err, ErrContractBalanceExhausted) ||
		errors.Is(census.err, errRedisGrantSelectionCapacity) != errors.Is(listed.err, errRedisGrantSelectionCapacity) {
		t.Fatalf("%s: outcomes differ: census %v, listed %v", scenario.name, census.err, listed.err)
	}
	if errors.Is(census.err, ErrContractBalanceExhausted) && census.err.Error() != listed.err.Error() {
		t.Errorf("%s: refusals differ: %q and %q", scenario.name, census.err, listed.err)
	}
	if (census.escrow == nil) != (listed.escrow == nil) {
		t.Fatalf("%s: funding differs: %v and %v", scenario.name, census.escrow, listed.escrow)
	}
	if census.escrow != nil && (census.escrow.TransferByteCount != listed.escrow.TransferByteCount ||
		census.escrow.Priority != listed.escrow.Priority || !maps.Equal(census.funded, listed.funded)) {
		t.Errorf("%s: funded contracts differ: %v and %v", scenario.name, census.funded, listed.funded)
	}
	if !slices.Equal(census.held, listed.held) {
		t.Errorf("%s: held bytes differ: %v and %v", scenario.name, census.held, listed.held)
	}
	if census.counts["exhaustion-proof proven"] != 0 || census.counts["exhaustion-proof unproven"] != 0 {
		t.Errorf("%s: an unlisted payer tried the proof", scenario.name)
	}
	proven, unproven := listed.counts["exhaustion-proof proven"], listed.counts["exhaustion-proof unproven"]
	if scenario.proven && (proven != 1 || unproven != 0) || !scenario.proven && (proven != 0 || unproven != 1) {
		t.Errorf("%s: proof proven=%v unproven=%v, want proven %t", scenario.name, proven, unproven, scenario.proven)
	}
	if scenario.proven {
		for _, name := range []string{"reserve-pending accepted", "reserve-pending refused", "reserve-pending-full accepted", "reserve-pending-full refused", "release-pending accepted"} {
			if listed.counts[name] != 0 {
				t.Errorf("%s: the proven refusal still did %s", scenario.name, name)
			}
		}
	}
}

// The listed payer and its unlisted twin, in identical states, reach the same
// outcome. The proof decides only the exhausted states; it leaves the rest to
// the census.
func TestRedisExhaustedPayerProofKeepsCensusOutcomes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		withRedisExhaustedPayersForTest(t)
		for _, scenario := range []redisExhaustionScenario{
			{name: "every grant held", grants: 4, credit: 1000, request: 512, proven: true, outcome: "refused"},
			{name: "room below the request", grants: 4, credit: 1000, request: 512, room: map[int]ByteCount{1: 200, 3: 311}, proven: true, outcome: "refused"},
			{name: "room below the shrink floor", grants: 4, credit: 4 * Mib, request: 2 * Mib, room: map[int]ByteCount{1: 512 * Kib, 3: 512*Kib - 1}, proven: true, outcome: "refused"},
			{name: "spread payer held", spread: true, grants: 6, credit: 1000, request: 512, room: map[int]ByteCount{1: 100, 4: 100}, proven: true, outcome: "refused"},
			{name: "no grants", grants: 0, credit: 1000, request: 512, proven: true, outcome: "refused"},
			{name: "legacy debt holds a grant", grants: 2, credit: 1000, request: 512, proven: true, outcome: "refused",
				setup: func(t testing.TB, ctx context.Context, ids []server.Id) {
					redisGrantProbeToken(t, ctx, ids[0], 1000, 1000)
					redisGrantProbeToken(t, ctx, ids[1], 1000, 400)
					server.Redis(ctx, func(client server.RedisClient) {
						server.Raise(client.Set(ctx, netEscrowKey(ids[1]), 600, time.Hour).Err())
					})
				}},
			{name: "a marker without a token", grants: 2, credit: 1000, request: 512, proven: true, outcome: "refused",
				setup: func(t testing.TB, ctx context.Context, ids []server.Id) {
					redisGrantProbeToken(t, ctx, ids[0], 1000, 1000)
					redisGrantProbeToken(t, ctx, ids[1], 1000, 1000)
					server.Redis(ctx, func(client server.RedisClient) {
						server.Raise(client.ZAdd(ctx, redisContractReservationKeys(ids[1])[4], redis.Z{Score: 1, Member: server.NewId().String()}).Err())
					})
				}},
			{name: "room reaches the shrink floor", grants: 4, credit: 4 * Mib, request: 2 * Mib, room: map[int]ByteCount{1: 512 * Kib, 3: 512 * Kib}, outcome: "funded"},
			{name: "room across grants covers the request", grants: 4, credit: 1000, request: 512, room: map[int]ByteCount{1: 300, 3: 300}, outcome: "funded"},
			{name: "one grant covers the request", grants: 4, credit: 1000, request: 512, room: map[int]ByteCount{2: 600}, outcome: "funded"},
			{name: "spread grant covers the request", spread: true, grants: 6, credit: 1000, request: 512, room: map[int]ByteCount{4: 1000}, outcome: "funded"},
			{name: "a fresh grant", grants: 2, credit: 1000, request: 512, outcome: "funded",
				setup: func(t testing.TB, ctx context.Context, ids []server.Id) {
					redisGrantProbeToken(t, ctx, ids[0], 1000, 1000)
				}},
			{name: "an expired lease", grants: 2, credit: 1000, request: 512, outcome: "funded",
				setup: func(t testing.TB, ctx context.Context, ids []server.Id) {
					redisGrantProbeToken(t, ctx, ids[0], 1000, 1000)
					redisGrantProbeExpireLease(t, ctx, ids[1], redisGrantProbeToken(t, ctx, ids[1], 1000, 1000))
				}},
			{name: "an invalid counter", grants: 2, credit: 1000, request: 512,
				setup: func(t testing.TB, ctx context.Context, ids []server.Id) {
					redisGrantProbeToken(t, ctx, ids[0], 1000, 1000)
					server.Redis(ctx, func(client server.RedisClient) {
						server.Raise(client.Set(ctx, redisContractReservationKeys(ids[1])[0], "malformed", time.Hour).Err())
					})
				}},
			{name: "the selected limit", grants: 4, credit: 1000, request: 512, room: map[int]ByteCount{0: 100, 1: 100}, outcome: "capacity",
				limits: &redisGrantSelectionLimits{MaxRows: 4096, MaxSelected: 2}},
			{name: "the row limit", grants: 4, credit: 1000, request: 512, outcome: "refused",
				limits: &redisGrantSelectionLimits{MaxRows: 64, MaxSelected: 64}},
		} {
			requireRedisExhaustionTwinsAgree(t, ctx, scenario)
		}
	})
}

// More grants than one keyset page. The proof reads every page and carries
// its room across them; a grant that fits on any page leaves the request to
// the census; and the census's row limit, after the spread windows' 64 rows,
// bounds the proof: a payer one row past it is the census's capacity hold.
// Grant i starts i ms after grant 0 and ends 2 h after it starts, so the
// keyset pages hold grants 0-63, 64-127 and 128-129, and the spread windows
// read the 64 newest, 66-129.
func TestRedisExhaustedPayerProofKeepsCensusOutcomesAcrossPages(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		withRedisExhaustedPayersForTest(t)
		const grants = 2*redisGrantPageRows + 2
		for _, scenario := range []redisExhaustionScenario{
			{name: "room on every page below the request", grants: grants, credit: 1000, request: 512,
				room: map[int]ByteCount{10: 150, 70: 150, 129: 150}, proven: true, outcome: "refused"},
			{name: "room on every page covers the request", grants: grants, credit: 1000, request: 512,
				room: map[int]ByteCount{10: 200, 70: 200, 129: 200}, outcome: "funded",
				funded: map[int]ByteCount{10: 200, 70: 200, 129: 112}},
			{name: "room on both sides of a page boundary", grants: grants, credit: 1000, request: 512,
				room: map[int]ByteCount{63: 150, 64: 150}, proven: true, outcome: "refused"},
			{name: "the only fitting grant opens the second page", grants: grants, credit: 1000, request: 512,
				room: map[int]ByteCount{64: 1000}, outcome: "funded", funded: map[int]ByteCount{64: 512}},
			{name: "the only fitting grant opens the third page", grants: grants, credit: 1000, request: 512,
				room: map[int]ByteCount{128: 600}, outcome: "funded", funded: map[int]ByteCount{128: 512}},
			{name: "spread payer held on every page", spread: true, grants: grants, credit: 1000, request: 512,
				room: map[int]ByteCount{5: 100, 100: 100}, proven: true, outcome: "refused"},
			{name: "spread payer fits outside its windows", spread: true, grants: grants, credit: 1000, request: 512,
				room: map[int]ByteCount{65: 1000}, outcome: "funded", funded: map[int]ByteCount{65: 512}},
			{name: "spread payer at the row limit after its windows", spread: true, grants: grants, credit: 1000, request: 512,
				limits: &redisGrantSelectionLimits{MaxRows: grants + redisGrantWindowRows + 1, MaxSelected: 64}, proven: true, outcome: "refused"},
			{name: "spread payer one row past it", spread: true, grants: grants, credit: 1000, request: 512,
				limits: &redisGrantSelectionLimits{MaxRows: grants + redisGrantWindowRows, MaxSelected: 64}, outcome: "capacity"},
		} {
			requireRedisExhaustionTwinsAgree(t, ctx, scenario)
		}
	})
}

// A dead request's aged pending token holds the only grant with room. The
// proof keeps that marked grant in the refused request's recovery page, which
// releases the token, and the rerun funds the same request through the census
// without a second proof, as an unlisted payer is funded. Funding takes the
// payer off the list.
func TestRedisExhaustedPayerProofKeepsRecoveryRerun(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		withRedisExhaustedPayersForTest(t)
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := redisSelectionTestGrants(t, ctx, clients, 2, 1000, false)
		redisGrantProbeToken(t, ctx, ids[0], 1000, 1000)
		dead := server.NewId()
		if reserved, err := redisContractReservation(ctx, "reserve-pending", ids[1], dead, 1000, 1000, redisContractReservationLease); err != nil || reserved != 1000 {
			t.Fatal("dead request setup", reserved, err)
		}
		redisRecoveryAgePendingMarker(t, ctx, ids[1], dead)
		exhaustedRedisPayers.note(clients.payerNetworkId, time.Now())

		escrow, owner, counts, _, err := redisExhaustionCreate(t, ctx, clients, 29)
		if err != nil || escrow == nil || escrow.ContractId != owner.contractId || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != ids[1] || escrow.Balances[0].BalanceByteCount != 29 {
			t.Fatal("recovery after the proof did not fund the same request", escrow, err)
		}
		if got := redisExhaustionHeld(ctx, ids); !slices.Equal(got, []ByteCount{1000, 29}) {
			t.Fatal("the dead token was not released or custody was duplicated", got)
		}
		redisGrantProbeRequireCounts(t, "recovered after the proof", counts, map[string]float64{
			// the proof reads both grants
			"reserve-probe empty":     2,
			"exhaustion-proof proven": 1,
			// the rerun's census refuses the first grant and funds from the second
			"reserve-pending refused":  1,
			"reserve-pending accepted": 1,
			"reserve-probe available":  1,
		})
		if !slices.Equal(owner.attemptedBalanceIds, []server.Id{ids[1], ids[0]}) {
			t.Error("recovery discovery did not start with the marked grant", len(owner.attemptedBalanceIds))
		}
		if exhaustedRedisPayers.recent(clients.payerNetworkId, time.Now()) {
			t.Fatal("a funded payer stayed listed")
		}
	})
}

// Only a complete census that refuses an ordinary payer lists it: funding,
// an unknown counter, a selection limit and the internal prober's census do
// not. A proof that decides nothing is not tried again within its hold.
func TestRedisExhaustedPayersFollowCensusOutcomes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		withRedisExhaustedPayersForTest(t)
		listed := func(clients escrowSelectionTestClients) bool {
			return exhaustedRedisPayers.recent(clients.payerNetworkId, time.Now())
		}

		exhausted := newEscrowSelectionTestClients(t, ctx)
		exhaustedIds := redisSelectionTestGrants(t, ctx, exhausted, 2, 1000, false)
		for _, id := range exhaustedIds {
			redisGrantProbeToken(t, ctx, id, 1000, 1000)
		}
		if _, err := exhausted.create(ctx, 512, false); !errors.Is(err, ErrContractBalanceExhausted) || !listed(exhausted) {
			t.Fatal("a refusing census did not list its payer", err)
		}
		// credit returns: the proof defers and the census funds and unlists
		redisSelectionTestGrants(t, ctx, exhausted, 1, 1000, false)
		if escrow, err := exhausted.create(ctx, 512, false); err != nil || escrow == nil || listed(exhausted) {
			t.Fatal("funding did not unlist the payer", escrow, err)
		}

		unknown := newEscrowSelectionTestClients(t, ctx)
		unknownIds := redisSelectionTestGrants(t, ctx, unknown, 2, 1000, false)
		redisGrantProbeToken(t, ctx, unknownIds[0], 1000, 1000)
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Set(ctx, redisContractReservationKeys(unknownIds[1])[0], "malformed", time.Hour).Err())
		})
		if _, err := unknown.create(ctx, 512, false); err == nil || errors.Is(err, ErrContractBalanceExhausted) || listed(unknown) {
			t.Fatal("an unknown counter listed its payer", err)
		}

		limited := newEscrowSelectionTestClients(t, ctx)
		limitedIds := redisSelectionTestGrants(t, ctx, limited, 4, 1000, false)
		for _, id := range limitedIds {
			redisGrantProbeToken(t, ctx, id, 1000, 900)
		}
		bounded := context.WithValue(ctx, redisGrantSelectionContextKey{}, redisGrantSelectionLimits{MaxRows: 64, MaxSelected: 2})
		if _, err := limited.create(bounded, 512, false); !errors.Is(err, errRedisGrantSelectionCapacity) || listed(limited) {
			t.Fatal("a selection limit listed its payer", err)
		}
		drainRedisContractCompensations(t, ctx)

		// A payer whose grants could reach the census's row limit is beyond
		// the proof: one proof decides nothing, and the payer's refusals stay
		// with the census for the hold.
		unprovable := newEscrowSelectionTestClients(t, ctx)
		for _, id := range redisSelectionTestGrants(t, ctx, unprovable, 2, 1000, false) {
			redisGrantProbeToken(t, ctx, id, 1000, 1000)
		}
		rowBound := context.WithValue(ctx, redisGrantSelectionContextKey{}, redisGrantSelectionLimits{MaxRows: 64, MaxSelected: 64})
		for attempt := range 3 {
			_, _, counts, _, err := redisExhaustionCreate(t, rowBound, unprovable, 512)
			if !errors.Is(err, ErrContractBalanceExhausted) {
				t.Fatal("the unprovable payer was not refused", attempt, err)
			}
			want := 0.0
			if attempt == 1 {
				want = 1
			}
			if counts["exhaustion-proof unproven"] != want || counts["exhaustion-proof proven"] != 0 {
				t.Fatal("the unprovable payer's proofs", attempt, counts)
			}
		}

		prober := newEscrowSelectionTestClients(t, ctx)
		setDynamicProberIdentityForTest(t, ctx, prober)
		proberIds := redisSelectionTestGrants(t, ctx, prober, 2, 4096, true)
		for _, id := range proberIds {
			redisGrantProbeToken(t, ctx, id, 4096, 4096)
		}
		if _, err := prober.create(ctx, 1024, false); !errors.Is(err, ErrContractBalanceExhausted) || listed(prober) {
			t.Fatal("the internal prober's refusal listed it", err)
		}
	})
}
