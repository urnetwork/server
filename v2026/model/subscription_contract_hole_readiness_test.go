package model

// Readiness is a control-plane proof of one complete timely source pass. It
// cannot extend an early pair's expiry or become a dependency of packet reads.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A measured lease can be slightly shorter than the nominal pass-start bound.
// Store that real bound without rejecting healthy millisecond/I/O rounding.
func TestContractHoleReadinessCapsMeasuredLeaseAndAbsoluteDeadline(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, finite := range []bool{false, true} {
			source, destination, contract := server.NewId(), server.NewId(), server.NewId()
			start := server.NowUtc()
			bound := start.Add(ContractHoleTtl - 5*time.Millisecond).Truncate(time.Millisecond)
			if finite {
				bound = start.Add(3 * ContractHoleTtl / 4).Truncate(time.Millisecond)
				server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "create", bound))
			} else {
				server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "create"))
				server.Redis(ctx, func(client server.RedisClient) {
					for _, key := range contractHoleKeys(source, destination)[:2] {
						server.Raise(client.PExpireAt(ctx, key, bound).Err())
					}
				})
			}
			receipt := &ContractHoleReadiness{Version: 1, PassStarted: start, PassCompleted: server.NowUtc(),
				PairVisits: 1, SuccessfulPairs: 1, PositivePairs: 1, Pages: 1,
				EarliestPositive: &ContractHoleWitness{SourceClientId: source, DestinationClientId: destination, SourceStarted: start}}
			ready, err := PublishContractHoleReadiness(ctx, receipt)
			if err != nil || !ready || receipt.CoveredUntil.After(bound) || receipt.MinimumRemainingTtlMillis <= contractHoleReadinessMargin.Milliseconds() {
				t.Fatalf("measured finite=%t readiness=%t receipt=%+v error=%v", finite, ready, receipt, err)
			}
			server.Redis(ctx, func(client server.RedisClient) {
				expiry, err := client.Eval(ctx, `return redis.call('PEXPIRETIME', KEYS[1])`, []string{contractHoleReadinessKey}).Int64()
				if err != nil || expiry > bound.UnixMilli() || expiry != receipt.CoveredUntil.UnixMilli() {
					t.Fatalf("receipt exceeded observed lease: expiry=%d receipt=%s bound=%s error=%v", expiry, receipt.CoveredUntil, bound, err)
				}
			})
			observed, err := ReadContractHoleReadiness(ctx)
			if err != nil || observed == nil || observed.CoveredUntil.After(receipt.CoveredUntil) {
				t.Fatalf("measurement changed coverage optimistically: receipt=%+v error=%v", observed, err)
			}
			for _, invalidBound := range []time.Time{{}, start.Add(ContractHoleTtl + time.Second)} {
				corrupt := *receipt
				corrupt.CoveredUntil = invalidBound
				encoded, err := json.Marshal(corrupt)
				server.Raise(err)
				server.Redis(ctx, func(client server.RedisClient) {
					server.Raise(client.Set(ctx, contractHoleReadinessKey, encoded, ContractHoleTtl).Err())
				})
				if observed, err := ReadContractHoleReadiness(ctx); err == nil || observed != nil {
					t.Fatalf("invalid coverage bound accepted: receipt=%+v error=%v", observed, err)
				}
			}
		}
	})
}

// Pure clock boundaries distinguish an incomplete/slow pass from healthy zero.
func TestContractHoleReadinessRequiresCompleteTimelyPass(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, scenario := range []string{"healthy", "empty", "version", "pages", "unknown", "unsuccessful", "negative", "missing_witness", "unexpected_witness", "early_witness", "late_witness", "future", "half_ttl", "expired"} {
		now := start.Add(2 * time.Second)
		receipt := &ContractHoleReadiness{Version: 1, PassStarted: start, PassCompleted: start.Add(time.Second),
			PairVisits: 3, SuccessfulPairs: 3, PositivePairs: 3, Pages: 1,
			EarliestPositive: &ContractHoleWitness{SourceStarted: start}}
		switch scenario {
		case "empty":
			receipt.PairVisits, receipt.SuccessfulPairs, receipt.PositivePairs, receipt.EarliestPositive = 0, 0, 0, nil
		case "version":
			receipt.Version = 0
		case "pages":
			receipt.Pages = 0
		case "unknown":
			receipt.UnknownPairs = 1
		case "unsuccessful":
			receipt.SuccessfulPairs--
		case "negative":
			receipt.PairVisits, receipt.SuccessfulPairs = -1, -1
		case "missing_witness":
			receipt.EarliestPositive = nil
		case "unexpected_witness":
			receipt.PositivePairs = 0
		case "early_witness":
			receipt.EarliestPositive.SourceStarted = start.Add(-time.Nanosecond)
		case "late_witness":
			receipt.EarliestPositive.SourceStarted = receipt.PassCompleted.Add(time.Nanosecond)
		case "future":
			receipt.PassCompleted = now.Add(time.Nanosecond)
		case "half_ttl":
			now = start.Add(contractHoleReadinessMargin)
			receipt.PassCompleted = now
		case "expired":
			now = start.Add(ContractHoleTtl)
		}
		want := scenario == "healthy" || scenario == "empty"
		if got := validContractHoleReadiness(receipt, now); got != want {
			t.Fatalf("%s readiness=%t want=%t receipt=%+v", scenario, got, want, receipt)
		}
	}
}

