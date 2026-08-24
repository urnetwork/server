package competition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server/v2026"
)

var (
	ErrNotFound    = errors.New("competition object not found")
	ErrConflict    = errors.New("competition state conflict")
	ErrRoundClosed = errors.New("competition round is not open")
	ErrQueueFull   = errors.New("competition queue is full")
	ErrLeaseLost   = errors.New("competition worker lease lost")
)

type Store interface {
	CreateRound(context.Context, *Settings, GenerateRoundArgs) (*roundRecord, error)
	CurrentRound(context.Context, *Settings) (*roundRecord, error)
	GetRound(context.Context, *Settings, server.Id) (*roundRecord, error)
	Enqueue(context.Context, *Settings, server.Id, *CanonicalPatch, string) (*queuedJob, bool, error)
	GetJob(context.Context, *Settings, server.Id, *Principal) (*queuedJob, error)
	Readiness(context.Context, *Settings) (map[string]bool, error)
	RegisterHost(context.Context, *Settings, HostSelfCheck) error
	Claim(context.Context, *Settings, string) (*queuedJob, error)
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
	})
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
	conflict := false
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-round-v1', 0))`))
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
					round_id, competition_id, workload_commitment, seed_nonce,
					seed_ciphertext, providers_sha256, providers_path, policy_json,
					opens_at, closes_at, reveal_at, created_at, canceled
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, $11, $12, false)
			`, round.RoundId, round.CompetitionId, round.WorkloadCommitment,
				round.SeedNonce, round.SeedCiphertext, round.ProvidersSha256,
				round.ProvidersPath, string(round.PolicyJson), round.OpensAt,
				round.ClosesAt, round.RevealAt, round.CreatedAt))
		})
	})
	if err != nil {
		removeRoundWorkload(settings, round)
		return nil, err
	}
	if conflict {
		removeRoundWorkload(settings, round)
		return nil, ErrConflict
	}
	setRoundStatus(round, server.NowUtc())
	return round, nil
}

func (PostgresStore) CurrentRound(ctx context.Context, settings *Settings) (round *roundRecord, err error) {
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			row := conn.QueryRow(ctx, `
				SELECT round_id, competition_id, workload_commitment, seed_nonce,
				       seed_ciphertext, providers_sha256, providers_path,
				       policy_json, opens_at, closes_at, reveal_at,
				       created_at, canceled
				FROM competition_round
				WHERE competition_id = $1 AND canceled = false
				ORDER BY
					CASE
						WHEN opens_at <= $2 AND $2 < closes_at THEN 0
						WHEN $2 < opens_at THEN 1
						ELSE 2
					END,
					CASE WHEN $2 < opens_at THEN opens_at END ASC,
					opens_at DESC
				LIMIT 1
			`, settings.CompetitionId, server.NowUtc())
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
				SELECT round_id, competition_id, workload_commitment, seed_nonce,
				       seed_ciphertext, providers_sha256, providers_path,
				       policy_json, opens_at, closes_at, reveal_at,
				       created_at, canceled
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

func scanRound(row pgx.Row) (*roundRecord, error) {
	round := &roundRecord{}
	var policy []byte
	err := row.Scan(
		&round.RoundId, &round.CompetitionId, &round.WorkloadCommitment,
		&round.SeedNonce, &round.SeedCiphertext, &round.ProvidersSha256,
		&round.ProvidersPath, &policy, &round.OpensAt,
		&round.ClosesAt, &round.RevealAt, &round.CreatedAt, &round.Canceled,
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
	case now.Before(round.RevealAt):
		round.Status = "closed"
	default:
		round.Status = "revealed"
	}
}

func cacheKey(roundId server.Id, patch []byte) string {
	h := sha256.New()
	h.Write([]byte("urnetwork-sim-latency-cache-v1\x00"))
	h.Write(roundId.Bytes())
	h.Write(patch)
	return hex.EncodeToString(h.Sum(nil))
}

func (PostgresStore) Enqueue(ctx context.Context, settings *Settings, roundId server.Id, patch *CanonicalPatch, principalId string) (job *queuedJob, cacheHit bool, err error) {
	now := server.NowUtc()
	var stateErr error
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('competition-submit-v1', 0))`))
			round, scanErr := scanRound(tx.QueryRow(ctx, `
				SELECT round_id, competition_id, workload_commitment, seed_nonce,
				       seed_ciphertext, providers_sha256, providers_path,
				       policy_json, opens_at, closes_at, reveal_at,
				       created_at, canceled
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
				appendEvent(ctx, tx, job.JobId, now, "cache_hit", principalId, map[string]any{"cache_key": key})
				return
			}
			if !errors.Is(scanErr, pgx.ErrNoRows) {
				server.Raise(scanErr)
			}
			var active int
			server.Raise(tx.QueryRow(ctx, `
				SELECT count(*) FROM competition_job WHERE state IN ('queued', 'running')
			`).Scan(&active))
			if settings.EvaluationPolicy.QueueLimit <= active {
				stateErr = ErrQueueFull
				return
			}
			jobId := server.NewId()
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO competition_job (
					job_id, round_id, patch_bytes, patch_sha256, cache_key, state,
					submitted_at, available_at, artifact_retain_until
				) VALUES ($1, $2, $3, $4, $5, 'queued', $6, $6, $7)
			`, jobId, roundId, patch.Bytes, patch.Sha256, key, now, settings.RetainUntil))
			addPrincipal(ctx, tx, jobId, principalId, now)
			appendEvent(ctx, tx, jobId, now, "submitted", principalId, map[string]any{
				"round_id": roundId.String(), "patch_sha256": patch.Sha256, "cache_key": key,
			})
			job, scanErr = scanJob(tx.QueryRow(ctx, jobSelect+` WHERE j.job_id = $1`, jobId), true)
			server.Raise(scanErr)
		})
	})
	if err == nil && stateErr != nil {
		err = stateErr
	}
	return job, cacheHit, err
}

