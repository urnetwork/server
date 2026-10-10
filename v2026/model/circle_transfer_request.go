// One client operation retains one immutable Circle request across processes.
// Concurrent callers may reconcile only the same provider idempotency key.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

var ErrCircleTransferRequestChanged = errors.New("customer transfer request ID already binds different immutable terms")
var ErrCircleTransferObservation = errors.New("customer transfer challenge contradicts retained evidence")
var ErrCircleTransferCapacity = errors.New("customer transfer observation capacity requires review; original request retained")

// No token, key material or provider earning policy belongs in these terms.
type CircleTransferBasis struct {
	CircleUserId server.Id `json:"circle_user_id"`
	WalletId     string    `json:"wallet_id"`
	TokenId      string    `json:"token_id"`
	Blockchain   string    `json:"blockchain"`
	Destination  string    `json:"destination"`
	Amount       NanoCents `json:"amount_nano_cents"`
}

type CircleTransferRequest struct {
	NetworkId       server.Id
	UserId          server.Id
	RequestId       server.Id
	IdempotencyKey  server.Id
	Basis           CircleTransferBasis
	Body            string
	ChallengeId     *string
	ChallengeStatus string
	SubmissionCount int64
	ReviewRequired  bool
}

// USDC has six decimal places. Reject precision loss instead of rounding an
// int64 through float64 or silently sending a different monetary amount.
func CircleTransferAmount(amount NanoCents) (string, error) {
	if amount <= 0 || amount%1000 != 0 {
		return "", fmt.Errorf("USDC transfer requires a positive exact six-decimal amount")
	}
	return fmt.Sprintf("%d.%06d", int64(amount)/1_000_000_000, (int64(amount)%1_000_000_000)/1000), nil
}

func circleTransferRequestBody(basis CircleTransferBasis, key, requestId server.Id) (string, error) {
	amount, err := CircleTransferAmount(basis.Amount)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(struct {
		IdempotencyKey server.Id `json:"idempotencyKey"`
		Amounts        []string  `json:"amounts"`
		Destination    string    `json:"destinationAddress"`
		WalletId       string    `json:"walletId"`
		TokenId        string    `json:"tokenId"`
		FeeLevel       string    `json:"feeLevel"`
		RefId          string    `json:"refId"`
	}{IdempotencyKey: key, Amounts: []string{amount}, Destination: basis.Destination, WalletId: basis.WalletId, TokenId: basis.TokenId, FeeLevel: "LOW", RefId: requestId.String()})
	return string(body), err
}

const circleTransferColumns = `network_id,user_id,request_id,idempotency_key,basis,request_body,challenge_id,challenge_status,submission_count,review_required`

// Database outages preserve ordinary API errors without swallowing programming
// panics or caller cancellation. The exact request remains retained on failure.
func recoverCircleTransferStorage(ctx context.Context, returnErr *error) {
	if value := recover(); value != nil {
		if _, ok := value.(runtime.Error); ok {
			panic(value)
		}
		if err, ok := value.(error); ok {
			*returnErr = errors.Join(err, ctx.Err())
			return
		}
		panic(value)
	}
}

func scanCircleTransfer(row pgx.Row) (*CircleTransferRequest, error) {
	value := &CircleTransferRequest{}
	var basis string
	if err := row.Scan(&value.NetworkId, &value.UserId, &value.RequestId, &value.IdempotencyKey, &basis, &value.Body, &value.ChallengeId, &value.ChallengeStatus, &value.SubmissionCount, &value.ReviewRequired); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(basis), &value.Basis); err != nil {
		return nil, err
	}
	body, err := circleTransferRequestBody(value.Basis, value.IdempotencyKey, value.RequestId)
	if err != nil || body != value.Body {
		return nil, ErrCircleTransferRequestChanged
	}
	return value, nil
}

