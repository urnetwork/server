package controller

// The task that cleans up after an account deletion (owner 2026-11-16,
// "clean up everything on account deletion"; model.RemoveDeletedNetworkData).
// Every deletion path queues it in the deletion's own transaction
// (RemoveNetworkAndContacts for the account deletion and `bringyourctl
// network remove`, ScheduleDeletedNetworkCleanup for the lag.dev guest
// expiry), so a deletion that commits always has its cleanup queued, and one
// that does not queues nothing. One task per network, keyed by its id: a run
// cleans as far as its budget allows and queues the next run, a minute later
// while work is left, or when the network's first client becomes due for the
// hard delete, until nothing is left. A failed run is retried after
// RetryInterval.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type RemoveDeletedNetworkDataArgs struct {
	DeletedNetwork *model.DeletedNetwork `json:"deleted_network"`
}

type RemoveDeletedNetworkDataResult struct {
	Cleanup *model.DeletedNetworkCleanupResult `json:"cleanup"`
	// when the next run starts; nil once nothing is left
	RunAt *time.Time `json:"run_at,omitempty"`
}

// Queues the cleanup of the deleted network in tx, to run at runAt. The task
// is queued without the deleting session's credential or address, so its
// rows hold the network's id and user ids and nothing else of the account.
func ScheduleRemoveDeletedNetworkDataInTx(
	ctx context.Context,
	tx server.PgTx,
	deletedNetwork *model.DeletedNetwork,
	runAt time.Time,
) {
	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()
	task.ScheduleTaskInTx(
		tx,
		RemoveDeletedNetworkData,
		&RemoveDeletedNetworkDataArgs{
			DeletedNetwork: deletedNetwork,
		},
		clientSession,
		task.RunOnce("remove_deleted_network_data", deletedNetwork.NetworkId),
		task.RunAt(runAt),
		task.MaxTime(model.DefaultDeletedNetworkCleanupSettings().TaskMaxTime),
	)
}

// The scheduler a deletion that removes no sign-in addresses hands to the
// model (the lag.dev guest expiry): it queues the cleanup only.
func ScheduleDeletedNetworkCleanup(ctx context.Context) model.ScheduleNetworkRemovalsInTx {
	return func(tx server.PgTx, deletedNetwork *model.DeletedNetwork, userAuths map[string]bool) {
		ScheduleRemoveDeletedNetworkDataInTx(ctx, tx, deletedNetwork, server.NowUtc())
	}
}

func RemoveDeletedNetworkData(
	removeDeletedNetworkData *RemoveDeletedNetworkDataArgs,
	clientSession *session.ClientSession,
) (*RemoveDeletedNetworkDataResult, error) {
	deletedNetwork := removeDeletedNetworkData.DeletedNetwork
	if deletedNetwork == nil {
		// nothing names a network to clean up; there is nothing to retry
		return &RemoveDeletedNetworkDataResult{}, nil
	}
	settings := model.DefaultDeletedNetworkCleanupSettings()
	return removeDeletedNetworkDataWithSettings(clientSession.Ctx, deletedNetwork, server.NowUtc(), settings)
}

// One run on the given clock and settings; a failed run returns an error
// whose retry waits RetryInterval.
func removeDeletedNetworkDataWithSettings(
	ctx context.Context,
	deletedNetwork *model.DeletedNetwork,
	now time.Time,
	settings *model.DeletedNetworkCleanupSettings,
) (*RemoveDeletedNetworkDataResult, error) {
	var cleanup *model.DeletedNetworkCleanupResult
	if r := server.HandleError(func() {
		cleanup = model.RemoveDeletedNetworkData(ctx, deletedNetwork, now, settings)
	}); r != nil {
		err, ok := r.(error)
		if !ok {
			err = fmt.Errorf("%v", r)
		}
		return nil, task.WithRetryDelay(
			errors.Join(fmt.Errorf("deleted network %s cleanup", deletedNetwork.NetworkId), err),
			settings.RetryInterval,
		)
	}

	result := &RemoveDeletedNetworkDataResult{
		Cleanup: cleanup,
	}
	switch {
	case cleanup.NetworkExists:
		glog.Infof("[dnc]network %s exists; no cleanup\n", deletedNetwork.NetworkId)
	case cleanup.Complete:
		glog.Infof("[dnc]network %s cleaned up: %v\n", deletedNetwork.NetworkId, cleanup.RowCounts)
	default:
		// while work is left the next run continues soon; clients that wait
		// for their hard delete set the time
		runAt := now.Add(settings.ContinueInterval)
		if !cleanup.More && cleanup.NextClientDueTime != nil && runAt.Before(*cleanup.NextClientDueTime) {
			runAt = *cleanup.NextClientDueTime
		}
		result.RunAt = &runAt
	}
	return result, nil
}

// Queues the next run while the cleanup has work left.
func RemoveDeletedNetworkDataPost(
	removeDeletedNetworkData *RemoveDeletedNetworkDataArgs,
	removeDeletedNetworkDataResult *RemoveDeletedNetworkDataResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if removeDeletedNetworkDataResult.RunAt == nil || removeDeletedNetworkData.DeletedNetwork == nil {
		return nil
	}
	ScheduleRemoveDeletedNetworkDataInTx(
		clientSession.Ctx,
		tx,
		removeDeletedNetworkData.DeletedNetwork,
		*removeDeletedNetworkDataResult.RunAt,
	)
	return nil
}
