package model

import (
	"context"
	"regexp"
	"testing"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/session"
	"github.com/go-playground/assert/v2"
)

func TestNetworkCreateGuestMode(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkCreate := NetworkCreateArgs{
			Terms:     true,
			GuestMode: true,
		}

		byJwt := jwt.ByJwt{}

		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)

		pattern := `^guest`

		// Compile the regex
		regex, err := regexp.Compile(pattern)
		assert.Equal(t, err, nil)

		result, err := NetworkCreate(networkCreate, clientSession)
		assert.Equal(t, err, nil)

		assert.MatchRegex(t, result.Network.NetworkName, regex)
	})
}

func TestNetworkCreateTermsFail(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
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
