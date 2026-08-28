package competition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

var (
	ErrNotFound          = errors.New("competition object not found")
	ErrConflict          = errors.New("competition state conflict")
	ErrRoundClosed       = errors.New("competition round is not open")
	ErrLeaseLost         = errors.New("competition worker lease lost")
	ErrSeasonComplete    = errors.New("competition season is complete")
	ErrPreviousEpochOpen = errors.New("previous competition epoch is not finalized")
	ErrReviewNotReady    = errors.New("competition epoch is not ready for honesty review")
	ErrReviewOutOfOrder  = errors.New("competition honesty review candidate is out of order")
)

type Store interface {
	CreateRound(context.Context, *Settings, GenerateRoundArgs) (*roundRecord, error)
	CurrentRound(context.Context, *Settings) (*roundRecord, error)
	GetRound(context.Context, *Settings, server.Id) (*roundRecord, error)
	PrepareCandidateReview(context.Context, *Settings, int) (*CandidateReviewState, error)
	RecordCandidateReview(context.Context, *Settings, int, CandidateReviewDecision) (*CandidateReviewState, error)
	Leaderboards(context.Context, *Settings) (*SeasonLeaderboardResult, error)
	Enqueue(context.Context, *Settings, server.Id, *CanonicalPatch, string, string) (*queuedJob, bool, error)
	GetJob(context.Context, *Settings, server.Id, *Principal) (*queuedJob, error)
	Readiness(context.Context, *Settings) (map[string]bool, error)
	RegisterHost(context.Context, *Settings, HostSelfCheck) error
	Claim(context.Context, *Settings, string, string) (*queuedJob, error)
	Heartbeat(context.Context, *Settings, string, server.Id) error
	Complete(context.Context, *Settings, string, server.Id, EvaluationOutcome) (bool, error)
	HandBack(context.Context, string, server.Id, string) error
}

type PostgresStore struct{}

type roundPolicySnapshot struct {
	Schema               int              `json:"schema"`
	CompetitionId        string           `json:"competition_id"`
	BaseSha              string           `json:"base_sha"`
	EvaluatorImageDigest string           `json:"evaluator_image_digest"`
	ScoreSchema          int              `json:"score_schema"`
	ScorerVersion        string           `json:"scorer_version"`
	PatchPolicy          PatchPolicy      `json:"patch_policy"`
	EvaluationPolicy     EvaluationPolicy `json:"evaluation_policy"`
	SeasonPolicy         SeasonPolicy     `json:"season_policy"`
}

func policySnapshot(settings *Settings) ([]byte, error) {
	return json.Marshal(roundPolicySnapshot{
		Schema:               1,
		CompetitionId:        settings.CompetitionId,
		BaseSha:              settings.BaseSha,
		EvaluatorImageDigest: settings.EvaluatorImageDigest,
		ScoreSchema:          ScoreSchema,
		ScorerVersion:        ScorerVersion,
		PatchPolicy:          settings.PatchPolicy,
		EvaluationPolicy:     settings.EvaluationPolicy,
		SeasonPolicy:         settings.SeasonPolicy,
	})
}

// Reads the evaluator identity frozen with a round rather than the current
// process configuration, so historical job responses retain exact provenance.
func evaluatorImageDigestFromPolicy(stored json.RawMessage) (string, error) {
	var policy roundPolicySnapshot
	if len(stored) == 0 {
		return "", errors.New("round policy is empty")
	}
	if err := json.Unmarshal(stored, &policy); err != nil {
		return "", fmt.Errorf("decode round policy: %w", err)
	}
	if !imageDigestPattern.MatchString(policy.EvaluatorImageDigest) {
		return "", errors.New("round evaluator image digest is invalid")
	}
	return policy.EvaluatorImageDigest, nil
}

func (PostgresStore) CreateRound(ctx context.Context, settings *Settings, args GenerateRoundArgs) (*roundRecord, error) {
	round := &roundRecord{
		RoundResult: RoundResult{
			RoundId:     server.NewId(),
			ScoreSchema: ScoreSchema,
			OpensAt:     args.OpensAt.UTC(),
			ClosesAt:    args.ClosesAt.UTC(),
			RevealAt:    args.RevealAt.UTC(),
			CreatedAt:   server.NowUtc(),
		},
		CompetitionId: settings.CompetitionId,
	}
	policy, err := policySnapshot(settings)
	if err != nil {
		return nil, err
	}
	round.PolicyJson = policy
	round.SeedNonce, round.SeedCiphertext, round.WorkloadCommitment, err = createRoundSecret(settings, round.RoundId)
	if err != nil {
		return nil, err
	}
	seed, err := revealRoundSecret(settings, round)
	if err != nil {
		return nil, err
	}
	workload, err := generateRoundWorkload(ctx, settings, round.RoundId, seed)
	seed = ""
	if err != nil {
		return nil, err
	}
	round.ProvidersPath = workload.Path
	round.ProvidersSha256 = workload.Sha256
	if settings.artifactArchive == nil {
		removeRoundWorkload(settings, round)
		return nil, errors.New("competition artifact archive is unavailable")
	}
	if err := settings.artifactArchive.ArchiveRound(ctx, settings, round, workload); err != nil {
		removeRoundWorkload(settings, round)
		return nil, fmt.Errorf("archive round workload: %w", err)
	}
	conflict := false
	var stateErr error
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-round-v1', 0))`))
			var previousEpoch int
			server.Raise(tx.QueryRow(ctx, `
				SELECT COALESCE(max(epoch_number), 0)
				FROM competition_round WHERE competition_id = $1
			`, settings.CompetitionId).Scan(&previousEpoch))
			if settings.SeasonPolicy.EpochCount <= previousEpoch {
				stateErr = ErrSeasonComplete
				return
			}
			if 0 < previousEpoch {
				var previousFinalized *time.Time
				server.Raise(tx.QueryRow(ctx, `
					SELECT finalized_at FROM competition_round
					WHERE competition_id = $1 AND epoch_number = $2
				`, settings.CompetitionId, previousEpoch).Scan(&previousFinalized))
				if previousFinalized == nil {
					stateErr = ErrPreviousEpochOpen
					return
				}
			}
			round.Epoch = previousEpoch + 1
			var overlap bool
			server.Raise(tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM competition_round
					WHERE competition_id = $1 AND canceled = false
					  AND opens_at < $3 AND $2 < closes_at
				)
			`, settings.CompetitionId, round.OpensAt, round.ClosesAt).Scan(&overlap))
			if overlap {
				conflict = true
				return
			}
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO competition_round (
					round_id, competition_id, epoch_number, workload_commitment, seed_nonce,
					seed_ciphertext, providers_sha256, providers_path, policy_json,
					opens_at, closes_at, reveal_at, created_at, canceled
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13, false)
			`, round.RoundId, round.CompetitionId, round.Epoch, round.WorkloadCommitment,
				round.SeedNonce, round.SeedCiphertext, round.ProvidersSha256,
				round.ProvidersPath, string(round.PolicyJson), round.OpensAt,
				round.ClosesAt, round.RevealAt, round.CreatedAt))
		})
	})
	if err != nil {
		removeRoundWorkload(settings, round)
		return nil, err
	}
	if stateErr != nil {
		removeRoundWorkload(settings, round)
		return nil, stateErr
	}
	if conflict {
		removeRoundWorkload(settings, round)
		return nil, ErrConflict
	}
	setRoundStatus(round, server.NowUtc())
	competitionRoundEvents.WithLabelValues("created").Inc()
	return round, nil
}

