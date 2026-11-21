package model

// The verification code limits (auth_verify_code_limit.go) against the
// database and Redis, through what the routes call: AuthVerifyCreateCode for a
// code (/auth/verify-send, and the sign-up and password sign-in that send one)
// and AuthVerify for a try (/auth/verify). Besides the limits' constants and
// their test clock, the tests use only what the code before the limits has,
// and they fail there. The root causes:
//   - a code took any number of wrong tries: spread over client networks of
//     their own, which the auth attempt limits do not join, five wrong tries
//     left the right code good;
//   - nothing locked an address out, however many of its codes were guessed
//     at;
//   - a caller's wrong tries across addresses were bounded by nothing, from one
//     client network or signed in to one network.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A clock the limits read (verifyCodeLimitClockKey), moved by the test so a
// lockout ends without waiting for it.
type verifyCodeTestClock struct {
	mutex sync.Mutex
	now   time.Time
}

func newVerifyCodeTestClock() *verifyCodeTestClock {
	return &verifyCodeTestClock{now: server.NowUtc()}
}

func (self *verifyCodeTestClock) advance(d time.Duration) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.now = self.now.Add(d)
}

func (self *verifyCodeTestClock) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, verifyCodeLimitClockKey{}, func() time.Time {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		return self.now
	})
}

// Requests from client networks of their own (documentation /56s), so the
// auth attempt limits of one client network, five tries in five minutes,
// never refuse a step: the limits under test are the ones across client
// networks.
type verifyCodeTestSources struct {
	ctx  context.Context
	next int
}

func (self *verifyCodeTestSources) address() string {
	self.next += 1
	return fmt.Sprintf("[2001:db8:%x:%02x00::1]:4000", 0xc000+self.next/256, self.next%256)
}

func (self *verifyCodeTestSources) session(byJwt *session.ByJwt) *session.ClientSession {
	clientSession := session.Testing_CreateClientSession(self.ctx, byJwt)
	clientSession.ClientAddress = self.address()
	return clientSession
}

// A request that presents bearer, a network or client token.
func (self *verifyCodeTestSources) signedIn(bearer string) *session.ClientSession {
	clientSession := self.session(nil)
	clientSession.Header["Authorization"] = []string{"Bearer " + bearer}
	return clientSession
}

// The ith wrong guess of code: its last digit moved by 1+i (i below 15), so
// every guess differs from the code and from the others.
func verifyCodeTestWrongGuess(code string, i int) string {
	const digits = "0123456789abcdef"
	last := strings.IndexByte(digits, code[len(code)-1])
	return code[:len(code)-1] + string(digits[(last+1+i)%len(digits)])
}

func verifyCodeTestTry(clientSession *session.ClientSession, userAuth string, code string) (*AuthVerifyResult, error) {
	return AuthVerify(AuthVerifyArgs{UserAuth: userAuth, VerifyCode: code}, clientSession)
}

// count wrong guesses of code, each from a client network of its own, as a
// caller that presents bearer ("" for none). Returns their answers.
func verifyCodeTestWrongTries(t testing.TB, sources *verifyCodeTestSources, bearer string, userAuth string, code string, count int) []string {
	t.Helper()
	replies := []string{}
	for i := 0; i < count; i++ {
		clientSession := sources.session(nil)
		if bearer != "" {
			clientSession = sources.signedIn(bearer)
		}
		result := emailClaimVerify(t, clientSession, userAuth, verifyCodeTestWrongGuess(code, i))
		if result.Network != nil || result.Error == nil {
			t.Fatalf("wrong guess %d of %s was answered %+v", i+1, userAuth, result)
		}
		replies = append(replies, result.Error.Message)
	}
	return replies
}

