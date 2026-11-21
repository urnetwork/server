package model

// The legitimate flows around unverified email and phone claims, against the
// database (auth_email_claim_db_test.go has the root-cause tests): a sign-up,
// or a sign-in with the password of an added address, sends its code bound to
// the client network that proved the password (AuthVerifyCreateCodeArgs
// .PasswordUserId), and verifying there keeps that password.

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A code as the sign-up or a password sign-in sends one: after the password
// was proved for passwordUserId on the client's network.
func emailClaimProvenCode(t testing.TB, clientSession *session.ClientSession, userAuth string, passwordUserId *server.Id) string {
	t.Helper()
	result, err := AuthVerifyCreateCode(
		AuthVerifyCreateCodeArgs{UserAuth: userAuth, PasswordUserId: passwordUserId},
		clientSession,
	)
	if err != nil || result == nil || result.VerifyCode == nil {
		t.Fatalf("code for %s: result=%+v err=%v", userAuth, result, err)
	}
	return *result.VerifyCode
}

// The person who signs up verifies on the network they signed up from and
// keeps their password, also after asking for the code again.
func TestSignUpVerifiedOnItsOwnNetworkKeepsItsPassword(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		password := "own-password"
		for i, resend := range []bool{false, true} {
			userAuth := emailClaimTestAddress("own")
			own := emailClaimTestSession(ctx, 10*i+1, nil)
			_, userId := emailClaimSignUp(t, ctx, own, userAuth, password)
			code := emailClaimProvenCode(t, own, userAuth, &userId)
			if resend {
				// the verify screen's resend: /auth/verify-send, no password
				code = emailClaimCode(t, ctx, own, userAuth)
			}
			result := emailClaimVerify(t, own, userAuth, code)
			if result.Error != nil || result.Network == nil || !result.NewAccount {
				t.Fatalf("resend %t: the sign-up was not completed: %+v", resend, result)
			}
			if signIn := emailClaimPasswordSignIn(t, emailClaimTestSession(ctx, 10*i+2, nil), userAuth, password); !emailClaimSignedIn(signIn) {
				t.Fatalf("resend %t: the sign-up's own password stopped working: %+v", resend, signIn)
			}
		}
	})
}

// A code that followed the claimant's own sign-up, verified from another
// network, never keeps the claimant's password.
func TestVerifyCodeFromAnotherNetworkRetiresTheSignUpPassword(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userAuth := emailClaimTestAddress("elsewhere")
		password := "claimant-password"
		claimant := emailClaimTestSession(ctx, 1, nil)
		_, userId := emailClaimSignUp(t, ctx, claimant, userAuth, password)

		code := emailClaimProvenCode(t, claimant, userAuth, &userId)
		result := emailClaimVerify(t, emailClaimTestSession(ctx, 2, nil), userAuth, code)
		if result.Error != nil || result.Network == nil {
			t.Fatalf("the verifier was not signed in: %+v", result)
		}
		if signIn := emailClaimPasswordSignIn(t, claimant, userAuth, password); emailClaimSignedIn(signIn) {
			t.Fatalf("the claimant's password works after another network verified the address")
		}
	})
}

