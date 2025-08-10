package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
)

func TestRemoveNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

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
		assert.Equal(t, err, nil)

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
		assert.NotEqual(t, networkUser, nil)
		assert.Equal(t, len(networkUser.UserAuths), 1)
		assert.Equal(t, len(networkUser.SsoAuths), 1)
		assert.Equal(t, len(networkUser.WalletAuths), 1)

		RemoveNetwork(ctx, networkId, userId)

		networkUser = GetNetworkUser(ctx, userId)
		assert.Equal(t, networkUser, nil)

		userAuths, err := getUserAuths(userId, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(userAuths), 0)

		ssoAuths, err := getSsoAuths(ctx, userId)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(ssoAuths), 0)

		walletAuths, err := getWalletAuths(ctx, userId)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(walletAuths), 0)

	})
}
