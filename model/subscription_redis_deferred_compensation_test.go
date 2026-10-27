// A request's caller waits for one bounded compensation pass; the deferred
// owner retries the rest with bounded concurrency, backoff on timers and a
// bounded number of balances. Seam tests give each request a logical clock, a
// transport that fails on demand and a private deferred owner; custody tests
// use the fixture PG and Redis.
package model

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// One request's logical clock: a wait advances it at once.
type redisCompensationTestClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *redisCompensationTestClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *redisCompensationTestClock) Wait(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(delay)
	return nil
}

// Times fire on the logical clock: the clock advances by the delay and fire
// runs at once, as a timer's would once that delay had passed.
func (self *redisCompensationTestClock) After(delay time.Duration, fire func()) func() bool {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.now = self.now.Add(delay)
	}()
	fire()
	return func() bool { return false }
}

// Times fire on the logical clock once gate opens, unless the returned stop
// comes first: the clock then advances by the delay and fire runs. A nil gate
// never opens. Each timed retry sends to armed, when it has room.
func (self *redisCompensationTestClock) AfterGate(gate <-chan struct{}, armed chan<- struct{}) func(time.Duration, func()) func() bool {
	return func(delay time.Duration, fire func()) func() bool {
		stopped := make(chan struct{})
		stop := sync.OnceValue(func() bool {
			close(stopped)
			return true
		})
		select {
		case armed <- struct{}{}:
		default:
		}
		go func() {
			select {
			case <-gate:
			case <-stopped:
				return
			}
			select {
			case <-stopped:
			default:
				self.After(delay, fire)
			}
		}()
		return stop
	}
}

func redisCompensationTestCount(result string) float64 {
	return testutil.ToFloat64(redisContractReservationResults.WithLabelValues("compensation", result))
}

// A private deferred owner, quiesced and stopped when the test ends.
func newRedisDeferredCompensationsForTest(t testing.TB, workers, balances int) *redisDeferredCompensations {
	t.Helper()
	deferred := newRedisDeferredCompensations(workers, balances)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), redisDeferredCompensationQuiesceTimeout)
		defer cancel()
		_ = deferred.quiesce(ctx)
		deferred.stopWorkers()
	})
	return deferred
}

// Waits for the process-wide deferred owner to end every compensation, so a
// test observes a release that its request's own pass left to that owner. A
// compensation ends within its budget, plus a release call begun before the
// budget ended and the give-up that follows it, each of which a late reply can
// stretch by a socket timeout.
func drainRedisContractCompensations(t testing.TB, ctx context.Context) {
	t.Helper()
	bounded, cancel := context.WithTimeout(ctx, redisReservationCleanupBudget+4*redisContractAdmissionTimeout)
	defer cancel()
	if err := deferredRedisCompensations.drain(bounded); err != nil {
		t.Fatal("the deferred compensations did not end", err)
	}
}

// The most passes one compensation can make within its budget: every retry
// waits the least its backoff allows, half of the nominal delay that doubles
// from one second to the thirty-second cap.
func redisCompensationTestMaxPasses() int {
	passes := 1
	elapsed := time.Duration(0)
	for retry := 0; ; retry++ {
		elapsed += min(time.Second<<retry, 30*time.Second) / 2
		if redisReservationCleanupBudget <= elapsed {
			return passes
		}
		passes++
	}
}

// One public admission whose action leaves a possibly reserved token on each
// balance, as reservations with lost replies do, and returns refusal.
func runRedisCompensationTestRequest(ctx context.Context, balances []server.Id, refusal error,
	observe func(redisReservationCleanup) redisReservationCleanup) (returnErr error) {
	if value := server.HandleError(func() {
		_, returnErr = runRedisContractAdmission(context.WithValue(ctx, redisReservationCleanupContextKey{}, observe), func(owned context.Context) (*TransferEscrow, error) {
			for _, balance := range balances {
				redisAdmissionFromContext(owned).noteBalance(balance)
			}
			return nil, refusal
		})
	}); value != nil {
		returnErr = fmt.Errorf("admission panic: %v", value)
	}
	return
}

// Returns the caller's result if it returns within limit. Otherwise release
// lets a caller still in its cleanup finish before the test fails.
func redisCompensationTestReturn(t testing.TB, limit time.Duration, release func(), call func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		release()
		<-done
		t.Fatalf("the caller waited beyond %s for its compensation instead of one bounded pass", limit)
		return nil
	}
}

