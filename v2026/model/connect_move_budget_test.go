// The move budget (connect_move_budget.go): concurrent movers never exceed a
// cap, a refusal names the first cap that refused and spends nothing, and the
// windows are minutes of the Redis node's clock.
package model

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Every budget key shares the `{connect_move}` hash tag, so the one script
// that checks and increments all three runs on one cluster slot.
func TestConnectMoveBudgetKeysShareOneSlot(t *testing.T) {
	for _, key := range []string{
		connectMoveBudgetFleetKey,
		connectMoveBudgetSourceKey("edge-a.example"),
		connectMoveBudgetTargetKey("edge-b.example"),
		connectMoveControlKey,
	} {
		if !strings.HasPrefix(key, "{connect_move}") {
			t.Errorf("key %q is not on the {connect_move} slot", key)
		}
	}
	if connectMoveBudgetSourceKey("edge-a.example") == connectMoveBudgetTargetKey("edge-a.example") {
		t.Error("a host's source and target counters share a key")
	}
}

// Reserves concurrently from goroutineCount movers, each its share of
// reservationCount, with source and target hosts from hostFor, and returns
// the answers counted: "" for allowed, else the refusing cap.
func connectMoveTestReserveConcurrently(
	ctx context.Context,
	t testing.TB,
	goroutineCount int,
	reservationCount int,
	hostFor func(i int) (string, string),
	caps ConnectMoveBudgetCaps,
) map[string]int {
	t.Helper()
	answerCounts := map[string]int{}
	var answerLock sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	next := make(chan int, reservationCount)
	for i := 0; i < reservationCount; i += 1 {
		next <- i
	}
	close(next)
	for g := 0; g < goroutineCount; g += 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				sourceHost, targetHost := hostFor(i)
				refusedCap, err := ReserveConnectMoveBudget(ctx, sourceHost, targetHost, caps)
				func() {
					answerLock.Lock()
					defer answerLock.Unlock()
					if err != nil {
						errs = append(errs, err)
						return
					}
					answerCounts[refusedCap] += 1
				}()
			}
		}()
	}
	wg.Wait()
	if 0 < len(errs) {
		t.Fatalf("%d reservations failed, the first: %s", len(errs), errs[0])
	}
	return answerCounts
}

// Waits, if needed, until the node's clock is clear of a minute boundary for
// the next few seconds, so a test's reservations land in one window. Returns
// the window.
func connectMoveTestSettleInWindow(ctx context.Context, t testing.TB) int64 {
	t.Helper()
	for {
		nodeTime := connectMoveTestNodeTime(ctx, t)
		secondInMinute := nodeTime.Unix() % connectMoveBudgetWindowSeconds
		if secondInMinute < connectMoveBudgetWindowSeconds-15 {
			return nodeTime.Unix() / connectMoveBudgetWindowSeconds
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Duration(connectMoveBudgetWindowSeconds-secondInMinute) * time.Second):
		}
	}
}

// The {window, count} of a counter.
func connectMoveTestBudgetCounter(ctx context.Context, t testing.TB, key string) (window int64, count int64) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := r.HMGet(ctx, key, "window", "count").Result()
		if err != nil {
			t.Fatal(err)
		}
		parse := func(value any) int64 {
			text, ok := value.(string)
			if !ok {
				return -1
			}
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return -1
			}
			return n
		}
		window, count = parse(values[0]), parse(values[1])
	})
	return
}

// Forty movers reserving at once across many hosts: exactly the fleet cap is
// allowed, and every other answer names the fleet cap.
func TestConnectMoveBudgetFleetCapUnderConcurrency(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		connectMoveTestSettleInWindow(ctx, t)
		answerCounts := connectMoveTestReserveConcurrently(ctx, t, 40, 200, func(i int) (string, string) {
			return fmt.Sprintf("edge-s%d.example", i%7), fmt.Sprintf("edge-t%d.example", i%11)
		}, ConnectMoveBudgetCaps{Fleet: 37, Source: 1000, Target: 1000})
		if answerCounts[""] != 37 || answerCounts[ConnectMoveBudgetCapFleet] != 200-37 || len(answerCounts) != 2 {
			t.Fatalf("answers = %v, want 37 allowed and the rest refused by the fleet cap", answerCounts)
		}
		_, fleetCount := connectMoveTestBudgetCounter(ctx, t, connectMoveBudgetFleetKey)
		if fleetCount != 37 {
			t.Fatalf("fleet counter = %d", fleetCount)
		}
	})
}