func (PostgresStore) CurrentRound(ctx context.Context, settings *Settings) (round *roundRecord, err error) {
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			row := conn.QueryRow(ctx, `
				SELECT round_id, competition_id, epoch_number, workload_commitment, seed_nonce,
				       seed_ciphertext, providers_sha256, providers_path,
				       policy_json, opens_at, closes_at, reveal_at,
				       created_at, canceled, finalized_at, winner_job_id
				FROM competition_round
				WHERE competition_id = $1 AND canceled = false
				ORDER BY epoch_number DESC
				LIMIT 1
			`, settings.CompetitionId)
			round, err = scanRound(row)
			if errors.Is(err, pgx.ErrNoRows) {
				round, err = nil, nil
			} else {
				server.Raise(err)
			}
		})
	})
	if err == nil && round != nil {
		setRoundStatus(round, server.NowUtc())
	}
	return round, err
}

func (PostgresStore) GetRound(ctx context.Context, settings *Settings, roundId server.Id) (round *roundRecord, err error) {
	var stateErr error
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			round, err = scanRound(conn.QueryRow(ctx, `
				SELECT round_id, competition_id, epoch_number, workload_commitment, seed_nonce,
				       seed_ciphertext, providers_sha256, providers_path,
				       policy_json, opens_at, closes_at, reveal_at,
				       created_at, canceled, finalized_at, winner_job_id
				FROM competition_round
				WHERE round_id = $1 AND competition_id = $2
			`, roundId, settings.CompetitionId))
			if errors.Is(err, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(err)
		})
	})
	if err == nil && stateErr != nil {
		return nil, stateErr
	}
	if err == nil {
		setRoundStatus(round, server.NowUtc())
	}
	return round, err
}

const nextCandidateReviewSql = `
	WITH eligible AS (
		SELECT job_id, patch_sha256, patch_bytes, submitted_at, score_json,
		       CAST(row_number() OVER (
		           ORDER BY (score_json->>'normalized_score')::numeric DESC,
		                    (score_json->>'raw_score')::numeric ASC,
		                    submitted_at, job_id
		       ) AS integer) AS candidate_rank
		FROM competition_job
		WHERE round_id = $1 AND state = 'succeeded'
		  AND score_json @> '{"placeable":true,"takeover_eligible":true}'::jsonb
		  AND score_json @> '{"significance":{"statistically_significant":true,"recommended_next_epoch_takeover_margin_supported":true}}'::jsonb
		  AND jsonb_typeof(score_json->'gates') = 'object'
		  AND score_json->'gates' <> '{}'::jsonb
		  AND NOT EXISTS (
		      SELECT 1 FROM jsonb_each(score_json->'gates') AS gate
		      WHERE NOT COALESCE((gate.value->>'passed')::boolean, false)
		  )
	)
	SELECT eligible.candidate_rank, eligible.job_id, eligible.patch_sha256,
	       eligible.patch_bytes, eligible.submitted_at, eligible.score_json
	FROM eligible
	LEFT JOIN competition_candidate_review AS review
	  ON review.round_id = $1 AND review.job_id = eligible.job_id
	WHERE review.job_id IS NULL
	ORDER BY eligible.candidate_rank
	LIMIT 1
`

func validateCandidateReviewDecision(decision CandidateReviewDecision) error {
	decision.Reason = strings.TrimSpace(decision.Reason)
	if decision.JobId == (server.Id{}) {
		return errors.New("candidate review job id is required")
	}
	if decision.Decision != "approved" && decision.Decision != "rejected" {
		return errors.New("candidate review decision must be approved or rejected")
	}
	if !workerIdPattern.MatchString(decision.ReviewerId) {
		return errors.New("candidate review reviewer id is invalid")
	}
	if decision.Reason == "" || 4096 < len(decision.Reason) || strings.ContainsRune(decision.Reason, '\x00') {
		return errors.New("candidate review reason must contain 1..4096 bytes")
	}
	if !sha256Pattern.MatchString(decision.EvidenceSha256) ||
		len(decision.Evidence) == 0 || len(decision.Evidence) > 1024*1024 ||
		!json.Valid(decision.Evidence) {
		return errors.New("candidate review evidence must be valid JSON of at most 1 MiB with a SHA-256 digest")
	}
	var evidenceObject map[string]json.RawMessage
	if err := json.Unmarshal(decision.Evidence, &evidenceObject); err != nil || evidenceObject == nil {
		return errors.New("candidate review evidence must be a JSON object")
	}
	digest := sha256.Sum256(decision.Evidence)
	if hex.EncodeToString(digest[:]) != decision.EvidenceSha256 {
		return errors.New("candidate review evidence hash mismatch")
	}
	return nil
}

