package model

// Sign in with Microsoft through the SSO login (support inbox 7721), on the
// in-memory store: a verified Microsoft email signs in to the account that
// proved it and links Microsoft there, a Microsoft email that no account holds
// signs up, and an unverified claim on the email never lets the Microsoft
// account in. ParseAuthJwt routes the type, and the sign-in methods name it.

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

func TestMicrosoftSignInJoinsTheAccountThatProvedTheEmail(t *testing.T) {
	ctx := context.Background()
	fake := newSsoLoginFakeStore()
	userId := server.NewId()
	userAuth := "person@example.com"
	fake.passwordAuths[userAuth] = userId
	fake.addNetwork(userId, "microsoft-joined")

	result, err := handleLoginParsedAuthJwtWithStore(ssoLoginTestArgs(AuthTypeMicrosoft, "Person@Example.com "), ctx, fake.store())
	if err != nil || result == nil || result.Network == nil {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if len(fake.addedSso) != 1 || fake.addedSso[0].AuthJwtType != SsoAuthTypeMicrosoft || fake.addedSso[0].UserId != userId || !fake.addedSso[0].Notify {
		t.Fatalf("linked = %+v", fake.addedSso)
	}

	// the next sign-in finds the Microsoft row and links nothing again
	again, err := handleLoginParsedAuthJwtWithStore(ssoLoginTestArgs(AuthTypeMicrosoft, userAuth), ctx, fake.store())
	if err != nil || again.Network == nil || len(fake.addedSso) != 1 {
		t.Fatalf("again = %+v, err = %v, linked %d", again, err, len(fake.addedSso))
	}
}

func TestMicrosoftSignInOfANewEmailSignsUp(t *testing.T) {
	fake := newSsoLoginFakeStore()
	args := ssoLoginTestArgs(AuthTypeMicrosoft, "new@example.com")
	args.AuthJwt.UserName = "New Person"
	result, err := handleLoginParsedAuthJwtWithStore(args, context.Background(), fake.store())
	if err != nil || result.Network != nil || result.UserName == nil || *result.UserName != "New Person" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

// An unverified sign-up with the email is only a claim: the Microsoft sign-in
// never joins it.
func TestMicrosoftSignInNeverJoinsAnUnverifiedClaim(t *testing.T) {
	fake := newSsoLoginFakeStore()
	claimantUserId := server.NewId()
	userAuth := "claimed@example.com"
	fake.unverifiedPasswordAuths[userAuth] = claimantUserId
	fake.addNetwork(claimantUserId, "claimant")

	result, err := handleLoginParsedAuthJwtWithStore(ssoLoginTestArgs(AuthTypeMicrosoft, userAuth), context.Background(), fake.store())
	if err != nil || result.Network != nil || len(fake.addedSso) != 0 {
		t.Fatalf("result = %+v, err = %v, linked %d", result, err, len(fake.addedSso))
	}
}

// Microsoft is an SSO provider everywhere the email's proof counts, and an
// unknown type is still refused.
func TestMicrosoftIsAnSsoAuthType(t *testing.T) {
	if !isSsoAuthType(AuthTypeMicrosoft) || !isSsoAuthType(AuthTypeGoogle) || !isSsoAuthType(AuthTypeApple) || isSsoAuthType("facebook") {
		t.Fatal("the SSO auth types are wrong")
	}
	// a Microsoft account's legacy hold is proven, as Google's and Apple's are
	hold := &userAuthHold{legacy: true, createAuthType: AuthTypeMicrosoft}
	if !hold.proven() || hold.signUp() {
		t.Fatalf("a Microsoft-created account's hold: proven %t, sign-up %t", hold.proven(), hold.signUp())
	}
	if _, err := ParseAuthJwt("not-a-token", AuthTypeMicrosoft); err == nil {
		t.Fatal("a malformed Microsoft token parsed")
	}
	if _, err := ParseAuthJwt("not-a-token", "facebook"); err == nil {
		t.Fatal("an unknown auth type parsed")
	}
}
