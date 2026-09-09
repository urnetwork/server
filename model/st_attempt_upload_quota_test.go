// Real Redis executes the atomic quota script. The explicit trusted clock
// seam fixes bucket boundaries without sleeps, expiry polling or host skew.
package model

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

// Small, independent storage-admission capacities, not protocol trail limits.
func stAttemptUploadTestBudget() StAttemptUploadBudget {
	return StAttemptUploadBudget{RequestsPerHour: 5, BytesPerHour: 40, AccountRequestsPerHour: 3, AccountBytesPerHour: 24}
}

// No profile with absent or partial limits silently enables unbounded staging.
func TestStAttemptUploadBudgetRejectsMissingOverflowAndInvertedCapacity(t *testing.T) {
	t.Parallel()
	good := stAttemptUploadTestBudget()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"requests", "bytes", "account-requests", "account-bytes", "overflow", "inverted-count", "inverted-bytes"} {
		budget := good
		switch fault {
		case "requests":
			budget.RequestsPerHour = 0
		case "bytes":
			budget.BytesPerHour = 0
		case "account-requests":
			budget.AccountRequestsPerHour = 0
		case "account-bytes":
			budget.AccountBytesPerHour = 0
		case "overflow":
			budget.BytesPerHour = 9007199254740992
		case "inverted-count":
			budget.AccountRequestsPerHour = budget.RequestsPerHour + 1
		case "inverted-bytes":
			budget.AccountBytesPerHour = budget.BytesPerHour + 1
		}
		if err := budget.Validate(); err == nil {
			t.Fatalf("%s capacity was accepted", fault)
		}
	}
}

// Every configured counter preserves exact integer bytes before the generic
// YAML decoder can truncate a float; failed parsing cannot partially replace
// a previously valid budget. The normal renderer's decimal roundtrip remains.
func TestStAttemptUploadBudgetYAMLRejectsLossyAndUnknownNumbers(t *testing.T) {
	t.Parallel()
	good := stAttemptUploadTestBudget()
	canonical, err := yaml.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip StAttemptUploadBudget
	if err := yaml.Unmarshal(canonical, &roundtrip); err != nil || roundtrip != good || roundtrip.Validate() != nil {
		t.Fatalf("canonical budget roundtrip prerequisite: %v", err)
	}
	for _, field := range []string{"requests_per_hour: 5", "bytes_per_hour: 40", "account_requests_per_hour: 3", "account_bytes_per_hour: 24"} {
		name := strings.SplitN(field, ":", 2)[0]
		for _, scalar := range []string{"3.5", "\"3\"", "-1", "+3", "03", "0x3", "3e0", "true", "null", "18446744073709551616"} {
			raw := strings.Replace(string(canonical), field, name+": "+scalar, 1)
			if raw == string(canonical) {
				t.Fatalf("canonical field %s is missing", name)
			}
			next := good
			if err := yaml.Unmarshal([]byte(raw), &next); err == nil || next != good {
				t.Fatalf("lossy budget YAML reached typed admission: %s=%s error=%v", name, scalar, err)
			}
		}
	}
	for _, raw := range []string{string(canonical) + "unknown_capacity: 1\n", string(canonical) + "requests_per_hour: 5\n", "[1, 2, 3, 4]\n", "requests_per_hour: &count 3\nbytes_per_hour: *count\n"} {
		next := good
		if err := yaml.Unmarshal([]byte(raw), &next); err == nil || next != good {
			t.Fatalf("noncanonical budget structure reached typed admission: %v", err)
		}
	}
	var absent StAttemptUploadBudget
	if err := yaml.Unmarshal([]byte("{}"), &absent); err != nil || absent != (StAttemptUploadBudget{}) || absent.Validate() == nil {
		t.Fatalf("absent capacities became enabled: %v", err)
	}
}

