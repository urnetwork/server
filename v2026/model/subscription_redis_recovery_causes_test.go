// Cleanup retains exact pending obligations when error inspection cannot finish.
package model

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Stable diagnostics leave graph traversal exclusively to production code.
type redisCauseTestOne struct{ cause error }

func (self *redisCauseTestOne) Error() string { return "synthetic Redis cause" }
func (self *redisCauseTestOne) Unwrap() error { return self.cause }

// Nil children remain explicit instead of being removed by errors.Join.
type redisCauseTestMany struct{ causes []error }

func (self *redisCauseTestMany) Error() string   { return "synthetic Redis joined cause" }
func (self *redisCauseTestMany) Unwrap() []error { return self.causes }

// Real cleanup continues a healthy peer while retaining the malformed request.
func TestRedisCleanupBoundsMalformedCausesAndRetainsPendingPeer(t *testing.T) {
	cycle := &redisCauseTestOne{}
	cycle.cause = cycle
	var deep error = context.DeadlineExceeded
	for range 40 {
		deep = &redisCauseTestOne{cause: deep}
	}
	wide := make([]error, 256)
	for index := range wide {
		wide[index] = context.DeadlineExceeded
	}
	for index, cause := range []error{cycle, deep, &redisCauseTestMany{causes: wide}, &redisCauseTestMany{},
		&redisCauseTestMany{causes: []error{nil, nil}}, &redisCauseTestOne{}} {
		first, second, request := server.NewId(), server.NewId(), server.NewId()
		firstCalls, secondCalls, waits := 0, 0, 0
		cleanup := redisReservationCleanup{now: time.Now,
			recover: func(_ context.Context, id, actualRequest server.Id) error {
				if actualRequest != request {
					t.Fatal("cleanup changed original request")
				}
				if id == first {
					firstCalls++
					return cause
				}
				if id != second {
					t.Fatal("cleanup acquired another balance")
				}
				secondCalls++
				return nil
			},
			wait: func(context.Context, time.Duration) error { waits++; return context.Canceled },
		}
		err := cleanup.run(t.Context(), request, []server.Id{first, second})
		pending, retained := false, false
		for _, node := range server.InspectErrorCauses(err).Nodes {
			pending = pending || node.Err == errRedisReservationCleanupPending
			retained = retained || node.Err == cause
		}
		if !pending || !retained || firstCalls != 1 || secondCalls != 1 || waits != 0 {
			t.Fatal("incomplete cleanup cause retried, released obligation, or blocked healthy peer", index, firstCalls, secondCalls, waits)
		}
	}
}

// Compensation that cannot reach Redis gives up after the short cleanup budget
// instead of holding its failed CreateContract caller for minutes. On
// 2026-10-26 a 300 s budget kept about 170k failed proxy calls retrying
// release-pending every second, and their pool waits failed the next creations.
func TestRedisCleanupBudgetBoundsUnreachableRelease(t *testing.T) {
	request, balance := server.NewId(), server.NewId()
	now := time.Unix(1, 0)
	calls, waits := 0, 0
	cleanup := redisReservationCleanup{now: func() time.Time { return now },
		recover: func(context.Context, server.Id, server.Id) error {
			calls++
			return context.DeadlineExceeded
		},
		wait: func(_ context.Context, delay time.Duration) error {
			waits++
			now = now.Add(delay)
			return nil
		},
	}
	err := cleanup.run(t.Context(), request, []server.Id{balance})
	if !errors.Is(err, errRedisReservationCleanupPending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unreachable release was not reported as pending compensation", err)
	}
	if 10*time.Second < redisReservationCleanupBudget || 10 < calls || calls < 2 {
		t.Fatal("cleanup retried outside a short bounded budget", redisReservationCleanupBudget, calls, waits)
	}
}

// A canceled original request does not cancel the independent cleanup owner.
// Its transient retries back off within the short budget: each wait falls in
// the upper half of a nominal delay that doubles from one second to the
// thirty-second cap.
func TestRedisCleanupPreservesIndependentTransientCancellationPolicy(t *testing.T) {
	caller, cancel := context.WithCancel(t.Context())
	cancel()
	request, balance := server.NewId(), server.NewId()
	now := time.Unix(1, 0)
	calls := 0
	var delays []time.Duration
	cleanup := redisReservationCleanup{now: func() time.Time { return now },
		recover: func(owner context.Context, id, actualRequest server.Id) error {
			if owner.Err() != nil || id != balance || actualRequest != request {
				t.Fatal("cleanup owner or exact obligation changed")
			}
			calls++
			// two backoffs take under three seconds of the budget
			if calls <= 2 {
				return errors.Join(context.Canceled, context.DeadlineExceeded)
			}
			return nil
		},
		wait: func(owner context.Context, delay time.Duration) error {
			if owner.Err() != nil {
				t.Fatal("cleanup lost its finite independent owner")
			}
			delays = append(delays, delay)
			now = now.Add(delay)
			return nil
		},
	}
	if err := cleanup.run(caller, request, []server.Id{balance}); err != nil || calls != 3 || len(delays) != 2 {
		t.Fatal("typed transient cleanup failed within original budget", calls, len(delays), err)
	}
	for retry, delay := range delays {
		nominal := min(time.Second<<retry, 30*time.Second)
		if delay < nominal/2 || nominal <= delay {
			t.Fatalf("cleanup retry %d waited %s; backoff waits within [%s, %s)", retry, delay, nominal/2, nominal)
		}
	}
	if redisReservationRecoveryRetryable(errors.Join(context.Canceled, errRedisReservationRecoveryIdentity)) {
		t.Fatal("cleanup cancellation overrode observed token identity refusal")
	}
}

// A transport that never recovers costs one request at most the passes that
// backoff fits in its budget, not one per second, and every pass is the same
// exact obligation.
func TestRedisCleanupBackoffBoundsPassesWithinBudget(t *testing.T) {
	for _, extreme := range []float64{0, math.Nextafter(1, 0)} {
		for retry := range 24 {
			nominal := min(time.Second<<retry, 30*time.Second)
			if delay := redisReservationCleanupRetryDelay(retry, extreme); delay < nominal/2 || nominal <= delay {
				t.Fatalf("retry %d with jitter %v waits %s; backoff waits within [%s, %s)", retry, extreme, delay, nominal/2, nominal)
			}
		}
	}
	request, balance := server.NewId(), server.NewId()
	start := time.Unix(1, 0)
	now := start
	calls := 0
	cleanup := redisReservationCleanup{now: func() time.Time { return now },
		recover: func(_ context.Context, id, actualRequest server.Id) error {
			if id != balance || actualRequest != request {
				t.Fatal("cleanup changed its exact obligation")
			}
			calls++
			return context.DeadlineExceeded
		},
		wait: func(_ context.Context, delay time.Duration) error {
			if delay > redisReservationCleanupRetryCap {
				t.Fatal("cleanup waited beyond the backoff cap", delay)
			}
			now = now.Add(delay)
			return nil
		},
	}
	err := cleanup.run(t.Context(), request, []server.Id{balance})
	if !errors.Is(err, errRedisReservationCleanupPending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("exhausted cleanup was reported complete", err)
	}
	if maxPasses := redisCompensationTestMaxPasses(); calls < 2 || maxPasses < calls {
		t.Fatalf("cleanup made %d passes in its budget; backoff allows at most %d", calls, maxPasses)
	}
	if elapsed := now.Sub(start); elapsed != redisReservationCleanupBudget {
		t.Fatal("cleanup did not retry through exactly its one budget", elapsed)
	}
}
