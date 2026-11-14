package model

// Explicit control-plane source observations retain the resumable predicate.
// Redis packet authorization never calls this historical rollout seam.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Bounded source-only existence for independent control-plane work.
// A caller may impose a shorter budget; SQL errors remain unknown, not false
// evidence. Normal authorization calls ReadContractHole instead of this seam.
func HasResumableContractForPair(ctx context.Context, sourceClientId, destinationClientId server.Id) (bool, error) {
	status, _, err := ReadResumableContractLease(ctx, sourceClientId, destinationClientId)
	return status == ContractHolePositive, err
}

// LIMIT 1 preserves the indexed existence cost. The first eligible deadline is
// conservative when a later member survives longer; legacy NULL grants only a
// bounded cache lease. This source observation never seeds a partial Redis count.
func ReadResumableContractLease(ctx context.Context, sourceClientId, destinationClientId server.Id) (status ContractHoleStatus, validUntil time.Time, returnErr error) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, contractHoleSourceTimeout)
	defer cancel()
	if recovered := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, contractHoleMembersSql, sourceClientId, destinationClientId, 1)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					status = ContractHoleNegative
					return
				}
				var member contractHoleMember
				server.Raise(rows.Scan(&member.ContractId, &member.ExpirationTime))
				validUntil = started.Add(ContractHoleTtl)
				if member.ExpirationTime != nil && member.ExpirationTime.Before(validUntil) {
					validUntil = *member.ExpirationTime
				}
				status = ContractHolePositive
			})
		}, server.OptNoRetry())
	}); recovered != nil {
		if err, ok := recovered.(error); ok {
			return ContractHoleUnknown, time.Time{}, fmt.Errorf("resumable contract source check failed: %w", err)
		}
		return ContractHoleUnknown, time.Time{}, fmt.Errorf("resumable contract source check failed: %v", recovered)
	}
	if status == ContractHolePositive && !time.Now().Before(validUntil) {
		// Another member may survive a row whose deadline elapsed during I/O.
		return ContractHoleUnknown, time.Time{}, nil
	}
	return status, validUntil, nil
}