// A code as /auth/verify-send sends one, or as a sign-up or password sign-in
// does when passwordUserId is set.
func verifyCodeTestSend(clientSession *session.ClientSession, userAuth string, codeType VerifyCodeType, passwordUserId *server.Id) (string, error) {
	result, err := AuthVerifyCreateCode(
		AuthVerifyCreateCodeArgs{UserAuth: userAuth, CodeType: codeType, PasswordUserId: passwordUserId},
		clientSession,
	)
	if err != nil {
		return "", err
	}
	if result == nil || result.VerifyCode == nil {
		return "", fmt.Errorf("no code: %+v", result)
	}
	return *result.VerifyCode, nil
}

// err is a 429 refusal that names text and asks for wait in Retry-After.
func verifyCodeTestRequireRefusal(t testing.TB, err error, wait time.Duration, text string) {
	t.Helper()
	var rateLimit interface{ RetryAfterSeconds() int }
	if err == nil || !strings.HasPrefix(err.Error(), "429 ") || !strings.Contains(err.Error(), text) || !errors.As(err, &rateLimit) {
		t.Fatalf("err = %v, want a 429 refusal %q", err, text)
	}
	if got, want := rateLimit.RetryAfterSeconds(), int(wait/time.Second); got != want {
		t.Fatalf("retry after %d s, want %d s: %v", got, want, err)
	}
}

// A US phone number of its own.
func verifyCodeTestPhone() string {
	id := server.NewId()
	n := binary.BigEndian.Uint32(id.Bytes()[12:16])%9000000 + 1000000
	return fmt.Sprintf("+1 650 %03d %04d", n/10000, n%10000)
}

