// Shared caller-ip rate limiting and infrastructure exclusions.
package server

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"
)

const rateLimitExcludeSubnetsEnv = "WARP_LIMIT_EXCLUDE_SUBNETS"

var defaultRateLimitExcludePrefixes = sync.OnceValue(loadRateLimitExcludePrefixes)

// Describes the normalized identity used by every caller-ip limiter.
//
// Construction parses the source address, checks the deployment exclusions,
// and computes the privacy-preserving address hash only for callers that will
// actually be counted. Methods in this file check excluded before invoking
// their storage operation, so infrastructure traffic never consumes a budget.
type RateLimitClient struct {
	clientAddr      netip.Addr
	clientIpHash    [32]byte
	clientIpHashHex string
	excluded        bool
}

// Defines one fixed, non-overlapping Redis rate window.
type RateLimitWindowSettings struct {
	Namespace string
	Name      string
	Duration  time.Duration
	Limit     int64
}

// Reports the decision and counter value for one caller.
type RateLimitResult struct {
	Allowed  bool
	Excluded bool
	Count    int64
}

// Configures a sliding caller-address history and an optional global identity
// history. Exclusions remove only the address history.
type IpRateLimitAttemptSettings struct {
	KeyPrefix       string
	AddressLookback time.Duration
	AddressLimit    int
	GlobalLookback  time.Duration
	GlobalLimit     int
}

// Identifies the histories changed by an attempt without retaining raw caller
// or account identifiers.
type IpRateLimitAttemptId struct {
	AddressRedisKey string
	GlobalRedisKey  string
}

// Reports the database state of a seedphrase account-creation check.
type NetworkCreateIpRateLimitResult struct {
	Count             int
	RetryAfterSeconds int
}

// Configures Connect's burst and concurrent-connection budgets.
type ConnectionRateLimitSettings struct {
	BurstDuration        time.Duration
	BurstConnectionCount int

	// Linear delay per connection above the burst allowance.
	BurstConnectionDelay time.Duration

	// Concurrent connections allowed for one ip and handler.
	MaxTotalConnectionCount int
	TotalConnectionDelay    time.Duration
	TotalExpiration         time.Duration
}

// Owns the counters for one Connect admission attempt.
type ConnectionRateLimit struct {
	ctx       context.Context
	client    *RateLimitClient
	handlerId Id
	settings  *ConnectionRateLimitSettings
}

type rateLimitWindowCounter func(
	ctx context.Context,
	key string,
	duration time.Duration,
) (int64, error)

// Atomically records one sliding-window attempt. Key position is conditional:
// excluded callers omit the address key but still use the global key when the
// route supplied an identity scope.
var ipRateLimitAttemptScript = redis.NewScript(`
local has_address = tonumber(ARGV[1])
local has_global = tonumber(ARGV[2])
local now_ms = tonumber(ARGV[3])
local address_expiry_ms = tonumber(ARGV[4])
local global_expiry_ms = tonumber(ARGV[5])
local member = ARGV[6]
local address_limit = tonumber(ARGV[7])
local global_limit = tonumber(ARGV[8])
local address_ttl_ms = tonumber(ARGV[9])
local global_ttl_ms = tonumber(ARGV[10])

local key_index = 1
local address_count = 0
if has_address == 1 then
    local address_key = KEYS[key_index]
    key_index = key_index + 1
    redis.call('ZREMRANGEBYSCORE', address_key, '-inf', now_ms)
    redis.call('ZADD', address_key, address_expiry_ms, member)
    address_count = redis.call('ZCARD', address_key)
    if address_limit < address_count then
        redis.call('ZREMRANGEBYRANK', address_key, 0, address_count - address_limit - 1)
        address_count = address_limit
    end
    redis.call('PEXPIRE', address_key, address_ttl_ms)
end

local global_count = 0
if has_global == 1 then
    local global_key = KEYS[key_index]
    redis.call('ZREMRANGEBYSCORE', global_key, '-inf', now_ms)
    redis.call('ZADD', global_key, global_expiry_ms, member)
    global_count = redis.call('ZCARD', global_key)
    if global_limit < global_count then
        redis.call('ZREMRANGEBYRANK', global_key, 0, global_count - global_limit - 1)
        global_count = global_limit
    end
    redis.call('PEXPIRE', global_key, global_ttl_ms)
end

if (has_address == 0 or address_count < address_limit) and (has_global == 0 or global_count < global_limit) then
    return 1
end
return 0
`)

