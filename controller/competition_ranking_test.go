// Exercises legacy and shared-control ranking against PostgreSQL, with explicit
// historical fixtures and current scored-completion transactions.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Test-local clock and immutable identities are owned by one test goroutine.
type competitionRankingFixture struct {
	settings *Settings
	store    PostgresStore
	round    *roundRecord
	now      time.Time
	jobs     []*queuedJob
}

// Creates a synthetic round without queue or evaluator services.
func newCompetitionRankingFixture(t testing.TB, staging bool) *competitionRankingFixture {
	t.Helper()
	fixture := &competitionRankingFixture{
		settings: validSettings(),
		now:      server.NowUtc().Truncate(time.Second),
	}
	fixture.settings.CompetitionId = "synthetic-ranking-" + server.NewId().String()
	fixture.settings.ArtifactRoot = t.TempDir()
	fixture.settings.SeasonEndsAt = fixture.now.Add(60 * 24 * time.Hour)
	fixture.settings.RetainUntil = fixture.settings.SeasonEndsAt.Add(30 * 24 * time.Hour)
	fixture.store = PostgresStore{now: func() time.Time { return fixture.now }}
	args := GenerateRoundArgs{
		OpensAt: fixture.now, ClosesAt: fixture.now.Add(time.Hour), RevealAt: fixture.now.Add(time.Hour),
	}
	var err error
	if staging {
		fixture.round, err = fixture.store.CreateStagingRound(context.Background(), fixture.settings, args, false)
	} else {
		fixture.round, err = fixture.store.CreateRound(context.Background(), fixture.settings, args)
	}
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// Materializes a running attempt directly, without Redis signals or a worker.
func (self *competitionRankingFixture) addJob(t testing.TB) *queuedJob {
	t.Helper()
	self.now = self.now.Add(time.Second)
	jobId := server.NewId()
	patch := []byte("synthetic-ranking-patch-" + jobId.String() + "\n")
	patchDigest := sha256.Sum256(patch)
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId: jobId, RoundId: self.round.RoundId,
			PatchSha256: hex.EncodeToString(patchDigest[:]),
		},
		Patch: patch, AttemptCount: 1, Round: *self.round,
	}
	server.Db(context.Background(), func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(context.Background(), `
			INSERT INTO competition_job (
				job_id, round_id, patch_bytes, patch_sha256, cache_key, state,
				submitted_at, available_at, started_at, lease_owner, attempt_count,
				artifact_retain_until, api_image_digest, worker_image_digest
			) VALUES ($1, $2, $3, $4, $4, 'running', $5, $5, $5, 'ranking-worker', 1, $6, $7, $8)
		`, job.JobId, job.RoundId, job.Patch, job.PatchSha256, self.now,
			self.settings.RetainUntil, testApiImageDigest(), testWorkerImageDigest()))
		server.RaisePgResult(conn.Exec(context.Background(), `
			INSERT INTO competition_job_principal (job_id, principal_id, first_seen_at)
			VALUES ($1, 'synthetic-ranking-submitter', $2)
		`, job.JobId, self.now))
	}, server.OptReadWrite())
	self.jobs = append(self.jobs, job)
	return job
}

// Uses conflicting synthetic normalized/raw values to distinguish the rules.
func competitionRankingScore(raw, normalized float64) *ScoreResult {
	return &ScoreResult{
		ScoreSchema: ScoreSchema, RawScore: &raw, NormalizedScore: &normalized,
		Placeable: true, TakeoverEligible: true,
		Gates:        map[string]Gate{"G1": {Passed: true, Details: map[string]any{}}},
		Significance: testScoreSignificance(true),
	}
}

