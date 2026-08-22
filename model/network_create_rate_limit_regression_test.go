package model

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Regression suite for the wrongful 429 / 503 refusals users hit on account
// creation. Each test is named for the user-visible outcome it prevents.
//
// POST /auth/network-create runs two different limiters:
//
//   - the seedphrase branch calls CheckNetworkCreateRateLimit
//     (model/network_create_rate_limit.go), 5 per client-address bucket per
//     rolling 24h, refused with "429 You have reached the maximum number of
//     account creations for today."
//   - the email / SSO / wallet branch calls UserAuthAttempt
//     (model/auth_model_attempt.go), 5 per bucket per rolling 5 minutes,
//     refused with "503 User auth attempts exceeded limits."
//
// Both bucket on server.ClientIpHash of the session's ClientAddress.

// two addresses in different /29 networks, so they are genuinely different
// buckets and not just different addresses inside one bucket
const (
	regressionClientA = "203.0.113.9:41001"
	regressionClientB = "198.51.100.20:41002"
	// same /29 as regressionClientA (203.0.113.8/29 spans .8-.15)
	regressionClientANeighbour = "203.0.113.14:41003"
	// first address of the next /29
	regressionClientANextSubnet = "203.0.113.16:41004"
)

func regressionSession(ctx context.Context, address string) *session.ClientSession {
	return session.NewLocalClientSession(ctx, address, nil)
}

// seedphraseCreate performs the no-auth-method signup: the branch guarded by
// CheckNetworkCreateRateLimit.
func seedphraseCreate(clientSession *session.ClientSession) (*NetworkCreateResult, error) {
	return NetworkCreate(NetworkCreateArgs{Terms: true}, clientSession)
}

