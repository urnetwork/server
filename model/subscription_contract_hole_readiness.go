package model

// Warm-readiness is a control-plane rollout receipt, never a dependency of packet
// admission. It records a complete timely source pass and a surviving earliest
// positive pair. Partial passes invalidate it without blocking healthy packets.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/urnetwork/server"
)

const contractHoleReadinessKey = "contract-hole:v2:{refresh}:ready"

// SourceStarted precedes publication and bounds refresh freshness; each member
// may have an earlier absolute expiry, checked by the live witness lease.
type ContractHoleWitness struct {
	SourceClientId      server.Id `json:"source_client_id"`
	DestinationClientId server.Id `json:"destination_client_id"`
	SourceStarted       time.Time `json:"source_started"`
}

// PairVisits includes repeated candidates across page/direction boundaries; it
// does not claim a global distinct-pair census. All successful visits must have
// published a full snapshot. The TTL bound applies to the earliest whole pass.
type ContractHoleReadiness struct {
	Version                   int                  `json:"version"`
	PassStarted               time.Time            `json:"pass_started"`
	PassCompleted             time.Time            `json:"pass_completed"`
	CoveredUntil              time.Time            `json:"covered_until"`
	PairVisits                int                  `json:"pair_visits"`
	SuccessfulPairs           int                  `json:"successful_pairs"`
	UnknownPairs              int                  `json:"unknown_pairs"`
	PositivePairs             int                  `json:"positive_pairs"`
	Pages                     int                  `json:"pages"`
	EarliestPositive          *ContractHoleWitness `json:"earliest_positive,omitempty"`
	MinimumRemainingTtlMillis int64                `json:"minimum_remaining_ttl_millis"`
	WitnessRemainingTtlMillis int64                `json:"witness_remaining_ttl_millis"`
}

// Checks the receipt's full-pass freshness independently of Redis key expiry.
func validContractHoleReadiness(receipt *ContractHoleReadiness, now time.Time) bool {
	if receipt == nil || receipt.Version != 1 || receipt.Pages <= 0 || receipt.PairVisits < 0 ||
		receipt.UnknownPairs != 0 || receipt.SuccessfulPairs != receipt.PairVisits || receipt.PositivePairs < 0 || receipt.PositivePairs > receipt.SuccessfulPairs ||
		receipt.PassCompleted.Before(receipt.PassStarted) || receipt.PassCompleted.After(now) ||
		receipt.PassCompleted.Sub(receipt.PassStarted) >= contractHoleReadinessMargin || !now.Before(receipt.PassStarted.Add(ContractHoleTtl)) {
		return false
	}
	if receipt.PositivePairs == 0 {
		return receipt.EarliestPositive == nil
	}
	return receipt.EarliestPositive != nil && !receipt.EarliestPositive.SourceStarted.Before(receipt.PassStarted) &&
		!receipt.EarliestPositive.SourceStarted.After(receipt.PassCompleted)
}

// Reads the earliest positive witness in one slot. A legitimate intervening final
// close or absolute expiry can invalidate this conservative gate; a later pass
// supplies a new witness. Packet reads never depend on this receipt.
func contractHoleWitnessLease(ctx context.Context, witness *ContractHoleWitness) (time.Time, error) {
	status, validUntil, err := ReadContractHoleLease(ctx, witness.SourceClientId, witness.DestinationClientId)
	if err != nil {
		return time.Time{}, err
	}
	if status != ContractHolePositive || !time.Now().Before(validUntil) {
		return time.Time{}, errors.New("invalid contract hole witness")
	}
	return validUntil, nil
}