func candidateReviewState(round *roundRecord, status string, rejectedCount int) *CandidateReviewState {
	return &CandidateReviewState{
		CompetitionId: round.CompetitionId,
		RoundId:       round.RoundId,
		Epoch:         round.Epoch,
		Status:        status,
		RejectedCount: rejectedCount,
		FinalizedAt:   round.FinalizedAt,
		WinnerJobId:   round.WinnerJobId,
	}
}

func scanNextCandidate(row pgx.Row) (*CandidateReviewCandidate, error) {
	candidate := &CandidateReviewCandidate{}
	var scoreBytes []byte
	if err := row.Scan(
		&candidate.Rank,
		&candidate.JobId,
		&candidate.PatchSha256,
		&candidate.Patch,
		&candidate.SubmittedAt,
		&scoreBytes,
	); err != nil {
		return nil, err
	}
	if !sha256Pattern.MatchString(candidate.PatchSha256) || len(candidate.Patch) == 0 {
		return nil, errors.New("candidate review patch identity is invalid")
	}
	digest := sha256.Sum256(candidate.Patch)
	if hex.EncodeToString(digest[:]) != candidate.PatchSha256 {
		return nil, errors.New("candidate review patch hash mismatch")
	}
	if err := json.Unmarshal(scoreBytes, &candidate.Score); err != nil {
		return nil, fmt.Errorf("decode candidate review score: %w", err)
	}
	if err := validateScore(&candidate.Score); err != nil {
		return nil, fmt.Errorf("validate candidate review score: %w", err)
	}
	return candidate, nil
}

func loadCandidateReviewRound(
	ctx context.Context,
	tx server.PgTx,
	settings *Settings,
	epoch int,
) (*roundRecord, error) {
	return scanRound(tx.QueryRow(ctx, `
		SELECT round_id, competition_id, epoch_number, workload_commitment, seed_nonce,
		       seed_ciphertext, providers_sha256, providers_path,
		       policy_json, opens_at, closes_at, reveal_at,
		       created_at, canceled, finalized_at, winner_job_id
		FROM competition_round
		WHERE competition_id = $1 AND epoch_number = $2 AND canceled = false
		FOR UPDATE
	`, settings.CompetitionId, epoch))
}

func countRejectedCandidates(ctx context.Context, tx server.PgTx, roundId server.Id) int {
	var count int
	server.Raise(tx.QueryRow(ctx, `
		SELECT count(*) FROM competition_candidate_review
		WHERE round_id = $1 AND decision = 'rejected'
	`, roundId).Scan(&count))
	return count
}

func roundReadyForCandidateReview(ctx context.Context, tx server.PgTx, round *roundRecord, now time.Time) bool {
	if now.Before(round.ClosesAt) {
		return false
	}
	var active int
	server.Raise(tx.QueryRow(ctx, `
		SELECT count(*) FROM competition_job
		WHERE round_id = $1 AND state IN ('queued', 'running')
	`, round.RoundId).Scan(&active))
	return active == 0
}

func nextCandidateReview(ctx context.Context, tx server.PgTx, roundId server.Id) (*CandidateReviewCandidate, error) {
	candidate, err := scanNextCandidate(tx.QueryRow(ctx, nextCandidateReviewSql, roundId))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return candidate, err
}

func finalizeCandidateReviewRound(
	ctx context.Context,
	tx server.PgTx,
	round *roundRecord,
	winnerJobId *server.Id,
	now time.Time,
) {
	server.RaisePgResult(tx.Exec(ctx, `
		UPDATE competition_round
		SET finalized_at = $2, winner_job_id = $3
		WHERE round_id = $1
	`, round.RoundId, now, winnerJobId))
	round.FinalizedAt = &now
	round.WinnerJobId = winnerJobId
	setRoundStatus(round, now)
}

// PrepareCandidateReview seals the evaluation phase for one epoch without
// publishing a statistically significant candidate as the winner. If no
// eligible candidate remains, the epoch is safely finalized with no winner.
// Otherwise the exact next-ranked candidate stays embargoed for operator
// honesty review.
func (PostgresStore) PrepareCandidateReview(
	ctx context.Context,
	settings *Settings,
	epoch int,
) (state *CandidateReviewState, err error) {
	now := server.NowUtc()
	finalized := false
	var stateErr error
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-finalize-v2', 0))`))
			round, scanErr := loadCandidateReviewRound(ctx, tx, settings, epoch)
			if errors.Is(scanErr, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(scanErr)
			rejectedCount := countRejectedCandidates(ctx, tx, round.RoundId)
			if round.FinalizedAt != nil {
				state = candidateReviewState(round, "finalized", rejectedCount)
				return
			}
			if !roundReadyForCandidateReview(ctx, tx, round, now) {
				state = candidateReviewState(round, "evaluating", rejectedCount)
				return
			}
			candidate, candidateErr := nextCandidateReview(ctx, tx, round.RoundId)
			server.Raise(candidateErr)
			if candidate == nil {
				finalizeCandidateReviewRound(ctx, tx, round, nil, now)
				state = candidateReviewState(round, "finalized", rejectedCount)
				finalized = true
				return
			}
			state = candidateReviewState(round, "pending_review", rejectedCount)
			state.Candidate = candidate
		})
	})
	if err == nil && stateErr != nil {
		return nil, stateErr
	}
	if err == nil && finalized {
		competitionRoundEvents.WithLabelValues("finalized").Inc()
	}
	return state, err
}