// An added email whose owner signs in with its password on a new device is
// verified by the code that sign-in sends, and keeps the password. The same
// code from another network neither verifies nor releases it.
func TestAddedEmailVerifiedAfterItsPasswordSignIn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, userId, _, byJwt := emailClaimSeedphraseAccount(t, ctx)
		userAuth := emailClaimTestAddress("added-password")
		password := "added-password"
		emailClaimAddAuth(t, ctx, byJwt, userAuth, password)

		device := emailClaimTestSession(ctx, 1, nil)
		signIn := emailClaimPasswordSignIn(t, device, userAuth, password)
		if emailClaimSignedIn(signIn) || signIn.VerificationRequired == nil || signIn.VerificationRequired.PasswordUserId == nil {
			t.Fatalf("the pending email signed in: %+v", signIn)
		}
		code := emailClaimProvenCode(t, device, userAuth, signIn.VerificationRequired.PasswordUserId)

		elsewhere := emailClaimVerify(t, emailClaimTestSession(ctx, 2, nil), userAuth, code)
		if elsewhere.Network != nil || elsewhere.Error == nil || elsewhere.Error.Message != AuthVerifySignInAgainMessage {
			t.Fatalf("the code from another network: %+v", elsewhere)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_id = $1 AND NOT verified`, userId); count != 1 {
			t.Fatalf("the added email was not kept pending")
		}

		result := emailClaimVerify(t, device, userAuth, code)
		if result.Error != nil || result.Network == nil || emailClaimSignedNetworkId(t, ctx, result.Network.ByJwt) != networkId {
			t.Fatalf("the owner's verification failed: %+v", result)
		}
		if again := emailClaimPasswordSignIn(t, emailClaimTestSession(ctx, 3, nil), userAuth, password); !emailClaimSignedIn(again) {
			t.Fatalf("the password does not sign in after the verification: %+v", again)
		}
	})
}

// A code sent to an added address, verified without the account by its owner,
// is refused with the release message.
func TestVerifyCodeReleaseMessage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, _, _, byJwt := emailClaimSeedphraseAccount(t, ctx)
		userAuth := emailClaimTestAddress("release-message")
		emailClaimAddAuth(t, ctx, byJwt, userAuth, "claimant-password")
		owner := emailClaimTestSession(ctx, 1, nil)
		result := emailClaimVerify(t, owner, userAuth, emailClaimCode(t, ctx, owner, userAuth))
		if result.Error == nil || result.Error.Message != AuthVerifyReleasedMessage {
			t.Fatalf("result = %+v, want the release message", result)
		}
	})
}

// An expired sign-up's reset: the password whoever signed up chose is replaced
// by the one the address's owner sets, and signing in with it verifies the
// address.
func TestUnverifiedSignUpResetThenVerify(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userAuth := emailClaimTestAddress("reset")
		claimantPassword := "claimant-password"
		networkId, _ := emailClaimSignUp(t, ctx, emailClaimTestSession(ctx, 1, nil), userAuth, claimantPassword)
		emailClaimAge(ctx, userAuth)

		owner := emailClaimTestSession(ctx, 2, nil)
		reset, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, owner)
		if err != nil || reset == nil || reset.ResetCode == nil {
			t.Fatalf("no reset code: %+v %v", reset, err)
		}
		ownerPassword := "owner-password"
		set, err := AuthPasswordSet(AuthPasswordSetArgs{ResetCode: *reset.ResetCode, Password: ownerPassword}, owner)
		if err != nil || set == nil || set.Error != nil || set.NetworkId != networkId {
			t.Fatalf("the reset failed: %+v %v", set, err)
		}
		if signIn := emailClaimPasswordSignIn(t, emailClaimTestSession(ctx, 3, nil), userAuth, claimantPassword); emailClaimSignedIn(signIn) || signIn.VerificationRequired != nil {
			t.Fatalf("the claimant's password still works: %+v", signIn)
		}
		signIn := emailClaimPasswordSignIn(t, owner, userAuth, ownerPassword)
		if signIn.VerificationRequired == nil {
			t.Fatalf("the owner's password did not ask for the verification: %+v", signIn)
		}
		code := emailClaimProvenCode(t, owner, userAuth, signIn.VerificationRequired.PasswordUserId)
		result := emailClaimVerify(t, owner, userAuth, code)
		if result.Error != nil || result.Network == nil {
			t.Fatalf("the owner's verification failed: %+v", result)
		}
		if again := emailClaimPasswordSignIn(t, emailClaimTestSession(ctx, 4, nil), userAuth, ownerPassword); !emailClaimSignedIn(again) {
			t.Fatalf("the owner's password does not sign in: %+v", again)
		}
	})
}

// A password user who verified their email and later signs in with Google
// with the same email keeps one account, now with Google linked.
func TestVerifiedPasswordUserSignsInWithGoogle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userAuth := emailClaimTestAddress("verified")
		own := emailClaimTestSession(ctx, 1, nil)
		networkId, userId := emailClaimSignUp(t, ctx, own, userAuth, "own-password")
		code := emailClaimProvenCode(t, own, userAuth, &userId)
		if result := emailClaimVerify(t, own, userAuth, code); result.Error != nil || result.Network == nil {
			t.Fatalf("verification: %+v", result)
		}

		result := emailClaimSso(t, ctx, AuthTypeGoogle, userAuth)
		if result.Network == nil || emailClaimSignedNetworkId(t, ctx, result.Network.ByJwt) != networkId {
			t.Fatalf("google did not sign in to the verified account: %+v", result)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_sso WHERE user_id = $1 AND auth_type = 'google'`, userId); count != 1 {
			t.Fatalf("google was not linked to the account")
		}
		if again := emailClaimPasswordSignIn(t, emailClaimTestSession(ctx, 2, nil), userAuth, "own-password"); !emailClaimSignedIn(again) {
			t.Fatalf("the password stopped working: %+v", again)
		}
	})
}