// Each deployment/account gets exactly two stable same-slot keys; identifiers
// cannot inject Redis hash-tag syntax and client rotation is not an input.
func TestStAttemptUploadQuotaKeysKeepDeploymentAndAccountSeparate(t *testing.T) {
	t.Parallel()
	account, other := server.NewId(), server.NewId()
	first := stAttemptUploadQuotaKeys("945:coordinator", account)
	second := stAttemptUploadQuotaKeys("945:coordinator", other)
	replacement := stAttemptUploadQuotaKeys("945:replacement", account)
	if first[0] != second[0] || first[1] == second[1] || first[0] == replacement[0] || first[1] == replacement[1] || !reflect.DeepEqual(first, stAttemptUploadQuotaKeys("945:coordinator", account)) {
		t.Fatal("quota key ownership differs")
	}
	for _, keys := range [][]string{first, second, replacement, stAttemptUploadQuotaKeys("injected}{owner", account)} {
		end := strings.IndexByte(keys[0], '}')
		if end < 1 || keys[0][:end+1] != keys[1][:end+1] || strings.Count(keys[0], "{") != 1 || strings.Count(keys[0], "}") != 1 {
			t.Fatal("quota escaped its canonical cluster slot")
		}
	}
}

// Bad requests never acquire a Redis client, even when no service exists.
func TestStAttemptUploadQuotaRejectsInvalidAndCanceledBeforeRedis(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, call := range []func() error{
		func() error {
			return ReserveStAttemptUpload(nil, "test", server.NewId(), 1, stAttemptUploadTestBudget())
		},
		func() error {
			return ReserveStAttemptUpload(t.Context(), "", server.NewId(), 1, stAttemptUploadTestBudget())
		},
		func() error {
			return ReserveStAttemptUpload(t.Context(), "test", server.Id{}, 1, stAttemptUploadTestBudget())
		},
		func() error {
			return ReserveStAttemptUpload(t.Context(), "test", server.NewId(), 0, stAttemptUploadTestBudget())
		},
		func() error {
			return ReserveStAttemptUpload(t.Context(), "test", server.NewId(), 25, stAttemptUploadTestBudget())
		},
		func() error {
			return ReserveStAttemptUpload(ctx, "test", server.NewId(), 1, stAttemptUploadTestBudget())
		},
	} {
		if err := call(); err == nil {
			t.Fatal("invalid quota admission succeeded")
		}
	}
}

// Account refusal leaves both counters unchanged, so unrelated clients can
// use the remaining deployment allocation and may not exceed its byte cap.
func TestStAttemptUploadQuotaAtomicallyChecksAccountAndGlobalBytes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		deployment := StDeploymentKey(server.NewId().String())
		first := stAttemptUploadQuotaKeys(deployment, server.NewId())
		second := stAttemptUploadQuotaKeys(deployment, server.NewId())
		budget := stAttemptUploadTestBudget()
		server.RedisDoOnce(ctx, func(client server.RedisClient) {
			for range 3 {
				if err := reserveStAttemptUploadWithClient(ctx, client, first, 8, budget, 7200); err != nil {
					t.Fatal(err)
				}
			}
			before, err := client.HGetAll(ctx, first[0]).Result()
			if err != nil {
				t.Fatal(err)
			}
			err = reserveStAttemptUploadWithClient(ctx, client, first, 1, budget, 7200)
			var limited interface{ RetryAfterSeconds() int }
			if !errors.As(err, &limited) || limited.RetryAfterSeconds() != 3600 {
				t.Fatalf("account capacity did not refuse with exact bucket retry: %v", err)
			}
			after, err := client.HGetAll(ctx, first[0]).Result()
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("refusal spent global budget: %v", err)
			}
			for range 2 {
				if err := reserveStAttemptUploadWithClient(ctx, client, second, 8, budget, 7200); err != nil {
					t.Fatal(err)
				}
			}
			third := stAttemptUploadQuotaKeys(deployment, server.NewId())
			if err := reserveStAttemptUploadWithClient(ctx, client, third, 1, budget, 7200); !errors.As(err, &limited) {
				t.Fatalf("global capacity did not refuse: %v", err)
			}
			if exists, err := client.Exists(ctx, third[1]).Result(); err != nil || exists != 0 {
				t.Fatalf("refused account created a Redis key: %d/%v", exists, err)
			}
			actual, err := client.HGetAll(ctx, first[0]).Result()
			if err != nil || actual["requests"] != "5" || actual["bytes"] != "40" {
				t.Fatalf("exact accepted byte/count census differs: %v/%v", actual, err)
			}
		})
	})
}

