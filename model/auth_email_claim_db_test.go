package model

// An email or phone that nobody verified is a claim anyone can make with
// someone else's address; a proof of the address (Google or Apple, or a code
// sent to it) outranks it (owner decision 2026-11-20). Each test drives the
// real model paths against the database, and uses only what the code before
// the fix has, so each runs there too. The root-cause tests fail on it:
//   - Google or Apple sign-in joined the account that held an unverified row
//     with its email (a sign-up, or an email added to an account with none);
//   - a verification code signed in to that account and made the password its
//     claimant chose work, and left the claimant's other credentials;
//   - a code sent to one address of an account verified another;
//   - an unverified sign-up kept its address's owner out for good.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// How old a claim is aged to, past the 72 hour timeout.
const emailClaimTestExpiredAge = 73 * time.Hour

// A request from its own client network (a documentation /56, network 0 to
// 255), so the per-address attempt limits of one test step never refuse
// another's.
func emailClaimTestSession(ctx context.Context, network int, byJwt *session.ByJwt) *session.ClientSession {
	clientSession := session.Testing_CreateClientSession(ctx, byJwt)
	clientSession.ClientAddress = fmt.Sprintf("[2001:db8:e1:%02x00::1]:4000", network)
	return clientSession
}

// 16 random hex digits: the tail of a new id (its head is its time)
func emailClaimTestId() string {
	id := strings.ReplaceAll(server.NewId().String(), "-", "")
	return id[len(id)-16:]
}

func emailClaimTestName(prefix string) string {
	return prefix + emailClaimTestId()
}

func emailClaimTestAddress(prefix string) string {
	return fmt.Sprintf("%s-%s@example.com", prefix, emailClaimTestId())
}

