package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

func TestRemoveNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		email := fmt.Sprintf("%s@bringyour.com", networkId)

		/**
		 * Add SSO Auth
		 */
		parsedAuthJwt := AuthJwt{
			AuthType: SsoAuthTypeGoogle,
			UserAuth: email,
			UserName: "",
		}

		err := addSsoAuth(&AddSsoAuthArgs{
			ParsedAuthJwt: parsedAuthJwt,
			AuthJwt:       "",
			AuthJwtType:   SsoAuthTypeGoogle,
			UserId:        userId,
		}, ctx)
		connect.AssertEqual(t, err, nil)

		/**
		 * Add Wallet Auth
		 */

		pk := "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s"
		signature := "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw=="
		message := "Welcome to URnetwork"

		addWalletAuth(
			&AddWalletAuthArgs{
				WalletAuth: &WalletAuthArgs{
					PublicKey:  pk,
					Blockchain: AuthTypeSolana,
					Message:    message,
					Signature:  signature,
				},
				UserId: userId,
			},
			ctx,
		)

		networkUser := GetNetworkUser(ctx, userId)
		connect.AssertNotEqual(t, networkUser, nil)
		connect.AssertEqual(t, len(networkUser.UserAuths), 1)
		connect.AssertEqual(t, len(networkUser.SsoAuths), 1)
		connect.AssertEqual(t, len(networkUser.WalletAuths), 1)

		RemoveNetwork(ctx, networkId, &userId)

		networkUser = GetNetworkUser(ctx, userId)
		connect.AssertEqual(t, networkUser, nil)

		userAuths, err := getUserAuths(userId, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(userAuths), 0)

		ssoAuths, err := getSsoAuths(ctx, userId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(ssoAuths), 0)

		walletAuths, err := getWalletAuths(ctx, userId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletAuths), 0)

	})
}