// Simultaneous callers cannot each observe spare capacity before reserving it.
func TestStAttemptUploadQuotaConcurrentReservationsHaveOneAtomicWinner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		budget := StAttemptUploadBudget{RequestsPerHour: 1, BytesPerHour: 8, AccountRequestsPerHour: 1, AccountBytesPerHour: 8}
		keys := stAttemptUploadQuotaKeys(StDeploymentKey(server.NewId().String()), server.NewId())
		server.RedisDoOnce(ctx, func(client server.RedisClient) {
			ready, release := make(chan struct{}, 8), make(chan struct{})
			var joined sync.WaitGroup
			var passed, refused atomic.Int32
			failures := make(chan error, 8)
			for range 8 {
				joined.Add(1)
				go func() {
					defer joined.Done()
					ready <- struct{}{}
					<-release
					err := reserveStAttemptUploadWithClient(ctx, client, keys, 8, budget, 7200)
					var limited interface{ RetryAfterSeconds() int }
					if err == nil {
						passed.Add(1)
					} else if errors.As(err, &limited) {
						refused.Add(1)
					} else {
						failures <- err
					}
				}()
			}
			for range 8 {
				<-ready
			}
			close(release)
			joined.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			if passed.Load() != 1 || refused.Load() != 7 {
				t.Fatalf("atomic quota winners=%d refused=%d", passed.Load(), refused.Load())
			}
		})
	})
}

// Exact bucket rollover renews capacity; time reversal is a fail-closed signal.
func TestStAttemptUploadQuotaRolloverAndRollbackUseOwnedClock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		keys := stAttemptUploadQuotaKeys(StDeploymentKey(server.NewId().String()), server.NewId())
		budget := StAttemptUploadBudget{RequestsPerHour: 1, BytesPerHour: 8, AccountRequestsPerHour: 1, AccountBytesPerHour: 8}
		server.RedisDoOnce(ctx, func(client server.RedisClient) {
			if err := reserveStAttemptUploadWithClient(ctx, client, keys, 8, budget, 7199); err != nil {
				t.Fatal(err)
			}
			if err := reserveStAttemptUploadWithClient(ctx, client, keys, 8, budget, 7200); err != nil {
				t.Fatal(err)
			}
			before, err := client.HGetAll(ctx, keys[0]).Result()
			if err != nil {
				t.Fatal(err)
			}
			if err := reserveStAttemptUploadWithClient(ctx, client, keys, 1, budget, 7199); err == nil || !strings.Contains(err.Error(), "clock moved backward") {
				t.Fatalf("clock rollback reset authority: %v", err)
			}
			after, err := client.HGetAll(ctx, keys[0]).Result()
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("rollback mutated quota")
			}
			if after["bucket"] != "2" || after["requests"] != "1" || after["bytes"] != "8" {
				t.Fatal("rollover counters differ")
			}
		})
	})
}

// A malformed account state cannot spend the already checked global budget.
func TestStAttemptUploadQuotaRejectsCorruptionBeforeEitherWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.RedisDoOnce(ctx, func(client server.RedisClient) {
			for _, value := range []string{"-1", "1.5", "not-a-count", "9007199254740992"} {
				keys := stAttemptUploadQuotaKeys(StDeploymentKey(server.NewId().String()), server.NewId())
				if err := client.HSet(ctx, keys[1], "bucket", 2, "requests", value, "bytes", 0).Err(); err != nil {
					t.Fatal(err)
				}
				if err := reserveStAttemptUploadWithClient(ctx, client, keys, 1, stAttemptUploadTestBudget(), 7200); err == nil {
					t.Fatalf("corrupt counter %q was accepted", value)
				}
				if exists, err := client.Exists(ctx, keys[0]).Result(); err != nil || exists != 0 {
					t.Fatalf("corrupt account admitted global write: %v", err)
				}
			}
		})
	})
}

