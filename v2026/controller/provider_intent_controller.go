// The provider intent check task: one RunOnce chain per client, keyed by the
// client id. An intent connection starts the chain (or pulls a pending check
// forward), and each run reschedules the next check while the client stays
// connected with intent. The decisions are model.CheckProviderIntent; the
// connections only observe the state it writes.
package controller

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// a check does a few redis calls and at most a few indexed queries
const providerIntentCheckMaxTime = 2 * time.Minute

// The client whose qualification the check runs.
type ProviderIntentCheckArgs struct {
	NetworkId server.Id `json:"network_id"`
	ClientId  server.Id `json:"client_id"`
}

// The outcome of one check.
type ProviderIntentCheckResult struct {
	// when the next check runs; nil stops the chain
	CheckTime *time.Time `json:"check_time,omitempty"`
}

// The chain's key, so a schedule merges into a pending check of the client
// and keeps the earlier run.
func providerIntentCheckRunOnce(clientId server.Id) *task.RunOnceOption {
	return task.RunOnce("provider_intent_check", clientId)
}

// Starts a client's check chain at `runAt`, or pulls a pending check of the
// client forward to it.
func ScheduleProviderIntentCheck(ctx context.Context, networkId server.Id, clientId server.Id, runAt time.Time) {
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	task.ScheduleTask(
		ProviderIntentCheck,
		&ProviderIntentCheckArgs{
			NetworkId: networkId,
			ClientId:  clientId,
		},
		clientSession,
		providerIntentCheckRunOnce(clientId),
		task.RunAt(runAt),
		task.MaxTime(providerIntentCheckMaxTime),
	)
}

// Runs one check of the client's qualification (model.CheckProviderIntent).
func ProviderIntentCheck(
	providerIntentCheck *ProviderIntentCheckArgs,
	clientSession *session.ClientSession,
) (*ProviderIntentCheckResult, error) {
	checkTime := model.CheckProviderIntent(
		clientSession.Ctx,
		providerIntentCheck.NetworkId,
		providerIntentCheck.ClientId,
	)
	return &ProviderIntentCheckResult{
		CheckTime: checkTime,
	}, nil
}

// Schedules the next check of the chain, unless the chain stopped.
func ProviderIntentCheckPost(
	providerIntentCheck *ProviderIntentCheckArgs,
	providerIntentCheckResult *ProviderIntentCheckResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if providerIntentCheckResult.CheckTime == nil {
		return nil
	}
	task.ScheduleTaskInTx(
		tx,
		ProviderIntentCheck,
		providerIntentCheck,
		clientSession,
		providerIntentCheckRunOnce(providerIntentCheck.ClientId),
		task.RunAt(*providerIntentCheckResult.CheckTime),
		task.MaxTime(providerIntentCheckMaxTime),
	)
	return nil
}
