// Reserved counters are disjoint from ordinary authenticated accounts. One
// historically authenticated hotkey/operator owns its finite allowance across
// activation, VPK, JWT, client, and API-process rotation.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"

	"github.com/urnetwork/server/v2026"
	"gopkg.in/yaml.v3"
)

// Only duplicate-object tries spend RetryRequestsPerHour. Distinct immutable
// objects spend protected ObjectsPerHour/BytesPerHour even when retries are
// exhausted. Same-object idempotency lasts one UTC-hour bucket; all counters
// are explicit and exactly representable by Redis Lua.
type StReservedAttemptUploadBudget struct {
	RetryRequestsPerHour uint64 `yaml:"retry_requests_per_hour" json:"retry_requests_per_hour"`
	ObjectsPerHour       uint64 `yaml:"objects_per_hour" json:"objects_per_hour"`
	BytesPerHour         uint64 `yaml:"bytes_per_hour" json:"bytes_per_hour"`
}

// No implicit quota is enabled by absent, zero, float, or overflowing values.
func (self StReservedAttemptUploadBudget) Validate() error {
	for _, value := range []uint64{self.RetryRequestsPerHour, self.ObjectsPerHour, self.BytesPerHour} {
		if value == 0 || value > 9007199254740991 {
			return errors.New("reserved upload capacity is missing or exceeds exact counters")
		}
	}
	return nil
}

// YAML integer admission precedes assignment; generic uint64 decoding must not
// silently truncate floats or accept aliases as new reservation capacity.
func (self *StReservedAttemptUploadBudget) UnmarshalYAML(node *yaml.Node) error {
	if self == nil || node == nil || node.Kind != yaml.MappingNode || len(node.Content) > 6 || len(node.Content)%2 != 0 {
		return errors.New("reserved upload budget requires an explicit bounded mapping")
	}
	var next StReservedAttemptUploadBudget
	fields := map[string]*uint64{"retry_requests_per_hour": &next.RetryRequestsPerHour, "objects_per_hour": &next.ObjectsPerHour, "bytes_per_hour": &next.BytesPerHour}
	seen := make(map[string]bool, 3)
	for index := 0; index < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		if key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str" || fields[key.Value] == nil || seen[key.Value] || value.Kind != yaml.ScalarNode || value.ShortTag() != "!!int" {
			return errors.New("reserved upload budget has duplicate, unknown or noninteger fields")
		}
		number, err := strconv.ParseUint(value.Value, 10, 64)
		if err != nil || strconv.FormatUint(number, 10) != value.Value {
			return errors.New("reserved upload budget is not canonical uint64 decimal")
		}
		seen[key.Value] = true
		*fields[key.Value] = number
	}
	*self = next
	return nil
}

// This value is supplied only after the controller's real historical chain
// authentication. It is a quota namespace, not a serializable authorization.
type StReservedAttemptUploadOwner struct {
	Hotkey       [32]byte
	OperatorNoID uint64
}

// No mutable API account, native UID, VPK revision, or activation generation
// creates another quota owner. Both keys remain in one exact cluster slot.
func stReservedAttemptUploadKeys(deployment StDeploymentKey, replicaNoID uint64, owner StReservedAttemptUploadOwner) []string {
	data := append([]byte("urnetwork/reserved-upload-owner/v1\x00"), []byte(deployment)...)
	data = append(data, 0)
	data = binary.BigEndian.AppendUint64(data, replicaNoID)
	data = append(data, owner.Hotkey[:]...)
	data = binary.BigEndian.AppendUint64(data, owner.OperatorNoID)
	hash := sha256.Sum256(data)
	prefix := fmt.Sprintf("{st_reserved_attempt_upload:%x}", hash)
	return []string{prefix + ":counters", prefix + ":objects"}
}

// Malformed state, stale intents, or exhausted counters write nothing. Redis
// TIME owns the production clock. The object map is bounded by ObjectsPerHour
// and is never extended by idempotent retries. Retry exhaustion cannot debit
// or block a later distinct object's protected count/byte allocation.
const stReservedAttemptUploadQuotaScript = `
local now = tonumber(ARGV[7])
if now == -1 then
    local tm = redis.call('TIME')
    now = tonumber(tm[1])
end
local size, retryLimit, objectsLimit, bytesLimit = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
local notAfter, maximumLifetime = tonumber(ARGV[5]), tonumber(ARGV[6])
if now >= notAfter or notAfter - now > maximumLifetime then
    return redis.error_reply('reserved upload intent expired or exceeds trusted lifetime')
end
local bucket = math.floor(now / 3600)
local prior = redis.call('HMGET', KEYS[1], 'bucket', 'retries', 'objects', 'bytes')
local oldBucket, retries, objects, bytes = bucket, 0, 0, 0
local reset = false
if prior[1] or prior[2] or prior[3] or prior[4] then
    oldBucket, retries, objects, bytes = tonumber(prior[1]), tonumber(prior[2]), tonumber(prior[3]), tonumber(prior[4])
    for _, value in ipairs({oldBucket, retries, objects, bytes}) do
        if not value or value < 0 or value % 1 ~= 0 or value > 9007199254740991 then
            return redis.error_reply('invalid reserved upload quota state')
        end
    end
    if not oldBucket or not retries or not objects or not bytes then return redis.error_reply('incomplete reserved upload quota state') end
    if redis.call('HLEN', KEYS[2]) ~= objects then return redis.error_reply('reserved upload object census differs') end
    if oldBucket > bucket then return redis.error_reply('reserved upload quota clock moved backward') end
    if oldBucket < bucket then reset = true; retries, objects, bytes = 0, 0, 0 end
elseif redis.call('HLEN', KEYS[2]) ~= 0 then
    return redis.error_reply('orphaned reserved upload object census')
end
local priorSize = false
if not reset then priorSize = redis.call('HGET', KEYS[2], ARGV[8]) end
local fresh = 1
if priorSize then
    local parsed = tonumber(priorSize)
    if not parsed or parsed ~= size then return redis.error_reply('reserved upload object size differs') end
    fresh = 0
end
if bytes > bytesLimit or (fresh == 0 and retries >= retryLimit) or (fresh == 1 and (objects >= objectsLimit or size > bytesLimit - bytes)) then
    return {0, 0, 3600 - (now % 3600)}
end
if reset then redis.call('DEL', KEYS[2]) end
if fresh == 1 then
    objects, bytes = objects + 1, bytes + size
    redis.call('HSET', KEYS[2], ARGV[8], ARGV[1])
end
if fresh == 0 then retries = retries + 1 end
redis.call('HSET', KEYS[1], 'bucket', bucket, 'retries', retries, 'objects', objects, 'bytes', bytes)
redis.call('EXPIRE', KEYS[1], 7200)
redis.call('EXPIRE', KEYS[2], 7200)
return {1, fresh, 0}
`

