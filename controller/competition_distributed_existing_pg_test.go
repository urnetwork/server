// Disposable PostgreSQL regressions keep the real published job/slot/source
// guards enabled while testing witness-held exact attachment. Never run live.
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Native status alone is replaced; manifest, frozen baseline, typed witness,
// current supervisor checks and the exact SQL claim transaction remain real.
func competitionExistingPgWitness(t *testing.T, tb testing.TB, fixture *competitionDistributedPgFixture, settings *Settings) (*competitionExistingRunWitness, *queuedJob) {
	t.Helper()
	host := settings.DistributedEvaluationPolicy.Hosts[0]
	connection := competitionExecutorConnection{HostId: host.HostId, AgentConfigSha256: strings.Repeat("d", 64), ExistingControl: &competitionExistingControlConnection{ApprovalSha256: strings.Repeat("e", 64), ResourceBoundarySha256: strings.Repeat("f", 64)}}
	settings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{Settings: defaultCompetitionDistributedCoordinatorSettings(), Hosts: []competitionExecutorConnection{connection}}}
	job, err := fixture.store.existingCompetitionJob(tb.Context(), settings)
	if err != nil || job == nil {
		tb.Fatal("real store did not snapshot exact occupied custody", err)
	}
	manifest, err := competitionExistingManifest(tb.Context(), settings, job, host)
	if err != nil {
		tb.Fatal(err)
	}
	transport := &competitionExistingTestTransport{t: tb, manifest: manifest, connection: connection, firstFence: job.SlotFencingToken, keySeed: 1}
	evaluator := &competitionAgentEvaluator{host: host, transport: transport, now: fixture.clock.Now}
	witness, err := evaluator.observeExisting(tb.Context(), settings, job)
	if err != nil {
		tb.Fatal("real immutable manifest did not mint a fresh synthetic native observation", err)
	}
	tb.Cleanup(func() { clear(witness.manifest.EvaluatorRequest) })
	return witness, job
}

// The same hold which refuses every fresh FIFO claim does not strand a proven
// original native attempt. All independent static host/baseline guards remain.
func TestCompetitionExistingPgClaimHoldAllowsOnlyExactExistingWitness(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionPreparedIntakeCompletionFixture(t, tb, true)
		settings := fixture.selected(0)
		first, err := fixture.store.Claim(tb.Context(), settings, "synthetic-original-worker", testWorkerImageDigest())
		if err != nil || first == nil {
			tb.Fatal(err)
		}
		fixture.clock.Advance(2 * time.Minute)
		witness, snapshot := competitionExistingPgWitness(t, tb, fixture, settings)
		hold := model.CompetitionAutomationHoldArgs{Kind: "evaluation", Active: true, IdempotencyKey: "synthetic-existing-claim-hold", ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic existing-run regression.", EvidenceSha256: strings.Repeat("a", 64)}
		if _, err := fixture.store.SetAutomationHold(tb.Context(), settings, "synthetic-operator", hold); err != nil {
			tb.Fatal(err)
		}
		var gradingReady bool
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(tb.Context(), `SELECT competition_v2_intake_grading_ready($1)`, first.RoundId).Scan(&gradingReady))
		})
		if gradingReady {
			tb.Fatal("real intake grading gate did not refuse the claim-only hold")
		}
		if job, err := fixture.store.Claim(tb.Context(), fixture.selected(1), "synthetic-refused-fresh", testWorkerImageDigest()); job != nil || !errors.Is(err, ErrAutomationHeld) {
			tb.Fatal("claim-only hold admitted fresh work", err)
		}
		selected := *settings
		selected.executorReconcileOnly, selected.executorExistingWitness = true, witness
		attached, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-existing-successor", testWorkerImageDigest())
		if err != nil || attached == nil || attached.JobId != first.JobId || attached.AttemptCount != first.AttemptCount || attached.SlotFencingToken != first.SlotFencingToken+1 ||
			!attached.StartedAt.Equal(*snapshot.StartedAt) || !attached.ExecutionDeadline.Equal(*snapshot.ExecutionDeadline) || attached.WorkerImageDigest != snapshot.WorkerImageDigest {
			tb.Fatal("held typed attachment changed execution or failed its real claim guards", err)
		}
		if _, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-replayed-witness", testWorkerImageDigest()); !errors.Is(err, ErrLeaseLost) {
			tb.Fatal("single-use native witness was replayed", err)
		}
		if err := fixture.store.Heartbeat(withCompetitionJobAuthority(tb.Context(), first), settings, "synthetic-original-worker", first.JobId); !errors.Is(err, ErrLeaseLost) {
			tb.Fatal("obsolete fence renewed attached custody", err)
		}
	})
}

// A valid observation is not a lease: a committed competing reconciliation
// between observation and claim makes the old witness fail the exact SQL CAS.
func TestCompetitionExistingPgStaleCasNeverStealsNewCustody(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		settings := fixture.selected(0)
		first, err := fixture.store.Claim(tb.Context(), settings, "synthetic-first-worker", testWorkerImageDigest())
		if err != nil || first == nil {
			tb.Fatal(err)
		}
		fixture.clock.Advance(2 * time.Minute)
		witness, _ := competitionExistingPgWitness(t, tb, fixture, settings)
		newer, err := fixture.store.Claim(tb.Context(), settings, "synthetic-committed-successor", testWorkerImageDigest())
		if err != nil || newer == nil {
			tb.Fatal(err)
		}
		selected := *settings
		selected.executorReconcileOnly, selected.executorExistingWitness = true, witness
		job, err := fixture.store.Claim(tb.Context(), &selected, "synthetic-stale-observer", testWorkerImageDigest())
		if err != nil && !errors.Is(err, ErrLeaseLost) || job != nil {
			tb.Fatal("stale observation stole current custody", err)
		}
		stored, err := fixture.store.GetJob(tb.Context(), settings, first.JobId, &Principal{Id: "synthetic-operator", Role: "operator"})
		if err != nil || stored.SlotFencingToken != newer.SlotFencingToken || stored.LeaseOwner != newer.LeaseOwner || stored.AttemptCount != first.AttemptCount || !stored.ExecutionDeadline.Equal(*first.ExecutionDeadline) {
			tb.Fatal("failed CAS changed existing execution", err)
		}
	})
}

// An expired observation or manufactured empty capability cannot waive hold
// or any exact original-run check, even when all normal static gates pass.
func TestCompetitionExistingPgExpiredAndArbitraryWitnessFailClosed(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		settings := fixture.selected(0)
		first, err := fixture.store.Claim(tb.Context(), settings, "synthetic-first-worker", testWorkerImageDigest())
		if err != nil || first == nil {
			tb.Fatal(err)
		}
		fixture.clock.Advance(2 * time.Minute)
		witness, _ := competitionExistingPgWitness(t, tb, fixture, settings)
		fixture.clock.Advance(settings.distributedCoordinator.Config.Settings.ExistingWitnessMaximumAge)
		for _, proof := range []*competitionExistingRunWitness{witness, {}} {
			selected := *settings
			selected.executorReconcileOnly, selected.executorExistingWitness = true, proof
			if job, err := fixture.store.Claim(context.Background(), &selected, "synthetic-refused-observer", testWorkerImageDigest()); job != nil || !errors.Is(err, ErrLeaseLost) {
				tb.Fatal("expired/arbitrary native witness gained a lease", err)
			}
		}
	})
}
