package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// Testing_CreateLegacyGuestNetwork inserts a network_user/network pair shaped
// exactly like the pre-removal networkCreateGuest function used to create:
// auth_type='guest', no user_auth, no password, no rows in any auth table.
// Guest signup is retired, but rows shaped like this still exist in
// production and this fork's code must not mistreat them.
func Testing_CreateLegacyGuestNetwork(ctx context.Context, networkId server.Id, userId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user (user_id, user_name, auth_type) VALUES ($1, $2, $3)`,
			userId, "guest", "guest",
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network (network_id, network_name, admin_user_id) VALUES ($1, $2, $3)`,
			networkId, "g"+networkId.String(), userId,
		))
	})
}

func TestHasAnyAuthMethodReflectsRealAuthMethods(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()

		Testing_CreateLegacyGuestNetwork(ctx, networkId, userId)
		if has := HasAnyAuthMethod(ctx, userId); has {
			t.Fatalf("expected fresh legacy guest to have no auth methods, HasAnyAuthMethod=true")
		}

		userAuth := "guest-upgrade-test@example.com"
		addResult, err := AddAuth(AddAuthMethod{
			UserAuth: &userAuth,
			Password: strPtr("SomeValidPassword123!"),
		}, session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId, UserId: userId, NetworkName: "g" + networkId.String(), GuestMode: true,
		}))
		if err != nil {
			t.Fatalf("AddAuth returned error: %v", err)
		}
		if addResult.Error != nil {
			t.Fatalf("AddAuth returned soft error: %s", addResult.Error.Message)
		}

		if has := HasAnyAuthMethod(ctx, userId); !has {
			t.Fatalf("expected HasAnyAuthMethod=true after AddAuth succeeded, got false")
		}
	})
}

func TestValidateClientIdentityArgsBlocksBareGuestButAllowsUpgradedAccount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()

		Testing_CreateLegacyGuestNetwork(ctx, networkId, userId)

		// a network-level (no ClientId) guest session, as a legacy guest's
		// still-valid pre-refresh JWT would present -- must still be blocked
		// from assigning explicit roles/principal
		guestSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId, UserId: userId, NetworkName: "g" + networkId.String(), GuestMode: true,
		})
		result, err := AuthNetworkClient(&AuthNetworkClientArgs{
			Description: "d",
			DeviceSpec:  "s",
			Roles:       []string{"admin"},
			Principal:   "attacker-controlled-principal",
		}, guestSession)
		assert.Equal(t, err, nil)
		if result.Error == nil {
			t.Fatalf("expected a bare guest account to be rejected when assigning explicit roles/principal, got success")
		}

		// the same account, after adding a real auth method (simulating a
		// guest that upgraded via AddAuth), must now be allowed -- even
		// though nothing here re-signed its JWT (GuestMode is still stale
		// true), and even though network_user.auth_type is still 'guest'
		// (AddAuth never updates it)
		userAuth := "guest-upgrade-test-2@example.com"
		_, err = AddAuth(AddAuthMethod{
			UserAuth: &userAuth,
			Password: strPtr("SomeValidPassword123!"),
		}, guestSession)
		assert.Equal(t, err, nil)

		result, err = AuthNetworkClient(&AuthNetworkClientArgs{
			Description: "d2",
			DeviceSpec:  "s2",
			Roles:       []string{"admin"},
			Principal:   "a-legitimate-principal",
		}, guestSession)
		assert.Equal(t, err, nil)
		if result.Error != nil {
			t.Fatalf("expected an upgraded (formerly-guest) account to be allowed to assign explicit roles/principal, got error: %s", result.Error.Message)
		}
		clientByJwt, err := jwt.ParseByJwt(ctx, *result.ByClientJwt)
		assert.Equal(t, err, nil)
		assert.Equal(t, clientByJwt.Principal, "a-legitimate-principal")
	})
}

func strPtr(s string) *string { return &s }
