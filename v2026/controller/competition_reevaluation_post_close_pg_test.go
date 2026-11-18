// Disposable stores exercise the actual operator transaction after the round
// closes, including its durable hold guards and immutable admission history.
package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The observation is inside the actual migrated fixture and owns no resource
// across the caller's store operation. A missed external poll is not evidence.
func requireCompetitionPostCloseFixtureSchema(t testing.TB, ctx context.Context) {
	t.Helper()
	var actual int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(MAX(end_version_number),0) FROM migration_audit WHERE status='success'`).Scan(&actual))
	})
	if actual != server.MigrationCount() {
		t.Fatalf("post-close fixture schema=%d want=%d", actual, server.MigrationCount())
	}
	t.Logf("post_close_fixture_schema_head=%d", actual)
}

// Existing fixtures genuinely fail and retain inventory evidence; only the
// clock and the separately digested close approval change for these controls.
func competitionPostCloseReevaluationRequest(t testing.TB, fixture *competitionReevaluationAuthorizationPgFixture) {
	t.Helper()
	fixture.now = fixture.round.ClosesAt.Add(time.Minute)
	a := &fixture.request.Authorization
	a.AuthorizedAt, a.ExpiresAt = fixture.now, fixture.now.Add(time.Hour)
	a.PostCloseApproval = &model.CompetitionJobReevaluationPostCloseApproval{Schema: 1,
		ClosesAt: fixture.round.ClosesAt, ExpiresAt: a.ExpiresAt, EvidenceSha256: strings.Repeat("b", 64)}
	var err error
	fixture.request.AuthorizationSha256, err = model.CompetitionAutomationDigest(*a)
	if err != nil {
		t.Fatal(err)
	}
}

// The public grant records a new execution cycle, never a new admission or a
// changed result. Exact replay after expiry remains receipt-only.
func TestCompetitionReevaluationPostClosePgPreservesOriginalOrderAndEvidence(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		requireCompetitionPostCloseFixtureSchema(t, ctx)
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "post-close-preserve")
		competitionPostCloseReevaluationRequest(t, fixture)
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, fixture.principal.Id,
			competitionFinalizationHoldArgs("synthetic-post-close-finalization")); err != nil {
			t.Fatal(err)
		}
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || receipt == nil || receipt.Cycle != 1 || receipt.StartedAt != nil {
			t.Fatalf("bound post-close approval refused or started execution: receipt=%+v err=%v", receipt, err)
		}
		after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		requireCompetitionReevaluationOriginalEvidence(t, before, after)
		fixture.now = fixture.request.Authorization.ExpiresAt.Add(time.Minute)
		replayed, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request)
		if err != nil || !reflect.DeepEqual(receipt, replayed) {
			t.Fatalf("expired exact replay granted or lost authority: %v", err)
		}
		if replay := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(replay, after) {
			t.Fatal("receipt replay mutated queue, admission or prior failure history")
		}
	})
}

// A new post-close grant is unavailable without both distinct control holds;
// the old open-round grant cannot be reused by merely moving its clock.
func TestCompetitionReevaluationPostClosePgRejectsUnheldOrUnapprovedGrant(t *testing.T) {
	runCompetitionReevaluationPgTest(t, func(t testing.TB, ctx context.Context) {
		requireCompetitionPostCloseFixtureSchema(t, ctx)
		fixture := newCompetitionReevaluationAuthorizationPgFixture(t, ctx, "post-close-hold")
		competitionPostCloseReevaluationRequest(t, fixture)
		before := readCompetitionReevaluationPgSnapshot(ctx, fixture.job)
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, fixture.request); !errors.Is(err, ErrAutomationHeld) || receipt != nil {
			t.Fatalf("missing finalization hold granted recovery: %+v %v", receipt, err)
		}
		unapproved := fixture.request
		unapproved.Authorization.PostCloseApproval = nil
		var err error
		unapproved.AuthorizationSha256, err = model.CompetitionAutomationDigest(unapproved.Authorization)
		if err != nil {
			t.Fatal(err)
		}
		if receipt, err := fixture.store.AuthorizeJobReevaluation(ctx, fixture.settings, fixture.principal, unapproved); err == nil || receipt != nil {
			t.Fatal("old open-round authorization silently became post-close authority")
		}
		if after := readCompetitionReevaluationPgSnapshot(ctx, fixture.job); !reflect.DeepEqual(after, before) {
			t.Fatal("rejected post-close grant changed persisted state")
		}
	})
}