// The caller returns after its own pass, with its refusal. Only then does the
// deferred owner retry, on its backoff, and its later success completes the
// compensation.
func TestRedisDeferredCompensationCallerWaitsForOnePass(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		balance := server.NewId()
		refusal := errors.New("synthetic refusal before publication")
		deferred := newRedisDeferredCompensationsForTest(t, 1, 1)
		clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
		retry := make(chan struct{})
		var healthy atomic.Bool
		var stateLock sync.Mutex
		var attempts []time.Time
		var requests []server.Id
		observe := func(cleanup redisReservationCleanup) redisReservationCleanup {
			cleanup.now = clock.Now
			cleanup.after = clock.AfterGate(retry, nil)
			cleanup.recover = func(_ context.Context, id, request server.Id) error {
				func() {
					stateLock.Lock()
					defer stateLock.Unlock()
					attempts = append(attempts, clock.Now())
					requests = append(requests, request)
				}()
				if id != balance {
					t.Error("compensation acquired another balance", id)
				}
				if !healthy.Load() {
					// a pool timeout under the round trip's own deadline
					return context.DeadlineExceeded
				}
				return nil
			}
			cleanup.deferred = deferred
			return cleanup
		}
		takenBefore, completedBefore := redisCompensationTestCount("deferred"), redisCompensationTestCount("completed")
		err := redisCompensationTestReturn(t, 2*time.Second, func() { close(retry) }, func() error {
			return runRedisCompensationTestRequest(ctx, []server.Id{balance}, refusal, observe)
		})
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			if len(attempts) != 1 {
				t.Fatal("the caller made other than one compensation attempt", len(attempts))
			}
		}()
		if !errors.Is(err, refusal) || errors.Is(err, errRedisReservationCleanupPending) {
			t.Fatal("the caller's refusal changed or reported its deferred compensation abandoned", err)
		}
		if taken := redisCompensationTestCount("deferred") - takenBefore; taken != 1 {
			t.Fatal("the pending compensation did not pass to the deferred owner", taken)
		}

		healthy.Store(true)
		close(retry)
		if err := deferred.drain(ctx); err != nil {
			t.Fatal("the deferred owner did not end the compensation", err)
		}
		if completed := redisCompensationTestCount("completed") - completedBefore; completed != 1 {
			t.Fatal("the deferred owner did not complete the compensation", completed)
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		if len(attempts) != 2 || requests[0] != requests[1] {
			t.Fatal("the deferred owner did not retry the exact request once", len(attempts))
		}
		if backoff := attempts[1].Sub(attempts[0]); backoff < 500*time.Millisecond || time.Second <= backoff {
			t.Fatal("the first deferred retry did not follow its backoff", backoff)
		}
	})
}

// However many requests fail together, retries run on the deferred owner's
// workers only, each request's budget bounds its passes by backoff, and the
// owner hands each exhausted request's token to recovery once, after its last
// pass.
func TestRedisDeferredCompensationBoundsRetriesAcrossRequests(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		const requests = 32
		refusal := errors.New("synthetic refusal before publication")
		deferred := newRedisDeferredCompensationsForTest(t, redisDeferredCompensationWorkers, requests)
		var stateLock sync.Mutex
		attempts := map[server.Id]int{}
		handoffs := map[server.Id]int{}
		retrying, maxRetrying := 0, 0
		observe := func(cleanup redisReservationCleanup) redisReservationCleanup {
			clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
			cleanup.now, cleanup.after = clock.Now, clock.After
			cleanup.handoff = func(_ context.Context, _, request server.Id) error {
				stateLock.Lock()
				defer stateLock.Unlock()
				if handoffs[request] == 0 && attempts[request] < 2 {
					t.Error("the owner gave up before its deferred pass", request)
				}
				handoffs[request]++
				return nil
			}
			cleanup.recover = func(_ context.Context, _, request server.Id) error {
				first := func() bool {
					stateLock.Lock()
					defer stateLock.Unlock()
					attempts[request]++
					if attempts[request] == 1 {
						return true
					}
					retrying++
					maxRetrying = max(maxRetrying, retrying)
					return false
				}()
				if !first {
					// a retry's round trip, so that concurrent retries overlap
					time.Sleep(time.Millisecond)
					func() {
						stateLock.Lock()
						defer stateLock.Unlock()
						retrying--
					}()
				}
				return context.DeadlineExceeded
			}
			cleanup.deferred = deferred
			return cleanup
		}
		takenBefore, pendingBefore := redisCompensationTestCount("deferred"), redisCompensationTestCount("pending")
		errs := make(chan error, requests)
		var joined sync.WaitGroup
		for range requests {
			joined.Go(func() {
				errs <- runRedisCompensationTestRequest(ctx, []server.Id{server.NewId()}, refusal, observe)
			})
		}
		joined.Wait()
		close(errs)
		for err := range errs {
			if !errors.Is(err, refusal) {
				t.Fatal("a caller's refusal changed", err)
			}
		}
		if err := deferred.drain(ctx); err != nil {
			t.Fatal("the deferred owner did not end every compensation", err)
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		if maxRetrying < 1 || redisDeferredCompensationWorkers < maxRetrying {
			t.Fatalf("%d compensation retries ran at once; the deferred owner has %d workers", maxRetrying, redisDeferredCompensationWorkers)
		}
		if len(attempts) != requests {
			t.Fatal("compensation attempts lost a request", len(attempts))
		}
		maxPasses := redisCompensationTestMaxPasses()
		for request, count := range attempts {
			if count < 2 || maxPasses < count {
				t.Fatalf("request %s made %d compensation passes in its budget; backoff allows at most %d", request, count, maxPasses)
			}
			if handoffs[request] != 1 {
				t.Fatalf("request %s handed its exhausted token to recovery %d times", request, handoffs[request])
			}
		}
		if taken := redisCompensationTestCount("deferred") - takenBefore; taken != requests {
			t.Fatal("the pending compensations did not pass to the deferred owner", taken)
		}
		if pending := redisCompensationTestCount("pending") - pendingBefore; pending != requests {
			t.Fatal("an exhausted compensation was not counted pending", pending)
		}
	})
}

