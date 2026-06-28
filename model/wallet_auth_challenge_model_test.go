package model

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/gagliardetto/solana-go"
	"github.com/urnetwork/server"
)

func TestWalletAuthChallengeCreateAndUse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// Generate a fresh Solana keypair for a real end-to-end signature test.
		privateKey, err := solana.NewRandomPrivateKey()
		assert.Equal(t, err, nil)
		publicKey := privateKey.PublicKey()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		assert.Equal(t, result.Error, nil)
		assert.Equal(t, strings.Contains(result.MessageTemplate, result.Challenge), true)

		signature, err := privateKey.Sign([]byte(result.MessageTemplate))
		assert.Equal(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  publicKey.String(),
			Message:    result.MessageTemplate,
			Signature:  signatureB64,
		}, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, useResult.Valid, true)

		// replay must fail
		useResult2, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  publicKey.String(),
			Message:    result.MessageTemplate,
			Signature:  signatureB64,
		}, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, useResult2.Valid, false)
		assert.Equal(t, useResult2.Error != nil, true)
	})
}

func TestWalletAuthChallengeExpired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		privateKey, err := solana.NewRandomPrivateKey()
		assert.Equal(t, err, nil)
		publicKey := privateKey.PublicKey()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		assert.Equal(t, result.Error, nil)

		// mutate the message to an old timestamp
		oldMessage := FormatWalletAuthChallengeMessage(result.Challenge, result.Timestamp-int64((6*time.Minute)/time.Second))
		signature, err := privateKey.Sign([]byte(oldMessage))
		assert.Equal(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  publicKey.String(),
			Message:    oldMessage,
			Signature:  signatureB64,
		}, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, useResult.Valid, false)
	})
}

func TestWalletAuthChallengeFutureTimestamp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		privateKey, err := solana.NewRandomPrivateKey()
		assert.Equal(t, err, nil)
		publicKey := privateKey.PublicKey()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		assert.Equal(t, result.Error, nil)

		futureMessage := FormatWalletAuthChallengeMessage(result.Challenge, result.Timestamp+int64((6*time.Minute)/time.Second))
		signature, err := privateKey.Sign([]byte(futureMessage))
		assert.Equal(t, err, nil)
		signatureB64 := base64.StdEncoding.EncodeToString(signature[:])

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  publicKey.String(),
			Message:    futureMessage,
			Signature:  signatureB64,
		}, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, useResult.Valid, false)
	})
}
