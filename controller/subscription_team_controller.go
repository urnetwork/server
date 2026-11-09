package controller

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// ----- Team grant: runs every day at 00:00:00 UTC -----
//
// Every Team period starts at a 00:00:00 UTC anchor of its own day of the month (DESIGN.md 20.9's
// anchor row), so the job runs at each day's start and writes the periods whose anchors have
// passed (model.RefreshTeamTransferBalances, plan-QUOTA-team-renewal-window). Not scheduled in a
// binary that does not link MG6, whose network_team and transfer_balance.tier it reads; sidecar
// billing, so only the production registry claims it.

type RefreshTeamTransferBalancesArgs struct {
}

type RefreshTeamTransferBalancesResult struct {
	GrantCount int `json:"grant_count"`
}

func ScheduleRefreshTeamTransferBalances(clientSession *session.ClientSession, tx server.PgTx) {
	if !server.SidecarMg6Linked() {
		return
	}
	// the start of the next day
	year, month, day := server.NowUtc().Date()
	runAt := time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
	task.ScheduleTaskInTx(
		tx,
		RefreshTeamTransferBalances,
		&RefreshTeamTransferBalancesArgs{},
		clientSession,
		task.RunOnce("refresh_team_transfer_balances"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

// Writes each live Team's next period, its team grant and credit line, at the period's anchor.
// The worker's task id is the run's replay receipt, so a body replayed after a lost handback
// grants nothing twice.
func RefreshTeamTransferBalances(
	refreshTeamTransferBalances *RefreshTeamTransferBalancesArgs,
	clientSession *session.ClientSession,
) (*RefreshTeamTransferBalancesResult, error) {
	if !server.SidecarMg6Linked() {
		return &RefreshTeamTransferBalancesResult{}, nil
	}
	grants := model.RefreshTeamTransferBalances(
		clientSession.Ctx,
		server.NowUtc(),
		transferGrantRunIds(clientSession.Ctx)...,
	)
	return &RefreshTeamTransferBalancesResult{
		GrantCount: len(grants),
	}, nil
}

func RefreshTeamTransferBalancesPost(
	refreshTeamTransferBalances *RefreshTeamTransferBalancesArgs,
	refreshTeamTransferBalancesResult *RefreshTeamTransferBalancesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRefreshTeamTransferBalances(clientSession, tx)
	return nil
}