// The fifth wrong try of a code kills it: it answers that the code no longer
// works, and the right code is refused from then on. Before, the fifth wrong
// try was "Invalid code." like the others and the right code still verified.
// So on every path a code verifies an email or phone: an email or a phone
// sign-up, an email added to an account (verified signed in), and the
// password sign-in that verifies an abandoned sign-up after its reset.
func TestVerifyCodeDiesAtItsFifthWrongTry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sources := &verifyCodeTestSources{ctx: ctx}
		password := "own-password"

		type verifyPath struct {
			name     string
			userAuth string
			userId   server.Id
			code     string
			// who would verify the right code: the sign-up's own network, the
			// account signed in, the owner after the reset
			verifier *session.ClientSession
			// the token the wrong tries present, "" for none
			bearer string
		}
		signUp := func(name string, userAuth string, codeType VerifyCodeType) verifyPath {
			own := sources.session(nil)
			_, userId := emailClaimSignUp(t, ctx, own, userAuth, password)
			code, err := verifyCodeTestSend(own, userAuth, codeType, &userId)
			if err != nil {
				t.Fatalf("%s: the sign-up's code: %v", name, err)
			}
			return verifyPath{name: name, userAuth: userAuth, userId: userId, code: code, verifier: own}
		}
		added := func() verifyPath {
			_, userId, signed, byJwt := emailClaimSeedphraseAccount(t, ctx)
			userAuth := emailClaimTestAddress("dies-added")
			emailClaimAddAuth(t, ctx, byJwt, userAuth, password)
			code := emailClaimCode(t, ctx, sources.signedIn(signed), userAuth)
			return verifyPath{name: "added email", userAuth: userAuth, userId: userId, code: code, verifier: sources.signedIn(signed), bearer: signed}
		}
		reset := func() verifyPath {
			userAuth := emailClaimTestAddress("dies-reset")
			_, userId := emailClaimSignUp(t, ctx, sources.session(nil), userAuth, "claimant-password")
			emailClaimAge(ctx, userAuth)
			owner := sources.session(nil)
			resetCode, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, owner)
			if err != nil || resetCode == nil || resetCode.ResetCode == nil {
				t.Fatalf("reset: no reset code: %+v %v", resetCode, err)
			}
			set, err := AuthPasswordSet(AuthPasswordSetArgs{ResetCode: *resetCode.ResetCode, Password: password}, owner)
			if err != nil || set == nil || set.Error != nil {
				t.Fatalf("reset: the reset failed: %+v %v", set, err)
			}
			signIn := emailClaimPasswordSignIn(t, owner, userAuth, password)
			if signIn.VerificationRequired == nil {
				t.Fatalf("reset: the owner's sign-in did not ask for the verification: %+v", signIn)
			}
			code := emailClaimProvenCode(t, owner, userAuth, signIn.VerificationRequired.PasswordUserId)
			return verifyPath{name: "sign-in after a reset", userAuth: userAuth, userId: userId, code: code, verifier: owner}
		}

		for _, path := range []verifyPath{
			signUp("email sign-up", emailClaimTestAddress("dies"), VerifyCodeDefault),
			signUp("phone sign-up", verifyCodeTestPhone(), VerifyCodeNumeric),
			added(),
			reset(),
		} {
			replies := verifyCodeTestWrongTries(t, sources, path.bearer, path.userAuth, path.code, VerifyCodeTryLimit)

			right := emailClaimVerify(t, path.verifier, path.userAuth, path.code)
			if right.Network != nil {
				t.Fatalf("%s: the right code verified after %d wrong tries", path.name, VerifyCodeTryLimit)
			}
			if right.Error == nil || right.Error.Message != "Invalid code." {
				t.Fatalf("%s: the right code of the killed code was answered %+v", path.name, right.Error)
			}
			for i, reply := range replies[:VerifyCodeTryLimit-1] {
				if reply != "Invalid code." {
					t.Fatalf("%s: wrong try %d was answered %q", path.name, i+1, reply)
				}
			}
			if reply := replies[VerifyCodeTryLimit-1]; reply != AuthVerifyCodeSpentMessage {
				t.Fatalf("%s: the fifth wrong try was answered %q, want %q", path.name, reply, AuthVerifyCodeSpentMessage)
			}
			normalUserAuth, _ := NormalUserAuth(path.userAuth)
			if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_id = $1 AND user_auth = $2 AND verified`, path.userId, normalUserAuth); count != 0 {
				t.Fatalf("%s: the address was verified", path.name)
			}
		}
	})
}

// Each code of an address killed by wrong tries is a strike against it. The
// first locks nothing out, so a person who mistyped asks for a new code at
// once; the second, third and fourth lock the address out for 15 minutes, an
// hour and four hours. While it is locked out no code of it is tried, not even
// the right one, and none is sent: each is refused with a 429 that names the
// wait. When the lockout ends, the right code of a new code verifies. Before,
// the right code verified at every step.
func TestVerifyCodeLockoutsEscalate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clock := newVerifyCodeTestClock()
		ctx := clock.context(context.Background())
		sources := &verifyCodeTestSources{ctx: ctx}
		userAuth := emailClaimTestAddress("ladder")
		emailClaimSignUp(t, ctx, sources.session(nil), userAuth, "own-password")
		const lockedText = "for this email or phone"

		firstReply := ""
		for i, lockout := range []time.Duration{0, 15 * time.Minute, time.Hour, 4 * time.Hour} {
			strike := i + 1
			code, err := verifyCodeTestSend(sources.session(nil), userAuth, VerifyCodeNumeric, nil)
			if err != nil {
				t.Fatalf("strike %d: the code was not sent: %v", strike, err)
			}
			replies := verifyCodeTestWrongTries(t, sources, "", userAuth, code, VerifyCodeTryLimit)
			if lockout == 0 {
				firstReply = replies[VerifyCodeTryLimit-1]
				continue
			}

			// locked out: the right code is not tried, and no code is sent
			_, err = verifyCodeTestTry(sources.session(nil), userAuth, code)
			verifyCodeTestRequireRefusal(t, err, lockout, lockedText)
			_, err = verifyCodeTestSend(sources.session(nil), userAuth, VerifyCodeNumeric, nil)
			verifyCodeTestRequireRefusal(t, err, lockout, lockedText)
			if reply, want := replies[VerifyCodeTryLimit-1], authVerifyCodeSpentLockedMessage(lockout); reply != want {
				t.Fatalf("strike %d: the fifth wrong try was answered %q, want %q", strike, reply, want)
			}

			// still locked out a minute before the end, and not at the end
			clock.advance(lockout - time.Minute)
			_, err = verifyCodeTestTry(sources.session(nil), userAuth, code)
			verifyCodeTestRequireRefusal(t, err, time.Minute, lockedText)
			clock.advance(time.Minute)
		}
		if firstReply != AuthVerifyCodeSpentMessage {
			t.Fatalf("the first strike was answered %q, want %q", firstReply, AuthVerifyCodeSpentMessage)
		}

		code, err := verifyCodeTestSend(sources.session(nil), userAuth, VerifyCodeNumeric, nil)
		if err != nil {
			t.Fatalf("the code after the last lockout was not sent: %v", err)
		}
		if result := emailClaimVerify(t, sources.session(nil), userAuth, code); result.Network == nil {
			t.Fatalf("the right code was refused after the lockout ended: %+v", result.Error)
		}
	})
}

// A person who mistypes gets on: a resend replaces the code their wrong tries
// killed, and keeps the sign-up's password when it is asked for from the
// network the sign-up was made from, and the right code after four wrong tries
// completes the sign-up. Verifying clears the address's strike, so the next
// code killed is a first strike again and locks nothing out.
func TestVerifyCodeRightWithinTheLimitsStillWorks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sources := &verifyCodeTestSources{ctx: ctx}
		password := "own-password"
		userAuth := emailClaimTestAddress("within")
		own := sources.session(nil)
		_, userId := emailClaimSignUp(t, ctx, own, userAuth, password)

		code := emailClaimProvenCode(t, own, userAuth, &userId)
		killReplies := verifyCodeTestWrongTries(t, sources, "", userAuth, code, VerifyCodeTryLimit)

		// the verify screen's resend, from the sign-up's network
		code = emailClaimCode(t, ctx, own, userAuth)
		replies := verifyCodeTestWrongTries(t, sources, "", userAuth, code, VerifyCodeTryLimit-1)
		result := emailClaimVerify(t, own, userAuth, code)
		if result.Error != nil || result.Network == nil || !result.NewAccount {
			t.Fatalf("the right code after %d wrong tries did not complete the sign-up: %+v", VerifyCodeTryLimit-1, result)
		}
		for i, reply := range replies {
			if reply != "Invalid code." {
				t.Fatalf("wrong try %d of the resent code was answered %q", i+1, reply)
			}
		}
		if signIn := emailClaimPasswordSignIn(t, sources.session(nil), userAuth, password); !emailClaimSignedIn(signIn) {
			t.Fatalf("the sign-up's password stopped working: %+v", signIn)
		}

		// a code sign-in's code, killed by wrong tries
		code = emailClaimCode(t, ctx, sources.session(nil), userAuth)
		laterReplies := verifyCodeTestWrongTries(t, sources, "", userAuth, code, VerifyCodeTryLimit)

		if reply := killReplies[VerifyCodeTryLimit-1]; reply != AuthVerifyCodeSpentMessage {
			t.Fatalf("the first killed code was answered %q, want %q", reply, AuthVerifyCodeSpentMessage)
		}
		if reply := laterReplies[VerifyCodeTryLimit-1]; reply != AuthVerifyCodeSpentMessage {
			t.Fatalf("the code killed after the verification was answered %q, want %q (a first strike)", reply, AuthVerifyCodeSpentMessage)
		}
	})
}

// After the fifth wrong try kills a code, a resend issues a fresh code at once
// (a first strike locks nothing out). The fresh code verifies and the killed
// one does not; a resend from the sign-up's network keeps its password.
func TestVerifyCodeResendIssuesAFreshCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sources := &verifyCodeTestSources{ctx: ctx}
		password := "own-password"
		userAuth := emailClaimTestAddress("fresh")
		own := sources.session(nil)
		_, userId := emailClaimSignUp(t, ctx, own, userAuth, password)

		killed, err := verifyCodeTestSend(own, userAuth, VerifyCodeNumeric, &userId)
		if err != nil {
			t.Fatalf("the sign-up's code: %v", err)
		}
		replies := verifyCodeTestWrongTries(t, sources, "", userAuth, killed, VerifyCodeTryLimit)

		fresh, err := verifyCodeTestSend(own, userAuth, VerifyCodeNumeric, nil)
		if err != nil {
			t.Fatalf("the resend was refused: %v", err)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM user_auth_verify WHERE user_id = $1 AND NOT used`, userId); count != 1 {
			t.Fatalf("%d live codes after the resend, want the fresh one", count)
		}
		if result := emailClaimVerify(t, sources.session(nil), userAuth, killed); result.Network != nil {
			t.Fatalf("the killed code verified")
		}
		result := emailClaimVerify(t, own, userAuth, fresh)
		if result.Error != nil || result.Network == nil || !result.NewAccount {
			t.Fatalf("the fresh code did not complete the sign-up: %+v", result)
		}
		if signIn := emailClaimPasswordSignIn(t, sources.session(nil), userAuth, password); !emailClaimSignedIn(signIn) {
			t.Fatalf("the sign-up's password stopped working: %+v", signIn)
		}
		if reply := replies[VerifyCodeTryLimit-1]; reply != AuthVerifyCodeSpentMessage {
			t.Fatalf("the fifth wrong try was answered %q, want %q", reply, AuthVerifyCodeSpentMessage)
		}
	})
}

