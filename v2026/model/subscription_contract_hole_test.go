package model

// Real Redis scripts exercise membership conservation and exact publication
// ordering. Tests expire keys explicitly rather than waiting on scheduler time.

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Both supplied client IDs are visible in the key and share one cluster slot.
func TestContractHoleKeysUseUnorderedClientIds(t *testing.T) {
	source, destination, other := server.NewId(), server.NewId(), server.NewId()
	forward, reverse := contractHoleKeys(source, destination), contractHoleKeys(destination, source)
	if !reflect.DeepEqual(forward, reverse) || reflect.DeepEqual(forward, contractHoleKeys(source, other)) {
		t.Fatal("pair direction or isolation changed")
	}
	for _, key := range forward {
		if !strings.Contains(key, source.String()) || !strings.Contains(key, destination.String()) || strings.Count(key, "{") != 1 || strings.Count(key, "}") != 1 {
			t.Fatalf("key does not name both client ids in one slot: %s", key)
		}
	}
	if ContractHoleRefreshInterval*2 != ContractHoleTtl || contractHoleEventLifetime >= ContractHoleTtl {
		t.Fatal("refresh cadence or replay-fence lifetime changed")
	}
}

// One selected pair refuses Redis publication without disturbing other tests or
// relying on dependency sleep. Disabling the instance restores its real client.
type contractHoleRedisFailure struct {
	key     string
	enabled atomic.Bool
}

func (self *contractHoleRedisFailure) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *contractHoleRedisFailure) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		args := command.Args()
		if self.enabled.Load() && command.Name() == "eval" && len(args) > 3 && args[3] == self.key {
			return errors.New("synthetic contract-hole Redis refusal")
		}
		return next(ctx, command)
	}
}

func (self *contractHoleRedisFailure) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// The scalar, exact membership and TTL must agree after every mutation.
func requireContractHoleCount(t testing.TB, ctx context.Context, source, destination server.Id, want int64) {
	t.Helper()
	server.Redis(ctx, func(client server.RedisClient) {
		keys := contractHoleKeys(source, destination)
		count, err := client.Get(ctx, keys[0]).Int64()
		if want == 0 {
			if !errors.Is(err, server.RedisNil) {
				t.Fatalf("zero hole key remains: count=%d error=%v", count, err)
			}
		} else {
			if err != nil || count != want {
				t.Fatalf("hole count=%d error=%v want=%d", count, err, want)
			}
			for _, key := range keys[:2] {
				ttl, err := client.PTTL(ctx, key).Result()
				if err != nil || ttl <= 0 || ttl > ContractHoleTtl {
					t.Fatalf("invalid projection ttl=%s error=%v", ttl, err)
				}
			}
		}
		members, err := client.ZCard(ctx, keys[1]).Result()
		if err != nil || members != want {
			t.Fatalf("hole membership=%d error=%v want=%d", members, err, want)
		}
	})
	if HasOpenContractHole(ctx, source, destination) != (want > 0) || HasOpenContractHole(ctx, destination, source) != (want > 0) {
		t.Fatal("reader disagrees with counter")
	}
}

// Repeated create/close, either direction and self-pairs each conserve members.
func TestContractHoleRedisLifecycleConservation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		first, second, self := server.NewId(), server.NewId(), server.NewId()
		for range 2 {
			server.Raise(applyContractHoleEvent(ctx, first, source, destination, "create"))
			server.Raise(applyContractHoleEvent(ctx, second, destination, source, "create"))
			server.Raise(applyContractHoleEvent(ctx, self, source, source, "create"))
		}
		requireContractHoleCount(t, ctx, source, destination, 2)
		requireContractHoleCount(t, ctx, source, source, 1)
		for range 2 {
			server.Raise(applyContractHoleEvent(ctx, first, source, destination, "remove"))
		}
		requireContractHoleCount(t, ctx, source, destination, 1)
		server.Raise(applyContractHoleEvent(ctx, second, destination, source, "remove"))
		requireContractHoleCount(t, ctx, source, destination, 0)
		requireContractHoleCount(t, ctx, source, source, 1)
	})
}

