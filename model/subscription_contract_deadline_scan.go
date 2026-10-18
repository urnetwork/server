// Contracts issued with an immutable deadline carry it on their own row, the
// same deadline a queued close copies from it. Deadline closers find due open
// contracts by contract id: an id leads with the millisecond it was minted, so
// id order is creation order, and the outcome-null index serves every read.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
)

// One open contract, its creation time and its stored deadline. Legacy rows
// have no stored deadline.
type OpenContractDeadline struct {
	ContractId     server.Id
	CreateTime     time.Time
	ExpirationTime *time.Time
}

// The deadline of the contract once it has passed at now. A legacy row without
// a stored deadline is never due through its row.
func (self OpenContractDeadline) Due(now time.Time) (ContractDeadline, bool) {
	if self.ExpirationTime == nil || now.Before(*self.ExpirationTime) {
		return ContractDeadline{}, false
	}
	return ContractDeadline{ContractId: self.ContractId, Deadline: *self.ExpirationTime}, true
}

// For a legacy row without a stored deadline, the end of its maximum lifetime
// once it has passed at now: creation plus the default lifetime, the deadline
// the expiry sweep gives a missing one (see contractExpirationDue). A row with
// a stored deadline has none here.
func (self OpenContractDeadline) LegacyDue(now time.Time) (ContractDeadline, bool) {
	if self.ExpirationTime != nil || self.CreateTime.IsZero() {
		return ContractDeadline{}, false
	}
	deadline := self.CreateTime.Add(DefaultContractExpiration)
	if now.Before(deadline) {
		return ContractDeadline{}, false
	}
	return ContractDeadline{ContractId: self.ContractId, Deadline: deadline}, true
}

// The smallest id minted in the millisecond of at. Every id minted before
// that millisecond is smaller, and every id minted in or after it is not.
func ContractIdTimeBound(at time.Time) server.Id {
	ms := uint64(at.UnixMilli())
	var id server.Id
	for index := range 6 {
		id[index] = byte(ms >> (8 * (5 - index)))
	}
	return id
}

// Reads at most limit open contracts with lower < contract_id < upper, newest
// first when descending, through the outcome-null index. Creation time comes
// from the heap tuple the stored deadline is already read from.
func ReadOpenContractDeadlines(ctx context.Context, lower server.Id, upper server.Id, limit int, descending bool) (contracts []OpenContractDeadline, returnErr error) {
	if limit < 1 {
		return nil, errors.New("invalid open contract deadline page")
	}
	order := "ASC"
	if descending {
		order = "DESC"
	}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT contract_id,create_time,expiration_time FROM transfer_contract
				WHERE outcome IS NULL AND contract_id>$1 AND contract_id<$2
				ORDER BY contract_id `+order+` LIMIT $3`, lower, upper, limit)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var contract OpenContractDeadline
					server.Raise(rows.Scan(&contract.ContractId, &contract.CreateTime, &contract.ExpirationTime))
					contracts = append(contracts, contract)
				}
			})
		}, server.OptNoRetry())
	}, func(err error) { contracts, returnErr = nil, err })
	return
}

// The requested contracts that exist without an outcome.
func ReadOpenContractIds(ctx context.Context, contractIds []server.Id) (openIds []server.Id, returnErr error) {
	if len(contractIds) == 0 {
		return nil, nil
	}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NULL`, contractIds)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var contractId server.Id
					server.Raise(rows.Scan(&contractId))
					openIds = append(openIds, contractId)
				}
			})
		}, server.OptNoRetry())
	}, func(err error) { openIds, returnErr = nil, err })
	return
}

// The requested open contracts whose stored deadline has passed at now, each
// with that deadline. It reads only through the caller's query, which may be
// an owner's transaction; the closer rechecks each contract under its lock.
func ReadDueContractExpirationsInConn(ctx context.Context, query server.PgCanQuery, contractIds []server.Id, now time.Time) (deadlines []ContractDeadline) {
	if len(contractIds) == 0 {
		return nil
	}
	rows, err := query.Query(ctx, `SELECT contract_id,expiration_time FROM transfer_contract
		WHERE contract_id=ANY($1) AND outcome IS NULL AND expiration_time<=$2`, contractIds, now.UTC())
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var deadline ContractDeadline
			server.Raise(rows.Scan(&deadline.ContractId, &deadline.Deadline))
			deadlines = append(deadlines, deadline)
		}
	})
	return
}
