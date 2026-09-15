package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Wallet binding and late-signature behaviour against the local stack.

// writeWalletAuthRowForTest binds a wallet with an arbitrary blockchain label,
// bypassing addWalletAuthInTx so a legacy column value can be reproduced.
func writeWalletAuthRowForTest(
	t testing.TB,
	ctx context.Context,
	userId server.Id,
	walletAddress string,
	blockchain string,
) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user_auth_wallet
				(user_id, wallet_address, blockchain)
				VALUES ($1, $2, $3)
			`,
			userId,
			walletAddress,
			blockchain,
		))
	})
}

// backdateWalletAuthChallengeForTest moves an issued challenge back in time,
// keeping create_time and expire_time consistent with each other, so a
// signature that arrives `age` later can be exercised without sleeping.
func backdateWalletAuthChallengeForTest(
	t testing.TB,
	ctx context.Context,
	challengeValue string,
	age time.Duration,
) time.Time {
	t.Helper()
	var createTime time.Time
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
				UPDATE wallet_auth_challenge
				SET create_time = create_time - $2::interval,
					expire_time = expire_time - $2::interval
				WHERE challenge_value = $1
				RETURNING create_time
			`,
			challengeValue,
			age.String(),
		)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				t.Fatalf("challenge %s was not stored", challengeValue)
			}
			server.Raise(result.Scan(&createTime))
		})
	})
	return createTime
}

// A Bittensor signature that comes back 90 seconds later -- the ordinary cost
// of a WalletConnect relay round trip to a phone -- must still be accepted,
// because the api advertises and returns expires_in: 300.
func TestWalletAuthChallengeBittensorLateSignature(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		wallet := newTestingBittensorWallet(t)
		blockchain := TAO.String()
		address := wallet.address

		for _, age := range []time.Duration{90 * time.Second, 240 * time.Second} {
			challenge := CreateWalletAuthChallenge(WalletAuthChallengeArgs{
				WalletAddress: &address,
				Blockchain:    &blockchain,
			}, ctx)
			connect.AssertEqual(t, challenge.Error, nil)
			connect.AssertEqual(t, challenge.ExpiresIn, int64(300))

			createTime := backdateWalletAuthChallengeForTest(t, ctx, challenge.Challenge, age)
			message := FormatWalletAuthChallengeMessage(challenge.Challenge, createTime.Unix())

			result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
				Blockchain: "tao",
				PublicKey:  wallet.address,
				Message:    message,
				Signature:  wallet.sign(message),
			}, ctx)
			connect.AssertEqual(t, err, nil)
			if !result.Valid {
				t.Fatalf("age %s: %s", age, result.Error.Message)
			}
		}

		// past the lifetime the row's own expire_time is what rejects it,
		// which is the deadline the schema and the api always described
		challenge := CreateWalletAuthChallenge(WalletAuthChallengeArgs{
			WalletAddress: &address,
			Blockchain:    &blockchain,
		}, ctx)
		connect.AssertEqual(t, challenge.Error, nil)
		createTime := backdateWalletAuthChallengeForTest(t, ctx, challenge.Challenge, 310*time.Second)
		message := FormatWalletAuthChallengeMessage(challenge.Challenge, createTime.Unix())
		result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "tao",
			PublicKey:  wallet.address,
			Message:    message,
			Signature:  wallet.sign(message),
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Valid, false)
		connect.AssertEqual(t, result.Error.Message, "403 challenge expired")
	})
}

