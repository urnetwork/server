// Exercises automatic staging winner selection and publication independently
// of production's manual honesty-review and source-promotion lifecycle.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Persists a synthetic completed job using the current shared-control completion
// path, or explicit historical score bytes for legacy ranking fixtures.
func (self *competitionRankingFixture) completeScore(t testing.TB, score *ScoreResult, shared bool) *queuedJob {
	t.Helper()
	job := self.addJob(t)
	if shared {
		outcome := testSharedBaselineOutcome(t, self.settings, job, EvaluationOutcome{Score: score})
		if retry, err := self.store.Complete(context.Background(), self.settings, "ranking-worker", job.JobId, outcome); err != nil || retry {
			t.Fatalf("complete synthetic staging score: retry=%t error=%v", retry, err)
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
	return job
}

// Direct SQL callers must obey the same winner identity as the application.
func assertStagingWinnerWriteRejected(t testing.TB, fixture *competitionRankingFixture, winnerJobId *server.Id, want string) {
	t.Helper()
	var writeErr error
	server.Db(context.Background(), func(conn server.PgConn) {
		_, writeErr = conn.Exec(context.Background(), `
			UPDATE competition_round SET finalized_at = $2, winner_job_id = $3,
				admission_closed_at = COALESCE(admission_closed_at, closes_at)
			WHERE round_id = $1
		`, fixture.round.RoundId, fixture.now, winnerJobId)
	}, server.OptReadWrite())
	if writeErr == nil || !strings.Contains(writeErr.Error(), want) {
		t.Fatalf("direct staging winner write error = %v, want %q", writeErr, want)
	}
}

// New staging finalizations obey the round's own ranking policy, while reviews
// remain prohibited and neither scope can authorize the other's promotion.
func TestCompetitionStagingWinnerPreservesRankingAndReviewIsolation(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, shared := range []bool{false, true} {
			fixture := newCompetitionRankingFixture(t, true)
			fixture.completeJobs(t, shared)
			fixture.now = fixture.round.ClosesAt.Add(time.Second)
			bestIndex, otherIndex := 0, 1
			if shared {
				bestIndex, otherIndex = 1, 0
			}
			bestJobId := fixture.jobs[bestIndex].JobId
			assertStagingWinnerWriteRejected(t, fixture, nil, "highest-ranked eligible job")
			assertStagingWinnerWriteRejected(t, fixture, &fixture.jobs[otherIndex].JobId, "highest-ranked eligible job")
			var reviewErr error
			server.Db(ctx, func(conn server.PgConn) {
				_, reviewErr = conn.Exec(ctx, `
					INSERT INTO competition_candidate_review (
						round_id, job_id, candidate_rank, decision, reviewer_id,
						reason, evidence_json, evidence_sha256, reviewed_at
					) VALUES ($1, $2, 1, 'approved', 'synthetic-staging-reviewer',
						'staging review must remain blocked', '{"synthetic":true}'::json, $3, $4)
				`, fixture.round.RoundId, bestJobId, strings.Repeat("a", 64), fixture.now)
			}, server.OptReadWrite())
			if reviewErr == nil || !strings.Contains(reviewErr.Error(), "staging round cannot enter candidate review") {
				t.Fatalf("shared=%t staging review gate error = %v", shared, reviewErr)
			}
			if _, err := fixture.store.PrepareCandidateReview(ctx, fixture.settings, fixture.round.Epoch); !errors.Is(err, ErrNotFound) {
				t.Fatalf("production review selected staging: %v", err)
			}
			finalized, err := fixture.store.FinalizeStagingRound(ctx, fixture.settings, fixture.round.Epoch)
			if err != nil || finalized.FinalizedAt == nil || finalized.WinnerJobId == nil || *finalized.WinnerJobId != bestJobId {
				t.Fatalf("shared=%t automatic winner = %+v, error=%v", shared, finalized, err)
			}
			for _, winnerJobId := range []*server.Id{nil, &bestJobId} {
				if _, err := fixture.store.RequirePromotionDecision(ctx, fixture.settings, fixture.round.Epoch, winnerJobId); !errors.Is(err, ErrNotFound) {
					t.Fatalf("staging authorized source promotion: %v", err)
				}
			}
			boards, err := fixture.store.Leaderboards(ctx, fixture.settings, true)
			if err != nil || len(boards.Epochs) != 1 || boards.Epochs[0].WinnerJobId == nil || *boards.Epochs[0].WinnerJobId != bestJobId {
				t.Fatalf("shared=%t staging winner board = %+v, error=%v", shared, boards, err)
			}
			for index, entry := range boards.Epochs[0].Entries {
				if entry.HonestyReview != "not_reviewed" || entry.Winner != (index == 0) {
					t.Fatalf("automatic staging review/winner state = %+v", entry)
				}
			}
			if public, err := fixture.store.Leaderboards(ctx, fixture.settings, false); err != nil || len(public.Epochs) != 0 {
				t.Fatalf("default leaderboard leaked staging = %+v, %v", public, err)
			}
			fixture.now = fixture.now.Add(time.Minute)
			again, err := fixture.store.FinalizeStagingRound(ctx, fixture.settings, fixture.round.Epoch)
			if err != nil || again.WinnerJobId == nil || *again.WinnerJobId != bestJobId || !again.FinalizedAt.Equal(*finalized.FinalizedAt) {
				t.Fatalf("repeated staging finalization changed winner/history = %+v, %v", again, err)
			}
			assertStagingWinnerWriteRejected(t, fixture, &fixture.jobs[otherIndex].JobId, "immutable")
			server.Db(ctx, func(conn server.PgConn) {
				var reviews int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_candidate_review WHERE round_id = $1`, fixture.round.RoundId).Scan(&reviews))
				if reviews != 0 {
					t.Fatalf("staging created %d honesty review records", reviews)
				}
			})
		}
	})
}

// Faster but unplaceable jobs cannot win; an all-unplaceable round remains null.
// Statistical significance and takeover margin are not staging gates.
func TestCompetitionStagingWinnerRequiresEveryEligibilityGate(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		for _, includeEligible := range []bool{false, true} {
			fixture := newCompetitionRankingFixture(t, true)
			for index, mutate := range []func(*ScoreResult){
				func(score *ScoreResult) { score.Placeable, score.TakeoverEligible = false, false },
				func(score *ScoreResult) {
					score.Gates["G1"] = Gate{Passed: false, Details: map[string]any{}}
				},
				func(score *ScoreResult) { score.Gates = map[string]Gate{} },
			} {
				score := competitionRankingScore(float64(10+index), 190)
				mutate(score)
				fixture.completeScore(t, score, true)
			}
			var expectedWinner *server.Id
			if includeEligible {
				job := fixture.completeScore(t, competitionRankingScore(100, 110), true)
				expectedWinner = &job.JobId
			}
			fixture.now = fixture.round.ClosesAt.Add(time.Second)
			assertStagingWinnerWriteRejected(t, fixture, &fixture.jobs[2].JobId, "highest-ranked eligible job")
			finalized, err := fixture.store.FinalizeStagingRound(context.Background(), fixture.settings, fixture.round.Epoch)
			if err != nil || finalized.FinalizedAt == nil || (finalized.WinnerJobId == nil) != (expectedWinner == nil) ||
				expectedWinner != nil && *finalized.WinnerJobId != *expectedWinner {
				t.Fatalf("include eligible=%t: winner=%+v, error=%v", includeEligible, finalized, err)
			}
		}
	})
}

// The four-job epoch-8 shape must elect its fastest safe result even when
// every candidate misses both significance and the production takeover bar.
func TestCompetitionStagingWinnerIgnoresSignificanceAndTakeover(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		fixture := newCompetitionRankingFixture(t, true)
		var bestJobId server.Id
		for index, raw := range []float64{120, 130, 115, 110} {
			score := competitionRankingScore(raw, 100)
			score.TakeoverEligible = false
			score.Significance = testScoreSignificance(false)
			job := fixture.completeScore(t, score, true)
			if index == 3 {
				bestJobId = job.JobId
			}
		}
		fixture.now = fixture.round.ClosesAt.Add(time.Second)
		assertStagingWinnerWriteRejected(t, fixture, nil, "highest-ranked eligible job")
		finalized, err := fixture.store.FinalizeStagingRound(context.Background(), fixture.settings, fixture.round.Epoch)
		if err != nil || finalized.WinnerJobId == nil || *finalized.WinnerJobId != bestJobId {
			t.Fatalf("non-significant staging winner = %+v, error=%v", finalized, err)
		}
		boards, err := fixture.store.Leaderboards(context.Background(), fixture.settings, true)
		if err != nil || len(boards.Epochs) != 1 || boards.Epochs[0].WinnerJobId == nil ||
			*boards.Epochs[0].WinnerJobId != bestJobId || !boards.Epochs[0].Entries[0].Winner {
			t.Fatalf("non-significant staging leaderboard = %+v, error=%v", boards, err)
		}
	})
}

// Natural close must publish consistently before a later scheduled reveal,
// and equal-score ties must use submission time then immutable job identity.
func TestCompetitionStagingWinnerNaturalCloseAndTieBreaks(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionRankingFixture(t, true)
		// Close the empty first epoch, then create a synthetic future-reveal round.
		fixture.now = fixture.round.ClosesAt.Add(time.Second)
		if _, err := fixture.store.FinalizeStagingRound(ctx, fixture.settings, fixture.round.Epoch); err != nil {
			t.Fatal(err)
		}
		var err error
		fixture.round, err = fixture.store.CreateStagingRound(ctx, fixture.settings, GenerateRoundArgs{
			OpensAt: fixture.now, ClosesAt: fixture.now.Add(time.Hour), RevealAt: fixture.now.Add(2 * time.Hour),
		}, false)
		if err != nil {
			t.Fatal(err)
		}
		first := fixture.completeScore(t, competitionRankingScore(100, 110), true)
		fixture.now = fixture.now.Add(-time.Second)
		second := fixture.completeScore(t, competitionRankingScore(100, 110), true)
		fixture.completeScore(t, competitionRankingScore(100, 110), true)
		expectedJobId := first.JobId
		if second.JobId.String() < expectedJobId.String() {
			expectedJobId = second.JobId
		}
		fixture.now = fixture.round.ClosesAt
		finalized, err := fixture.store.FinalizeStagingRound(ctx, fixture.settings, fixture.round.Epoch)
		if err != nil || finalized.WinnerJobId == nil || *finalized.WinnerJobId != expectedJobId ||
			finalized.AdmissionClosedAt == nil || !finalized.AdmissionClosedAt.Equal(fixture.round.ClosesAt) ||
			!finalized.RevealAt.Equal(fixture.round.RevealAt) || !roundPublished(finalized, fixture.now) {
			t.Fatalf("natural-close winner/publication = %+v, %v", finalized, err)
		}
		boards, err := fixture.store.Leaderboards(ctx, fixture.settings, true)
		if err != nil || len(boards.Epochs) != 2 || boards.Epochs[1].WinnerJobId == nil || *boards.Epochs[1].WinnerJobId != expectedJobId {
			t.Fatalf("natural-close winner board = %+v, %v", boards, err)
		}
	})
}

// The same numeric epoch in staging and production identifies different
// rounds. Stale staging jobs cannot become a production review or promotion.
func TestCompetitionStagingWinnerCannotCrossProductionEpoch(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionRankingFixture(t, true)
		fixture.now = fixture.round.ClosesAt.Add(time.Second)
		if _, err := fixture.store.FinalizeStagingRound(ctx, fixture.settings, fixture.round.Epoch); err != nil {
			t.Fatal(err)
		}
		args := GenerateRoundArgs{OpensAt: fixture.now, ClosesAt: fixture.now.Add(time.Hour), RevealAt: fixture.now.Add(time.Hour)}
		var err error
		fixture.round, err = fixture.store.CreateStagingRound(ctx, fixture.settings, args, false)
		if err != nil || fixture.round.Epoch != 1 {
			t.Fatalf("staging epoch one = %+v, %v", fixture.round, err)
		}
		stagingJob := fixture.completeScore(t, competitionRankingScore(90, 115), true)
		if _, err := fixture.store.CloseStagingRound(ctx, fixture.settings); err != nil {
			t.Fatal(err)
		}
		stagingRound, err := fixture.store.FinalizeStagingRound(ctx, fixture.settings, 1)
		if err != nil || stagingRound.WinnerJobId == nil || *stagingRound.WinnerJobId != stagingJob.JobId {
			t.Fatalf("staging winner = %+v, %v", stagingRound, err)
		}
		fixture.round, err = fixture.store.CreateRound(ctx, fixture.settings, args)
		if err != nil || fixture.round.Epoch != 1 || fixture.round.Staging {
			t.Fatalf("production epoch one = %+v, %v", fixture.round, err)
		}
		productionJob := fixture.completeScore(t, competitionRankingScore(95, 110), true)
		fixture.now = fixture.round.ClosesAt.Add(time.Second)
		review, err := fixture.store.PrepareCandidateReview(ctx, fixture.settings, 1)
		if err != nil || review.Candidate == nil || review.Candidate.JobId != productionJob.JobId || review.RoundId == stagingRound.RoundId {
			t.Fatalf("production review crossed staging scope = %+v, %v", review, err)
		}
		if _, err := fixture.store.RecordCandidateReview(ctx, fixture.settings, 1, testCandidateReviewDecision(stagingJob.JobId, "approved")); !errors.Is(err, ErrReviewOutOfOrder) {
			t.Fatalf("stale staging review accepted by production: %v", err)
		}
		review, err = fixture.store.RecordCandidateReview(ctx, fixture.settings, 1, testCandidateReviewDecision(productionJob.JobId, "approved"))
		if err != nil || review.WinnerJobId == nil || *review.WinnerJobId != productionJob.JobId {
			t.Fatalf("production approval changed = %+v, %v", review, err)
		}
		if _, err := fixture.store.RequirePromotionDecision(ctx, fixture.settings, 1, &stagingJob.JobId); !errors.Is(err, ErrConflict) {
			t.Fatalf("staging winner authorized production promotion: %v", err)
		}
		if candidate, err := fixture.store.RequirePromotionDecision(ctx, fixture.settings, 1, &productionJob.JobId); err != nil || candidate.JobId != productionJob.JobId {
			t.Fatalf("production approval no longer authorizes promotion = %+v, %v", candidate, err)
		}
		retained, err := fixture.store.FinalizeStagingRound(ctx, fixture.settings, 1)
		if err != nil || retained.WinnerJobId == nil || *retained.WinnerJobId != stagingJob.JobId || !retained.FinalizedAt.Equal(*stagingRound.FinalizedAt) {
			t.Fatalf("production changed historical staging winner = %+v, %v", retained, err)
		}
		boards, err := fixture.store.Leaderboards(ctx, fixture.settings, false)
		if err != nil || len(boards.Epochs) != 1 || boards.Epochs[0].Staging || *boards.Epochs[0].WinnerJobId != productionJob.JobId {
			t.Fatalf("production leaderboard crossed scopes = %+v, %v", boards, err)
		}
	})
}

// Creates an isolated durable adapter record for a synthetic admission.
func newStagingWinnerAdapterFixture(t testing.TB, staging bool) (*ApexAdapterFileStore, LeaderboardResult, time.Time) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := NewApexAdapterFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	roundId, jobId := server.NewId(), server.NewId()
	const submissionId = "synthetic-winner"
	patchSha256 := strings.Repeat("a", 64)
	if _, err := store.BeginSubmission(submissionId, patchSha256, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRound(submissionId, roundId, staging, now); err != nil {
		t.Fatal(err)
	}
	if !staging {
		if _, err := store.RecordFee(submissionId, "synthetic-receipt", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RecordAdmission(submissionId, ScoreAcceptedResult{
		JobId: jobId, RoundId: roundId, PatchSha256: patchSha256,
		Staging: staging, State: "queued", StatusUrl: "/competition/score/" + jobId.String(),
	}, now); err != nil {
		t.Fatal(err)
	}
	honestyReview := "approved"
	if staging {
		honestyReview = "not_reviewed"
	}
	board := LeaderboardResult{
		RoundId: roundId, Staging: staging, Status: "finalized", FinalizedAt: now.Add(time.Hour), WinnerJobId: &jobId,
		Entries: []LeaderboardEntry{{
			Rank: 1, JobId: jobId, PatchSha256: patchSha256, Winner: true, HonestyReview: honestyReview,
			Score: *competitionRankingScore(90, 110),
		}},
	}
	return store, board, now.Add(2 * time.Hour)
}

// Both scopes require a consistent eligible winner, but staging never claims
// an honesty approval. Historical null-winner staging boards remain accepted.
func TestApexAdapterAcceptsAutomaticStagingAndReviewedProductionWinners(t *testing.T) {
	for _, staging := range []bool{false, true} {
		store, board, now := newStagingWinnerAdapterFixture(t, staging)
		if staging {
			board.Entries[0].Score.TakeoverEligible = false
			board.Entries[0].Score.Significance = testScoreSignificance(false)
		}
		if err := store.ReconcileLeaderboard(SeasonLeaderboardResult{Epochs: []LeaderboardResult{board}}, now); err != nil {
			t.Fatal(err)
		}
		record, err := store.Get("synthetic-winner")
		if err != nil || !record.Published || !record.Winner || record.HonestyReview != board.Entries[0].HonestyReview || record.Score == nil {
			t.Fatalf("staging=%t coherent winner was not persisted = %+v, %v", staging, record, err)
		}
	}
	store, board, now := newStagingWinnerAdapterFixture(t, true)
	board.WinnerJobId, board.Entries[0].Winner = nil, false
	if err := store.ReconcileLeaderboard(SeasonLeaderboardResult{Epochs: []LeaderboardResult{board}}, now); err != nil {
		t.Fatalf("historical null-winner staging board rejected: %v", err)
	}
}

// Malformed or scope-confused finalization must not partially persist a result.
func TestApexAdapterRejectsInconsistentAutomaticAndReviewedWinners(t *testing.T) {
	for _, staging := range []bool{false, true} {
		for index, mutate := range []func(*LeaderboardResult){
			func(board *LeaderboardResult) { board.WinnerJobId = nil },
			func(board *LeaderboardResult) { board.Entries[0].Winner = false },
			func(board *LeaderboardResult) { board.Entries = nil },
			func(board *LeaderboardResult) { board.Entries = append(board.Entries, board.Entries[0]) },
			func(board *LeaderboardResult) { board.Entries[0].HonestyReview = "rejected" },
			func(board *LeaderboardResult) {
				if board.Staging {
					board.Entries[0].HonestyReview = "approved"
				} else {
					board.Entries[0].HonestyReview = "not_reviewed"
				}
			},
			func(board *LeaderboardResult) { board.Entries[0].Score.TakeoverEligible = false },
			func(board *LeaderboardResult) { board.Entries[0].Score.Significance = testScoreSignificance(false) },
			func(board *LeaderboardResult) { board.Entries[0].Score.Gates = map[string]Gate{} },
			func(board *LeaderboardResult) {
				board.Entries[0].Score.Gates["G1"] = Gate{Passed: false, Details: map[string]any{}}
			},
			func(board *LeaderboardResult) { board.Entries[0].Score.RawScore = nil },
		} {
			if staging && (index == 6 || index == 7) {
				continue
			}
			store, board, now := newStagingWinnerAdapterFixture(t, staging)
			mutate(&board)
			if err := store.ReconcileLeaderboard(SeasonLeaderboardResult{Epochs: []LeaderboardResult{board}}, now); err == nil {
				t.Fatalf("staging=%t invalid winner mutation %d accepted", staging, index)
			}
			record, err := store.Get("synthetic-winner")
			if err != nil || record.Published || record.Winner || record.Score != nil || record.State != "queued" {
				t.Fatalf("rejected winner mutated durable state = %+v, %v", record, err)
			}
		}
	}
}
