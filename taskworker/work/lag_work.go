package work

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The lag sweep (lag.dev/PLAN.md A2, A5). Every guest_sweep_interval it
// removes the lag guest networks past their expiry, with their clients, which
// is what ends a guest's jwt while expired tokens are still accepted
// (server/session/AUTH.md §8). Only networks marked as lag guests expire. It
// also applies the retention of what lag.dev stores (A5): tickets after
// ticket_retention, and everything of a track, its results and then the
// track with its client fields, once the track is older than
// result_retention (7 days). The sweep runs every guest_sweep_interval
// (15 minutes), so nothing outlives the retention by more than that.

// Guests removed per batch. Each is removed in its own transaction; batches
// repeat until the backlog is gone or the run's time is up.
const lagSweepGuestBatchSize = 500

// How long one sweep may keep removing. A backlog left over is the next
// sweep's.
const lagSweepTimeout = 5 * time.Minute

type SweepLagArgs struct {
}

type SweepLagResult struct {
	RemovedGuests  int `json:"removed_guests"`
	KeptGuests     int `json:"kept_guests"`
	RemovedTickets int `json:"removed_tickets"`
	RemovedResults int `json:"removed_results"`
	RemovedTracks  int `json:"removed_tracks"`
}

// Schedules the next sweep one sweep interval from now, once per chain.
func ScheduleSweepLag(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		SweepLag,
		&SweepLagArgs{},
		clientSession,
		task.RunOnce("sweep_lag"),
		task.RunAt(server.NowUtc().Add(controller.GetLagSettings().GuestSweepInterval)),
	)
}

// Removes the expired lag guests, and what is past its retention, as the
// clock reads now.
func SweepLag(
	_ *SweepLagArgs,
	clientSession *session.ClientSession,
) (*SweepLagResult, error) {
	result := sweepLag(clientSession.Ctx, server.NowUtc(), controller.GetLagSettings(), lagSweepGuestBatchSize, lagSweepTimeout)
	// what the sweep removed counts by kind (X9)
	controller.CountLagSweep(result.RemovedGuests, result.RemovedTickets, result.RemovedResults, result.RemovedTracks)
	return result, nil
}

// The sweep as the clock reads `now`; a test moves the clock past an expiry.
func sweepLag(
	ctx context.Context,
	now time.Time,
	settings *controller.LagSettings,
	batchSize int,
	timeout time.Duration,
) *SweepLagResult {
	result := &SweepLagResult{}
	endTime := time.Now().Add(timeout)
	for {
		removed := model.RemoveExpiredLagGuests(ctx, now, batchSize)
		result.RemovedGuests += removed.Removed
		result.KeptGuests += removed.Kept
		if removed.Removed+removed.Kept < batchSize || endTime.Before(time.Now()) || ctx.Err() != nil {
			break
		}
	}

	// a ticket's report comes when its session's minute closes, and a
	// session has no time limit, so a ticket is kept for its retention
	// rather than to its expiry, but never past its track's
	ticketCut := now.Add(-settings.TicketRetention)
	retainedCut := now.Add(-settings.ResultRetention)
	if ticketCut.Before(retainedCut) {
		ticketCut = retainedCut
	}
	removed := model.RemoveExpiredLagResults(ctx, ticketCut, retainedCut)
	result.RemovedTickets = removed.Tickets
	result.RemovedResults = removed.Results
	result.RemovedTracks = removed.Tracks

	if 0 < result.RemovedGuests || 0 < result.KeptGuests || 0 < removed.Tickets || 0 < removed.Results || 0 < removed.Tracks {
		glog.Infof(
			"[lag]swept %d expired guests (kept %d paid), %d tickets, %d results, %d tracks\n",
			result.RemovedGuests,
			result.KeptGuests,
			removed.Tickets,
			removed.Results,
			removed.Tracks,
		)
	}
	return result
}

// Re-arms the chain in the transaction that finishes this sweep.
func SweepLagPost(
	_ *SweepLagArgs,
	_ *SweepLagResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleSweepLag(clientSession, tx)
	return nil
}