// One client network gets VerifyCodeSourceWrongTryLimit wrong tries in
// VerifyCodeSourceWrongTryWindow across every address. Then its tries are
// refused, the right code of an address included, while other client networks
// are not, until its oldest wrong try leaves the window.
func TestVerifyCodeWrongTriesOfOneSourceAreBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clock := newVerifyCodeTestClock()
		ctx := clock.context(context.Background())
		sources := &verifyCodeTestSources{ctx: ctx}
		userAuth := emailClaimTestAddress("source")
		own := sources.session(nil)
		_, userId := emailClaimSignUp(t, ctx, own, userAuth, "own-password")
		code := emailClaimProvenCode(t, own, userAuth, &userId)
		const refusedText = "from your network address"

		address := sources.address()
		at := func() *session.ClientSession {
			clientSession := session.Testing_CreateClientSession(ctx, nil)
			clientSession.ClientAddress = address
			return clientSession
		}
		for i := 0; i < VerifyCodeSourceWrongTryLimit; i++ {
			// an address of no account: one wrong try of each
			if result := emailClaimVerify(t, at(), emailClaimTestAddress("nobody"), "000000"); result.Network != nil {
				t.Fatalf("wrong try %d signed in", i+1)
			}
		}
		_, err := verifyCodeTestTry(at(), userAuth, code)
		verifyCodeTestRequireRefusal(t, err, VerifyCodeSourceWrongTryWindow, refusedText)

		if result := emailClaimVerify(t, own, userAuth, code); result.Network == nil {
			t.Fatalf("another client network was refused: %+v", result.Error)
		}
		clock.advance(VerifyCodeSourceWrongTryWindow)
		if result, err := verifyCodeTestTry(at(), emailClaimTestAddress("nobody"), "000000"); err != nil || result.Error == nil {
			t.Fatalf("the client network was refused after the window: %+v %v", result, err)
		}
	})
}