// The full Bittensor deny matrix against the stored challenge.
func TestWalletAuthChallengeBittensorDenyMatrix(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		wallet := newTestingBittensorWallet(t)
		other := newTestingBittensorWallet(t)
		blockchain := TAO.String()
		address := wallet.address

		newChallenge := func() *WalletAuthChallengeResult {
			challenge := CreateWalletAuthChallenge(WalletAuthChallengeArgs{
				WalletAddress: &address,
				Blockchain:    &blockchain,
			}, ctx)
			connect.AssertEqual(t, challenge.Error, nil)
			return challenge
		}

		// a challenge value the server never issued
		unissued := FormatWalletAuthChallengeMessage("bmV2ZXItaXNzdWVkLWNoYWxsZW5nZQ==", server.NowUtc().Unix())
		result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "tao",
			PublicKey:  wallet.address,
			Message:    unissued,
			Signature:  wallet.sign(unissued),
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Valid, false)
		connect.AssertEqual(t, result.Error.Message, "401 challenge not found")

		// a challenge issued for a different wallet
		boundChallenge := newChallenge()
		result, err = UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "tao",
			PublicKey:  other.address,
			Message:    boundChallenge.MessageTemplate,
			Signature:  other.sign(boundChallenge.MessageTemplate),
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Valid, false)
		connect.AssertEqual(t, result.Error.Message, "400 challenge wallet address mismatch")

		// a challenge issued for solana cannot be spent on tao
		solBlockchain := SOL.String()
		solChallenge := CreateWalletAuthChallenge(WalletAuthChallengeArgs{Blockchain: &solBlockchain}, ctx)
		connect.AssertEqual(t, solChallenge.Error, nil)
		result, err = UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "tao",
			PublicKey:  wallet.address,
			Message:    solChallenge.MessageTemplate,
			Signature:  wallet.sign(solChallenge.MessageTemplate),
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Valid, false)
		connect.AssertEqual(t, result.Error.Message, "400 challenge blockchain mismatch")

		// both spellings of the blockchain identifier spend a challenge
		for _, spelling := range []string{"tao", "TAO", "bittensor", "BITTENSOR"} {
			challenge := newChallenge()
			result, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
				Blockchain: spelling,
				PublicKey:  wallet.address,
				Message:    challenge.MessageTemplate,
				Signature:  wallet.sign(challenge.MessageTemplate),
			}, ctx)
			connect.AssertEqual(t, err, nil)
			if !result.Valid {
				t.Fatalf("%s: %s", spelling, result.Error.Message)
			}

			// and it is single use
			replay, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
				Blockchain: spelling,
				PublicKey:  wallet.address,
				Message:    challenge.MessageTemplate,
				Signature:  wallet.sign(challenge.MessageTemplate),
			}, ctx)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, replay.Valid, false)
			connect.AssertEqual(t, replay.Error.Message, "403 challenge already used")
		}
	})
}

// network_user_auth_wallet holds one wallet per user (PRIMARY KEY (user_id)),
// so adding a second one used to silently replace the first -- discarding the
// only credential that could still sign that user in.
func TestAddWalletAuthDoesNotSilentlyReplaceABoundWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "add-wallet-replace", userId)

		first := newSolanaAcceptanceWalletSigner(t)
		second := newBittensorAcceptanceWalletSigner(t)

		err := addWalletAuth(&AddWalletAuthArgs{
			WalletAuth: signedAcceptanceWalletChallenge(t, ctx, first),
			UserId:     userId,
		}, ctx)
		connect.AssertEqual(t, err, nil)

		err = addWalletAuth(&AddWalletAuthArgs{
			WalletAuth: signedAcceptanceWalletChallenge(t, ctx, second),
			UserId:     userId,
		}, ctx)
		connect.AssertNotEqual(t, err, nil)
		if !strings.Contains(err.Error(), "A different wallet is already linked to this account.") {
			t.Fatalf("second wallet error = %q", err.Error())
		}

		// the first wallet is still the bound one
		walletAuths, err := getWalletAuthsByAddress(ctx, first.address)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletAuths), 1)
		connect.AssertEqual(t, *walletAuths[0].UserId, userId)

		secondAuths, err := getWalletAuthsByAddress(ctx, second.address)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(secondAuths), 0)

		// re-adding the SAME wallet stays idempotent
		err = addWalletAuth(&AddWalletAuthArgs{
			WalletAuth: signedAcceptanceWalletChallenge(t, ctx, first),
			UserId:     userId,
		}, ctx)
		connect.AssertEqual(t, err, nil)
	})
}

