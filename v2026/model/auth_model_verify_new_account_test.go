package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// fakeAuthVerifyAccountLookup stands in for network_user.auth_type and the
// verified column of network_user_auth_password, read before the verification
// marks its own sign-in verified.
type fakeAuthVerifyAccountLookup struct {
	createAuthTypes  map[server.Id]AuthType
	verifiedPassword map[server.Id]bool
}

func (self *fakeAuthVerifyAccountLookup) createAuthType(userId server.Id) AuthType {
	return self.createAuthTypes[userId]
}

func (self *fakeAuthVerifyAccountLookup) hasVerifiedPasswordAuth(userId server.Id) bool {
	return self.verifiedPassword[userId]
}

// Only the verification that completes an email or phone sign-up is a new
// account. An email or phone added to an Apple, Google, wallet, seed phrase or
// guest network, a second email or phone, and a re-verification are not, and
// must not get the welcome email or onboarding enrollment again.
func TestIsNewAccountVerification(t *testing.T) {
	emailSignUp := server.NewId()
	phoneSignUp := server.NewId()
	appleNetwork := server.NewId()
	googleNetwork := server.NewId()
	solanaNetwork := server.NewId()
	bittensorNetwork := server.NewId()
	seedphraseNetwork := server.NewId()
	guestNetwork := server.NewId()
	verifiedEmailAddsPhone := server.NewId()
	unknownUser := server.NewId()

	lookup := &fakeAuthVerifyAccountLookup{
		createAuthTypes: map[server.Id]AuthType{
			emailSignUp:            AuthTypePassword,
			phoneSignUp:            AuthTypePassword,
			appleNetwork:           AuthTypeApple,
			googleNetwork:          AuthTypeGoogle,
			solanaNetwork:          AuthTypeSolana,
			bittensorNetwork:       AuthTypeBittensor,
			seedphraseNetwork:      AuthTypeSeedphrase,
			guestNetwork:           AuthTypeGuest,
			verifiedEmailAddsPhone: AuthTypePassword,
		},
		verifiedPassword: map[server.Id]bool{
			verifiedEmailAddsPhone: true,
		},
	}

	for _, c := range []struct {
		name   string
		userId server.Id
		want   bool
	}{
		{"email sign-up", emailSignUp, true},
		{"phone sign-up", phoneSignUp, true},
		{"apple network adds email", appleNetwork, false},
		{"google network adds email", googleNetwork, false},
		{"solana wallet network adds email", solanaNetwork, false},
		{"bittensor wallet network adds email", bittensorNetwork, false},
		{"seed phrase network adds email", seedphraseNetwork, false},
		{"guest network adds email", guestNetwork, false},
		{"verified email account adds phone", verifiedEmailAddsPhone, false},
		{"no network_user row", unknownUser, false},
	} {
		if got := isNewAccountVerification(lookup, c.userId); got != c.want {
			t.Errorf("%s: isNewAccountVerification = %t, want %t", c.name, got, c.want)
		}
	}
}

// verifyWithNewCode creates a code for userAuth and verifies it.
func verifyWithNewCode(t testing.TB, ctx context.Context, userAuth string) *AuthVerifyResult {
	clientSession := session.Testing_CreateClientSession(ctx, nil)
	createResult, err := AuthVerifyCreateCode(
		AuthVerifyCreateCodeArgs{UserAuth: userAuth},
		clientSession,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, createResult.VerifyCode, nil)
	verifyResult, err := AuthVerify(
		AuthVerifyArgs{
			UserAuth:   userAuth,
			VerifyCode: *createResult.VerifyCode,
		},
		clientSession,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, verifyResult.Error, nil)
	connect.AssertNotEqual(t, verifyResult.Network, nil)
	return verifyResult
}

func addUnverifiedUserAuth(t testing.TB, ctx context.Context, userId server.Id, userAuth string) {
	passwordSalt := createPasswordSalt()
	err := addUserAuth(
		&AddUserAuthArgs{
			UserId:       userId,
			UserAuth:     &userAuth,
			PasswordHash: computePasswordHashV1([]byte("password123"), passwordSalt),
			PasswordSalt: passwordSalt,
		},
		ctx,
	)
	connect.AssertEqual(t, err, nil)
}

// AuthVerify against the database: the first verification of an email
// sign-up is a new account; a re-verification, a phone added to it, and an
// email added to a Google network are not.
func TestAuthVerifyNewAccount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// an email sign-up that has not verified yet
		networkId := server.NewId()
		userId := server.NewId()
		userAuth := Testing_CreateNetwork(ctx, networkId, "test", userId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_user_auth_password SET verified = false WHERE user_id = $1`,
				userId,
			))
		})
		connect.AssertEqual(t, verifyWithNewCode(t, ctx, userAuth).NewAccount, true)
		// verifying it again is not a sign-up
		connect.AssertEqual(t, verifyWithNewCode(t, ctx, userAuth).NewAccount, false)
		// nor is a phone added to the verified account
		phone := "+1 609-737-0012"
		addUnverifiedUserAuth(t, ctx, userId, phone)
		connect.AssertEqual(t, verifyWithNewCode(t, ctx, phone).NewAccount, false)

		// a Google network that adds an email
		ssoNetworkId := server.NewId()
		ssoUserId := server.NewId()
		Testing_CreateNetworkSso(
			ssoNetworkId,
			ssoUserId,
			AuthJwt{
				AuthType: SsoAuthTypeGoogle,
				UserAuth: fmt.Sprintf("sso-%s@bringyour.com", ssoNetworkId),
			},
			ctx,
		)
		addedEmail := fmt.Sprintf("added-%s@bringyour.com", ssoNetworkId)
		addUnverifiedUserAuth(t, ctx, ssoUserId, addedEmail)
		connect.AssertEqual(t, verifyWithNewCode(t, ctx, addedEmail).NewAccount, false)
	})
}
