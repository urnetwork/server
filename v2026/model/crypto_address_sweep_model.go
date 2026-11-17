package model

// The reads and writes behind the sweep of a per-address coin's issued
// addresses (controller/crypto_pay_sweep.go): how far each address an invoice
// was ever given has been read on its chain (crypto_address_sweep), and where
// the coin's rotation over those addresses stands
// (crypto_address_sweep_position).

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// How far one issued address's incoming transfers have been read. Every
// transfer to the address up to it was recorded.
type CryptoAddressSweepCursor struct {
	Asset   string
	Address string
	// ETH: the last block read through; SOL: the slot of Signature. nil
	// before the first read
	Height *int64
	// SOL: the newest transaction signature examined; "" before the first
	// read and for ETH
	Signature string
	// when a read of the address last completed; nil before the first
	ReadAt *time.Time
	// why the last read failed; "" when it did not
	LastError string
}

// An address an invoice was given, as the sweep reads it: its pool index, its
// invoice and its cursor (nil before its first read).
type CryptoAddressSweepAddress struct {
	Address      string
	AddressIndex int64
	IntentId     server.Id
	Cursor       *CryptoAddressSweepCursor
}

// The coin's addresses that an invoice was ever given, after afterAddressIndex
// in pool index order, at most limit, each with its sweep cursor: every
// invoice, open or closed, credited, voided or past the retention unlink. One
// indexed range read by the invoice address index, so the work of a call is
// bounded by limit however many addresses the coin has issued. Reuse the
// caller's PostgreSQL session; nil selects the outer acquisition boundary.
func ListCryptoAddressesToSweepInConn(
	connOwner server.PgConn,
	ctx context.Context,
	asset string,
	afterAddressIndex int64,
	limit int,
) (addresses []*CryptoAddressSweepAddress) {
	addresses = []*CryptoAddressSweepAddress{}
	if limit <= 0 {
		return
	}
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					intent.address,
					intent.address_index,
					intent.intent_id,
					sweep.address IS NOT NULL,
					sweep.height,
					sweep.signature,
					sweep.read_at,
					sweep.last_error
				FROM crypto_payment_intent intent
				LEFT JOIN crypto_address_sweep sweep
					ON sweep.asset = intent.asset AND sweep.address = intent.address
				WHERE intent.asset = $1
				  AND intent.address_index > $2
				  AND intent.solana_reference IS NULL
				ORDER BY intent.address_index
				LIMIT $3
			`,
			asset,
			afterAddressIndex,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				address := &CryptoAddressSweepAddress{}
				var swept bool
				var height *int64
				var signature *string
				var readAt *time.Time
				var lastError *string
				server.Raise(result.Scan(
					&address.Address,
					&address.AddressIndex,
					&address.IntentId,
					&swept,
					&height,
					&signature,
					&readAt,
					&lastError,
				))
				if swept {
					address.Cursor = &CryptoAddressSweepCursor{
						Asset:   asset,
						Address: address.Address,
						Height:  height,
						ReadAt:  readAt,
					}
					if signature != nil {
						address.Cursor.Signature = *signature
					}
					if lastError != nil {
						address.Cursor.LastError = *lastError
					}
				}
				addresses = append(addresses, address)
			}
		})
	})
	return
}

// Writes one address's cursor as given.
// Reuse the caller's PostgreSQL session; nil selects the outer acquisition
// boundary.
func SetCryptoAddressSweepCursorInConn(
	connOwner server.PgConn,
	ctx context.Context,
	cursor *CryptoAddressSweepCursor,
	now time.Time,
) {
	// a failed statement raises, which ends the transaction at once
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO crypto_address_sweep
				(asset, address, height, signature, read_at, last_error, update_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (asset, address) DO UPDATE
				SET height = $3, signature = $4, read_at = $5, last_error = $6, update_time = $7
			`,
			cursor.Asset,
			cursor.Address,
			cursor.Height,
			nullIfEmptyString(cursor.Signature),
			cursor.ReadAt,
			nullIfEmptyString(cursor.LastError),
			now,
		))
	}, server.TxReadCommitted)
}

// The pool index the coin's sweep rotation last passed, -1 at the start of a
// rotation (and before the first). Reuse the caller's PostgreSQL session; nil
// selects the outer acquisition boundary.
func GetCryptoAddressSweepPositionInConn(
	connOwner server.PgConn,
	ctx context.Context,
	asset string,
) (afterAddressIndex int64) {
	afterAddressIndex = -1
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT after_address_index FROM crypto_address_sweep_position WHERE asset = $1`,
			asset,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&afterAddressIndex))
			}
		})
	})
	return
}

// Moves the coin's sweep rotation to after afterAddressIndex (-1: the start of
// the next rotation). Reuse the caller's PostgreSQL session; nil selects the
// outer acquisition boundary.
func SetCryptoAddressSweepPositionInConn(
	connOwner server.PgConn,
	ctx context.Context,
	asset string,
	afterAddressIndex int64,
	now time.Time,
) {
	// a failed statement raises, which ends the transaction at once
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO crypto_address_sweep_position (asset, after_address_index, update_time)
				VALUES ($1, $2, $3)
				ON CONFLICT (asset) DO UPDATE
				SET after_address_index = $2, update_time = $3
			`,
			asset,
			afterAddressIndex,
			now,
		))
	}, server.TxReadCommitted)
}
