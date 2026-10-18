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
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) { server.Raise(store.requireAutomationLeaseInTx(ctx, tx, settings, lease)) })
	})
}

// Uses the normal worker/evaluator and its singleton slot; cancellation joins
// its existing containment cleanup. Periodic fence checks stop an orphaned
// launcher, while claims and evaluation heartbeats recheck the fence in SQL.
func RunCompetitionSupervisorWorker(ctx context.Context, settings *Settings, request CompetitionSupervisorWorkerRequest, workerId string) error {
	if settings == nil || !settings.AutomationPolicy.Enabled || request.Round.Staging != settings.AutomationPolicy.Staging || request.Operation != "run_one_epoch" || request.Lease.Environment != settings.AutomationPolicy.Environment || request.Lease.CompetitionId != settings.CompetitionId || request.Lease.SeasonId != settings.AutomationPolicy.SeasonId {
		return ErrAutomationDisabled
	}
	store := PostgresStore{}
	if err := checkCompetitionSupervisorWorkerFence(ctx, store, settings, request.Lease); err != nil {
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
	if err := LoadCompetitionRuntimeManifest(frozen, intent); err != nil {
		return err
	}
	frozen.automationLease, frozen.automationRoundId = &request.Lease, round.RoundId
	worker, err := NewWorker(frozen, competitionSupervisorWorkerStore{PostgresStore: store, roundId: round.RoundId}, CommandEvaluator{}, workerId)
	if err != nil {
		return err
	}
	operationCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-operationCtx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			if err := checkCompetitionSupervisorWorkerFence(operationCtx, store, frozen, request.Lease); err != nil {
				cancel(err)
				return
			}
		}
	}()
	defer func() { cancel(nil); <-done }()
	err = worker.Run(operationCtx)
	if cause := context.Cause(operationCtx); cause != nil {
		return cause
	}
	return err
}