// RecordCandidateReview appends an authenticated honesty decision for exactly
// the current ranked candidate. Rejections advance in score order. Approval is
// atomic with winner publication, and rejecting the last candidate finalizes
// the epoch with no winner.
func (PostgresStore) RecordCandidateReview(
	ctx context.Context,
	settings *Settings,
	epoch int,
	decision CandidateReviewDecision,
) (state *CandidateReviewState, err error) {
	decision.Reason = strings.TrimSpace(decision.Reason)
	if err := validateCandidateReviewDecision(decision); err != nil {
		return nil, err
	}
	now := server.NowUtc()
	finalized := false
	var stateErr error
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-finalize-v2', 0))`))
			round, scanErr := loadCandidateReviewRound(ctx, tx, settings, epoch)
			if errors.Is(scanErr, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(scanErr)
			if round.FinalizedAt != nil {
				stateErr = ErrConflict
				return
			}
			if !roundReadyForCandidateReview(ctx, tx, round, now) {
				stateErr = ErrReviewNotReady
				return
			}
			candidate, candidateErr := nextCandidateReview(ctx, tx, round.RoundId)
			server.Raise(candidateErr)
			if candidate == nil || candidate.JobId != decision.JobId {
				stateErr = ErrReviewOutOfOrder
				return
			}
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO competition_candidate_review (
					round_id, job_id, candidate_rank, decision, reviewer_id,
					reason, evidence_json, evidence_sha256, reviewed_at
				) VALUES ($1, $2, $3, $4, $5, $6, $7::json, $8, $9)
			`,
				round.RoundId,
				candidate.JobId,
				candidate.Rank,
				decision.Decision,
				decision.ReviewerId,
				decision.Reason,
				string(decision.Evidence),
				decision.EvidenceSha256,
				now,
			))
			rejectedCount := countRejectedCandidates(ctx, tx, round.RoundId)
			if decision.Decision == "approved" {
				winnerJobId := candidate.JobId
				finalizeCandidateReviewRound(ctx, tx, round, &winnerJobId, now)
				state = candidateReviewState(round, "finalized", rejectedCount)
				finalized = true
				return
			}
			next, nextErr := nextCandidateReview(ctx, tx, round.RoundId)
			server.Raise(nextErr)
			if next == nil {
				finalizeCandidateReviewRound(ctx, tx, round, nil, now)
				state = candidateReviewState(round, "finalized", rejectedCount)
				finalized = true
				return
			}
			state = candidateReviewState(round, "pending_review", rejectedCount)
			state.Candidate = next
		})
	})
	if err == nil && stateErr != nil {
		return nil, stateErr
	}
	if err == nil && finalized {
		competitionRoundEvents.WithLabelValues("finalized").Inc()
	}
	return state, err
}

// RequirePromotionDecision is the final source-promotion interlock. The
// promotion CLI may only carry forward a finalized no-winner epoch or apply the
// exact job that has an append-only approved honesty review.
func (PostgresStore) RequirePromotionDecision(
	ctx context.Context,
	settings *Settings,
	epoch int,
	winnerJobId *server.Id,
) (candidate *CandidateReviewCandidate, err error) {
	var stateErr error
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var finalizedAt *time.Time
			var recordedWinner *server.Id
			scanErr := conn.QueryRow(ctx, `
				SELECT finalized_at, winner_job_id
				FROM competition_round
				WHERE competition_id = $1 AND epoch_number = $2 AND canceled = false
			`, settings.CompetitionId, epoch).Scan(&finalizedAt, &recordedWinner)
			if errors.Is(scanErr, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(scanErr)
			if finalizedAt == nil {
				stateErr = ErrReviewNotReady
				return
			}
			if winnerJobId == nil {
				if recordedWinner != nil {
					stateErr = ErrConflict
				}
				return
			}
			if recordedWinner == nil || *recordedWinner != *winnerJobId {
				stateErr = ErrConflict
				return
			}
			candidate, scanErr = scanNextCandidate(conn.QueryRow(ctx, `
				SELECT review.candidate_rank, job.job_id, job.patch_sha256,
				       job.patch_bytes, job.submitted_at, job.score_json
				FROM competition_candidate_review AS review
				JOIN competition_job AS job ON job.job_id = review.job_id
				JOIN competition_round AS round ON round.round_id = review.round_id
				WHERE round.competition_id = $1 AND round.epoch_number = $2
				  AND review.job_id = $3 AND review.decision = 'approved'
			`, settings.CompetitionId, epoch, *winnerJobId))
			if errors.Is(scanErr, pgx.ErrNoRows) {
				stateErr = ErrConflict
				return
			}
			server.Raise(scanErr)
		})
	})
	if err == nil && stateErr != nil {
		return nil, stateErr
	}
	return candidate, err
}

func (PostgresStore) Leaderboards(ctx context.Context, settings *Settings) (result *SeasonLeaderboardResult, err error) {
	result = &SeasonLeaderboardResult{CompetitionId: settings.CompetitionId, Epochs: []LeaderboardResult{}}
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, queryErr := conn.Query(ctx, `
				SELECT round_id, epoch_number, finalized_at, winner_job_id
				FROM competition_round
				WHERE competition_id = $1 AND canceled = false AND finalized_at IS NOT NULL
				  AND reveal_at <= $2
				ORDER BY epoch_number
			`, settings.CompetitionId, server.NowUtc())
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					var board LeaderboardResult
					server.Raise(rows.Scan(&board.RoundId, &board.Epoch, &board.FinalizedAt, &board.WinnerJobId))
					board.CompetitionId = settings.CompetitionId
					board.Status = "finalized"
					board.Entries = []LeaderboardEntry{}
					result.Epochs = append(result.Epochs, board)
				}
			})
			for boardIndex := range result.Epochs {
				board := &result.Epochs[boardIndex]
				jobRows, jobsErr := conn.Query(ctx, `
					SELECT job.job_id, job.patch_sha256, job.submitted_at,
					       job.score_json, count(principal.principal_id),
					       COALESCE(review.decision, 'not_reviewed')
					FROM competition_job AS job
					JOIN competition_job_principal AS principal ON principal.job_id = job.job_id
					LEFT JOIN competition_candidate_review AS review
					  ON review.round_id = job.round_id AND review.job_id = job.job_id
					WHERE job.round_id = $1 AND job.state = 'succeeded'
					GROUP BY job.job_id, review.decision
					ORDER BY (job.score_json->>'normalized_score')::numeric DESC,
					         (job.score_json->>'raw_score')::numeric ASC,
					         job.submitted_at, job.job_id
				`, board.RoundId)
				server.WithPgResult(jobRows, jobsErr, func() {
					for jobRows.Next() {
						var entry LeaderboardEntry
						var scoreBytes []byte
						server.Raise(jobRows.Scan(
							&entry.JobId, &entry.PatchSha256, &entry.SubmittedAt,
							&scoreBytes, &entry.SubmitterCount, &entry.HonestyReview,
						))
						server.Raise(json.Unmarshal(scoreBytes, &entry.Score))
						server.Raise(validateScore(&entry.Score))
						entry.Rank = len(board.Entries) + 1
						entry.Winner = board.WinnerJobId != nil && *board.WinnerJobId == entry.JobId
						board.Entries = append(board.Entries, entry)
					}
				})
			}
		})
	})
	return result, err
}

func scanRound(row pgx.Row) (*roundRecord, error) {
	round := &roundRecord{}
	var policy []byte
	err := row.Scan(
		&round.RoundId, &round.CompetitionId, &round.Epoch, &round.WorkloadCommitment,
		&round.SeedNonce, &round.SeedCiphertext, &round.ProvidersSha256,
		&round.ProvidersPath, &policy, &round.OpensAt,
		&round.ClosesAt, &round.RevealAt, &round.CreatedAt, &round.Canceled,
		&round.FinalizedAt, &round.WinnerJobId,
	)
	round.PolicyJson = policy
	round.ScoreSchema = ScoreSchema
	return round, err
}

func setRoundStatus(round *roundRecord, now time.Time) {
	switch {
	case round.Canceled:
		round.Status = "canceled"
	case now.Before(round.OpensAt):
		round.Status = "scheduled"
	case now.Before(round.ClosesAt):
		round.Status = "open"
	case round.FinalizedAt == nil:
		round.Status = "grading"
	case now.Before(round.RevealAt):
		round.Status = "finalized"
	default:
		round.Status = "finalized"
	}
}

func cacheKey(roundId server.Id, patch []byte) string {
	h := sha256.New()
	h.Write([]byte("urnetwork-sim-latency-cache-v1\x00"))
	h.Write(roundId.Bytes())
	h.Write(patch)
	return hex.EncodeToString(h.Sum(nil))
}

func (PostgresStore) Enqueue(
	ctx context.Context,
	settings *Settings,
	roundId server.Id,
	patch *CanonicalPatch,
	principalId string,
	apiImageDigest string,
) (job *queuedJob, cacheHit bool, err error) {
	if _, identityErr := validateRuntimeImageDigest(apiImageDigest); identityErr != nil {
		return nil, false, identityErr
	}
	if settings == nil || settings.artifactArchive == nil {
		return nil, false, errors.New("competition submission archive is unavailable")
	}
	now := server.NowUtc()
	var stateErr error
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-submit-v1', 0))`))
			round, scanErr := scanRound(tx.QueryRow(ctx, `
				SELECT round_id, competition_id, epoch_number, workload_commitment, seed_nonce,
				       seed_ciphertext, providers_sha256, providers_path,
				       policy_json, opens_at, closes_at, reveal_at,
				       created_at, canceled, finalized_at, winner_job_id
				FROM competition_round WHERE round_id = $1 FOR SHARE
			`, roundId))
			if errors.Is(scanErr, pgx.ErrNoRows) || round.CompetitionId != settings.CompetitionId {
				stateErr = ErrNotFound
				return
			}
			server.Raise(scanErr)
			if round.Canceled || now.Before(round.OpensAt) || !now.Before(round.ClosesAt) {
				stateErr = ErrRoundClosed
				return
			}
			key := cacheKey(roundId, patch.Bytes)
			job, scanErr = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.cache_key = $1`, key), true)
			if scanErr == nil {
				cacheHit = true
				addPrincipal(ctx, tx, job.JobId, principalId, now)
				appendEvent(ctx, tx, job.JobId, now, "cache_hit", principalId, map[string]any{
					"cache_key": key, "api_image_digest": apiImageDigest,
				})
				return
			}
			if !errors.Is(scanErr, pgx.ErrNoRows) {
				server.Raise(scanErr)
			}
			jobId := server.NewId()
			submissionArtifact, archiveErr := settings.artifactArchive.ArchiveSubmission(
				ctx,
				settings,
				roundId,
				patch,
			)
			server.Raise(archiveErr)
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO competition_job (
					job_id, round_id, patch_bytes, patch_sha256, cache_key, state,
					submitted_at, available_at, artifact_retain_until, api_image_digest
				) VALUES ($1, $2, $3, $4, $5, 'queued', $6, $6, $7, $8)
			`, jobId, roundId, patch.Bytes, patch.Sha256, key, now, settings.RetainUntil, apiImageDigest))
			addPrincipal(ctx, tx, jobId, principalId, now)
			appendEvent(ctx, tx, jobId, now, "submitted", principalId, map[string]any{
				"round_id": roundId.String(), "patch_sha256": patch.Sha256, "cache_key": key,
				"api_image_digest": apiImageDigest, "submission_artifact": submissionArtifact,
			})
			job, scanErr = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.job_id = $1`, jobId), true)
			server.Raise(scanErr)
		})
	})
	if err == nil && stateErr != nil {
		err = stateErr
	}
	if err == nil && job != nil && job.State == "queued" {
		err = enqueueCompetitionJob(ctx, settings, job.JobId)
	}
	return job, cacheHit, err
}

const jobSelect = `
	SELECT j.job_id, j.round_id, j.patch_sha256, j.state, j.submitted_at,
	       j.started_at, j.completed_at, j.cache_key, j.score_json,
	       j.eval_error_json, j.patch_bytes, j.attempt_count, COALESCE(j.lease_owner, ''),
	       j.lease_expires_at, j.api_image_digest, COALESCE(j.worker_image_digest, ''),
	       r.competition_id, r.workload_commitment, r.seed_nonce,
	       r.seed_ciphertext, r.providers_sha256, r.providers_path,
	       r.policy_json, r.opens_at, r.closes_at,
	       r.reveal_at, r.created_at, r.canceled, r.epoch_number,
	       r.finalized_at, r.winner_job_id
	FROM competition_job j JOIN competition_round r ON r.round_id = j.round_id
