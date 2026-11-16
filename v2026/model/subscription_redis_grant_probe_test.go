// A payer whose grants are all reserved used to pay one full reservation
// script per grant on every request. The probe skips only reservations that
// would refuse: funding, refusals, held counters and recovery discovery stay
// those of the reservation script itself.
package model

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

func TestRedisGrantProbeVerdictProvesOnlyExactRefusals(t *testing.T) {
	for _, test := range []struct {
		name      string
		verdict   redisGrantProbeVerdict
		remaining ByteCount
		whole     bool
		refused   bool
	}{
		{name: "inexact partial", verdict: redisGrantProbeVerdict{}, remaining: 100},
		{name: "inexact whole", verdict: redisGrantProbeVerdict{}, remaining: 100, whole: true},
		{name: "empty partial", verdict: redisGrantProbeVerdict{exact: true}, remaining: 100, refused: true},
		{name: "empty whole", verdict: redisGrantProbeVerdict{exact: true}, remaining: 100, whole: true, refused: true},
		{name: "one byte partial", verdict: redisGrantProbeVerdict{exact: true, available: 1}, remaining: 100},
		{name: "short whole", verdict: redisGrantProbeVerdict{exact: true, available: 99}, remaining: 100, whole: true, refused: true},
		{name: "exact whole", verdict: redisGrantProbeVerdict{exact: true, available: 100}, remaining: 100, whole: true},
		{name: "ample whole", verdict: redisGrantProbeVerdict{exact: true, available: 1 << 40}, remaining: 100, whole: true},
	} {
		if got := test.verdict.refuses(test.remaining, test.whole); got != test.refused {
			t.Errorf("%s: refuses=%t, want %t", test.name, got, test.refused)
		}
	}
	// A grant the batch never read is never skipped.
	probe := redisGrantProbe{contractId: server.NewId()}
	if refused, idle := probe.refuses(server.NewId(), 100, false); refused || idle {
		t.Fatal("an unprobed grant was treated as refused", refused, idle)
	}
}

// Reads start after the first refusal with one grant and double each time,
// skip grants without durable credit, never read a grant twice in one batch,
// and keep their width across batches up to one page.
func TestRedisGrantProbeReadsDoubleAfterTheFirstRefusal(t *testing.T) {
	balances := make([]*escrowTransferBalance, 12)
	for index := range balances {
		balances[index] = &escrowTransferBalance{balanceId: server.NewId(), balanceByteCount: 1000}
	}
	balances[2].balanceByteCount = 0
	ids := func(probed []*escrowTransferBalance) []server.Id {
		result := []server.Id{}
		for _, balance := range probed {
			result = append(result, balance.balanceId)
		}
		return result
	}
	want := func(indexes ...int) []server.Id {
		result := []server.Id{}
		for _, index := range indexes {
			result = append(result, balances[index].balanceId)
		}
		return result
	}
	probe := redisGrantProbe{contractId: server.NewId()}
	if probed := probe.take(balances); len(probed) != 0 {
		t.Fatal("a request without a refusal probed", len(probed))
	}
	probe.noteRefusal()
	for _, step := range []struct {
		next int
		want []server.Id
	}{
		{next: 0, want: want(0)},
		{next: 0, want: want()},
		{next: 1, want: want(1, 3)},
		{next: 2, want: want()},
		{next: 3, want: want()},
		{next: 4, want: want(4, 5, 6, 7)},
		{next: 8, want: want(8, 9, 10, 11)},
	} {
		if got := ids(probe.take(balances[step.next:])); !slices.Equal(got, step.want) {
			t.Fatalf("attempt %d probed %d grants, want %d", step.next, len(got), len(step.want))
		}
	}
	// A read in flight or failed is inexact and never skips a reservation.
	if refused, idle := probe.refuses(balances[11].balanceId, 1, false); refused || idle {
		t.Fatal("an unread grant was treated as refused")
	}
	probe.noteRefusal()
	probe.forget()
	if got := ids(probe.take(balances)); !slices.Equal(got, want(0, 1, 3, 4, 5, 6, 7, 8, 9, 10, 11)) {
		t.Fatal("a new batch did not keep the grown width", len(got))
	}
	for range 8 {
		probe.forget()
		probe.take(balances)
	}
	if probe.width != redisGrantPageRows {
		t.Fatal("probe width exceeded one keyset page", probe.width)
	}
}

