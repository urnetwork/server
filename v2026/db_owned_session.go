// A caller retains this direct session across serialized business transactions.
// Each transaction releases only the exact advisory references it acquired;
// execution and writer-group locks already held by the caller remain untouched.
package server

import (
	"context"
	"errors"
	"slices"
	"time"
)

// The caller owns connection cleanup and must serialize every use of it. An
// uncertain reply prevents further business work, but never closes a session
// whose unrelated live executions have not joined. Err survives such a failure
// even when its transaction committed and its acknowledged posts must run.
type PgOwnedSession struct {
	conn      PgConn
	entered   bool
	uncertain error
}

func NewPgOwnedSession(conn PgConn) *PgOwnedSession {
	return &PgOwnedSession{conn: conn}
}

func (self *PgOwnedSession) Err() error {
	return self.uncertain
}

// Admission stays outside BEGIN with the same finite budget as OwnedTx. Only
// known busy admission repeats; no callback, commit, or uncertain unlock does.
func (self *PgOwnedSession) Tx(ctx context.Context, keys []PgOwnershipKey, callback func(PgTx), options ...any) {
	checkPostgresAllowed(ctx)
	self.txWithResource(ctx, keys, requirePgOwnershipResource(), callback, options...)
}

func (self *PgOwnedSession) txWithResource(ctx context.Context, keys []PgOwnershipKey, resource pgOwnershipResource,
	callback func(PgTx), options ...any) {
	checkPostgresAllowed(ctx)
	if self.entered {
		panic(errors.New("database ownership session is already in use"))
	}
	Raise(self.uncertain)
	if self.conn == nil || self.conn.Conn().IsClosed() || self.conn.Conn().PgConn().TxStatus() != 'I' {
		panic(errors.New("database ownership session is not idle"))
	}
	Raise(resource.validate(self.conn))
	keys = normalizePgOwnershipKeys(keys)
	if len(keys) == 0 {
		panic(errors.New("database ownership requires at least one key"))
	}
	self.entered = true
	defer func() { self.entered = false }()
	observation, _ := ctx.Value(pgOwnershipObservationKey{}).(*PgOwnershipObservation)
	for _, option := range options {
		if value, ok := option.(*PgOwnershipObservation); ok {
			observation = value
		}
	}
	admissionCtx, cancel := context.WithTimeout(ctx, PgOwnershipAdmissionTimeout)
	defer cancel()
	for {
		Raise(admissionCtx.Err())
		owner := &pgOwnedConnection{conn: self.conn, keys: keys, observation: observation,
			backendPid: self.conn.Conn().PgConn().PID()}
		acquired, admitted, err := self.acquire(admissionCtx, owner.backendPid, keys)
		if err != nil {
			// Some unreturned rows may have acquired references. Even a key
			// collision with an inherited group lock cannot be guessed away.
			self.uncertain = err
			self.observeUncertain(owner)
			panic(err)
		}
		owner.releaseScope = func(cleanupParent context.Context) error {
			if owner.discard || self.conn.Conn().IsClosed() || self.conn.Conn().PgConn().TxStatus() != 'I' {
				self.uncertain = errors.New("database ownership scope ended on an uncertain session")
			} else {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(cleanupParent), PgCloseTimeout)
				self.uncertain = self.releaseReferences(cleanupCtx, owner.backendPid, acquired)
				cleanupCancel()
			}
			if owner.admitted {
				kind := PgOwnershipReleased
				if self.uncertain != nil {
					kind = PgOwnershipUncertain
				}
				func() { defer func() { _ = recover() }(); owner.observe(kind) }()
			}
			return self.uncertain
		}
		func() {
			defer owner.release(ctx)
			if !admitted {
				Raise(owner.release(ctx))
				owner.observe(PgOwnershipWaiting)
				return
			}
			owner.admitted = true
			owner.observe(PgOwnershipAdmitted)
			ownedOptions := append(append([]any{}, options...), owner, OptNoRetry())
			Tx(ctx, func(tx PgTx) {
				var backendPid uint32
				Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPid))
				if backendPid != owner.backendPid {
					owner.discard = true
					panic(errors.New("database ownership backend changed before transaction"))
				}
				transaction := tx.(*postCommitPgTx)
				transaction.ownershipAllowed = false
				transaction.ownershipKeys = keys
				callback(tx)
			}, ownedOptions...)
		}()
		if admitted {
			return
		}
		select {
		case <-admissionCtx.Done():
			panic(admissionCtx.Err())
		case <-time.After(pgOwnershipPollPeriod):
		}
	}
}