func networkCreateAttemptRowCount(t testing.TB, ctx context.Context) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT COUNT(*) FROM network_create_attempt`)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				t.Fatal("network_create_attempt count returned no row")
			}
			server.Raise(result.Scan(&count))
		})
	})
	return count
}

func mustSeedphraseCreate(t testing.TB, clientSession *session.ClientSession, what string) {
	t.Helper()
	result, err := seedphraseCreate(clientSession)
	if err != nil {
		t.Fatalf("%s was refused: %v", what, err)
	}
	if result.Error != nil {
		t.Fatalf("%s failed: %s", what, result.Error.Message)
	}
	if result.Network == nil {
		t.Fatalf("%s returned no network and no error", what)
	}
}

// TestNetworkCreateBudgetIsNotSharedAcrossClientAddresses is the highest-value
// test in this file. If the address the limiter buckets on is anything other
// than the individual client -- the ingress proxy's own address, a constant, a
// too-coarse mask -- then a user signing up from their own connection is
// refused because five unrelated strangers signed up first, and the deployment
// accepts five account creations per 24h in total.
func TestNetworkCreateBudgetIsNotSharedAcrossClientAddresses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientA := regressionSession(ctx, regressionClientA)
		defer clientA.Cancel()
		clientB := regressionSession(ctx, regressionClientB)
		defer clientB.Cancel()

		for i := 0; i < NetworkCreateDailyLimit; i += 1 {
			mustSeedphraseCreate(t, clientA, fmt.Sprintf("signup %d of %d from client A", i+1, NetworkCreateDailyLimit))
		}

		// client A has spent its own budget; that part is intended
		if _, err := seedphraseCreate(clientA); err == nil {
			t.Fatalf(
				"client A was allowed signup %d after using its documented %d; the limiter is not being enforced at all",
				NetworkCreateDailyLimit+1,
				NetworkCreateDailyLimit,
			)
		}

		// the unrelated client must be completely unaffected
		result, err := seedphraseCreate(clientB)
		if err != nil {
			t.Fatalf(
				"a first-ever signup from client address %s was refused with %q after "+
					"%d signups from the unrelated address %s: the two clients share one "+
					"rate-limit budget, so the deployment accepts %d account creations in "+
					"total across all users",
				regressionClientB, err, NetworkCreateDailyLimit, regressionClientA, NetworkCreateDailyLimit,
			)
		}
		if result.Network == nil {
			t.Fatalf("client B signup returned no network: %+v", result)
		}
	})
}

// TestNetworkCreateAllowsTheDocumentedNumberOfSignups pins the limit at the
// documented number. An off-by-one that refuses the 5th signup would present to
// that user as a wrongful 429 while every constant and comment in the file
// still says 5.
func TestNetworkCreateAllowsTheDocumentedNumberOfSignups(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientA := regressionSession(ctx, regressionClientA)
		defer clientA.Cancel()

		for i := 0; i < NetworkCreateDailyLimit; i += 1 {
			result, err := seedphraseCreate(clientA)
			if err != nil {
				t.Fatalf(
					"signup %d of the documented %d was refused with %q; the limit is "+
						"being enforced one lower than it is documented",
					i+1, NetworkCreateDailyLimit, err,
				)
			}
			if result.Network == nil {
				t.Fatalf("signup %d returned no network: %+v", i+1, result)
			}
		}

		_, err := seedphraseCreate(clientA)
		if err == nil {
			t.Fatalf("signup %d was allowed; the documented limit of %d is not enforced",
				NetworkCreateDailyLimit+1, NetworkCreateDailyLimit)
		}
		if !strings.HasPrefix(err.Error(), "429 ") {
			t.Fatalf(
				"over-limit signup returned %q; a client-attributable refusal must carry "+
					"the 429 prefix that router.RaiseHttpError turns into HTTP 429",
				err,
			)
		}

		// A refused attempt must not itself be recorded. If it were, a client
		// retrying on the refusal would keep its own window permanently full and
		// could never recover, which is exactly the trap the Redis auth limiter
		// has.
		if count := networkCreateAttemptRowCount(t, ctx); count != NetworkCreateDailyLimit {
			t.Fatalf(
				"network_create_attempt holds %d rows after %d signups and 1 refusal, want %d: "+
					"a refused attempt extended the user's own 24h window",
				count, NetworkCreateDailyLimit, NetworkCreateDailyLimit,
			)
		}
	})
}

// TestNetworkCreateBucketsWholeIpv4SubnetsTogether pins the /29 (v4) bucketing
// in server.ClientIpHashForAddr.
//
// This is CURRENT, WRONGFUL behaviour deliberately pinned rather than asserted
// away: eight consecutive IPv4 addresses share one 5-per-24h budget, so the
// sixth person behind a carrier NAT, office, dorm or VPN egress to install the
// app in a day gets a 429 on their first ever request. Nothing they can do
// distinguishes them from the other occupants of the bucket. When the bucketing
// is narrowed, this test fails and should be rewritten to assert the neighbour
// succeeds.
func TestNetworkCreateBucketsWholeIpv4SubnetsTogether(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientA := regressionSession(ctx, regressionClientA)
		defer clientA.Cancel()
		neighbour := regressionSession(ctx, regressionClientANeighbour)
		defer neighbour.Cancel()
		nextSubnet := regressionSession(ctx, regressionClientANextSubnet)
		defer nextSubnet.Cancel()

		for i := 0; i < NetworkCreateDailyLimit; i += 1 {
			mustSeedphraseCreate(t, clientA, fmt.Sprintf("signup %d from %s", i+1, regressionClientA))
		}

		if _, err := seedphraseCreate(neighbour); err == nil {
			t.Fatalf(
				"%s was allowed to sign up after %s used the budget; the /29 bucketing in "+
					"server.ClientIpHashForAddr has changed -- update the note on this test",
				regressionClientANeighbour, regressionClientA,
			)
		} else if !strings.HasPrefix(err.Error(), "429 ") {
			t.Fatalf("neighbour refusal was %q, want a 429-prefixed message", err)
		}

		// the next /29 up is a different bucket
		mustSeedphraseCreate(t, nextSubnet, "signup from the next /29")
	})
}

// TestSeedphraseSignupRefusedForTermsDoesNotConsumeBudget: a user who submits
// the signup form without ticking the terms box has created nothing. That
// refusal must not spend one of their five daily slots -- otherwise five
// mistakes lock the address out for 24 hours with zero accounts created, and
// the client cannot tell the two refusals apart.
func TestSeedphraseSignupRefusedForTermsDoesNotConsumeBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientA := regressionSession(ctx, regressionClientA)
		defer clientA.Cancel()

		const termsMistakes = 3
		for i := 0; i < termsMistakes; i += 1 {
			result, err := NetworkCreate(NetworkCreateArgs{Terms: false}, clientA)
			if err != nil {
				t.Fatalf("terms-not-accepted signup %d returned a transport error: %v", i+1, err)
			}
			if result.Error == nil || result.Error.Message != AgreeToTerms {
				t.Fatalf("terms-not-accepted signup %d returned %+v, want the AgreeToTerms body error", i+1, result)
			}
		}

		if count := networkCreateAttemptRowCount(t, ctx); count != 0 {
			t.Fatalf(
				"%d rate-limit attempts were recorded for %d signups that were refused "+
					"before anything was created: an ordinary form mistake now spends the "+
					"user's daily account-creation budget",
				count, termsMistakes,
			)
		}

		// and the user still has their full documented budget
		for i := 0; i < NetworkCreateDailyLimit; i += 1 {
			mustSeedphraseCreate(t, clientA, fmt.Sprintf("signup %d after %d terms mistakes", i+1, termsMistakes))
		}
	})
}

// TestEmailSignupValidationMistakesDoNotConsumeTheAuthBudget.
//
// WHAT THIS USED TO PIN. model.NetworkCreate consumed UserAuthAttempt at
// network_model.go:217, BEFORE the terms check (:222) and before network-name
// validation (:231). Every ordinary form mistake therefore spent one of five
// slots in a 5-minute window, and the email branch never calls
// SetUserAuthAttemptSuccess to drain it. Four mistakes and the user's next --
// correct -- submission was refused with "503 User auth attempts exceeded
// limits.": a server-fault status that tells every retrying client to try
// again, and each retry records another attempt. This test asserted exactly
// that, on the grounds that the bug should be visible until it was fixed.
//
// WHY THAT WAS WRONG. A user's form mistake is not an authentication attempt.
// Charging it spent a budget that, for a signup carrying no user auth, is
// shared with every other client on the same address -- so one person fumbling
// a form refused strangers.
//
// The limiter now runs after input validation (model/network_model.go), so the
// assertion is inverted: the corrected submission must go through.
func TestEmailSignupValidationMistakesDoNotConsumeTheAuthBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientA := regressionSession(ctx, regressionClientA)
		defer clientA.Cancel()

		userAuth := "regression-burned@example.com"
		password := "SomeValidPassword123!"

		// more mistakes than the whole 5-minute budget, to show none are charged
		mistakes := AttemptFailedCountThreshold + 2
		for i := 0; i < mistakes; i += 1 {
			result, err := NetworkCreate(NetworkCreateArgs{
				UserAuth: &userAuth,
				Password: &password,
				Terms:    false,
			}, clientA)
			if err != nil {
				t.Fatalf(
					"terms mistake %d was refused with %q instead of returning the "+
						"AgreeToTerms body error: an unticked terms box is spending the "+
						"auth-attempt budget again",
					i+1, err,
				)
			}
			if result.Error == nil || result.Error.Message != AgreeToTerms {
				t.Fatalf("terms mistake %d returned %+v, want the AgreeToTerms body error", i+1, result)
			}
		}

		// a network name that is already taken is the same class of mistake
		result, err := NetworkCreate(NetworkCreateArgs{
			UserAuth:    &userAuth,
			Password:    &password,
			NetworkName: "a",
			Terms:       true,
		}, clientA)
		if err != nil {
			t.Fatalf("an invalid network name was refused with %q, want a body error", err)
		}
		if result.Error == nil {
			t.Fatalf("an invalid network name returned %+v, want a body error", result)
		}

		result, err = NetworkCreate(NetworkCreateArgs{
			UserAuth:    &userAuth,
			Password:    &password,
			NetworkName: "regressionnet",
			Terms:       true,
		}, clientA)
		if err != nil {
			t.Fatalf(
				"the corrected signup after %d form mistakes was refused with %q: "+
					"pre-validation refusals are charging the auth-attempt limiter again, "+
					"so a user's own typing locks them out",
				mistakes, err,
			)
		}
		if result.Error != nil {
			t.Fatalf("the corrected signup returned the body error %q", result.Error.Message)
		}
		if result.VerificationRequired == nil {
			t.Fatalf("the corrected signup returned %+v, want a verification-required result", result)
		}
	})
}

// TestAuthAttemptLimitIsReportedAsAClientError.
//
// WHAT THIS REPLACES. maxUserAuthAttemptsError used to return
// "503 User auth attempts exceeded limits." A 5xx tells every well-behaved
// client and SDK that the server is broken and the request should be retried;
// every retry records another attempt (model/auth_model_attempt.go
// authAttemptScript), so the status itself consumed the remaining budget faster
// and the condition reinforced itself. Rate limiting is client-attributable.
//
// The message is also checked here. "User auth attempts exceeded limits" told a
// user nothing about why they, personally, were refused -- and for a signup
// with no user auth the budget is not theirs at all, it is the whole address's.
func TestAuthAttemptLimitIsReportedAsAClientError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientA := regressionSession(ctx, regressionClientA)
		defer clientA.Cancel()

		for i := 0; i < AttemptFailedCountThreshold; i += 1 {
			UserAuthAttempt(nil, clientA)
		}

		authJwt := "sso-token-for-a-first-time-user"
		authJwtType := "google"
		_, err := NetworkCreate(NetworkCreateArgs{
			AuthJwt:     &authJwt,
			AuthJwtType: &authJwtType,
			NetworkName: "overbudgetnet",
			Terms:       true,
		}, clientA)
		if err == nil {
			t.Fatal("an over-budget signup was allowed; the auth-attempt limiter is not enforced")
		}
		if strings.HasPrefix(err.Error(), "503 ") {
			t.Fatalf(
				"the auth-attempt limit still answers %q. A 5xx tells every SDK to retry, "+
					"and each retry records another attempt, so the status makes the "+
					"refusal self-reinforcing",
				err,
			)
		}
		if !strings.HasPrefix(err.Error(), "429 ") {
			t.Fatalf(
				"the auth-attempt limit answers %q; a client-attributable refusal must "+
					"carry the 429 prefix router.RaiseHttpError turns into the status",
				err,
			)
		}
		if strings.Contains(err.Error(), "User auth attempts exceeded limits") {
			t.Fatalf("the refusal still uses the old opaque message: %q", err)
		}
		if !strings.Contains(err.Error(), "address") {
			t.Fatalf(
				"an identity-less auth-attempt refusal reads %q. That budget is shared by "+
					"everyone at the client address, so the message has to say the limit is "+
					"address-scoped -- otherwise support cannot tell a wrongly-refused "+
					"stranger from abuse",
				err,
			)
		}

		// and it carries a machine-readable wait
		var retryAfter interface{ RetryAfterSeconds() int }
		if !errors.As(err, &retryAfter) {
			t.Fatalf("the auth-attempt refusal %q carries no retry hint for Retry-After", err)
		}
		if seconds := retryAfter.RetryAfterSeconds(); seconds <= 0 {
			t.Fatalf("the auth-attempt refusal reported RetryAfterSeconds()=%d", seconds)
		}
	})
}

// TestAccountLimitRefusalIsHonestAboutItsScope covers item 4 on the
// account-creation limiter.
//
// The message used to read "You have reached the maximum number of account
// creations for today." The recipient has, in the overwhelming majority of
// cases, created none: the budget is keyed on a subnet of the client address
// (server.ClientIpHashForAddr), so a user on a shared connection is refused for
// what other people did and then told it was their own doing.
func TestAccountLimitRefusalIsHonestAboutItsScope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientA := regressionSession(ctx, regressionClientA)
		defer clientA.Cancel()

		for i := 0; i < NetworkCreateDailyLimit; i += 1 {
			mustSeedphraseCreate(t, clientA, fmt.Sprintf("signup %d", i+1))
		}

		_, err := seedphraseCreate(clientA)
		if err == nil {
			t.Fatal("the over-limit signup was allowed")
		}
		if strings.Contains(err.Error(), "You have reached the maximum number of account creations") {
			t.Fatalf(
				"the refusal still accuses the caller of creating the accounts: %q. The "+
					"budget is address-scoped, so the usual recipient created none of them",
				err,
			)
		}
		if !strings.Contains(err.Error(), "address") {
			t.Fatalf(
				"the account-creation refusal reads %q and never says the limit is scoped "+
					"to the network address; support cannot tell these apart from abuse",
				err,
			)
		}

		var retryAfter interface{ RetryAfterSeconds() int }
		if !errors.As(err, &retryAfter) {
			t.Fatalf("the account-creation refusal %q carries no retry hint for Retry-After", err)
		}
		seconds := retryAfter.RetryAfterSeconds()
		if seconds <= 0 || int(NetworkCreateDailyWindow/time.Second) < seconds {
			t.Fatalf(
				"the account-creation refusal reported RetryAfterSeconds()=%d, want a "+
					"positive value no larger than the %s window",
				seconds, NetworkCreateDailyWindow,
			)
		}
	})
}

// TestAccountLimitRetryHintTracksTheOldestAttempt is the assertion that tells
// the documented Retry-After apart from a flat restatement of the window.
//
// CheckNetworkCreateRateLimit computes the hint in SQL, in the same statement
// and against the same clock as the count:
//
//	COALESCE(CEIL(EXTRACT(EPOCH FROM (MIN(create_time) + INTERVAL '1 seconds' * $2 - now())))::bigint, 0)
//
// and the comment above it promises "the real remaining time on the window: the
// oldest attempt still counted expires then, freeing exactly one slot." Nothing
// asserted the VALUE. The test above only requires 0 < seconds <= window, and
// api/handlers only requires that the header parses and is positive, so a flat
// `NetworkCreateDailyWindow`, or arithmetic broken by the timestamp /
// timestamptz mix in that expression, passed the entire suite -- and a client
// told to come back in 24 hours when a slot frees in 4 either waits a day it
// did not have to or hammers the endpoint because the hint was obviously wrong.
//
// So the attempts are backdated by a known amount and the hint has to follow
// them. The two candidate answers are 20 hours apart, which is far outside any
// tolerance the clock needs.
func TestAccountLimitRetryHintTracksTheOldestAttempt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientA := regressionSession(ctx, regressionClientA)
		defer clientA.Cancel()

		clientAddressHash, _, err := clientA.ClientAddressHashPort()
		if err != nil {
			t.Fatal(err)
		}

		// fill the budget with attempts made 20 hours ago, so ~4h of the 24h
		// window remains on the oldest one
		const aged = 20 * time.Hour
		agedAt := server.NowUtc().Add(-aged)
		server.Tx(ctx, func(tx server.PgTx) {
			for i := 0; i < NetworkCreateDailyLimit; i += 1 {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_create_attempt
						(network_create_attempt_id, client_address_hash, create_time)
						VALUES ($1, $2, $3)
					`,
					server.NewId(),
					clientAddressHash[:],
					// spread them so MIN() has something to choose
					agedAt.Add(time.Duration(i)*time.Minute),
				))
			}
		})

		err = CheckNetworkCreateRateLimit(ctx, clientA)
		if err == nil {
			t.Fatal("the over-limit call was allowed; the backdated attempts are still inside the window")
		}
		var retryAfter interface{ RetryAfterSeconds() int }
		if !errors.As(err, &retryAfter) {
			t.Fatalf("the account-creation refusal %q carries no retry hint for Retry-After", err)
		}

		seconds := retryAfter.RetryAfterSeconds()
		want := int((NetworkCreateDailyWindow - aged) / time.Second)
		flat := int(NetworkCreateDailyWindow / time.Second)
		// generous: absorbs the test's own runtime and any clock skew between
		// this process and postgres, while staying 19 hours clear of `flat`
		const tolerance = 15 * 60
		if seconds < want-tolerance || want+tolerance < seconds {
			t.Fatalf(
				"%d attempts made %s ago produced Retry-After = %d seconds, want ~%d (the "+
					"time left on the OLDEST attempt). %d would be a flat restatement of the "+
					"%s window, which is what this test exists to rule out",
				NetworkCreateDailyLimit, aged, seconds, want, flat, NetworkCreateDailyWindow,
			)
		}
	})
}