func GetCircleTransferRequest(ctx context.Context, networkId, userId, requestId server.Id) (value *CircleTransferRequest, err error) {
	defer recoverCircleTransferStorage(ctx, &err)
	if err = ctx.Err(); err != nil {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		value, err = scanCircleTransfer(conn.QueryRow(ctx, `SELECT `+circleTransferColumns+` FROM circle_transfer_request WHERE network_id=$1 AND user_id=$2 AND request_id=$3`, networkId, userId, requestId))
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return
}

// Request creation commits before external I/O. The first writer's key wins;
// a concurrent identical request reuses it rather than replacing it.
func RetainCircleTransferRequest(ctx context.Context, networkId, userId, requestId server.Id, basis CircleTransferBasis) (value *CircleTransferRequest, returnErr error) {
	defer recoverCircleTransferStorage(ctx, &returnErr)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if networkId == (server.Id{}) || userId == (server.Id{}) || requestId == (server.Id{}) || basis.CircleUserId == (server.Id{}) || len(basis.WalletId) == 0 || len(basis.WalletId) > 256 || len(basis.TokenId) == 0 || len(basis.TokenId) > 256 || len(basis.Blockchain) == 0 || len(basis.Blockchain) > 64 || len(basis.Destination) == 0 || len(basis.Destination) > 256 {
		return nil, ErrCircleTransferRequestChanged
	}
	if err := server.RequireCircleTransferSchema(ctx); err != nil {
		return nil, err
	}
	key := server.RequireParseId(uuid.NewString())
	body, err := circleTransferRequestBody(basis, key, requestId)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(basis)
	if err != nil {
		return nil, err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO circle_transfer_request(network_id,user_id,request_id,idempotency_key,basis,request_body) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(network_id,user_id,request_id) DO NOTHING`, networkId, userId, requestId, key, string(encoded), body))
		value, returnErr = scanCircleTransfer(tx.QueryRow(ctx, `SELECT `+circleTransferColumns+` FROM circle_transfer_request WHERE network_id=$1 AND user_id=$2 AND request_id=$3 FOR UPDATE`, networkId, userId, requestId))
		server.Raise(returnErr)
		if value.Basis != basis {
			value = nil
			returnErr = ErrCircleTransferRequestChanged
		}
	}, server.TxReadCommitted)
	return
}

func lockCircleTransfer(ctx context.Context, tx server.PgTx, expected *CircleTransferRequest) (*CircleTransferRequest, error) {
	if expected == nil {
		return nil, ErrCircleTransferRequestChanged
	}
	value, err := scanCircleTransfer(tx.QueryRow(ctx, `SELECT `+circleTransferColumns+` FROM circle_transfer_request WHERE network_id=$1 AND user_id=$2 AND request_id=$3 FOR UPDATE`, expected.NetworkId, expected.UserId, expected.RequestId))
	if err != nil {
		return nil, err
	}
	if value.IdempotencyKey != expected.IdempotencyKey || value.Body != expected.Body || value.Basis != expected.Basis {
		return nil, ErrCircleTransferRequestChanged
	}
	return value, nil
}

// A known challenge is always read rather than submitted again. Unknown
// prior outcomes may recover only through Circle's same-key response contract.
func BeginCircleTransferSubmission(ctx context.Context, expected *CircleTransferRequest) (value *CircleTransferRequest, returnErr error) {
	defer recoverCircleTransferStorage(ctx, &returnErr)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := server.RequireCircleTransferSchema(ctx); err != nil {
		return nil, err
	}
	server.Tx(ctx, func(tx server.PgTx) {
		value, returnErr = lockCircleTransfer(ctx, tx, expected)
		server.Raise(returnErr)
		if value.ReviewRequired {
			returnErr = ErrCircleTransferObservation
			return
		}
		if value.ChallengeId != nil {
			return
		}
		if value.SubmissionCount == math.MaxInt64 {
			returnErr = ErrCircleTransferCapacity
			return
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE circle_transfer_request SET submission_count=submission_count+1 WHERE network_id=$1 AND user_id=$2 AND request_id=$3`, value.NetworkId, value.UserId, value.RequestId))
		value.SubmissionCount++
	}, server.TxReadCommitted)
	return
}