// Count exhaustion is independent: both byte budgets retain ample capacity.
func TestStAttemptUploadQuotaCountOnlyLimitsAreIndependent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		stAttemptUploadIndependentQuotaTest(tb, StAttemptUploadBudget{RequestsPerHour: 5, BytesPerHour: 400, AccountRequestsPerHour: 3, AccountBytesPerHour: 240})
	})
}

// Byte exhaustion is independent: both request budgets retain ample capacity.
func TestStAttemptUploadQuotaByteOnlyLimitsAreIndependent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		stAttemptUploadIndependentQuotaTest(tb, StAttemptUploadBudget{RequestsPerHour: 50, BytesPerHour: 40, AccountRequestsPerHour: 30, AccountBytesPerHour: 24})
	})
}

// The same real four-counter script proves that either independent dimension
// can refuse atomically without spending a different account's reservation.
func stAttemptUploadIndependentQuotaTest(t testing.TB, budget StAttemptUploadBudget) {
	t.Helper()
	ctx := t.Context()
	deployment := StDeploymentKey(server.NewId().String())
	first := stAttemptUploadQuotaKeys(deployment, server.NewId())
	second := stAttemptUploadQuotaKeys(deployment, server.NewId())
	third := stAttemptUploadQuotaKeys(deployment, server.NewId())
	server.RedisDoOnce(ctx, func(client server.RedisClient) {
		for range 3 {
			if err := reserveStAttemptUploadWithClient(ctx, client, first, 8, budget, 7200); err != nil {
				t.Fatal(err)
			}
		}
		before := make([]map[string]string, 2)
		for i, key := range first {
			var err error
			before[i], err = client.HGetAll(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			if before[i]["requests"] != "3" || before[i]["bytes"] != "24" {
				t.Fatal("independent account prerequisite differs")
			}
		}
		var limited interface{ RetryAfterSeconds() int }
		if err := reserveStAttemptUploadWithClient(ctx, client, first, 1, budget, 7200); !errors.As(err, &limited) {
			t.Fatalf("independent account cap was ignored: %v", err)
		}
		for i, key := range first {
			after, err := client.HGetAll(ctx, key).Result()
			if err != nil || !reflect.DeepEqual(before[i], after) {
				t.Fatalf("independent refusal changed counter%d: %v", i, err)
			}
		}
		for range 2 {
			if err := reserveStAttemptUploadWithClient(ctx, client, second, 8, budget, 7200); err != nil {
				t.Fatal(err)
			}
		}
		if err := reserveStAttemptUploadWithClient(ctx, client, third, 1, budget, 7200); !errors.As(err, &limited) {
			t.Fatalf("independent deployment cap was ignored: %v", err)
		}
		if exists, err := client.Exists(ctx, third[1]).Result(); err != nil || exists != 0 {
			t.Fatalf("independent refusal created an account owner: %v", err)
		}
		actual, err := client.HGetAll(ctx, first[0]).Result()
		if err != nil || actual["requests"] != "5" || actual["bytes"] != "40" {
			t.Fatalf("independent final counters differ: %v/%v", actual, err)
		}
	})
}

// The former account allowance fails on precisely the seventeenth logical
// history response. No large response allocation is needed to reproduce it.
func TestStAttemptUploadQuotaOriginalHistoryAllowanceStopsAtSixteen(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		budget := StAttemptUploadBudget{RequestsPerHour: 32768, BytesPerHour: 1024 * 1024 * 1024, AccountRequestsPerHour: 4096, AccountBytesPerHour: 128 * 1024 * 1024}
		keys := stAttemptUploadQuotaKeys(StDeploymentKey(server.NewId().String()), server.NewId())
		server.RedisDoOnce(ctx, func(client server.RedisClient) {
			for index := range 16 {
				if err := reserveStAttemptUploadWithClient(ctx, client, keys, 8*1024*1024, budget, 7200); err != nil {
					t.Fatalf("history reservation %d: %v", index+1, err)
				}
			}
			before, err := client.HGetAll(ctx, keys[0]).Result()
			if err != nil {
				t.Fatal(err)
			}
			var limited interface{ RetryAfterSeconds() int }
			if err := reserveStAttemptUploadWithClient(ctx, client, keys, 8*1024*1024, budget, 7200); !errors.As(err, &limited) {
				t.Fatalf("seventeenth full-size history response did not reproduce exhaustion: %v", err)
			}
			after, err := client.HGetAll(ctx, keys[0]).Result()
			if err != nil || !reflect.DeepEqual(before, after) || after["requests"] != "16" || after["bytes"] != "134217728" {
				t.Fatalf("refused history response changed exact reservations: %v %v", after, err)
			}
		})
	})
}

