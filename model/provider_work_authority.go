// Independently signed complete rosters and assembled public companions survive
// producer restarts. No database enumeration can create a missing authority.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
)

// One exact roster owns a domain and epoch. Independent signatures permit
// public ingestion but cannot replace a previously retained roster or window.
func RetainProviderWorkAuthority(ctx context.Context, raw []byte, domain, approver [32]byte, signer common.Address) (digest [32]byte, resultErr error) {
	defer providerWorkRecover(&resultErr)
	authority, err := payoutartifact.DecodeWholeWorkAuthority(ctx, raw, signer)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return digest, err
		}
		return digest, errors.Join(ErrProviderWorkInvalid, err)
	}
	actual, err := authority.Domain.Digest()
	if err != nil || actual != domain || authority.RequestPublicKey != approver {
		return digest, errors.Join(ErrProviderWorkInvalid, err)
	}
	digest = sha256.Sum256(raw)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_authority(domain_hash,epoch,authority_hash,original) VALUES($1,$2,$3,$4) ON CONFLICT(domain_hash,epoch) DO NOTHING`, domain[:], strconv.FormatUint(authority.Epoch, 10), digest[:], raw))
		var prior, hash []byte
		rows, err := tx.Query(ctx, `SELECT original,authority_hash FROM provider_work_authority WHERE domain_hash=$1 AND epoch=$2`, domain[:], strconv.FormatUint(authority.Epoch, 10))
		server.WithPgResult(rows, err, func() {
			if !rows.Next() {
				panic(ErrProviderWorkConflict)
			}
			server.Raise(rows.Scan(&prior, &hash))
		})
		if !bytes.Equal(prior, raw) || !bytes.Equal(hash, digest[:]) {
			panic(ErrProviderWorkConflict)
		}
	}, pgx.ReadCommitted)
	return
}

// Every read rechecks the original signature, domain, epoch and content hash.
func GetProviderWorkAuthority(ctx context.Context, domain [32]byte, epoch uint64, signer common.Address) (raw []byte, authority payoutartifact.WholeWorkAuthority, resultErr error) {
	defer providerWorkRecover(&resultErr)
	var digest []byte
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT original,authority_hash FROM provider_work_authority WHERE domain_hash=$1 AND epoch=$2`, domain[:], strconv.FormatUint(epoch, 10))
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&raw, &digest))
			}
		})
	})
	if raw == nil {
		return nil, authority, ErrProviderWorkMissing
	}
	hash := sha256.Sum256(raw)
	if !bytes.Equal(hash[:], digest) {
		return nil, authority, ErrProviderWorkConflict
	}
	authority, err := payoutartifact.DecodeWholeWorkAuthority(ctx, raw, signer)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, authority, err
		}
		return nil, authority, errors.Join(ErrProviderWorkConflict, err)
	}
	actual, err := authority.Domain.Digest()
	if err != nil || actual != domain || authority.Epoch != epoch {
		return nil, authority, errors.Join(ErrProviderWorkConflict, err)
	}
	return raw, authority, nil
}

// The producer supplies a bounded verified companion after signing its exact
// artifact. Retrying publication never rebuilds or overwrites the first bytes.
func RetainProviderWorkWindow(ctx context.Context, artifact, authority [32]byte, raw []byte) (resultErr error) {
	defer providerWorkRecover(&resultErr)
	if artifact == ([32]byte{}) || authority == ([32]byte{}) || len(raw) == 0 || len(raw) > payoutartifact.MaxWholeWorkInventoryBytes {
		return ErrProviderWorkInvalid
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_window(artifact_hash,authority_hash,original) VALUES($1,$2,$3) ON CONFLICT(artifact_hash,authority_hash) DO NOTHING`, artifact[:], authority[:], raw))
		var prior []byte
		rows, err := tx.Query(ctx, `SELECT original FROM provider_work_window WHERE artifact_hash=$1 AND authority_hash=$2`, artifact[:], authority[:])
		server.WithPgResult(rows, err, func() {
			if !rows.Next() {
				panic(ErrProviderWorkConflict)
			}
			server.Raise(rows.Scan(&prior))
		})
		if !bytes.Equal(prior, raw) {
			panic(ErrProviderWorkConflict)
		}
	}, pgx.ReadCommitted)
	return
}

// Exact artifact and authority selectors cannot adopt a newly assembled
// present-day window for an older immutable payout artifact.
func GetProviderWorkWindow(ctx context.Context, artifact, authority [32]byte) (raw []byte, resultErr error) {
	defer providerWorkRecover(&resultErr)
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT original FROM provider_work_window WHERE artifact_hash=$1 AND authority_hash=$2`, artifact[:], authority[:])
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&raw))
			}
		})
	})
	if raw == nil {
		return nil, ErrProviderWorkMissing
	}
	if len(raw) > payoutartifact.MaxWholeWorkInventoryBytes {
		return nil, ErrProviderWorkConflict
	}
	return
}