func TestRedisGrantProbeRepliesDeferUnlessExact(t *testing.T) {
	for _, test := range []struct {
		reply   []any
		verdict redisGrantProbeVerdict
		result  string
	}{
		{reply: []any{"0", int64(1)}, verdict: redisGrantProbeVerdict{exact: true, idle: true}, result: "empty"},
		{reply: []any{"0", int64(0)}, verdict: redisGrantProbeVerdict{exact: true}, result: "empty"},
		{reply: []any{"1024", int64(0)}, verdict: redisGrantProbeVerdict{exact: true, available: 1024}, result: "available"},
		{reply: []any{"9223372036854775807", int64(1)}, verdict: redisGrantProbeVerdict{exact: true, available: 9223372036854775807, idle: true}, result: "available"},
		{reply: []any{"", int64(0)}, result: "undecided"},
		{reply: []any{"", int64(1)}, result: "undecided"},
		{reply: []any{"-1", int64(1)}, result: "error"},
		{reply: []any{"01", int64(1)}, result: "error"},
		{reply: []any{"x", int64(1)}, result: "error"},
		{reply: []any{"9223372036854775808", int64(1)}, result: "error"},
		{reply: []any{"5", int64(2)}, result: "error"},
		{reply: []any{"5", "1"}, result: "error"},
		{reply: []any{int64(5), int64(1)}, result: "error"},
		{reply: []any{"5"}, result: "error"},
		{reply: nil, result: "error"},
	} {
		verdict, result := parseRedisGrantProbe(test.reply)
		if verdict != test.verdict || result != test.result {
			t.Errorf("reply %v: verdict=%+v result=%s, want %+v %s", test.reply, verdict, result, test.verdict, test.result)
		}
	}
}

// The probe reuses the reservation script's decimal helpers verbatim and
// changes no key: a skipped reservation leaves its grant exactly as it was.
func TestRedisGrantProbeSharesReservationArithmeticAndWritesNothing(t *testing.T) {
	prelude, _, found := strings.Cut(redisContractReservationScript, "local rawTotal=")
	if !found || !strings.HasPrefix(redisGrantProbeScript, prelude) {
		t.Fatal("the probe does not share the reservation script's helpers")
	}
	body := strings.TrimPrefix(redisGrantProbeScript, prelude)
	for _, command := range []string{"SET", "DEL", "HSET", "HDEL", "HINCRBY", "INCRBY", "ZADD", "ZREM", "EXPIRE", "PEXPIRE", "PEXPIREAT", "UNLINK"} {
		if strings.Contains(body, "redis.call('"+command+"'") {
			t.Fatal("the probe writes", command)
		}
	}
}

// Probes one grant as the next batch of an active selection.
func redisGrantProbeObserve(ctx context.Context, request, balance server.Id, credit ByteCount) redisGrantProbeVerdict {
	probe := redisGrantProbe{contractId: request}
	probe.noteRefusal()
	probe.observe(ctx, []*escrowTransferBalance{{balanceId: balance, balanceByteCount: credit}})
	return probe.verdicts[balance]
}

// A published token: no pending or recovery marker remains after its
// request acknowledged it.
func redisGrantProbeToken(t testing.TB, ctx context.Context, balance server.Id, credit, amount ByteCount) server.Id {
	t.Helper()
	contract := server.NewId()
	reserved, err := redisContractReservation(ctx, "reserve", balance, contract, credit, amount, redisContractReservationLease)
	if err != nil || reserved != amount {
		t.Fatal("token setup", reserved, err)
	}
	return contract
}