// An email sign-up through NetworkCreate: the account waits on verification.
func emailClaimSignUp(t testing.TB, ctx context.Context, clientSession *session.ClientSession, userAuth string, password string) (networkId server.Id, userId server.Id) {
	t.Helper()
	result, err := NetworkCreate(
		NetworkCreateArgs{
			UserName:    "claim",
			UserAuth:    &userAuth,
			Password:    &password,
			NetworkName: emailClaimTestName("claim"),
			Terms:       true,
		},
		clientSession,
	)
	if err != nil || result == nil || result.Error != nil || result.VerificationRequired == nil || result.Network == nil {
		t.Fatalf("sign-up: result=%+v err=%v", result, err)
	}
	networkId = result.Network.NetworkId
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT admin_user_id FROM network WHERE network_id = $1`, networkId).Scan(&userId))
	})
	return
}

// A seed phrase account, as POST /auth/network-create with no sign-in makes
// one, and a signed network token of it.
func emailClaimSeedphraseAccount(t testing.TB, ctx context.Context) (networkId server.Id, userId server.Id, signed string, byJwt *session.ByJwt) {
	t.Helper()
	name := emailClaimTestName("seed")
	created := networkCreateSeedphrase(ctx, &NetworkCreateArgs{Terms: true}, name)
	if !created.Created {
		t.Fatalf("seed phrase account not created")
	}
	byJwt = session.NewByJwt(created.NetworkId, created.UserId, name, false, false)
	signed, err := session.MintNetworkSession(ctx, byJwt, "seedphrase")
	if err != nil {
		t.Fatalf("seed phrase sign-in: %v", err)
	}
	return created.NetworkId, created.UserId, signed, byJwt
}

// POST /auth/add-auth of an email or phone with a password.
func emailClaimAddAuth(t testing.TB, ctx context.Context, byJwt *session.ByJwt, userAuth string, password string) {
	t.Helper()
	result, err := AddAuth(AddAuthMethod{UserAuth: &userAuth, Password: &password}, emailClaimTestSession(ctx, 90, byJwt))
	if err != nil || result == nil || result.Error != nil {
		t.Fatalf("add %s: result=%+v err=%v", userAuth, result, err)
	}
}

// A code as /auth/verify-send sends one: no password proved.
func emailClaimCode(t testing.TB, ctx context.Context, clientSession *session.ClientSession, userAuth string) string {
	t.Helper()
	result, err := AuthVerifyCreateCode(AuthVerifyCreateCodeArgs{UserAuth: userAuth}, clientSession)
	if err != nil || result == nil || result.VerifyCode == nil {
		t.Fatalf("code for %s: result=%+v err=%v", userAuth, result, err)
	}
	return *result.VerifyCode
}

func emailClaimVerify(t testing.TB, clientSession *session.ClientSession, userAuth string, code string) *AuthVerifyResult {
	t.Helper()
	result, err := AuthVerify(AuthVerifyArgs{UserAuth: userAuth, VerifyCode: code}, clientSession)
	if err != nil || result == nil {
		t.Fatalf("verify %s: result=%+v err=%v", userAuth, result, err)
	}
	return result
}

func emailClaimPasswordSignIn(t testing.TB, clientSession *session.ClientSession, userAuth string, password string) *AuthLoginWithPasswordResult {
	t.Helper()
	result, err := AuthLoginWithPassword(AuthLoginWithPasswordArgs{UserAuth: userAuth, Password: password}, clientSession)
	if err != nil || result == nil {
		t.Fatalf("password sign-in %s: result=%+v err=%v", userAuth, result, err)
	}
	return result
}

// The password sign-in signed in: it returned a token. One that asks for the
// verification returns the network's name alone.
func emailClaimSignedIn(result *AuthLoginWithPasswordResult) bool {
	return result.Network != nil && result.Network.ByJwt != nil
}

// Sign-in with a verified Google or Apple token for the email.
func emailClaimSso(t testing.TB, ctx context.Context, authType AuthType, userAuth string) *AuthLoginResult {
	t.Helper()
	result, err := handleLoginParsedAuthJwt(
		&HandleLoginParsedAuthJwtArgs{
			AuthJwt:    AuthJwt{AuthType: authType, UserAuth: userAuth, UserName: "provider"},
			AuthJwtStr: "synthetic-id-token",
		},
		ctx,
	)
	if err != nil || result == nil {
		t.Fatalf("%s sign-in: result=%+v err=%v", authType, result, err)
	}
	return result
}

// Sign-up with a verified Google or Apple token for the email.
func emailClaimSsoSignUp(t testing.TB, ctx context.Context, authType AuthType, userAuth string) networkCreateResult {
	t.Helper()
	authJwt := "synthetic-id-token"
	authJwtType := string(authType)
	name := emailClaimTestName("sso")
	return networkCreateAuthJwt(
		ctx,
		&NetworkCreateArgs{
			UserName:    "provider",
			AuthJwt:     &authJwt,
			AuthJwtType: &authJwtType,
			NetworkName: name,
			Terms:       true,
		},
		false,
		AuthJwt{AuthType: authType, UserAuth: userAuth, UserName: "provider"},
		name,
		userAuth,
	)
}

func emailClaimSignedNetworkId(t testing.TB, ctx context.Context, signed string) server.Id {
	t.Helper()
	byJwt, err := session.ParseByJwt(ctx, signed)
	if err != nil {
		t.Fatalf("parse the signed token: %v", err)
	}
	return byJwt.NetworkId
}

func emailClaimAge(ctx context.Context, userAuth string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_user_auth_password SET create_time = $2 WHERE user_auth = $1`,
			userAuth,
			server.NowUtc().Add(-emailClaimTestExpiredAge),
		))
	})
}

