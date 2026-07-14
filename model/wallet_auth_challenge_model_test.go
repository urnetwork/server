package model

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/ChainSafe/go-schnorrkel"
	"github.com/gagliardetto/solana-go"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

func TestWalletAuthChallengeCreateAndUse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// Generate a fresh Solana keypair for a real end-to-end signature test.
		privateKey, err := solana.NewRandomPrivateKey()
		connect.AssertEqual(t, err, nil)
		publicKey := privateKey.PublicKey()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, strings.Contains(result.MessageTemplate, result.Challenge), true)

		signature, err := privateKey.Sign([]byte(result.MessageTemplate))
		connect.AssertEqual(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  publicKey.String(),
			Message:    result.MessageTemplate,
			Signature:  signatureB64,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		if useResult.Error != nil {
			t.Logf("UseWalletAuthChallenge error: %s", useResult.Error.Message)
		}
		connect.AssertEqual(t, useResult.Valid, true)

		// replay must fail
		useResult2, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  publicKey.String(),
			Message:    result.MessageTemplate,
			Signature:  signatureB64,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, useResult2.Valid, false)
		connect.AssertEqual(t, useResult2.Error != nil, true)
		connect.AssertEqual(t, strings.HasPrefix(useResult2.Error.Message, "403 "), true)
	})
}

func TestWalletAuthChallengeExpired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		privateKey, err := solana.NewRandomPrivateKey()
		connect.AssertEqual(t, err, nil)
		publicKey := privateKey.PublicKey()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, result.Error, nil)

		// mutate the message to an old timestamp
		oldMessage := FormatWalletAuthChallengeMessage(result.Challenge, result.Timestamp-int64((6*time.Minute)/time.Second))
		signature, err := privateKey.Sign([]byte(oldMessage))
		connect.AssertEqual(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  publicKey.String(),
			Message:    oldMessage,
			Signature:  signatureB64,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, useResult.Valid, false)
	})
}

func TestWalletAuthChallengeFutureTimestamp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		privateKey, err := solana.NewRandomPrivateKey()
		connect.AssertEqual(t, err, nil)
		publicKey := privateKey.PublicKey()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, result.Error, nil)

		futureMessage := FormatWalletAuthChallengeMessage(result.Challenge, result.Timestamp+int64((6*time.Minute)/time.Second))
		signature, err := privateKey.Sign([]byte(futureMessage))
		connect.AssertEqual(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  publicKey.String(),
			Message:    futureMessage,
			Signature:  signatureB64,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, useResult.Valid, false)
	})
}

func TestWalletAuthChallengeTimestampMismatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		privateKey, err := solana.NewRandomPrivateKey()
		connect.AssertEqual(t, err, nil)
		publicKey := privateKey.PublicKey()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, result.Error, nil)

		// mutate only the timestamp by a few seconds; signature is still valid,
		// but it does not match the stored create_time
		mismatchedMessage := FormatWalletAuthChallengeMessage(result.Challenge, result.Timestamp+30)
		signature, err := privateKey.Sign([]byte(mismatchedMessage))
		connect.AssertEqual(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  publicKey.String(),
			Message:    mismatchedMessage,
			Signature:  signatureB64,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		t.Logf("timestamp mismatch result: valid=%v error=%v", useResult.Valid, useResult.Error)
		connect.AssertEqual(t, useResult.Valid, false)
		connect.AssertEqual(t, useResult.Error != nil, true)
		connect.AssertEqual(t, strings.HasPrefix(useResult.Error.Message, "400 "), true)
		connect.AssertEqual(t, strings.Contains(useResult.Error.Message, "timestamp mismatch"), true)
	})
}

func TestWalletAuthChallengeInvalidSignature(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		privateKey, err := solana.NewRandomPrivateKey()
		connect.AssertEqual(t, err, nil)
		publicKey := privateKey.PublicKey()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, result.Error, nil)

		// sign a different message
		otherPrivateKey, err := solana.NewRandomPrivateKey()
		connect.AssertEqual(t, err, nil)
		badSignature, err := otherPrivateKey.Sign([]byte(result.MessageTemplate))
		connect.AssertEqual(t, err, nil)
		badSignatureB64 := base64.StdEncoding.EncodeToString(badSignature[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  publicKey.String(),
			Message:    result.MessageTemplate,
			Signature:  badSignatureB64,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, useResult.Valid, false)
		connect.AssertEqual(t, useResult.Error != nil, true)
		connect.AssertEqual(t, strings.HasPrefix(useResult.Error.Message, "401 "), true)
	})
}

func TestWalletAuthChallengeInvalidPublicKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		privateKey, err := solana.NewRandomPrivateKey()
		connect.AssertEqual(t, err, nil)

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, result.Error, nil)

		signature, err := privateKey.Sign([]byte(result.MessageTemplate))
		connect.AssertEqual(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  "not_a_valid_solana_address",
			Message:    result.MessageTemplate,
			Signature:  signatureB64,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, useResult.Valid, false)
		connect.AssertEqual(t, useResult.Error != nil, true)
		connect.AssertEqual(t, strings.HasPrefix(useResult.Error.Message, "400 "), true)
	})
}

