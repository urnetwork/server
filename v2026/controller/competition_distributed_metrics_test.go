// Executor telemetry follows the exact current automation scope and lifecycle.
// Historical policy never substitutes for an active singleton or missing round.
package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// These are the snapshot gauges and the last authenticated remote-state gauge;
// a successful omission removes all of their obsolete host label values.
func competitionDistributedMetricTestGauges() []*prometheus.GaugeVec {
	return []*prometheus.GaugeVec{competitionExecutorBusy, competitionExecutorReconciliationRequired,
		competitionExecutorHeartbeatAge, competitionExecutorDeadlineRemaining, competitionExecutorQualified,
		competitionExecutorRemoteState, competitionExecutorPhysicalAvailable, competitionExecutorPhysicalObservedAt}
}

// A previous successful scrape leaves real series to be withdrawn by omission.
func seedCompetitionDistributedMetricTestGauges() {
	for _, gauge := range competitionDistributedMetricTestGauges() {
		gauge.WithLabelValues("synthetic-prior-executor").Set(1)
	}
}

// Reading a gauge with WithLabelValues would recreate an absent series, so the
// assertion inspects collector cardinality without mutating its label set.
func requireCompetitionDistributedMetricOmission(t testing.TB, aggregate float64) {
	t.Helper()
	for _, gauge := range competitionDistributedMetricTestGauges() {
		if count := testutil.CollectAndCount(gauge); count != 0 {
			t.Fatalf("successful omission retained %d stale executor series", count)
		}
	}
	if actual := testutil.ToFloat64(competitionWorkerHeartbeatAge); actual != aggregate {
		t.Fatalf("historical executor heartbeat replaced current singleton aggregate: got %v, want %v", actual, aggregate)
	}
}