// Legacy rows model persisted pre-upgrade scores; shared rows must pass the
// current completion gate and persist a control before they become visible.
func (self *competitionRankingFixture) completeJobs(t testing.TB, shared bool) {
	t.Helper()
	for _, values := range []struct{ raw, normalized float64 }{
		{raw: 420, normalized: 160},
		{raw: 390, normalized: 140},
		{raw: 400, normalized: 140},
		{raw: 410, normalized: 130},
	} {
		job := self.addJob(t)
		score := competitionRankingScore(values.raw, values.normalized)
		if shared {
			outcome := testSharedBaselineOutcome(t, self.settings, job, EvaluationOutcome{Score: score})
			if retry, err := self.store.Complete(context.Background(), self.settings, "ranking-worker", job.JobId, outcome); err != nil || retry {
				t.Fatalf("complete shared score: retry=%t error=%v", retry, err)
			}
		} else {
			scoreBytes, err := json.Marshal(score)
			if err != nil {
				t.Fatal(err)
			}
			server.Db(context.Background(), func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(context.Background(), `
					UPDATE competition_job SET state = 'succeeded', completed_at = $2,
						lease_owner = NULL, score_json = $3::jsonb WHERE job_id = $1
				`, job.JobId, self.now, string(scoreBytes)))
			}, server.OptReadWrite())
		}
	}
}

// Finalized legacy ranks must not follow a new API deployment's current policy.
func TestCompetitionLegacyFinalizedLeaderboardRetainsNormalizedOrdering(t *testing.T) {
	testEnv := &server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}
	testEnv.Run(t, func(t testing.TB) {
		// Exercise an already-applied 677, not just a fresh latest schema.
		// TestEnv owns this disposable database; shared local data is untouched.
		server.ApplyDbMigrationsUpTo(context.Background(), 677)
		fixture := newCompetitionRankingFixture(t, true)
		fixture.completeJobs(t, false)
		fixture.now = fixture.round.ClosesAt.Add(time.Second)
		if _, err := fixture.store.FinalizeStagingRound(context.Background(), fixture.settings, fixture.round.Epoch); err != nil {
			t.Fatal(err)
		}
		var original []byte
		for iteration := range 2 {
			if iteration == 1 {
				server.ApplyDbMigrations(context.Background())
				fixture.settings.BaseSha = strings.Repeat("9", 40)
				fixture.settings.EvaluatorImageDigest = "sha256:" + strings.Repeat("8", 64)
			}
			boards, err := fixture.store.Leaderboards(context.Background(), fixture.settings, true)
			if err != nil || len(boards.Epochs) != 1 || len(boards.Epochs[0].Entries) != len(fixture.jobs) {
				t.Fatalf("legacy boards = %+v, error = %v", boards, err)
			}
			board := boards.Epochs[0]
			if board.WinnerJobId != nil {
				t.Fatal("legacy staging winner changed")
			}
			for index, entry := range board.Entries {
				if entry.JobId != fixture.jobs[index].JobId || entry.Rank != index+1 || entry.Winner {
					t.Fatalf("legacy entry %d = %+v, want job %s", index, entry, fixture.jobs[index].JobId)
				}
			}
			encoded, err := json.Marshal(boards)
			if err != nil {
				t.Fatal(err)
			}
			if iteration == 0 {
				original = encoded
			} else if string(encoded) != string(original) {
				t.Fatal("legacy leaderboard changed with current release identity")
			}
		}
	})
}

// Shared rounds ignore normalized score, even when the display values conflict.
func TestCompetitionSharedBaselineLeaderboardRanksRawLatency(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		fixture := newCompetitionRankingFixture(t, true)
		fixture.completeJobs(t, true)
		fixture.now = fixture.round.ClosesAt.Add(time.Second)
		if _, err := fixture.store.FinalizeStagingRound(context.Background(), fixture.settings, fixture.round.Epoch); err != nil {
			t.Fatal(err)
		}
		boards, err := fixture.store.Leaderboards(context.Background(), fixture.settings, true)
		if err != nil || len(boards.Epochs) != 1 || len(boards.Epochs[0].Entries) != len(fixture.jobs) {
			t.Fatalf("shared boards = %+v, error = %v", boards, err)
		}
		for rank, jobIndex := range []int{1, 2, 3, 0} {
			entry := boards.Epochs[0].Entries[rank]
			if entry.JobId != fixture.jobs[jobIndex].JobId || entry.Rank != rank+1 {
				t.Fatalf("shared rank %d = job %s, want %s", rank+1, entry.JobId, fixture.jobs[jobIndex].JobId)
			}
		}
	})
}