func TestWalletAuthChallengeUnsupportedBlockchain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		privateKey, err := solana.NewRandomPrivateKey()
		connect.AssertEqual(t, err, nil)
		publicKey := privateKey.PublicKey()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		connect.AssertEqual(t, result.Error, nil)

		signature, err := privateKey.Sign([]byte(result.MessageTemplate))
		connect.AssertEqual(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "ethereum",
			PublicKey:  publicKey.String(),
			Message:    result.MessageTemplate,
			Signature:  signatureB64,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, useResult.Valid, false)
		connect.AssertEqual(t, useResult.Error != nil, true)
		connect.AssertEqual(t, strings.HasPrefix(useResult.Error.Message, "400 "), true)
		connect.AssertEqual(t, strings.Contains(useResult.Error.Message, "unsupported blockchain"), true)
	})
}

func TestWalletAuthChallengeCreateInvalidBlockchain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		blockchain := "ethereum"
		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{
			Blockchain: &blockchain,
		}, ctx)
		connect.AssertEqual(t, result.Error != nil, true)
		connect.AssertEqual(t, strings.HasPrefix(result.Error.Message, "400 "), true)
	})
}

func TestWalletAuthChallengeCreateInvalidAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		address := "not_a_valid_solana_address"
		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{
			WalletAddress: &address,
		}, ctx)
		connect.AssertEqual(t, result.Error != nil, true)
		connect.AssertEqual(t, strings.HasPrefix(result.Error.Message, "400 "), true)
	})
}

func TestWalletAuthChallengeBittensorCreateAndUse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		secretKey, publicKey, err := schnorrkel.GenerateKeypair()
		connect.AssertEqual(t, err, nil)
		address := testingSS58Encode(42, publicKey.Encode())

		blockchain := "bittensor"
		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{
			Blockchain: &blockchain,
		}, ctx)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, strings.Contains(result.MessageTemplate, result.Challenge), true)

		wrapped := "<Bytes>" + result.MessageTemplate + "</Bytes>"
		transcript := schnorrkel.NewSigningContext([]byte("substrate"), []byte(wrapped))
		signature, err := secretKey.Sign(transcript)
		connect.AssertEqual(t, err, nil)
		signatureBytes := signature.Encode()
		signatureHex := hex.EncodeToString(signatureBytes[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "bittensor",
			PublicKey:  address,
			Message:    result.MessageTemplate,
			Signature:  signatureHex,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		if useResult.Error != nil {
			t.Logf("UseWalletAuthChallenge error: %s", useResult.Error.Message)
		}
		connect.AssertEqual(t, useResult.Valid, true)

		// replay must fail
		useResult2, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "bittensor",
			PublicKey:  address,
			Message:    result.MessageTemplate,
			Signature:  signatureHex,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, useResult2.Valid, false)
		connect.AssertEqual(t, useResult2.Error != nil, true)
		connect.AssertEqual(t, strings.HasPrefix(useResult2.Error.Message, "403 "), true)
	})
}

func TestWalletAuthChallengeBittensorInvalidAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		blockchain := "bittensor"
		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{
			Blockchain: &blockchain,
		}, ctx)
		connect.AssertEqual(t, result.Error, nil)

		secretKey, _, err := schnorrkel.GenerateKeypair()
		connect.AssertEqual(t, err, nil)
		wrapped := "<Bytes>" + result.MessageTemplate + "</Bytes>"
		transcript := schnorrkel.NewSigningContext([]byte("substrate"), []byte(wrapped))
		signature, err := secretKey.Sign(transcript)
		connect.AssertEqual(t, err, nil)
		signatureBytes := signature.Encode()
		signatureHex := hex.EncodeToString(signatureBytes[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "bittensor",
			PublicKey:  "not-a-valid-ss58-address",
			Message:    result.MessageTemplate,
			Signature:  signatureHex,
		}, ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, useResult.Valid, false)
		connect.AssertEqual(t, useResult.Error != nil, true)
		connect.AssertEqual(t, strings.HasPrefix(useResult.Error.Message, "400 "), true)
	})
}

func TestWalletAuthChallengeCreateBittensorInvalidAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		blockchain := "bittensor"
		address := "not-a-valid-ss58-address"
		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{
			Blockchain:    &blockchain,
			WalletAddress: &address,
		}, ctx)
		connect.AssertEqual(t, result.Error != nil, true)
		connect.AssertEqual(t, strings.HasPrefix(result.Error.Message, "400 "), true)
	})
}

func TestWalletAuthChallengeCreateBittensorValidAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, publicKey, err := schnorrkel.GenerateKeypair()
		connect.AssertEqual(t, err, nil)
		address := testingSS58Encode(42, publicKey.Encode())

		blockchain := "tao"
		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{
			Blockchain:    &blockchain,
			WalletAddress: &address,
		}, ctx)
		connect.AssertEqual(t, result.Error, nil)
	})
}