// TestAbandonedSsoSignupsDoNotSpendTheAddressWideAuthBudget pins the sharpest
// wrongful 503 in the report, from the other side.
//
// THE MECHANISM IS UNCHANGED AND IS STILL PINNED BELOW. model/network_model.go
// parses userAuth from networkCreate.UserAuth, and NormalUserAuthV1(nil)
// returns nil. SSO and wallet signups send no user_auth at all, so nil is what
// reaches UserAuthAttempt -- and userAuthAttemptRedisKeys' nil branch builds a
// key with NO user component. Every SSO and every wallet signup from one public
// address really does share a single bucket of 5 per 5 minutes. That bucketing
// is deliberate anti-abuse policy and is NOT changed here; the key shape is
// asserted so it cannot drift.
//
// WHAT THIS USED TO PIN. Because the limiter ran before the terms check, four
// people on one public address who opened the signup form and did not tick the
// box would exhaust that shared bucket, and the fifth -- a first-time user who
// had done nothing -- was refused with "503 User auth attempts exceeded
// limits." The old test asserted that refusal.
//
// WHY THAT WAS WRONG. Abandoning a form is not an authentication attempt.
// Spending a shared budget on it turns other people's hesitation into a refusal
// for a stranger, and the shared bucket makes it invisible to everyone
// involved. The bucket is not widened; it is simply no longer charged for
// submissions that were never going to create anything.
func TestAbandonedSsoSignupsDoNotSpendTheAddressWideAuthBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// the key shape itself: no identity, so no per-user separation. This is
		// current, intended behaviour, pinned so a change is deliberate.
		addressHashHex := hex.EncodeToString([]byte("0123456789abcdef"))
		addressKey, globalKey := userAuthAttemptRedisKeys(nil, addressHashHex)
		if globalKey != "" {
			t.Fatalf("identity-less auth attempt built a global key %q; the nil branch changed", globalKey)
		}
		if !strings.Contains(addressKey, addressHashHex) {
			t.Fatalf("identity-less auth attempt key %q does not contain the address hash", addressKey)
		}
		if strings.Contains(addressKey, "user_") {
			t.Fatalf(
				"identity-less auth attempt key %q now carries a user component; the "+
					"address-wide bucketing changed -- that is a limiter policy decision, "+
					"update the note on this test",
				addressKey,
			)
		}

		clientA := regressionSession(ctx, regressionClientA)
		defer clientA.Cancel()

		// more abandoned signups than the whole shared bucket holds
		attempts := AttemptFailedCountThreshold + 2
		for i := 0; i < attempts; i += 1 {
			// a distinct person each time: SSO carries no user_auth, so nothing
			// in the request distinguishes them to the limiter
			authJwt := fmt.Sprintf("sso-token-for-person-%d", i)
			authJwtType := "google"
			result, err := NetworkCreate(NetworkCreateArgs{
				AuthJwt:     &authJwt,
				AuthJwtType: &authJwtType,
				Terms:       false,
			}, clientA)
			if err != nil {
				t.Fatalf(
					"abandoned sso signup %d was refused with %q instead of returning a "+
						"body error: an unticked terms box is spending the address-wide "+
						"bucket again",
					i+1, err,
				)
			}
			if result.Error == nil || result.Error.Message != AgreeToTerms {
				t.Fatalf("sso signup %d returned %+v, want the AgreeToTerms body error", i+1, result)
			}
		}

		newcomerJwt := "sso-token-for-a-first-time-user"
		newcomerJwtType := "google"
		_, err := NetworkCreate(NetworkCreateArgs{
			AuthJwt:     &newcomerJwt,
			AuthJwtType: &newcomerJwtType,
			NetworkName: "newcomernet",
			Terms:       true,
		}, clientA)
		// The SSO token above is not a real signed JWT, so this submission
		// cannot reach a created account: ParseAuthJwt returns nil and
		// NetworkCreate falls through to "invalid login". What matters is that
		// it got PAST the limiter -- a rate-limit refusal is a 429 and would
		// have been returned before the branch dispatch was ever reached.
		if err != nil && strings.HasPrefix(err.Error(), "429 ") {
			t.Fatalf(
				"a first-time SSO signup from %s was rate limited with %q after %d "+
					"unrelated people abandoned the form from the same address: abandoned "+
					"forms are spending the shared bucket again, and this user did nothing",
				regressionClientA, err, attempts,
			)
		}
	})
}

