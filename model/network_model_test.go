package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestNetworkCreateTermsFail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkCreate := NetworkCreateArgs{
			Terms: false,
		}

		byJwt := jwt.ByJwt{}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		result, err := NetworkCreate(networkCreate, clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error.Message, AgreeToTerms)
	})
}

func TestNetworkUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		networkName := "abcdef"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		sourceSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		// fail
		// network name unavailable
		networkUpdateArgs := NetworkUpdateArgs{
			NetworkName: networkName,
		}
		result, err := NetworkUpdate(networkUpdateArgs, sourceSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.Error, nil)

		// fail
		// network name should be at least 6 characters
		networkUpdateArgs = NetworkUpdateArgs{
			NetworkName: "a",
		}
		result, err = NetworkUpdate(networkUpdateArgs, sourceSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.Error, nil)

		// success
		newName := "uvwxyz"
		networkUpdateArgs = NetworkUpdateArgs{
			NetworkName: newName,
		}
		result, err = NetworkUpdate(networkUpdateArgs, sourceSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)

		network := GetNetwork(sourceSession)
		assert.Equal(t, network.NetworkName, newName)

	})
}

func TestNetworkNameValidation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		// too short
		networkName := ""
		_, err := ValidateNetworkName(networkName)
		assert.NotEqual(t, err, nil)

		// too long
		networkName = "a123456789012345678901234567890123456789012345678901"
		_, err = ValidateNetworkName(networkName)
		assert.NotEqual(t, err, nil)

		/**
		 * testing special characters
		 */
		networkName = "abcde$"
		_, err = ValidateNetworkName(networkName)
		assert.NotEqual(t, err, nil)

		networkName = "abcdeé"
		_, err = ValidateNetworkName(networkName)
		assert.NotEqual(t, err, nil)

		networkName = "東京タワー"
		_, err = ValidateNetworkName(networkName)
		assert.NotEqual(t, err, nil)

		// test spaces
		networkName = "abc def"
		expected := "abc-def"
		validated, err := ValidateNetworkName(networkName)
		assert.Equal(t, validated, expected)

		// valid name should pass
		networkName = "abcdef"
		_, err = ValidateNetworkName(networkName)
		assert.Equal(t, err, nil)

	})
}