`

func scanJob(row pgx.Row, includePatch bool) (*queuedJob, error) {
	job := &queuedJob{}
	var scoreJson, errorJson, policyJson []byte
	err := row.Scan(
		&job.JobId, &job.RoundId, &job.PatchSha256, &job.State,
		&job.SubmittedAt, &job.StartedAt, &job.CompletedAt, &job.CacheKey,
		&scoreJson, &errorJson, &job.Patch, &job.AttemptCount, &job.LeaseOwner,
		&job.LeaseExpiresAt, &job.ApiImageDigest, &job.WorkerImageDigest,
		&job.Round.CompetitionId,
		&job.Round.WorkloadCommitment, &job.Round.SeedNonce,
		&job.Round.SeedCiphertext, &job.Round.ProvidersSha256,
		&job.Round.ProvidersPath, &policyJson, &job.Round.OpensAt,
		&job.Round.ClosesAt, &job.Round.RevealAt, &job.Round.CreatedAt,
		&job.Round.Canceled, &job.Round.Epoch, &job.Round.FinalizedAt,
		&job.Round.WinnerJobId,
	)
	if err != nil {
		return nil, err
	}
	job.Round.RoundId = job.RoundId
	job.Round.ScoreSchema = ScoreSchema
	job.Round.PolicyJson = policyJson
	job.EvaluatorImageDigest, err = evaluatorImageDigestFromPolicy(policyJson)
	if err != nil {
		return nil, err
	}
	setRoundStatus(&job.Round, server.NowUtc())
	if len(scoreJson) != 0 {
		job.Score = &ScoreResult{}
		if err := json.Unmarshal(scoreJson, job.Score); err != nil {
			return nil, fmt.Errorf("decode stored score: %w", err)
		}
	}
	if len(errorJson) != 0 {
		job.EvalError = &CompetitionError{}
		if err := json.Unmarshal(errorJson, job.EvalError); err != nil {
			return nil, fmt.Errorf("decode stored evaluation error: %w", err)
		}
	}
	if !includePatch {
		job.Patch = nil
	}
	return job, nil
}

func (PostgresStore) GetJob(ctx context.Context, settings *Settings, jobId server.Id, principal *Principal) (job *queuedJob, err error) {
	var stateErr error
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			query := jobSelect + ` WHERE j.job_id = $1 AND r.competition_id = $2`
			args := []any{jobId, settings.CompetitionId}
			if principal.Role != "operator" {
				query += ` AND EXISTS (SELECT 1 FROM competition_job_principal p WHERE p.job_id = j.job_id AND p.principal_id = $3)`
				args = append(args, principal.Id)
			}
			job, err = scanJob(conn.QueryRow(ctx, query, args...), false)
			if errors.Is(err, pgx.ErrNoRows) {
				stateErr = ErrNotFound
				return
			}
			server.Raise(err)
		})
	})
	if err == nil && stateErr != nil {
		err = stateErr
	}
	return job, err
}

func (PostgresStore) Readiness(ctx context.Context, settings *Settings) (checks map[string]bool, err error) {
	checks = map[string]bool{
		"configuration":                true,
		"frozen_policy":                true,
		"retention_window":             !settings.RetainUntil.Before(settings.SeasonEndsAt),
		"database":                     false,
		"fifo_slot":                    false,
		"queue_admission":              true,
		"authoritative_evaluator_host": false,
		"artifact_storage":             false,
		"host_rebaseline":              false,
	}
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var one int
			server.Raise(conn.QueryRow(ctx, `SELECT 1`).Scan(&one))
			checks["database"] = one == 1
			var slots int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_worker_slot WHERE slot_id = 1`).Scan(&slots))
			checks["fifo_slot"] = slots == 1
			var hosts, artifactHosts, rebaselineHosts int
			server.Raise(conn.QueryRow(ctx, `
				WITH current_round AS (
					SELECT round_id::text AS round_id
					FROM competition_round
					WHERE competition_id = $6 AND canceled = false AND $5 < closes_at
					ORDER BY opens_at
					LIMIT 1
				)
				SELECT count(*),
				       count(*) FILTER (WHERE (self_check_json->>'artifact_storage')::boolean),
				       count(*) FILTER (
				           WHERE (self_check_json->>'rebaseline_passed')::boolean
				             AND self_check_json->>'rebaseline_round_id' =
				                 COALESCE((SELECT round_id FROM current_round), '')
				       )
				FROM competition_evaluator_host
				WHERE eligible = true AND hardware_id = $1 AND image_digest = $2
				  AND self_check_json->>'qualification_sha256' = $3
				  AND heartbeat_at >= $4
			`, settings.EvaluationPolicy.HardwareId, settings.EvaluatorImageDigest,
				settings.EvaluationPolicy.HostQualificationSha256,
				server.NowUtc().Add(-time.Duration(settings.HostHeartbeatMaxAgeSeconds)*time.Second),
				server.NowUtc(), settings.CompetitionId).Scan(
				&hosts, &artifactHosts, &rebaselineHosts,
			))
			checks["authoritative_evaluator_host"] = 1 <= hosts
			checks["artifact_storage"] = 1 <= artifactHosts
			checks["host_rebaseline"] = 1 <= rebaselineHosts
		})
	})
	if err == nil {
		checks["queue_admission"] = checkCompetitionFifo(ctx, settings) == nil
	}
	return checks, err
}

