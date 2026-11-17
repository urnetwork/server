// Parent fencing failures retain evidence without weakening cancellation or cleanup.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Force parent cancellation only after evaluator admission, then explicitly join.
func TestCompetitionHeartbeatParentCausePersistsAfterJoinedCleanup(t *testing.T) {
	privateFailure := &pgconn.PgError{Code: "08006", Message: "synthetic-private-supervisor-detail"}
	for _, cause := range []error{
		competitionDatabasePhaseError{cause: privateFailure, phase: server.DbOperationCallback},
		competitionDatabasePhaseError{cause: errors.Join(privateFailure, context.Canceled), phase: server.DbOperationAcquire},
	} {
		fixture := newCompetitionLeaseRetryWorkerFixture(t)
		parent, cancelCause := context.WithCancelCause(fixture.ctx)
		fixture.ctx = parent
		t.Cleanup(func() { cancelCause(nil) })
		// Only the real parent-stop branch may cancel the candidate here;
		// automatic inheritance must not race its cleanup-release barrier.
		fixture.worker.evaluationContextForTest = func(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.WithoutCancel(parent), deadline)
		}
		fixture.Start(t)
		cancelCause(cause)
		returned := fixture.Finish(t)
		if !errors.Is(returned, context.Canceled) {
			t.Fatal("parent cause changed historical cancellation identity")
		}
		if fixture.store.diagnosticCalls != 1 || fixture.store.handbacks != 0 || fixture.store.reason != "worker_shutdown" || fixture.store.detail != competitionHeartbeatFailureDetails(cause) {
			t.Fatalf("parent failure evidence lost: calls=%d legacy=%d detail=%+v", fixture.store.diagnosticCalls, fixture.store.handbacks, fixture.store.detail)
		}
		if !fixture.store.joined || !fixture.store.cleanupContextLive || fixture.store.completedCount() != 0 || fixture.evaluator.calls != 1 {
			t.Fatal("parent failure bypassed joined cleanup or completed/repeated evaluation")
		}
		encoded, err := json.Marshal(competitionHandBackEvent{Reason: fixture.store.reason, competitionHeartbeatFailure: fixture.store.detail})
		if err != nil || strings.Contains(string(encoded), "synthetic-private") {
			t.Fatal("private parent cause reached serialized evidence")
		}
	}
}