// A caller signed in to a network gets VerifyCodeNetworkWrongTryLimit wrong
// tries in VerifyCodeNetworkWrongTryWindow across every address and every
// client network it tries from. Then its tries are refused, with the network's
// token or a client token of the network alike; the same client network
// without the token is not refused.
func TestVerifyCodeWrongTriesOfOneNetworkAreBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		clock := newVerifyCodeTestClock()
		ctx := clock.context(context.Background())
		sources := &verifyCodeTestSources{ctx: ctx}
		_, _, signed, byJwt := emailClaimSeedphraseAccount(t, ctx)
		client, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "app", DeviceSpec: "app"}, emailClaimTestSession(ctx, 91, byJwt))
		if err != nil || client.Error != nil || client.ByClientJwt == nil {
			t.Fatalf("client: %+v %v", client, err)
		}
		const refusedText = "from this account"

		for i := 0; i < VerifyCodeNetworkWrongTryLimit; i++ {
			if result := emailClaimVerify(t, sources.signedIn(signed), emailClaimTestAddress("nobody"), "000000"); result.Network != nil {
				t.Fatalf("wrong try %d signed in", i+1)
			}
		}
		refused := sources.signedIn(signed)
		_, err = verifyCodeTestTry(refused, emailClaimTestAddress("nobody"), "000000")
		verifyCodeTestRequireRefusal(t, err, VerifyCodeNetworkWrongTryWindow, refusedText)
		_, err = verifyCodeTestTry(sources.signedIn(*client.ByClientJwt), emailClaimTestAddress("nobody"), "000000")
		verifyCodeTestRequireRefusal(t, err, VerifyCodeNetworkWrongTryWindow, refusedText)

		signedOut := session.Testing_CreateClientSession(ctx, nil)
		signedOut.ClientAddress = refused.ClientAddress
		if result, err := verifyCodeTestTry(signedOut, emailClaimTestAddress("nobody"), "000000"); err != nil || result.Error == nil {
			t.Fatalf("the client network without the token was refused: %+v %v", result, err)
		}
	})
}

