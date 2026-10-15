// Completed settlement pages may yield only when every observed failure belongs
// to their stopped operation. The caller still proves deadline and cursor ownership.
package model

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

// A database stop preserves both its historical marker and physical cause.
// Hard siblings, custom matchers and incomplete graphs cannot donate a yield.
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
			return false
		}
	}
	return found
}