// A compensation waiting out its backoff holds no worker: while one waits for
// its retry, another's due pass runs at once on the owner's only worker.
func TestRedisDeferredCompensationWaitingRetryHoldsNoWorker(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	deferred := newRedisDeferredCompensationsForTest(t, 1, 2)
	// The first compensation's backoff does not end while the test runs.
	never := make(chan struct{})
	waiting := make(chan struct{}, 1)
	first := &redisCompensationTestClock{now: time.Unix(1, 0)}
	held := redisReservationCleanup{now: first.Now, after: first.AfterGate(never, waiting), deferred: deferred,
		// as an owner whose workers sleep through backoff would wait
		wait: func(ctx context.Context, delay time.Duration) error {
			select {
			case waiting <- struct{}{}:
			default:
			}
			select {
			case <-never:
			case <-ctx.Done():
				return ctx.Err()
			}
			return first.Wait(ctx, delay)
		},
		recover: func(context.Context, server.Id, server.Id) error {
			return context.DeadlineExceeded
		}}
	// The second's retry falls due at once, and its release succeeds.
	second := &redisCompensationTestClock{now: time.Unix(1, 0)}
	passed := make(chan struct{})
	pass := sync.OnceFunc(func() { close(passed) })
	ready := redisReservationCleanup{now: second.Now, wait: second.Wait, after: second.After, deferred: deferred,
		recover: func(context.Context, server.Id, server.Id) error {
			pass()
			return nil
		}}
	if !deferred.offer(ctx, held.compensation(server.NewId(), []server.Id{server.NewId()}, first.Now().Add(redisReservationCleanupBudget))) {
		t.Fatal("the deferred owner refused a compensation it had room for")
	}
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("the first compensation did not wait for its retry", ctx.Err())
	}
	completedBefore := redisCompensationTestCount("completed")
	if !deferred.offer(ctx, ready.compensation(server.NewId(), []server.Id{server.NewId()}, second.Now().Add(redisReservationCleanupBudget))) {
		t.Fatal("the deferred owner refused a compensation it had room for")
	}
	select {
	case <-passed:
	case <-time.After(10 * time.Second):
		t.Fatal("a compensation waiting for its retry held the owner's only worker")
	}
	for redisCompensationTestCount("completed") == completedBefore {
		select {
		case <-ctx.Done():
			t.Fatal("the second compensation did not complete", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The owner's throughput is bounded by its concurrent release calls, not by
// backoff: with one worker and real timers, every compensation of a burst
// makes its deferred pass within its budget, all waiting out their backoff at
// once. A worker that slept through each backoff served one compensation per
// backoff, so the end of the burst expired unattempted.
func TestRedisDeferredCompensationRetriesOnTimersNotWorkers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	const burst = 16
	deferred := newRedisDeferredCompensationsForTest(t, 1, burst)
	cleanup := defaultRedisReservationCleanup()
	var passes atomic.Int32
	cleanup.recover = func(context.Context, server.Id, server.Id) error {
		passes.Add(1)
		return nil
	}
	cleanup.deferred = deferred
	completedBefore, expiredBefore := redisCompensationTestCount("completed"), redisCompensationTestCount("expired")
	for range burst {
		// each request's own pass has just left its one balance pending
		compensation := cleanup.compensation(server.NewId(), []server.Id{server.NewId()}, time.Now().Add(redisReservationCleanupBudget))
		if !deferred.offer(ctx, compensation) {
			t.Fatal("the deferred owner refused a compensation it had room for")
		}
	}
	if err := deferred.drain(ctx); err != nil {
		t.Fatal("the deferred owner did not end the burst", err)
	}
	if expired := redisCompensationTestCount("expired") - expiredBefore; expired != 0 {
		t.Fatalf("%d of %d compensations expired before the owner's one worker reached them", int(expired), burst)
	}
	if completed := redisCompensationTestCount("completed") - completedBefore; completed != burst || passes.Load() != burst {
		t.Fatal("the burst's deferred passes did not each complete once", completed, passes.Load())
	}
}

// A cleanup made without a timer times its deferred retries on the runtime's
// timers: the first from the offer, and the next from a worker, outside the
// worker's recovery, where a raise would end the process.
func TestRedisDeferredCompensationWithoutTimerRetriesOnRuntimeTimers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	deferred := newRedisDeferredCompensationsForTest(t, 1, 1)
	var passes atomic.Int32
	cleanup := redisReservationCleanup{now: time.Now, deferred: deferred,
		recover: func(context.Context, server.Id, server.Id) error {
			if passes.Add(1) == 1 {
				// the first deferred pass fails, so a worker times the next
				return syscall.ECONNRESET
			}
			return nil
		}}
	completedBefore := redisCompensationTestCount("completed")
	offered := false
	if value := server.HandleError(func() {
		offered = deferred.offer(ctx, cleanup.compensation(server.NewId(), []server.Id{server.NewId()}, time.Now().Add(redisReservationCleanupBudget)))
	}); value != nil {
		t.Fatal("offering a compensation made without a timer raised", value)
	}
	if !offered {
		t.Fatal("the deferred owner refused a compensation it had room for")
	}
	if err := deferred.drain(ctx); err != nil {
		t.Fatal("the deferred owner did not end the compensation", err)
	}
	if passes.Load() != 2 || redisCompensationTestCount("completed")-completedBefore != 1 {
		t.Fatal("the compensation's retries did not run on the runtime's timers", passes.Load())
	}
}

// The deferred owner holds a bounded number of balances. Past it a
// compensation is dropped to retained recovery: its caller neither blocks nor
// starts a goroutine, and the drop is counted.
func TestRedisDeferredCompensationOverflowDropsWithoutBlocking(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		refusal := errors.New("synthetic refusal before publication")
		// one worker, with room for three balances
		deferred := newRedisDeferredCompensationsForTest(t, 1, 3)
		hold := make(chan struct{})
		release := sync.OnceFunc(func() { close(hold) })
		defer release()
		holding := make(chan struct{}, 1)
		observe := func(cleanup redisReservationCleanup) redisReservationCleanup {
			clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
			cleanup.now = clock.Now
			cleanup.after = clock.AfterGate(hold, holding)
			cleanup.recover = func(context.Context, server.Id, server.Id) error {
				return context.DeadlineExceeded
			}
			cleanup.deferred = deferred
			return cleanup
		}
		request := func(balances int) error {
			ids := make([]server.Id, balances)
			for index := range ids {
				ids[index] = server.NewId()
			}
			return redisCompensationTestReturn(t, 2*time.Second, release, func() error {
				return runRedisCompensationTestRequest(ctx, ids, refusal, observe)
			})
		}
		takenBefore, droppedBefore, pendingBefore := redisCompensationTestCount("deferred"), redisCompensationTestCount("dropped"), redisCompensationTestCount("pending")
		// One balance waits for its retry...
		if err := request(1); !errors.Is(err, refusal) || errors.Is(err, errRedisReservationCleanupPending) {
			t.Fatal("the first deferred compensation changed its caller's result", err)
		}
		select {
		case <-holding:
		case <-ctx.Done():
			t.Fatal("the deferred owner did not take the first compensation", ctx.Err())
		}
		// ...three more would exceed the owner's balances, though a
		// compensation would fit...
		if err := request(3); !errors.Is(err, refusal) || !errors.Is(err, errRedisReservationCleanupPending) {
			t.Fatal("a compensation beyond the owner's balances was not dropped", err)
		}
		// ...and two more fill it.
		if err := request(2); !errors.Is(err, refusal) || errors.Is(err, errRedisReservationCleanupPending) {
			t.Fatal("the held compensation changed its caller's result", err)
		}
		if taken := redisCompensationTestCount("deferred") - takenBefore; taken != 2 {
			t.Fatal("the owner did not take the compensations that fit", taken)
		}

		const overflow = 64
		goroutines := runtime.NumGoroutine()
		for range overflow {
			err := request(1)
			if !errors.Is(err, refusal) || !errors.Is(err, errRedisReservationCleanupPending) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("a dropped compensation was not reported to its caller as left to recovery", err)
			}
		}
		if grown := runtime.NumGoroutine() - goroutines; overflow/2 <= grown {
			t.Fatalf("%d dropped compensations grew the process by %d goroutines", overflow, grown)
		}
		if dropped := redisCompensationTestCount("dropped") - droppedBefore; dropped != 1+overflow {
			t.Fatal("the full owner did not count each dropped compensation", dropped)
		}
		if taken := redisCompensationTestCount("deferred") - takenBefore; taken != 2 {
			t.Fatal("a full owner took another compensation", taken)
		}

		release()
		if err := deferred.drain(ctx); err != nil {
			t.Fatal("the deferred owner did not end its compensations", err)
		}
		if pending := redisCompensationTestCount("pending") - pendingBefore; pending != 2 {
			t.Fatal("the held compensations did not end pending", pending)
		}
	})
}