// Moves one token's lease into the past without touching its amount.
func redisGrantProbeExpireLease(t testing.TB, ctx context.Context, balance, contract server.Id) {
	t.Helper()
	server.Redis(ctx, func(client server.RedisClient) {
		now, err := client.Time(ctx).Result()
		server.Raise(err)
		changed, err := client.ZAddArgs(ctx, redisContractReservationKeys(balance)[2], redis.ZAddArgs{
			XX: true, Ch: true,
			Members: []redis.Z{{Score: float64(now.Add(-time.Second).UnixMilli()), Member: contract.String()}},
		}).Result()
		server.Raise(err)
		if changed != 1 {
			t.Fatal("no lease to expire")
		}
	})
}

// The probe's verdict is checked against the reservation script on the same
// state, in both modes. Soundness: whenever the probe proves a refusal, the
// reservation reserves nothing and fails nothing. Usefulness: plain reserved
// states are proved. Every unusual state defers to the reservation, which may
// fund the request, take over its own token or hold an invalid counter.
func TestRedisGrantProbeAgreesWithReservationScript(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.ApplyDbMigrations = false
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const credit = ByteCount(1000)
		const requested = ByteCount(100)
		redisSet := func(key string, value any) {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Set(ctx, key, value, time.Hour).Err())
			})
		}
		type expectation struct {
			exact     bool
			available ByteCount
			idle      bool
		}
		for _, test := range []struct {
			name  string
			setup func(balance, request server.Id)
			want  expectation
		}{
			{name: "reserved", want: expectation{exact: true, idle: true}, setup: func(balance, request server.Id) {
				redisGrantProbeToken(t, ctx, balance, credit, credit)
			}},
			{name: "reserved with legacy debt", want: expectation{exact: true, idle: true}, setup: func(balance, request server.Id) {
				redisSet(netEscrowKey(balance), 600)
				redisGrantProbeToken(t, ctx, balance, credit, 400)
			}},
			{name: "legacy debt over a zero total", want: expectation{exact: true, idle: true}, setup: func(balance, request server.Id) {
				redisSet(redisContractReservationKeys(balance)[0], "0")
				redisSet(netEscrowKey(balance), credit)
			}},
			{name: "partial room", want: expectation{exact: true, available: 50, idle: true}, setup: func(balance, request server.Id) {
				redisGrantProbeToken(t, ctx, balance, credit, credit-50)
			}},
			{name: "whole room", want: expectation{exact: true, available: requested, idle: true}, setup: func(balance, request server.Id) {
				redisGrantProbeToken(t, ctx, balance, credit, credit-requested)
			}},
			{name: "recovery marker", want: expectation{exact: true}, setup: func(balance, request server.Id) {
				redisGrantProbeToken(t, ctx, balance, credit, credit)
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.ZAdd(ctx, redisContractReservationKeys(balance)[4], redis.Z{Score: 1, Member: server.NewId().String()}).Err())
				})
			}},
			{name: "pending marker", want: expectation{exact: true}, setup: func(balance, request server.Id) {
				if reserved, err := redisContractReservation(ctx, "reserve-pending", balance, server.NewId(), credit, credit, redisContractReservationLease); err != nil || reserved != credit {
					t.Fatal("pending setup", reserved, err)
				}
			}},
			{name: "no generation", setup: func(balance, request server.Id) {
				redisSet(netEscrowKey(balance), credit)
			}},
			{name: "expired lease", setup: func(balance, request server.Id) {
				redisGrantProbeExpireLease(t, ctx, balance, redisGrantProbeToken(t, ctx, balance, credit, credit))
			}},
			{name: "evicted tokens", setup: func(balance, request server.Id) {
				redisGrantProbeToken(t, ctx, balance, credit, credit)
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.Del(ctx, redisContractReservationKeys(balance)[1]).Err())
				})
			}},
			{name: "evicted leases", setup: func(balance, request server.Id) {
				redisGrantProbeToken(t, ctx, balance, credit, credit)
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.Del(ctx, redisContractReservationKeys(balance)[2]).Err())
				})
			}},
			{name: "own token", setup: func(balance, request server.Id) {
				if reserved, err := redisContractReservation(ctx, "reserve-pending", balance, request, credit, 30, redisContractReservationLease); err != nil || reserved != 30 {
					t.Fatal("own token setup", reserved, err)
				}
				redisGrantProbeToken(t, ctx, balance, credit, credit-30)
			}},
			{name: "invalid total", setup: func(balance, request server.Id) {
				redisSet(redisContractReservationKeys(balance)[0], "malformed")
			}},
			{name: "invalid legacy", setup: func(balance, request server.Id) {
				redisGrantProbeToken(t, ctx, balance, credit, credit)
				redisSet(netEscrowKey(balance), "-5")
			}},
			{name: "token key type", setup: func(balance, request server.Id) {
				keys := redisContractReservationKeys(balance)
				redisSet(keys[0], credit)
				redisSet(keys[1], "x")
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.ZAdd(ctx, keys[2], redis.Z{Score: float64(time.Now().Add(time.Hour).UnixMilli()), Member: "x"}).Err())
				})
			}},
			{name: "lease key type", setup: func(balance, request server.Id) {
				keys := redisContractReservationKeys(balance)
				redisSet(keys[0], credit)
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.HSet(ctx, keys[1], "x", credit).Err())
				})
				redisSet(keys[2], "x")
			}},
			{name: "recovery key type", setup: func(balance, request server.Id) {
				redisGrantProbeToken(t, ctx, balance, credit, credit)
				redisSet(redisContractReservationKeys(balance)[4], "x")
			}},
			{name: "pending key type", setup: func(balance, request server.Id) {
				keys := redisContractReservationKeys(balance)
				redisGrantProbeToken(t, ctx, balance, credit, credit)
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.ZAdd(ctx, keys[4], redis.Z{Score: 1, Member: server.NewId().String()}).Err())
				})
				redisSet(keys[5], "x")
			}},
		} {
			for _, whole := range []bool{false, true} {
				operation := "reserve-pending"
				if whole {
					operation = "reserve-pending-full"
				}
				balance, request := server.NewId(), server.NewId()
				test.setup(balance, request)
				verdict := redisGrantProbeObserve(ctx, request, balance, credit)
				got := expectation{exact: verdict.exact, available: verdict.available, idle: verdict.idle}
				if got != test.want {
					t.Errorf("%s: probe %+v, want %+v", test.name, got, test.want)
				}
				amount, err := redisContractReservation(ctx, operation, balance, request, credit, requested, redisContractReservationLease)
				if verdict.refuses(requested, whole) && (amount != 0 || err != nil) {
					t.Errorf("%s %s: the probe proved a refusal the reservation did not make: amount=%d err=%v", test.name, operation, amount, err)
				}
				if test.want.exact && err == nil && !verdict.refuses(requested, whole) && amount == 0 {
					t.Errorf("%s %s: the reservation refused what the probe found available", test.name, operation)
				}
			}
		}
	})
}

