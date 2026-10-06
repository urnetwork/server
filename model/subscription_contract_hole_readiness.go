package model

// Warm-readiness is a control-plane rollout receipt, never a dependency of packet
// admission. It records a complete timely source pass and a surviving earliest
// positive pair. Partial passes invalidate it without blocking healthy packets.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/urnetwork/server"
)

const contractHoleReadinessKey = "contract-hole:v1:{refresh}:ready"

// SourceStarted precedes this pair's publication and bounds its earliest expiry.
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
		receipt.PassCompleted.Sub(receipt.PassStarted) >= ContractHoleRefreshInterval || !now.Before(receipt.PassStarted.Add(ContractHoleTtl)) {
		return false
	}
	if receipt.PositivePairs == 0 {
		return receipt.EarliestPositive == nil
	}
	return receipt.EarliestPositive != nil && !receipt.EarliestPositive.SourceStarted.Before(receipt.PassStarted) &&
		!receipt.EarliestPositive.SourceStarted.After(receipt.PassCompleted)
}

const contractHoleWitnessScript = `
local value = redis.call('GET', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if not value or ttl <= 0 then return false end
return {value, ttl}
`

// Reads the earliest positive witness in one slot. A legitimate intervening final
// close can invalidate this conservative gate; a later pass supplies a new witness.
func contractHoleWitnessTtl(ctx context.Context, witness *ContractHoleWitness) (time.Duration, error) {
	var ttl time.Duration
	err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		values, err := client.Eval(ctx, contractHoleWitnessScript, contractHoleKeys(witness.SourceClientId, witness.DestinationClientId)[:1]).Slice()
		if err != nil {
			return err
		}
		if len(values) != 2 {
			return errors.New("invalid contract hole witness")
		}
		value, valueOk := values[0].(string)
		milliseconds, ttlOk := values[1].(int64)
		count, parseErr := strconv.ParseInt(value, 10, 64)
		if !valueOk || !ttlOk || parseErr != nil || value != strconv.FormatInt(count, 10) || count <= 0 || count > contractHoleMemberLimit || milliseconds <= 0 || milliseconds > ContractHoleTtl.Milliseconds() {
			return errors.New("invalid contract hole witness")
		}
		ttl = time.Duration(milliseconds) * time.Millisecond
		return nil
	})
	return ttl, err
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
	if receipt.EarliestPositive != nil {
		ttl, err := contractHoleWitnessTtl(ctx, receipt.EarliestPositive)
		if err != nil {
			_ = InvalidateContractHoleReadiness(ctx)
			return false, err
		}
		receipt.WitnessRemainingTtlMillis = ttl.Milliseconds()
	}
	now = server.NowUtc()
	remaining := receipt.PassStarted.Add(ContractHoleTtl).Sub(now)
	if now.Sub(receipt.PassStarted) >= ContractHoleRefreshInterval || remaining <= 0 ||
		(receipt.EarliestPositive != nil && receipt.WitnessRemainingTtlMillis < remaining.Milliseconds()) {
		return false, InvalidateContractHoleReadiness(ctx)
	}
	receipt.MinimumRemainingTtlMillis = remaining.Milliseconds()
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return false, err
	}
	err = server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		return client.Eval(ctx, `redis.call('SET',KEYS[1],ARGV[1],'PXAT',ARGV[2]); return 1`, []string{contractHoleReadinessKey},
			encoded, receipt.PassStarted.Add(ContractHoleTtl).UnixMilli()).Err()
	})
	return err == nil, err
}

// Loss of coverage closes the rollout gate without changing any pair's counter.
func InvalidateContractHoleReadiness(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, contractHoleRedisTimeout)
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
	if !validContractHoleReadiness(receipt, server.NowUtc()) || receipt.MinimumRemainingTtlMillis <= 0 || receipt.MinimumRemainingTtlMillis > ContractHoleTtl.Milliseconds() {
		return nil, errors.New("contract hole readiness is stale or incomplete")
	}
	if receipt.EarliestPositive != nil {
		ttl, err := contractHoleWitnessTtl(ctx, receipt.EarliestPositive)
		if err != nil {
			return nil, err
		}
		receipt.WitnessRemainingTtlMillis = ttl.Milliseconds()
	}
	receipt.MinimumRemainingTtlMillis = receipt.PassStarted.Add(ContractHoleTtl).Sub(server.NowUtc()).Milliseconds()
	if receipt.MinimumRemainingTtlMillis <= 0 ||
		(receipt.EarliestPositive != nil && receipt.WitnessRemainingTtlMillis < receipt.MinimumRemainingTtlMillis) {
		return nil, errors.New("contract hole readiness lost its expiry margin")
	}
	return receipt, nil
}