// A known insufficient-balance refusal reruns only after its own tokens are
// released. A pass that reaches its deadline with a token left returns the
// refusal without a rerun, and the deferred owner releases the token later.
func TestRedisInsufficientRefusalSkipsRerunWhileCompensationPends(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		balance := server.NewId()
		deferred := newRedisDeferredCompensationsForTest(t, 1, 1)
		clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
		retry := make(chan struct{})
		var healthy atomic.Bool
		var recovered atomic.Int32
		observe := func(cleanup redisReservationCleanup) redisReservationCleanup {
			cleanup.now = clock.Now
			cleanup.after = clock.AfterGate(retry, nil)
			cleanup.recover = func(owner context.Context, _, _ server.Id) error {
				recovered.Add(1)
				if healthy.Load() {
					return nil
				}
				// Redis answers nothing before the pass's own deadline.
				select {
				case <-owner.Done():
					return owner.Err()
				case <-retry:
					return context.DeadlineExceeded
				}
			}
			cleanup.deferred = deferred
			return cleanup
		}
		var actions atomic.Int32
		takenBefore, completedBefore := redisCompensationTestCount("deferred"), redisCompensationTestCount("completed")
		started := time.Now()
		err := redisCompensationTestReturn(t, 3*time.Second, func() { close(retry) }, func() (returnErr error) {
			if value := server.HandleError(func() {
				_, returnErr = runRedisContractAdmission(context.WithValue(ctx, redisReservationCleanupContextKey{}, observe), func(owned context.Context) (*TransferEscrow, error) {
					actions.Add(1)
					redisAdmissionFromContext(owned).noteBalance(balance)
					return nil, fmt.Errorf("%w (%d).", errRedisReservationInsufficient, 0)
				})
			}); value != nil {
				returnErr = fmt.Errorf("admission panic: %v", value)
			}
			return
		})
		elapsed := time.Since(started)
		if actions.Load() != 1 {
			t.Fatal("the refusal reran before its own token was released", actions.Load())
		}
		if !errors.Is(err, errRedisReservationInsufficient) || errors.Is(err, errRedisReservationCleanupPending) {
			t.Fatal("the caller did not receive its refusal", err)
		}
		if recovered.Load() != 1 || elapsed < redisContractAdmissionTimeout {
			t.Fatal("the refusal's own pass was not one attempt ended by its deadline", recovered.Load(), elapsed)
		}
		if taken := redisCompensationTestCount("deferred") - takenBefore; taken != 1 {
			t.Fatal("the pending compensation did not pass to the deferred owner", taken)
		}

		healthy.Store(true)
		close(retry)
		if err := deferred.drain(ctx); err != nil {
			t.Fatal("the deferred owner did not end the compensation", err)
		}
		if completed := redisCompensationTestCount("completed") - completedBefore; completed != 1 {
			t.Fatal("the deferred owner did not release the refused token", completed)
		}
		if recovered.Load() != 2 || actions.Load() != 1 {
			t.Fatal("the deferred release reran the action or retried a released token", recovered.Load(), actions.Load())
		}
	})
}

