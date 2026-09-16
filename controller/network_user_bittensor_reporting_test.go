package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A canonical prefix-42 ss58 address. The reporting surface never verifies a
// signature, so a well-formed address is all these need.
const testingTaoAddress = "5DkvcgoNiEkRPcuej5RvFi5M27ZtYkkE3ftrsuFgCUqtrRyh"

// GET /network/user is the surface a client reads to decide which sign-in
// methods to show, and which of them it may offer to remove. Everything that
// asserts Bittensor is reported correctly does so against model.GetNetworkUser
// directly -- but this PR's own comments note that the model layer is
// structurally blind to how the controller shapes a response, and the existing
// controller test here asserts only the legacy scalar AuthType.
//
// So the composite fields that actually drive the client had no assertion above
// the model: removing the json tag from WalletAuths[].Blockchain, or dropping
// the wallet entry from AuthTypes, broke no test. These two pin them.
func TestGetNetworkUserReportsBittensorWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "taoreport", userId)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		// Bind a TAO wallet the way the account model does, then read the
		// account back through the controller the client actually calls.
		model.Testing_AddWalletAuth(ctx, userId, model.TAO.String(), testingTaoAddress)

		result, err := GetNetworkUser(userSession)
		if err != nil {
			t.Fatalf("GetNetworkUser: %v", err)
		}
		if result == nil || result.NetworkUser == nil {
			t.Fatal("GetNetworkUser returned no network user")
		}

		// auth_types is what the client lists as sign-in methods.
		var sawBittensor bool
		for _, authType := range result.NetworkUser.AuthTypes {
			if authType == "bittensor" {
				sawBittensor = true
			}
			if authType == "solana" {
				t.Errorf("a TAO wallet reported as solana in auth_types: %#v",
					result.NetworkUser.AuthTypes)
			}
		}
		if !sawBittensor {
			t.Errorf("auth_types = %#v, want it to contain \"bittensor\"",
				result.NetworkUser.AuthTypes)
		}

		// wallet_auths[].blockchain is how a client tells one wallet chain from
		// another. Without it a TAO wallet and a SOL wallet are indistinguishable.
		if len(result.NetworkUser.WalletAuths) == 0 {
			t.Fatal("wallet_auths is empty for an account with a bound TAO wallet")
		}
		var sawTao bool
		for _, walletAuth := range result.NetworkUser.WalletAuths {
			if walletAuth.Blockchain == model.TAO.String() {
				sawTao = true
			}
		}
		if !sawTao {
			t.Errorf("no wallet_auths entry carries blockchain %q", model.TAO.String())
		}
	})
}

// The same surface must not claim a chain the account does not have: an account
// with no wallet reports no wallet, rather than an empty-stringed entry that a
// client would render as an unknown chain.
func TestGetNetworkUserReportsNoWalletWhenNoneBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "nowallet", userId)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		result, err := GetNetworkUser(userSession)
		if err != nil {
			t.Fatalf("GetNetworkUser: %v", err)
		}
		if len(result.NetworkUser.WalletAuths) != 0 {
			t.Errorf("wallet_auths = %#v for an account with no wallet",
				result.NetworkUser.WalletAuths)
		}
		for _, authType := range result.NetworkUser.AuthTypes {
			if authType == "bittensor" || authType == "solana" {
				t.Errorf("auth_types claims a wallet method %q with no wallet bound: %#v",
					authType, result.NetworkUser.AuthTypes)
			}
		}
	})
}