// All 1,000 miners are observed by both validators, with two reservations per
// member and concurrent hostile full-size uploads. Sixteen owned workers join
// before exact Redis counters are inspected; payload byte budgets stay scalars.
func TestStAttemptUploadQuotaFullPopulationWithConcurrentNoise(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		budget := StAttemptUploadBudget{RequestsPerHour: 262144, BytesPerHour: 2 * 1024 * 1024 * 1024 * 1024, AccountRequestsPerHour: 131072, AccountBytesPerHour: 1024 * 1024 * 1024 * 1024}
		if err := budget.Validate(); err != nil {
			t.Fatal(err)
		}
		accountKeys := make([][][]string, 2)
		for operator := range accountKeys {
			deployment := StDeploymentKey(server.NewId().String())
			for range 3 {
				accountKeys[operator] = append(accountKeys[operator], stAttemptUploadQuotaKeys(deployment, server.NewId()))
			}
		}
		type reservation struct {
			keys []string
			size uint64
		}
		reservations := make([]reservation, 0, 4064)
		for miner := range 500 {
			for operator := range 2 {
				for validator := range 2 {
					for range 2 {
						reservations = append(reservations, reservation{keys: accountKeys[operator][validator], size: 8 * 1024 * 1024})
					}
				}
				if miner%16 == 0 {
					reservations = append(reservations, reservation{keys: accountKeys[operator][2], size: StAttemptUploadMaximumObjectBytes})
				}
			}
		}
		if len(reservations) != 4064 {
			t.Fatal("full population or bounded hostile census changed")
		}
		server.RedisDoOnce(ctx, func(client server.RedisClient) {
			started := time.Now()
			var next atomic.Uint64
			var joined sync.WaitGroup
			ready, release := make(chan struct{}, 16), make(chan struct{})
			failures := make(chan error, 16)
			for range 16 {
				joined.Add(1)
				go func() {
					defer joined.Done()
					ready <- struct{}{}
					<-release
					for {
						index := next.Add(1) - 1
						if index >= uint64(len(reservations)) {
							return
						}
						item := reservations[index]
						if err := reserveStAttemptUploadWithClient(ctx, client, item.keys, item.size, budget, 7200); err != nil {
							failures <- fmt.Errorf("reservation %d: %w", index, err)
							return
						}
					}
				}()
			}
			for range 16 {
				<-ready
			}
			close(release)
			joined.Wait()
			close(failures)
			for err := range failures {
				t.Error(err)
			}
			if t.Failed() {
				return
			}
			for operator, accounts := range accountKeys {
				for member, keys := range accounts {
					wantRequests, wantBytes := uint64(1000), uint64(8000*1024*1024)
					if member == 2 {
						wantRequests, wantBytes = 32, 32*StAttemptUploadMaximumObjectBytes
					}
					actual, err := client.HGetAll(ctx, keys[1]).Result()
					if err != nil || actual["bucket"] != "2" || actual["requests"] != strconv.FormatUint(wantRequests, 10) || actual["bytes"] != strconv.FormatUint(wantBytes, 10) {
						t.Fatalf("operator %d account %d exact reservations differ: %v %v", operator, member, actual, err)
					}
				}
				actual, err := client.HGetAll(ctx, accounts[0][0]).Result()
				if err != nil || actual["bucket"] != "2" || actual["requests"] != "2032" || actual["bytes"] != "17850957824" {
					t.Fatalf("operator %d global reservation census differs: %v %v", operator, actual, err)
				}
			}
			t.Logf("miners=1000 validators=2 operators=2 fallback_reservations=2 workers=16 observations=4000 hostile=64 reserved_bytes=35701915648 elapsed=%s; no payload byte buffers allocated", time.Since(started))
		})
	})
}
