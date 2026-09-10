package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestCircleTransferLimiterDefersWithOneStableMember(t *testing.T) {
	ctx := context.Background()
	waits := []time.Duration{125 * time.Millisecond, 75 * time.Millisecond}
	var admittedMembers []string
	var slept []time.Duration
	limiter := circleTransferLimiter{
		newMember: func() string { return "stable-member" },
		admit: func(_ context.Context, member string) (circleTransferAdmission, error) {
			admittedMembers = append(admittedMembers, member)
			if len(admittedMembers) <= len(waits) {
				return circleTransferAdmission{wait: waits[len(admittedMembers)-1]}, nil
			}
			return circleTransferAdmission{allowed: true}, nil
		},
		sleep: func(_ context.Context, delay time.Duration) error {
			slept = append(slept, delay)
			return nil
		},
	}

	waited, deferrals, err := limiter.wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if waited != 200*time.Millisecond || deferrals != 2 {
		t.Fatalf("wait result = %s/%d, want 200ms/2", waited, deferrals)
	}
	if fmt.Sprint(admittedMembers) != "[stable-member stable-member stable-member]" {
		t.Fatalf("reservation members = %v, want one stable member", admittedMembers)
	}
	if fmt.Sprint(slept) != "[125ms 75ms]" {
		t.Fatalf("sleeps = %v, want [125ms 75ms]", slept)
	}
}

func TestCircleTransferLimiterFailsClosedOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	limiter := circleTransferLimiter{
		newMember: func() string { return "canceled-member" },
		admit: func(_ context.Context, _ string) (circleTransferAdmission, error) {
			return circleTransferAdmission{wait: time.Second}, nil
		},
		sleep: func(_ context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}

	waited, deferrals, err := limiter.wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if waited != 0 || deferrals != 1 {
		t.Fatalf("canceled wait result = %s/%d, want 0/1", waited, deferrals)
	}
}

func TestCircleTransferAdmissionConvertsRedisPanicToFailClosedError(t *testing.T) {
	decision, err := admitCircleTransferWithRedis(
		context.Background(),
		"redis-panic-member",
		func(context.Context, func(server.RedisClient)) {
			panic(errors.New("synthetic Redis connection timeout"))
		},
	)
	if err == nil || !strings.Contains(err.Error(), "circle transfer admission Redis failure") ||
		!strings.Contains(err.Error(), "synthetic Redis connection timeout") {
		t.Fatalf("Redis panic error = %v, want fail-closed admission error", err)
	}
	if decision.allowed || decision.wait != 0 {
		t.Fatalf("Redis panic decision = %+v, want no admission", decision)
	}
}

// This synthetic fleet uses one Redis key and one timestamp from eight
// independent callers. The Lua script must serialize them atomically, admit
// only three, keep a replay idempotent, and reopen capacity only after the
// rolling second has elapsed.
func TestCircleTransferAdmissionScriptEnforcesFleetRollingWindow(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := fmt.Sprintf("{circle_transfer_admission_test}:%s", server.NewId())
		const nowMillis int64 = 1_788_230_000_000

		server.Redis(ctx, func(client server.RedisClient) {
			defer client.Del(ctx, key)

			type result struct {
				member   string
				decision circleTransferAdmission
				err      error
			}
			results := make(chan result, 8)
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				member := fmt.Sprintf("taskworker-%d", i)
				wg.Add(1)
				go func() {
					defer wg.Done()
					decision, err := redisCircleTransferAdmission(
						ctx,
						client,
						key,
						member,
						nowMillis,
					)
					results <- result{member: member, decision: decision, err: err}
				}()
			}
			wg.Wait()
			close(results)

			allowed := 0
			denied := 0
			var admittedMember string
			for result := range results {
				if result.err != nil {
					t.Fatal(result.err)
				}
				if result.decision.allowed {
					allowed++
					admittedMember = result.member
				} else {
					denied++
					if result.decision.wait != time.Second {
						t.Fatalf("denied wait = %s, want 1s", result.decision.wait)
					}
				}
			}
			if allowed != int(circleTransferAdmissionLimit) || denied != 8-int(circleTransferAdmissionLimit) {
				t.Fatalf("fleet decisions = allowed:%d denied:%d, want 3/5", allowed, denied)
			}

			replay, err := redisCircleTransferAdmission(
				ctx,
				client,
				key,
				admittedMember,
				nowMillis,
			)
			if err != nil || !replay.allowed || replay.wait != 0 {
				t.Fatalf("admitted command replay = %+v, %v; want idempotent admission", replay, err)
			}
			if count, err := client.ZCard(ctx, key).Result(); err != nil || count != circleTransferAdmissionLimit {
				t.Fatalf("rolling set after replay = %d, %v; want %d", count, err, circleTransferAdmissionLimit)
			}

			beforeExpiry, err := redisCircleTransferAdmission(
				ctx,
				client,
				key,
				"next-before-expiry",
				nowMillis+circleTransferAdmissionWindow.Milliseconds()-1,
			)
			if err != nil || beforeExpiry.allowed || beforeExpiry.wait != time.Millisecond {
				t.Fatalf("decision before expiry = %+v, %v; want denied for 1ms", beforeExpiry, err)
			}

			afterExpiry, err := redisCircleTransferAdmission(
				ctx,
				client,
				key,
				"next-after-expiry",
				nowMillis+circleTransferAdmissionWindow.Milliseconds(),
			)
			if err != nil || !afterExpiry.allowed || afterExpiry.wait != 0 {
				t.Fatalf("decision after expiry = %+v, %v; want admission", afterExpiry, err)
			}
		})
	})
}
