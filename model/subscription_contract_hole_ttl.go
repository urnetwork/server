package model

// Periodic renewal owns only key expiry. Lifecycle and explicit initialization
// own membership; observing a missing or invalid projection never recreates it.

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
)

// Read-only validation includes live membership without pruning expired rows.
// Both renewal and its readiness witness use this view, so neither can change
// the counter indirectly through the packet reader's expiry cleanup.
const contractHoleInspectFunction = `
local function inspect()
    local value = redis.call('GET', KEYS[1])
    local ttl = redis.call('PTTL', KEYS[1])
    if not value or ttl <= 0 then return false end
    if ttl > tonumber(ARGV[2]) then return redis.error_reply('invalid contract hole expiry') end
    if value ~= '0' and not string.match(value, '^[1-9]%d*$') then return redis.error_reply('invalid contract hole count') end
    local count = tonumber(value)
    if not count or count > tonumber(ARGV[1]) then return redis.error_reply('invalid contract hole count') end
    if count == 0 then return {0, ttl, '0'} end
    local memberttl = redis.call('PTTL', KEYS[2])
    if memberttl <= 0 or memberttl > tonumber(ARGV[2]) then return redis.error_reply('invalid contract hole member expiry') end
    if redis.call('ZCARD', KEYS[2]) ~= count then return redis.error_reply('invalid contract hole membership') end
    if redis.call('EXISTS', KEYS[5]) ~= 0 then return redis.error_reply('oversized contract hole') end
    local now = redis.call('TIME')
    local nowms = tonumber(now[1])*1000 + math.floor(tonumber(now[2])/1000)
    local last = redis.call('ZREVRANGE', KEYS[2], 0, 0, 'WITHSCORES')
    if last[2] ~= 'inf' and last[2] ~= '+inf' then
        local deadline = tonumber(last[2])
        if not deadline or deadline ~= math.floor(deadline) or deadline >= 9223372036854775807 then
            return redis.error_reply('invalid contract hole deadline')
        end
        if deadline <= nowms then return {0, math.min(ttl, memberttl), '0'} end
    end
    return {count, math.min(ttl, memberttl), last[2]}
end
`

// Only these two PEXPIRE operations mutate projection state. Redis serializes
// validation and renewal with creates/closes; PEXPIRE cannot recreate a key.
const contractHoleRefreshTtlScript = contractHoleInspectFunction + `
local observed = inspect()
if not observed then return {0, 0} end
if observed.err then return observed end
if observed[1] == 0 then return {0, 0} end
local countRenewed = redis.call('PEXPIRE', KEYS[1], ARGV[2])
local membersRenewed = redis.call('PEXPIRE', KEYS[2], ARGV[2])
if countRenewed ~= 1 or membersRenewed ~= 1 then return {0, 0} end
return {1, observed[1]}
`

// The caller already selected an eligible pair. This operation performs no
// source query, membership mutation or repair, including on missing/error state.
func refreshContractHoleTtl(ctx context.Context, sourceClientId, destinationClientId server.Id) (bool, int, error) {
	ctx, cancel := context.WithTimeout(ctx, contractHoleRedisTimeout)
	defer cancel()
	var values []any
	err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		var err error
		values, err = client.Eval(ctx, contractHoleRefreshTtlScript, contractHoleKeys(sourceClientId, destinationClientId),
			contractHoleMemberLimit, ContractHoleTtl.Milliseconds()).Slice()
		return err
	})
	if err != nil {
		return false, 0, err
	}
	if len(values) != 2 {
		return false, 0, errors.New("invalid contract hole ttl result")
	}
	refreshed, refreshedOk := values[0].(int64)
	count, countOk := values[1].(int64)
	if !refreshedOk || !countOk || count < 0 || count > contractHoleMemberLimit || (refreshed != 0 && refreshed != 1) {
		return false, 0, errors.New("invalid contract hole ttl result")
	}
	return refreshed == 1, int(count), nil
}

// Readiness observes the same deadline and TTL bounds as packet authorization,
// but it cannot perform the packet reader's lazy expiry decrements/deletes.
func inspectContractHoleLease(ctx context.Context, sourceClientId, destinationClientId server.Id) (ContractHoleStatus, time.Time, error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, contractHoleRedisTimeout)
	defer cancel()
	var values []any
	err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		var err error
		values, err = client.Eval(ctx, contractHoleInspectFunction+`return inspect()`, contractHoleKeys(sourceClientId, destinationClientId),
			contractHoleMemberLimit, ContractHoleTtl.Milliseconds()).Slice()
		return err
	})
	if errors.Is(err, server.RedisNil) {
		return ContractHoleUnknown, time.Time{}, nil
	}
	if err != nil {
		return ContractHoleUnknown, time.Time{}, err
	}
	return contractHoleLease(values, started, time.Now())
}
