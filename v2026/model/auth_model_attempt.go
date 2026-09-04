package model

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// maxUserAuthAttemptsError reports the auth-attempt limit to the client.
//
// 429, not the 503 this used to return. A rate limit is client-attributable:
// the request was refused because of who sent it and how often, not because the
// service is broken. A 5xx tells every well-behaved client and SDK to retry,
// and every retry records another attempt (see server.CheckIpRateLimitAttempt), so the
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

func userAuthAttemptRedisKeys(
	userAuth *string,
	clientAddressHashHex string,
) (addressRedisKey string, globalRedisKey string) {
	return server.IpRateLimitAttemptRedisKeys(
		userAuthAttemptRedisKeyPrefix,
		userAuthAttemptGlobalHashTag(userAuth),
		clientAddressHashHex,
	)
}

func userAuthAttemptGlobalHashTag(userAuth *string) string {
	if userAuth == nil {
		return ""
	}

	// User auths are PII. HMAC gives us a stable cluster-distribution key
	// without putting an email address or phone number in Redis keyspace.
	mac := hmac.New(sha256.New, passwordPepper())
	_, err := mac.Write([]byte("auth-attempt-user\x00"))
	server.Raise(err)
	_, err = mac.Write([]byte(*userAuth))
	server.Raise(err)
	userAuthHashHex := hex.EncodeToString(mac.Sum(nil))
	return "user_" + userAuthHashHex
}

func UserAuthAttempt(
	userAuth *string,
	clientSession *session.ClientSession,
) (userAuthAttemptId UserAuthAttemptId, allow bool) {
	// The fixed acceptance phone is intentionally exercised repeatedly in
	// immediate reruns. Its tests.yml policy requires both the exact normalized
	// phone and a configured fixture password, so only that phone skips
	// histories; every email and identity-less SSO, wallet, and reset-code
	// attempt retains normal limits.
	if userAuth != nil && testAuthPolicyForUserAuth(userAuth).BypassRateLimits {
		return UserAuthAttemptId{}, true
	}
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

	rateLimitClient, err := server.NewRateLimitClient(clientSession.ClientAddress)
	if err != nil {
		// Deferred sessions can carry only a persisted hash. They cannot be
		// newly classified, but retain their original address budget.
		clientAddressHash, _, err := clientSession.ClientAddressHashPort()
		if err != nil {
			return
		}
		rateLimitClient = server.NewStoredRateLimitClient(clientAddressHash)
	}
	return userAuthAttemptAtForClient(
		userAuth,
		clientSession,
		now,
		settings,
		rateLimitClient,
	)
}

func userAuthAttemptAtForClient(
	userAuth *string,
	clientSession *session.ClientSession,
	now time.Time,
	settings userAuthAttemptSettings,
	rateLimitClient *server.RateLimitClient,
) (userAuthAttemptId UserAuthAttemptId, allow bool) {
	attemptId, allow, err := server.CheckIpRateLimitAttempt(
		clientSession.Ctx,
		rateLimitClient,
		userAuthAttemptGlobalHashTag(userAuth),
		now,
		server.IpRateLimitAttemptSettings{
			KeyPrefix:       userAuthAttemptRedisKeyPrefix,
			AddressLookback: settings.addressLookback,
			AddressLimit:    settings.addressLimit,
			GlobalLookback:  settings.globalLookback,
			GlobalLimit:     settings.globalLimit,
		},
	)
	server.Raise(err)
	userAuthAttemptId = UserAuthAttemptId{
		addressRedisKey: attemptId.AddressRedisKey,
		globalRedisKey:  attemptId.GlobalRedisKey,
	}
	return userAuthAttemptId, allow
}

func SetUserAuthAttemptSuccess(
	ctx context.Context,
	userAuthAttemptId UserAuthAttemptId,
	success bool,
) {
	if !success {
		return
	}
	server.Raise(server.ClearIpRateLimitAttempt(
		ctx,
		server.IpRateLimitAttemptId{
			AddressRedisKey: userAuthAttemptId.addressRedisKey,
			GlobalRedisKey:  userAuthAttemptId.globalRedisKey,
		},
	))
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
