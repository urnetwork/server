package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
)

func TestWalletAuthChallengeCreateAndUse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		assert.Equal(t, result.Error, nil)
		assert.Equal(t, strings.Contains(result.MessageTemplate, result.Challenge), true)

		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s",
			Message:    result.MessageTemplate,
			Signature:  "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw==",
		}, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, useResult.Valid, true)

		// replay must fail
		useResult2, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s",
			Message:    result.MessageTemplate,
			Signature:  "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw==",
		}, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, useResult2.Valid, false)
		assert.Equal(t, useResult2.Error != nil, true)
	})
}

func TestWalletAuthChallengeExpired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		assert.Equal(t, result.Error, nil)

		// mutate the message to an old timestamp
		oldMessage := FormatWalletAuthChallengeMessage(result.Challenge, result.Timestamp-int64((6*time.Minute)/time.Second))
		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s",
			Message:    oldMessage,
			Signature:  "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw==",
		}, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, useResult.Valid, false)
	})
}

func TestWalletAuthChallengeFutureTimestamp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		result := CreateWalletAuthChallenge(WalletAuthChallengeArgs{}, ctx)
		assert.Equal(t, result.Error, nil)

		futureMessage := FormatWalletAuthChallengeMessage(result.Challenge, result.Timestamp+int64((6*time.Minute)/time.Second))
		useResult, err := UseWalletAuthChallenge(&UseWalletAuthChallengeArgs{
			Blockchain: "solana",
			PublicKey:  "6UJtwDRMv2CCfVCKm6hgMDAGrFzv7z8WKEHut2u8dV8s",
			Message:    futureMessage,
			Signature:  "KEpagxVwv1FmPt3KIMdVZz4YsDxgD7J23+f6aafejwdnBy3WJgkE4qteYMwucNoH+9RaPU70YV2Bf+xI+Nd7Cw==",
		}, ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, useResult.Valid, false)
	})
}