// Removing a rollout receipt does not revoke a healthy pair. The witness and
// receipt must each retain a real expiry; malformed/persistent state is unknown.
func TestContractHoleReadinessDoesNotGatePacketPermission(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination, contract := server.NewId(), server.NewId(), server.NewId()
		start := server.NowUtc()
		server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "create"))
		receipt := &ContractHoleReadiness{Version: 1, PassStarted: start, PassCompleted: server.NowUtc(),
			PairVisits: 1, SuccessfulPairs: 1, PositivePairs: 1, Pages: 1,
			EarliestPositive: &ContractHoleWitness{SourceClientId: source, DestinationClientId: destination, SourceStarted: start}}
		ready, err := PublishContractHoleReadiness(ctx, receipt)
		if err != nil || !ready {
			t.Fatalf("healthy readiness=%t error=%v", ready, err)
		}
		observed, err := ReadContractHoleReadiness(ctx)
		if err != nil || observed == nil || observed.PairVisits != 1 || observed.UnknownPairs != 0 || observed.MinimumRemainingTtlMillis <= contractHoleReadinessMargin.Milliseconds() {
			t.Fatalf("healthy receipt=%+v error=%v", observed, err)
		}
		server.Raise(InvalidateContractHoleReadiness(ctx))
		if observed, err := ReadContractHoleReadiness(ctx); err == nil || observed != nil {
			t.Fatalf("missing gate authorized rollout: receipt=%+v error=%v", observed, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 1)
		encoded, err := json.Marshal(receipt)
		server.Raise(err)
		for _, scenario := range []string{"persistent", "malformed", "expired"} {
			server.Redis(ctx, func(client server.RedisClient) {
				switch scenario {
				case "persistent":
					server.Raise(client.Set(ctx, contractHoleReadinessKey, encoded, 0).Err())
				case "malformed":
					server.Raise(client.Set(ctx, contractHoleReadinessKey, "broken", ContractHoleTtl).Err())
				case "expired":
					server.Raise(client.Set(ctx, contractHoleReadinessKey, encoded, ContractHoleTtl).Err())
					server.Raise(client.PExpireAt(ctx, contractHoleReadinessKey, time.Unix(1, 0)).Err())
				}
			})
			if observed, err := ReadContractHoleReadiness(ctx); err == nil || observed != nil {
				t.Fatalf("%s receipt accepted: receipt=%+v error=%v", scenario, observed, err)
			}
			requireContractHoleCount(t, ctx, source, destination, 1)
		}
	})
}

// A completion timestamp cannot hide an early pair with insufficient TTL, even
// while that pair is still a valid positive packet permission at this instant.
func TestContractHoleReadinessRequiresSurvivingEarliestPair(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination, contract := server.NewId(), server.NewId(), server.NewId()
		start := server.NowUtc()
		server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "create"))
		receipt := &ContractHoleReadiness{Version: 1, PassStarted: start, PassCompleted: server.NowUtc(),
			PairVisits: 1, SuccessfulPairs: 1, PositivePairs: 1, Pages: 1,
			EarliestPositive: &ContractHoleWitness{SourceClientId: source, DestinationClientId: destination, SourceStarted: start}}
		ready, err := PublishContractHoleReadiness(ctx, receipt)
		if err != nil || !ready {
			t.Fatalf("initial readiness=%t error=%v", ready, err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			for _, key := range contractHoleKeys(source, destination)[:2] {
				server.Raise(client.PExpire(ctx, key, ContractHoleTtl/4).Err())
			}
		})
		if observed, err := ReadContractHoleReadiness(ctx); err != nil || observed == nil ||
			observed.MinimumRemainingTtlMillis <= 0 || observed.MinimumRemainingTtlMillis > (ContractHoleTtl/4).Milliseconds() ||
			!observed.CoveredUntil.Before(receipt.CoveredUntil) {
			t.Fatalf("short witness did not shorten coverage: receipt=%+v error=%v", observed, err)
		}
		ready, err = PublishContractHoleReadiness(ctx, receipt)
		if err != nil || ready {
			t.Fatalf("short witness republished: ready=%t error=%v", ready, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 1)
		server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "remove"))
		ready, err = PublishContractHoleReadiness(ctx, receipt)
		if err == nil || ready {
			t.Fatalf("closed witness republished: ready=%t error=%v", ready, err)
		}
	})
}
