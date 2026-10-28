// A compensation the deferred owner gives up on passes each token still
// pending to the recovery marker in one call, so the next page on the balance
// releases it through the fenced SQL custody check at once rather than after
// the pending age. The handoff reads the request's attempts when it runs and
// changes nothing on replay.
package model

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

func redisHandoffTestCount(result string) float64 {
	return testutil.ToFloat64(redisContractReservationResults.WithLabelValues("handoff", result))
}

// One request on the fixture whose action reserves the request's pending token
// and refuses it before publication, so the token is the request's to
// compensate. Its compensation runs under observe.
func runRedisHandoffTestRefusal(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, amount ByteCount,
	observe func(redisReservationCleanup) redisReservationCleanup) (context.Context, *redisContractAdmission) {
	t.Helper()
	request := withRedisContractAdmission(context.WithValue(ctx, redisReservationCleanupContextKey{}, observe))
	refusal := errors.New("synthetic refusal before publication")
	_, err := runRedisContractAdmission(request, func(attempt context.Context) (*TransferEscrow, error) {
		_, finish, err := reserveRedisTransferEscrowBeforeTx(attempt, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, amount)
		if err != nil {
			return nil, err
		}
		finish()
		return nil, refusal
	})
	if !errors.Is(err, refusal) {
		t.Fatal("the refusal fixture changed", err)
	}
	return request, redisAdmissionFromContext(request)
}

// The balance's reservation state: the counter and every token, lease and
// marker, to compare before and after a call.
func redisHandoffTestState(t testing.TB, ctx context.Context, balance server.Id) map[string]any {
	t.Helper()
	keys := redisContractReservationKeys(balance)
	state := map[string]any{}
	server.Redis(ctx, func(client server.RedisClient) {
		total, err := client.Get(ctx, keys[0]).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			t.Fatal(err)
		}
		state["reserved"] = total
		tokens, err := client.HGetAll(ctx, keys[1]).Result()
		server.Raise(err)
		state["tokens"] = tokens
		for _, index := range []int{2, 4, 5} {
			members, err := client.ZRangeWithScores(ctx, keys[index], 0, -1).Result()
			server.Raise(err)
			scores := map[string]float64{}
			for _, member := range members {
				scores[member.Member.(string)] = member.Score
			}
			state[keys[index]] = scores
		}
	})
	return state
}

func requireRedisHandoffTestState(t testing.TB, ctx context.Context, balance server.Id, want map[string]any, step string) {
	t.Helper()
	got := redisHandoffTestState(t, ctx, balance)
	for key, value := range want {
		if !redisHandoffTestEqual(got[key], value) {
			t.Fatalf("%s changed %s: %v, was %v", step, key, got[key], value)
		}
	}
}

func redisHandoffTestEqual(a, b any) bool {
	switch a := a.(type) {
	case string:
		return a == b.(string)
	case map[string]string:
		b := b.(map[string]string)
		if len(a) != len(b) {
			return false
		}
		for key, value := range a {
			if other, ok := b[key]; !ok || other != value {
				return false
			}
		}
		return true
	case map[string]float64:
		b := b.(map[string]float64)
		if len(a) != len(b) {
			return false
		}
		for key, value := range a {
			if other, ok := b[key]; !ok || other != value {
				return false
			}
		}
		return true
	}
	return false
}

