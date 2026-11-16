package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Codes an unauthenticated request can have mailed or texted to an email or
// phone, through the real code creation and a fake message sender.
//
// Root cause (inbox 835): anyone can sign up with someone else's email. The
// account stays unverified, and every verification or password reset request
// for the address mailed it another code: a reset code for such a sign-up was
// created again on 2026-10-25. The only limit was per client address (five in
// five minutes), which a stranger asking from many addresses never meets. A
// reset code now goes only to a verified address, and each address gets a few
// verification and reset codes a day, however many client addresses ask.

// An email sign-up that was never verified: what a sign-up with someone else's
// address leaves.
func newUnverifiedEmailSignup(ctx context.Context) (userId server.Id, userAuth string) {
	networkId := server.NewId()
	userId = server.NewId()
	userAuth = model.Testing_CreateNetwork(ctx, networkId, "test", userId)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_user_auth_password SET verified = false WHERE user_id = $1`,
			userId,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_user SET verified = false WHERE user_id = $1`,
			userId,
		))
	})
	return
}

// A request from its own client network, a documentation /56 (the bucket of
// the per-client-address attempt limit), so that limit never refuses first: a
// stranger who asks from many networks meets only the budget of the address
// the codes are mailed to.
func newCodeSendSession(ctx context.Context, i int) *session.ClientSession {
	clientSession := session.Testing_CreateClientSession(ctx, nil)
	clientSession.ClientAddress = fmt.Sprintf("[2001:db8:0:%x00::1]:4000", 1+i)
	return clientSession
}

// A message sender that keeps the last code it was handed.
type codeCapturingMessageSender struct {
	sendCount      int
	lastVerifyCode string
	lastResetCode  string
}

func (self *codeCapturingMessageSender) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {
	self.sendCount += 1
	switch v := template.(type) {
	case *AuthVerifyTemplate:
		self.lastVerifyCode = v.VerifyCode
	case *AuthPasswordResetTemplate:
		self.lastResetCode = v.ResetCode
	}
	return nil
}

// verifies userAuth with a fresh code, as the sign-up's verify screen does
func verifyUserAuth(t testing.TB, ctx context.Context, userAuth string, clientSession *session.ClientSession) {
	createResult, err := model.AuthVerifyCreateCode(
		model.AuthVerifyCreateCodeArgs{UserAuth: userAuth},
		clientSession,
	)
	if err != nil || createResult.VerifyCode == nil {
		t.Fatalf("no verification code: result = %+v, err = %v", createResult, err)
	}
	verifyResult, err := model.AuthVerify(
		model.AuthVerifyArgs{UserAuth: userAuth, VerifyCode: *createResult.VerifyCode},
		clientSession,
	)
	if err != nil || verifyResult.Error != nil || verifyResult.Network == nil {
		t.Fatalf("the verification failed: result = %+v, err = %v", verifyResult, err)
	}
}

// No password reset code is mailed to an address that was never verified. The
// same address gets its reset code once it is verified.
func TestAuthPasswordResetSendsNothingToAnUnverifiedAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, userAuth := newUnverifiedEmailSignup(ctx)

		sender := &fakeVerifySendMessageSender{}
		result, err := authPasswordReset(
			AuthPasswordResetArgs{UserAuth: userAuth, ResultErrors: true},
			newCodeSendSession(ctx, 0),
			model.AuthPasswordResetCreateCode,
			sender,
		)
		if sender.sendCount != 0 {
			t.Fatalf("a password reset code was mailed to an address that was never verified")
		}
		if err == nil && (result == nil || result.Error == nil) {
			t.Fatalf("the reset was reported as sent: %+v", result)
		}

		verifyUserAuth(t, ctx, userAuth, newCodeSendSession(ctx, 1))

		sender = &fakeVerifySendMessageSender{}
		result, err = authPasswordReset(
			AuthPasswordResetArgs{UserAuth: userAuth, ResultErrors: true},
			newCodeSendSession(ctx, 2),
			model.AuthPasswordResetCreateCode,
			sender,
		)
		if err != nil || result.Error != nil || sender.sendCount != 1 {
			t.Fatalf("the verified address got no reset code: result = %+v, err = %v, send count = %d", result, err, sender.sendCount)
		}
	})
}

// A sign-up still gets its verification code, and one address gets only a few
// a day however many client addresses ask. The refusal is a rate limit with
// the time it lifts, and it leaves the code already sent usable.
func TestAuthVerifySendIsLimitedPerAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, userAuth := newUnverifiedEmailSignup(ctx)

		sender := &codeCapturingMessageSender{}
		var refusal *AuthVerifySendError
		for i := range 20 {
			result, err := authVerifySend(
				AuthVerifySendArgs{UserAuth: userAuth, ResultErrors: true},
				newCodeSendSession(ctx, i),
				model.AuthVerifyCreateCode,
				sender,
			)
			if err != nil {
				t.Fatalf("send %d: %v", i+1, err)
			}
			if result.Error != nil {
				refusal = result.Error
				break
			}
		}
		if refusal == nil {
			t.Fatalf("%d verification codes were mailed to one address without a refusal", sender.sendCount)
		}
		if sender.sendCount < 1 {
			t.Fatalf("the sign-up got no verification code")
		}
		if refusal.Code != model.AuthVerifySendErrorCodeRateLimited || refusal.RetryAfterSeconds <= 0 {
			t.Fatalf("the refusal is not a rate limit with a retry time: %+v", refusal)
		}

		// the refused request did not invalidate the code already sent
		verifyResult, err := model.AuthVerify(
			model.AuthVerifyArgs{UserAuth: userAuth, VerifyCode: sender.lastVerifyCode},
			newCodeSendSession(ctx, 100),
		)
		if err != nil || verifyResult.Error != nil || verifyResult.Network == nil {
			t.Fatalf("the code sent before the refusal no longer verifies: result = %+v, err = %v", verifyResult, err)
		}
	})
}

// One address gets only a few password reset codes a day however many client
// addresses ask, and a refused request leaves the code already sent usable.
func TestAuthPasswordResetIsLimitedPerAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, "test", userId)

		sender := &codeCapturingMessageSender{}
		var refusal *AuthVerifySendError
		for i := range 20 {
			result, err := authPasswordReset(
				AuthPasswordResetArgs{UserAuth: userAuth, ResultErrors: true},
				newCodeSendSession(ctx, i),
				model.AuthPasswordResetCreateCode,
				sender,
			)
			if err != nil {
				t.Fatalf("send %d: %v", i+1, err)
			}
			if result.Error != nil {
				refusal = result.Error
				break
			}
		}
		if refusal == nil {
			t.Fatalf("%d password reset codes were mailed to one address without a refusal", sender.sendCount)
		}
		if sender.sendCount < 1 {
			t.Fatalf("the verified address got no reset code")
		}
		if refusal.Code != model.AuthVerifySendErrorCodeRateLimited || refusal.RetryAfterSeconds <= 0 {
			t.Fatalf("the refusal is not a rate limit with a retry time: %+v", refusal)
		}

		// the refused request did not invalidate the code already sent
		setResult, err := model.AuthPasswordSet(
			model.AuthPasswordSetArgs{ResetCode: sender.lastResetCode, Password: "a-new-password-1234"},
			newCodeSendSession(ctx, 100),
		)
		if err != nil || setResult.Error != nil {
			t.Fatalf("the code sent before the refusal no longer resets: result = %+v, err = %v", setResult, err)
		}
	})
}