// Both the application query and the database trigger must preserve the same
// absolute candidate ranks across rejection and approval for either policy.
func TestCompetitionCandidateReviewPreservesRoundRankingPolicy(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		for _, shared := range []bool{false, true} {
			fixture := newCompetitionRankingFixture(t, false)
			fixture.completeJobs(t, shared)
			fixture.now = fixture.round.ClosesAt.Add(time.Second)
			order := []int{0, 1, 2, 3}
			if shared {
				order = []int{1, 2, 3, 0}
			}
			state, err := fixture.store.PrepareCandidateReview(context.Background(), fixture.settings, fixture.round.Epoch)
			if err != nil || state == nil || state.Candidate == nil || state.Candidate.JobId != fixture.jobs[order[0]].JobId || state.Candidate.Rank != 1 {
				t.Fatalf("shared=%t first review = %+v, error = %v", shared, state, err)
			}
			var skippedErr error
			server.Db(context.Background(), func(conn server.PgConn) {
				_, skippedErr = conn.Exec(context.Background(), `
					INSERT INTO competition_candidate_review (
						round_id, job_id, candidate_rank, decision, reviewer_id,
						reason, evidence_json, evidence_sha256, reviewed_at
					) VALUES ($1, $2, 2, 'approved', 'synthetic-reviewer', 'skip first',
						'{"synthetic":true}'::json, $3, $4)
				`, fixture.round.RoundId, fixture.jobs[order[1]].JobId, strings.Repeat("a", 64), fixture.now)
			}, server.OptReadWrite())
			if skippedErr == nil || !strings.Contains(skippedErr.Error(), "higher-ranked candidate") {
				t.Fatalf("shared=%t direct skipped review = %v", shared, skippedErr)
			}
			state, err = fixture.store.RecordCandidateReview(context.Background(), fixture.settings, fixture.round.Epoch,
				testCandidateReviewDecision(fixture.jobs[order[0]].JobId, "rejected"))
			if err != nil || state == nil || state.Candidate == nil || state.Candidate.JobId != fixture.jobs[order[1]].JobId || state.Candidate.Rank != 2 {
				t.Fatalf("shared=%t next review = %+v, error = %v", shared, state, err)
			}
			state, err = fixture.store.RecordCandidateReview(context.Background(), fixture.settings, fixture.round.Epoch,
				testCandidateReviewDecision(fixture.jobs[order[1]].JobId, "approved"))
			if err != nil || state == nil || state.WinnerJobId == nil || *state.WinnerJobId != fixture.jobs[order[1]].JobId {
				t.Fatalf("shared=%t winner = %+v, error = %v", shared, state, err)
			}
			candidate, err := fixture.store.RequirePromotionDecision(context.Background(), fixture.settings, fixture.round.Epoch, state.WinnerJobId)
			if err != nil || candidate == nil || candidate.Rank != 2 || candidate.JobId != *state.WinnerJobId {
				t.Fatalf("shared=%t immutable approved review = %+v, error = %v", shared, candidate, err)
			}
			boards, err := fixture.store.Leaderboards(context.Background(), fixture.settings, false)
			if err != nil || len(boards.Epochs) != 1 || len(boards.Epochs[0].Entries) != len(order) {
				t.Fatalf("shared=%t reviewed leaderboard = %+v, error = %v", shared, boards, err)
			}
			for rank, jobIndex := range order {
				entry := boards.Epochs[0].Entries[rank]
				if entry.JobId != fixture.jobs[jobIndex].JobId || entry.Rank != rank+1 || entry.Winner != (rank == 1) {
					t.Fatalf("shared=%t reviewed rank %d = %+v", shared, rank+1, entry)
				}
			}
		}
	})
}

