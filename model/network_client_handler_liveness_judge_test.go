// The request-path judgement of the stale handlers
// (liveStaleNetworkClientHandlers) on a synthetic clock, and a judgement's
// lookahead against the database (network_client_handler_liveness.go).
package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A synthetic monotonic clock for the cache.
type liveStaleTestClock struct {
	now time.Time
}

// The clock's time, as the cache reads it.
func (self *liveStaleTestClock) Now() time.Time {
	return self.now
}

// Moves the clock forward by d.
func (self *liveStaleTestClock) Advance(d time.Duration) {
	self.now = self.now.Add(d)
}

// A judge that answers from a script, one entry per judgement, and counts them.
type liveStaleTestJudge struct {
	results    []liveStaleTestJudgement
	judgeCount int
}

// One scripted judgement: the handlers judged live, or the failure.
type liveStaleTestJudgement struct {
	handlerIds []server.Id
	err        error
}

// The next scripted judgement, as the cache's judge.
func (self *liveStaleTestJudge) Judge(ctx context.Context) ([]server.Id, error) {
	result := self.results[self.judgeCount]
	self.judgeCount += 1
	return result.handlerIds, result.err
}

// A cache over a scripted judge and a synthetic clock.
func liveStaleTestCache(judge *liveStaleTestJudge) (*liveStaleNetworkClientHandlers, *liveStaleTestClock) {
	clock := &liveStaleTestClock{
		now: time.Unix(1800000000, 0),
	}
	return &liveStaleNetworkClientHandlers{
		judge: judge.Judge,
		now:   clock.Now,
	}, clock
}

// One judgement serves every read for its max age, and the first read after
// it judges again.
func TestLiveStaleHandlersReuseOneJudgementForItsMaxAge(t *testing.T) {
	firstHandlerIds := []server.Id{server.NewId()}
	secondHandlerIds := []server.Id{server.NewId()}
	judge := &liveStaleTestJudge{
		results: []liveStaleTestJudgement{
			{handlerIds: firstHandlerIds},
			{handlerIds: secondHandlerIds},
		},
	}
	cache, clock := liveStaleTestCache(judge)
	ctx := context.Background()

	if handlerIds := cache.get(ctx); !slices.Equal(handlerIds, firstHandlerIds) || judge.judgeCount != 1 {
		t.Fatalf("first read: %v after %d judgements", handlerIds, judge.judgeCount)
	}
	clock.Advance(liveStaleNetworkClientHandlersMaxAge - time.Nanosecond)
	if handlerIds := cache.get(ctx); !slices.Equal(handlerIds, firstHandlerIds) || judge.judgeCount != 1 {
		t.Fatalf("read within the max age: %v after %d judgements", handlerIds, judge.judgeCount)
	}
	clock.Advance(time.Nanosecond)
	if handlerIds := cache.get(ctx); !slices.Equal(handlerIds, secondHandlerIds) || judge.judgeCount != 2 {
		t.Fatalf("read at the max age: %v after %d judgements", handlerIds, judge.judgeCount)
	}
}

// A failed judgement keeps the previous one until it is too old, and then the
// readers take no stale handler as live; the result is never nil, so a query
// binds an empty array.
func TestLiveStaleHandlersKeepThePreviousJudgementUntilItIsTooOld(t *testing.T) {
	handlerIds := []server.Id{server.NewId()}
	failure := errors.New("the database did not answer")
	judge := &liveStaleTestJudge{
		results: []liveStaleTestJudgement{
			{handlerIds: handlerIds},
			{err: failure},
			{err: failure},
		},
	}
	cache, clock := liveStaleTestCache(judge)
	ctx := context.Background()

	cache.get(ctx)
	clock.Advance(liveStaleNetworkClientHandlersMaxStaleness)
	if kept := cache.get(ctx); !slices.Equal(kept, handlerIds) {
		t.Fatalf("a failed judgement dropped a judgement %s old: %v", liveStaleNetworkClientHandlersMaxStaleness, kept)
	}
	clock.Advance(time.Nanosecond)
	if kept := cache.get(ctx); kept == nil || len(kept) != 0 {
		t.Fatalf("a judgement older than %s still served: %v", liveStaleNetworkClientHandlersMaxStaleness, kept)
	}
	if judge.judgeCount != 3 {
		t.Fatalf("%d judgements, want one per read past the max age", judge.judgeCount)
	}
}

// A judgement of none, and a first judgement that failed, both read as an
// empty list rather than nil.
func TestLiveStaleHandlersAreNeverNil(t *testing.T) {
	ctx := context.Background()
	for _, judgement := range []liveStaleTestJudgement{
		{handlerIds: nil},
		{err: errors.New("the database did not answer")},
	} {
		cache, _ := liveStaleTestCache(&liveStaleTestJudge{
			results: []liveStaleTestJudgement{judgement},
		})
		if handlerIds := cache.get(ctx); handlerIds == nil || len(handlerIds) != 0 {
			t.Errorf("judgement %+v read as %#v", judgement, handlerIds)
		}
	}
}

// A judgement covers the handlers that go stale within its lookahead, so a
// reused judgement already holds a handler whose heartbeat stops while it is
// in use. Of two handlers about to go stale, the one whose process stamps is
// live; the other, while the writer set vouches, ended.
func TestLiveStaleHandlersJudgeHandlersAboutToGoStale(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		handlers := handlerExpiryTestHandlers(ctx, t, 2, 20102)
		stamped, unstamped := handlers[0], handlers[1]
		// fresh for a reader's cutoff now, stale within the lookahead
		heartbeatTime := server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout + liveStaleNetworkClientHandlersLookahead/2)
		handlerExpiryTestHeartbeatAt(ctx, handlers, heartbeatTime)
		handlerExpiryTestStamp(ctx, t, []handlerExpiryTestHandler{stamped})

		for name, handlerIds := range map[string][]server.Id{
			"background": readLiveStaleNetworkClientHandlerIds(ctx),
			"request": func() []server.Id {
				handlerIds, err := judgeLiveStaleNetworkClientHandlerIds(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return handlerIds
			}(),
		} {
			if !slices.Contains(handlerIds, stamped.handlerId) {
				t.Errorf("%s judgement left out a live handler about to go stale", name)
			}
			if slices.Contains(handlerIds, unstamped.handlerId) {
				t.Errorf("%s judgement kept an ended handler about to go stale", name)
			}
		}
	})
}
