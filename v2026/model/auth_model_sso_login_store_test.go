package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// ssoLoginFakeStore is an in-memory parsedAuthJwtLoginStore. Login results
// depend only on the rows a test puts in it.
type ssoLoginFakeStore struct {
	ssoAuths      []NetworkUserSsoAuth
	passwordAuths map[string]server.Id
	networks      map[server.Id]string
	networkIds    map[server.Id]server.Id
	// network_user rows: user_auth -> rows
	legacyUsers map[string][]legacyNetworkUser

	addedSso []AddSsoAuthArgs
	signed   []string
	success  int
}

func newSsoLoginFakeStore() *ssoLoginFakeStore {
	return &ssoLoginFakeStore{
		passwordAuths: map[string]server.Id{},
		networks:      map[server.Id]string{},
		networkIds:    map[server.Id]server.Id{},
		legacyUsers:   map[string][]legacyNetworkUser{},
	}
}

func (self *ssoLoginFakeStore) addNetwork(userId server.Id, networkName string) server.Id {
	networkId := server.NewId()
	self.networks[userId] = networkName
	self.networkIds[userId] = networkId
	return networkId
}

func (self *ssoLoginFakeStore) store() *parsedAuthJwtLoginStore {
	return &parsedAuthJwtLoginStore{
		ssoAuthsByUserAuth: func(ctx context.Context, userAuth string) ([]NetworkUserSsoAuth, error) {
			matches := []NetworkUserSsoAuth{}
			for _, ssoAuth := range self.ssoAuths {
				if ssoAuth.UserAuth != nil && *ssoAuth.UserAuth == userAuth {
					matches = append(matches, ssoAuth)
				}
			}
			return matches, nil
		},
		passwordAuthByUserAuth: func(ctx context.Context, userAuth string) (*server.Id, bool, bool) {
			userId, ok := self.passwordAuths[userAuth]
			if !ok {
				return nil, false, false
			}
			return &userId, true, true
		},
		legacyUsersByUserAuth: func(ctx context.Context, normalUserAuth string) ([]legacyNetworkUser, error) {
			return self.legacyUsers[normalUserAuth], nil
		},
		adminNetwork: func(ctx context.Context, userId server.Id) (server.Id, string, bool) {
			networkName, ok := self.networks[userId]
			return self.networkIds[userId], networkName, ok
		},
		addSsoAuth: func(args *AddSsoAuthArgs, ctx context.Context) error {
			self.addedSso = append(self.addedSso, *args)
			// stored normalized, as addSsoAuthInTx does
			userAuth, _ := NormalUserAuth(args.ParsedAuthJwt.UserAuth)
			userId := args.UserId
			self.ssoAuths = append(self.ssoAuths, NetworkUserSsoAuth{
				UserId:   &userId,
				AuthType: SsoAuthType(args.AuthJwtType),
				AuthJwt:  args.AuthJwt,
				UserAuth: &userAuth,
			})
			return nil
		},
		setUserAuthAttemptSuccess: func(ctx context.Context, userAuthAttemptId UserAuthAttemptId) {
			self.success += 1
		},
		signByJwt: func(ctx context.Context, networkId server.Id, userId server.Id, networkName string) string {
			signed := fmt.Sprintf("network=%s user=%s name=%s", networkId, userId, networkName)
			self.signed = append(self.signed, signed)
			return signed
		},
	}
}

func ssoLoginTestArgs(authType AuthType, userAuth string) *HandleLoginParsedAuthJwtArgs {
	return &HandleLoginParsedAuthJwtArgs{
		AuthJwt: AuthJwt{
			AuthType: authType,
			UserAuth: userAuth,
		},
		AuthJwtStr: "synthetic-id-token",
	}
}

// A sign-in row whose user has no network must fail the login. Before the fix
// the not-found check compared the address of a local variable to nil, which
// is never true, so the login signed a JWT for the zero network id.
func TestSsoLoginWithoutNetworkFails(t *testing.T) {
	fake := newSsoLoginFakeStore()
	userId := server.NewId()
	userAuth := "orphan@example.invalid"
	fake.ssoAuths = append(fake.ssoAuths, NetworkUserSsoAuth{
		UserId:   &userId,
		AuthType: SsoAuthTypeApple,
		UserAuth: &userAuth,
	})

	result, err := handleLoginParsedAuthJwtWithStore(
		ssoLoginTestArgs(AuthTypeApple, userAuth),
		context.Background(),
		fake.store(),
	)
	if err == nil {
		t.Fatalf("login without a network succeeded: result=%+v signed=%v", result, fake.signed)
	}
	if len(fake.signed) != 0 || fake.success != 0 {
		t.Fatalf("login without a network signed %v and marked %d successes", fake.signed, fake.success)
	}
}

