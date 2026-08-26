package model

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/gagliardetto/solana-go"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type acceptanceWalletSigner struct {
	blockchain string
	address    string
	sign       func(string) (string, error)
}

func newSolanaAcceptanceWalletSigner(t testing.TB) acceptanceWalletSigner {
	t.Helper()
	privateKey, err := solana.NewRandomPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return acceptanceWalletSigner{
		blockchain: SOL.String(),
		address:    privateKey.PublicKey().String(),
		sign: func(message string) (string, error) {
			signature, err := privateKey.Sign([]byte(message))
			if err != nil {
				return "", err
			}
			return base64.StdEncoding.EncodeToString(signature[:]), nil
		},
	}
}

func newBittensorAcceptanceWalletSigner(t testing.TB) acceptanceWalletSigner {
	t.Helper()
	secretKey, publicKey, err := schnorrkel.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	return acceptanceWalletSigner{
		blockchain: TAO.String(),
		address:    testingSS58Encode(42, publicKey.Encode()),
		sign: func(message string) (string, error) {
			wrapped := "<Bytes>" + message + "</Bytes>"
			transcript := schnorrkel.NewSigningContext([]byte("substrate"), []byte(wrapped))
			signature, err := secretKey.Sign(transcript)
			if err != nil {
				return "", err
			}
			encoded := signature.Encode()
			return hex.EncodeToString(encoded[:]), nil
		},
	}
}

func signedAcceptanceWalletChallenge(
	t testing.TB,
	ctx context.Context,
	signer acceptanceWalletSigner,
) *WalletAuthArgs {
	t.Helper()
	blockchain := signer.blockchain
	address := signer.address
	challenge := CreateWalletAuthChallenge(WalletAuthChallengeArgs{
		WalletAddress: &address,
		Blockchain:    &blockchain,
	}, ctx)
	if challenge.Error != nil {
		t.Fatalf("create challenge: %s", challenge.Error.Message)
	}
	signature, err := signer.sign(challenge.MessageTemplate)
	if err != nil {
		t.Fatal(err)
	}
	return &WalletAuthArgs{
		PublicKey:  signer.address,
		Signature:  signature,
		Message:    challenge.MessageTemplate,
		Blockchain: signer.blockchain,
	}
}

func TestWalletNetworkCreateAndLoginLifecycle(t *testing.T) {
	tests := []struct {
		name      string
		network   string
		authType  AuthType
		newSigner func(testing.TB) acceptanceWalletSigner
	}{
		{name: "solana", network: "acceptance-solana-wallet", authType: AuthTypeSolana, newSigner: newSolanaAcceptanceWalletSigner},
		{name: "bittensor", network: "acceptance-bittensor-wallet", authType: AuthTypeBittensor, newSigner: newBittensorAcceptanceWalletSigner},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				clientSession := session.Testing_CreateClientSession(ctx, nil)
				signer := test.newSigner(t)

				// Discovery consumes its challenge and reports an unregistered wallet.
				discoveryAuth := signedAcceptanceWalletChallenge(t, ctx, signer)
				discovery, err := handleLoginWallet(discoveryAuth, ctx)
				if err != nil {
					t.Fatal(err)
				}
				if discovery.Network != nil || discovery.WalletAuth == nil {
					t.Fatalf("new wallet discovery = %#v", discovery)
				}

				// Creation requires a fresh challenge; the discovery signature is
				// deliberately not replayed.
				createAuth := signedAcceptanceWalletChallenge(t, ctx, signer)
				created, err := NetworkCreate(NetworkCreateArgs{
					NetworkName: test.network,
					Terms:       true,
					WalletAuth:  createAuth,
				}, clientSession)
				if err != nil {
					t.Fatal(err)
				}
				if created.Error != nil {
					t.Fatal(created.Error.Message)
				}
				if created.Network == nil || created.Network.ByJwt == nil {
					t.Fatalf("wallet signup did not return JWT: %#v", created)
				}

				walletAuths, err := getWalletAuthsByAddress(ctx, signer.address)
				if err != nil {
					t.Fatal(err)
				}
				if len(walletAuths) != 1 || walletAuths[0].UserId == nil {
					t.Fatalf("wallet binding = %#v", walletAuths)
				}
				userId := *walletAuths[0].UserId
				networkUser := GetNetworkUser(ctx, userId)
				if networkUser == nil || networkUser.AuthType != test.authType {
					t.Fatalf("network user auth type = %#v, want %s", networkUser, test.authType)
				}
				if len(networkUser.AuthTypes) != 1 || networkUser.AuthTypes[0] != test.authType {
					t.Fatalf("auth types = %#v, want [%s]", networkUser.AuthTypes, test.authType)
				}

				loginAuth := signedAcceptanceWalletChallenge(t, ctx, signer)
				login, err := handleLoginWallet(loginAuth, ctx)
				if err != nil {
					t.Fatal(err)
				}
				if login.Network == nil || login.Network.ByJwt == "" {
					t.Fatalf("wallet login did not return JWT: %#v", login)
				}

				// Replay protection remains active after successful login.
				if _, err := handleLoginWallet(loginAuth, ctx); err == nil {
					t.Fatal("replayed wallet login challenge was accepted")
				}

				RemoveNetwork(ctx, created.Network.NetworkId, &userId)
				afterDelete, err := handleLoginWallet(signedAcceptanceWalletChallenge(t, ctx, signer), ctx)
				if err != nil {
					t.Fatal(err)
				}
				if afterDelete.Network != nil || afterDelete.WalletAuth == nil {
					t.Fatalf("deleted wallet still resolved to a network: %#v", afterDelete)
				}
			})
		})
	}
}