// The owner's every release fails until the request's budget ends, here with a
// reset connection, as at a failover: transient, but not the slowness that
// holds the handoff back. It then hands the token to the recovery marker, and
// the next request on the balance releases it at once, not after the pending
// age.
func TestRedisDeferredCompensationGiveUpHandsTokenToRecovery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		deferred := newRedisDeferredCompensationsForTest(t, 1, 1)
		clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
		var releases atomic.Int32
		observe := func(cleanup redisReservationCleanup) redisReservationCleanup {
			cleanup.now, cleanup.after = clock.Now, clock.After
			cleanup.recover = func(context.Context, server.Id, server.Id) error {
				releases.Add(1)
				return syscall.ECONNRESET
			}
			cleanup.deferred = deferred
			return cleanup
		}
		pendingBefore, movedBefore := redisCompensationTestCount("pending"), redisHandoffTestCount("moved")
		_, owner := runRedisHandoffTestRefusal(t, ctx, f, 23, observe)
		if err := deferred.drain(ctx); err != nil {
			t.Fatal("the deferred owner did not give up", err)
		}
		if releases.Load() < 2 {
			t.Fatal("the deferred owner made no pass before it gave up", releases.Load())
		}
		if pending := redisCompensationTestCount("pending") - pendingBefore; pending != 1 {
			t.Fatal("the exhausted compensation was not counted pending", pending)
		}
		if moved := redisHandoffTestCount("moved") - movedBefore; moved != 1 {
			t.Fatal("the owner did not hand the token to recovery in one call", moved)
		}
		// still reserved, now under the recovery marker
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("the handoff released or lost the token", got)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, owner.contractId, false, true)

		next := createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 46 {
			t.Fatal("the next page on the balance did not release the handed-off token", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, owner.contractId, false)
		server.Redis(ctx, func(client server.RedisClient) {
			tokens, err := client.HGetAll(ctx, redisContractReservationKeys(f.balanceId)[1]).Result()
			server.Raise(err)
			if len(tokens) != 2 || tokens[neighbor.ContractId.String()] != "17" || tokens[next.ContractId.String()] != "29" {
				t.Fatal("recovery changed a published token", tokens)
			}
		})
	})
}

// The owner reads the request's attempts when it gives up, under the pending
// lock, never when it took the compensation: a token an attempt of the request
// started on meanwhile, or one whose publication started, keeps its pending
// marker, and no page takes it before its pending age.
func TestRedisDeferredCompensationHandoffSkipsLiveAttemptsAndPublication(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		_ = createRedisAdmissionTest(ctx, f, 17)
		deferred := newRedisDeferredCompensationsForTest(t, 1, 2)
		retry := make(chan struct{})
		observe := func(cleanup redisReservationCleanup) redisReservationCleanup {
			clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
			cleanup.now, cleanup.after = clock.Now, clock.AfterGate(retry, nil)
			// a reset connection: transient, and no timeout holds the handoff back
			cleanup.recover = func(context.Context, server.Id, server.Id) error {
				return syscall.ECONNRESET
			}
			cleanup.deferred = deferred
			return cleanup
		}
		attempting, attempter := runRedisHandoffTestRefusal(t, ctx, f, 23, observe)
		_, publisher := runRedisHandoffTestRefusal(t, ctx, f, 31, observe)
		// Both compensations wait for their retries. Another attempt of the
		// first request takes its pending token over, and the second request
		// starts publication.
		_, finish := reserveRedisAdmissionTestBeforeTx(t, attempting, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
		publisher.notePublication()
		skippedBefore, movedBefore := redisHandoffTestCount("skipped"), redisHandoffTestCount("moved")
		close(retry)
		if err := deferred.drain(ctx); err != nil {
			t.Fatal("the deferred owner did not give up", err)
		}
		finish()
		if skipped := redisHandoffTestCount("skipped") - skippedBefore; skipped != 2 {
			t.Fatal("the owner did not skip the live tokens' handoff", skipped)
		}
		if moved := redisHandoffTestCount("moved") - movedBefore; moved != 0 {
			t.Fatal("the owner handed a live token to recovery", moved)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, attempter.contractId, true, false)
		redisPendingRequireMarkers(t, ctx, f.balanceId, publisher.contractId, true, false)
		_ = createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17+23+31+29 {
			t.Fatal("a page took a live token before its pending age", got)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, attempter.contractId, true, false)

		// Once its attempt has ended, the same handoff moves the token.
		if err := attempter.handOffReservation(ctx, f.balanceId); err != nil {
			t.Fatal("the handoff failed", err)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, attempter.contractId, false, true)
	})
}

