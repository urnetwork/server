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
	const admissionSecond int64 = 1_788_230_000
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
			return circleTransferAdmission{allowed: true, admissionSecond: admissionSecond}, nil
		},
		sleep: func(_ context.Context, delay time.Duration) error {
			slept = append(slept, delay)
			return nil
		},
	}

	waited, deferrals, admittedAt, err := limiter.wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if waited != 200*time.Millisecond || deferrals != 2 || admittedAt != admissionSecond {
		t.Fatalf("wait result = %s/%d/%d, want 200ms/2/%d", waited, deferrals, admittedAt, admissionSecond)
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

	waited, deferrals, admittedAt, err := limiter.wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if waited != 0 || deferrals != 1 || admittedAt != 0 {
		t.Fatalf("canceled wait result = %s/%d/%d, want 0/1/0", waited, deferrals, admittedAt)
	}
}

func TestCircleTransferAdmissionRecordsBeforeCallerContinues(t *testing.T) {
	const admissionSecond int64 = 1_788_230_000
	sequence := []string{}
	limiter := circleTransferLimiter{
		newMember: func() string { return "synthetic-member" },
		admit: func(context.Context, string) (circleTransferAdmission, error) {
			sequence = append(sequence, "redis-admitted")
			return circleTransferAdmission{allowed: true, admissionSecond: admissionSecond}, nil
		},
		sleep: func(context.Context, time.Duration) error {
			t.Fatal("immediate admission unexpectedly slept")
			return nil
		},
	}

	err := waitForCircleTransferAdmissionWith(
		context.Background(),
		limiter,
		func(waited time.Duration, deferrals int, admittedAt int64) {
			if waited != 0 || deferrals != 0 || admittedAt != admissionSecond {
				t.Fatalf("admission observation = %s/%d/%d, want 0/0/%d", waited, deferrals, admittedAt, admissionSecond)
			}
			sequence = append(sequence, "admission-observed")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// This is the next operation in CreateTransferTransaction after the helper
	// returns. The observation must already exist before a processor POST can
	// begin.
	sequence = append(sequence, "processor-post")
	if fmt.Sprint(sequence) != "[redis-admitted admission-observed processor-post]" {
		t.Fatalf("admission sequence = %v", sequence)
	}
}

func TestCircleTransferAdmissionDoesNotRecordRejectedGate(t *testing.T) {
	limiter := circleTransferLimiter{
		newMember: func() string { return "synthetic-member" },
		admit: func(context.Context, string) (circleTransferAdmission, error) {
			return circleTransferAdmission{}, errors.New("synthetic gate unavailable")
		},
		sleep: func(context.Context, time.Duration) error {
			t.Fatal("failed admission unexpectedly slept")
			return nil
		},
	}
	recorded := false
	err := waitForCircleTransferAdmissionWith(
		context.Background(),
		limiter,
		func(time.Duration, int, int64) { recorded = true },
	)
	if err == nil || !strings.Contains(err.Error(), "synthetic gate unavailable") {
		t.Fatalf("failed admission error = %v", err)
	}
	if recorded {
		t.Fatal("failed Redis gate emitted an admitted marker")
	}
}

func TestCircleTransferAdmissionRejectsMissingRedisSecond(t *testing.T) {
	limiter := circleTransferLimiter{
		newMember: func() string { return "synthetic-member" },
		admit: func(context.Context, string) (circleTransferAdmission, error) {
			return circleTransferAdmission{allowed: true}, nil
		},
		sleep: func(context.Context, time.Duration) error {
			t.Fatal("allowed admission unexpectedly slept")
			return nil
		},
	}
	recorded := false
	err := waitForCircleTransferAdmissionWith(
		context.Background(),
		limiter,
		func(time.Duration, int, int64) { recorded = true },
	)
	if err == nil || !strings.Contains(err.Error(), "invalid admission second 0") {
		t.Fatalf("missing Redis second error = %v", err)
	}
	if recorded {
		t.Fatal("admission without an authoritative Redis second emitted a marker")
	}
}

func TestCircleTransferAdmissionLogLineIsBoundedAndIdentifierFree(t *testing.T) {
	line := circleTransferAdmissionLogLine(1250*time.Millisecond, 2, 7, 1_788_230_000)
	const expected = "[circlec][transfer-admission] admitted observable=v1 redis_second=1788230000 sequence=7 deferrals=2 wait_ms=1250"
	if line != expected {
		t.Fatalf("admission line = %q, want %q", line, expected)
	}
	for _, forbidden := range []string{
		"synthetic-member",
		"payment",
		"wallet",
		"address",
		"token",
		"http",
	} {
		if strings.Contains(strings.ToLower(line), forbidden) {
			t.Fatalf("admission line contains forbidden detail %q: %q", forbidden, line)
		}
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
// only three, keep a replay idempotent while its original reservation remains
// active, and reopen capacity only after the rolling second has elapsed.
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
					if result.decision.admissionSecond != nowMillis/1000 {
						t.Fatalf("admitted Redis second = %d, want %d", result.decision.admissionSecond, nowMillis/1000)
					}
				} else {
					denied++
					if result.decision.wait != time.Second || result.decision.admissionSecond != 0 {
						t.Fatalf("denied decision = %+v, want 1s wait and no admission second", result.decision)
					}
				}
			}
			if allowed != int(circleTransferAdmissionLimit) || denied != 8-int(circleTransferAdmissionLimit) {
				t.Fatalf("fleet decisions = allowed:%d denied:%d, want 3/5", allowed, denied)
			}

			withinWindowReplay, err := redisCircleTransferAdmission(
				ctx,
				client,
				key,
				admittedMember,
				nowMillis+750,
			)
			if err != nil || !withinWindowReplay.allowed || withinWindowReplay.wait != 0 ||
				withinWindowReplay.admissionSecond != nowMillis/1000 {
				t.Fatalf(
					"within-window replay = %+v, %v; want original Redis second %d",
					withinWindowReplay,
					err,
					nowMillis/1000,
				)
			}
			if count, err := client.ZCard(ctx, key).Result(); err != nil || count != circleTransferAdmissionLimit {
				t.Fatalf("rolling set after within-window replay = %d, %v; want %d", count, err, circleTransferAdmissionLimit)
			}

			beforeExpiry, err := redisCircleTransferAdmission(
				ctx,
				client,
				key,
				"next-before-expiry",
				nowMillis+circleTransferAdmissionWindow.Milliseconds()-1,
			)
			if err != nil || beforeExpiry.allowed || beforeExpiry.wait != time.Millisecond || beforeExpiry.admissionSecond != 0 {
				t.Fatalf("decision before expiry = %+v, %v; want denied for 1ms", beforeExpiry, err)
			}

			afterExpiry, err := redisCircleTransferAdmission(
				ctx,
				client,
				key,
				"next-after-expiry",
				nowMillis+circleTransferAdmissionWindow.Milliseconds(),
			)
			if err != nil || !afterExpiry.allowed || afterExpiry.wait != 0 || afterExpiry.admissionSecond != nowMillis/1000+1 {
				t.Fatalf("decision after expiry = %+v, %v; want admission", afterExpiry, err)
			}
		})
	})
}