const jobSelect = `
	SELECT j.job_id, j.round_id, j.patch_sha256, j.state, j.submitted_at,
	       j.started_at, j.completed_at, j.cache_key, j.score_json,
	       j.eval_error_json, j.patch_bytes, j.attempt_count, COALESCE(j.lease_owner, ''),
	       j.lease_expires_at,
	       r.competition_id, r.workload_commitment, r.seed_nonce,
	       r.seed_ciphertext, r.providers_sha256, r.providers_path,
	       r.policy_json, r.opens_at, r.closes_at,
	       r.reveal_at, r.created_at, r.canceled
	FROM competition_job j JOIN competition_round r ON r.round_id = j.round_id
`

func scanJob(row pgx.Row, includePatch bool) (*queuedJob, error) {
	job := &queuedJob{}
	var scoreJson, errorJson, policyJson []byte
	err := row.Scan(
		&job.JobId, &job.RoundId, &job.PatchSha256, &job.State,
		&job.SubmittedAt, &job.StartedAt, &job.CompletedAt, &job.CacheKey,
		&scoreJson, &errorJson, &job.Patch, &job.AttemptCount, &job.LeaseOwner,
		&job.LeaseExpiresAt, &job.Round.CompetitionId,
		&job.Round.WorkloadCommitment, &job.Round.SeedNonce,
		&job.Round.SeedCiphertext, &job.Round.ProvidersSha256,
		&job.Round.ProvidersPath, &policyJson, &job.Round.OpensAt,
		&job.Round.ClosesAt, &job.Round.RevealAt, &job.Round.CreatedAt,
		&job.Round.Canceled,
	)
	if err != nil {
		return nil, err
	}
	job.Round.RoundId = job.RoundId
	job.Round.ScoreSchema = ScoreSchema
	job.Round.PolicyJson = policyJson
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
		"configuration":                  true,
		"frozen_policy":                  true,
		"retention_window":               !settings.RetainUntil.Before(settings.SeasonEndsAt),
		"database":                       false,
		"fifo_slot":                      false,
		"queue_admission":                false,
		"authoritative_evaluator_host":    false,
		"artifact_storage":               false,
		"host_rebaseline":                false,
	}
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var one int
			server.Raise(conn.QueryRow(ctx, `SELECT 1`).Scan(&one))
			checks["database"] = one == 1
			var slots int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_worker_slot WHERE slot_id = 1`).Scan(&slots))
			checks["fifo_slot"] = slots == 1
			var active int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job WHERE state IN ('queued', 'running')`).Scan(&active))
			checks["queue_admission"] = active < settings.EvaluationPolicy.QueueLimit
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

func (PostgresStore) Claim(ctx context.Context, settings *Settings, workerId string) (job *queuedJob, err error) {
	now := server.NowUtc()
	leaseUntil := now.Add(time.Duration(settings.WorkerLeaseSeconds) * time.Second)
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
				WHERE (j.state = 'queued' AND j.available_at <= $1)
				   OR (j.state = 'running' AND j.lease_expires_at <= $1)
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
					lease_owner = $3, lease_expires_at = $4, attempt_count = attempt_count + 1
				WHERE job_id = $1
			`, job.JobId, now, workerId, leaseUntil))
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_worker_slot SET worker_id = $1, job_id = $2,
					lease_expires_at = $3, heartbeat_at = $4 WHERE slot_id = 1
			`, workerId, job.JobId, leaseUntil, now))
			appendEvent(ctx, tx, job.JobId, now, "claimed", workerId, map[string]any{
				"attempt": job.AttemptCount + 1,
			})
			job.State = "running"
			job.AttemptCount++
			job.LeaseOwner = workerId
			job.LeaseExpiresAt = &leaseUntil
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

func (PostgresStore) Complete(ctx context.Context, settings *Settings, workerId string, jobId server.Id, outcome EvaluationOutcome) (retry bool, err error) {
	now := server.NowUtc()
	leaseLost := false
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			var state, owner string
			var attempts int
			server.Raise(tx.QueryRow(ctx, `
				SELECT state, COALESCE(lease_owner, ''), attempt_count
				FROM competition_job WHERE job_id = $1 FOR UPDATE
			`, jobId).Scan(&state, &owner, &attempts))
			if state != "running" || owner != workerId {
				leaseLost = true
				return
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
			if outcome.Infrastructure && attempts < settings.MaxInfrastructureAttempts {
				retry = true
				backoff := time.Duration(attempts*attempts) * 15 * time.Second
				server.RaisePgResult(tx.Exec(ctx, `
					UPDATE competition_job SET state = 'queued', available_at = $2,
						lease_owner = NULL, lease_expires_at = NULL
					WHERE job_id = $1
				`, jobId, now.Add(backoff)))
				errorCode := "unknown_infrastructure_error"
				if outcome.Error != nil {
					errorCode = outcome.Error.Code
				}
				appendEvent(ctx, tx, jobId, now, "infrastructure_retry", workerId, map[string]any{
					"attempt": attempts, "error_code": errorCode,
					"artifact_manifest_sha256": manifestHash,
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
				appendEvent(ctx, tx, jobId, now, terminal, workerId, map[string]any{"attempt": attempts})
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