// The compensation after an insufficient-balance rerun still makes the
// request's own pass, bounded by its own deadline alone, however much of the
// request's shared budget the rerun spent.
func TestRedisRerunCompensationMakesItsOwnPassAfterTheBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		// A dead request's aged token, which the refused request's page
		// releases, so that the request reruns.
		dead := server.NewId()
		if reserved, err := redisContractReservation(ctx, "reserve-pending", f.balanceId, dead, 1000, 5, redisContractReservationLease); err != nil || reserved != 5 {
			t.Fatal("dead request setup", reserved, err)
		}
		redisRecoveryAgePendingMarker(t, ctx, f.balanceId, dead)
		deferred := newRedisDeferredCompensationsForTest(t, 1, 1)
		clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
		var recovered atomic.Int32
		observe := func(cleanup redisReservationCleanup) redisReservationCleanup {
			cleanup.now, cleanup.after = clock.Now, clock.After
			cleanup.recover = func(context.Context, server.Id, server.Id) error {
				recovered.Add(1)
				return nil
			}
			cleanup.deferred = deferred
			return cleanup
		}
		refusal := errors.New("synthetic refusal of the rerun")
		var actions atomic.Int32
		takenBefore, completedBefore := redisCompensationTestCount("deferred"), redisCompensationTestCount("completed")
		_, err := runRedisContractAdmission(context.WithValue(ctx, redisReservationCleanupContextKey{}, observe), func(owned context.Context) (*TransferEscrow, error) {
			redisAdmissionFromContext(owned).noteBalance(f.balanceId)
			if actions.Add(1) == 1 {
				return nil, fmt.Errorf("%w (%d).", errRedisReservationInsufficient, 0)
			}
			// the rerun outlasts the request's whole compensation budget
			if err := clock.Wait(owned, redisReservationCleanupBudget); err != nil {
				return nil, err
			}
			return nil, refusal
		})
		if actions.Load() != 2 || !errors.Is(err, refusal) {
			t.Fatal("the refusal did not rerun after its page released the dead token", actions.Load(), err)
		}
		if recovered.Load() != 2 {
			t.Fatal("the compensation after the rerun made no pass of its own", recovered.Load())
		}
		if taken := redisCompensationTestCount("deferred") - takenBefore; taken != 0 {
			t.Fatal("a compensation its own pass completed passed to the deferred owner", taken)
		}
		if completed := redisCompensationTestCount("completed") - completedBefore; completed != 2 {
			t.Fatal("the request's compensations did not both complete", completed)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 0 {
			t.Fatal("the page did not release the dead token", got)
		}
	})
}