// A successful transaction cannot expose a new score without its control row.
func TestCompetitionScoredCompletionRequiresRoundBaseline(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		fixture := newCompetitionRankingFixture(t, true)
		job := fixture.addJob(t)
		valid := testSharedBaselineOutcome(t, fixture.settings, job, EvaluationOutcome{Score: competitionRankingScore(90, 110)})
		for _, fields := range []struct{ bytes, hash bool }{
			{bytes: false, hash: false}, {bytes: true, hash: false}, {bytes: false, hash: true},
		} {
			outcome := valid
			if !fields.bytes {
				outcome.RoundBaselineJson = nil
			}
			if !fields.hash {
				outcome.RoundBaselineSha256 = ""
			}
			if _, err := fixture.store.Complete(context.Background(), fixture.settings, "ranking-worker", job.JobId, outcome); err == nil ||
				!strings.Contains(err.Error(), "scored competition outcome requires its round baseline") {
				t.Fatalf("missing baseline fields %+v: %v", fields, err)
			}
			server.Db(context.Background(), func(conn server.PgConn) {
				var state string
				var controlCount int
				server.Raise(conn.QueryRow(context.Background(), `
					SELECT state, (SELECT count(*) FROM competition_round_baseline WHERE round_id = $2)
					FROM competition_job WHERE job_id = $1
				`, job.JobId, job.RoundId).Scan(&state, &controlCount))
				if state != "running" || controlCount != 0 {
					t.Fatalf("incomplete outcome changed persistent state: state=%s controls=%d", state, controlCount)
				}
			})
		}
		if retry, err := fixture.store.Complete(context.Background(), fixture.settings, "ranking-worker", job.JobId, valid); err != nil || retry {
			t.Fatalf("complete authenticated score: retry=%t error=%v", retry, err)
		}
		server.Db(context.Background(), func(conn server.PgConn) {
			var state, baselineSha256 string
			server.Raise(conn.QueryRow(context.Background(), `
				SELECT job.state, baseline.baseline_sha256 FROM competition_job AS job
				JOIN competition_round_baseline AS baseline ON baseline.round_id = job.round_id
				WHERE job.job_id = $1
			`, job.JobId).Scan(&state, &baselineSha256))
			if state != "succeeded" || baselineSha256 != valid.RoundBaselineSha256 {
				t.Fatalf("scored outcome lacks its atomic control identity: %s %s", state, baselineSha256)
			}
		})
	})
}

// A first shared control cannot relabel historical scored or finalized rounds.
func TestCompetitionRoundBaselineCannotChangeLegacyRankingPolicy(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		fixture := newCompetitionRankingFixture(t, true)
		fixture.completeJobs(t, false)
		job := fixture.addJob(t)
		outcome := testSharedBaselineOutcome(t, fixture.settings, job, EvaluationOutcome{Score: competitionRankingScore(90, 110)})
		if _, err := fixture.store.Complete(context.Background(), fixture.settings, "ranking-worker", job.JobId, outcome); err == nil ||
			!strings.Contains(err.Error(), "cannot replace legacy ranking policy") {
			t.Fatalf("legacy ranking policy changed: %v", err)
		}
		if _, err := fixture.store.Complete(context.Background(), fixture.settings, "ranking-worker", job.JobId,
			EvaluationOutcome{Error: submissionError("synthetic_failure", "synthetic terminal fixture")}); err != nil {
			t.Fatal(err)
		}
		fixture.now = fixture.round.ClosesAt.Add(time.Second)
		if _, err := fixture.store.FinalizeStagingRound(context.Background(), fixture.settings, fixture.round.Epoch); err != nil {
			t.Fatal(err)
		}
		var insertErr error
		server.Db(context.Background(), func(conn server.PgConn) {
			_, insertErr = conn.Exec(context.Background(), `
				INSERT INTO competition_round_baseline (
					round_id, baseline_json, baseline_sha256, source_job_id, source_attempt,
					source_artifact_manifest_sha256, base_sha, providers_sha256,
					evaluator_image_digest, scorer_version, hardware_id, host_qualification_sha256, created_at
				) VALUES ($1, $2, $3, $4, 1, $5, $6, $7, $8, $9, $10, $11, $12)
			`, job.RoundId, outcome.RoundBaselineJson, outcome.RoundBaselineSha256, job.JobId,
				strings.Repeat("a", 64), fixture.settings.BaseSha, job.Round.ProvidersSha256,
				fixture.settings.EvaluatorImageDigest, ScorerVersion, fixture.settings.EvaluationPolicy.HardwareId,
				fixture.settings.EvaluationPolicy.HostQualificationSha256, fixture.now)
		}, server.OptReadWrite())
		if insertErr == nil || !strings.Contains(insertErr.Error(), "cannot be attached after finalization or cancellation") {
			t.Fatalf("finalized legacy ranking policy changed: %v", insertErr)
		}
		server.Db(context.Background(), func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(context.Background(), `SELECT count(*) FROM competition_round_baseline WHERE round_id = $1`, job.RoundId).Scan(&count))
			if count != 0 {
				t.Fatalf("legacy round acquired %d shared controls", count)
			}
		})
	})
}
