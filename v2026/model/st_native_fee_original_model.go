// Complete original proof inputs are retained as immutable digest-keyed chunks
// in the same transaction that grants fee credit or records a contradiction.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/v2026/nativefee"
	"github.com/urnetwork/server/v2026"
)

const stNativeFeeOriginalChunkBytes = 1024 * 1024

// The opaque verifier orders callbacks by digest, then kind. Shared original
// objects therefore acquire their database locks in the same order, even when
// different operator scopes retain one original checkpoint concurrently.
func stNativeFeeRetainOriginal(ctx context.Context, tx server.PgTx, statementHash, kind string, reference nativefee.Reference, reader io.Reader) error {
	var ignored any
	if err := tx.QueryRow(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "st-native-fee-original:"+reference.Sha256).Scan(&ignored); err != nil {
		return err
	}
	var originalBytes int64
	var originalChunks int
	err := tx.QueryRow(ctx, `SELECT byte_count,chunk_count FROM st_operator_native_fee_original_object WHERE original_sha256=$1 FOR UPDATE`, reference.Sha256).Scan(&originalBytes, &originalChunks)
	newObject := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !newObject {
		return err
	}
	buffer := make([]byte, stNativeFeeOriginalChunkBytes)
	digest := sha256.New()
	var total int64
	chunks := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := io.ReadFull(reader, buffer)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		if n != 0 {
			total += int64(n)
			if total > 512*1024*1024 {
				return errors.New("native fee original exceeds the bounded custody stream")
			}
			_, _ = digest.Write(buffer[:n])
			if newObject {
				if chunks == 0 {
					// This uncommitted header is replaced with the full count only
					// after EOF. No partial object survives a read/hash refusal.
					if _, err := tx.Exec(ctx, `INSERT INTO st_operator_native_fee_original_object(original_sha256,byte_count,chunk_count,create_time) VALUES($1,$2,1,$3) ON CONFLICT(original_sha256) DO UPDATE SET original_sha256=EXCLUDED.original_sha256`, reference.Sha256, n, server.NowUtc()); err != nil {
						return err
					}
				}
				if _, err := tx.Exec(ctx, `INSERT INTO st_operator_native_fee_original_chunk(original_sha256,chunk_index,original_bytes) VALUES($1,$2,$3)`, reference.Sha256, chunks, buffer[:n]); err != nil {
					return err
				}
			} else {
				var retained []byte
				if err := tx.QueryRow(ctx, `SELECT original_bytes FROM st_operator_native_fee_original_chunk WHERE original_sha256=$1 AND chunk_index=$2`, reference.Sha256, chunks).Scan(&retained); err != nil {
					return err
				}
				if !bytes.Equal(retained, buffer[:n]) {
					return errors.New("native fee retained original chunk differs from invoked proof")
				}
			}
			chunks++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}
	if chunks == 0 || "sha256:"+hex.EncodeToString(digest.Sum(nil)) != reference.Sha256 {
		return errors.New("native fee original stream is empty or differs from its exact digest")
	}
	if newObject {
		if _, err := tx.Exec(ctx, `UPDATE st_operator_native_fee_original_object SET byte_count=$2,chunk_count=$3 WHERE original_sha256=$1`, reference.Sha256, total, chunks); err != nil {
			return err
		}
	} else if originalBytes != total || originalChunks != chunks {
		return errors.New("native fee retained original byte or chunk census differs")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO st_operator_native_fee_original_reference(statement_sha256,kind,original_path,original_sha256) VALUES($1,$2,$3,$4) ON CONFLICT(statement_sha256,kind) DO NOTHING`, statementHash, kind, reference.Path, reference.Sha256); err != nil {
		return err
	}
	var originalPath, originalHash string
	if err := tx.QueryRow(ctx, `SELECT original_path,original_sha256 FROM st_operator_native_fee_original_reference WHERE statement_sha256=$1 AND kind=$2`, statementHash, kind).Scan(&originalPath, &originalHash); err != nil {
		return err
	}
	if originalPath != reference.Path || originalHash != reference.Sha256 {
		return errors.New("native fee retained original reference changed")
	}
	return ctx.Err()
}

// Reproduce one original without trusting a summary flag. The reader checks
// contiguous chunks, complete length and the exact retained SHA256 while
// streaming; the caller owns and closes its destination and context.
func WriteStNativeFeeOriginal(ctx context.Context, statementHash, kind string, destination io.Writer) (result *nativefee.Reference, resultErr error) {
	defer func() {
		if resultErr != nil {
			result = nil
		}
	}()
	defer recoverStGasError(&resultErr)
	if ctx == nil || destination == nil {
		return nil, errors.New("native fee original reader lacks its lifecycle or destination")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		var reference nativefee.Reference
		var expectedBytes int64
		var expectedChunks int
		server.Raise(tx.QueryRow(ctx, `SELECT reference.original_path,reference.original_sha256,original.byte_count,original.chunk_count FROM st_operator_native_fee_original_reference reference JOIN st_operator_native_fee_original_object original ON original.original_sha256=reference.original_sha256 WHERE reference.statement_sha256=$1 AND reference.kind=$2`, statementHash, kind).Scan(&reference.Path, &reference.Sha256, &expectedBytes, &expectedChunks))
		if expectedBytes <= 0 || expectedBytes > 512*1024*1024 || expectedChunks <= 0 || expectedChunks > 512 {
			panic(errors.New("native fee original reader has an invalid retained census"))
		}
		rows, err := tx.Query(ctx, `SELECT chunk_index,original_bytes FROM st_operator_native_fee_original_chunk WHERE original_sha256=$1 ORDER BY chunk_index`, reference.Sha256)
		server.Raise(err)
		defer rows.Close()
		digest := sha256.New()
		var total int64
		chunks := 0
		for rows.Next() {
			server.Raise(ctx.Err())
			var index int
			var raw []byte
			server.Raise(rows.Scan(&index, &raw))
			if index != chunks || len(raw) == 0 || len(raw) > stNativeFeeOriginalChunkBytes || chunks >= expectedChunks || total+int64(len(raw)) > expectedBytes {
				panic(errors.New("native fee original reader lost contiguous complete chunks"))
			}
			n, err := destination.Write(raw)
			server.Raise(err)
			if n != len(raw) {
				panic(io.ErrShortWrite)
			}
			_, _ = digest.Write(raw)
			total += int64(n)
			chunks++
		}
		server.Raise(rows.Err())
		if chunks != expectedChunks || total != expectedBytes || "sha256:"+hex.EncodeToString(digest.Sum(nil)) != reference.Sha256 {
			panic(errors.New("native fee original reader differs from its retained complete digest"))
		}
		server.Raise(ctx.Err())
		result = &reference
	}, server.OptNoRetry())
	return result, nil
}