// Tries made at once each take one of the code's tries before any compares
// its guess, so however many arrive together, the code is killed once, by its
// fifth try, and its address is struck once: the next code killed is the
// second strike, locked out for 15 minutes.
func TestVerifyCodeTriesMadeAtOnceKillTheCodeOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sources := &verifyCodeTestSources{ctx: ctx}
		userAuth := emailClaimTestAddress("at-once")
		own := sources.session(nil)
		_, userId := emailClaimSignUp(t, ctx, own, userAuth, "own-password")
		code := emailClaimProvenCode(t, own, userAuth, &userId)

		const tryCount = 3 * VerifyCodeTryLimit
		sessions := []*session.ClientSession{}
		for i := 0; i < tryCount; i++ {
			sessions = append(sessions, sources.session(nil))
		}
		results := make([]*AuthVerifyResult, tryCount)
		errs := make([]error, tryCount)
		var wait sync.WaitGroup
		for i := 0; i < tryCount; i++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				results[i], errs[i] = verifyCodeTestTry(sessions[i], userAuth, verifyCodeTestWrongGuess(code, i))
			}()
		}
		wait.Wait()
		for i := 0; i < tryCount; i++ {
			if errs[i] != nil || results[i] == nil || results[i].Network != nil {
				t.Fatalf("try %d made at once: %+v %v", i+1, results[i], errs[i])
			}
		}

		if result := emailClaimVerify(t, own, userAuth, code); result.Network != nil {
			t.Fatalf("the right code verified after %d wrong tries made at once", tryCount)
		}
		next, err := verifyCodeTestSend(sources.session(nil), userAuth, VerifyCodeNumeric, nil)
		if err != nil {
			t.Fatalf("the next code was not sent: %v", err)
		}
		replies := verifyCodeTestWrongTries(t, sources, "", userAuth, next, VerifyCodeTryLimit)
		if reply, want := replies[VerifyCodeTryLimit-1], authVerifyCodeSpentLockedMessage(15*time.Minute); reply != want {
			t.Fatalf("the next killed code was answered %q, want %q (the second strike)", reply, want)
		}
	})
}