// The handoff is one idempotent call on the no-retry deadline pool. A replay,
// as a delayed duplicate of the same call would arrive, finds no pending marker
// and changes nothing, before or after a page has released the token, and the
// handoff never touches a published token.
func TestRedisReservationHandoffReplayChangesNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		dead := server.NewId()
		if reserved, err := redisContractReservation(ctx, "reserve-pending", f.balanceId, dead, 1000, 23, redisContractReservationLease); err != nil || reserved != 23 {
			t.Fatal("pending token setup", reserved, err)
		}
		keys := redisContractReservationKeys(f.balanceId)
		before := redisHandoffTestState(t, ctx, f.balanceId)
		pendingScore, ok := before[keys[5]].(map[string]float64)[dead.String()]
		if !ok {
			t.Fatal("the token has no pending marker")
		}

		// The handoff runs outside any transaction, so as the reservation
		// calls do there, it sends its one script with no preflight PING on
		// the late-reply pool.
		deadline := installRedisPreflightCountHook(ctx)
		late := installRedisPreflightCountHook(ctx, server.OptRedisKeepLateReplies())
		deadline.armed.Store(true)
		late.armed.Store(true)
		moved, err := redisReservationHandoff(ctx, f.balanceId, dead)
		deadline.armed.Store(false)
		late.armed.Store(false)
		if err != nil || !moved {
			t.Fatal("the handoff did not move the pending marker", moved, err)
		}
		if late.scripts.Load() != 1 || late.pings.Load() != 0 || deadline.scripts.Load() != 0 || deadline.pings.Load() != 0 {
			t.Fatalf("the handoff sent %d scripts and %d PINGs on the late-reply pool and %d and %d on the deadline pool; want one script on the late-reply pool alone",
				late.scripts.Load(), late.pings.Load(), deadline.scripts.Load(), deadline.pings.Load())
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, dead, false, true)
		handedOff := redisHandoffTestState(t, ctx, f.balanceId)
		if score := handedOff[keys[4]].(map[string]float64)[dead.String()]; score != pendingScore {
			t.Fatal("the recovery marker did not keep the pending marker's age", score, pendingScore)
		}
		// no amount, lease or other marker changed
		requireRedisHandoffTestState(t, ctx, f.balanceId, map[string]any{
			"reserved": before["reserved"], "tokens": before["tokens"], keys[2]: before[keys[2]],
		}, "the handoff")

		for _, step := range []struct {
			name  string
			token server.Id
		}{{"a replay", dead}, {"a handoff of a published token", neighbor.ContractId}} {
			moved, err := redisReservationHandoff(ctx, f.balanceId, step.token)
			if err != nil || moved {
				t.Fatalf("%s moved a marker: moved=%v err=%v", step.name, moved, err)
			}
			requireRedisHandoffTestState(t, ctx, f.balanceId, handedOff, step.name)
		}

		// The next page on the balance releases the handed-off token...
		next := createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 46 {
			t.Fatal("the page did not release the handed-off token", got)
		}
		released := redisHandoffTestState(t, ctx, f.balanceId)
		// ...and a replay after it resurrects neither the token nor a marker.
		moved, err = redisReservationHandoff(ctx, f.balanceId, dead)
		if err != nil || moved {
			t.Fatal("a replay after the release moved a marker", moved, err)
		}
		requireRedisHandoffTestState(t, ctx, f.balanceId, released, "a replay after the release")
		redisRecoveryRequireMarker(t, ctx, f.balanceId, dead, false)
		if tokens := released["tokens"].(map[string]string); len(tokens) != 2 || tokens[neighbor.ContractId.String()] != "17" || tokens[next.ContractId.String()] != "29" {
			t.Fatal("the released token or a neighbor changed", tokens)
		}
	})
}

