package model

// §7.9's append limits (D-TEAM-19; §20.4's pro.yml keys): per network, api
// takes at most config_statements_per_min statements a minute in bursts to
// config_statements_burst and config_statements_per_day a day, and
// config_bases_per_min bases a minute and config_bases_per_day a day, counted
// in Redis {acl_<networkId>}q, a hash in {acl_}v's hash tag that lives 25 h
// past its last write, and answers 429 {retry_after_ms} beyond them.
//
//	buckets   the minute limit a GCRA, a theoretical arrival time in µs of the
//	          Redis master's clock, so every api instance counts against one
//	          clock, its burst the tolerance; the day limit a count per UTC day
//	          of that clock, the key's 25 h outliving its day; a statement passes
//	          both together or charges neither, and a base its two
//	exempt    revocations of every kind, Suspend, BridgeWithdraw and
//	          RecoveryVeto never count, so admins revoke a device that spent
//	          the day's budget at once; ticks never count (AppendTick charges
//	          nothing), nor do reseed uploads (§19.10, not built)
//	repeat    an exempt revocation whose subject is already so with no
//	          stronger reason (compromise over lost or removed, any of the
//	          three over reaped, a since_n over none, an earlier since_n)
//	          answers that entry's {n, A_n} and appends nothing, which bounds
//	          the exempt appends by the subjects admissions create
//	          (chargeStatement)
//
// The limiter decides nothing about a statement's validity: a statement it
// admits is still verified at the tip, and a refused one appends nothing.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// Charges §7.9's append limits. Safe for concurrent use.
type ConfigLogLimiter interface {
	// One statement of the network, or a *ConfigLogLimitError.
	ChargeStatement(ctx context.Context, networkId server.Id) error
	// One base of the network, or a *ConfigLogLimitError.
	ChargeBase(ctx context.Context, networkId server.Id) error
}

// Tunables of the limiter; the limits themselves are pro.yml's (§20.4).
type ConfigLogLimitSettings struct {
	// {acl_}q lives this long after its last write: 25 h (§2.8 api-7), past
	// the day bucket's whole span.
	KeyTtl time.Duration
	// How long one charge waits for Redis before it answers an error.
	RedisTimeout time.Duration
}

func DefaultConfigLogLimitSettings() *ConfigLogLimitSettings {
	return &ConfigLogLimitSettings{
		KeyTtl:       25 * time.Hour,
		RedisTimeout: time.Second,
	}
}

// {acl_<networkId>}q, in {acl_}v's hash tag.
func configLogLimitKey(networkId server.Id) string {
	return "{acl_" + networkId.String() + "}q"
}

// {acl_}q's fields: each minute bucket's theoretical arrival time, and each
// day's count, the day's number appended (sd:<day>).
const (
	configLogLimitFieldStatementMinute = "sm"
	configLogLimitFieldStatementDay    = "sd"
	configLogLimitFieldBaseMinute      = "bm"
	configLogLimitFieldBaseDay         = "bd"
)

// An append past a limit: 429 with Retry-After and {"error", "code",
// "retry_after_ms"}.
type ConfigLogLimitError struct {
	RetryAfterMs uint32
}

func (self *ConfigLogLimitError) Error() string {
	return fmt.Sprintf("%d The network's log is at its append limit.", http.StatusTooManyRequests)
}

// Whole seconds, rounded up.
func (self *ConfigLogLimitError) RetryAfterSeconds() int {
	return int((int64(self.RetryAfterMs) + 999) / 1000)
}

// The coded body with the wait.
func (self *ConfigLogLimitError) HttpErrorResultBody() any {
	return map[string]any{
		"error":          "The network's log is at its append limit.",
		"code":           "rate_limited",
		"retry_after_ms": self.RetryAfterMs,
	}
}

// A minute bucket: its emission interval and tolerance in µs. An interval of 0
// is no limit.
type configLogBucket struct {
	field     string
	interval  int64
	tolerance int64
}

// The bucket of count a minute in bursts to burst, from pro.yml: -1 is no
// limit, and 0 or another value below it admits nothing (an interval past any
// day).
func newConfigLogBucket(field string, count int64, burst int64) configLogBucket {
	if count == -1 {
		return configLogBucket{field: field}
	}
	if count <= 0 {
		return configLogBucket{field: field, interval: math.MaxInt64 / 4, tolerance: 0}
	}
	interval := max(int64(time.Minute/time.Microsecond)/count, 1)
	if burst < 1 {
		burst = 1
	}
	return configLogBucket{field: field, interval: interval, tolerance: (burst - 1) * interval}
}

