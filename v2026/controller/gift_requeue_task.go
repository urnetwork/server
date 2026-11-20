package controller

// The gift year queue's hourly reconciliation (model.RebaseQueuedGiftYears,
// mmm/GIFTOFPRIVACY.md §5). A gift year redeemed while the recipient had paid
// Pro waits behind that cover; when the cover ends early (a store refund or
// revocation, a card period cut), this moves the waiting years up to start
// within the hour, so the recipient is never left without Pro while a gift
// waits.

import (
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// How often the reconciliation runs: the longest a recipient can go without
// Pro after the cover ahead of a queued gift year ends.
const RebaseQueuedGiftYearsInterval = 1 * time.Hour

type RebaseQueuedGiftYearsArgs struct {
}

type RebaseQueuedGiftYearsResult struct {
	// networks whose queued gift years were moved up to start now
	NetworkCount int `json:"network_count"`
	// gift years moved
	YearCount int `json:"year_count"`
}

func ScheduleRebaseQueuedGiftYears(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RebaseQueuedGiftYears,
		&RebaseQueuedGiftYearsArgs{},
		clientSession,
		task.RunOnce("rebase_queued_gift_years"),
		task.RunAt(server.NowUtc().Add(RebaseQueuedGiftYearsInterval)),
		task.MaxTime(30*time.Minute),
	)
}

// RebaseQueuedGiftYears moves up the queued gift years of every network left
// without paid Pro cover (model.RebaseQueuedGiftYears).
func RebaseQueuedGiftYears(
	args *RebaseQueuedGiftYearsArgs,
	clientSession *session.ClientSession,
) (*RebaseQueuedGiftYearsResult, error) {
	rebased := model.RebaseQueuedGiftYears(clientSession.Ctx, server.NowUtc())
	result := &RebaseQueuedGiftYearsResult{
		NetworkCount: len(rebased),
	}
	for _, count := range rebased {
		result.YearCount += count
	}
	if 0 < result.NetworkCount {
		glog.Infof("[gift]moved up %d queued gift years of %d networks left without paid Pro\n", result.YearCount, result.NetworkCount)
	}
	return result, nil
}

// Schedules the next hour's run.
func RebaseQueuedGiftYearsPost(
	args *RebaseQueuedGiftYearsArgs,
	result *RebaseQueuedGiftYearsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRebaseQueuedGiftYears(clientSession, tx)
	return nil
}