func TestSsoLoginWithNetworkSignsThatNetwork(t *testing.T) {
	fake := newSsoLoginFakeStore()
	userId := server.NewId()
	userAuth := "member@example.invalid"
	fake.ssoAuths = append(fake.ssoAuths, NetworkUserSsoAuth{
		UserId:   &userId,
		AuthType: SsoAuthTypeApple,
		UserAuth: &userAuth,
	})
	networkId := fake.addNetwork(userId, "synthetic")

	result, err := handleLoginParsedAuthJwtWithStore(
		ssoLoginTestArgs(AuthTypeApple, userAuth),
		context.Background(),
		fake.store(),
	)
	if err != nil || result == nil || result.Network == nil {
		t.Fatalf("login result=%+v err=%v, want a network", result, err)
	}
	want := fmt.Sprintf("network=%s user=%s name=synthetic", networkId, userId)
	if result.Network.ByJwt != want {
		t.Fatalf("signed %q, want %q", result.Network.ByJwt, want)
	}
	if fake.success != 1 || len(fake.addedSso) != 0 {
		t.Fatalf("successes=%d added sso=%v, want 1 success and no new sso row", fake.success, fake.addedSso)
	}
}

func (self *ssoLoginFakeStore) addLegacyUser(userAuth string, authType AuthType) server.Id {
	userId := server.NewId()
	self.legacyUsers[userAuth] = append(self.legacyUsers[userAuth], legacyNetworkUser{
		UserId:   userId,
		AuthType: string(authType),
	})
	return userId
}

// An account created with Apple sign-in whose network_user_auth_sso row was
// never migrated logs in with Apple. Before the fix the login found no sso or
// password row and sent the user to network creation, which then failed
// because network_user already held the email (inbox 501).
func TestSsoLoginAppleLegacyUnmigratedLogsInAndBackfillsOnce(t *testing.T) {
	fake := newSsoLoginFakeStore()
	userAuth := "legacy@example.invalid"
	userId := fake.addLegacyUser(userAuth, AuthTypeApple)
	networkId := fake.addNetwork(userId, "legacy")
	want := fmt.Sprintf("network=%s user=%s name=legacy", networkId, userId)

	for i := 0; i < 2; i += 1 {
		result, err := handleLoginParsedAuthJwtWithStore(
			// the token email may differ in case from the stored, normalized one
			ssoLoginTestArgs(AuthTypeApple, " Legacy@Example.invalid"),
			context.Background(),
			fake.store(),
		)
		if err != nil || result == nil || result.Network == nil {
			t.Fatalf("login %d: result=%+v err=%v, want the legacy account's network", i, result, err)
		}
		if result.Network.ByJwt != want {
			t.Fatalf("login %d: signed %q, want %q", i, result.Network.ByJwt, want)
		}
	}
	if len(fake.addedSso) != 1 {
		t.Fatalf("sso rows added %d times, want one backfill", len(fake.addedSso))
	}
	backfill := fake.addedSso[0]
	if backfill.UserId != userId || backfill.AuthJwtType != SsoAuthTypeApple {
		t.Fatalf("backfill = %+v, want an apple row for user %s", backfill, userId)
	}
}

// The fallback never logs in to an account that was not created with Apple
// sign-in, even when the verified email matches.
func TestSsoLoginAppleLegacyFallbackNeedsAppleAccount(t *testing.T) {
	for _, authType := range []AuthType{AuthTypeGoogle, AuthType(UserAuthTypeEmail), AuthType("password"), AuthType("guest")} {
		t.Run(string(authType), func(t *testing.T) {
			fake := newSsoLoginFakeStore()
			userAuth := "other@example.invalid"
			userId := fake.addLegacyUser(userAuth, authType)
			fake.addNetwork(userId, "other")

			result, err := handleLoginParsedAuthJwtWithStore(
				ssoLoginTestArgs(AuthTypeApple, userAuth),
				context.Background(),
				fake.store(),
			)
			if err != nil || result == nil || result.Network != nil || result.UserName == nil {
				t.Fatalf("result=%+v err=%v, want the new-network route", result, err)
			}
			if len(fake.signed) != 0 || len(fake.addedSso) != 0 {
				t.Fatalf("signed %v and added sso %v for a %s account", fake.signed, fake.addedSso, authType)
			}
		})
	}
}