// Removes histories after a successful authenticated operation.
var ipRateLimitAttemptSuccessScript = redis.NewScript(`
for _, key in ipairs(KEYS) do
    redis.call('DEL', key)
end
return 1
`)

// Returns the production Connect admission limits.
func DefaultConnectionRateLimitSettings() *ConnectionRateLimitSettings {
	return &ConnectionRateLimitSettings{
		BurstDuration:           60 * time.Second,
		BurstConnectionCount:    200,
		BurstConnectionDelay:    500 * time.Millisecond,
		MaxTotalConnectionCount: 200,
		TotalConnectionDelay:    30 * time.Second,
		TotalExpiration:         7 * 24 * time.Hour,
	}
}

// Parses the semicolon-separated deployment setting without consulting global
// process state. Keeping parsing separate makes both address families and bad
// configuration deterministic to test.
func parseRateLimitExcludePrefixes(value string) ([]netip.Prefix, error) {
	prefixes := []netip.Prefix{}
	for _, subnet := range strings.Split(value, ";") {
		subnet = strings.TrimSpace(subnet)
		if subnet == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(subnet)
		if err != nil {
			return nil, fmt.Errorf("invalid rate-limit exclusion %q: %w", subnet, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

// Loads exclusions once because Warp supplies them as immutable process
// configuration. Invalid infrastructure configuration fails process startup.
func loadRateLimitExcludePrefixes() []netip.Prefix {
	prefixes, err := parseRateLimitExcludePrefixes(os.Getenv(rateLimitExcludeSubnetsEnv))
	if err != nil {
		panic(err)
	}
	glog.Infof("[ratelimit]found exclude prefixes=%s\n", prefixes)
	return prefixes
}

// Returns a copy of the configured exclusions for diagnostics and tests.
func RateLimitExcludePrefixes() []netip.Prefix {
	return append([]netip.Prefix{}, defaultRateLimitExcludePrefixes()...)
}

// Reports whether an address belongs to infrastructure excluded from caller
// rate limits. IPv4-mapped IPv6 is normalized before matching.
func IsRateLimitExcluded(addr netip.Addr) bool {
	return isRateLimitExcluded(addr, defaultRateLimitExcludePrefixes())
}

// Reports membership against an explicit exclusion set without loading
// process configuration or computing the caller's keyed address hash.
func isRateLimitExcluded(addr netip.Addr, prefixes []netip.Prefix) bool {
	addr = addr.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// Classifies one client address using the process-wide exclusion setting.
func NewRateLimitClient(clientAddress string) (*RateLimitClient, error) {
	return newRateLimitClient(clientAddress, defaultRateLimitExcludePrefixes())
}

// Classifies a raw ip when no source port is available.
func NewRateLimitClientIp(clientIp string) (*RateLimitClient, error) {
	return newRateLimitClientIp(clientIp, defaultRateLimitExcludePrefixes())
}

// Reconstructs non-excluded ownership from a persisted privacy-preserving
// hash. Exclusion cannot be inferred after the raw address has been discarded.
func NewStoredRateLimitClient(clientIpHash [32]byte) *RateLimitClient {
	return &RateLimitClient{
		clientIpHash:    clientIpHash,
		clientIpHashHex: hex.EncodeToString(clientIpHash[:]),
	}
}

// Classifies one client address against an explicit prefix set.
func newRateLimitClient(
	clientAddress string,
	excludePrefixes []netip.Prefix,
) (*RateLimitClient, error) {
	clientIp, _, err := SplitClientAddress(clientAddress)
	if err != nil {
		return nil, err
	}
	return newRateLimitClientIp(clientIp, excludePrefixes)
}

func newRateLimitClientIp(
	clientIp string,
	excludePrefixes []netip.Prefix,
) (*RateLimitClient, error) {
	clientAddr, err := netip.ParseAddr(clientIp)
	if err != nil {
		return nil, err
	}
	clientAddr = clientAddr.Unmap()
	if isRateLimitExcluded(clientAddr, excludePrefixes) {
		return &RateLimitClient{
			clientAddr: clientAddr,
			excluded:   true,
		}, nil
	}

	clientIpHash := ClientIpHashForAddr(clientAddr)
	return &RateLimitClient{
		clientAddr:      clientAddr,
		clientIpHash:    clientIpHash,
		clientIpHashHex: hex.EncodeToString(clientIpHash[:]),
	}, nil
}

// Reports whether this caller bypasses all caller-ip limits.
func (self *RateLimitClient) Excluded() bool {
	return self.excluded
}

// Returns the normalized source address for attribution and diagnostics.
func (self *RateLimitClient) Addr() netip.Addr {
	return self.clientAddr
}

// Returns the privacy-preserving subnet hash used for counter ownership.
func (self *RateLimitClient) IpHash() [32]byte {
	return self.clientIpHash
}

// Returns the encoded subnet hash used in Redis keys and safe logs.
func (self *RateLimitClient) IpHashHex() string {
	return self.clientIpHashHex
}

// Builds cluster-safe keys for one address and optional global identity.
func IpRateLimitAttemptRedisKeys(
	keyPrefix string,
	globalHashTag string,
	clientIpHashHex string,
) (addressRedisKey string, globalRedisKey string) {
	if globalHashTag == "" {
		addressRedisKey = fmt.Sprintf(
			"%s{address_%s}.address",
			keyPrefix,
			clientIpHashHex,
		)
		return
	}

	addressRedisKey = fmt.Sprintf(
		"%s{%s}.address.%s",
		keyPrefix,
		globalHashTag,
		clientIpHashHex,
	)
	globalRedisKey = fmt.Sprintf(
		"%s{%s}.global",
		keyPrefix,
		globalHashTag,
	)
	return
}

// Records one attempt after applying the canonical infrastructure exclusion.
// A success may later clear the returned histories with
// ClearIpRateLimitAttempt.
func CheckIpRateLimitAttempt(
	ctx context.Context,
	client *RateLimitClient,
	globalHashTag string,
	now time.Time,
	settings IpRateLimitAttemptSettings,
) (attemptId IpRateLimitAttemptId, allowed bool, returnErr error) {
	if settings.KeyPrefix == "" || settings.AddressLookback < time.Millisecond ||
		settings.AddressLimit <= 0 || settings.GlobalLookback < time.Millisecond ||
		settings.GlobalLimit <= 0 {
		return attemptId, false, fmt.Errorf("invalid ip rate-limit attempt settings")
	}

	addressRedisKey, globalRedisKey := IpRateLimitAttemptRedisKeys(
		settings.KeyPrefix,
		globalHashTag,
		client.clientIpHashHex,
	)
	if client.excluded {
		addressRedisKey = ""
	}
	attemptId = IpRateLimitAttemptId{
		AddressRedisKey: addressRedisKey,
		GlobalRedisKey:  globalRedisKey,
	}

	hasAddress := 0
	keys := []string{}
	if addressRedisKey != "" {
		hasAddress = 1
		keys = append(keys, addressRedisKey)
	}
	hasGlobal := 0
	if globalRedisKey != "" {
		hasGlobal = 1
		keys = append(keys, globalRedisKey)
	}
	if len(keys) == 0 {
		return attemptId, true, nil
	}

	var allowedInt int64
	Redis(ctx, func(r RedisClient) {
		allowedInt, returnErr = ipRateLimitAttemptScript.Run(
			ctx,
			r,
			keys,
			hasAddress,
			hasGlobal,
			now.UnixMilli(),
			now.Add(settings.AddressLookback).UnixMilli(),
			now.Add(settings.GlobalLookback).UnixMilli(),
			NewId().String(),
			settings.AddressLimit,
			settings.GlobalLimit,
			settings.AddressLookback.Milliseconds(),
			settings.GlobalLookback.Milliseconds(),
		).Int64()
	})
	return attemptId, allowedInt == 1, returnErr
}

// Clears histories owned by a successful attempt. Empty ids are no-ops.
func ClearIpRateLimitAttempt(
	ctx context.Context,
	attemptId IpRateLimitAttemptId,
) error {
	keys := []string{}
	if attemptId.AddressRedisKey != "" {
		keys = append(keys, attemptId.AddressRedisKey)
	}
	if attemptId.GlobalRedisKey != "" {
		keys = append(keys, attemptId.GlobalRedisKey)
	}
	if len(keys) == 0 {
		return nil
	}
	var returnErr error
	Redis(ctx, func(r RedisClient) {
		_, returnErr = ipRateLimitAttemptSuccessScript.Run(ctx, r, keys).Result()
	})
	return returnErr
}

// Increments one caller-owned Redis counter unless its subnet is excluded.
// The caller owns the stable key name and threshold; this function owns the
// exclusion-before-storage invariant and bounded ttl.
func IncrementIpRateLimit(
	ctx context.Context,
	client *RateLimitClient,
	key string,
	duration time.Duration,
) (int64, error) {
	if client.excluded {
		return 0, nil
	}
	return IncrementRateLimitWindow(ctx, key, duration)
}

// Atomically checks and records a seedphrase account creation. PostgreSQL owns
// the rolling-window history; excluded callers return before opening it.
func CheckNetworkCreateIpRateLimit(
	ctx context.Context,
	client *RateLimitClient,
	limit int,
	window time.Duration,
) (result NetworkCreateIpRateLimitResult) {
	if client.excluded {
		return
	}
	if limit <= 0 || window < time.Second {
		panic("invalid network-create ip rate-limit settings")
	}

	Tx(ctx, func(tx PgTx) {
		queryResult, err := tx.Query(
			ctx,
			`
				SELECT
					COUNT(*),
					COALESCE(
						CEIL(EXTRACT(EPOCH FROM (
							MIN(create_time) + INTERVAL '1 seconds' * $2 - now()
						)))::bigint,
						0
					)
				FROM network_create_attempt
				WHERE
					client_address_hash = $1 AND
					now() - INTERVAL '1 seconds' * $2 <= create_time
			`,
			client.clientIpHash[:],
			int(window/time.Second),
		)
		WithPgResult(queryResult, err, func() {
			if queryResult.Next() {
				Raise(queryResult.Scan(&result.Count, &result.RetryAfterSeconds))
			}
		})
		if limit <= result.Count {
			return
		}
		RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_create_attempt
				(network_create_attempt_id, client_address_hash, create_time)
				VALUES ($1, $2, $3)
			`,
			NewId(),
			client.clientIpHash[:],
			NowUtc(),
		))
	})
	return
}

// Removes expired seedphrase account-creation histories.
func RemoveExpiredNetworkCreateIpRateLimitAttempts(ctx context.Context, minTime time.Time) {
	MaintenanceTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_create_attempt
				WHERE create_time < $1
			`,
			minTime.UTC(),
		))
	})
}

// Atomically records and checks a wallet-challenge source-address history.
// Excluded callers receive a zero attempt id and never open PostgreSQL.
func CheckWalletChallengeIpRateLimit(
	ctx context.Context,
	client *RateLimitClient,
	clientPort int,
	lookback time.Duration,
	failedLimit int,
) (attemptId Id, allowed bool) {
	if client.excluded {
		return Id{}, true
	}
	if clientPort < 0 || 65535 < clientPort || lookback < time.Second || failedLimit <= 0 {
		panic("invalid wallet-challenge ip rate-limit settings")
	}

	Tx(ctx, func(tx PgTx) {
		attemptId = NewId()
		RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO wallet_auth_challenge_attempt
				(wallet_auth_challenge_attempt_id, client_address_hash, client_address_port, success)
				VALUES ($1, $2, $3, $4)
			`,
			attemptId,
			client.clientIpHash[:],
			clientPort,
			false,
		))

		failedCount := 0
		queryResult, err := tx.Query(
			ctx,
			`
				SELECT COUNT(*)
				FROM (
					SELECT 1
					FROM wallet_auth_challenge_attempt
					WHERE
						client_address_hash = $1 AND
						client_address_port = $2 AND
						now() - INTERVAL '1 seconds' * $3 <= attempt_time AND
						success = false
					ORDER BY attempt_time DESC
					LIMIT $4
				) attempts
			`,
			client.clientIpHash[:],
			clientPort,
			lookback/time.Second,
			failedLimit,
		)
		WithPgResult(queryResult, err, func() {
			if queryResult.Next() {
				Raise(queryResult.Scan(&failedCount))
			}
		})
		allowed = failedCount < failedLimit
	})
	return
}

// Marks a wallet-challenge attempt successful. A zero id represents an
// excluded no-op and returns before opening PostgreSQL.
func SetWalletChallengeIpRateLimitSuccess(
	ctx context.Context,
	attemptId Id,
	success bool,
) {
	if attemptId == (Id{}) {
		return
	}
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE wallet_auth_challenge_attempt
				SET success = $1
				WHERE wallet_auth_challenge_attempt_id = $2
			`,
			success,
			attemptId,
		))
	})
}

// Creates Connect admission state with production settings.
func NewConnectionRateLimitWithDefaults(
	ctx context.Context,
	clientAddress string,
	handlerId Id,
) (*ConnectionRateLimit, error) {
	return NewConnectionRateLimit(
		ctx,
		clientAddress,
		handlerId,
		DefaultConnectionRateLimitSettings(),
	)
}

// Creates Connect admission state through the canonical caller classifier.
func NewConnectionRateLimit(
	ctx context.Context,
	clientAddress string,
	handlerId Id,
	settings *ConnectionRateLimitSettings,
) (*ConnectionRateLimit, error) {
	client, err := NewRateLimitClient(clientAddress)
	if err != nil {
		return nil, err
	}
	return newConnectionRateLimit(ctx, client, handlerId, settings), nil
}

func newConnectionRateLimit(
	ctx context.Context,
	client *RateLimitClient,
	handlerId Id,
	settings *ConnectionRateLimitSettings,
) *ConnectionRateLimit {
	return &ConnectionRateLimit{
		ctx:       ctx,
		client:    client,
		handlerId: handlerId,
		settings:  settings,
	}
}

// Records one Connect admission and returns its mandatory release callback.
// Excluded infrastructure returns before touching Redis.
func (self *ConnectionRateLimit) Connect() (err error, disconnect func()) {
	disconnect = func() {}
	if self.client.excluded {
		return
	}

	// Both counters for one caller share a Redis hash tag so the transaction
	// stays in one cluster slot while different callers distribute normally.
	burstKey := fmt.Sprintf(
		"{connect_%s}burst_%d",
		self.client.clientIpHashHex,
		NowUtc().Unix()/int64(self.settings.BurstDuration/time.Second),
	)
	totalKey := fmt.Sprintf(
		"{connect_%s}total_%s",
		self.client.clientIpHashHex,
		self.handlerId,
	)

	totalIncremented := false
	disconnect = func() {
		if !totalIncremented {
			return
		}
		// Cleanup must survive cancellation of the admitted connection.
		cleanupCtx := context.Background()
		var cleanupErr error
		var totalCount int64
		Redis(cleanupCtx, func(r RedisClient) {
			var totalCmd *redis.IntCmd
			_, pipelineErr := r.Pipelined(cleanupCtx, func(pipe redis.Pipeliner) error {
				totalCmd = pipe.Decr(cleanupCtx, totalKey)
				// A decrement can recreate an expired key. Keep that repair bounded
				// without extending a live counter's original expiry.
				pipe.ExpireNX(cleanupCtx, totalKey, self.settings.TotalExpiration)
				return nil
			})
			if pipelineErr != nil {
				cleanupErr = pipelineErr
				return
			}
			totalCount, cleanupErr = totalCmd.Result()
		})
		if cleanupErr != nil {
			glog.Errorf(
				"[ratelimit][connect][%s]total could not decrement err = %s\n",
				self.client.clientIpHashHex,
				cleanupErr,
			)
		} else if glog.V(1) {
			glog.Infof(
				"[ratelimit][connect][%s]total -1 @%d\n",
				self.client.clientIpHashHex,
				totalCount,
			)
		}
	}

	var burstCount int64
	var totalCount int64
	Redis(self.ctx, func(r RedisClient) {
		var burstCmd *redis.IntCmd
		var totalCmd *redis.IntCmd
		_, pipelineErr := r.TxPipelined(self.ctx, func(pipe redis.Pipeliner) error {
			burstCmd = pipe.Incr(self.ctx, burstKey)
			pipe.Expire(self.ctx, burstKey, self.settings.BurstDuration)

			totalCmd = pipe.Incr(self.ctx, totalKey)
			pipe.Expire(self.ctx, totalKey, self.settings.TotalExpiration)
			return nil
		})
		if pipelineErr != nil {
			err = pipelineErr
			return
		}
		burstCount, err = burstCmd.Result()
		if err != nil {
			return
		}
		totalCount, err = totalCmd.Result()
		if err == nil {
			totalIncremented = true
		}
	})
	if err != nil {
		return
	}

	if glog.V(1) {
		glog.Infof(
			"[ratelimit][connect][%s]total +1 @%d\n",
			self.client.clientIpHashHex,
			totalCount,
		)
	}
	if int64(self.settings.MaxTotalConnectionCount) < totalCount {
		delay := self.settings.TotalConnectionDelay
		if glog.V(1) {
			glog.Infof(
				"[ratelimit][connect][%s]total limit @%d (+%.2fs delay)\n",
				self.client.clientIpHashHex,
				totalCount,
				float64(delay/time.Millisecond)/1000.0,
			)
		}
		select {
		case <-self.ctx.Done():
			err = fmt.Errorf("Done.")
			return
		case <-time.After(delay):
		}
		err = fmt.Errorf("Total connection count exceeded.")
		return
	}

	if int64(self.settings.BurstConnectionCount) < burstCount {
		delay := time.Duration(
			burstCount-int64(self.settings.BurstConnectionCount),
		) * self.settings.BurstConnectionDelay
		if glog.V(1) {
			glog.Infof(
				"[ratelimit][connect][%s]burst limit @%d (+%.2fs delay)\n",
				self.client.clientIpHashHex,
				burstCount,
				float64(delay/time.Millisecond)/1000.0,
			)
		}
		select {
		case <-self.ctx.Done():
			err = fmt.Errorf("Done.")
			return
		case <-time.After(delay):
		}
		err = fmt.Errorf("Burst connection count exceeded.")
		return
	}

	return
}

// Records and checks one caller against a fixed Redis window.
func CheckRateLimitWindow(
	ctx context.Context,
	clientAddress string,
	settings RateLimitWindowSettings,
) (RateLimitResult, error) {
	client, err := NewRateLimitClient(clientAddress)
	if err != nil {
		return RateLimitResult{}, err
	}
	return checkRateLimitWindow(ctx, client, settings, incrementRateLimitCounter)
}

// Applies the exclusion before invoking the supplied counter. The callback is
// injectable so the no-storage guarantee is deterministic to test.
func checkRateLimitWindow(
	ctx context.Context,
	client *RateLimitClient,
	settings RateLimitWindowSettings,
	counter rateLimitWindowCounter,
) (RateLimitResult, error) {
	if settings.Namespace == "" || settings.Name == "" {
		return RateLimitResult{}, fmt.Errorf("rate-limit namespace and name are required")
	}
	if settings.Duration < time.Millisecond {
		return RateLimitResult{}, fmt.Errorf("rate-limit duration must be at least one millisecond")
	}
	if settings.Limit <= 0 {
		return RateLimitResult{}, fmt.Errorf("rate-limit count must be positive")
	}
	if client.excluded {
		return RateLimitResult{
			Allowed:  true,
			Excluded: true,
		}, nil
	}

	window := NowUtc().UnixMilli() / settings.Duration.Milliseconds()
	key := fmt.Sprintf(
		"{ratelimit_%s_%s}%s_%d",
		settings.Namespace,
		client.clientIpHashHex,
		settings.Name,
		window,
	)
	count, err := counter(ctx, key, settings.Duration)
	if err != nil {
		return RateLimitResult{}, err
	}
	return RateLimitResult{
		Allowed: count <= settings.Limit,
		Count:   count,
	}, nil
}

// rateLimitWindowKey assigns one Redis key to one epoch-aligned fixed window.
// The window number is part of the key; merely refreshing a counter's TTL
// would instead turn a "per window" cap into a lifetime cap for any caller
// that remains active more frequently than duration.
func rateLimitWindowKey(key string, duration time.Duration, now time.Time) (string, error) {
	if key == "" || duration < time.Millisecond {
		return "", fmt.Errorf("invalid rate-limit counter settings")
	}
	windowMillis := duration.Milliseconds()
	return fmt.Sprintf("%s_%d", key, now.UnixMilli()/windowMillis), nil
}

// IncrementRateLimitWindow atomically increments the current epoch-aligned
// fixed-window counter and gives that physical key a bounded cleanup TTL.
func IncrementRateLimitWindow(ctx context.Context, key string, duration time.Duration) (int64, error) {
	return incrementRateLimitWindowAt(ctx, key, duration, NowUtc())
}

func incrementRateLimitWindowAt(
	ctx context.Context,
	key string,
	duration time.Duration,
	now time.Time,
) (count int64, returnErr error) {
	windowKey, err := rateLimitWindowKey(key, duration, now)
	if err != nil {
		return 0, err
	}
	return incrementRateLimitCounter(ctx, windowKey, duration)
}

// incrementRateLimitCounter owns only the atomic Redis increment and cleanup
// TTL. Its callers must supply an already window-qualified physical key.
func incrementRateLimitCounter(
	ctx context.Context,
	key string,
	duration time.Duration,
) (count int64, returnErr error) {
	Redis(ctx, func(r RedisClient) {
		var countCmd *redis.IntCmd
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			countCmd = pipe.Incr(ctx, key)
			pipe.Expire(ctx, key, duration)
			return nil
		})
		if err != nil {
			returnErr = err
			return
		}
		count, returnErr = countCmd.Result()
	})
	return
}
