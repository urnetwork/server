// The per-client /connect/control limit (connect_control_limit.go): a client
// is admitted its burst plus one at once, then its rate; a refusal names the
// wait until its next admission and spends nothing; one client never spends
// another's budget (support inbox 8093, 8077).
package model

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A fixed clock, in microseconds since the epoch, for the decisions below.
const connectControlLimitTestNow = int64(1_800_000_000_000_000)

func connectControlLimitTestAdmit(ctx context.Context, t testing.TB, clientId server.Id, settings ConnectControlLimitSettings, nowMicros int64) ConnectControlLimitResult {
	t.Helper()
	result, err := AdmitConnectControlAt(ctx, clientId, settings, nowMicros)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Admits n requests at nowMicros and returns how many were allowed and the
// last refusal's wait.
func connectControlLimitTestAdmitN(ctx context.Context, t testing.TB, clientId server.Id, settings ConnectControlLimitSettings, nowMicros int64, n int) (allowed int, retryAfter time.Duration) {
	t.Helper()
	for range n {
		result := connectControlLimitTestAdmit(ctx, t, clientId, settings, nowMicros)
		if result.Allowed {
			if result.RetryAfter != 0 {
				t.Fatalf("an allowed request has a wait %s", result.RetryAfter)
			}
			allowed += 1
		} else {
			retryAfter = result.RetryAfter
		}
	}
	return
}

// The defaults validate and work out to the tuned limit: 6,000 a minute is
// one request per 10 ms, and a burst of 3,000 is 30 s of it.
func TestConnectControlLimitDefaults(t *testing.T) {
	settings := DefaultConnectControlLimitSettings()
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	if settings.RequestsPerMinute != 6000 || settings.Burst != 3000 {
		t.Fatalf("defaults = %+v", settings)
	}
	if interval := settings.emissionIntervalMicros(); interval != 10_000 {
		t.Fatalf("interval = %dus, want 10000", interval)
	}
	if tolerance := settings.toleranceMicros(); tolerance != 30_000_000 {
		t.Fatalf("tolerance = %dus, want 30 s", tolerance)
	}
}

// A profile sets both fields and nothing else, within bounds.
func TestParseConnectControlLimitSettings(t *testing.T) {
	settings, err := ParseConnectControlLimitSettings([]byte("requests_per_minute: 9000\nburst: 4500\n"))
	if err != nil || settings != (ConnectControlLimitSettings{RequestsPerMinute: 9000, Burst: 4500}) {
		t.Fatalf("settings = %+v, %v", settings, err)
	}
	if settings, err := ParseConnectControlLimitSettings([]byte("requests_per_minute: 60\nburst: 0\n")); err != nil || settings.Burst != 0 {
		t.Fatalf("a zero burst = %+v, %v", settings, err)
	}
	for _, profile := range []string{
		"",
		"burst: 10\n",
		"requests_per_minute: 60\n",
		"requests_per_minute: 0\nburst: 10\n",
		"requests_per_minute: 60\nburst: -1\n",
		"requests_per_minute: 6000001\nburst: 10\n",
		"requests_per_minute: 60\nburst: 1000001\n",
		"requests_per_minute: 60.5\nburst: 10\n",
		"requests_per_minute: 60\nburst: 10\nenabled: false\n",
		"requests_per_minute: 60\nburst: 10\n---\nrequests_per_minute: 1\nburst: 1\n",
	} {
		if settings, err := ParseConnectControlLimitSettings([]byte(profile)); err == nil {
			t.Errorf("profile %q parsed as %+v", profile, settings)
		}
	}
}

// A refusal is a 429 that names the whole seconds until the client's next
// admission, at least one, for RaiseHttpError's Retry-After.
func TestConnectControlLimitErrorIs429WithRetryAfter(t *testing.T) {
	for _, test := range []struct {
		retryAfter time.Duration
		seconds    int
	}{
		{retryAfter: 0, seconds: 1},
		{retryAfter: time.Microsecond, seconds: 1},
		{retryAfter: time.Second, seconds: 1},
		{retryAfter: 1500 * time.Millisecond, seconds: 2},
		{retryAfter: 60 * time.Second, seconds: 60},
	} {
		err := NewConnectControlLimitError(test.retryAfter)
		if !strings.HasPrefix(err.Error(), "429 ") {
			t.Fatalf("%s: %q is not a 429", test.retryAfter, err)
		}
		var retryAfter interface{ RetryAfterSeconds() int }
		if !errors.As(err, &retryAfter) || retryAfter.RetryAfterSeconds() != test.seconds {
			t.Fatalf("%s: retry after = %v, want %d s", test.retryAfter, retryAfter, test.seconds)
		}
	}
}

// A client is admitted its burst plus one at once and then one request per
// interval. A refusal names the exact wait and spends nothing, so the client
// is admitted again one interval on; a full interval per admitted request
// later the bucket is whole again.
func TestConnectControlLimitAdmitsTheBurstThenTheRate(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		// one request a second, five at once
		settings := ConnectControlLimitSettings{RequestsPerMinute: 60, Burst: 4}
		now := connectControlLimitTestNow

		if allowed, retryAfter := connectControlLimitTestAdmitN(ctx, t, clientId, settings, now, 8); allowed != 5 || retryAfter != time.Second {
			t.Fatalf("at once: allowed %d, retry after %s; want 5 and 1s", allowed, retryAfter)
		}
		if result := connectControlLimitTestAdmit(ctx, t, clientId, settings, now+999_999); result.Allowed || result.RetryAfter != time.Microsecond {
			t.Fatalf("one microsecond early: %+v, want refused with 1us", result)
		}
		// the refusals spent nothing: one interval on, one more is admitted
		if allowed, retryAfter := connectControlLimitTestAdmitN(ctx, t, clientId, settings, now+1_000_000, 3); allowed != 1 || retryAfter != time.Second {
			t.Fatalf("one interval on: allowed %d, retry after %s; want 1 and 1s", allowed, retryAfter)
		}
		// six admitted, so six intervals after the first the bucket is whole
		if allowed, _ := connectControlLimitTestAdmitN(ctx, t, clientId, settings, now+6_000_000, 8); allowed != 5 {
			t.Fatalf("with the bucket whole again: allowed %d, want 5", allowed)
		}
	})
}