// TestValidSignupsStillSpendTheAuthBudget is the abuse-side counterpart to the
// test above, and the reason moving the limiter is not a weakening.
//
// Only pre-validation refusals stopped being charged. A submission that is
// well-formed -- the shape an attacker actually sends -- still consumes a slot,
// and the address-wide bucket still refuses the caller at the documented count.
func TestValidSignupsStillSpendTheAuthBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientB := regressionSession(ctx, regressionClientB)
		defer clientB.Cancel()

		// These tokens are not real signed JWTs, so each submission ends at
		// NetworkCreate's "invalid login" fallthrough -- which is BELOW the
		// limiter, so each one is charged exactly as a genuine attempt is. That
		// is the shape an attacker sends: well-formed input, no account.
		//
		// A refusal that costs nothing is interleaved before every charged
		// submission. This is the assertion that discriminates the fix from the
		// bug in the ABUSE direction: if the limiter moved back above input
		// validation, those free refusals would be charged too and the count
		// below would come out at half. Making pre-validation refusals free
		// must not hand an attacker a way to stretch the budget.
		authJwtType := "google"
		rateLimitedAt := 0
		for i := 0; i < AttemptFailedCountThreshold+1; i += 1 {
			skipped := "sso-token-abandoned"
			if _, err := NetworkCreate(NetworkCreateArgs{
				AuthJwt:     &skipped,
				AuthJwtType: &authJwtType,
				Terms:       false,
			}, clientB); err != nil {
				t.Fatalf("interleaved terms refusal %d was itself rate limited: %v", i+1, err)
			}

			authJwt := fmt.Sprintf("sso-token-attacker-%d", i)
			networkName := fmt.Sprintf("attackernet%d", i)
			_, err := NetworkCreate(NetworkCreateArgs{
				AuthJwt:     &authJwt,
				AuthJwtType: &authJwtType,
				NetworkName: networkName,
				Terms:       true,
			}, clientB)
			if err != nil && strings.HasPrefix(err.Error(), "429 ") {
				rateLimitedAt = i + 1
				break
			}
		}
		if rateLimitedAt == 0 {
			t.Fatalf(
				"%d well-formed submissions from one address were all admitted past the "+
					"limiter; it no longer charges submissions that reach the create "+
					"branches, so moving it below input validation has weakened it",
				AttemptFailedCountThreshold+1,
			)
		}
		// authAttemptScript records the attempt and then admits it only when the
		// resulting count is strictly below the threshold, so AttemptFailedCountThreshold-1
		// are admitted and the AttemptFailedCountThreshold'th is refused. That
		// is pre-existing limiter behaviour, unchanged by this fix and pinned
		// here so a change to it is deliberate.
		if rateLimitedAt != AttemptFailedCountThreshold {
			t.Fatalf(
				"well-formed submissions were rate limited at %d, want %d: the limit is "+
					"enforced at a different count than it is written",
				rateLimitedAt, AttemptFailedCountThreshold,
			)
		}
	})
}
