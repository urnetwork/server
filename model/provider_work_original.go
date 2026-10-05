// Public transport preserves signed SDK originals in append-only custody. The
// independent request key, domain and complete owner roster come from callers.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
)

var ErrProviderWorkInvalid = errors.New("provider work original is invalid")
var ErrProviderWorkConflict = errors.New("provider work original conflicts with retained custody")
var ErrProviderWorkMissing = errors.New("provider work original is not retained")
var ErrProviderWorkCapacity = errors.New("provider work outstanding request capacity exhausted")

// Exact polling identity never selects a different SDK generation or key.
type ProviderWorkOwner struct {
	DomainHash [32]byte
	ClientId   [16]byte
	Generation [16]byte
	PublicKey  [32]byte
}

// Both original signatures survive SQL custody and remain independently useful.
type ProviderWorkOriginalPair struct {
	Request []byte
	Cut     []byte
}

// Database callbacks raise errors. Preserve their actual identity at this API
// boundary; unexpected non-error panics retain their ordinary process diagnosis.
func providerWorkRecover(resultErr *error) {
	if cause := recover(); cause != nil {
		if err, ok := cause.(error); ok {
			*resultErr = errors.Join(*resultErr, err)
		} else {
			panic(cause)
		}
	}
}

// Admission serializes one SDK identity under ReadCommitted so a competing
// commit is visible after the lock. Exact retries remain valid after expiry.
func RetainProviderWorkRequest(ctx context.Context, raw []byte, approver, domain [32]byte, now time.Time) (digest [32]byte, resultErr error) {
	defer providerWorkRecover(&resultErr)
	request, err := protocol.DecodeOriginalWorkRequest(raw, approver)
	if err != nil || request.DomainHash != domain || domain == ([32]byte{}) {
		return digest, errors.Join(ErrProviderWorkInvalid, err)
	}
	digest = sha256.Sum256(raw)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, fmt.Sprintf("provider-work/%x/%x/%x", domain, request.ClientId, request.Generation)))
		var prior []byte
		rows, err := tx.Query(ctx, `SELECT original FROM provider_work_request WHERE request_hash=$1 OR request_id=$2 OR (domain_hash=$3 AND client_id=$4 AND generation=$5 AND epoch=$6 AND kind=$7)`, digest[:], request.RequestId[:], domain[:], request.ClientId[:], request.Generation[:], strconv.FormatUint(request.Epoch, 10), request.Kind)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				server.Raise(rows.Scan(&prior))
				if !bytes.Equal(prior, raw) {
					panic(ErrProviderWorkConflict)
				}
			}
		})
		if prior != nil {
			return
		}
		if now.Unix() < request.IssuedAtUnix || now.Unix() >= request.ExpiresAtUnix {
			panic(ErrProviderWorkInvalid)
		}
		var count int
		rows, err = tx.Query(ctx, `SELECT count(*) FROM provider_work_request r WHERE domain_hash=$1 AND client_id=$2 AND generation=$3 AND public_key=$4 AND expires_at>$5 AND NOT EXISTS(SELECT 1 FROM provider_work_cut c WHERE c.request_hash=r.request_hash)`, domain[:], request.ClientId[:], request.Generation[:], request.PublicKey[:], now.Unix())
		server.WithPgResult(rows, err, func() {
			if !rows.Next() {
				panic(ErrProviderWorkConflict)
			}
			server.Raise(rows.Scan(&count))
		})
		if count >= protocol.MaximumOriginalWorkRequests {
			panic(ErrProviderWorkCapacity)
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_request(request_hash,request_id,domain_hash,client_id,generation,public_key,epoch,kind,issued_at,expires_at,original) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, digest[:], request.RequestId[:], domain[:], request.ClientId[:], request.Generation[:], request.PublicKey[:], strconv.FormatUint(request.Epoch, 10), request.Kind, request.IssuedAtUnix, request.ExpiresAtUnix, raw))
	}, pgx.ReadCommitted)
	return
}