// Gives compensations up on a private owner and counts the handoffs it makes.
type redisHandoffTestOwner struct {
	deferred *redisDeferredCompensations
	handoffs atomic.Int32
}

// One compensation of one balance whose every release fails with failure,
// deferred after its request's own pass and run until the owner gives it up.
func (self *redisHandoffTestOwner) GiveUp(t testing.TB, ctx context.Context, failure error) {
	t.Helper()
	clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
	cleanup := redisReservationCleanup{now: clock.Now, after: clock.After, deferred: self.deferred,
		recover: func(context.Context, server.Id, server.Id) error {
			return failure
		},
		handoff: func(context.Context, server.Id, server.Id) error {
			self.handoffs.Add(1)
			return nil
		}}
	if !self.deferred.offer(ctx, cleanup.compensation(server.NewId(), []server.Id{server.NewId()}, clock.Now().Add(redisReservationCleanupBudget))) {
		t.Fatal("the deferred owner refused a compensation it had room for")
	}
	if err := self.deferred.drain(ctx); err != nil {
		t.Fatal("the deferred owner did not give the compensation up", err)
	}
}

// A timeout in one of the owner's calls means a slow Redis node or PostgreSQL
// pool, which the page releasing a handed-off token would wait on while it
// holds a PostgreSQL connection. For the quiet period after one, the owner
// hands no given-up token to recovery, whatever ended its compensation; then
// the handoff resumes.
func TestRedisDeferredCompensationHandoffWaitsOutTimeouts(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	owner := &redisHandoffTestOwner{deferred: newRedisDeferredCompensationsForTest(t, 1, 1)}
	var ownerNanos atomic.Int64
	ownerNanos.Store(time.Unix(1000, 0).UnixNano())
	// set before the first offer starts the owner's workers
	owner.deferred.now = func() time.Time { return time.Unix(0, ownerNanos.Load()) }
	slowBefore := redisHandoffTestCount("slow")

	// releases that time out
	owner.GiveUp(t, ctx, context.DeadlineExceeded)
	if owner.handoffs.Load() != 0 || redisHandoffTestCount("slow")-slowBefore != 1 {
		t.Fatal("a compensation whose releases timed out was handed to recovery", owner.handoffs.Load())
	}
	// within the quiet period, a reset connection gives up without a handoff too
	ownerNanos.Add(int64(redisDeferredCompensationHandoffQuiet - time.Nanosecond))
	owner.GiveUp(t, ctx, syscall.ECONNRESET)
	if owner.handoffs.Load() != 0 || redisHandoffTestCount("slow")-slowBefore != 2 {
		t.Fatal("a give-up within the quiet period after a timeout was handed to recovery", owner.handoffs.Load())
	}
	// once the quiet period has passed, the handoff resumes
	ownerNanos.Add(int64(time.Nanosecond))
	owner.GiveUp(t, ctx, syscall.ECONNRESET)
	if owner.handoffs.Load() != 1 || redisHandoffTestCount("slow")-slowBefore != 2 {
		t.Fatal("the handoff did not resume after the quiet period", owner.handoffs.Load())
	}
}