// One source host, many targets: exactly the source cap is allowed for it,
// and another source still has its own.
func TestConnectMoveBudgetSourceCapUnderConcurrency(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		connectMoveTestSettleInWindow(ctx, t)
		caps := ConnectMoveBudgetCaps{Fleet: 1000, Source: 13, Target: 1000}
		answerCounts := connectMoveTestReserveConcurrently(ctx, t, 40, 100, func(i int) (string, string) {
			return "edge-hot.example", fmt.Sprintf("edge-t%d.example", i%9)
		}, caps)
		if answerCounts[""] != 13 || answerCounts[ConnectMoveBudgetCapSource] != 100-13 || len(answerCounts) != 2 {
			t.Fatalf("answers = %v, want 13 allowed and the rest refused by the source cap", answerCounts)
		}
		refusedCap, err := ReserveConnectMoveBudget(ctx, "edge-other.example", "edge-t0.example", caps)
		if err != nil || refusedCap != "" {
			t.Fatalf("another source was refused %q (%v)", refusedCap, err)
		}
	})
}

// Many sources, one target: exactly the target cap arrives there, which
// protects a new or empty host from a herd (MOVE.md §6).
func TestConnectMoveBudgetTargetCapUnderConcurrency(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		connectMoveTestSettleInWindow(ctx, t)
		caps := ConnectMoveBudgetCaps{Fleet: 1000, Source: 1000, Target: 11}
		answerCounts := connectMoveTestReserveConcurrently(ctx, t, 40, 100, func(i int) (string, string) {
			return fmt.Sprintf("edge-s%d.example", i%9), "edge-new.example"
		}, caps)
		if answerCounts[""] != 11 || answerCounts[ConnectMoveBudgetCapTarget] != 100-11 || len(answerCounts) != 2 {
			t.Fatalf("answers = %v, want 11 allowed and the rest refused by the target cap", answerCounts)
		}
		_, targetCount := connectMoveTestBudgetCounter(ctx, t, connectMoveBudgetTargetKey("edge-new.example"))
		_, fleetCount := connectMoveTestBudgetCounter(ctx, t, connectMoveBudgetFleetKey)
		if targetCount != 11 || fleetCount != 11 {
			t.Fatalf("a refusal spent budget: target %d, fleet %d", targetCount, fleetCount)
		}
	})
}

// A refusal names the first cap in the order fleet, source, target, and
// spends no counter. A non-positive cap allows none.
func TestConnectMoveBudgetRefusalOrderAndZeroCaps(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		connectMoveTestSettleInWindow(ctx, t)

		for _, c := range []struct {
			caps       ConnectMoveBudgetCaps
			refusedCap string
		}{
			{caps: ConnectMoveBudgetCaps{Fleet: 0, Source: 0, Target: 0}, refusedCap: ConnectMoveBudgetCapFleet},
			{caps: ConnectMoveBudgetCaps{Fleet: -1, Source: 10, Target: 10}, refusedCap: ConnectMoveBudgetCapFleet},
			{caps: ConnectMoveBudgetCaps{Fleet: 10, Source: 0, Target: 0}, refusedCap: ConnectMoveBudgetCapSource},
			{caps: ConnectMoveBudgetCaps{Fleet: 10, Source: 10, Target: 0}, refusedCap: ConnectMoveBudgetCapTarget},
			{caps: ConnectMoveBudgetCaps{Fleet: 10, Source: 10, Target: -3}, refusedCap: ConnectMoveBudgetCapTarget},
		} {
			refusedCap, err := ReserveConnectMoveBudget(ctx, "edge-a.example", "edge-b.example", c.caps)
			if err != nil {
				t.Fatal(err)
			}
			if refusedCap != c.refusedCap {
				t.Errorf("caps %+v refused by %q, want %q", c.caps, refusedCap, c.refusedCap)
			}
		}
		for _, key := range []string{
			connectMoveBudgetFleetKey,
			connectMoveBudgetSourceKey("edge-a.example"),
			connectMoveBudgetTargetKey("edge-b.example"),
		} {
			if _, count := connectMoveTestBudgetCounter(ctx, t, key); count != -1 {
				t.Errorf("a refused reservation wrote %s: count %d", key, count)
			}
		}

		if _, err := ReserveConnectMoveBudget(ctx, "", "edge-b.example", ConnectMoveBudgetCaps{Fleet: 1, Source: 1, Target: 1}); err == nil {
			t.Error("reserved without a source host")
		}
	})
}

