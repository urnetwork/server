// A concrete one-epoch worker entrypoint binds the existing evaluator singleton
// to the supervisor's exact current round, source installation, and live fence.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The round is a claimed identity, not configuration authority. Every field
// used for work is reloaded from frozen database state before execution.
type CompetitionSupervisorWorkerRequest struct {
	Operation string                           `json:"operation"`
	Round     model.CompetitionRoundResult     `json:"round"`
	Lease     model.CompetitionSupervisorLease `json:"lease"`
}

// Bounds stdin and rejects unknown/trailing fields before any service access.
func DecodeCompetitionSupervisorWorkerRequest(reader io.Reader) (*CompetitionSupervisorWorkerRequest, error) {
	content, err := io.ReadAll(io.LimitReader(reader, 1024*1024+1))
	if err != nil || len(content) > 1024*1024 {
		return nil, errors.New("supervisor worker request exceeds its bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	request := &CompetitionSupervisorWorkerRequest{}
	if err := decoder.Decode(request); err != nil {
		return nil, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("supervisor worker request has trailing JSON")
	}
	if request.Operation != "run_one_epoch" || request.Round.RoundId == (server.Id{}) || request.Round.Epoch < 1 || request.Round.Epoch > 6 ||
		request.Round.DecisionMode != model.CompetitionDecisionModeV2 || request.Lease.OwnerId == "" || request.Lease.FencingToken <= 0 || request.Lease.Environment == "" || request.Lease.CompetitionId == "" || request.Lease.SeasonId == "" {
		return nil, errors.New("supervisor worker request identity is invalid")
	}
	return request, nil
}

// Retains the existing Store contract while prohibiting latest-round lookup
// from silently switching this process into a successor or staging round.
type competitionSupervisorWorkerStore struct {
	PostgresStore
	roundId server.Id
}

// Frozen claims also carry this exact round id into the transaction boundary.
func (self competitionSupervisorWorkerStore) CurrentRound(ctx context.Context, settings *Settings) (*roundRecord, error) {
	round, err := self.PostgresStore.CurrentAutomationRound(ctx, settings)
	if err != nil {
		return nil, err
	}
	if round == nil || round.RoundId != self.roundId {
		return nil, ErrConflict
	}
	return round, nil
}

// A supervisor request never falls through to staging even if state is absent.
func (self competitionSupervisorWorkerStore) CurrentStagingRound(context.Context, *Settings) (*roundRecord, error) {
	return nil, ErrConflict
}

// Checks only the current lease, never renews it on behalf of a dead parent.
func checkCompetitionSupervisorWorkerFence(ctx context.Context, store PostgresStore, settings *Settings, lease model.CompetitionSupervisorLease) error {
	_, err := readCompetitionSupervisorWorkerFence(ctx, store, settings, lease)
	return err
}

// A successful complete read acknowledges only the database's existing
// expiry. Reading it never extends a dead parent's lease or changes its token.
func readCompetitionSupervisorWorkerFence(ctx context.Context, store PostgresStore, settings *Settings, lease model.CompetitionSupervisorLease) (time.Time, error) {
	observation := newCompetitionSupervisorRenewObservation()
	entered := server.ObserveDbContextBudget(ctx)
	var expiresAt time.Time
	err := observation.capture(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			observation.callbackEntered = true
			observation.callbacks++
			tx = observation.observeReadPrefix(tx, true)
			observation.step = competitionSupervisorWorkerFenceRead
			if !settings.AutomationPolicy.Enabled {
				panic(ErrAutomationDisabled)
			}
			if lease.Environment != settings.AutomationPolicy.Environment || lease.CompetitionId != settings.CompetitionId || lease.SeasonId != settings.AutomationPolicy.SeasonId {
				panic(ErrLeaseLost)
			}
			err := tx.QueryRow(ctx, `SELECT lease_expires_at FROM competition_supervisor_lease
			 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND owner_id=$4 AND fencing_token=$5 AND $6<lease_expires_at`,
				lease.Environment, lease.CompetitionId, lease.SeasonId, lease.OwnerId, lease.FencingToken, store.nowUtc()).Scan(&expiresAt)
			if errors.Is(err, pgx.ErrNoRows) {
				panic(ErrLeaseLost)
			}
			server.Raise(err)
			server.Raise(setAutomationLeaseGuardsInTx(ctx, tx, settings, lease))
		}, &observation.phase, &observation.timing, observation.read, &observation.transaction)
	})
	if proof, ok := err.(competitionSupervisorRenewError); ok {
		proof.entered, proof.returned = entered, server.ObserveDbContextBudget(ctx)
		err = proof
	}
	if err != nil {
		return time.Time{}, err
	}
	return expiresAt, nil
}

