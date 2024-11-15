package model

import (
	"context"
	"regexp"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestNetworkCreateGuestMode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkCreate := NetworkCreateArgs{
			Terms:     true,
			GuestMode: true,
		}

		byJwt := jwt.ByJwt{}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		pattern := `^g[0-9a-f]+$`

		// Compile the regex
		regex, err := regexp.Compile(pattern)
		assert.Equal(t, err, nil)

		result, err := NetworkCreate(networkCreate, clientSession)
		assert.Equal(t, err, nil)

		assert.MatchRegex(t, result.Network.NetworkName, regex)
	})
}

func TestNetworkCreateTermsFail(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkCreate := NetworkCreateArgs{
			GuestMode: true,
		}

		byJwt := jwt.ByJwt{}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		result, err := NetworkCreate(networkCreate, clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error.Message, AgreeToTerms)
	})
}

func TestNetworkUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
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
