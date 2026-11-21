package model

// The verification code limits' records in Redis, at a clock the test sets
// (auth_verify_code_limit_db_test.go drives them through AuthVerify).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// An address's strikes lock it out for the ladder's lockouts, the fifth and
// every later one for a day. A strike leaves the count a week after it was
// made, and verifying the address clears its strikes and its lockout.
func TestVerifyCodeAddressStrikesEscalateAndAge(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		address := verifyCodeAddressOf(emailClaimTestAddress("strikes"))
		now := server.NowUtc()

		for i, want := range []time.Duration{0, 15 * time.Minute, time.Hour, 4 * time.Hour, 24 * time.Hour, 24 * time.Hour} {
			strikes, lockout := address.strike(ctx, now)
			if wantStrikes := min(i+1, len(verifyCodeLockouts)); strikes != wantStrikes || lockout != want {
				t.Fatalf("strike %d: %d strikes, lockout %s; want %d, %s", i+1, strikes, lockout, wantStrikes, want)
			}
			if got := address.lockedFor(ctx, now); got != want {
				t.Fatalf("strike %d: locked out for %s, want %s", i+1, got, want)
			}
			if got := address.lockedFor(ctx, now.Add(want)); got != 0 {
				t.Fatalf("strike %d: still locked out for %s at the lockout's end", i+1, got)
			}
		}

		later := now.Add(VerifyCodeStrikeWindow + time.Millisecond)
		if strikes, lockout := address.strike(ctx, later); strikes != 1 || lockout != 0 {
			t.Fatalf("a week later: %d strikes, lockout %s; want the first strike again", strikes, lockout)
		}
		if strikes, lockout := address.strike(ctx, later); strikes != 2 || lockout != 15*time.Minute {
			t.Fatalf("a week later, again: %d strikes, lockout %s", strikes, lockout)
		}
		address.clear(ctx)
		if got := address.lockedFor(ctx, later); got != 0 {
			t.Fatalf("locked out for %s after the address was verified", got)
		}
		if strikes, lockout := address.strike(ctx, later); strikes != 1 || lockout != 0 {
			t.Fatalf("after the verification: %d strikes, lockout %s; want the first strike again", strikes, lockout)
		}
	})
}

// A try takes one of its code's tries before its guess is compared: with the
// code's five tries taken by tries still in flight, a sixth try is not
// compared, though its guess is the code, and the code is killed.
func TestVerifyCodeTriesAreTakenBeforeTheGuessIsCompared(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sources := &verifyCodeTestSources{ctx: ctx}
		userAuth := emailClaimTestAddress("taken")
		own := sources.session(nil)
		_, userId := emailClaimSignUp(t, ctx, own, userAuth, "own-password")
		code := emailClaimProvenCode(t, own, userAuth, &userId)

		var liveCode liveVerifyCode
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT user_auth_verify_id, verify_code FROM user_auth_verify WHERE user_id = $1 AND NOT used`,
				userId,
			).Scan(&liveCode.userAuthVerifyId, &liveCode.verifyCode))
		})
		for i := 0; i < VerifyCodeTryLimit; i++ {
			newVerifyCodeTryLimits(sources.session(nil), userAuth).tryCodes([]liveVerifyCode{liveCode})
		}

		match, message := newVerifyCodeTryLimits(sources.session(nil), userAuth).tryGuess(userId, []liveVerifyCode{liveCode}, code)
		if match != nil || message != AuthVerifyCodeSpentMessage {
			t.Fatalf("the sixth try: match %+v, message %q", match, message)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM user_auth_verify WHERE user_id = $1 AND NOT used`, userId); count != 0 {
			t.Fatalf("the code was not killed")
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_action_attempt WHERE user_id = $1 AND action = $2`, liveCode.userAuthVerifyId, AccountActionVerifyCodeKilled); count != 1 {
			t.Fatalf("the kill was recorded %d times, want once", count)
		}
	})
}

// A guess the try finds right gives its places in the caller's budgets back:
// right codes do not spend a client network's wrong tries.
func TestVerifyCodeRightGuessGivesBackItsPlaces(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sources := &verifyCodeTestSources{ctx: ctx}
		userAuth := emailClaimTestAddress("places")
		own := sources.session(nil)
		_, userId := emailClaimSignUp(t, ctx, own, userAuth, "own-password")
		code := emailClaimProvenCode(t, own, userAuth, &userId)

		var liveCode liveVerifyCode
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT user_auth_verify_id, verify_code FROM user_auth_verify WHERE user_id = $1 AND NOT used`,
				userId,
			).Scan(&liveCode.userAuthVerifyId, &liveCode.verifyCode))
		})
		caller := sources.session(nil)
		places := func(key string) int64 {
			var count int64
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				count, err = r.ZCard(ctx, key).Result()
				server.Raise(err)
			})
			return count
		}

		limits := newVerifyCodeTryLimits(caller, userAuth)
		if limits.sourceKey == "" {
			t.Fatalf("the caller has no client network budget")
		}
		if err := limits.admit(); err != nil {
			t.Fatalf("admit: %v", err)
		}
		if got := places(limits.sourceKey); got != 1 {
			t.Fatalf("an admitted try holds %d places, want 1", got)
		}
		if match, _ := limits.tryGuess(userId, []liveVerifyCode{liveCode}, code); match == nil {
			t.Fatalf("the right guess did not match")
		}
		if got := places(limits.sourceKey); got != 0 {
			t.Fatalf("the right guess kept %d places", got)
		}

		wrong := newVerifyCodeTryLimits(caller, userAuth)
		if err := wrong.admit(); err != nil {
			t.Fatalf("admit: %v", err)
		}
		if match, _ := wrong.tryGuess(userId, []liveVerifyCode{liveCode}, verifyCodeTestWrongGuess(code, 0)); match != nil {
			t.Fatalf("the wrong guess matched")
		}
		if got := places(wrong.sourceKey); got != 1 {
			t.Fatalf("the wrong guess holds %d places, want 1", got)
		}
	})
}