// Finding 1: Google or Apple sign-in never joins an email sign-up nobody
// verified. The sign-in goes on as a new user, and its sign-up takes the
// address from the claim.
func TestSsoSignInDoesNotJoinAnUnverifiedSignUp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for i, authType := range []AuthType{AuthTypeGoogle, AuthTypeApple} {
			userAuth := emailClaimTestAddress("squat")
			_, claimantUserId := emailClaimSignUp(t, ctx, emailClaimTestSession(ctx, 10*i+1, nil), userAuth, "claimant-password")

			result := emailClaimSso(t, ctx, authType, userAuth)
			if result.Network != nil || result.UserName == nil {
				t.Fatalf("%s: the sign-in joined the unverified sign-up: %+v", authType, result)
			}
			if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_sso WHERE user_id = $1`, claimantUserId); count != 0 {
				t.Fatalf("%s: the provider was linked to the claimant's account", authType)
			}

			created := emailClaimSsoSignUp(t, ctx, authType, userAuth)
			if !created.Created {
				t.Fatalf("%s: the sign-up of the proven email was refused (status %d)", authType, created.refusalStatus)
			}
			if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_auth = $1`, userAuth); count != 0 {
				t.Fatalf("%s: the claim was not released", authType)
			}
			if count := countRows(ctx, `SELECT COUNT(*) FROM network_user WHERE user_id = $1 AND user_auth IS NOT NULL`, claimantUserId); count != 0 {
				t.Fatalf("%s: the claimant still holds the address", authType)
			}

			again := emailClaimSso(t, ctx, authType, userAuth)
			if again.Network == nil || emailClaimSignedNetworkId(t, ctx, again.Network.ByJwt) != created.NetworkId {
				t.Fatalf("%s: the next sign-in did not reach the new account: %+v", authType, again)
			}
		}
	})
}

// Finding 3: an email added to an account that has none (a seed phrase
// account) waits unverified. A Google sign-in with it used to land in that
// account and keep the account's seed phrase, sessions and API keys. It gets
// its own account, and the added email is released.
func TestSsoSignInDoesNotJoinAnEmailAddedToAnotherAccount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, claimantUserId, _, claimantByJwt := emailClaimSeedphraseAccount(t, ctx)
		userAuth := emailClaimTestAddress("added")
		emailClaimAddAuth(t, ctx, claimantByJwt, userAuth, "claimant-password")

		result := emailClaimSso(t, ctx, AuthTypeGoogle, userAuth)
		if result.Network != nil || result.UserName == nil {
			t.Fatalf("the sign-in joined the account the email was added to: %+v", result)
		}

		created := emailClaimSsoSignUp(t, ctx, AuthTypeGoogle, userAuth)
		if !created.Created {
			t.Fatalf("the sign-up of the proven email was refused (status %d)", created.refusalStatus)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_id = $1`, claimantUserId); count != 0 {
			t.Fatalf("the added email was not released")
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_seedphrase WHERE user_id = $1`, claimantUserId); count != 1 {
			t.Fatalf("the claimant's own seed phrase was touched")
		}
	})
}

