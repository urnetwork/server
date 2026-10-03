package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server"
)

// ssoLoginFakeStore is an in-memory parsedAuthJwtLoginStore. Login results
// depend only on the rows a test puts in it.
type ssoLoginFakeStore struct {
	ssoAuths      []NetworkUserSsoAuth
	passwordAuths map[string]server.Id
	networks      map[server.Id]string
	networkIds    map[server.Id]server.Id

	addedSso []AddSsoAuthArgs
	signed   []string
	success  int
}

func newSsoLoginFakeStore() *ssoLoginFakeStore {
	return &ssoLoginFakeStore{
		passwordAuths: map[string]server.Id{},
		networks:      map[server.Id]string{},
		networkIds:    map[server.Id]server.Id{},
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
		adminNetwork: func(ctx context.Context, userId server.Id) (server.Id, string, bool) {
			networkName, ok := self.networks[userId]
			return self.networkIds[userId], networkName, ok
		},
		addSsoAuth: func(args *AddSsoAuthArgs, ctx context.Context) error {
			self.addedSso = append(self.addedSso, *args)
			userAuth := args.ParsedAuthJwt.UserAuth
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
