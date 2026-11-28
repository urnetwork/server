package model

// A credential rotation ends the OAuth grants of the account: a password reset,
// a sign-out-all, and an unverified sign-up handed to the verifier of its
// address. Before, none of them touched the oauth tables, so an application
// approved before the rotation kept its refresh token (90 days, sliding with
// each use), its remembered approval and its unexchanged codes. Each test
// drives the real rotation against the database.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/oauth"
	"github.com/urnetwork/server/v2026/session"
)

// An application the user approved, working before the rotation.
func oauthRotationTestGrant(t testing.TB, ctx context.Context, networkId server.Id, userId server.Id, credentialTime time.Time) *oauth.Testing_Grant {
	t.Helper()
	grant, err := oauth.Testing_CreateGrant(ctx, userId, networkId, credentialTime)
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	if err := grant.Testing_Refresh(ctx); err != nil {
		t.Fatalf("the grant did not refresh before the rotation: %v", err)
	}
	if !grant.Testing_Remembered(ctx) {
		t.Fatalf("the approval was not remembered before the rotation")
	}
	return grant
}

// The grant ended: its refresh token is revoked and refused, its unexchanged
// code is gone, and the approval is forgotten, so the consent page asks again.
func requireOauthGrantEnded(t testing.TB, ctx context.Context, grant *oauth.Testing_Grant) {
	t.Helper()
	if count := countRows(ctx, `SELECT COUNT(*) FROM oauth_refresh_token WHERE user_id = $1 AND client_id = $2 AND revoke_time IS NULL`, grant.UserId, grant.ClientId); count != 0 {
		t.Fatalf("%d refresh tokens of the grant are not revoked", count)
	}
	if err := grant.Testing_Refresh(ctx); err == nil {
		t.Fatalf("the refresh token from before the rotation still refreshes")
	}
	if err := grant.Testing_RedeemPendingCode(ctx); err == nil {
		t.Fatalf("a code from before the rotation was redeemed")
	}
	if grant.Testing_Remembered(ctx) {
		t.Fatalf("the approval from before the rotation is remembered")
	}
}

func TestPasswordResetEndsOauthGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, userId := server.NewId(), server.NewId()
		userAuth := Testing_CreateNetwork(ctx, networkId, "oauth-reset", userId)
		actor := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "oauth-reset", false, false))
		defer actor.Cancel()

		grant := oauthRotationTestGrant(t, ctx, networkId, userId, server.NowUtc())

		reset, err := AuthPasswordResetCreateCode(AuthPasswordResetCreateCodeArgs{UserAuth: userAuth}, actor)
		if err != nil || reset.ResetCode == nil {
			t.Fatal(reset, err)
		}
		result, err := AuthPasswordSet(AuthPasswordSetArgs{ResetCode: *reset.ResetCode, Password: "rotated-password"}, actor)
		if err != nil || result == nil || result.Error != nil {
			t.Fatal("reset failed", result, err)
		}

		requireOauthGrantEnded(t, ctx, grant)

		// a sign-in after the reset approves the application again
		again, err := oauth.Testing_CreateGrant(ctx, userId, networkId, server.NowUtc())
		if err != nil {
			t.Fatalf("approval after the reset: %v", err)
		}
		if err := again.Testing_Refresh(ctx); err != nil {
			t.Fatalf("a grant after the reset does not refresh: %v", err)
		}
	})
}

func TestNetworkSignOutAllEndsOauthGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId, userId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, networkId, "oauth-sign-out", userId)

		grant := oauthRotationTestGrant(t, ctx, networkId, userId, server.NowUtc())

		plan, err := PlanNetworkSignOut(ctx, networkId, false)
		if err != nil || plan.Refusal != "" {
			t.Fatal(plan, err)
		}
		result, err := CommitNetworkSignOut(ctx, networkId, false, plan.Digest)
		if err != nil || result.EnforcementError != nil {
			t.Fatal(result, err)
		}
		if result.RevokedOauthGrantCount != 1 {
			t.Fatalf("the sign-out reports %d applications disconnected, want 1", result.RevokedOauthGrantCount)
		}

		requireOauthGrantEnded(t, ctx, grant)
	})
}

// The verifier of an unverified sign-up's address takes the account
// (auth_verify_claim.go). An application approved for the account before, by
// whoever signed up, ends with the rest of its credentials.
func TestVerifierTakeoverEndsOauthGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userAuth := emailClaimTestAddress("oauth-handover")
		networkId, userId := emailClaimSignUp(t, ctx, emailClaimTestSession(ctx, 11, nil), userAuth, "claimant-password")

		// approved by a sign-in of the claimant's
		grant := oauthRotationTestGrant(t, ctx, networkId, userId, server.NowUtc())

		verifier := emailClaimTestSession(ctx, 13, nil)
		code := emailClaimCode(t, ctx, verifier, userAuth)
		result := emailClaimVerify(t, verifier, userAuth, code)
		if result.Error != nil || result.Network == nil {
			t.Fatalf("the verifier was not signed in: %+v", result)
		}

		requireOauthGrantEnded(t, ctx, grant)
	})
}
