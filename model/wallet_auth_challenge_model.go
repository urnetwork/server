package model

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

const (
	WalletAuthChallengeLifetime = 5 * time.Minute
	WalletAuthChallengeSkewPast = 1 * time.Minute
	// Use the same lifetime for max future clock skew so a challenge
	// cannot be hoarded beyond its own expiry.
	WalletAuthChallengeSkewFuture = 5 * time.Minute
	WalletAuthChallengeValueBytes = 32
)

type WalletAuthChallengeArgs struct {
	WalletAddress *string `json:"wallet_address,omitempty"`
	Blockchain    *string `json:"blockchain,omitempty"`
}

type WalletAuthChallengeResult struct {
	Challenge       string `json:"challenge"`
	Timestamp       int64  `json:"timestamp"`
	ExpiresIn       int64  `json:"expires_in"`
	MessageTemplate string `json:"message_template"`
	Error           *WalletAuthChallengeResultError `json:"error,omitempty"`
}

type WalletAuthChallengeResultError struct {
	Message string `json:"message"`
}

func CreateWalletAuthChallenge(
	args WalletAuthChallengeArgs,
	ctx context.Context,
) *WalletAuthChallengeResult {
	now := server.NowUtc()
	expire := now.Add(WalletAuthChallengeLifetime)

	challengeBytes := make([]byte, WalletAuthChallengeValueBytes)
	if _, err := rand.Read(challengeBytes); err != nil {
		glog.Errorf("Failed to generate wallet auth challenge: %v", err)
		return &WalletAuthChallengeResult{
			Error: &WalletAuthChallengeResultError{
				Message: "failed to generate challenge",
			},
		}
	}
	challengeValue := base64.URLEncoding.EncodeToString(challengeBytes)

	blockchain := ""
	if args.Blockchain != nil {
		blockchain = strings.ToLower(strings.TrimSpace(*args.Blockchain))
	}
	// default empty/missing blockchain to solana to match existing behavior
	if blockchain == "" {
		blockchain = SOL.String()
	}

	var walletAddress *string
	if args.WalletAddress != nil {
		w := strings.TrimSpace(*args.WalletAddress)
		walletAddress = &w
	}

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO wallet_auth_challenge (
					challenge_id,
					challenge_value,
					wallet_address,
					blockchain,
					create_time,
					expire_time,
					used
				)
				VALUES ($1, $2, $3, $4, $5, $6, false)
			`,
			server.NewId(),
			challengeValue,
			walletAddress,
			blockchain,
			now,
			expire,
		))
	})

	message := FormatWalletAuthChallengeMessage(challengeValue, now.Unix())

	return &WalletAuthChallengeResult{
		Challenge:       challengeValue,
		Timestamp:       now.Unix(),
		ExpiresIn:       int64(WalletAuthChallengeLifetime / time.Second),
		MessageTemplate: message,
	}
}

// FormatWalletAuthChallengeMessage must match the client-side construction
// exactly. Any change here must be reflected in the dashboard hook.
func FormatWalletAuthChallengeMessage(challenge string, timestamp int64) string {
	return fmt.Sprintf("Sign in to URnetwork\nChallenge: %s\nTimestamp: %d", challenge, timestamp)
}

// parseWalletAuthChallengeMessage is the inverse of FormatWalletAuthChallengeMessage.
// It returns the embedded challenge and timestamp so the server can look up the row.
func parseWalletAuthChallengeMessage(message string) (challenge string, timestamp int64, err error) {
	parts := strings.SplitN(message, "\n", 3)
	if len(parts) != 3 ||
		parts[0] != "Sign in to URnetwork" ||
		!strings.HasPrefix(parts[1], "Challenge: ") ||
		!strings.HasPrefix(parts[2], "Timestamp: ") {
		return "", 0, errors.New("invalid message format")
	}
	challenge = strings.TrimPrefix(parts[1], "Challenge: ")
	tsStr := strings.TrimPrefix(parts[2], "Timestamp: ")
	timestamp, err = strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return "", 0, errors.New("invalid timestamp in message")
	}
	return challenge, timestamp, nil
}

type UseWalletAuthChallengeArgs struct {
	Blockchain string
	PublicKey  string
	Message    string
	Signature  string
}

type UseWalletAuthChallengeResult struct {
	Valid bool
	Error *WalletAuthChallengeResultError
}

func UseWalletAuthChallenge(
	args *UseWalletAuthChallengeArgs,
	ctx context.Context,
) (*UseWalletAuthChallengeResult, error) {
	challengeValue, timestamp, err := parseWalletAuthChallengeMessage(args.Message)
	if err != nil {
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: "400 invalid message format"},
		}, nil
	}

	now := server.NowUtc()
	messageTime := time.Unix(timestamp, 0).UTC()

	if messageTime.Before(now.Add(-WalletAuthChallengeSkewPast)) {
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: "400 challenge timestamp too old"},
		}, nil
	}
	if messageTime.After(now.Add(WalletAuthChallengeSkewFuture)) {
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: "400 challenge timestamp too far in the future"},
		}, nil
	}

	blockchain := args.Blockchain
	if blockchain == "" {
		blockchain = SOL.String()
	}

	isValid, err := VerifySignature(blockchain, args.PublicKey, args.Message, args.Signature)
	if err != nil {
		return nil, err
	}
	if !isValid {
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: "401 invalid signature"},
		}, nil
	}

	var used bool
	var expireTime time.Time
	server.Tx(ctx, func(tx server.PgTx) {
		result, dbErr := tx.Query(
			ctx,
			`
				SELECT
					used,
					expire_time
				FROM wallet_auth_challenge
				WHERE challenge_value = $1
				FOR UPDATE
			`,
			challengeValue,
		)
		if dbErr != nil {
			server.Raise(dbErr)
		}
		server.WithPgResult(result, dbErr, func() {
			if !result.Next() {
				err = errors.New("challenge not found")
				return
			}
			server.Raise(result.Scan(&used, &expireTime))
		})

		if err != nil {
			return
		}

		if used {
			err = errors.New("challenge already used")
			return
		}

		if server.NowUtc().After(expireTime) {
			err = errors.New("challenge expired")
			return
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE wallet_auth_challenge
				SET used = true,
					wallet_address = $2,
					blockchain = $3
				WHERE challenge_value = $1
			`,
			challengeValue,
			args.PublicKey,
			blockchain,
		))
	})

	if err != nil {
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: fmt.Sprintf("401 %s", err.Error())},
		}, nil
	}

	return &UseWalletAuthChallengeResult{Valid: true}, nil
}

func RemoveExpiredWalletAuthChallenges(ctx context.Context, minTime time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM wallet_auth_challenge
				WHERE expire_time < $1
			`,
			minTime.UTC(),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM wallet_auth_challenge_attempt
				WHERE attempt_time < $1
			`,
			minTime.UTC(),
		))
	})
}