// A signed cut cannot self-enroll a request. Commit uncertainty is reconciled
// through the same immutable request row and byte-for-byte receipt on retry.
func RetainProviderWorkCut(ctx context.Context, submission protocol.OriginalWorkCutSubmission, approver, domain [32]byte) (receipt protocol.OriginalWorkCutReceipt, resultErr error) {
	defer providerWorkRecover(&resultErr)
	receipt, err := protocol.VerifyOriginalWorkSubmission(ctx, submission, approver)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return receipt, err
		}
		return receipt, errors.Join(ErrProviderWorkInvalid, err)
	}
	request, err := protocol.DecodeOriginalWorkRequest(submission.Request, approver)
	if err != nil || request.DomainHash != domain || domain == ([32]byte{}) {
		return receipt, errors.Join(ErrProviderWorkInvalid, err)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		var retained []byte
		rows, err := tx.Query(ctx, `SELECT original FROM provider_work_request WHERE request_hash=$1 FOR UPDATE`, receipt.RequestHash[:])
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&retained))
			}
		})
		if retained == nil {
			panic(ErrProviderWorkMissing)
		}
		if !bytes.Equal(retained, submission.Request) {
			panic(ErrProviderWorkConflict)
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_cut(request_hash,cut_hash,original) VALUES($1,$2,$3) ON CONFLICT(request_hash) DO NOTHING`, receipt.RequestHash[:], receipt.CutHash[:], submission.Cut))
		var digest []byte
		rows, err = tx.Query(ctx, `SELECT cut_hash,original FROM provider_work_cut WHERE request_hash=$1`, receipt.RequestHash[:])
		server.WithPgResult(rows, err, func() {
			if !rows.Next() {
				panic(ErrProviderWorkConflict)
			}
			server.Raise(rows.Scan(&digest, &retained))
		})
		if !bytes.Equal(digest, receipt.CutHash[:]) || !bytes.Equal(retained, submission.Cut) {
			panic(ErrProviderWorkConflict)
		}
	}, pgx.ReadCommitted)
	return
}

// Polling returns every outstanding live request for this exact identity, or a
// capacity refusal. It cannot silently truncate an independently signed request.
func ListProviderWorkRequests(ctx context.Context, owner ProviderWorkOwner, approver [32]byte, now time.Time) (result protocol.OriginalWorkRequests, resultErr error) {
	defer providerWorkRecover(&resultErr)
	result = protocol.OriginalWorkRequests{Schema: protocol.OriginalWorkRequestsSchema, Requests: make([][]byte, 0)}
	if owner.DomainHash == ([32]byte{}) || owner.ClientId == ([16]byte{}) || owner.Generation == ([16]byte{}) || owner.PublicKey == ([32]byte{}) {
		return result, ErrProviderWorkInvalid
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT original FROM provider_work_request r WHERE domain_hash=$1 AND client_id=$2 AND generation=$3 AND public_key=$4 AND issued_at<=$5 AND expires_at>$5 AND NOT EXISTS(SELECT 1 FROM provider_work_cut c WHERE c.request_hash=r.request_hash) ORDER BY issued_at,request_hash LIMIT $6`, owner.DomainHash[:], owner.ClientId[:], owner.Generation[:], owner.PublicKey[:], now.Unix(), protocol.MaximumOriginalWorkRequests+1)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var raw []byte
				server.Raise(rows.Scan(&raw))
				request, err := protocol.DecodeOriginalWorkRequest(raw, approver)
				if err != nil {
					panic(errors.Join(ErrProviderWorkConflict, err))
				}
				if request.DomainHash != owner.DomainHash || request.ClientId != owner.ClientId || request.Generation != owner.Generation || request.PublicKey != owner.PublicKey {
					panic(ErrProviderWorkConflict)
				}
				result.Requests = append(result.Requests, raw)
			}
		})
	})
	if len(result.Requests) > protocol.MaximumOriginalWorkRequests {
		return protocol.OriginalWorkRequests{}, ErrProviderWorkCapacity
	}
	return
}

// Content-addressed public retrieval makes no signer-selection decision. Each
// consumer still supplies its own expected signer when validating these bytes.
func GetProviderWorkOriginal(ctx context.Context, digest [32]byte, cut bool) (raw []byte, resultErr error) {
	defer providerWorkRecover(&resultErr)
	if digest == ([32]byte{}) {
		return nil, ErrProviderWorkInvalid
	}
	query := `SELECT original FROM provider_work_request WHERE request_hash=$1`
	if cut {
		query = `SELECT original FROM provider_work_cut WHERE cut_hash=$1 LIMIT 1`
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, query, digest[:])
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&raw))
			}
		})
	})
	if raw == nil {
		return nil, ErrProviderWorkMissing
	}
	if sha256.Sum256(raw) != digest {
		return nil, ErrProviderWorkConflict
	}
	return
}

// The caller supplies the complete signed roster. This lookup joins just its
// exact boundary and does not claim all owners can be enumerated from SQL.
func GetProviderWorkBoundary(ctx context.Context, owner ProviderWorkOwner, epoch uint64, kind string, approver [32]byte) (pair *ProviderWorkOriginalPair, resultErr error) {
	defer providerWorkRecover(&resultErr)
	if kind != "start" && kind != "end" {
		return nil, ErrProviderWorkInvalid
	}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT r.original,c.original FROM provider_work_request r JOIN provider_work_cut c ON c.request_hash=r.request_hash WHERE r.domain_hash=$1 AND r.client_id=$2 AND r.generation=$3 AND r.public_key=$4 AND r.epoch=$5 AND r.kind=$6`, owner.DomainHash[:], owner.ClientId[:], owner.Generation[:], owner.PublicKey[:], strconv.FormatUint(epoch, 10), kind)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				pair = &ProviderWorkOriginalPair{}
				server.Raise(rows.Scan(&pair.Request, &pair.Cut))
			}
		})
	})
	if pair == nil {
		return nil, ErrProviderWorkMissing
	}
	request, err := protocol.DecodeOriginalWorkRequest(pair.Request, approver)
	if err != nil {
		return nil, errors.Join(ErrProviderWorkConflict, err)
	}
	if request.DomainHash != owner.DomainHash || request.ClientId != owner.ClientId || request.Generation != owner.Generation || request.PublicKey != owner.PublicKey || request.Epoch != epoch || request.Kind != kind {
		return nil, ErrProviderWorkConflict
	}
	if _, err := protocol.VerifyOriginalWorkSubmission(ctx, protocol.OriginalWorkCutSubmission{Request: pair.Request, Cut: pair.Cut}, approver); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, errors.Join(ErrProviderWorkConflict, err)
	}
	return
}