// The reservation and probe counters these tests discriminate.
func redisGrantProbeCounts() map[string]float64 {
	counts := map[string]float64{}
	for _, operation := range []string{"reserve-pending", "reserve-pending-full"} {
		for _, result := range []string{"accepted", "refused", "error", "skipped"} {
			counts[operation+" "+result] = testutil.ToFloat64(redisContractReservationResults.WithLabelValues(operation, result))
		}
	}
	for _, result := range []string{"empty", "available", "undecided", "error"} {
		counts["reserve-probe "+result] = testutil.ToFloat64(redisContractReservationResults.WithLabelValues("reserve-probe", result))
	}
	// an unlisted payer never tries to prove its exhaustion
	// (subscription_redis_exhaustion.go)
	for _, result := range []string{"proven", "unproven"} {
		counts["exhaustion-proof "+result] = testutil.ToFloat64(redisContractReservationResults.WithLabelValues("exhaustion-proof", result))
	}
	return counts
}

func redisGrantProbeCountsSince(before map[string]float64) map[string]float64 {
	delta := map[string]float64{}
	for name, value := range redisGrantProbeCounts() {
		if difference := value - before[name]; difference != 0 {
			delta[name] = difference
		}
	}
	return delta
}

// One request with an observable admission owner.
func redisGrantProbeCreate(ctx context.Context, clients escrowSelectionTestClients, amount ByteCount) (*TransferEscrow, *redisContractAdmission, map[string]float64, error) {
	request := withRedisContractAdmission(ctx)
	owner := redisAdmissionFromContext(request)
	before := redisGrantProbeCounts()
	escrow, err := CreateTransferEscrow(request, clients.payerNetworkId, clients.payerId,
		clients.providerNetworkId, clients.providerId, amount)
	return escrow, owner, redisGrantProbeCountsSince(before), err
}

