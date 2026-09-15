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

	"github.com/gagliardetto/solana-go"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

const (
	WalletAuthChallengeLifetime = 5 * time.Minute
	// The signed timestamp is the one the SERVER issued (UseWalletAuthChallenge
	// requires it to equal the stored create_time exactly), so there is no
	// client clock in this protocol and this skew cannot absorb one. It only
	// pads the wall-clock sanity check either side of the challenge lifetime,
	// which is the real gate.
	WalletAuthChallengeSkewPast = 1 * time.Minute
	// Same reasoning in the other direction: there is no client clock to drift,
	// so this only tolerates the server's own time moving backwards (an NTP
	// step between issuing a challenge and verifying it) rather than a
	// client-supplied timestamp.
	WalletAuthChallengeSkewFuture = 1 * time.Minute
	WalletAuthChallengeValueBytes = 32
)

type WalletAuthChallengeArgs struct {
	WalletAddress *string `json:"wallet_address,omitempty"`
	Blockchain    *string `json:"blockchain,omitempty"`
}

// WalletAuthChallengeResult carries everything a wallet needs to sign in.
//
// MessageTemplate is the complete signable payload, not a template with holes:
// the challenge and the timestamp are already interpolated into it, and the
// client must hand those exact bytes to the wallet and submit them back
// unmodified as `wallet_message`. Challenge and Timestamp are the same two
// values broken out, for clients that want to display or check them; a client
// must never rebuild the message from them, because any difference in
// spacing, line endings or ordering fails `400 invalid message format`.
//
// There is no scannable payload here, and deliberately so. A WalletConnect
// pairing QR encodes a relay topic and a symmetric key -- it structurally
// cannot carry an application payload, so the challenge and the timestamp
// reach the wallet AFTER pairing, as the polkadot_signMessage (or Solana
// signMessage) request the client builds from MessageTemplate. Putting a live
// challenge value in a scannable URL would also publish it outside the TLS
// session it was issued in. If a login QR is ever wanted, it must encode a
// pairing or hand-off reference, never this payload.
type WalletAuthChallengeResult struct {
	Challenge       string                          `json:"challenge"`
	Timestamp       int64                           `json:"timestamp"`
	ExpiresIn       int64                           `json:"expires_in"`
	MessageTemplate string                          `json:"message_template"`
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

	blockchainStr := ""
	if args.Blockchain != nil {
		blockchainStr = strings.TrimSpace(*args.Blockchain)
	}
	if blockchainStr == "" {
		blockchainStr = SOL.String()
	}
	// Wallet authentication supports Solana and Bittensor (TAO); other
	// chains are not yet supported for wallet auth challenges.
	parsedBlockchain, err := ParseBlockchain(blockchainStr)
	if err != nil || (parsedBlockchain != SOL && parsedBlockchain != TAO) {
		return &WalletAuthChallengeResult{
			Error: &WalletAuthChallengeResultError{
				Message: "400 unsupported blockchain for wallet authentication",
			},
		}
	}
	blockchain := parsedBlockchain.String()

	var walletAddress *string
	if args.WalletAddress != nil {
		w := strings.TrimSpace(*args.WalletAddress)
		if w != "" {
			// Validate early for clear 400; duplicated inside VerifySignature chain verifiers.
			validAddress := false
			switch parsedBlockchain {
			case SOL:
				_, addrErr := solana.PublicKeyFromBase58(w)
				validAddress = addrErr == nil
			case TAO:
				validAddress = IsValidBittensorAddress(w)
			}
			if !validAddress {
				return &WalletAuthChallengeResult{
					Error: &WalletAuthChallengeResultError{
						Message: "400 invalid wallet address",
					},
				}
			}
			walletAddress = &w
		}
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

// FormatWalletAuthChallengeMessage builds the exact text the wallet signs:
// three LF-separated lines carrying the challenge and the server timestamp.
// The server returns the finished string as WalletAuthChallengeResult
// .MessageTemplate, so a client never needs to build it -- but any client
// that does (the dashboard hook, the apps' wallet sheets, the SDK) must match
// this byte for byte, because parseWalletAuthChallengeMessage is its strict
// inverse and rejects anything else with `400 invalid message format`.
// Changing this format is a breaking wire change for every signer.
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
	blockchainStr := strings.TrimSpace(args.Blockchain)
	if blockchainStr == "" {
		blockchainStr = SOL.String()
	}
	parsedBlockchain, err := ParseBlockchain(blockchainStr)
	if err != nil || (parsedBlockchain != SOL && parsedBlockchain != TAO) {
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: "400 unsupported blockchain for wallet authentication"},
		}, nil
	}
	blockchain := parsedBlockchain.String()

	// Validate early for clear 400; duplicated inside VerifySignature chain verifiers.
	validAddress := false
	switch parsedBlockchain {
	case SOL:
		_, addrErr := solana.PublicKeyFromBase58(args.PublicKey)
		validAddress = addrErr == nil
	case TAO:
		validAddress = IsValidBittensorAddress(args.PublicKey)
	}
	if !validAddress {
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: "400 invalid wallet address"},
		}, nil
	}

	challengeValue, timestamp, err := parseWalletAuthChallengeMessage(args.Message)
	if err != nil {
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: "400 invalid message format"},
		}, nil
	}

	now := server.NowUtc()
	messageTime := time.Unix(timestamp, 0).UTC()

	// Bound this by the advertised lifetime, not by the skew alone. The signed
	// timestamp is always the issuance time (it must equal create_time exactly
	// below), so comparing it against now - skew silently turned the
	// 300 second challenge the api documents and returns as `expires_in` into
	// a ~60 second one, and rejected here -- before signature verification and
	// before the row is even read. A WalletConnect round trip to a phone
	// routinely takes longer than that, which made the failure look like a
	// signing problem. expire_time (checked once the row is loaded) is the
	// real deadline; this stays a cheap pre-database sanity bound.
	if messageTime.Before(now.Add(-(WalletAuthChallengeLifetime + WalletAuthChallengeSkewPast))) {
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

	isValid, err := VerifySignature(blockchain, args.PublicKey, args.Message, args.Signature)
	if err != nil {
		// A verifier error means the client sent something undecodable, not
		// that the server failed. Returning it here reached the router as an
		// unprefixed error and became a 500 carrying the raw text, while the
		// same malformed input on the Solana path answered a clean 401 --
		// the asymmetry that makes a Bittensor client look server-broken.
		glog.Infof(
			"Wallet challenge signature verification failed: blockchain=%s err=%s",
			blockchain,
			err.Error(),
		)
		message := "401 invalid signature"
		if errors.Is(err, ErrWalletSignatureEncoding) {
			message = "400 invalid signature encoding"
		}
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: message},
		}, nil
	}
	if !isValid {
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: "401 invalid signature"},
		}, nil
	}

	var used bool
	var expireTime time.Time
	var createTime time.Time
	var issuedWalletAddress *string
	var issuedBlockchain string
	server.Tx(ctx, func(tx server.PgTx) {
		result, dbErr := tx.Query(
			ctx,
			`
				SELECT
					used,
					expire_time,
					create_time,
					wallet_address,
					blockchain
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
			server.Raise(result.Scan(&used, &expireTime, &createTime, &issuedWalletAddress, &issuedBlockchain))
		})

		if err != nil {
			return
		}
		if issuedBlockchain != blockchain {
			err = errors.New("challenge blockchain mismatch")
			return
		}
		if issuedWalletAddress != nil && *issuedWalletAddress != args.PublicKey {
			err = errors.New("challenge wallet address mismatch")
			return
		}

		// The message must be the exact one issued for this challenge.
		if messageTime.Unix() != createTime.Unix() {
			err = errors.New("challenge timestamp mismatch")
			return
		}
		// Then allow small clock skew relative to challenge creation time.
		// Unreachable while the equality check above stands -- it already
		// forced a sub-second delta, and these bounds are a minute wide.
		// Kept as the backstop that would matter if that equality were ever
		// relaxed to a tolerance.
		if messageTime.Before(createTime.Add(-WalletAuthChallengeSkewPast)) ||
			messageTime.After(createTime.Add(WalletAuthChallengeSkewFuture)) {
			err = errors.New("challenge timestamp outside allowed skew")
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
		code := "401"
		switch err.Error() {
		case "challenge already used", "challenge expired":
			code = "403"
		case "challenge timestamp mismatch", "challenge timestamp outside allowed skew", "challenge blockchain mismatch", "challenge wallet address mismatch":
			code = "400"
		}
		return &UseWalletAuthChallengeResult{
			Valid: false,
			Error: &WalletAuthChallengeResultError{Message: fmt.Sprintf("%s %s", code, err.Error())},
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