func (self *PgOwnedSession) observeUncertain(owner *pgOwnedConnection) {
	func() { defer func() { _ = recover() }(); owner.observe(PgOwnershipUncertain) }()
}

// Every successful reply adds exactly one session reference. Track the reply's
// identity rather than relying on row order or unlocking a whole attempted set.
func (self *PgOwnedSession) acquire(ctx context.Context, backendPid uint32, keys []PgOwnershipKey) ([]PgOwnershipKey, bool, error) {
	acquired := []PgOwnershipKey{}
	for offset := 0; offset < len(keys); offset += pgOwnershipQueryLimit {
		page := keys[offset:min(offset+pgOwnershipQueryLimit, len(keys))]
		firsts, seconds := make([]int32, len(page)), make([]int32, len(page))
		for index, key := range page {
			firsts[index], seconds[index] = key.first, key.second
		}
		rows, err := self.conn.Query(ctx, `SELECT pg_backend_pid(),owner.first,owner.second,
			pg_try_advisory_lock(owner.first,owner.second)
			FROM unnest($1::integer[],$2::integer[]) AS owner(first,second)`, firsts, seconds)
		if err != nil {
			return acquired, false, err
		}
		seen := map[PgOwnershipKey]bool{}
		admitted := true
		for rows.Next() {
			var observedPid uint32
			var key PgOwnershipKey
			var got bool
			if err := rows.Scan(&observedPid, &key.first, &key.second, &got); err != nil {
				rows.Close()
				return acquired, false, err
			}
			if observedPid != backendPid || !slices.Contains(page, key) || seen[key] {
				rows.Close()
				return acquired, false, errors.New("database ownership scope reply changed its authority")
			}
			seen[key] = true
			if got {
				acquired = append(acquired, key)
			}
			admitted = admitted && got
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return acquired, false, err
		}
		if len(seen) != len(page) {
			return acquired, false, errors.New("database ownership scope reply count mismatch")
		}
		if !admitted {
			return acquired, false, nil
		}
	}
	return acquired, true, nil
}

// A missing or uncertain unlock never repeats. A second decrement could drop
// an inherited group reference after the first decrement already succeeded.
func (self *PgOwnedSession) releaseReferences(ctx context.Context, backendPid uint32, keys []PgOwnershipKey) error {
	for offset := 0; offset < len(keys); offset += pgOwnershipQueryLimit {
		page := keys[offset:min(offset+pgOwnershipQueryLimit, len(keys))]
		firsts, seconds := make([]int32, len(page)), make([]int32, len(page))
		for index, key := range page {
			firsts[index], seconds[index] = key.first, key.second
		}
		rows, err := self.conn.Query(ctx, `SELECT pg_backend_pid(),owner.first,owner.second,
			pg_advisory_unlock(owner.first,owner.second)
			FROM unnest($1::integer[],$2::integer[]) AS owner(first,second)`, firsts, seconds)
		if err != nil {
			return err
		}
		seen := map[PgOwnershipKey]bool{}
		for rows.Next() {
			var observedPid uint32
			var key PgOwnershipKey
			var unlocked bool
			if err := rows.Scan(&observedPid, &key.first, &key.second, &unlocked); err != nil {
				rows.Close()
				return err
			}
			if observedPid != backendPid || !unlocked || !slices.Contains(page, key) || seen[key] {
				rows.Close()
				return errors.New("database ownership scope unlock changed its authority")
			}
			seen[key] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(seen) != len(page) {
			return errors.New("database ownership scope unlock count mismatch")
		}
	}
	return nil
}