// Busy unrelated deltas cannot preserve a lost revocation past the last source
// expiry. Explicit Redis deadlines force the boundary without sleeping.
func TestContractHoleRedisLifecyclePreservesExpiry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		first, second := server.NewId(), server.NewId()
		keys := contractHoleKeys(source, destination)
		server.Raise(applyContractHoleEvent(ctx, first, source, destination, "create"))
		var originalExpiry int64
		server.Redis(ctx, func(client server.RedisClient) {
			originalExpiry = server.NowUtc().Add(ContractHoleTtl / 4).UnixMilli()
			for _, key := range keys[:2] {
				server.Raise(client.PExpireAt(ctx, key, time.UnixMilli(originalExpiry)).Err())
			}
		})
		for _, operation := range []string{"create", "create", "remove", "remove"} {
			server.Raise(applyContractHoleEvent(ctx, second, source, destination, operation))
			server.Redis(ctx, func(client server.RedisClient) {
				for _, key := range keys[:2] {
					expiry, err := client.Eval(ctx, `local t=redis.call('TIME'); return tonumber(t[1])*1000+math.floor(tonumber(t[2])/1000)+redis.call('PTTL',KEYS[1])`, []string{key}).Int64()
					if err != nil || expiry > originalExpiry {
						t.Fatalf("lifecycle %s renewed expiry: got=%d original=%d error=%v", operation, expiry, originalExpiry, err)
					}
				}
			})
		}
		requireContractHoleCount(t, ctx, source, destination, 1)
		server.Redis(ctx, func(client server.RedisClient) {
			for _, key := range keys[:2] {
				server.Raise(client.PExpireAt(ctx, key, time.Unix(1, 0)).Err())
			}
		})
		requireContractHoleCount(t, ctx, source, destination, 0)
		server.Raise(applyContractHoleEvent(ctx, server.NewId(), source, destination, "create"))
		requireContractHoleCount(t, ctx, source, destination, 1)
	})
}

// A delayed create cannot revive a closed member, including after all ephemeral
// Redis state expires: the captured event itself has a fixed bounded lifetime.
func TestContractHoleRedisCloseBeforeCreateAndExpiredCallback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination, contract := server.NewId(), server.NewId(), server.NewId()
		server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "remove"))
		server.Raise(applyContractHoleEvent(ctx, contract, destination, source, "create"))
		requireContractHoleCount(t, ctx, source, destination, 0)
		server.Redis(ctx, func(client server.RedisClient) {
			for _, key := range contractHoleKeys(source, destination) {
				server.Raise(client.PExpireAt(ctx, key, time.Unix(1, 0)).Err())
			}
		})
		contractHoleEventPost(ctx, contract, source, destination, "create", time.Unix(1, 0))()
		requireContractHoleCount(t, ctx, source, destination, 0)
	})
}

// Both successful authority and metadata disappear at expiry; malformed and
// canceled Redis reads deny without an inline reconstruction attempt.
func TestContractHoleRedisExpiryAndMalformedReadsFailClosed(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination, contract := server.NewId(), server.NewId(), server.NewId()
		server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "create"))
		requireContractHoleCount(t, ctx, source, destination, 1)
		server.Redis(ctx, func(client server.RedisClient) {
			for _, key := range contractHoleKeys(source, destination) {
				server.Raise(client.PExpireAt(ctx, key, time.Unix(1, 0)).Err())
			}
		})
		requireContractHoleCount(t, ctx, source, destination, 0)
		for _, value := range []string{"broken", "-1", "-0", "0", "1.5", "0001", "+1", "1e0", " 1", "8193"} {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Set(ctx, contractHoleKeys(source, destination)[0], value, ContractHoleTtl).Err())
			})
			if HasOpenContractHole(ctx, source, destination) {
				t.Fatalf("malformed count %q authorized", value)
			}
		}
		for _, expiry := range []time.Duration{0, ContractHoleTtl + time.Hour} {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Set(ctx, contractHoleKeys(source, destination)[0], "1", expiry).Err())
			})
			if HasOpenContractHole(ctx, source, destination) {
				t.Fatalf("scalar escaped the authorization ttl: expiry=%s", expiry)
			}
		}
		server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "create"))
		if !HasOpenContractHole(ctx, source, destination) {
			t.Fatal("cancellation control lacks a healthy permission")
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if HasOpenContractHole(canceled, source, destination) {
			t.Fatal("unavailable Redis authorized")
		}
	})
}

