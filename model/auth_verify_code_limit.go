package model

// The tries a verification code, an email or phone, and a caller get.
//
// A verification code is short: six digits when the client asks for a numeric
// code (`use_numeric`), eight hex digits otherwise. The auth attempt limits
// (auth_model_attempt.go) bound the tries of an address from one client
// network over minutes, and of the address over half an hour, and every try
// was a fresh guess at the same live code. These limits bound the tries in
// total (owner decision 2026-11-21):
//   - A code takes at most VerifyCodeTryLimit wrong tries. The fifth kills it:
//     the person who mistyped asks for a new code (a resend), which starts
//     over.
//   - Each code of an email or phone killed so is a strike against the
//     address. Its strikes in the trailing VerifyCodeStrikeWindow lock the
//     address out of codes, longer for each one (verifyCodeLockouts): no code
//     is verified or sent for it until the lockout ends. Verifying the
//     address clears its strikes.
//   - A caller's wrong tries are bounded across every address: those from its
//     client network (the /29 or /56 the attempt limits use), and those of the
//     network it is signed in to when it presents a network or client token.
//
// A lockout or a spent budget is refused (429 with Retry-After) before any code
// is read, so a refused try learns nothing about a code. Every try holds its
// place in the caller's budgets and takes its code's try before its guess is
// compared, so tries made at once cannot pass a limit together; a right guess
// gives both back, so only wrong tries count.
//
// The counts live in Redis, as the auth attempt limits do; a killed code is
// marked used in user_auth_verify. The fixed acceptance phone fixture skips
// them, as it skips the auth attempt limits (testAuthPolicy.BypassRateLimits).

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A code takes this many wrong tries; the last of them kills it. A try is
// counted before its guess is compared, and a right one gives it back.
const VerifyCodeTryLimit = 5

// The record of a code killed by wrong tries, an account action attempt keyed
// by the code's id (killVerifyCode). It is kept longer than a code lives
// (AccountActionAttemptRetention, VerifyCodeTimeout).
const AccountActionVerifyCodeKilled = "verify_code_killed"

// How long the strikes of an email or phone (its codes killed by wrong tries)
// count toward its lockouts.
const VerifyCodeStrikeWindow = 7 * 24 * time.Hour

// The wrong tries of one client network (a /29 or /56) across every address.
const (
	VerifyCodeSourceWrongTryLimit  = 20
	VerifyCodeSourceWrongTryWindow = time.Hour
)

// The wrong tries of a caller signed in to a network (with a network or client
// token of it) across every address.
const (
	VerifyCodeNetworkWrongTryLimit  = 20
	VerifyCodeNetworkWrongTryWindow = time.Hour
)

// The lockout of an email or phone after its nth strike in the window: none
// after the first, so a person who mistyped asks for a new code at once; then
// longer each time, up to a day.
var verifyCodeLockouts = []time.Duration{
	0,
	15 * time.Minute,
	time.Hour,
	4 * time.Hour,
	24 * time.Hour,
}

// The lockout after an address's nth strike in the window.
func verifyCodeLockout(strikes int) time.Duration {
	if strikes <= 0 {
		return 0
	}
	return verifyCodeLockouts[min(strikes, len(verifyCodeLockouts))-1]
}

// The answer to the wrong try that killed a code, and to a try of a code that
// has taken its tries.
const AuthVerifyCodeSpentMessage = "Too many wrong codes. This code no longer works. Please request a new code."

// AuthVerifyCodeSpentMessage when the strike also locked the address out.
func authVerifyCodeSpentLockedMessage(lockout time.Duration) string {
	return fmt.Sprintf(
		"Too many wrong codes. This code no longer works. Please request a new code in %s.",
		accountActionRetryText(lockout),
	)
}

// The refusal of a try or a new code for an address that is locked out.
func verifyCodeAddressLockedError(retryAfter time.Duration) error {
	return &rateLimitError{
		message: fmt.Sprintf(
			"429 Too many wrong codes were entered for this email or phone. Please try again in %s.",
			accountActionRetryText(retryAfter),
		),
		retryAfterSeconds: verifyCodeRetryAfterSeconds(retryAfter),
	}
}

// The refusal of an address locked out for retryAfter, as AuthVerifyCreateCode
// returns it. Exposed for tests in other packages.
func Testing_VerifyCodeAddressLockedError(retryAfter time.Duration) error {
	return verifyCodeAddressLockedError(retryAfter)
}

