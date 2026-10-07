package model

// Absolute deadlines constrain both live Redis membership and cached leases.
// Clock boundaries are explicit; fixture mutations avoid scheduler sleeps.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Equality expires authority. A response cannot recover time already spent in
// I/O, and a legacy member keeps only a bounded lease rather than a new lifespan.
func TestContractHoleLeaseAbsoluteAndTransportBoundaries(t *testing.T) {
	start := time.Unix(1000, 0)
	deadline := start.Add(2 * time.Second)
	for _, sample := range []struct {
		name       string
		values     []any
		now        time.Time
		status     ContractHoleStatus
		validUntil time.Time
		wantError  bool
	}{
		{name: "finite", values: []any{int64(2), int64(60000), strconv.FormatInt(deadline.UnixMilli(), 10)}, now: start.Add(time.Second), status: ContractHolePositive, validUntil: deadline},
		{name: "equality", values: []any{int64(1), int64(60000), strconv.FormatInt(deadline.UnixMilli(), 10)}, now: deadline, status: ContractHoleNegative},
		{name: "elapsed", values: []any{int64(1), int64(60000), strconv.FormatInt(deadline.UnixMilli(), 10)}, now: deadline.Add(time.Millisecond), status: ContractHoleNegative},
		{name: "legacy", values: []any{int64(2), int64(15000), "inf"}, now: start.Add(time.Second), status: ContractHolePositive, validUntil: start.Add(15 * time.Second)},
		{name: "transport", values: []any{int64(1), int64(1000), "inf"}, now: start.Add(time.Second), status: ContractHoleUnknown},
		{name: "negative", values: []any{int64(0), int64(1000), "0"}, now: start, status: ContractHoleNegative},
		{name: "fraction", values: []any{int64(1), int64(1000), "1000000.5"}, now: start, status: ContractHoleUnknown, wantError: true},
		{name: "nan", values: []any{int64(1), int64(1000), "NaN"}, now: start, status: ContractHoleUnknown, wantError: true},
		{name: "count", values: []any{int64(8193), int64(1000), "inf"}, now: start, status: ContractHoleUnknown, wantError: true},
		{name: "ttl", values: []any{int64(1), contractHoleMaximumTtl.Milliseconds() + 1, "inf"}, now: start, status: ContractHoleUnknown, wantError: true},
		{name: "shape", values: []any{int64(1)}, now: start, status: ContractHoleUnknown, wantError: true},
	} {
		status, validUntil, err := contractHoleLease(sample.values, start, sample.now)
		if status != sample.status || !validUntil.Equal(sample.validUntil) || (err != nil) != sample.wantError {
			t.Fatalf("%s status=%d valid_until=%s error=%v", sample.name, status, validUntil, err)
		}
	}
	if contractHoleExpirationScore() != "+inf" || contractHoleExpirationScore(time.Time{}) != "-62135596800000" {
		t.Fatal("an explicit ancient deadline became a legacy contract")
	}
}

// An expired member is decremented exactly once while a later or legacy member
// retains the pair. Duplicate events never extend a persisted finite deadline.
func TestContractHoleRedisExpirationConservesMixedMembers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		first, later, legacy := server.NewId(), server.NewId(), server.NewId()
		earlyDeadline := server.NowUtc().Add(30 * time.Second).Truncate(time.Millisecond)
		laterDeadline := earlyDeadline.Add(15 * time.Second)
		server.Raise(applyContractHoleEvent(ctx, first, source, destination, "create", earlyDeadline))
		server.Raise(applyContractHoleEvent(ctx, later, destination, source, "create", laterDeadline))
		server.Raise(applyContractHoleEvent(ctx, first, source, destination, "create", laterDeadline.Add(time.Hour)))
		requireContractHoleCount(t, ctx, source, destination, 2)
		server.Redis(ctx, func(client server.RedisClient) {
			score, err := client.ZScore(ctx, contractHoleKeys(source, destination)[1], first.String()).Result()
			if err != nil || score != float64(earlyDeadline.UnixMilli()) {
				t.Fatal("duplicate event extended immutable deadline", score, err)
			}
		})
		status, validUntil, err := ReadContractHoleLease(ctx, source, destination)
		if err != nil || status != ContractHolePositive || !validUntil.Equal(laterDeadline) {
			t.Fatalf("mixed finite lease status=%d deadline=%s error=%v", status, validUntil, err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.ZAdd(ctx, contractHoleKeys(source, destination)[1], redis.Z{Score: 1, Member: first.String()}).Err())
		})
		status, validUntil, err = ReadContractHoleLease(ctx, source, destination)
		if err != nil || status != ContractHolePositive || !validUntil.Equal(laterDeadline) {
			t.Fatalf("surviving finite lease status=%d deadline=%s error=%v", status, validUntil, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 1)
		server.Raise(applyContractHoleEvent(ctx, first, source, destination, "create", time.UnixMilli(1)))
		server.Raise(applyContractHoleEvent(ctx, server.NewId(), source, destination, "create", time.Time{}))
		requireContractHoleCount(t, ctx, source, destination, 1)
		server.Raise(applyContractHoleEvent(ctx, legacy, source, destination, "create"))
		status, validUntil, err = ReadContractHoleLease(ctx, source, destination)
		if err != nil || status != ContractHolePositive || !validUntil.After(laterDeadline) || time.Until(validUntil) > ContractHoleTtl {
			t.Fatalf("legacy lease status=%d deadline=%s error=%v", status, validUntil, err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.ZAdd(ctx, contractHoleKeys(source, destination)[1], redis.Z{Score: 1, Member: later.String()}).Err())
		})
		if !HasOpenContractHole(ctx, source, destination) {
			t.Fatal("finite expiry revoked a legacy member")
		}
		requireContractHoleCount(t, ctx, source, destination, 1)
		server.Raise(applyContractHoleEvent(ctx, legacy, source, destination, "remove"))
		requireContractHoleCount(t, ctx, source, destination, 0)
	})
}

