// Completed settlement pages may yield only when every observed failure belongs
// to their stopped operation. The caller still proves deadline and cursor ownership.
package model

import (
	"context"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
)

// A database stop preserves both its historical marker and physical cause.
// Hard siblings, custom matchers and incomplete graphs cannot donate a yield.
// A statement the stop let finish (server.PgStatementStopTimeout) can end in
// its own lock or statement timeout, which rolls its transaction back as the
// stop would have; that refusal joins a stop but cannot yield by itself. Its
// classification is legacySettlementGrantWaitExpired's: any 55P03, and 57014
// only for a statement timeout, never an operator cancel.
func isSettlementPageCancellation(err error) bool {
	causes := server.InspectErrorCauses(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	found := false
	for _, cause := range causes.Nodes {
		if !cause.Leaf {
			continue
		}
		switch cause.Err {
		case context.Canceled, context.DeadlineExceeded, server.DbContextDoneError:
			found = true
		default:
			// the server's refusal at the statement's own lock or statement
			// timeout, read from the observed leaf without custom matching
			pgErr, ok := cause.Err.(*pgconn.PgError)
			if !ok || pgErr.Code != pgerrcode.LockNotAvailable &&
				(pgErr.Code != pgerrcode.QueryCanceled || pgErr.Message != "canceling statement due to statement timeout") {
				return false
			}
		}
	}
	return found
}