// Finding 2: a code sent to an unverified sign-up's address, verified by
// someone who did not set its password on that network, gives the account to
// the verifier: the password set before stops working, and so does every
// earlier session, API key and other sign-in method.
func TestVerifyCodeHandsAnUnverifiedSignUpToItsVerifier(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userAuth := emailClaimTestAddress("handover")
		claimantPassword := "claimant-password"
		networkId, userId := emailClaimSignUp(t, ctx, emailClaimTestSession(ctx, 1, nil), userAuth, claimantPassword)

		// credentials made before the address is verified
		before := session.NewByJwt(networkId, userId, "handover", false, false)
		beforeSigned, err := session.MintNetworkSession(ctx, before, "password")
		server.Raise(err)
		apiKey, err := CreateApiKey(&CreateApiKeyArgs{Name: "before"}, emailClaimTestSession(ctx, 2, before))
		if err != nil || apiKey.Error != nil {
			t.Fatalf("api key: %+v %v", apiKey, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(CreateSeedphraseAuthInTx(tx, ctx, userId, "synthetic seed "+emailClaimTestId()))
		})

		verifier := emailClaimTestSession(ctx, 3, nil)
		code := emailClaimCode(t, ctx, verifier, userAuth)
		result := emailClaimVerify(t, verifier, userAuth, code)
		if result.Error != nil || result.Network == nil || !result.NewAccount {
			t.Fatalf("the verifier was not signed in: %+v", result)
		}
		if emailClaimSignedNetworkId(t, ctx, result.Network.ByJwt) != networkId {
			t.Fatalf("the verifier was signed in to another network")
		}

		if signIn := emailClaimPasswordSignIn(t, emailClaimTestSession(ctx, 4, nil), userAuth, claimantPassword); emailClaimSignedIn(signIn) || signIn.VerificationRequired != nil {
			t.Fatalf("the password set before the verification still signs in: %+v", signIn)
		}
		parsedBefore, err := session.ParseByJwt(ctx, beforeSigned)
		server.Raise(err)
		if err := session.ValidateByJwtState(ctx, parsedBefore, false); err == nil {
			t.Fatalf("a session from before the verification still works")
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM account_api_key WHERE network_id = $1`, networkId); count != 0 {
			t.Fatalf("%d API keys from before the verification remain", count)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_seedphrase WHERE user_id = $1`, userId); count != 0 {
			t.Fatalf("a seed phrase from before the verification remains")
		}
		after, err := session.ParseByJwt(ctx, result.Network.ByJwt)
		server.Raise(err)
		if err := session.ValidateByJwtState(ctx, after, false); err != nil {
			t.Fatalf("the verifier's own session does not work: %v", err)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_id = $1 AND user_auth = $2 AND verified`, userId, userAuth); count != 1 {
			t.Fatalf("the address was not verified")
		}
	})
}

// Finding 3: a code sent to an email added to another account never joins
// that account. The added email is released, and its owner can use it.
func TestVerifyCodeNeverJoinsAnEmailAddedToAnotherAccount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, claimantUserId, _, claimantByJwt := emailClaimSeedphraseAccount(t, ctx)
		userAuth := emailClaimTestAddress("joined")
		emailClaimAddAuth(t, ctx, claimantByJwt, userAuth, "claimant-password")

		owner := emailClaimTestSession(ctx, 1, nil)
		code := emailClaimCode(t, ctx, owner, userAuth)
		result := emailClaimVerify(t, owner, userAuth, code)
		if result.Network != nil || result.Error == nil {
			t.Fatalf("the code joined the account the email was added to: %+v", result)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_id = $1`, claimantUserId); count != 0 {
			t.Fatalf("the added email was not released")
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_seedphrase WHERE user_id = $1`, claimantUserId); count != 1 {
			t.Fatalf("the claimant's own seed phrase was touched")
		}
		// the owner signs up with it at once
		emailClaimSignUp(t, ctx, emailClaimTestSession(ctx, 2, nil), userAuth, "owner-password")
	})
}

// A code is proof of the address it was sent to only. An account could verify
// any address it added with a code sent to another of its addresses.
func TestVerifyCodeSentToAnotherAddressDoesNotVerify(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, userId, signed, byJwt := emailClaimSeedphraseAccount(t, ctx)
		phone := "+1 609-737-0041"
		normalPhone, _ := NormalUserAuth(phone)
		userAuth := emailClaimTestAddress("other-address")
		emailClaimAddAuth(t, ctx, byJwt, phone, "claimant-password")
		emailClaimAddAuth(t, ctx, byJwt, userAuth, "claimant-password")

		signedIn := emailClaimTestSession(ctx, 1, nil)
		phoneCode := emailClaimCode(t, ctx, signedIn, normalPhone)
		signedIn.Header["Authorization"] = []string{"Bearer " + signed}
		result := emailClaimVerify(t, signedIn, userAuth, phoneCode)
		if result.Network != nil || result.Error == nil {
			t.Fatalf("a code sent to the phone verified the email: %+v", result)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_id = $1 AND verified`, userId); count != 0 {
			t.Fatalf("an address was verified by a code sent to another")
		}
	})
}