func redisGrantProbeRequireCounts(t testing.TB, name string, got map[string]float64, want map[string]float64) {
	t.Helper()
	for counter := range redisGrantProbeCounts() {
		if got[counter] != want[counter] {
			t.Errorf("%s: %s changed by %v, want %v (all %v)", name, counter, got[counter], want[counter], got)
		}
	}
}

func redisGrantProbeTokenAmount(t testing.TB, ctx context.Context, balance, contract server.Id) (ByteCount, bool) {
	t.Helper()
	var amount ByteCount
	var found bool
	server.Redis(ctx, func(client server.RedisClient) {
		value, err := client.HGet(ctx, redisContractReservationKeys(balance)[1], contract.String()).Int64()
		if errors.Is(err, redis.Nil) {
			return
		}
		server.Raise(err)
		amount, found = value, true
	})
	return amount, found
}

// Production shape: every grant of the payer is fully reserved by published
// contracts, so each request is refused after examining them all. The first
// refusal is a real reservation; the remaining grants are probed in one
// pipeline and skipped. Refusal, retained debt and recovery discovery match a
// reservation per grant, which the same request made before probing.
func TestRedisGrantSelectionSkipsProvenRefusals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()

		// An ordinary payer: earliest-expiry partial reservations.
		ordinary := newEscrowSelectionTestClients(t, ctx)
		ordinaryIds := redisSelectionTestGrants(t, ctx, ordinary, 6, 1000, false)
		for _, id := range ordinaryIds {
			redisGrantProbeToken(t, ctx, id, 1000, 1000)
		}
		escrow, owner, counts, err := redisGrantProbeCreate(ctx, ordinary, 512)
		if escrow != nil || !errors.Is(err, ErrContractBalanceExhausted) {
			t.Fatal("a reserved payer was not refused as exhausted", escrow, err)
		}
		for _, id := range ordinaryIds {
			if got := Testing_NetEscrowByteCount(ctx, id); got != 1000 {
				t.Fatal("refusal changed retained debt", got)
			}
			if _, found := redisGrantProbeTokenAmount(t, ctx, id, owner.contractId); found {
				t.Fatal("refused request kept a token")
			}
		}
		redisGrantProbeRequireCounts(t, "reserved ordinary payer", counts, map[string]float64{
			"reserve-pending refused": 1,
			"reserve-pending skipped": 5,
			"reserve-probe empty":     5,
		})
		if !slices.Equal(owner.attemptedBalanceIds, ordinaryIds[:1]) {
			t.Errorf("recovery discovery of idle reserved grants: %d grants, want the one reserved", len(owner.attemptedBalanceIds))
		}

		// A spreading payer: whole-grant windows, then the keyset census.
		spread := newEscrowSelectionTestClients(t, ctx)
		spreadIds := redisSelectionTestGrants(t, ctx, spread, 6, 1000, true)
		for _, id := range spreadIds {
			redisGrantProbeToken(t, ctx, id, 1000, 1000)
		}
		escrow, owner, counts, err = redisGrantProbeCreate(ctx, spread, 512)
		if escrow != nil || !errors.Is(err, ErrContractBalanceExhausted) {
			t.Fatal("a reserved spreading payer was not refused as exhausted", escrow, err)
		}
		for _, id := range spreadIds {
			if got := Testing_NetEscrowByteCount(ctx, id); got != 1000 {
				t.Fatal("refusal changed retained debt", got)
			}
			if _, found := redisGrantProbeTokenAmount(t, ctx, id, owner.contractId); found {
				t.Fatal("refused request kept a token")
			}
		}
		redisGrantProbeRequireCounts(t, "reserved spreading payer", counts, map[string]float64{
			"reserve-pending-full refused": 1,
			"reserve-pending-full skipped": proberGrantAttemptsPerWindow - 1,
			"reserve-pending skipped":      6,
			// the window's remaining attempts, then every grant of the page
			"reserve-probe empty": proberGrantAttemptsPerWindow - 1 + 6,
		})
		if len(owner.attemptedBalanceIds) != 1 {
			t.Errorf("recovery discovery of idle reserved grants: %d grants, want the one reserved", len(owner.attemptedBalanceIds))
		}
	})
}

