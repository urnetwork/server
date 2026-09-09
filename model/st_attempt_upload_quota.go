// Typed artifact staging consumes a deployment and account byte/count budget.
// Redis owns the fixed UTC-hour clock and the atomic four-counter decision;
// storage permission is never validator eligibility or historical authority.
package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"

	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

// The actual ordinary typed Http handlers and workload admission share this
// object ceiling. It is separate from hourly quota and active memory bounds.
const StAttemptUploadMaximumObjectBytes = 32 * 1024 * 1024

// Explicit deployment capacity, with no fallback to an unlimited upload path.
// Counters fit exactly in Redis Lua numbers; subtraction precedes addition.
type StAttemptUploadBudget struct {
	RequestsPerHour        uint64 `yaml:"requests_per_hour" json:"requests_per_hour"`
	BytesPerHour           uint64 `yaml:"bytes_per_hour" json:"bytes_per_hour"`
	AccountRequestsPerHour uint64 `yaml:"account_requests_per_hour" json:"account_requests_per_hour"`
	AccountBytesPerHour    uint64 `yaml:"account_bytes_per_hour" json:"account_bytes_per_hour"`
}

// YAML's generic uint64 decoder truncates floating-point scalars. Admit each
// exact decimal field before assigning a value; absent/zero fields still leave
// staging disabled through Validate rather than gaining implicit capacities.
func (self *StAttemptUploadBudget) UnmarshalYAML(node *yaml.Node) error {
	if self == nil || node == nil || node.Kind != yaml.MappingNode || len(node.Content) > 8 || len(node.Content)%2 != 0 {
		return errors.New("attempt upload budget must be an explicit bounded field mapping")
	}
	var next StAttemptUploadBudget
	fields := map[string]*uint64{"requests_per_hour": &next.RequestsPerHour, "bytes_per_hour": &next.BytesPerHour, "account_requests_per_hour": &next.AccountRequestsPerHour, "account_bytes_per_hour": &next.AccountBytesPerHour}
	seen := make(map[string]bool, 4)
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str" || fields[key.Value] == nil || seen[key.Value] || value.Kind != yaml.ScalarNode || value.ShortTag() != "!!int" {
			return errors.New("attempt upload budget has an unknown, duplicate or noninteger field")
		}
		parsed, err := strconv.ParseUint(value.Value, 10, 64)
		if err != nil || strconv.FormatUint(parsed, 10) != value.Value {
			return errors.New("attempt upload budget field is not a canonical uint64 decimal")
		}
		seen[key.Value] = true
		*fields[key.Value] = parsed
	}
	*self = next
	return nil
}

// Zero disables staging; a partially specified or overflowing budget refuses.
func (self StAttemptUploadBudget) Validate() error {
	const maximumExactInteger = 9007199254740991
	for _, limit := range []uint64{self.RequestsPerHour, self.BytesPerHour, self.AccountRequestsPerHour, self.AccountBytesPerHour} {
		if limit == 0 || limit > maximumExactInteger {
			return errors.New("attempt upload budget is missing or overflows exact counters")
		}
	}
	if self.AccountRequestsPerHour > self.RequestsPerHour || self.AccountBytesPerHour > self.BytesPerHour {
		return errors.New("attempt upload account budget exceeds deployment budget")
	}
	return nil
}

// All checks precede either write. Refused requests create no account key;
// admitted requests, including bad bodies and retries, consume both budgets.
// A lost response is an uncertain consumed reservation, never permission to
// retry the non-idempotent Redis command. HTTP clients may make a new request.
const stAttemptUploadQuotaScript = `
local now = tonumber(ARGV[6])
if now == -1 then
    local tm = redis.call('TIME')
    now = tonumber(tm[1])
end
local bucket = math.floor(now / 3600)
local size = tonumber(ARGV[1])
local limits = {{tonumber(ARGV[2]), tonumber(ARGV[3])}, {tonumber(ARGV[4]), tonumber(ARGV[5])}}
local values = {}
for i = 1, 2 do
    local old = redis.call('HMGET', KEYS[i], 'bucket', 'requests', 'bytes')
    local count, bytes = 0, 0
    if old[1] or old[2] or old[3] then
        local prior = tonumber(old[1])
        count, bytes = tonumber(old[2]), tonumber(old[3])
        if not prior or not count or not bytes or prior < 0 or prior % 1 ~= 0 or count < 0 or count % 1 ~= 0 or bytes < 0 or bytes % 1 ~= 0 or count > 9007199254740991 or bytes > 9007199254740991 then
            return redis.error_reply('invalid attempt upload quota state')
        end
        if prior > bucket then return redis.error_reply('attempt upload quota clock moved backward') end
        if prior < bucket then count, bytes = 0, 0 end
    end
    if count >= limits[i][1] or bytes > limits[i][2] or size > limits[i][2] - bytes then
        return {0, 3600 - (now % 3600)}
    end
    values[i] = {count + 1, bytes + size}
end
for i = 1, 2 do
    redis.call('HSET', KEYS[i], 'bucket', bucket, 'requests', values[i][1], 'bytes', values[i][2])
    redis.call('EXPIRE', KEYS[i], 7200)
end
return {1, 0}
`

