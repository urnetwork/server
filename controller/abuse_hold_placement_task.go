package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The API authorizes the staff action. Its file-cluster identity cannot read
// GroupForms. The worker owns that read, so it places each hold, its object
// and its row. A renewal reads nothing the worker alone can, and writes through
// the API's own hold/ writer (installApiLegalHolds; QD-1399). The API
// acknowledges only after the placement transaction and mirror have succeeded.
var legalHoldDispatch atomic.Bool

// Hands each hold's placement to the worker, in a process that serves the staff route (InstallApiRoutes). Answers
// what puts the previous choice back.
func InstallLegalHoldDispatch() (restore func()) {
	previous := legalHoldDispatch.Swap(true)
	return func() { legalHoldDispatch.Store(previous) }
}

type LegalHoldPlacementTaskArgs struct {
	NetworkId server.Id `json:"network_id"`
	CaseId    server.Id `json:"case_id"`
}

type LegalHoldPlacementTaskResult struct {
	Inserted bool   `json:"inserted,omitempty"`
	Failure  string `json:"failure,omitempty"`
}

var holdTaskErrors = []error{
	model.ErrLegalHoldsDisabled, model.ErrLegalHoldInvalid, model.ErrLegalHoldConflict, model.ErrLegalHoldNotRenewable,
	model.ErrLegalHoldNotFound, model.ErrLegalHoldExpired, model.ErrAbuseCaseNotFound,
	model.ErrAbuseUnlinked, model.ErrNetworkNotFound,
	model.ErrHoldPlacementSnapshotUnavailable,
}

func LegalHoldPlacementTask(args *LegalHoldPlacementTaskArgs, owner *session.ClientSession) (*LegalHoldPlacementTaskResult, error) {
	if !server.SidecarMg5Linked() {
		return nil, model.ErrAbuseUnlinked
	}
	terms, err := model.LegalHoldPlacementTerms(owner.Ctx, args.NetworkId, args.CaseId)
	var result *LegalHoldResult
	if err == nil {
		result, err = placeLegalHoldLocally(owner.Ctx, terms)
	}
	if err != nil {
		for _, refusal := range holdTaskErrors {
			if errors.Is(err, refusal) {
				if terms != nil {
					model.DiscardLegalHoldPlacementRequest(owner.Ctx, terms)
				}
				return &LegalHoldPlacementTaskResult{Failure: refusal.Error()}, nil
			}
		}
		// The host may have lost the reply to a successful mirror PUT.
		// Retrying from the durable manifest uses its original values.
		return nil, model.ErrLegalHoldStoreUnavailable
	}
	return &LegalHoldPlacementTaskResult{Inserted: result.Inserted}, nil
}

func dispatchLegalHold(ctx context.Context, terms *model.LegalHoldArgs) (*LegalHoldResult, error) {
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	args := &LegalHoldPlacementTaskArgs{NetworkId: terms.NetworkId, CaseId: terms.CaseId}
	_, _, now := currentAbuseHoldDeps()
	settings := model.DefaultAbuseHoldSettings()
	var id server.Id
	if err := model.QueueLegalHoldPlacement(ctx, terms, now(), func(tx server.PgTx) {
		id = task.ScheduleTaskInTx(tx, LegalHoldPlacementTask, args, owner, task.Priority(task.TaskPriorityFastest),
			task.MaxTime(settings.PassTaskMaxTime))
	}); err != nil {
		return nil, err
	}
	poll := time.NewTicker(settings.PlacementPollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-poll.C:
			finished := task.GetFinishedTasks(ctx, id)[id]
			if finished == nil {
				continue
			}
			var result LegalHoldPlacementTaskResult
			if err := json.Unmarshal([]byte(finished.ResultJson), &result); err != nil {
				return nil, model.ErrLegalHoldStoreUnavailable
			}
			if result.Failure != "" {
				for _, refusal := range holdTaskErrors {
					if result.Failure == refusal.Error() {
						return nil, refusal
					}
				}
				return nil, model.ErrLegalHoldStoreUnavailable
			}
			hold := model.GetLegalHold(ctx, args.NetworkId, args.CaseId)
			if hold == nil {
				return nil, model.ErrLegalHoldNotFound
			}
			return &LegalHoldResult{Hold: hold, Inserted: result.Inserted, BindTime: legalHoldBindTime(now())}, nil
		}
	}
}