// Only the existing source-activation insertion guards are bypassed for this
// synthetic metric fixture, as in the shared slot fixture. Frozen policy and
// every new distributed guard stay enabled; no historical row is rewritten.
func insertCompetitionDistributedMetricTestRound(t testing.TB, fixture *competitionDistributedPgFixture, settings *Settings, epoch int, opensAt time.Time, finalizedAt, sealedAt *time.Time) {
	t.Helper()
	copy := *settings
	copy.automationTargetRoundId = server.NewId()
	round, err := fixture.store.prepareRound(t.Context(), &copy, GenerateRoundArgs{
		OpensAt: opensAt, ClosesAt: opensAt.Add(7 * 24 * time.Hour), RevealAt: opensAt.Add(7 * 24 * time.Hour),
	}, settings.AutomationPolicy.Staging)
	if err != nil {
		t.Fatal(err)
	}
	server.Tx(t.Context(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(t.Context(), `ALTER TABLE competition_round DISABLE TRIGGER competition_v2_round_source`))
		server.RaisePgResult(tx.Exec(t.Context(), `ALTER TABLE competition_round DISABLE TRIGGER competition_v2_round_reserved_identity`))
		// Like the shared future-round fixture, this synthetic row has no
		// activated source. Only source-installation guards are suspended here.
		server.RaisePgResult(tx.Exec(t.Context(), `ALTER TABLE competition_round DISABLE TRIGGER competition_work_coverage_round`))
		server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,
		 seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled,finalized_at,automation_sealed_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11,$12,$13,$14,false,$15,$16)`,
			round.RoundId, round.CompetitionId, epoch, round.Staging, round.WorkloadCommitment, round.SeedNonce, round.SeedCiphertext,
			round.ProvidersSha256, round.ProvidersPath, string(round.PolicyJson), round.OpensAt, round.ClosesAt, round.RevealAt, round.CreatedAt, finalizedAt, sealedAt))
		server.RaisePgResult(tx.Exec(t.Context(), `ALTER TABLE competition_round ENABLE TRIGGER competition_v2_round_source`))
		server.RaisePgResult(tx.Exec(t.Context(), `ALTER TABLE competition_round ENABLE TRIGGER competition_v2_round_reserved_identity`))
		server.RaisePgResult(tx.Exec(t.Context(), `ALTER TABLE competition_round ENABLE TRIGGER competition_work_coverage_round`))
	})
}

// An earlier season's qualified epoch two cannot override the current season's
// epoch-one singleton, even when both seasons reuse the same competition id.
func TestCompetitionDistributedMetricsIgnorePriorSeasonForCurrentSingleton(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		if err := refreshCompetitionDistributedMetrics(tb.Context(), fixture.settings, fixture.clock.Now()); err != nil {
			tb.Fatal(err)
		}
		if testutil.CollectAndCount(competitionExecutorQualified) != 2 {
			tb.Fatal("historical distributed fixture did not first publish both host series")
		}
		current := *fixture.settings
		current.AutomationPolicy.SeasonId = "synthetic-current-season"
		current.DistributedEvaluationPolicy = nil
		current.EvaluationPolicy.G2VolumePolicy = model.CompetitionG2VolumePolicyLegacy
		current.EvaluationPolicy.WorkCoveragePolicy = nil
		insertCompetitionDistributedMetricTestRound(tb, fixture, &current, 1, fixture.clock.Now(), nil, nil)
		competitionExecutorRemoteState.WithLabelValues("synthetic-prior-executor").Set(3)
		competitionWorkerHeartbeatAge.Set(37)
		if err := refreshCompetitionDistributedMetrics(tb.Context(), &current, fixture.clock.Now()); err != nil {
			tb.Fatal(err)
		}
		requireCompetitionDistributedMetricOmission(tb, 37)
	})
}

// A successful no-row query is a new empty snapshot, not permission to retain
// hosts from another environment or keep their aggregate heartbeat override.
func TestCompetitionDistributedMetricsSuccessfulOmissionIgnoresForeignEnvironment(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		if err := refreshCompetitionDistributedMetrics(tb.Context(), fixture.settings, fixture.clock.Now()); err != nil {
			tb.Fatal(err)
		}
		current := *fixture.settings
		current.AutomationPolicy.Environment = "synthetic-other-environment"
		seedCompetitionDistributedMetricTestGauges()
		competitionWorkerHeartbeatAge.Set(19)
		if err := refreshCompetitionDistributedMetrics(tb.Context(), &current, fixture.clock.Now()); err != nil {
			tb.Fatal(err)
		}
		requireCompetitionDistributedMetricOmission(tb, 19)
	})
}

// The current round is selected before its execution mode. Omitting the mode
// predicate from that selection prevents fallback to older distributed history.
func TestCompetitionDistributedMetricsDoNotSkipCurrentSingleton(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		current := *fixture.settings
		current.DistributedEvaluationPolicy = nil
		insertCompetitionDistributedMetricTestRound(tb, fixture, &current, 3, fixture.clock.Now(), nil, nil)
		seedCompetitionDistributedMetricTestGauges()
		competitionWorkerHeartbeatAge.Set(23)
		if err := refreshCompetitionDistributedMetrics(tb.Context(), fixture.settings, fixture.clock.Now()); err != nil {
			tb.Fatal(err)
		}
		requireCompetitionDistributedMetricOmission(tb, 23)
	})
}

// Terminal current rounds do not revive an older unsealed round's telemetry.
func TestCompetitionDistributedMetricsOmitSealedAndFinalizedCurrentRound(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		terminalAt := fixture.clock.Now()
		for _, c := range []struct {
			epoch                 int
			finalizedAt, sealedAt *time.Time
		}{
			{epoch: 3, sealedAt: &terminalAt},
			{epoch: 4, finalizedAt: &terminalAt},
		} {
			insertCompetitionDistributedMetricTestRound(tb, fixture, fixture.settings, c.epoch, fixture.clock.Now(), c.finalizedAt, c.sealedAt)
			seedCompetitionDistributedMetricTestGauges()
			competitionWorkerHeartbeatAge.Set(29)
			if err := refreshCompetitionDistributedMetrics(tb.Context(), fixture.settings, fixture.clock.Now()); err != nil {
				tb.Fatal(err)
			}
			requireCompetitionDistributedMetricOmission(tb, 29)
		}
	})
}

// Closing does not stop executor health while already admitted work drains.
// Missing heartbeats stay unknown, not a fabricated healthy zero-age sample.
func TestCompetitionDistributedMetricsKeepClosingHostsAndMissingHeartbeat(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		fixture.clock.Advance(8 * 24 * time.Hour)
		competitionWorkerHeartbeatAge.Set(0)
		if err := refreshCompetitionDistributedMetrics(tb.Context(), fixture.settings, fixture.clock.Now()); err != nil {
			tb.Fatal(err)
		}
		if testutil.CollectAndCount(competitionExecutorQualified) != 2 || testutil.ToFloat64(competitionWorkerHeartbeatAge) != -1 {
			tb.Fatal("closing hosts vanished or absent heartbeats became healthy")
		}
		for _, host := range fixture.settings.DistributedEvaluationPolicy.Hosts {
			if testutil.ToFloat64(competitionExecutorHeartbeatAge.WithLabelValues(host.HostId)) != -1 {
				tb.Fatal("missing host heartbeat was not preserved as unknown")
			}
		}
	})
}

// A prospective scheduled round must not borrow the prior round's hosts.
func TestCompetitionDistributedMetricsDoNotFallBackBeforeScheduledOpening(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		insertCompetitionDistributedMetricTestRound(tb, fixture, fixture.settings, 3, fixture.clock.Now().Add(time.Hour), nil, nil)
		seedCompetitionDistributedMetricTestGauges()
		competitionWorkerHeartbeatAge.Set(31)
		if err := refreshCompetitionDistributedMetrics(tb.Context(), fixture.settings, fixture.clock.Now()); err != nil {
			tb.Fatal(err)
		}
		requireCompetitionDistributedMetricOmission(tb, 31)
	})
}

// Disabled or absent automation does not even acquire a database connection.
func TestCompetitionDistributedMetricsDisabledScopeSuccessfullyOmitsHosts(t *testing.T) {
	for _, settings := range []*Settings{nil, {}} {
		seedCompetitionDistributedMetricTestGauges()
		competitionWorkerHeartbeatAge.Set(41)
		if err := refreshCompetitionDistributedMetrics(t.Context(), settings, time.Time{}); err != nil {
			t.Fatal(err)
		}
		requireCompetitionDistributedMetricOmission(t, 41)
	}
}