// Exercise the actual source-publication script with explicit barriers: a
// lifecycle event and then a newer refresher each invalidate the old snapshot.
func TestContractHoleRedisRefreshFencesEventsAndOlderSnapshots(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination, contract := server.NewId(), server.NewId(), server.NewId()
		keys := contractHoleKeys(source, destination)
		begin := func(token string) {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Eval(ctx, contractHoleBeginRefreshScript, keys, token, contractHoleSourceTimeout.Milliseconds()).Err())
			})
		}
		publish := func(token string, members ...server.Id) int64 {
			var result int64
			args := []any{token, ContractHoleTtl.Milliseconds()}
			for _, member := range members {
				args = append(args, member.String(), "+inf")
			}
			server.Redis(ctx, func(client server.RedisClient) {
				values, err := client.Eval(ctx, contractHolePublishScript, keys, args...).Slice()
				server.Raise(err)
				if len(values) != 2 {
					t.Fatal("invalid snapshot result", values)
				}
				result = values[0].(int64)
			})
			return result
		}
		server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "create"))
		begin("first")
		server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "remove"))
		if publish("first", contract) != 0 {
			t.Fatal("pre-close snapshot resurrected membership")
		}
		requireContractHoleCount(t, ctx, source, destination, 0)
		begin("older")
		begin("newer")
		if publish("older", contract) != 0 || publish("newer") != 1 {
			t.Fatal("snapshot generation fence failed")
		}
		begin("reopened")
		if publish("reopened", contract) != 1 {
			t.Fatal("authoritative reopen refused")
		}
		server.Redis(ctx, func(client server.RedisClient) { server.Raise(client.Set(ctx, keys[0], 99, ContractHoleTtl).Err()) })
		begin("repair")
		if publish("repair", contract, contract) != 1 {
			t.Fatal("repair refused")
		}
		requireContractHoleCount(t, ctx, source, destination, 1)
	})
}

// Every page owns its concurrency cap; held callbacks prove exactly eight may
// enter, and the joined high-water count proves later jobs cannot exceed it.
func TestContractHoleRefreshWorkersBoundAndJoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	positions := make([]ContractHoleCursor, ContractHoleRefreshPageSize)
	for index := range positions {
		positions[index] = ContractHoleCursor{SourceClientId: server.NewId(), DestinationClientId: server.NewId()}
	}
	entered := make(chan struct{}, len(positions))
	release := make(chan struct{})
	var active, peak, calls atomic.Int32
	type result struct{ pairs, failed int }
	done := make(chan result, 1)
	go func() {
		pairs, failed := refreshContractHolePairs(ctx, positions, func(ctx context.Context, _, _ server.Id) (bool, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); current > old; old = peak.Load() {
				if peak.CompareAndSwap(old, current) {
					break
				}
			}
			calls.Add(1)
			entered <- struct{}{}
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-release:
				return true, nil
			}
		})
		done <- result{pairs: pairs, failed: failed}
	}()
	for range contractHoleRefreshWorkers {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("worker barrier did not complete", ctx.Err())
		}
	}
	if active.Load() != contractHoleRefreshWorkers {
		t.Error("worker barrier did not retain the configured ownership")
	}
	close(release)
	got := <-done
	if got.pairs != len(positions) || got.failed != 0 || calls.Load() != int32(len(positions)) || active.Load() != 0 || peak.Load() != contractHoleRefreshWorkers {
		t.Fatalf("result=%+v calls=%d active=%d peak=%d", got, calls.Load(), active.Load(), peak.Load())
	}
}

// Cancellation accounts for unobserved pairs and admits no source callback.
func TestContractHoleRefreshCanceledPageRemainsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	positions := []ContractHoleCursor{{SourceClientId: server.NewId(), DestinationClientId: server.NewId()}}
	called := false
	pairs, failed := refreshContractHolePairs(ctx, positions, func(context.Context, server.Id, server.Id) (bool, error) {
		called = true
		return true, nil
	})
	if called || pairs != 1 || failed != 1 {
		t.Fatalf("canceled page called=%t pairs=%d failed=%d", called, pairs, failed)
	}
}