// A day's count: -1 is no limit, and 0 or another value below it admits
// nothing.
type configLogDayCount struct {
	field string
	limit int64
}

// KEYS[1] is {acl_}q. ARGV[1] is now in µs, or -1 for the Redis master's clock;
// ARGV[2..4] the minute bucket, its field, interval and tolerance in µs;
// ARGV[5..6] the day's count, its field and limit; ARGV[7] the key's ttl in
// seconds. Both admit, and the bucket moves one interval on and the day's count
// one up, or the call answers the longer wait and changes nothing: the
// bucket's, or the day's until the next UTC midnight. Returns {1, '0'} or {0,
// wait in µs}. Times are written with %.0f, since Lua's default format keeps 14
// digits and a µs time has 16.
const configLogLimitScriptSource = `
local now = tonumber(ARGV[1])
if now < 0 then
    local time = redis.call('TIME')
    now = tonumber(time[1]) * 1000000 + tonumber(time[2])
end
local tat = tonumber(redis.call('HGET', KEYS[1], ARGV[2]))
if tat == nil or tat < now then
    tat = now
end
local wait = tat - tonumber(ARGV[4]) - now
local dayMicros = 86400000000
local day = math.floor(now / dayMicros)
local dayField = ARGV[5] .. ':' .. string.format('%.0f', day)
local dayLimit = tonumber(ARGV[6])
local dayCount = tonumber(redis.call('HGET', KEYS[1], dayField) or '0')
if dayLimit ~= -1 and dayLimit <= dayCount then
    wait = math.max(wait, (day + 1) * dayMicros - now)
end
if 0 < wait then
    return {0, string.format('%.0f', wait)}
end
redis.call('HSET', KEYS[1], ARGV[2], string.format('%.0f', tat + tonumber(ARGV[3])))
redis.call('HINCRBY', KEYS[1], dayField, 1)
redis.call('HDEL', KEYS[1], ARGV[5] .. ':' .. string.format('%.0f', day - 1), ARGV[5] .. ':' .. string.format('%.0f', day - 2))
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[7]))
return {1, '0'}
`

var configLogLimitScript = redis.NewScript(configLogLimitScriptSource)

// The limiter on server's Redis, with each network's limits from pro.yml and
// its rows (NetworkSidecarLimits).
type configLogRedisLimiter struct {
	settings *ConfigLogLimitSettings
	// µs since the epoch for a test's clock; nil is the Redis master's
	nowMicros func() int64
	// the network's limits: NetworkSidecarLimits, or a test's
	limits func(ctx context.Context, networkId server.Id) *ProSidecarLimits
}

func NewConfigLogRedisLimiter(settings *ConfigLogLimitSettings) ConfigLogLimiter {
	if settings == nil {
		settings = DefaultConfigLogLimitSettings()
	}
	return &configLogRedisLimiter{settings: settings, limits: NetworkSidecarLimits}
}

// NewConfigLogRedisLimiter counting on now's clock in place of the Redis master's, for a test whose statements
// are signed on a fake clock, so its day turns only when the test moves it.
func Testing_NewConfigLogRedisLimiterAt(settings *ConfigLogLimitSettings, now func() time.Time) ConfigLogLimiter {
	limiter := NewConfigLogRedisLimiter(settings).(*configLogRedisLimiter)
	limiter.nowMicros = func() int64 {
		return now().UnixMicro()
	}
	return limiter
}

func (self *configLogRedisLimiter) ChargeStatement(ctx context.Context, networkId server.Id) error {
	limits := self.limits(ctx, networkId)
	return self.charge(
		ctx,
		networkId,
		newConfigLogBucket(configLogLimitFieldStatementMinute, limits.Limit("config_statements_per_min"), limits.Limit("config_statements_burst")),
		configLogDayCount{field: configLogLimitFieldStatementDay, limit: limits.Limit("config_statements_per_day")},
	)
}

func (self *configLogRedisLimiter) ChargeBase(ctx context.Context, networkId server.Id) error {
	limits := self.limits(ctx, networkId)
	return self.charge(
		ctx,
		networkId,
		newConfigLogBucket(configLogLimitFieldBaseMinute, limits.Limit("config_bases_per_min"), 1),
		configLogDayCount{field: configLogLimitFieldBaseDay, limit: limits.Limit("config_bases_per_day")},
	)
}