// Both keys share one cluster hash slot; a deployment replacement cannot
// inherit an old contract's counters and a rotating client cannot reset its
// authenticated account's budget. No caller path or raw key enters Redis.
func stAttemptUploadQuotaKeys(deployment StDeploymentKey, accountId server.Id) []string {
	identity := sha256.Sum256([]byte(deployment))
	prefix := fmt.Sprintf("{st_attempt_upload:%x}", identity)
	return []string{prefix + ":global", prefix + ":account:" + accountId.String()}
}

// Uses the real no-retry client. Redis failure fails closed before body or
// storage work; only this infrastructure call's panic contract is translated.
func ReserveStAttemptUpload(ctx context.Context, deployment StDeploymentKey, accountId server.Id, size uint64, budget StAttemptUploadBudget) (resultErr error) {
	if ctx == nil || deployment == "" || accountId == (server.Id{}) || size == 0 || size > budget.AccountBytesPerHour {
		return errors.New("attempt upload quota identity or byte count is invalid")
	}
	if err := errors.Join(ctx.Err(), budget.Validate()); err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				resultErr = fmt.Errorf("attempt upload quota Redis failure: %w", err)
			} else {
				resultErr = errors.New("attempt upload quota Redis failure")
			}
		}
		resultErr = errors.Join(resultErr, ctx.Err())
	}()
	completed := false
	server.RedisDoOnce(ctx, func(client server.RedisClient) {
		resultErr = reserveStAttemptUploadWithClient(ctx, client, stAttemptUploadQuotaKeys(deployment, accountId), size, budget, -1)
		completed = true
	})
	if !completed {
		return errors.New("attempt upload quota reservation did not complete")
	}
	return resultErr
}

// The shared script is tested against real Redis, not an in-memory counter.
// Production always passes -1 for Redis TIME; a private explicit clock seam
// makes bucket rollover/rollback controls independent of wall-clock timing.
func reserveStAttemptUploadWithClient(ctx context.Context, client server.RedisClient, keys []string, size uint64, budget StAttemptUploadBudget, nowSeconds int64) error {
	if ctx == nil || client == nil || len(keys) != 2 || keys[0] == "" || keys[1] == "" || keys[0] == keys[1] || size == 0 || size > budget.AccountBytesPerHour || nowSeconds < -1 || nowSeconds > 9007199254740991 {
		return errors.New("attempt upload quota owner is invalid")
	}
	if err := errors.Join(ctx.Err(), budget.Validate()); err != nil {
		return err
	}
	values, err := client.Eval(ctx, stAttemptUploadQuotaScript, keys, size, budget.RequestsPerHour, budget.BytesPerHour, budget.AccountRequestsPerHour, budget.AccountBytesPerHour, nowSeconds).Slice()
	if err != nil {
		return err
	}
	if len(values) != 2 {
		return errors.New("attempt upload quota returned incomplete result")
	}
	allowed, validAllowed := values[0].(int64)
	retry, validRetry := values[1].(int64)
	if !validAllowed || !validRetry || (allowed != 0 && allowed != 1) || (allowed == 1 && retry != 0) || (allowed == 0 && (retry < 1 || retry > 3600)) {
		return errors.New("attempt upload quota returned invalid result")
	}
	if allowed == 0 {
		return &rateLimitError{message: "429 Attempt upload budget exhausted.", retryAfterSeconds: int(retry)}
	}
	return ctx.Err()
}
