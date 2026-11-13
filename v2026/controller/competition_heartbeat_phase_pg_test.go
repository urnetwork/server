// Disposable database fixtures exercise observer wiring at actual store boundaries.
package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A genuine missing authority row must retain the transaction callback phase.
func TestCompetitionHeartbeatPhasePgMissingAuthority(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture, _ := newCompetitionLeasePgFixture(t, ctx, "phase-missing-authority")
		err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-other-worker", server.NewId())
		detail := competitionHeartbeatFailureDetails(err)
		if !errors.Is(err, ErrLeaseLost) || detail.DatabasePhase != server.DbOperationCallback {
			t.Fatalf("real heartbeat lost authority/phase evidence: %+v", detail)
		}
	})
}

// The adjacent host write records its own actual database callback refusal.
func TestCompetitionHeartbeatPhasePgHostWriteRefusal(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture, _ := newCompetitionLeasePgFixture(t, ctx, "phase-host-refusal")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_refuse_host_write() RETURNS trigger AS $body$
			 BEGIN RAISE EXCEPTION 'synthetic host refusal' USING ERRCODE='P0001'; END $body$ LANGUAGE plpgsql;
			 CREATE TRIGGER synthetic_refuse_host_write BEFORE INSERT OR UPDATE ON competition_evaluator_host
			 FOR EACH ROW EXECUTE FUNCTION synthetic_refuse_host_write();`))
		})
		err := fixture.store.RegisterHost(ctx, fixture.settings, HostSelfCheck{HostId: "synthetic-phase-host"})
		detail := competitionHeartbeatFailureDetails(err)
		proof, observed := err.(competitionHostHeartbeatAcquireError)
		if err == nil || detail.DatabasePhase != server.DbOperationCallback || detail.SqlState != "P0001" || !observed ||
			!proof.callbackEntered || proof.read.AcquireSucceeded == 0 || competitionHostHeartbeatUnadmittedDeadline(err) {
			t.Fatalf("real host write lost refusal/phase evidence: %+v", detail)
		}
	})
}

// A stale supervisor remains fenced, with the real lease-query phase retained.
func TestCompetitionHeartbeatPhasePgSupervisorFence(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		fixture := newCompetitionAutomationFixture(t)
		lease := model.CompetitionSupervisorLease{
			Environment:   fixture.settings.AutomationPolicy.Environment,
			CompetitionId: fixture.settings.CompetitionId,
			SeasonId:      fixture.settings.AutomationPolicy.SeasonId,
			OwnerId:       "synthetic-stale-supervisor", FencingToken: 1,
		}
		err := checkCompetitionSupervisorWorkerFence(context.Background(), fixture.store, fixture.settings, lease)
		detail := competitionHeartbeatFailureDetails(err)
		if !errors.Is(err, ErrLeaseLost) || detail.DatabasePhase != server.DbOperationCallback {
			t.Fatalf("real supervisor fence lost authority/phase evidence: %+v", detail)
		}
	})
}