// The operator switch turns the handoff off and on again while the process
// runs: each give-up reads the current config. Only a false value turns it off,
// in YAML 1.1 spelling too; a file without the key, or one that does not
// decode, keeps it on.
func TestRedisDeferredCompensationHandoffSwitchAppliesLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	owner := &redisHandoffTestOwner{deferred: newRedisDeferredCompensationsForTest(t, 1, 1)}
	disabledBefore := redisHandoffTestCount("disabled")

	off := server.Config.PushSimpleResource(redisContractCompensationResourceName, []byte("handoff: false\n"))
	t.Cleanup(sync.OnceFunc(off))
	owner.GiveUp(t, ctx, syscall.ECONNRESET)
	if owner.handoffs.Load() != 0 || redisHandoffTestCount("disabled")-disabledBefore != 1 {
		t.Fatal("the handoff ran with the switch off", owner.handoffs.Load())
	}
	off()
	owner.GiveUp(t, ctx, syscall.ECONNRESET)
	if owner.handoffs.Load() != 1 {
		t.Fatal("the handoff did not resume once the switch was removed", owner.handoffs.Load())
	}
	for _, setting := range []struct {
		content string
		on      bool
	}{
		{"handoff: true\n", true},
		{"handoff: off\n", false},
		{"handoff: [\n", true},
		{"other: 1\n", true},
	} {
		before := owner.handoffs.Load()
		pop := server.Config.PushSimpleResource(redisContractCompensationResourceName, []byte(setting.content))
		owner.GiveUp(t, ctx, syscall.ECONNRESET)
		pop()
		if on := owner.handoffs.Load() != before; on != setting.on {
			t.Fatalf("the switch %q left the handoff on=%v, want %v", setting.content, on, setting.on)
		}
	}
	if redisHandoffTestCount("disabled")-disabledBefore != 2 {
		t.Fatal("a give-up the switch held back was not counted disabled, or one it let through was")
	}
}

// With the operator switch off, a compensation the owner gives up on leaves its
// token pending for the 10-minute age gate, as before the handoff, and no page
// takes it before then. Turned back on while the process runs, the next
// give-up hands its token to recovery, and the next page releases it at once.
func TestRedisDeferredCompensationHandoffSwitchLeavesTokenPending(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		deferred := newRedisDeferredCompensationsForTest(t, 1, 1)
		observe := func(cleanup redisReservationCleanup) redisReservationCleanup {
			clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
			cleanup.now, cleanup.after = clock.Now, clock.After
			// a reset connection: transient, and no timeout holds the handoff back
			cleanup.recover = func(context.Context, server.Id, server.Id) error {
				return syscall.ECONNRESET
			}
			cleanup.deferred = deferred
			return cleanup
		}
		off := server.Config.PushSimpleResource(redisContractCompensationResourceName, []byte("handoff: false\n"))
		t.Cleanup(sync.OnceFunc(off))
		disabledBefore, movedBefore := redisHandoffTestCount("disabled"), redisHandoffTestCount("moved")
		_, first := runRedisHandoffTestRefusal(t, ctx, f, 23, observe)
		if err := deferred.drain(ctx); err != nil {
			t.Fatal("the deferred owner did not give up", err)
		}
		if disabled := redisHandoffTestCount("disabled") - disabledBefore; disabled != 1 || redisHandoffTestCount("moved") != movedBefore {
			t.Fatal("the give-up was not held back by the switch", disabled)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, first.contractId, true, false)
		_ = createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17+23+29 {
			t.Fatal("a page took the given-up token before its pending age", got)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, first.contractId, true, false)

		off()
		_, second := runRedisHandoffTestRefusal(t, ctx, f, 31, observe)
		if err := deferred.drain(ctx); err != nil {
			t.Fatal("the deferred owner did not give up", err)
		}
		if moved := redisHandoffTestCount("moved") - movedBefore; moved != 1 {
			t.Fatal("the switch turned back on did not apply to the next give-up", moved)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, second.contractId, false, true)
		last := createRedisAdmissionTest(ctx, f, 37)
		// the second token is released; the first still waits out its age
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 17+23+29+37 {
			t.Fatal("the next page did not release the handed-off token", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, second.contractId, false)
		redisPendingRequireMarkers(t, ctx, f.balanceId, first.contractId, true, false)
		server.Redis(ctx, func(client server.RedisClient) {
			tokens, err := client.HGetAll(ctx, redisContractReservationKeys(f.balanceId)[1]).Result()
			server.Raise(err)
			if _, ok := tokens[second.contractId.String()]; ok || tokens[neighbor.ContractId.String()] != "17" || tokens[last.ContractId.String()] != "37" {
				t.Fatal("the release took the wrong token", tokens)
			}
		})
	})
}