// One client's flood never spends another client's budget.
func TestConnectControlLimitIsPerClient(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		flooding, other := server.NewId(), server.NewId()
		settings := ConnectControlLimitSettings{RequestsPerMinute: 60, Burst: 9}
		now := connectControlLimitTestNow

		if allowed, _ := connectControlLimitTestAdmitN(ctx, t, flooding, settings, now, 1000); allowed != 10 {
			t.Fatalf("the flooding client was allowed %d of 1000, want 10", allowed)
		}
		if allowed, retryAfter := connectControlLimitTestAdmitN(ctx, t, other, settings, now, 11); allowed != 10 || retryAfter != time.Second {
			t.Fatalf("the other client: allowed %d, retry after %s; want its own 10 and 1s", allowed, retryAfter)
		}
	})
}

// The state is one key per client holding its arrival time exactly, kept
// until the bucket would be whole again and no longer.
func TestConnectControlLimitKeyHoldsTheExactArrivalTimeUntilTheBucketRefills(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		settings := ConnectControlLimitSettings{RequestsPerMinute: 60, Burst: 4}
		// a clock with microseconds, which the default Lua number format would
		// round away
		now := connectControlLimitTestNow + 123_457
		if allowed, _ := connectControlLimitTestAdmitN(ctx, t, clientId, settings, now, 3); allowed != 3 {
			t.Fatalf("allowed %d of 3", allowed)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			key := connectControlLimitKey(clientId)
			value, err := r.Get(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			if want := strconv.FormatInt(now+3_000_000, 10); value != want {
				t.Fatalf("arrival time = %q, want %s", value, want)
			}
			ttl, err := r.PTTL(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			// three intervals, plus the script's millisecond of margin
			if ttl <= 2*time.Second || 3*time.Second+time.Millisecond < ttl {
				t.Fatalf("ttl = %s, want just under 3s", ttl)
			}
		})
	})
}

// Without a clock of the caller's, the decision reads the Redis node's clock,
// the one every api process shares.
func TestConnectControlLimitReadsTheRedisNodeClock(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		settings := ConnectControlLimitSettings{RequestsPerMinute: 1, Burst: 0}
		first, err := AdmitConnectControl(ctx, clientId, settings)
		if err != nil || !first.Allowed {
			t.Fatalf("first = %+v, %v", first, err)
		}
		second, err := AdmitConnectControl(ctx, clientId, settings)
		if err != nil || second.Allowed {
			t.Fatalf("second = %+v, %v; want refused", second, err)
		}
		if second.RetryAfter <= 50*time.Second || 60*time.Second < second.RetryAfter {
			t.Fatalf("second waits %s, want just under the 60s interval", second.RetryAfter)
		}
	})
}

// A canceled request has no decision; the caller admits it undecided.
func TestConnectControlLimitCanceledHasNoDecision(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if result, err := AdmitConnectControlAt(ctx, server.NewId(), DefaultConnectControlLimitSettings(), connectControlLimitTestNow); err == nil {
			t.Fatalf("a canceled decision = %+v, want an error", result)
		}
	})
}