func (PostgresStore) RegisterHost(ctx context.Context, settings *Settings, selfCheck HostSelfCheck) error {
	bytes, err := json.Marshal(selfCheck)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(bytes)
	eligible := selfCheck.Eligible(settings)
	return captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `
				INSERT INTO competition_evaluator_host (
					host_id, hardware_id, image_digest, self_check_json,
					self_check_sha256, eligible, heartbeat_at
				) VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
				ON CONFLICT (host_id) DO UPDATE SET
					hardware_id = EXCLUDED.hardware_id,
					image_digest = EXCLUDED.image_digest,
					self_check_json = EXCLUDED.self_check_json,
					self_check_sha256 = EXCLUDED.self_check_sha256,
					eligible = EXCLUDED.eligible,
					heartbeat_at = EXCLUDED.heartbeat_at
			`, selfCheck.HostId, selfCheck.HardwareId, selfCheck.ImageDigest,
				string(bytes), hex.EncodeToString(digest[:]), eligible, server.NowUtc()))
		}, server.OptReadWrite())
	})
}

func (PostgresStore) Claim(ctx context.Context, settings *Settings, workerId string, workerImageDigest string) (job *queuedJob, err error) {
	if _, identityErr := validateRuntimeImageDigest(workerImageDigest); identityErr != nil {
		return nil, identityErr
	}
	now := server.NowUtc()
	leaseUntil := now.Add(time.Duration(settings.WorkerLeaseSeconds) * time.Second)
	// Consume one Redis wake-up in FIFO order. PostgreSQL still selects the
	// authoritative oldest eligible row, which recovers correctly after a
	// Redis flush, a lost post-commit push, or a stale list entry.
	if _, err := dequeueCompetitionJob(ctx, settings); err != nil {
		return nil, err
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			var slotWorker *string
			var slotJob *server.Id
			var slotLease *time.Time
			server.Raise(tx.QueryRow(ctx, `
				SELECT worker_id, job_id, lease_expires_at
				FROM competition_worker_slot WHERE slot_id = 1 FOR UPDATE
			`).Scan(&slotWorker, &slotJob, &slotLease))
			// An unexpired global slot is busy even when the caller reused the
			// same worker id. Treating an id match as ownership would let an
			// accidentally duplicated/restarted worker overwrite the live slot
			// and run a second job concurrently.
			if slotWorker != nil && slotLease != nil && now.Before(*slotLease) {
				return
			}
			row := tx.QueryRow(ctx, jobSelect+`
				WHERE (
				        (j.state = 'queued' AND j.available_at <= $1) OR
				        (j.state = 'running' AND j.lease_expires_at <= $1)
				      )
				  AND r.canceled = false
				  AND r.opens_at <= $1
				  AND r.finalized_at IS NULL
				ORDER BY j.submitted_at, j.job_id
				LIMIT 1 FOR UPDATE OF j SKIP LOCKED
			`, now)
			var scanErr error
			job, scanErr = scanJob(row, true)
			if errors.Is(scanErr, pgx.ErrNoRows) {
				server.RaisePgResult(tx.Exec(ctx, `
					UPDATE competition_worker_slot SET worker_id = NULL, job_id = NULL,
					lease_expires_at = NULL, heartbeat_at = $1 WHERE slot_id = 1
				`, now))
				job = nil
				return
			}
			server.Raise(scanErr)
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_job SET state = 'running', started_at = COALESCE(started_at, $2),
					lease_owner = $3, lease_expires_at = $4, attempt_count = attempt_count + 1,
					worker_image_digest = $5
				WHERE job_id = $1
			`, job.JobId, now, workerId, leaseUntil, workerImageDigest))
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_worker_slot SET worker_id = $1, job_id = $2,
					lease_expires_at = $3, heartbeat_at = $4 WHERE slot_id = 1
			`, workerId, job.JobId, leaseUntil, now))
			appendEvent(ctx, tx, job.JobId, now, "claimed", workerId, map[string]any{
				"attempt": job.AttemptCount + 1, "worker_image_digest": workerImageDigest,
			})
			job.State = "running"
			job.AttemptCount++
			job.LeaseOwner = workerId
			job.LeaseExpiresAt = &leaseUntil
			job.WorkerImageDigest = workerImageDigest
			if job.StartedAt == nil {
				job.StartedAt = &now
			}
		})
	})
	return job, err
}

