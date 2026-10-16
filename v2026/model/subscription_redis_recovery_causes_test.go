// Cleanup retains exact pending obligations when error inspection cannot finish.
package model

import (
	"context"
	"errors"
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

// A canceled original request does not cancel the independent cleanup owner.
func TestRedisCleanupPreservesIndependentTransientCancellationPolicy(t *testing.T) {
	caller, cancel := context.WithCancel(t.Context())
	cancel()
	request, balance := server.NewId(), server.NewId()
	now := time.Unix(1, 0)
	calls, waits := 0, 0
	cleanup := redisReservationCleanup{now: func() time.Time { return now },
		recover: func(owner context.Context, id, actualRequest server.Id) error {
			if owner.Err() != nil || id != balance || actualRequest != request {
				t.Fatal("cleanup owner or exact obligation changed")
			}
			calls++
			if calls <= 65 {
				return errors.Join(context.Canceled, context.DeadlineExceeded)
			}
			return nil
		},
		wait: func(owner context.Context, delay time.Duration) error {
			waits++
			if delay != time.Second || owner.Err() != nil {
				t.Fatal("cleanup lost finite independent retry pacing")
			}
			now = now.Add(delay)
			return nil
		},
	}
	if err := cleanup.run(caller, request, []server.Id{balance}); err != nil || calls != 66 || waits != 65 {
		t.Fatal("typed transient cleanup failed within original budget", calls, waits, err)
	}
	if redisReservationRecoveryRetryable(errors.Join(context.Canceled, errRedisReservationRecoveryIdentity)) {
		t.Fatal("cleanup cancellation overrode observed token identity refusal")
	}
}