func (self *configLogRedisLimiter) charge(ctx context.Context, networkId server.Id, minute configLogBucket, day configLogDayCount) error {
	nowMicros := int64(-1)
	if self.nowMicros != nil {
		nowMicros = self.nowMicros()
	}
	boundedCtx, cancel := context.WithTimeout(ctx, self.settings.RedisTimeout)
	defer cancel()
	var values []any
	err := server.RedisWithDeadline(boundedCtx, func(r server.RedisClient) error {
		var err error
		dayLimit := day.limit
		if dayLimit < -1 {
			dayLimit = 0
		}
		values, err = configLogLimitScript.Run(
			boundedCtx,
			r,
			[]string{configLogLimitKey(networkId)},
			nowMicros,
			minute.field, minute.interval, minute.tolerance,
			day.field, dayLimit,
			int64(self.settings.KeyTtl/time.Second),
		).Slice()
		return err
	})
	if err != nil {
		return &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: fmt.Errorf("append limits: %w", err)}
	}
	if len(values) != 2 {
		return &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: fmt.Errorf("append limits answered %v", values)}
	}
	admitted, _ := values[0].(int64)
	if admitted == 1 {
		return nil
	}
	waitText, _ := values[1].(string)
	var waitMicros int64
	if _, err := fmt.Sscan(waitText, &waitMicros); err != nil || waitMicros <= 0 {
		waitMicros = 1000
	}
	retryAfterMs := min((waitMicros+999)/1000, int64(math.MaxUint32))
	return &ConfigLogLimitError{RetryAfterMs: uint32(retryAfterMs)}
}

// The exempt statement types (§7.9's limits row).
var configLogExemptTypes = map[bridge.SignedType]bool{
	bridge.SignedType_SIGNED_SIDECAR_REVOKE:    true,
	bridge.SignedType_SIGNED_MEMBER_REVOKE:     true,
	bridge.SignedType_SIGNED_DELEGATION_REVOKE: true,
	bridge.SignedType_SIGNED_KEY_REVOKE:        true,
	bridge.SignedType_SIGNED_GRANT_REVOKE:      true,
	bridge.SignedType_SIGNED_SUSPEND:           true,
	bridge.SignedType_SIGNED_BRIDGE_WITHDRAW:   true,
	bridge.SignedType_SIGNED_RECOVERY_VETO:     true,
}

// The strength of a SidecarRevoke's reason for §7.9's repeat rule: compromise
// over lost or removed, any of the three over reaped.
func configLogRevokeReasonStrength(reason bridge.SidecarRevokeReason) int {
	switch reason {
	case bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_COMPROMISE:
		return 3
	case bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_LOST, bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED:
		return 2
	case bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REAPED:
		return 1
	}
	return 0
}

// Whether a since_n is stronger than one already in force: one over none, an
// earlier over a later.
func configLogSinceStronger(sinceN uint64, existingSinceN uint64) bool {
	return 0 < sinceN && (existingSinceN == 0 || sinceN < existingSinceN)
}

// Charges a statement once, before its append: an exempt one charges nothing,
// and one repeating a revocation already in force answers that entry's pin; any
// other is charged against the limits. Runs under the network's lock, with its
// verifier at the tip.
func (self *ConfigLogSequencer) chargeStatement(ctx context.Context, networkId server.Id, network *configLogNetwork, signedBytes []byte) (*bridge.AuthPin, error) {
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedBytes, signed); err != nil {
		// the verifier took it, so this does not happen; it counts like any
		return nil, self.chargeStatementLimit(ctx, networkId)
	}
	signedType := bridge.SignedType(signed.GetType())
	if !configLogExemptTypes[signedType] {
		return nil, self.chargeStatementLimit(ctx, networkId)
	}
	existingN, repeats := configLogRepeatedRevocation(ctx, self, networkId, network, signedType, signed.GetBody())
	if !repeats {
		return nil, nil
	}
	pin, err := self.pinAt(ctx, networkId, existingN)
	if err != nil {
		return nil, &ConfigLogError{Status: http.StatusServiceUnavailable, Code: "unavailable", Tip: network.log.Pin(), Err: err}
	}
	return pin, nil
}

func (self *ConfigLogSequencer) chargeStatementLimit(ctx context.Context, networkId server.Id) error {
	if self.settings.Limiter == nil {
		return nil
	}
	return self.settings.Limiter.ChargeStatement(ctx, networkId)
}

