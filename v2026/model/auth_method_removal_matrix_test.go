package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The removal matrix: for every auth type a user can actually unlink, removing
// a BOUND method must succeed and must disappear from auth_types.
//
// The suite already covered the refusal direction thoroughly -- removing a
// method that is not bound, and removing the last one -- but the success
// direction was covered only for email and the wallet types. Nothing removed a
// bound apple or google, and "phone" appeared in no test at all.
//
// That gap was load bearing rather than cosmetic. Mutating the SSO branch's
// unlink condition so that no SSO row can ever match left the entire suite
// green, which means the apple/google success path had no witness anywhere.
// This test is that witness, and it is table driven so a newly supported auth
// type is one line rather than a new function.
func TestRemoveBoundAuthMethodMatrix(t *testing.T) {
	// Each case plants its method on an account that already holds email, then
	// removes it. Email is the co-method that keeps the last-method guard out
	// of the way, so a failure here is the removal itself and never the guard.
	cases := []struct {
		authType string
		plant    func(t testing.TB, ctx context.Context, userId server.Id)
	}{
		{
			authType: "apple",
			plant: func(t testing.TB, ctx context.Context, userId server.Id) {
				addTestSsoAuth(t, ctx, userId, SsoAuthTypeApple, "matrix-apple@example.com")
			},
		},
		{
			authType: "google",
			plant: func(t testing.TB, ctx context.Context, userId server.Id) {
				addTestSsoAuth(t, ctx, userId, SsoAuthTypeGoogle, "matrix-google@example.com")
			},
		},
		{
			authType: "seedphrase",
			plant: func(t testing.TB, ctx context.Context, userId server.Id) {
				if _, err := GenerateSeedphrase(ctx, userId); err != nil {
					t.Fatalf("GenerateSeedphrase: %v", err)
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.authType, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				userId, _, _ := authParityAccount(t, ctx, "rm-"+c.authType)
				c.plant(t, ctx, userId)

				before := authTypesOf(t, ctx, userId)
				if !hasAuthType(before, c.authType) {
					t.Fatalf("fixture did not bind %s: auth_types = %#v", c.authType, before)
				}

				if err := RemoveAuth(ctx, userId, c.authType); err != nil {
					t.Fatalf("removing a bound %s failed: %v", c.authType, err)
				}

				after := authTypesOf(t, ctx, userId)
				if hasAuthType(after, c.authType) {
					t.Fatalf("%s still present after removal: auth_types = %#v", c.authType, after)
				}
				if !hasAuthType(after, "email") {
					t.Fatalf("removing %s also removed the co-method: auth_types = %#v", c.authType, after)
				}
			})
		})
	}
}

// Removing one SSO provider must leave the other bound.
//
// The two share a table and are distinguished only by the auth_type column, so
// a DELETE that forgot to scope on it would take both and no single-provider
// test could see it.
func TestRemoveOneSsoLeavesTheOther(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId, _, _ := authParityAccount(t, ctx, "two-sso")
		addTestSsoAuth(t, ctx, userId, SsoAuthTypeApple, "both-apple@example.com")
		addTestSsoAuth(t, ctx, userId, SsoAuthTypeGoogle, "both-google@example.com")

		if err := RemoveAuth(ctx, userId, "apple"); err != nil {
			t.Fatalf("removing apple failed: %v", err)
		}

		authTypes := authTypesOf(t, ctx, userId)
		if hasAuthType(authTypes, "apple") {
			t.Fatalf("apple survived its own removal: %#v", authTypes)
		}
		if !hasAuthType(authTypes, "google") {
			t.Fatalf("removing apple took google with it: %#v", authTypes)
		}
	})
}