// A counter left from an earlier window counts zero: the first reservation of
// the current window, as the Redis node's clock tells it, is allowed and
// restarts the counter at one in that window. Counters expire.
func TestConnectMoveBudgetWindowRollsOverOnTheNodeClock(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		window := connectMoveTestSettleInWindow(ctx, t)
		caps := ConnectMoveBudgetCaps{Fleet: 2, Source: 2, Target: 2}

		// every counter spent in the previous window
		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{
				connectMoveBudgetFleetKey,
				connectMoveBudgetSourceKey("edge-a.example"),
				connectMoveBudgetTargetKey("edge-b.example"),
			} {
				if err := r.HSet(ctx, key, "window", window-1, "count", 2).Err(); err != nil {
					t.Fatal(err)
				}
			}
		})
		refusedCap, err := ReserveConnectMoveBudget(ctx, "edge-a.example", "edge-b.example", caps)
		if err != nil || refusedCap != "" {
			t.Fatalf("the new window was refused %q (%v)", refusedCap, err)
		}
		for _, key := range []string{
			connectMoveBudgetFleetKey,
			connectMoveBudgetSourceKey("edge-a.example"),
			connectMoveBudgetTargetKey("edge-b.example"),
		} {
			counterWindow, count := connectMoveTestBudgetCounter(ctx, t, key)
			if counterWindow != window || count != 1 {
				t.Errorf("%s = {window %d, count %d}, want {window %d, count 1}", key, counterWindow, count, window)
			}
			server.Redis(ctx, func(r server.RedisClient) {
				ttl, err := r.TTL(ctx, key).Result()
				if err != nil {
					t.Fatal(err)
				}
				if ttl <= 0 || time.Duration(connectMoveBudgetCounterTtlSeconds)*time.Second < ttl {
					t.Errorf("%s ttl = %s", key, ttl)
				}
			})
		}

		// within the window the cap holds
		refusedCap, err = ReserveConnectMoveBudget(ctx, "edge-a.example", "edge-b.example", caps)
		if err != nil || refusedCap != "" {
			t.Fatalf("the second reservation was refused %q (%v)", refusedCap, err)
		}
		refusedCap, err = ReserveConnectMoveBudget(ctx, "edge-a.example", "edge-b.example", caps)
		if err != nil || refusedCap != ConnectMoveBudgetCapFleet {
			t.Fatalf("the third reservation answered %q (%v)", refusedCap, err)
		}

		// a counter from a window ahead of the node's clock (a clock that
		// stepped back) counts zero too, and is restarted
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.HSet(ctx, connectMoveBudgetFleetKey, "window", window+5, "count", 2).Err(); err != nil {
				t.Fatal(err)
			}
		})
		refusedCap, err = ReserveConnectMoveBudget(ctx, "edge-c.example", "edge-d.example", caps)
		if err != nil || refusedCap != "" {
			t.Fatalf("a counter of another window was not reset: %q (%v)", refusedCap, err)
		}
	})
}