// Only an Apple sign-in uses the fallback.
func TestSsoLoginLegacyFallbackIsAppleOnly(t *testing.T) {
	fake := newSsoLoginFakeStore()
	userAuth := "apple@example.invalid"
	userId := fake.addLegacyUser(userAuth, AuthTypeApple)
	fake.addNetwork(userId, "apple")

	result, err := handleLoginParsedAuthJwtWithStore(
		ssoLoginTestArgs(AuthTypeGoogle, userAuth),
		context.Background(),
		fake.store(),
	)
	if err != nil || result == nil || result.Network != nil {
		t.Fatalf("google login result=%+v err=%v, want no login to the apple account", result, err)
	}
	if len(fake.signed) != 0 || len(fake.addedSso) != 0 {
		t.Fatalf("signed %v and added sso %v", fake.signed, fake.addedSso)
	}
}

// Sign-in rows are stored under the normalized user auth (trimmed,
// lowercase). A provider token whose email differs from the stored one only in
// case or surrounding spaces logs in to the stored account. Before the fix the
// sso and password lookups used the raw token email, missed the stored rows,
// and sent the user to network creation, which then failed with a conflict.
func TestSsoLoginTokenEmailIsNormalizedBeforeLookup(t *testing.T) {
	type ssoLoginNormalizeCase struct {
		authType AuthType
		// the stored row is an sso row of this type, or a password row if empty
		storedSsoType SsoAuthType
	}
	cases := []ssoLoginNormalizeCase{
		{authType: AuthTypeGoogle, storedSsoType: SsoAuthTypeGoogle},
		{authType: AuthTypeApple, storedSsoType: SsoAuthTypeApple},
		{authType: AuthTypeGoogle, storedSsoType: SsoAuthTypeApple},
		{authType: AuthTypeGoogle, storedSsoType: ""},
		{authType: AuthTypeApple, storedSsoType: ""},
	}
	storedUserAuth := "mixed.case@example.invalid"
	tokenUserAuth := "  Mixed.Case@Example.INVALID "
	for _, c := range cases {
		fake := newSsoLoginFakeStore()
		userId := server.NewId()
		if c.storedSsoType == "" {
			fake.passwordAuths[storedUserAuth] = userId
		} else {
			fake.ssoAuths = append(fake.ssoAuths, NetworkUserSsoAuth{
				UserId:   &userId,
				AuthType: c.storedSsoType,
				UserAuth: &storedUserAuth,
			})
		}
		networkId := fake.addNetwork(userId, "stored")

		result, err := handleLoginParsedAuthJwtWithStore(
			ssoLoginTestArgs(c.authType, tokenUserAuth),
			context.Background(),
			fake.store(),
		)
		if err != nil || result == nil || result.Network == nil {
			t.Fatalf("%s token, stored %q row: result=%+v err=%v, want the stored account's network", c.authType, c.storedSsoType, result, err)
		}
		want := fmt.Sprintf("network=%s user=%s name=stored", networkId, userId)
		if result.Network.ByJwt != want {
			t.Fatalf("%s token, stored %q row: signed %q, want %q", c.authType, c.storedSsoType, result.Network.ByJwt, want)
		}
	}
}

// An unparseable token email normalizes to empty and must not look up rows
// stored under an empty user auth.
func TestSsoLoginUnparseableTokenEmailMatchesNoRow(t *testing.T) {
	fake := newSsoLoginFakeStore()
	userId := server.NewId()
	emptyUserAuth := ""
	fake.ssoAuths = append(fake.ssoAuths, NetworkUserSsoAuth{
		UserId:   &userId,
		AuthType: SsoAuthTypeGoogle,
		UserAuth: &emptyUserAuth,
	})
	fake.passwordAuths[emptyUserAuth] = userId
	fake.addNetwork(userId, "empty")

	result, err := handleLoginParsedAuthJwtWithStore(
		ssoLoginTestArgs(AuthTypeGoogle, "not an email"),
		context.Background(),
		fake.store(),
	)
	if err != nil || result == nil || result.Network != nil {
		t.Fatalf("result=%+v err=%v, want no login", result, err)
	}
	if len(fake.signed) != 0 || len(fake.addedSso) != 0 {
		t.Fatalf("signed %v and added sso %v", fake.signed, fake.addedSso)
	}
}