func (PostgresStore) Heartbeat(ctx context.Context, settings *Settings, workerId string, jobId server.Id) error {
	now := server.NowUtc()
	leaseUntil := now.Add(time.Duration(settings.WorkerLeaseSeconds) * time.Second)
	leaseLost := false
	err := captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Exec(ctx, `
				UPDATE competition_worker_slot SET lease_expires_at = $3, heartbeat_at = $4
				WHERE slot_id = 1 AND worker_id = $1 AND job_id = $2
			`, workerId, jobId, leaseUntil, now)
			server.Raise(err)
			if result.RowsAffected() != 1 {
				leaseLost = true
				return
			}
			result, err = tx.Exec(ctx, `
				UPDATE competition_job SET lease_expires_at = $3
				WHERE job_id = $1 AND state = 'running' AND lease_owner = $2
			`, jobId, workerId, leaseUntil)
			server.Raise(err)
			if result.RowsAffected() != 1 {
				leaseLost = true
				return
			}
		})
	})
	if err == nil && leaseLost {
		return ErrLeaseLost
	}
	return err
}

type EvaluationOutcome struct {
	Score            *ScoreResult
	Error            *CompetitionError
	ArtifactManifest json.RawMessage
	Infrastructure   bool
}

// Schedules an infrastructure retry only when it can begin before the one
// submission-wide execution deadline. Queue wait before the first claim is not
// charged, while every attempt and retry backoff after it is charged.
func infrastructureRetrySchedule(
	settings *Settings,
	startedAt time.Time,
	completedAt time.Time,
	attempts int,
) (time.Time, bool) {
	retryAt := completedAt.Add(time.Duration(attempts*attempts) * 15 * time.Second)
	deadline := startedAt.Add(time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
	return retryAt, attempts < settings.MaxInfrastructureAttempts && retryAt.Before(deadline)
}

func (PostgresStore) Complete(ctx context.Context, settings *Settings, workerId string, jobId server.Id, outcome EvaluationOutcome) (retry bool, err error) {
	now := server.NowUtc()
	leaseLost := false
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			var state, owner string
			var attempts int
			var apiImageDigest, workerImageDigest string
			var startedAt time.Time
			server.Raise(tx.QueryRow(ctx, `
				SELECT state, COALESCE(lease_owner, ''), attempt_count,
				       api_image_digest, COALESCE(worker_image_digest, ''), started_at
				FROM competition_job WHERE job_id = $1 FOR UPDATE
			`, jobId).Scan(&state, &owner, &attempts, &apiImageDigest, &workerImageDigest, &startedAt))
			if state != "running" || owner != workerId {
				leaseLost = true
				return
			}
			if !imageDigestPattern.MatchString(apiImageDigest) || !imageDigestPattern.MatchString(workerImageDigest) {
				panic(errors.New("competition job runtime image identity is invalid"))
			}
			retryAt, retryBudgetAvailable := infrastructureRetrySchedule(settings, startedAt, now, attempts)
			if outcome.Infrastructure && attempts < settings.MaxInfrastructureAttempts && !retryBudgetAvailable {
				outcome.Score = nil
				outcome.Error = infrastructureError(
					"evaluation_time_budget_exhausted",
					"submission exhausted its total evaluation time budget",
				)
			}
			scoreJson, errorJson, manifestJson := nullableJson(outcome.Score), nullableJson(outcome.Error), []byte(outcome.ArtifactManifest)
			manifestHash := any(nil)
			if len(manifestJson) != 0 {
				if !json.Valid(manifestJson) {
					panic(errors.New("artifact manifest is invalid JSON"))
				}
				h := sha256.Sum256(manifestJson)
				manifestHash = hex.EncodeToString(h[:])
			}
			if outcome.Infrastructure && retryBudgetAvailable {
				retry = true
				server.RaisePgResult(tx.Exec(ctx, `
					UPDATE competition_job SET state = 'queued', available_at = $2,
						lease_owner = NULL, lease_expires_at = NULL
					WHERE job_id = $1
				`, jobId, retryAt))
				errorCode := "unknown_infrastructure_error"
				if outcome.Error != nil {
					errorCode = outcome.Error.Code
				}
				appendEvent(ctx, tx, jobId, now, "infrastructure_retry", workerId, map[string]any{
					"attempt": attempts, "error_code": errorCode,
					"artifact_manifest_sha256": manifestHash,
					"api_image_digest":         apiImageDigest, "worker_image_digest": workerImageDigest,
				})
			} else {
				terminal := "failed"
				if outcome.Score != nil && outcome.Error == nil {
					terminal = "succeeded"
				}
				server.RaisePgResult(tx.Exec(ctx, `
					UPDATE competition_job SET state = $2, completed_at = $3,
						lease_owner = NULL, lease_expires_at = NULL,
						score_json = $4::jsonb, eval_error_json = $5::jsonb,
						artifact_manifest_json = $6::jsonb,
						artifact_manifest_sha256 = $7
					WHERE job_id = $1
				`, jobId, terminal, now, scoreJson, errorJson, nullableBytes(manifestJson), manifestHash))
				appendEvent(ctx, tx, jobId, now, terminal, workerId, map[string]any{
					"attempt": attempts, "api_image_digest": apiImageDigest,
					"worker_image_digest": workerImageDigest,
				})
			}
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_worker_slot SET worker_id = NULL, job_id = NULL,
					lease_expires_at = NULL, heartbeat_at = $1
				WHERE slot_id = 1 AND worker_id = $2 AND job_id = $3
			`, now, workerId, jobId))
		})
	})
	if err == nil && leaseLost {
		err = ErrLeaseLost
	}
	if err == nil && retry {
		err = enqueueCompetitionJob(ctx, settings, jobId)
	}
	return retry, err
}

func (PostgresStore) HandBack(ctx context.Context, workerId string, jobId server.Id, reason string) error {
	now := server.NowUtc()
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, err := tx.Exec(ctx, `
				UPDATE competition_job SET state = 'queued', available_at = $3,
					lease_owner = NULL, lease_expires_at = NULL
				WHERE job_id = $1 AND state = 'running' AND lease_owner = $2
			`, jobId, workerId, now)
			server.Raise(err)
			if result.RowsAffected() == 1 {
				appendEvent(ctx, tx, jobId, now, "handed_back", workerId, map[string]any{"reason": reason})
			}
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_worker_slot SET worker_id = NULL, job_id = NULL,
					lease_expires_at = NULL, heartbeat_at = $1
				WHERE slot_id = 1 AND worker_id = $2 AND job_id = $3
			`, now, workerId, jobId))
		})
	})
}