// Unknown Redis outcomes stay errors; the no-retry call cannot silently spend
// twice. The same object may later retry under its separate retry bound; this
// cannot spend or block fresh-object minima. Actual public storage/readback
// still precedes every successful HTTP acknowledgement.
func ReserveStReservedAttemptUpload(ctx context.Context, deployment StDeploymentKey, replicaNoID uint64, owner StReservedAttemptUploadOwner, object [32]byte, size, notAfter, maximumLifetime uint64, budget StReservedAttemptUploadBudget) (fresh bool, resultErr error) {
	if ctx == nil || deployment == "" || replicaNoID == 0 || owner.Hotkey == ([32]byte{}) || owner.OperatorNoID == 0 {
		return false, errors.New("reserved upload owner is incomplete")
	}
	if err := errors.Join(ctx.Err(), budget.Validate()); err != nil {
		return false, err
	}
	if object == ([32]byte{}) || size == 0 || size > budget.BytesPerHour || notAfter == 0 || notAfter > 9007199254740991 || maximumLifetime == 0 || maximumLifetime > 3600 {
		return false, errors.New("reserved upload object or lifetime exceeds trusted limits")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				resultErr = fmt.Errorf("reserved upload Redis failure: %w", err)
			} else {
				resultErr = errors.New("reserved upload Redis failure")
			}
		}
		resultErr = errors.Join(resultErr, ctx.Err())
		if resultErr != nil {
			fresh = false
		}
	}()
	completed := false
	server.RedisDoOnce(ctx, func(client server.RedisClient) {
		fresh, resultErr = reserveStReservedAttemptUploadWithClient(ctx, client, stReservedAttemptUploadKeys(deployment, replicaNoID, owner), object, size, notAfter, maximumLifetime, budget, -1)
		completed = true
	})
	if !completed {
		return false, errors.New("reserved upload reservation did not complete")
	}
	return fresh, resultErr
}

// Only the private test seam accepts an explicit clock; production always
// selects Redis TIME. Real Redis executes every mutation and atomic decision.
func reserveStReservedAttemptUploadWithClient(ctx context.Context, client server.RedisClient, keys []string, object [32]byte, size, notAfter, maximumLifetime uint64, budget StReservedAttemptUploadBudget, now int64) (bool, error) {
	if ctx == nil || client == nil || len(keys) != 2 || keys[0] == "" || keys[1] == "" || keys[0] == keys[1] || object == ([32]byte{}) ||
		size == 0 || size > budget.BytesPerHour || notAfter == 0 || notAfter > 9007199254740991 || maximumLifetime == 0 || maximumLifetime > 3600 || now < -1 || now > 9007199254740991 {
		return false, errors.New("reserved upload counter owner is invalid")
	}
	if err := errors.Join(ctx.Err(), budget.Validate()); err != nil {
		return false, err
	}
	values, err := client.Eval(ctx, stReservedAttemptUploadQuotaScript, keys, size, budget.RetryRequestsPerHour, budget.ObjectsPerHour, budget.BytesPerHour, notAfter, maximumLifetime, now, fmt.Sprintf("%x", object)).Slice()
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if len(values) != 3 {
		return false, errors.New("reserved upload counter reply is incomplete")
	}
	allowed, aok := values[0].(int64)
	fresh, fok := values[1].(int64)
	retry, rok := values[2].(int64)
	if !aok || !fok || !rok || (allowed != 0 && allowed != 1) || (fresh != 0 && fresh != 1) ||
		(allowed == 1 && retry != 0) || (allowed == 0 && (fresh != 0 || retry < 1 || retry > 3600)) {
		return false, errors.New("reserved upload counter reply is invalid")
	}
	if allowed == 0 {
		return false, &rateLimitError{message: "429 Validator reserved upload budget exhausted.", retryAfterSeconds: int(retry)}
	}
	return fresh == 1, nil
}