func circleTransferTerminal(status string) bool {
	return status == "COMPLETE" || status == "FAILED" || status == "EXPIRED"
}

// Only normalized public challenge evidence is stored; arbitrary response
// bodies, bearer tokens and session encryption keys never enter this journal.
func ObserveCircleTransfer(ctx context.Context, expected *CircleTransferRequest, challengeId, status, responseDigest string, httpStatus int) (value *CircleTransferRequest, returnErr error) {
	defer recoverCircleTransferStorage(ctx, &returnErr)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := server.RequireCircleTransferSchema(ctx); err != nil {
		return nil, err
	}
	if len(challengeId) > 256 || len(responseDigest) != 64 {
		return nil, ErrCircleTransferObservation
	}
	if _, err := hex.DecodeString(responseDigest); err != nil {
		return nil, err
	}
	if status != "UNKNOWN" && status != "PENDING" && status != "IN_PROGRESS" && !circleTransferTerminal(status) {
		return nil, ErrCircleTransferObservation
	}
	if (challengeId == "") != (status == "UNKNOWN") {
		return nil, ErrCircleTransferObservation
	}
	encoded, err := json.Marshal(struct {
		ChallengeId, Status, ResponseSha256 string
		HttpStatus                          int
	}{ChallengeId: challengeId, Status: status, ResponseSha256: responseDigest, HttpStatus: httpStatus})
	if err != nil {
		return nil, err
	}
	// Keep the first exact response witness for each semantic observation.
	// Changing response timestamps must not consume custody capacity forever.
	semantic, err := json.Marshal(struct {
		ChallengeId, Status string
		HttpStatus          int
	}{ChallengeId: challengeId, Status: status, HttpStatus: httpStatus})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(semantic)
	server.Tx(ctx, func(tx server.PgTx) {
		value, returnErr = lockCircleTransfer(ctx, tx, expected)
		server.Raise(returnErr)
		var count int
		var retained bool
		server.Raise(tx.QueryRow(ctx, `SELECT count(*),coalesce(bool_or(digest=$4),false) FROM circle_transfer_observation WHERE network_id=$1 AND user_id=$2 AND request_id=$3`, value.NetworkId, value.UserId, value.RequestId, hex.EncodeToString(digest[:])).Scan(&count, &retained))
		if count >= 64 && !retained {
			returnErr = ErrCircleTransferCapacity
			return
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO circle_transfer_observation(network_id,user_id,request_id,digest,detail) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, value.NetworkId, value.UserId, value.RequestId, hex.EncodeToString(digest[:]), string(encoded)))
		if challengeId == "" {
			return
		}
		if value.ChallengeId != nil && *value.ChallengeId != challengeId {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE circle_transfer_request SET review_required=true WHERE network_id=$1 AND user_id=$2 AND request_id=$3`, value.NetworkId, value.UserId, value.RequestId))
			returnErr = ErrCircleTransferObservation
			return
		}
		if circleTransferTerminal(value.ChallengeStatus) {
			if circleTransferTerminal(status) && status != value.ChallengeStatus {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE circle_transfer_request SET review_required=true WHERE network_id=$1 AND user_id=$2 AND request_id=$3`, value.NetworkId, value.UserId, value.RequestId))
				returnErr = ErrCircleTransferObservation
			}
			return
		}
		if value.ChallengeStatus == "IN_PROGRESS" && status == "PENDING" {
			return
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE circle_transfer_request SET challenge_id=$4,challenge_status=$5 WHERE network_id=$1 AND user_id=$2 AND request_id=$3`, value.NetworkId, value.UserId, value.RequestId, challengeId, status))
		value.ChallengeId = &challengeId
		value.ChallengeStatus = status
	}, server.TxReadCommitted)
	return
}
