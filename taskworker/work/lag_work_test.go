package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The lag sweep (lag.dev/PLAN.md A2, A5): the guests it removes, the
// retention it applies, and its chain.

func testLagSweepCount(t testing.TB, ctx context.Context, table string) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count))
	})
	return
}

// One sweep removes every guest past its expiry, however many batches that
// takes, and leaves the guests still inside it.
func TestSweepLagRemovesExpiredGuests(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := controller.DefaultLagSettings()
		now := server.NowUtc().Truncate(time.Second)
		for range 5 {
			model.CreateLagGuestNetwork(ctx, now, now.Add(settings.GuestTtl))
		}
		live := model.CreateLagGuestNetwork(ctx, now.Add(time.Hour), now.Add(time.Hour).Add(settings.GuestTtl))

		// a batch of 2, so 5 expired guests take three batches of one sweep
		result := sweepLag(ctx, now.Add(settings.GuestTtl), settings, 2, time.Minute)
		connect.AssertEqual(t, result.RemovedGuests, 5)
		connect.AssertEqual(t, result.KeptGuests, 0)
		connect.AssertEqual(t, testLagSweepCount(t, ctx, "lag_guest"), 1)
		connect.AssertNotEqual(t, model.GetLagGuest(ctx, live.NetworkId), (*model.LagGuest)(nil))

		// nothing more is due until the last guest's own expiry
		result = sweepLag(ctx, now.Add(settings.GuestTtl).Add(30*time.Minute), settings, 2, time.Minute)
		connect.AssertEqual(t, result.RemovedGuests, 0)
		result = sweepLag(ctx, now.Add(settings.GuestTtl).Add(time.Hour), settings, 2, time.Minute)
		connect.AssertEqual(t, result.RemovedGuests, 1)
		connect.AssertEqual(t, testLagSweepCount(t, ctx, "lag_guest"), 0)
	})
}

// The sweep keeps a ticket for the ticket retention, 48 h, and everything of
// a track for the result retention, 7 days: just under it the track and its
// results stay, and just past it they go together, a late result included.
func TestSweepLagAppliesRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := controller.DefaultLagSettings()
		connect.AssertEqual(t, settings.ResultRetention, 7*24*time.Hour)
		connect.AssertEqual(t, settings.TicketRetention, 48*time.Hour)
		connect.AssertEqual(t, settings.GuestSweepInterval, 15*time.Minute)
		now := server.NowUtc().Truncate(time.Second)

		track := &model.LagTrack{
			TrackId:   server.NewId(),
			NetworkId: server.NewId(),
			ClientId:  server.NewId(),
			StartedAt: now,
		}
		tickets := []*model.LagTicket{}
		for position := range 2 {
			tickets = append(tickets, &model.LagTicket{
				TicketId:   server.NewId(),
				TrackId:    track.TrackId,
				Position:   position,
				ClientId:   track.ClientId,
				NetworkId:  track.NetworkId,
				ExtenderId: server.NewId(),
				IssuedAt:   now,
				ExpiresAt:  now.Add(settings.TicketTtl),
			})
		}
		model.CreateLagTrack(ctx, track, tickets)
		// a report as its minute closed, and one that came a day late
		for i, createdAt := range []time.Time{now.Add(time.Minute), now.Add(24 * time.Hour)} {
			connect.AssertEqual(t, model.AddLagResult(ctx, &model.LagResult{
				LagResultId:      server.NewId(),
				TrackId:          track.TrackId,
				TrackPosition:    i,
				TicketId:         tickets[i].TicketId,
				ExtenderId:       tickets[i].ExtenderId,
				ExtenderIpFamily: 4,
				WindowStartTime:  now,
				EndReason:        "no_load",
				CreatedAt:        createdAt,
			}), true)
		}

		counts := func() [3]int {
			return [3]int{
				testLagSweepCount(t, ctx, "lag_ticket"),
				testLagSweepCount(t, ctx, "lag_result"),
				testLagSweepCount(t, ctx, "lag_track"),
			}
		}
		sweepAt := func(at time.Time) *SweepLagResult {
			return sweepLag(ctx, at, settings, 100, time.Minute)
		}

		// inside every retention nothing goes
		sweepAt(now.Add(settings.TicketRetention - time.Minute))
		connect.AssertEqual(t, counts(), [3]int{2, 2, 1})
		// the tickets go first
		result := sweepAt(now.Add(settings.TicketRetention + time.Minute))
		connect.AssertEqual(t, result.RemovedTickets, 2)
		connect.AssertEqual(t, counts(), [3]int{0, 2, 1})
		// just under 7 days the track and its results stay
		sweepAt(now.Add(settings.ResultRetention - time.Minute))
		connect.AssertEqual(t, counts(), [3]int{0, 2, 1})
		// just past, they go together, the late result too
		result = sweepAt(now.Add(settings.ResultRetention + time.Minute))
		connect.AssertEqual(t, result.RemovedResults, 2)
		connect.AssertEqual(t, result.RemovedTracks, 1)
		connect.AssertEqual(t, counts(), [3]int{0, 0, 0})
	})
}

// The sweep re-arms itself one sweep interval out, once.
func TestSweepLagChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		result, err := SweepLag(&SweepLagArgs{}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *result, SweepLagResult{})

		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				connect.AssertEqual(t, SweepLagPost(&SweepLagArgs{}, result, clientSession, tx), nil)
			})
		}
		var count int
		var runAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT COUNT(*), MIN(run_at) FROM pending_task WHERE run_once_key = '["sweep_lag"]'`,
			).Scan(&count, &runAt))
		})
		connect.AssertEqual(t, count, 1)
		interval := controller.GetLagSettings().GuestSweepInterval
		if until := runAt.Sub(server.NowUtc()); until <= 0 || interval < until {
			t.Fatalf("the next sweep is %s out, want within %s", until, interval)
		}
	})
}
