package model

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// maxUserAuthAttemptsError reports the auth-attempt limit to the client.
//
// 429, not the 503 this used to return. A rate limit is client-attributable:
// the request was refused because of who sent it and how often, not because the
// service is broken. A 5xx tells every well-behaved client and SDK to retry,
// and every retry records another attempt (see authAttemptScript), so the
// status itself made the condition worse and did so self-reinforcingly.
//
// The wording depends on which budget was spent, because the two are not the
// same event. With no user auth -- SSO signup, wallet signup, AuthPasswordSet
// -- userAuthAttemptRedisKeys builds a key with no identity component, so the
// budget is shared by everyone at the client address and the refused user may
// have done nothing at all. With a user auth the budget is that account's.
// Support cannot tell a wrongly-refused stranger from abuse if both are handed
// the same sentence, which is why the old single message ("User auth attempts
// exceeded limits.") is gone.
//
// Retry-After is the address lookback rather than the global one. Both windows
// feed this refusal, but the address window is the binding constraint in
// practice (5 attempts / 5 minutes against 300 / 30 minutes), and advising the
// longer wait would hold back a legitimate caller six times longer than needed.
// A caller that retries on the hint and is still over budget simply gets
// another 429 carrying a fresh hint.
func maxUserAuthAttemptsError(userAuth *string) error {
	retryAfterSeconds := int(AttemptLookback / time.Second)
	if userAuth == nil {
		return &rateLimitError{
			message: fmt.Sprintf(
				"429 Too many recent sign-in or account-creation attempts from your "+
					"network address. This limit is scoped to the address you are "+
					"connecting from and is shared with everyone else on it, so someone "+
					"else on your network may have used it. Please try again in %d minutes.",
				int(AttemptLookback/time.Minute),
			),
			retryAfterSeconds: retryAfterSeconds,
		}
	}
	return &rateLimitError{
		message: fmt.Sprintf(
			"429 Too many recent sign-in attempts for this account from your network "+
				"address. Please try again in %d minutes.",
			int(AttemptLookback/time.Minute),
		),
		retryAfterSeconds: retryAfterSeconds,
	}
}

const AttemptLookback = 5 * time.Minute
const AttemptFailedCountThreshold = 5
const AttemptLookback2 = 30 * time.Minute
const AttemptFailedCountThreshold2 = 300

const userAuthAttemptRedisKeyPrefix = "auth_attempt."

type userAuthAttemptSettings struct {
	addressLookback time.Duration
	addressLimit    int
	globalLookback  time.Duration
	globalLimit     int
}

var defaultUserAuthAttemptSettings = userAuthAttemptSettings{
	addressLookback: AttemptLookback,
	addressLimit:    AttemptFailedCountThreshold,
	globalLookback:  AttemptLookback2,
	globalLimit:     AttemptFailedCountThreshold2,
}

// UserAuthAttemptId is an opaque handle to the Redis histories changed by an
// authentication attempt. It deliberately contains no raw user auth value.
type UserAuthAttemptId struct {
	addressRedisKey string
	globalRedisKey  string
}

// authAttemptScript atomically records an attempt in the address history and,
// for identified users, their global history. Scores are expiry times. Each
// whole sorted set also expires one lookback after its most recent attempt, so
// inactive histories clean themselves up without a periodic Redis scan.
//
// Every attempt, including a rejected one, is recorded. That matches the old
// PostgreSQL limiter: continued attempts keep the sliding window active. Each
// set is pruned by expiry and then by oldest rank, bounding it by time and its
// own threshold count at every write.
var authAttemptScript = redis.NewScript(`
local address_key = KEYS[1]
local has_global = tonumber(ARGV[1])
local now_ms = tonumber(ARGV[2])
local address_expiry_ms = tonumber(ARGV[3])
local global_expiry_ms = tonumber(ARGV[4])
local member = ARGV[5]
local address_limit = tonumber(ARGV[6])
local global_limit = tonumber(ARGV[7])
local address_ttl_ms = tonumber(ARGV[8])
local global_ttl_ms = tonumber(ARGV[9])

redis.call('ZREMRANGEBYSCORE', address_key, '-inf', now_ms)
redis.call('ZADD', address_key, address_expiry_ms, member)
local address_count = redis.call('ZCARD', address_key)
if address_limit < address_count then
    redis.call('ZREMRANGEBYRANK', address_key, 0, address_count - address_limit - 1)
    address_count = address_limit
end
redis.call('PEXPIRE', address_key, address_ttl_ms)

local global_count = 0
if has_global == 1 then
    local global_key = KEYS[2]
    redis.call('ZREMRANGEBYSCORE', global_key, '-inf', now_ms)
    redis.call('ZADD', global_key, global_expiry_ms, member)
    global_count = redis.call('ZCARD', global_key)
    if global_limit < global_count then
        redis.call('ZREMRANGEBYRANK', global_key, 0, global_count - global_limit - 1)
        global_count = global_limit
    end
    redis.call('PEXPIRE', global_key, global_ttl_ms)
end

if address_count < address_limit and (has_global == 0 or global_count < global_limit) then
    return 1
end
return 0
`)

// A success resets the global window and the window for its client-address
// hash. Per-address histories for other client hashes remain untouched.
var authAttemptSuccessScript = redis.NewScript(`
redis.call('DEL', KEYS[1])
if #KEYS == 2 then
    redis.call('DEL', KEYS[2])
end
return 1
`)