// The funded case: earliest-expiry grants are reserved, one holds a dead
// request's aged pending token, and the request is funded from the partial
// remainder of one grant and the next. The same grants fund the same bytes at
// the same priority. Skipped grants are never reserved, while the grant with
// a marker stays in recovery discovery, so this request's recovery page still
// releases the dead token.
func TestRedisGrantSelectionProbeKeepsFundingAndRecovery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := redisSelectionTestGrants(t, ctx, clients, 5, 1000, false)
		redisGrantProbeToken(t, ctx, ids[0], 1000, 1000)
		redisGrantProbeToken(t, ctx, ids[1], 1000, 1000)
		dead := server.NewId()
		if reserved, err := redisContractReservation(ctx, "reserve-pending", ids[2], dead, 1000, 1000, redisContractReservationLease); err != nil || reserved != 1000 {
			t.Fatal("dead request setup", reserved, err)
		}
		redisRecoveryAgePendingMarker(t, ctx, ids[2], dead)
		redisGrantProbeToken(t, ctx, ids[3], 1000, 700)

		escrow, owner, counts, err := redisGrantProbeCreate(ctx, clients, 1000)
		if err != nil || escrow == nil {
			t.Fatal("the remaining credit did not fund the request", escrow, err)
		}
		allocations := map[server.Id]ByteCount{}
		for _, balance := range escrow.Balances {
			allocations[balance.BalanceId] = balance.BalanceByteCount
		}
		if len(allocations) != 2 || allocations[ids[3]] != 300 || allocations[ids[4]] != 700 || escrow.TransferByteCount != 1000 || escrow.Priority != UnpaidPriority {
			t.Fatal("funding changed grants, amounts or priority", escrow)
		}
		for index, want := range []ByteCount{1000, 1000, 0, 1000, 700} {
			if got := Testing_NetEscrowByteCount(ctx, ids[index]); got != want {
				t.Fatalf("grant %d holds %d reserved bytes, want %d", index, got, want)
			}
		}
		if _, found := redisGrantProbeTokenAmount(t, ctx, ids[2], dead); found {
			t.Fatal("the request's recovery page no longer reached the dead token")
		}
		redisGrantProbeRequireCounts(t, "funded after reserved grants", counts, map[string]float64{
			"reserve-pending refused":  1,
			"reserve-pending skipped":  2,
			"reserve-pending accepted": 2,
			"reserve-probe empty":      2,
			"reserve-probe available":  1,
			// the last grant has no reservation counters yet
			"reserve-probe undecided": 1,
		})
		if !slices.Equal(owner.attemptedBalanceIds, []server.Id{ids[0], ids[2], ids[3], ids[4]}) {
			t.Errorf("recovery discovery %d grants, want the reserved, marked and funding grants", len(owner.attemptedBalanceIds))
		}
	})
}