// A Redis client may retry the atomic command after the server applied the
// first decision but its response was lost. If that retry arrives only after
// the original reservation is cut off, the old slot no longer accounts for
// the current window: the same member must acquire one fresh slot and bucket.
// The admission observer still runs exactly once, synchronously before the
// single caller return that permits the processor POST.
func TestCircleTransferAdmissionPostCutoffLostResponseGetsFreshSlotBeforeCallerReturn(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := fmt.Sprintf("{circle_transfer_admission_post_cutoff_test}:%s", server.NewId())
		const firstMillis int64 = 1_788_230_000_000
		const member = "stable-synthetic-member"

		server.Redis(ctx, func(client server.RedisClient) {
			defer client.Del(ctx, key)
			sequence := []string{}
			limiter := circleTransferLimiter{
				newMember: func() string { return member },
				admit: func(ctx context.Context, gotMember string) (circleTransferAdmission, error) {
					if gotMember != member {
						return circleTransferAdmission{}, fmt.Errorf("member = %q, want stable synthetic member", gotMember)
					}
					first, err := redisCircleTransferAdmission(ctx, client, key, member, firstMillis)
					if err != nil {
						return circleTransferAdmission{}, err
					}
					if !first.allowed || first.admissionSecond != firstMillis/1000 {
						return circleTransferAdmission{}, fmt.Errorf("first decision = %+v, want initial admission", first)
					}
					sequence = append(sequence, "first-applied", "first-response-lost")
					sequence = append(sequence, "post-cutoff-retry")
					return redisCircleTransferAdmission(
						ctx,
						client,
						key,
						member,
						firstMillis+circleTransferAdmissionWindow.Milliseconds(),
					)
				},
				sleep: func(context.Context, time.Duration) error {
					t.Fatal("post-cutoff retry unexpectedly deferred")
					return nil
				},
			}

			observations := 0
			err := waitForCircleTransferAdmissionWith(
				ctx,
				limiter,
				func(waited time.Duration, deferrals int, admissionSecond int64) {
					observations++
					if waited != 0 || deferrals != 0 || admissionSecond != firstMillis/1000+1 {
						t.Fatalf(
							"post-cutoff observation = %s/%d/%d, want 0/0/%d",
							waited,
							deferrals,
							admissionSecond,
							firstMillis/1000+1,
						)
					}
					sequence = append(sequence, "fresh-admission-observed")
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			sequence = append(sequence, "caller-returned")
			if observations != 1 {
				t.Fatalf("admission observations = %d, want 1", observations)
			}
			if fmt.Sprint(sequence) != "[first-applied first-response-lost post-cutoff-retry fresh-admission-observed caller-returned]" {
				t.Fatalf("post-cutoff sequence = %v", sequence)
			}
			if count, err := client.ZCard(ctx, key).Result(); err != nil || count != 1 {
				t.Fatalf("current rolling set after post-cutoff replay = %d, %v; want one fresh slot", count, err)
			}
			score, err := client.ZScore(ctx, key, member).Result()
			if err != nil || int64(score) != firstMillis+circleTransferAdmissionWindow.Milliseconds() {
				t.Fatalf("fresh reservation score = %.0f, %v; want %d", score, err, firstMillis+circleTransferAdmissionWindow.Milliseconds())
			}
		})
	})
}