// Uses the normal worker/evaluator and its singleton slot; cancellation joins
// its existing containment cleanup. Periodic fence checks stop an orphaned
// launcher, while claims and evaluation heartbeats recheck the fence in SQL.
func RunCompetitionSupervisorWorker(ctx context.Context, settings *Settings, request CompetitionSupervisorWorkerRequest, workerId string) error {
	if settings == nil || !settings.AutomationPolicy.Enabled || request.Round.Staging != settings.AutomationPolicy.Staging || request.Operation != "run_one_epoch" || request.Lease.Environment != settings.AutomationPolicy.Environment || request.Lease.CompetitionId != settings.CompetitionId || request.Lease.SeasonId != settings.AutomationPolicy.SeasonId {
		return ErrAutomationDisabled
	}
	store := PostgresStore{}
	confirmedExpiry, err := readCompetitionSupervisorWorkerFence(ctx, store, settings, request.Lease)
	if err != nil {
		return err
	}
	round, err := store.CurrentAutomationRound(ctx, settings)
	if err != nil {
		return err
	}
	if round == nil || !roundUsesAutomation(round) || round.Staging != request.Round.Staging || round.RoundId != request.Round.RoundId || round.Epoch != request.Round.Epoch ||
		!round.OpensAt.Equal(request.Round.OpensAt) || !round.ClosesAt.Equal(request.Round.ClosesAt) || round.ProvidersSha256 != request.Round.ProvidersSha256 || round.WorkloadCommitment != request.Round.WorkloadCommitment {
		return ErrConflict
	}
	intent, err := store.CurrentSourceActivation(ctx, settings)
	if err != nil {
		return err
	}
	derived, err := sourceSettingsFromIntent(settings, intent)
	if err != nil {
		return err
	}
	frozen, err := settingsForFrozenRound(derived, round)
	if err != nil {
		return err
	}
	if intent == nil || intent.TargetRoundId != round.RoundId || intent.TargetEpoch != round.Epoch || intent.Generation != frozen.AutomationPolicy.SourceActivationGeneration || intent.SourceLockSha256 != frozen.AutomationPolicy.SourceLockSha256 {
		return ErrConflict
	}
	if competitionIntakeFirstApplies(settings, intent) {
		// Keep the strict genuine-runtime check and attach only an authenticated
		// intake grant so shared-baseline preparation cannot start its source job.
		frozen, err = settingsForAutomationRound(ctx, settings, store, round)
		if err != nil {
			return err
		}
	}
	if err := LoadCompetitionRuntimeManifestContext(ctx, frozen, intent); err != nil {
		return err
	}
	frozen.automationLease, frozen.automationRoundId = &request.Lease, round.RoundId
	if frozen.DistributedEvaluationPolicy != nil {
		frozen.distributedSourceCommits = map[string]string{}
		for repository, commit := range intent.Commits {
			if repository != "operator-proxy" {
				frozen.distributedSourceCommits[repository] = commit
			}
		}
	}
	workerStore := competitionSupervisorWorkerStore{PostgresStore: store, roundId: round.RoundId}
	// Setup may have outlived the request's initial confirmed window. No
	// evaluator has started; only a fresh real read may authorize its launch.
	confirmedExpiry, err = readCompetitionSupervisorWorkerFence(ctx, store, frozen, request.Lease)
	if err != nil {
		return err
	}
	return runCompetitionSupervisorWorkerFenced(ctx, confirmedExpiry, defaultCompetitionSupervisorWorkerFenceSettings(),
		func(ctx context.Context) (time.Time, error) {
			return readCompetitionSupervisorWorkerFence(ctx, store, frozen, request.Lease)
		},
		func(operationCtx context.Context) error {
			if frozen.DistributedEvaluationPolicy != nil {
				return runCompetitionDistributedCoordinator(operationCtx, frozen, workerStore, workerId)
			}
			worker, err := NewWorker(frozen, workerStore, CommandEvaluator{}, workerId)
			if err != nil {
				return err
			}
			return worker.Run(operationCtx)
		})
}