// Every grant is reserved, one of them by a dead request. The skipped grant
// holding its marker stays in recovery discovery, so the refused request's
// recovery releases the dead token and the same request is funded on its
// rerun, as it was when every grant was reserved.
func TestRedisGrantSelectionProbeKeepsRecoveryRerun(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := redisSelectionTestGrants(t, ctx, clients, 2, 1000, false)
		redisGrantProbeToken(t, ctx, ids[0], 1000, 1000)
		dead := server.NewId()
		if reserved, err := redisContractReservation(ctx, "reserve-pending", ids[1], dead, 1000, 1000, redisContractReservationLease); err != nil || reserved != 1000 {
			t.Fatal("dead request setup", reserved, err)
		}
		redisRecoveryAgePendingMarker(t, ctx, ids[1], dead)

		escrow, owner, counts, err := redisGrantProbeCreate(ctx, clients, 29)
		if err != nil || escrow == nil || escrow.ContractId != owner.contractId || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != ids[1] || escrow.Balances[0].BalanceByteCount != 29 {
			t.Fatal("recovery of the skipped grant did not fund the same request", escrow, err)
		}
		if got := Testing_NetEscrowByteCount(ctx, ids[1]); got != 29 {
			t.Fatal("the dead token was not released or custody was duplicated", got)
		}
		if got := Testing_NetEscrowByteCount(ctx, ids[0]); got != 1000 {
			t.Fatal("recovery changed a live grant", got)
		}
		redisGrantProbeRequireCounts(t, "recovered after refusal", counts, map[string]float64{
			// each run refuses the first grant; the first skips the second
			"reserve-pending refused":  2,
			"reserve-pending skipped":  1,
			"reserve-pending accepted": 1,
			"reserve-probe empty":      1,
			"reserve-probe available":  1,
		})
	})
}

// States only a reservation can decide are reserved, never skipped: an
// expired lease still funds the request, and an invalid counter still holds
// the census instead of becoming an exhausted refusal.
func TestRedisGrantSelectionProbeDefersUndecidedStates(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()

		expired := newEscrowSelectionTestClients(t, ctx)
		expiredIds := redisSelectionTestGrants(t, ctx, expired, 2, 1000, false)
		redisGrantProbeToken(t, ctx, expiredIds[0], 1000, 1000)
		redisGrantProbeExpireLease(t, ctx, expiredIds[1], redisGrantProbeToken(t, ctx, expiredIds[1], 1000, 1000))
		escrow, _, counts, err := redisGrantProbeCreate(ctx, expired, 500)
		if err != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != expiredIds[1] || escrow.Balances[0].BalanceByteCount != 500 {
			t.Fatal("an expired lease no longer funded the request", escrow, err)
		}
		redisGrantProbeRequireCounts(t, "expired lease", counts, map[string]float64{
			"reserve-pending refused":  1,
			"reserve-pending accepted": 1,
			"reserve-probe undecided":  1,
		})

		invalid := newEscrowSelectionTestClients(t, ctx)
		invalidIds := redisSelectionTestGrants(t, ctx, invalid, 2, 1000, false)
		redisGrantProbeToken(t, ctx, invalidIds[0], 1000, 1000)
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Set(ctx, redisContractReservationKeys(invalidIds[1])[0], "malformed", time.Hour).Err())
		})
		escrow, _, counts, err = redisGrantProbeCreate(ctx, invalid, 500)
		if escrow != nil || err == nil || errors.Is(err, ErrContractBalanceExhausted) || !strings.Contains(err.Error(), "counter remains unknown") {
			t.Fatal("an invalid counter became a known refusal", escrow, err)
		}
		redisGrantProbeRequireCounts(t, "invalid counter", counts, map[string]float64{
			"reserve-pending refused": 1,
			"reserve-pending error":   1,
			"reserve-probe undecided": 1,
		})
	})
}

// A request funded by the grant after its refusal reads only that grant: the
// probe never costs more than one read beyond the reservations it skips, even
// for a payer with a full page of unread grants behind it.
func TestRedisGrantSelectionProbeReadsGrowWithRefusals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		ids := redisSelectionTestGrants(t, ctx, clients, 12, 1000, false)
		redisGrantProbeToken(t, ctx, ids[0], 1000, 1000)
		for _, id := range ids[1:] {
			redisGrantProbeToken(t, ctx, id, 1000, 100)
		}
		escrow, _, counts, err := redisGrantProbeCreate(ctx, clients, 500)
		if err != nil || escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != ids[1] || escrow.Balances[0].BalanceByteCount != 500 {
			t.Fatal("the next grant did not fund the request", escrow, err)
		}
		redisGrantProbeRequireCounts(t, "funded after one refusal", counts, map[string]float64{
			"reserve-pending refused":  1,
			"reserve-pending accepted": 1,
			"reserve-probe available":  1,
		})
	})
}
