// A source-intake grant opens exactly one real full-window round. External
// workload retention precedes the opening transaction and starts no job clock.
package controller

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const competitionIntakeFirstRoundSelect = `SELECT round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,
 seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled,
 finalized_at,winner_job_id,admission_closed_at,automation_sealed_at,automation_publication_at
 FROM competition_round WHERE round_id=$1 AND competition_id=$2`

// This read is used inside an existing transaction, never as a nested pool
// acquisition. Replay authenticates the already committed workload and dates.
func competitionIntakeFirstRound(ctx context.Context, reader competitionIntakeReader, settings *Settings, roundId server.Id) (*roundRecord, error) {
	round, err := scanRound(reader.QueryRow(ctx, competitionIntakeFirstRoundSelect, roundId, settings.CompetitionId))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return round, err
}

// A repeated call returns the immutable dates without preparing another secret
// or workload. Only a not-yet-created target proceeds to protected publication.
func (self PostgresStore) CreateIntakeFirstRound(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, generation int64) (*RoundResult, error) {
	intent, err := self.SourceActivation(ctx, settings, generation)
	if err != nil || !competitionIntakeFirstApplies(settings, intent) {
		if err != nil {
			return nil, err
		}
		return nil, ErrConflict
	}
	var grant *model.CompetitionSourceIntakeActivation
	var api model.CompetitionSourceReadiness
	var existing *roundRecord
	var observed time.Time
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, lease))
			server.Raise(setCompetitionIntakeFirstAuthority(ctx, tx, settings))
			var err error
			grant, err = sourceIntakeActivation(ctx, tx, settings, generation)
			server.Raise(err)
			api, err = sourceIntakeApiReceipt(ctx, tx, settings, intent)
			server.Raise(err)
			existing, err = competitionIntakeFirstRound(ctx, tx, settings, intent.TargetRoundId)
			server.Raise(err)
			if existing != nil {
				_, err := competitionIntakeFirstActivationMatches(settings, intent, existing, api, grant)
				server.Raise(err)
			}
			observed, err = competitionIntakeFirstDatabaseTime(ctx, tx)
			server.Raise(err)
		})
	})
	if err != nil {
		return nil, err
	}
	if existing != nil {
		setRoundStatus(existing, self.nowUtc())
		return &existing.RoundResult, nil
	}
	frozen, _, err := competitionIntakeFirstFrozenSettings(settings, intent)
	if err != nil {
		return nil, err
	}
	frozen.automationLease, frozen.automationTargetRoundId = &lease, intent.TargetRoundId
	frozen.intakeFirstActivation = grant
	// Preliminary bounds avoid pointless work. The transaction chooses again
	// after ArchiveRound, so this preliminary timestamp is never the opening.
	args, err := competitionIntakeFirstOpening(frozen, intent, grant, observed)
	if err != nil {
		return nil, err
	}
	round, err := self.CreateRound(ctx, frozen, args)
	if err != nil {
		return nil, err
	}
	return &round.RoundResult, nil
}

// Called after scope and round locks but before any round insertion or job
// mutation. The concrete database trigger independently repeats this boundary.
func (self PostgresStore) checkIntakeFirstRoundInTx(ctx context.Context, tx server.PgTx, settings *Settings, round *roundRecord) error {
	grant := settings.intakeFirstActivation
	intent, err := sourceActivationInTx(ctx, tx, settings, grant.Generation)
	if err != nil {
		return err
	}
	api, err := sourceIntakeApiReceipt(ctx, tx, settings, intent)
	if err != nil {
		return err
	}
	if _, err := competitionIntakeFirstActivationMatches(settings, intent, round, api, grant); err != nil {
		return err
	}
	if err := checkCompetitionIntakeFirstAdmission(ctx, tx, settings, round); err != nil {
		return err
	}
	if err := requireSourceOperatorHandoffInTx(ctx, tx, settings, intent); err != nil {
		return err
	}
	now, err := competitionIntakeFirstDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	args, err := competitionIntakeFirstOpening(settings, intent, grant, now)
	if err != nil {
		return err
	}
	// Seed AAD and workload identity bind round/source, not these times. The
	// retained providers bytes therefore remain exactly the prepared workload.
	round.OpensAt, round.ClosesAt, round.RevealAt = args.OpensAt, args.ClosesAt, args.RevealAt
	return setCompetitionIntakeFirstAuthority(ctx, tx, settings)
}

// A missing baseline or ACK cannot consume the first attempt/clock. Old rows
// and old deployment settings still follow their unchanged path; the new SQL
// trigger additionally fences stale writers that do not know this capability.
func competitionIntakeFirstCandidateClaimInTx(ctx context.Context, tx server.PgTx, settings *Settings, roundId server.Id) (bool, error) {
	if settings.IntakeFirstPolicy == nil {
		return true, nil
	}
	var ready bool
	err := tx.QueryRow(ctx, `SELECT competition_v2_intake_grading_ready($1)`, roundId).Scan(&ready)
	return ready, err
}