func userAuthAttemptRedisKeys(
	userAuth *string,
	clientAddressHashHex string,
) (addressRedisKey string, globalRedisKey string) {
	if userAuth == nil {
		// The hash tag is per address so identity-less histories spread across
		// the cluster rather than sharing one hot slot.
		addressRedisKey = fmt.Sprintf(
			"%s{address_%s}.address",
			userAuthAttemptRedisKeyPrefix,
			clientAddressHashHex,
		)
		return
	}

	// User auths are PII. HMAC gives us a stable cluster-distribution key
	// without putting an email address or phone number in Redis keyspace.
	mac := hmac.New(sha256.New, passwordPepper())
	_, err := mac.Write([]byte("auth-attempt-user\x00"))
	server.Raise(err)
	_, err = mac.Write([]byte(*userAuth))
	server.Raise(err)
	userAuthHashHex := hex.EncodeToString(mac.Sum(nil))
	hashTag := "user_" + userAuthHashHex

	// The two histories for one user share a hash tag, allowing the Lua
	// updates to remain atomic in Redis Cluster. Different users distribute
	// across independent slots.
	addressRedisKey = fmt.Sprintf(
		"%s{%s}.address.%s",
		userAuthAttemptRedisKeyPrefix,
		hashTag,
		clientAddressHashHex,
	)
	globalRedisKey = fmt.Sprintf(
		"%s{%s}.global",
		userAuthAttemptRedisKeyPrefix,
		hashTag,
	)
	return
}

func UserAuthAttempt(
	userAuth *string,
	clientSession *session.ClientSession,
) (userAuthAttemptId UserAuthAttemptId, allow bool) {
	return userAuthAttemptAt(
		userAuth,
		clientSession,
		server.NowUtc(),
		defaultUserAuthAttemptSettings,
	)
}

func userAuthAttemptAt(
	userAuth *string,
	clientSession *session.ClientSession,
	now time.Time,
	settings userAuthAttemptSettings,
) (userAuthAttemptId UserAuthAttemptId, allow bool) {
	if settings.addressLookback < time.Millisecond || settings.addressLimit <= 0 ||
		settings.globalLookback < time.Millisecond || settings.globalLimit <= 0 {
		panic("invalid user auth attempt settings")
	}

	clientAddressHash, _, err := clientSession.ClientAddressHashPort()
	if err != nil {
		return
	}
	clientAddressHashHex := hex.EncodeToString(clientAddressHash[:])
	addressRedisKey, globalRedisKey := userAuthAttemptRedisKeys(userAuth, clientAddressHashHex)
	member := server.NewId().String()
	userAuthAttemptId = UserAuthAttemptId{
		addressRedisKey: addressRedisKey,
		globalRedisKey:  globalRedisKey,
	}

	hasGlobal := 0
	keys := []string{addressRedisKey}
	if globalRedisKey != "" {
		hasGlobal = 1
		keys = append(keys, globalRedisKey)
	}

	var allowed int64
	server.Redis(clientSession.Ctx, func(r server.RedisClient) {
		allowed, err = authAttemptScript.Run(
			clientSession.Ctx,
			r,
			keys,
			hasGlobal,
			now.UnixMilli(),
			now.Add(settings.addressLookback).UnixMilli(),
			now.Add(settings.globalLookback).UnixMilli(),
			member,
			settings.addressLimit,
			settings.globalLimit,
			settings.addressLookback.Milliseconds(),
			settings.globalLookback.Milliseconds(),
		).Int64()
		server.Raise(err)
	})
	return userAuthAttemptId, allowed == 1
}

func SetUserAuthAttemptSuccess(
	ctx context.Context,
	userAuthAttemptId UserAuthAttemptId,
	success bool,
) {
	if !success || userAuthAttemptId.addressRedisKey == "" {
		return
	}

	keys := []string{userAuthAttemptId.addressRedisKey}
	if userAuthAttemptId.globalRedisKey != "" {
		keys = append(keys, userAuthAttemptId.globalRedisKey)
	}
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := authAttemptSuccessScript.Run(
			ctx,
			r,
			keys,
		).Result()
		server.Raise(err)
	})
}

// removeExpiredAuthAttemptsBatchSize bounds each delete pass. user_auth_attempt
// is now legacy, so the cleanup task drains it while Redis takes over. A var
// (not const) lets tests force the multi-batch loop with a small batch.
var removeExpiredAuthAttemptsBatchSize = 50000

func RemoveExpiredAuthAttempts(ctx context.Context, minTime time.Time) (databaseRowsRemoved int64) {
	// LIMIT-batched legacy-table drain: each pass deletes at most one batch in
	// its own transaction until the older-than-window backlog is empty.
	for {
		batchCount := int64(0)
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					DELETE FROM user_auth_attempt
					USING (
						SELECT user_auth_attempt_id
						FROM user_auth_attempt
						WHERE attempt_time < $1
						ORDER BY attempt_time
						LIMIT $2
					) t
					WHERE user_auth_attempt.user_auth_attempt_id = t.user_auth_attempt_id
				`,
				minTime.UTC(),
				removeExpiredAuthAttemptsBatchSize,
			))
			batchCount = tag.RowsAffected()
		})
		databaseRowsRemoved += batchCount
		if batchCount < int64(removeExpiredAuthAttemptsBatchSize) {
			break
		}
	}
	// wallet_auth_challenge_attempt cleanup is handled by RemoveExpiredWalletAuthChallenges
	return
}