// The refusal of a try from a client network that has spent its wrong tries.
func verifyCodeSourceRefusedError(retryAfter time.Duration) error {
	return &rateLimitError{
		message: fmt.Sprintf(
			"429 Too many wrong codes were entered from your network address. Please try again in %s.",
			accountActionRetryText(retryAfter),
		),
		retryAfterSeconds: verifyCodeRetryAfterSeconds(retryAfter),
	}
}

// The refusal of a try by a caller whose network has spent its wrong tries.
func verifyCodeNetworkRefusedError(retryAfter time.Duration) error {
	return &rateLimitError{
		message: fmt.Sprintf(
			"429 Too many wrong codes were entered from this account. Please try again in %s.",
			accountActionRetryText(retryAfter),
		),
		retryAfterSeconds: verifyCodeRetryAfterSeconds(retryAfter),
	}
}

// Whole seconds, rounded up so that trying again after them is never early.
func verifyCodeRetryAfterSeconds(retryAfter time.Duration) int {
	return int(max((retryAfter+time.Second-1)/time.Second, 1))
}

// Tests set the time the limits read through the request's context, so a
// lockout ends without waiting for it. The limits read the server's clock
// otherwise.
type verifyCodeLimitClockKey struct{}

func verifyCodeLimitNow(ctx context.Context) time.Time {
	if now, ok := ctx.Value(verifyCodeLimitClockKey{}).(func() time.Time); ok {
		return now()
	}
	return server.NowUtc()
}

const verifyCodeLimitKeyPrefix = "auth_verify_code."

// One email or phone's strikes and lockout. The keys share the address's
// hash tag with its auth attempt histories (userAuthAttemptGlobalHashTag): an
// hmac of the address, so no email or phone is in the Redis keyspace.
type verifyCodeAddress struct {
	// the fixed acceptance phone fixture: no strikes or lockouts
	bypass bool
	// the strikes, each scored by when it was made
	strikesKey string
	// the end of the lockout, in unix milliseconds
	lockKey string
}

func verifyCodeAddressOf(userAuth string) verifyCodeAddress {
	if testAuthPolicyForUserAuth(&userAuth).BypassRateLimits {
		return verifyCodeAddress{bypass: true}
	}
	hashTag := userAuthAttemptGlobalHashTag(&userAuth)
	return verifyCodeAddress{
		strikesKey: fmt.Sprintf("%s{%s}.strikes", verifyCodeLimitKeyPrefix, hashTag),
		lockKey:    fmt.Sprintf("%s{%s}.lock", verifyCodeLimitKeyPrefix, hashTag),
	}
}

// How long the address stays locked out after now; zero when it is not. The
// lockout's end is kept in milliseconds, so now is read in them too.
func (self verifyCodeAddress) lockedFor(ctx context.Context, now time.Time) (retryAfter time.Duration) {
	if self.bypass {
		return 0
	}
	now = now.Truncate(time.Millisecond)
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, self.lockKey).Result()
		if errors.Is(err, redis.Nil) {
			return
		}
		server.Raise(err)
		lockEndMillis, err := strconv.ParseInt(value, 10, 64)
		server.Raise(err)
		retryAfter = max(time.UnixMilli(lockEndMillis).Sub(now), 0)
	})
	return
}

// The refusal of a code for an address that is locked out, or nil.
func (self verifyCodeAddress) lockRefusal(ctx context.Context, now time.Time) error {
	if retryAfter := self.lockedFor(ctx, now); 0 < retryAfter {
		return verifyCodeAddressLockedError(retryAfter)
	}
	return nil
}

// Records a strike: drops the strikes that left the window, adds this one,
// keeps at most one per lockout step (later strikes lock as long as the last
// step), and extends the lockout to the end the strike count asks for. ARGV:
// now, the window's start, the strike's member, the window, then for each
// lockout step its end and its length (0 for none), all in milliseconds.
var verifyCodeStrikeScript = redis.NewScript(`
local strikes_key = KEYS[1]
local lock_key = KEYS[2]
redis.call('ZREMRANGEBYSCORE', strikes_key, '-inf', '(' .. ARGV[2])
redis.call('ZADD', strikes_key, ARGV[1], ARGV[3])
local count = redis.call('ZCARD', strikes_key)
local steps = (#ARGV - 4) / 2
if steps < count then
    redis.call('ZREMRANGEBYRANK', strikes_key, 0, count - steps - 1)
    count = steps
end
redis.call('PEXPIRE', strikes_key, ARGV[4])
local lock_end = ARGV[3 + 2 * count]
local lock_length = ARGV[4 + 2 * count]
if 0 < tonumber(lock_length) then
    local current = redis.call('GET', lock_key)
    if (not current) or tonumber(current) < tonumber(lock_end) then
        redis.call('SET', lock_key, lock_end, 'PX', lock_length)
    end
end
return count
`)