func addPrincipal(ctx context.Context, tx server.PgTx, jobId server.Id, principal string, at time.Time) {
	server.RaisePgResult(tx.Exec(ctx, `
		INSERT INTO competition_job_principal (job_id, principal_id, first_seen_at)
		VALUES ($1, $2, $3) ON CONFLICT (job_id, principal_id) DO NOTHING
	`, jobId, principal, at))
}

func appendEvent(ctx context.Context, tx server.PgTx, jobId server.Id, at time.Time, eventType, actor string, payload any) {
	bytes, err := json.Marshal(payload)
	server.Raise(err)
	h := sha256.Sum256(bytes)
	server.RaisePgResult(tx.Exec(ctx, `
		INSERT INTO competition_job_event (
			job_id, event_at, event_type, actor_id, payload_json, payload_sha256
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6)
	`, jobId, at, eventType, actor, string(bytes), hex.EncodeToString(h[:])))
}

func nullableJson(value any) any {
	if value == nil {
		return nil
	}
	bytes, err := json.Marshal(value)
	server.Raise(err)
	return string(bytes)
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func captureDatabaseError(run func()) error {
	if recovered := server.HandleError(run); recovered != nil {
		if err, ok := recovered.(error); ok {
			return err
		}
		return fmt.Errorf("%v", recovered)
	}
	return nil
}
