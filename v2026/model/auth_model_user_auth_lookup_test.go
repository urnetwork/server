package model

import (
	"testing"

	"github.com/urnetwork/server/v2026"
)

// fakeUserAuthUserIdLookup stands in for the two tables: passwordAuths is
// network_user_auth_password, legacy is network_user.user_auth.
type fakeUserAuthUserIdLookup struct {
	passwordAuths map[string]server.Id
	legacy        map[string]server.Id
}

func (self *fakeUserAuthUserIdLookup) passwordAuthUserId(userAuth string) *server.Id {
	if userId, ok := self.passwordAuths[userAuth]; ok {
		return &userId
	}
	return nil
}

func (self *fakeUserAuthUserIdLookup) legacyUserAuthUserId(userAuth string) *server.Id {
	if userId, ok := self.legacy[userAuth]; ok {
		return &userId
	}
	return nil
}

// Verification and reset codes must find the user of an email or phone added
// with AddAuth. That sign-in exists only in network_user_auth_password;
// network_user.user_auth keeps the identity the account was created with (an
// SSO email, or nothing for a wallet account).
func TestFindUserIdByUserAuth(t *testing.T) {
	walletUserId := server.NewId()
	ssoUserId := server.NewId()
	legacyUserId := server.NewId()
	lookup := &fakeUserAuthUserIdLookup{
		passwordAuths: map[string]server.Id{
			// added to a wallet account: no network_user.user_auth at all
			"+16097370011": walletUserId,
			// added to an SSO account whose user_auth is its SSO email
			"added@example.com": ssoUserId,
			// created with a password: in both places
			"legacy@example.com": legacyUserId,
		},
		legacy: map[string]server.Id{
			"sso@example.com":    ssoUserId,
			"legacy@example.com": legacyUserId,
			"old@example.com":    legacyUserId,
		},
	}

	for _, test := range []struct {
		userAuth string
		want     *server.Id
	}{
		{userAuth: "+16097370011", want: &walletUserId},
		{userAuth: "added@example.com", want: &ssoUserId},
		{userAuth: "legacy@example.com", want: &legacyUserId},
		// accounts whose only identity is the legacy column still resolve
		{userAuth: "sso@example.com", want: &ssoUserId},
		{userAuth: "old@example.com", want: &legacyUserId},
		{userAuth: "nobody@example.com", want: nil},
	} {
		got := findUserIdByUserAuth(lookup, test.userAuth)
		switch {
		case test.want == nil && got != nil:
			t.Errorf("%s resolved to %s, want no user", test.userAuth, got)
		case test.want != nil && got == nil:
			t.Errorf("%s resolved to no user, want %s (no code would be created or sent)", test.userAuth, test.want)
		case test.want != nil && *got != *test.want:
			t.Errorf("%s resolved to %s, want %s", test.userAuth, got, test.want)
		}
	}
}

// When both places name a user, the sign-in table is authoritative.
func TestFindUserIdByUserAuthPrefersSignInTable(t *testing.T) {
	signInUserId := server.NewId()
	staleUserId := server.NewId()
	lookup := &fakeUserAuthUserIdLookup{
		passwordAuths: map[string]server.Id{"moved@example.com": signInUserId},
		legacy:        map[string]server.Id{"moved@example.com": staleUserId},
	}
	got := findUserIdByUserAuth(lookup, "moved@example.com")
	if got == nil || *got != signInUserId {
		t.Fatalf("resolved to %v, want sign-in table user %s", got, signInUserId)
	}
}