// A strike against the address: one of its codes was killed by wrong tries.
// Returns how many strikes are in the window, counting at most one per lockout
// step, and the lockout the strike set.
func (self verifyCodeAddress) strike(ctx context.Context, now time.Time) (strikes int, lockout time.Duration) {
	if self.bypass {
		return 0, 0
	}
	args := []any{
		now.UnixMilli(),
		now.Add(-VerifyCodeStrikeWindow).UnixMilli(),
		server.NewId().String(),
		VerifyCodeStrikeWindow.Milliseconds(),
	}
	for _, stepLockout := range verifyCodeLockouts {
		args = append(args, now.Add(stepLockout).UnixMilli(), stepLockout.Milliseconds())
	}
	server.Redis(ctx, func(r server.RedisClient) {
		count, err := verifyCodeStrikeScript.Run(ctx, r, []string{self.strikesKey, self.lockKey}, args...).Int64()
		server.Raise(err)
		strikes = int(count)
	})
	return strikes, verifyCodeLockout(strikes)
}

// Clears the address's strikes and lockout: it was verified.
func (self verifyCodeAddress) clear(ctx context.Context) {
	if self.bypass {
		return
	}
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Del(ctx, self.strikesKey, self.lockKey).Err())
	})
}

// Holds a place for a try in a caller's budget of wrong tries, a sliding window
// whose members are scored by when they leave it, unless the budget is spent.
// A refusal adds nothing. Returns 1 and 0, or 0 and the milliseconds until the
// oldest place leaves the window. ARGV: now, when this place leaves the window,
// the place's member, the limit, the window (milliseconds).
var verifyCodeBudgetScript = redis.NewScript(`
local key = KEYS[1]
redis.call('ZREMRANGEBYSCORE', key, '-inf', ARGV[1])
local count = redis.call('ZCARD', key)
if tonumber(ARGV[4]) <= count then
    local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
    return {0, tonumber(oldest[2]) - tonumber(ARGV[1])}
end
redis.call('ZADD', key, ARGV[2], ARGV[3])
redis.call('PEXPIRE', key, ARGV[5])
return {1, 0}
`)

func holdVerifyCodeBudget(
	ctx context.Context,
	key string,
	member string,
	now time.Time,
	limit int,
	window time.Duration,
) (allowed bool, retryAfter time.Duration) {
	server.Redis(ctx, func(r server.RedisClient) {
		values, err := verifyCodeBudgetScript.Run(
			ctx,
			r,
			[]string{key},
			now.UnixMilli(),
			now.Add(window).UnixMilli(),
			member,
			limit,
			window.Milliseconds(),
		).Int64Slice()
		server.Raise(err)
		if len(values) != 2 {
			panic(fmt.Errorf("verification code budget answered %v", values))
		}
		allowed = values[0] == 1
		retryAfter = time.Duration(values[1]) * time.Millisecond
	})
	return
}

// The limits of one try of an address's codes.
type verifyCodeTryLimits struct {
	ctx     context.Context
	now     time.Time
	address verifyCodeAddress
	// the caller's budgets of wrong tries: of its client network, and of the
	// network it is signed in to. "" when the caller has none (an excluded
	// infrastructure address, no token).
	sourceKey  string
	networkKey string
	// this try's place in the budgets it holds
	member string
	held   []string
	// the try counts of the codes this try took a try of
	triedKeys []string
}

func newVerifyCodeTryLimits(clientSession *session.ClientSession, userAuth string) *verifyCodeTryLimits {
	ctx := clientSession.Ctx
	self := &verifyCodeTryLimits{
		ctx:     ctx,
		now:     verifyCodeLimitNow(ctx),
		address: verifyCodeAddressOf(userAuth),
		member:  server.NewId().String(),
	}
	if self.address.bypass {
		// the fixture's tries count in no history, as in UserAuthAttempt
		return self
	}
	if sourceHashHex := verifyCodeSourceHashHex(clientSession); sourceHashHex != "" {
		self.sourceKey = fmt.Sprintf("%s{address_%s}.wrong", verifyCodeLimitKeyPrefix, sourceHashHex)
	}
	if networkId := verifyCodeCallerNetworkId(clientSession); networkId != nil {
		self.networkKey = fmt.Sprintf("%s{network_%s}.wrong", verifyCodeLimitKeyPrefix, *networkId)
	}
	return self
}