// Publishes only complete passes inside the half-TTL margin. The receipt's own
// expiry is capped at pass-start plus TTL, so completing a pass cannot conceal
// an early pair's expiry. This function performs no source/database operation.
func PublishContractHoleReadiness(ctx context.Context, receipt *ContractHoleReadiness) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, contractHoleRedisTimeout)
	defer cancel()
	now := server.NowUtc()
	if !validContractHoleReadiness(receipt, now) {
		return false, InvalidateContractHoleReadiness(ctx)
	}
	receipt.CoveredUntil = receipt.PassStarted.Add(ContractHoleTtl)
	var witnessUntil time.Time
	if receipt.EarliestPositive != nil {
		var err error
		witnessUntil, err = contractHoleWitnessLease(ctx, receipt.EarliestPositive)
		if err != nil {
			_ = InvalidateContractHoleReadiness(ctx)
			return false, err
		}
		if witnessUntil.Before(receipt.CoveredUntil) {
			receipt.CoveredUntil = witnessUntil
		}
	}
	receipt.CoveredUntil = receipt.CoveredUntil.Truncate(time.Millisecond)
	now = server.NowUtc()
	remaining := receipt.CoveredUntil.Sub(now)
	if now.Sub(receipt.PassStarted) >= contractHoleReadinessMargin || remaining <= contractHoleReadinessMargin {
		return false, InvalidateContractHoleReadiness(ctx)
	}
	if receipt.EarliestPositive != nil {
		receipt.WitnessRemainingTtlMillis = witnessUntil.Sub(now).Milliseconds()
	}
	receipt.MinimumRemainingTtlMillis = remaining.Milliseconds()
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return false, err
	}
	err = server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		return client.Eval(ctx, `redis.call('SET',KEYS[1],ARGV[1],'PXAT',ARGV[2]); return 1`, []string{contractHoleReadinessKey},
			encoded, receipt.CoveredUntil.UnixMilli()).Err()
	})
	return err == nil, err
}

// Loss of coverage closes the rollout gate without changing any pair's counter.
// The task owns one bounded cleanup even after its source context was canceled.
func InvalidateContractHoleReadiness(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), contractHoleRedisTimeout)
	defer cancel()
	return server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		return client.Eval(ctx, `return redis.call('DEL',KEYS[1])`, []string{contractHoleReadinessKey}).Err()
	})
}

// Control/startup qualification only. It rechecks expiry and the surviving early
// witness; packet paths use HasOpenContractHole and never consult this receipt.
func ReadContractHoleReadiness(ctx context.Context) (*ContractHoleReadiness, error) {
	ctx, cancel := context.WithTimeout(ctx, contractHoleRedisTimeout)
	defer cancel()
	var encoded string
	err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		var err error
		encoded, err = client.Eval(ctx, `if redis.call('PTTL',KEYS[1]) <= 0 then return false end; return redis.call('GET',KEYS[1])`, []string{contractHoleReadinessKey}).Text()
		return err
	})
	if err != nil {
		return nil, err
	}
	receipt := &ContractHoleReadiness{}
	if err := json.Unmarshal([]byte(encoded), receipt); err != nil {
		return nil, err
	}
	if !validContractHoleReadiness(receipt, server.NowUtc()) || receipt.MinimumRemainingTtlMillis <= 0 || receipt.MinimumRemainingTtlMillis > ContractHoleTtl.Milliseconds() ||
		receipt.CoveredUntil.After(receipt.PassStarted.Add(ContractHoleTtl)) || !receipt.PassCompleted.Before(receipt.CoveredUntil) {
		return nil, errors.New("contract hole readiness is stale or incomplete")
	}
	var witnessUntil time.Time
	if receipt.EarliestPositive != nil {
		var err error
		witnessUntil, err = contractHoleWitnessLease(ctx, receipt.EarliestPositive)
		if err != nil {
			return nil, err
		}
		if witnessUntil.Before(receipt.CoveredUntil) {
			receipt.CoveredUntil = witnessUntil
		}
	}
	now := server.NowUtc()
	if receipt.EarliestPositive != nil {
		receipt.WitnessRemainingTtlMillis = witnessUntil.Sub(now).Milliseconds()
	}
	receipt.MinimumRemainingTtlMillis = receipt.CoveredUntil.Sub(now).Milliseconds()
	if receipt.MinimumRemainingTtlMillis <= 0 {
		return nil, errors.New("contract hole readiness lost its expiry margin")
	}
	return receipt, nil
}