// The pin of the entry the store holds at n.
func (self *ConfigLogSequencer) pinAt(ctx context.Context, networkId server.Id, n uint64) (*bridge.AuthPin, error) {
	entryBytes, found, err := configLogGet(ctx, self.cluster, sidecar.ConfigEntryKey(sidecar.Id(networkId), n))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("entry %d is not held", n)
	}
	entry := &bridge.AuthEntry{}
	if err := proto.Unmarshal(entryBytes, entry); err != nil {
		return nil, err
	}
	chain, err := bridge.AuthChainDigest(entry.GetN(), entry.GetPrevChain(), entry.GetKind(), entry.GetTimeMs(), entry.GetBody())
	if err != nil {
		return nil, err
	}
	return &bridge.AuthPin{Version: n, Chain: chain[:]}, nil
}

// The statement of type signedType the store holds at n, and whether it is one.
func (self *ConfigLogSequencer) statementAt(ctx context.Context, networkId server.Id, n uint64, signedType bridge.SignedType) ([]byte, bool) {
	entryBytes, found, err := configLogGet(ctx, self.cluster, sidecar.ConfigEntryKey(sidecar.Id(networkId), n))
	if err != nil || !found {
		return nil, false
	}
	entry := &bridge.AuthEntry{}
	if proto.Unmarshal(entryBytes, entry) != nil {
		return nil, false
	}
	switch entry.GetKind() {
	case bridge.AuthEntryKindStatement:
		signed := &bridge.Signed{}
		if proto.Unmarshal(entry.GetBody(), signed) != nil || bridge.SignedType(signed.GetType()) != signedType {
			return nil, false
		}
		return signed.GetBody(), true
	}
	return nil, false
}

// Whether an exempt revocation repeats one already in force with no stronger
// reason, and the position of the entry in force. Built for SidecarRevoke,
// KeyRevoke and DelegationRevoke, whose subjects' rows name the entry that ended
// them; any other exempt type, and any subject a row does not show ended, is
// appended as new.
func configLogRepeatedRevocation(ctx context.Context, sequencer *ConfigLogSequencer, networkId server.Id, network *configLogNetwork, signedType bridge.SignedType, body []byte) (existingN uint64, repeats bool) {
	state := network.log.State()
	switch signedType {
	case bridge.SignedType_SIGNED_SIDECAR_REVOKE:
		revoke := &bridge.SidecarRevoke{}
		if proto.Unmarshal(body, revoke) != nil {
			return 0, false
		}
		sidecarId, err := sidecar.IdFromBytes(revoke.GetSidecarId())
		if err != nil {
			return 0, false
		}
		row := state.SidecarRow(sidecarId)
		if row == nil || row.GetRevokedAt() == 0 {
			return 0, false
		}
		existingN = row.GetRevokedAt()
		// a tick's reap, or another statement than a SidecarRevoke: reaped
		existingReason := bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REAPED
		existingSinceN := uint64(0)
		if existingBody, ok := sequencer.statementAt(ctx, networkId, existingN, bridge.SignedType_SIGNED_SIDECAR_REVOKE); ok {
			existing := &bridge.SidecarRevoke{}
			if proto.Unmarshal(existingBody, existing) == nil {
				existingReason = existing.GetReason()
				existingSinceN = existing.GetSinceN()
			}
		}
		stronger := configLogRevokeReasonStrength(existingReason) < configLogRevokeReasonStrength(revoke.GetReason()) ||
			configLogSinceStronger(revoke.GetSinceN(), existingSinceN)
		return existingN, !stronger
	case bridge.SignedType_SIGNED_KEY_REVOKE:
		revoke := &protocol.KeyRevoke{}
		if proto.Unmarshal(body, revoke) != nil {
			return 0, false
		}
		row := state.RegisteredKey(revoke.GetKeyId())
		if row == nil || row.GetRevokedAt() == 0 {
			return 0, false
		}
		return row.GetRevokedAt(), true
	case bridge.SignedType_SIGNED_DELEGATION_REVOKE:
		revoke := &protocol.DelegationRevoke{}
		if proto.Unmarshal(body, revoke) != nil {
			return 0, false
		}
		row := state.Delegation(revoke.GetDelegationId())
		if row == nil || row.GetEndedAt() == 0 {
			return 0, false
		}
		existingN = row.GetEndedAt()
		existingSinceN := uint64(0)
		if existingBody, ok := sequencer.statementAt(ctx, networkId, existingN, bridge.SignedType_SIGNED_DELEGATION_REVOKE); ok {
			existing := &protocol.DelegationRevoke{}
			if proto.Unmarshal(existingBody, existing) == nil {
				existingSinceN = existing.GetSinceN()
			}
		}
		return existingN, !configLogSinceStronger(revoke.GetSinceN(), existingSinceN)
	}
	return 0, false
}