// The keyed hash of the caller's client network (the /29 or /56) its budget is
// kept under, as the auth attempt limits key it, or "" for an excluded
// infrastructure address or a request with no address.
func verifyCodeSourceHashHex(clientSession *session.ClientSession) string {
	rateLimitClient, err := server.NewRateLimitClient(clientSession.ClientAddress)
	if err != nil {
		// a deferred session carries only its address's hash
		clientAddressHash, _, err := clientSession.ClientAddressHashPort()
		if err != nil {
			return ""
		}
		rateLimitClient = server.NewStoredRateLimitClient(clientAddressHash)
	}
	if rateLimitClient.Excluded() {
		return ""
	}
	return rateLimitClient.IpHashHex()
}

// The network of the token the caller presents, a network or client token
// (any whose signature verifies), or nil. /auth/verify serves signed-out
// callers, so the token is read here as model.AuthVerify reads it. A lag guest
// token stands for a guest network shared by no one, and is left out.
func verifyCodeCallerNetworkId(clientSession *session.ClientSession) *server.Id {
	token, ok := strings.CutPrefix(http.Header(clientSession.Header).Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return nil
	}
	presented, err := session.ParseByJwtForAudience(clientSession.Ctx, token, session.ByJwtAudienceApi)
	if err != nil || presented.LagGuest {
		return nil
	}
	return &presented.NetworkId
}

// Admits the try before any code is read, or refuses it: the address is locked
// out, or the caller has spent the wrong tries of its client network or of its
// network. An admitted try holds a place in each of the caller's budgets until
// its guess is known (release).
func (self *verifyCodeTryLimits) admit() error {
	if self.address.bypass {
		return nil
	}
	if err := self.address.lockRefusal(self.ctx, self.now); err != nil {
		return err
	}
	if self.sourceKey != "" {
		allowed, retryAfter := holdVerifyCodeBudget(
			self.ctx,
			self.sourceKey,
			self.member,
			self.now,
			VerifyCodeSourceWrongTryLimit,
			VerifyCodeSourceWrongTryWindow,
		)
		if !allowed {
			return verifyCodeSourceRefusedError(retryAfter)
		}
		self.held = append(self.held, self.sourceKey)
	}
	if self.networkKey != "" {
		allowed, retryAfter := holdVerifyCodeBudget(
			self.ctx,
			self.networkKey,
			self.member,
			self.now,
			VerifyCodeNetworkWrongTryLimit,
			VerifyCodeNetworkWrongTryWindow,
		)
		if !allowed {
			self.release()
			return verifyCodeNetworkRefusedError(retryAfter)
		}
		self.held = append(self.held, self.networkKey)
	}
	return nil
}

// Gives back the try's places in the caller's budgets: its guess was right, or
// it was refused before its guess was compared.
func (self *verifyCodeTryLimits) release() {
	held := self.held
	self.held = nil
	for _, key := range held {
		server.Redis(self.ctx, func(r server.RedisClient) {
			server.Raise(r.ZRem(self.ctx, key, self.member).Err())
		})
	}
}

// A live code of the address's holder: unused and not expired.
type liveVerifyCode struct {
	userAuthVerifyId server.Id
	verifyCode       string
}

// One more try of each live code: the code's try count with this one. A code
// counts its tries for as long as it lives (VerifyCodeTimeout).
func (self *verifyCodeTryLimits) tryCodes(liveCodes []liveVerifyCode) []int64 {
	tries := make([]int64, len(liveCodes))
	if self.address.bypass {
		return tries
	}
	for i, liveCode := range liveCodes {
		key := fmt.Sprintf("%s{code_%s}.tries", verifyCodeLimitKeyPrefix, liveCode.userAuthVerifyId)
		server.Redis(self.ctx, func(r server.RedisClient) {
			var count *redis.IntCmd
			_, err := r.TxPipelined(self.ctx, func(pipe redis.Pipeliner) error {
				count = pipe.Incr(self.ctx, key)
				pipe.PExpire(self.ctx, key, VerifyCodeTimeout)
				return nil
			})
			server.Raise(err)
			tries[i] = count.Val()
		})
		self.triedKeys = append(self.triedKeys, key)
	}
	return tries
}