// An expired final member revokes before any background task runs. Old version
// counts and absent/persistent/mismatched member metadata cannot bypass expiry.
func TestContractHoleRedisExpirationRequiresVersionedMembership(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, scenario := range []string{"expired", "missing_members", "persistent_members", "mismatch", "old_version"} {
			source, destination, contract := server.NewId(), server.NewId(), server.NewId()
			keys := contractHoleKeys(source, destination)
			if scenario != "old_version" {
				server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "create", server.NowUtc().Add(time.Hour)))
			}
			server.Redis(ctx, func(client server.RedisClient) {
				switch scenario {
				case "expired":
					server.Raise(client.ZAdd(ctx, keys[1], redis.Z{Score: 1, Member: contract.String()}).Err())
				case "missing_members":
					server.Raise(client.Del(ctx, keys[1]).Err())
				case "persistent_members":
					server.Raise(client.Persist(ctx, keys[1]).Err())
				case "mismatch":
					server.Raise(client.Set(ctx, keys[0], 2, ContractHoleTtl).Err())
				case "old_version":
					server.Raise(client.Set(ctx, strings.Replace(keys[0], ":v2:", ":v1:", 1), 1, ContractHoleTtl).Err())
				}
			})
			status, _, err := ReadContractHoleLease(ctx, source, destination)
			if scenario == "expired" {
				if status != ContractHoleNegative || err != nil {
					t.Fatalf("expired status=%d error=%v", status, err)
				}
				requireContractHoleCount(t, ctx, source, destination, 0)
			} else if status != ContractHoleUnknown || (err != nil) != (scenario != "old_version") {
				t.Fatalf("%s status=%d error=%v", scenario, status, err)
			}
		}
	})
}

// A source read begun before expiry cannot publish that member after expiry.
// Publication reports the actual surviving count for the readiness witness.
func TestContractHoleRedisPublicationRechecksAbsoluteDeadline(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		keys := contractHoleKeys(source, destination)
		expired, ancient, live := server.NewId(), server.NewId(), server.NewId()
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Eval(ctx, contractHoleBeginRefreshScript, keys, "deadline", contractHoleSourceTimeout.Milliseconds()).Err())
			values, err := client.Eval(ctx, contractHolePublishScript, keys, "deadline", ContractHoleTtl.Milliseconds(),
				expired.String(), "1", ancient.String(), contractHoleExpirationScore(time.Time{}), live.String(), "+inf").Slice()
			server.Raise(err)
			if len(values) != 2 || values[0] != int64(1) || values[1] != int64(1) {
				t.Fatal("snapshot counted an expired member", values)
			}
		})
		requireContractHoleCount(t, ctx, source, destination, 1)
	})
}

// Both source APIs and refresh share absolute expiry, while a resumable
// checkpoint leaves the original deadline intact and legacy NULL stays eligible.
func TestContractHoleAbsoluteExpirationSourceAndRefresh(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		expired := insertContractHoleSourceRow(ctx, source, destination, time.UnixMilli(1))
		deadline := server.NowUtc().Add(time.Hour).Truncate(time.Millisecond)
		live := insertContractHoleSourceRow(ctx, destination, source, deadline)
		for _, pair := range [][2]server.Id{{source, destination}, {destination, source}} {
			status, validUntil, err := ReadResumableContractLease(ctx, pair[0], pair[1])
			if err != nil || status != ContractHolePositive || !validUntil.After(server.NowUtc()) || validUntil.After(deadline) {
				t.Fatalf("source lease status=%d deadline=%s error=%v", status, validUntil, err)
			}
		}
		server.Raise(CloseContract(ctx, live, destination, 0, true))
		published, err := refreshContractHole(ctx, source, destination)
		if err != nil || !published {
			t.Fatal("checkpoint refresh", published, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 1)
		server.Redis(ctx, func(client server.RedisClient) {
			score, err := client.ZScore(ctx, contractHoleKeys(source, destination)[1], live.String()).Result()
			if err != nil || score != float64(deadline.UnixMilli()) {
				t.Fatal("checkpoint changed the persisted deadline", score, err)
			}
			if _, err := client.ZScore(ctx, contractHoleKeys(source, destination)[1], expired.String()).Result(); !errors.Is(err, server.RedisNil) {
				t.Fatal("expired source member was published", err)
			}
		})
		server.Raise(CloseContract(ctx, live, destination, 0, false))
		status, _, err := ReadResumableContractLease(ctx, source, destination)
		if err != nil || status != ContractHoleNegative || HasOpenContractForPair(ctx, source, destination) {
			t.Fatalf("expired/final source granted: status=%d error=%v", status, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 0)
		insertContractHoleSourceRow(ctx, source, destination)
		status, validUntil, err := ReadResumableContractLease(ctx, source, destination)
		if err != nil || status != ContractHolePositive || time.Until(validUntil) > ContractHoleTtl {
			t.Fatalf("legacy source lease status=%d deadline=%s error=%v", status, validUntil, err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if status, _, err := ReadResumableContractLease(canceled, source, destination); status != ContractHoleUnknown || err == nil {
			t.Fatalf("canceled source status=%d error=%v", status, err)
		}
	})
}