// A raise inside a deferred compensation, even a runtime error, ends only that
// compensation, counted pending; the worker goes on with the next.
func TestRedisDeferredCompensationWorkerSurvivesPanic(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	deferred := newRedisDeferredCompensationsForTest(t, 1, 2)
	clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
	poisoned, healthy := server.NewId(), server.NewId()
	cleanup := redisReservationCleanup{now: clock.Now, after: clock.After, deferred: deferred,
		recover: func(_ context.Context, id, _ server.Id) error {
			if id == poisoned {
				// a programming error in the release path
				return []error{}[len(id.String())]
			}
			return nil
		}}
	pendingBefore, completedBefore := redisCompensationTestCount("pending"), redisCompensationTestCount("completed")
	// each request's own pass has already run
	for _, id := range []server.Id{poisoned, healthy} {
		compensation := cleanup.compensation(server.NewId(), []server.Id{id}, clock.Now().Add(redisReservationCleanupBudget))
		if !deferred.offer(ctx, compensation) {
			t.Fatal("the deferred owner refused a compensation it had room for")
		}
	}
	if err := deferred.drain(ctx); err != nil {
		t.Fatal("the worker did not survive a raise", err)
	}
	if pending := redisCompensationTestCount("pending") - pendingBefore; pending != 1 {
		t.Fatal("the raised compensation was not counted pending", pending)
	}
	if completed := redisCompensationTestCount("completed") - completedBefore; completed != 1 {
		t.Fatal("the worker did not go on after a raise", completed)
	}
}

// A compensation whose budget leaves the deferred owner no pass makes none
// and is counted expired, not pending: one whose budget ends as its request's
// own pass leaves it pending, and one whose budget ends while it waits for a
// worker after its retry fell due.
func TestRedisDeferredCompensationExpiredInQueueMakesNoPass(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	deferred := newRedisDeferredCompensationsForTest(t, 1, 2)
	var calls atomic.Int32
	recover := func(context.Context, server.Id, server.Id) error {
		calls.Add(1)
		return nil
	}
	expiredBefore, completedBefore, pendingBefore := redisCompensationTestCount("expired"), redisCompensationTestCount("completed"), redisCompensationTestCount("pending")
	// the budget ends as the request's own pass leaves the balance pending
	ended := &redisCompensationTestClock{now: time.Unix(1, 0)}
	cleanup := redisReservationCleanup{now: ended.Now, after: ended.After, deferred: deferred, recover: recover}
	if !deferred.offer(ctx, cleanup.compensation(server.NewId(), []server.Id{server.NewId()}, ended.Now())) {
		t.Fatal("the deferred owner refused a compensation it had room for")
	}
	// the budget ends while the compensation waits for a worker
	late := &redisCompensationTestClock{now: time.Unix(1, 0)}
	queued := redisReservationCleanup{now: late.Now, deferred: deferred, recover: recover,
		after: func(delay time.Duration, fire func()) func() bool {
			return late.After(delay+redisReservationCleanupBudget, fire)
		}}
	if !deferred.offer(ctx, queued.compensation(server.NewId(), []server.Id{server.NewId()}, late.Now().Add(redisReservationCleanupBudget))) {
		t.Fatal("the deferred owner refused a compensation it had room for")
	}
	if err := deferred.drain(ctx); err != nil {
		t.Fatal("the deferred owner did not end the expired compensations", err)
	}
	if calls.Load() != 0 {
		t.Fatal("an expired compensation made a pass", calls.Load())
	}
	if expired := redisCompensationTestCount("expired") - expiredBefore; expired != 2 {
		t.Fatal("the expired compensations were not counted expired", expired)
	}
	if redisCompensationTestCount("completed") != completedBefore || redisCompensationTestCount("pending") != pendingBefore {
		t.Fatal("an expired compensation was counted as another outcome")
	}
}