// Gives back the tries this try took of the codes: its guess was right, so only
// wrong tries count toward a code's death. A right code that is refused after
// it is compared (a session cap, a code sent to another address) is tried
// again as often as needed.
func (self *verifyCodeTryLimits) untryCodes() {
	triedKeys := self.triedKeys
	self.triedKeys = nil
	for _, key := range triedKeys {
		server.Redis(self.ctx, func(r server.RedisClient) {
			server.Raise(r.Decr(self.ctx, key).Err())
		})
	}
}

// What one try of a guess does to the live codes it is tried against.
type verifyCodeTryOutcome struct {
	// the index of the code the guess is, or -1
	match int
	// the indexes of the codes that have taken their tries, which are killed
	spent []int
	// the guess was wrong and was the last try of a code: a strike against
	// the address
	strike bool
}

// Decides a try of guess against verifyCodes, the address holder's live codes,
// with tries[i] the try count of verifyCodes[i] including this try. A code
// past its tries is not compared. A code at or past them is killed unless the
// guess is it; it was this try that took a code's last, wrong, try exactly when
// its count is the limit.
func decideVerifyCodeTry(verifyCodes []string, tries []int64, guess string) verifyCodeTryOutcome {
	outcome := verifyCodeTryOutcome{match: -1}
	for i, verifyCode := range verifyCodes {
		if VerifyCodeTryLimit < tries[i] {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(verifyCode), []byte(guess)) == 1 && outcome.match < 0 {
			outcome.match = i
		}
	}
	for i := range verifyCodes {
		if i == outcome.match || tries[i] < VerifyCodeTryLimit {
			continue
		}
		outcome.spent = append(outcome.spent, i)
		if tries[i] == VerifyCodeTryLimit && outcome.match < 0 {
			outcome.strike = true
		}
	}
	return outcome
}

// Tries guess against the live codes of userId, the holder of the address. Each
// code takes one more try (tryCodes), and the codes that have taken their
// tries are killed. Returns the code the guess is, with the try's places in the
// caller's budgets and its tries of the codes given back; or nil and the
// message of a code that took its tries, "" for a plain wrong code.
func (self *verifyCodeTryLimits) tryGuess(userId server.Id, liveCodes []liveVerifyCode, guess string) (match *liveVerifyCode, spentMessage string) {
	verifyCodes := make([]string, len(liveCodes))
	for i, liveCode := range liveCodes {
		verifyCodes[i] = liveCode.verifyCode
	}
	outcome := decideVerifyCodeTry(verifyCodes, self.tryCodes(liveCodes), guess)
	for _, i := range outcome.spent {
		killVerifyCode(self.ctx, liveCodes[i].userAuthVerifyId)
	}
	if 0 <= outcome.match {
		self.release()
		self.untryCodes()
		return &liveCodes[outcome.match], ""
	}
	if outcome.strike {
		strikes, lockout := self.address.strike(self.ctx, self.now)
		glog.Infof(
			"[auth]a verification code of user %s took %d wrong tries and was killed; strike %d of its address, lockout %s\n",
			userId.String(),
			VerifyCodeTryLimit,
			strikes,
			lockout,
		)
		if 0 < lockout {
			return nil, authVerifyCodeSpentLockedMessage(lockout)
		}
	}
	if 0 < len(outcome.spent) {
		return nil, AuthVerifyCodeSpentMessage
	}
	return nil, ""
}

// Kills a code that has taken its tries: it is marked used, so it never
// verifies, whatever becomes of its count in Redis, and is reaped with the used
// codes. The kill is recorded under the code's id, so the code still proves
// the password its sending request proved when the person who mistyped asks
// for another (liveVerifyCodeProvenInTx).
func killVerifyCode(ctx context.Context, userAuthVerifyId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE user_auth_verify SET used = true WHERE user_auth_verify_id = $1 AND used = false`,
			userAuthVerifyId,
		))
		if tag.RowsAffected() == 1 {
			insertAccountActionAttemptInTx(ctx, tx, userAuthVerifyId, AccountActionVerifyCodeKilled, server.NowUtc())
		}
	})
}

// The refusal of a new code for userAuth while the address is locked out, or
// nil (AuthVerifyCreateCode).
func verifyCodeSendRefusal(clientSession *session.ClientSession, userAuth string) error {
	return verifyCodeAddressOf(userAuth).lockRefusal(clientSession.Ctx, verifyCodeLimitNow(clientSession.Ctx))
}