// Finding 4: an unverified claim keeps another sign-up of its address, an add
// of it, and a password reset out for 72 hours only. After that its address's
// owner signs up or resets.
func TestUnverifiedClaimExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a sign-up held by an unverified sign-up
		userAuth := emailClaimTestAddress("expires")
		_, claimantUserId := emailClaimSignUp(t, ctx, emailClaimTestSession(ctx, 1, nil), userAuth, "claimant-password")
		password := "owner-password"
		ownerSignUp := func() *NetworkCreateResult {
			result, err := NetworkCreate(
				NetworkCreateArgs{UserName: "owner", UserAuth: &userAuth, Password: &password, NetworkName: emailClaimTestName("owner"), Terms: true},
				emailClaimTestSession(ctx, 2, nil),
			)
			if err != nil {
				t.Fatalf("owner sign-up: %v", err)
			}
			return result
		}
		if result := ownerSignUp(); result.Error == nil {
			t.Fatalf("a fresh claim did not hold its address: %+v", result)
		}
		if reset, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, emailClaimTestSession(ctx, 3, nil)); err == nil && reset != nil && reset.ResetCode != nil {
			t.Fatalf("a fresh claim's address was sent a reset code")
		}
		emailClaimAge(ctx, userAuth)
		reset, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, emailClaimTestSession(ctx, 4, nil))
		if err != nil || reset == nil || reset.ResetCode == nil {
			t.Fatalf("an expired claim's address got no reset code: %+v %v", reset, err)
		}
		if result := ownerSignUp(); result.Error != nil || result.VerificationRequired == nil {
			t.Fatalf("the owner's sign-up after the claim expired was refused: %+v", result)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_id = $1`, claimantUserId); count != 0 {
			t.Fatalf("the expired claim was not released")
		}

		// an address added to another account and never verified
		_, _, _, claimantByJwt := emailClaimSeedphraseAccount(t, ctx)
		_, _, _, ownerByJwt := emailClaimSeedphraseAccount(t, ctx)
		added := emailClaimTestAddress("added-expires")
		emailClaimAddAuth(t, ctx, claimantByJwt, added, "claimant-password")
		if result, err := AddAuth(AddAuthMethod{UserAuth: &added, Password: &password}, emailClaimTestSession(ctx, 5, ownerByJwt)); err != nil || result.Error == nil {
			t.Fatalf("a fresh added claim did not hold its address: %+v %v", result, err)
		}
		emailClaimAge(ctx, added)
		emailClaimAddAuth(t, ctx, ownerByJwt, added, password)
	})
}

// A seed phrase user adds their own email and verifies it signed in, with the
// sign-in's network token (the site, current apps) or a client token (apps
// before 2026-11-18): the email is verified, and the account keeps its seed
// phrase, its session and the password the user set.
func TestSeedphraseUserAddsAndVerifiesTheirOwnEmail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for i, clientToken := range []bool{false, true} {
			networkId, userId, signed, byJwt := emailClaimSeedphraseAccount(t, ctx)
			userAuth := emailClaimTestAddress("own-added")
			password := "own-added-password"
			emailClaimAddAuth(t, ctx, byJwt, userAuth, password)

			bearer := signed
			if clientToken {
				client, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "app", DeviceSpec: "app"}, emailClaimTestSession(ctx, 10*i+1, byJwt))
				if err != nil || client.Error != nil || client.ByClientJwt == nil {
					t.Fatalf("client: %+v %v", client, err)
				}
				bearer = *client.ByClientJwt
			}

			signedIn := emailClaimTestSession(ctx, 10*i+2, nil)
			code := emailClaimCode(t, ctx, signedIn, userAuth)
			signedIn.Header["Authorization"] = []string{"Bearer " + bearer}
			result := emailClaimVerify(t, signedIn, userAuth, code)
			if result.Error != nil || result.Network == nil || emailClaimSignedNetworkId(t, ctx, result.Network.ByJwt) != networkId {
				t.Fatalf("client token %t: the owner's verification failed: %+v", clientToken, result)
			}
			if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_id = $1 AND verified`, userId); count != 1 {
				t.Fatalf("client token %t: the email was not verified", clientToken)
			}
			if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_seedphrase WHERE user_id = $1`, userId); count != 1 {
				t.Fatalf("client token %t: the seed phrase was removed", clientToken)
			}
			parsed, err := session.ParseByJwt(ctx, signed)
			server.Raise(err)
			if err := session.ValidateByJwtState(ctx, parsed, false); err != nil {
				t.Fatalf("client token %t: the owner's session ended: %v", clientToken, err)
			}
			if signIn := emailClaimPasswordSignIn(t, emailClaimTestSession(ctx, 10*i+3, nil), userAuth, password); !emailClaimSignedIn(signIn) {
				t.Fatalf("client token %t: the password the owner set does not sign in: %+v", clientToken, signIn)
			}
		}
	})
}
