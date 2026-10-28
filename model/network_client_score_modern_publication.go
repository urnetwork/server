// Union pages remain readable by old binaries. A fenced dirty partition keeps
// new readers from trusting a boundary while its mutable pages are changing.
package model

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// One caller payload owns its dirty metadata until commit or supersession.
// The predecessor checksum is retained across retries; a newer owner is never stolen.
type clientScoreModernPublication struct {
	key        string
	generation string
	dirty      []byte
	expected   string
	prepared   bool
}

// Publication identities are compared for equality, never ordered by clocks.
func newClientScoreModernPublication(key string) *clientScoreModernPublication {
	generation := server.NewId().String()
	return &clientScoreModernPublication{
		key: key, generation: generation, dirty: []byte("writing:" + generation),
	}
}

// Capture the predecessor once, including absence. A failed read acquires nothing.
func (self *clientScoreModernPublication) prepare(ctx context.Context, r server.RedisClient) error {
	if self.prepared {
		return nil
	}
	expected, err := r.Get(ctx, self.key).Bytes()
	if err != nil && err != redis.Nil {
		return err
	}
	if err != redis.Nil {
		// Redis exposes SHA-1 for fixed-size internal equality checks, not authentication.
		checksum := sha1.Sum(expected)
		self.expected = hex.EncodeToString(checksum[:])
	}
	self.prepared = true
	return nil
}

// Every mutation rejects an expired queued owner before touching Redis state.
const clientScoreModernExpiryScript = `
local expiresAt = tonumber(ARGV[1])
local now = redis.call('TIME')
if not expiresAt or expiresAt <= tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000) then
  return redis.error_reply('client score export expired')
end
`

// Retain a predecessor's later deadline while dirty, so a failed shorter export
// cannot make metadata disappear before the pages it invalidated. A persistent
// predecessor also leaves persistent dirty metadata until a complete replacement.
const clientScoreModernBeginScript = clientScoreModernExpiryScript + `
local current = redis.call('GET', KEYS[1])
if current == ARGV[3] then return 1 end
local checksum = current and redis.sha1hex(current) or ''
if checksum ~= ARGV[2] then return 0 end
local previousExpiry = redis.call('PEXPIRETIME', KEYS[1])
redis.call('SET', KEYS[1], ARGV[3])
if previousExpiry ~= -1 then
  redis.call('PEXPIREAT', KEYS[1], math.max(expiresAt, previousExpiry))
end
return 1
`

// Check the metadata owner in the same hash slot as each legacy payload key.
const clientScoreModernSetScript = clientScoreModernExpiryScript + `
if redis.call('GET', KEYS[1]) ~= ARGV[2] then return 0 end
redis.call('SET', KEYS[2], ARGV[3], 'PXAT', expiresAt)
return 1
`

// Exact commit replay is safe; a generation in the completed bytes prevents
// equal-content publications from creating an ABA match for an older begin.
const clientScoreModernCommitScript = clientScoreModernExpiryScript + `
local current = redis.call('GET', KEYS[1])
if current == ARGV[3] then return 1 end
if current ~= ARGV[2] then return 0 end
redis.call('SET', KEYS[1], ARGV[3], 'PXAT', expiresAt)
return 1
`

// A dirty begin and completed commit use one key; ordinary payloads use two.
func (self *clientScoreModernPublication) queue(ctx context.Context, pipe redis.Pipeliner, set clientScoreRedisSet, expiresAt time.Time) *redis.Cmd {
	switch {
	case set.modernBegin:
		return pipe.Eval(ctx, clientScoreModernBeginScript, []string{self.key}, expiresAt.UnixMilli(), self.expected, self.dirty)
	case set.key == self.key:
		return pipe.Eval(ctx, clientScoreModernCommitScript, []string{self.key}, expiresAt.UnixMilli(), self.dirty, set.value)
	default:
		return pipe.Eval(ctx, clientScoreModernSetScript, []string{self.key, set.key}, expiresAt.UnixMilli(), self.dirty, set.value)
	}
}

// Lua refusals are successful Redis commands, but failed publication ownership.
func checkClientScoreModernWrite(command *redis.Cmd) error {
	result, err := command.Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return fmt.Errorf("client score modern publisher lost its generation")
	}
	return nil
}

// Include the fixed-size fence retained beside each command's payload budget.
func (self clientScoreRedisSet) retainedBytes() int {
	size := len(self.key) + len(self.value)
	if self.modernPublication != nil {
		size += len(self.modernPublication.key) + len(self.modernPublication.dirty) + 2*sha1.Size
	}
	return size
}
