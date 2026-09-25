// Exercises network model validation separately from its HTTP error transport.
package model

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

// Every signup method refuses missing terms with plain model/JSON text while
// preserving the status-bearing error consumed by the HTTP controller.
func TestNetworkCreateTermsFail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		byJwt := jwt.ByJwt{}
		clientSession := session.Testing_CreateClientSession(ctx, &byJwt)
		defer clientSession.Cancel()

		userAuth := "terms-refusal@signup.example"
		password, token := "synthetic-password-not-a-secret", "synthetic-provider-token"
		google, apple := string(AuthTypeGoogle), string(AuthTypeApple)
		for _, testCase := range []struct {
			name string
			args NetworkCreateArgs
		}{
			{name: "seedphrase", args: NetworkCreateArgs{}},
			{name: "email", args: NetworkCreateArgs{UserAuth: &userAuth, Password: &password}},
			{name: "google", args: NetworkCreateArgs{AuthJwt: &token, AuthJwtType: &google}},
			{name: "apple", args: NetworkCreateArgs{AuthJwt: &token, AuthJwtType: &apple}},
			{name: "wallet", args: NetworkCreateArgs{WalletAuth: &WalletAuthArgs{}}},
			{name: "orphan-password", args: NetworkCreateArgs{Password: &password}},
			{name: "orphan-provider", args: NetworkCreateArgs{AuthJwtType: &google}},
		} {
			result, err := NetworkCreate(testCase.args, clientSession)
			if err != nil || result == nil || result.Error == nil {
				t.Fatalf("%s terms refusal result=%+v err=%v, want a model error", testCase.name, result, err)
			}
			if result.Error.Message != AgreeToTerms || result.Error.Error() != "400 "+AgreeToTerms {
				t.Fatalf("%s terms refusal message=%q transport=%q, want plain terms text and its 400 transport", testCase.name, result.Error.Message, result.Error.Error())
			}
			raw, err := json.Marshal(result.Error)
			if err != nil || string(raw) != `{"message":"`+AgreeToTerms+`"}` {
				t.Fatalf("%s terms refusal JSON=%s err=%v, want only the unprefixed message", testCase.name, raw, err)
			}
			if result.Network != nil || result.UserAuth != nil || result.Seedphrase != nil || result.VerificationRequired != nil {
				t.Fatalf("%s terms refusal returned a success payload", testCase.name)
			}
		}
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
