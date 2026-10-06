package model

// Temporary rollout callers own source permission, rate limits and the
// coverage-based transition to Redis-only. The Redis reader never calls this seam.

import (
	"context"
	"fmt"

	"github.com/urnetwork/server"
)

// Reuse the complete projection predicate, including either direction and
// resumable checkpoints. EXISTS stops after one eligible member; this never
// rebuilds Redis from an incomplete source snapshot.
const hasResumableContractForPairSql = `SELECT EXISTS (` + contractHoleMembersSql + `)`

// Bounded source-only existence for an explicitly authorized rollout bridge.
// A caller may impose a shorter budget; SQL errors remain unknown, not false
// evidence. Normal authorization calls ReadContractHole instead of this seam.
func HasResumableContractForPair(ctx context.Context, sourceClientId, destinationClientId server.Id) (found bool, returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, contractHoleSourceTimeout)
	defer cancel()
	if recovered := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, hasResumableContractForPairSql, sourceClientId, destinationClientId, 1).Scan(&found))
		}, server.OptNoRetry())
	}); recovered != nil {
		if err, ok := recovered.(error); ok {
			return false, fmt.Errorf("resumable contract source check failed: %w", err)
		}
		return false, fmt.Errorf("resumable contract source check failed: %v", recovered)
	}
	return found, nil
}