// A row written before the blockchain column was canonicalized holds the auth
// type string 'solana'. UNIQUE (wallet_address, blockchain) and a
// blockchain = 'SOL' pre-check are both byte exact, so such a row was
// invisible and the same wallet could bind to a second account -- after which
// login resolved whichever row Postgres happened to return first.
func TestAddWalletAuthSeesLegacyLowercaseBlockchainRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		firstNetworkId := server.NewId()
		firstUserId := server.NewId()
		Testing_CreateNetwork(ctx, firstNetworkId, "legacy-wallet-owner", firstUserId)
		secondNetworkId := server.NewId()
		secondUserId := server.NewId()
		Testing_CreateNetwork(ctx, secondNetworkId, "legacy-wallet-taker", secondUserId)

		signer := newSolanaAcceptanceWalletSigner(t)
		// the legacy writer put AuthTypeSolana ("solana") in the blockchain column
		writeWalletAuthRowForTest(t, ctx, firstUserId, signer.address, AuthTypeSolana)

		err := addWalletAuth(&AddWalletAuthArgs{
			WalletAuth: signedAcceptanceWalletChallenge(t, ctx, signer),
			UserId:     secondUserId,
		}, ctx)
		connect.AssertNotEqual(t, err, nil)
		if !strings.Contains(err.Error(), "already linked to another account") {
			t.Fatalf("legacy row conflict error = %q", err.Error())
		}

		// exactly one binding survives, so login cannot pick arbitrarily
		walletAuths, err := getWalletAuthsByAddress(ctx, signer.address)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletAuths), 1)
		connect.AssertEqual(t, *walletAuths[0].UserId, firstUserId)
	})
}

// The legacy 'solana' label must keep resolving its account for a client
// presenting the canonical 'SOL': the chain filter compares parsed
// blockchains, so canonicalizing the lookup cannot lock those users out.
func TestHandleLoginWalletResolvesLegacyLowercaseBlockchainRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "legacy-wallet-login", userId)

		signer := newSolanaAcceptanceWalletSigner(t)
		writeWalletAuthRowForTest(t, ctx, userId, signer.address, AuthTypeSolana)

		login, err := handleLoginWallet(signedAcceptanceWalletChallenge(t, ctx, signer), ctx)
		connect.AssertEqual(t, err, nil)
		if login.Network == nil || login.Network.ByJwt == "" {
			t.Fatalf("legacy 'solana' row did not resolve its network: %#v", login)
		}
	})
}

// A wallet already bound to another account must come back as the structured
// NetworkCreateResult.Error every other failure in NetworkCreate returns, not
// as a panic escaping server.Tx into the router's generic 500.
//
// Today's writers keep network_user.wallet_address and
// network_user_auth_wallet in step (addWalletAuthInTx mirrors, RemoveAuth
// clears both), and NetworkCreate pre-checks the mirrored column -- so this
// state is only reachable when the two have diverged, which is exactly the
// legacy shape addWalletAuthInTx's own comment describes. Construct it
// directly: a binding with no mirror.
func TestNetworkCreateDuplicateWalletReturnsStructuredError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)

		ownerNetworkId := server.NewId()
		ownerUserId := server.NewId()
		Testing_CreateNetwork(ctx, ownerNetworkId, "wallet-create-owner", ownerUserId)

		signer := newBittensorAcceptanceWalletSigner(t)
		writeWalletAuthRowForTest(t, ctx, ownerUserId, signer.address, TAO.String())

		created, err := NetworkCreate(NetworkCreateArgs{
			NetworkName: "wallet-create-taker",
			Terms:       true,
			WalletAuth:  signedAcceptanceWalletChallenge(t, ctx, signer),
		}, clientSession)
		// the load bearing assertion: a structured result, not a panic that
		// the router's recover turns into a generic 500
		connect.AssertEqual(t, err, nil)
		if created == nil || created.Error == nil {
			t.Fatalf("duplicate wallet network create = %#v", created)
		}
		if !strings.Contains(created.Error.Message, "already linked to another account") {
			t.Fatalf("network create error = %q", created.Error.Message)
		}
		connect.AssertEqual(t, created.Network, nil)
	})
}