// The process-wide quiesce, run before exit and by each test environment
// reset, ends what the deferred owner holds without waiting out its budget.
func TestRedisContractCompensationQuiesceEndsHeldCompensations(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
	holding := make(chan struct{}, 1)
	// its retry never falls due
	cleanup := redisReservationCleanup{now: clock.Now, after: clock.AfterGate(nil, holding), deferred: deferredRedisCompensations,
		recover: func(context.Context, server.Id, server.Id) error {
			return context.DeadlineExceeded
		}}
	pendingBefore := redisCompensationTestCount("pending")
	compensation := cleanup.compensation(server.NewId(), []server.Id{server.NewId()}, clock.Now().Add(redisReservationCleanupBudget))
	if !deferredRedisCompensations.offer(ctx, compensation) {
		t.Fatal("the process-wide deferred owner refused a compensation")
	}
	select {
	case <-holding:
	case <-ctx.Done():
		t.Fatal("the deferred owner did not take the compensation", ctx.Err())
	}
	quiesced, stop := context.WithTimeout(ctx, redisDeferredCompensationQuiesceTimeout)
	defer stop()
	QuiesceRedisContractCompensations(quiesced)
	if err := deferredRedisCompensations.drain(quiesced); err != nil {
		t.Fatal("the quiesce did not end the held compensation", err)
	}
	if pending := redisCompensationTestCount("pending") - pendingBefore; pending != 1 {
		t.Fatal("the quiesced compensation was not left pending to recovery", pending)
	}
}

// The owner's gauges read the process-wide owner: the balances it holds,
// waiting for a retry or in a pass, and the release calls its workers are
// making, one per worker at most.
func TestRedisDeferredCompensationGaugesReadTheProcessWideOwner(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := deferredRedisCompensations.drain(ctx); err != nil {
		t.Fatal("an earlier compensation is still held", err)
	}
	held := func() float64 { return testutil.ToFloat64(redisDeferredCompensationBalancesHeld) }
	calls := func() float64 { return testutil.ToFloat64(redisDeferredCompensationCallsInflight) }
	if held() != 0 || calls() != 0 {
		t.Fatal("an idle owner reported held balances or calls", held(), calls())
	}
	clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
	retry := make(chan struct{})
	armed := make(chan struct{}, 1)
	calling := make(chan struct{})
	call := sync.OnceFunc(func() { close(calling) })
	release := make(chan struct{})
	cleanup := redisReservationCleanup{now: clock.Now, after: clock.AfterGate(retry, armed), deferred: deferredRedisCompensations,
		recover: func(ctx context.Context, _, _ server.Id) error {
			call()
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	balances := []server.Id{server.NewId(), server.NewId(), server.NewId()}
	if !deferredRedisCompensations.offer(ctx, cleanup.compensation(server.NewId(), balances, clock.Now().Add(redisReservationCleanupBudget))) {
		t.Fatal("the process-wide deferred owner refused a compensation")
	}
	select {
	case <-armed:
	case <-ctx.Done():
		t.Fatal("the compensation did not wait for its retry", ctx.Err())
	}
	// waiting for its retry: every balance held, no call
	if held() != 3 || calls() != 0 {
		t.Fatal("the gauges did not show a compensation waiting for its retry", held(), calls())
	}
	close(retry)
	select {
	case <-calling:
	case <-ctx.Done():
		t.Fatal("the compensation's retry made no call", ctx.Err())
	}
	// in its pass, one release call at a time
	if held() != 3 || calls() != 1 {
		t.Fatal("the gauges did not show the pass's release call", held(), calls())
	}
	close(release)
	if err := deferredRedisCompensations.drain(ctx); err != nil {
		t.Fatal("the deferred owner did not end the compensation", err)
	}
	if held() != 0 || calls() != 0 {
		t.Fatal("the gauges did not return to zero", held(), calls())
	}
}

// A compensation the deferred owner holds reads its request's attempts when it
// runs, not when it was deferred: a token its request has published meanwhile
// is acknowledged through the fenced SQL custody check, never released.
func TestRedisDeferredCompensationRereadsPublicationWhenItRuns(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		neighbor := createRedisAdmissionTest(ctx, f, 17)
		deferred := newRedisDeferredCompensationsForTest(t, 1, 1)
		// the budget elapses only in the deferred owner's backoff, never while
		// the publication fixture runs
		clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
		retry := make(chan struct{})
		var attempts atomic.Int32
		observe := func(cleanup redisReservationCleanup) redisReservationCleanup {
			release := cleanup.recover
			cleanup.recover = func(owner context.Context, balance, request server.Id) error {
				if attempts.Add(1) == 1 {
					// the request's own pass meets a pool timeout
					return context.DeadlineExceeded
				}
				return release(owner, balance, request)
			}
			cleanup.now = clock.Now
			cleanup.after = clock.AfterGate(retry, nil)
			cleanup.deferred = deferred
			return cleanup
		}
		request := withRedisContractAdmission(context.WithValue(ctx, redisReservationCleanupContextKey{}, observe))
		owner := redisAdmissionFromContext(request)
		refusal := errors.New("synthetic refusal before publication")
		_, err := runRedisContractAdmission(request, func(attempt context.Context) (*TransferEscrow, error) {
			_, finish, err := reserveRedisTransferEscrowBeforeTx(attempt, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
			if err != nil {
				return nil, err
			}
			finish()
			return nil, refusal
		})
		if !errors.Is(err, refusal) {
			t.Fatal("the refusal fixture changed", err)
		}
		// The request's own page left the deferred token pending.
		redisPendingRequireMarkers(t, ctx, f.balanceId, owner.contractId, true, false)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("the deferred token was released or lost", got)
		}

		// Another attempt of the same request takes the pending token over
		// and publishes it before the deferred owner retries.
		reserved, finish := reserveRedisAdmissionTestBeforeTx(t, request, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
		var published *TransferEscrow
		var publishErr error
		server.Tx(ctx, func(tx server.PgTx) {
			published, _, publishErr = createTransferEscrowInTx(reserved, tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23, nil)
		}, server.TxReadCommitted, server.OptNoRetry())
		finish()
		if publishErr != nil || published == nil || published.ContractId != owner.contractId {
			t.Fatal("the publication fixture failed", publishErr)
		}

		completedBefore := redisCompensationTestCount("completed")
		close(retry)
		if err := deferred.drain(ctx); err != nil {
			t.Fatal("the deferred owner did not end the compensation", err)
		}
		if attempts.Load() != 2 || redisCompensationTestCount("completed")-completedBefore != 1 {
			t.Fatal("the deferred retry did not complete through custody", attempts.Load())
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("the deferred compensation released a token its request published", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, owner.contractId, false)
		server.Redis(ctx, func(client server.RedisClient) {
			tokens, err := client.HGetAll(ctx, redisContractReservationKeys(f.balanceId)[1]).Result()
			server.Raise(err)
			if len(tokens) != 2 || tokens[neighbor.ContractId.String()] != "17" || tokens[owner.contractId.String()] != "23" {
				t.Fatal("the deferred compensation changed a published token", tokens)
			}
		})
	})
}

// A quiesce, as process exit, ends a held compensation and leaves its token
// reserved and pending. Recovery takes it once it ages, as for a request that
// died.
func TestRedisDeferredCompensationQuiesceLeavesTokensToAgedRecovery(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		_ = createRedisAdmissionTest(ctx, f, 17)
		deferred := newRedisDeferredCompensationsForTest(t, 1, 1)
		holding := make(chan struct{}, 1)
		clock := &redisCompensationTestClock{now: time.Unix(1, 0)}
		observe := func(cleanup redisReservationCleanup) redisReservationCleanup {
			cleanup.recover = func(context.Context, server.Id, server.Id) error {
				return context.DeadlineExceeded
			}
			// its retry never falls due
			cleanup.now, cleanup.after = clock.Now, clock.AfterGate(nil, holding)
			cleanup.deferred = deferred
			return cleanup
		}
		request := withRedisContractAdmission(context.WithValue(ctx, redisReservationCleanupContextKey{}, observe))
		owner := redisAdmissionFromContext(request)
		refusal := errors.New("synthetic refusal before publication")
		_, err := runRedisContractAdmission(request, func(attempt context.Context) (*TransferEscrow, error) {
			_, finish, err := reserveRedisTransferEscrowBeforeTx(attempt, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 23)
			if err != nil {
				return nil, err
			}
			finish()
			return nil, refusal
		})
		if !errors.Is(err, refusal) {
			t.Fatal("the refusal fixture changed", err)
		}
		select {
		case <-holding:
		case <-ctx.Done():
			t.Fatal("the deferred owner did not take the compensation", ctx.Err())
		}
		pendingBefore := redisCompensationTestCount("pending")
		quiesced, stop := context.WithTimeout(ctx, redisDeferredCompensationQuiesceTimeout)
		defer stop()
		if err := deferred.quiesce(quiesced); err != nil {
			t.Fatal("the quiesce did not end the held compensation", err)
		}
		if pending := redisCompensationTestCount("pending") - pendingBefore; pending != 1 {
			t.Fatal("the quiesced compensation was not left pending to recovery", pending)
		}
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 40 {
			t.Fatal("the quiesce released or lost the token", got)
		}
		redisPendingRequireMarkers(t, ctx, f.balanceId, owner.contractId, true, false)

		redisRecoveryAgePendingMarker(t, ctx, f.balanceId, owner.contractId)
		_ = createRedisAdmissionTest(ctx, f, 29)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 46 {
			t.Fatal("recovery did not take the aged token the quiesce left", got)
		}
		redisRecoveryRequireMarker(t, ctx, f.balanceId, owner.contractId, false)
		server.Redis(ctx, func(client server.RedisClient) {
			_, err := client.HGet(ctx, redisContractReservationKeys(f.balanceId)[1], owner.contractId.String()).Result()
			if !errors.Is(err, redis.Nil) {
				t.Fatal("recovery left the quiesced token", err)
			}
		})
	})
}
