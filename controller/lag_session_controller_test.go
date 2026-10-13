package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Tests of the lag.dev guest session (lag.dev/PLAN.md A1, A2, T2): what it
// creates, its limit per /24 (/48), and that the sweep ends its jwt.

func lagSessionFrom(t testing.TB, ctx context.Context, clientAddress string) *LagSessionResult {
	t.Helper()
	clientSession := session.NewLocalClientSession(ctx, clientAddress, nil)
	defer clientSession.Cancel()
	result, err := LagSession(clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// A guest session is a lag guest network with one client, whose jwt is an
// ordinary client token that works until the sweep removes the guest at its
// expiry.
func TestLagSession(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetLagSettings(DefaultLagSettings())()

		before := server.NowUtc()
		result := lagSessionFrom(t, ctx, "198.51.100.7:5555")
		after := server.NowUtc()
		if result.Error != nil {
			t.Fatalf("the session was refused: %+v", result.Error)
		}
		connect.AssertNotEqual(t, result.ByClientJwt, "")
		connect.AssertNotEqual(t, result.NetworkId, (*server.Id)(nil))
		connect.AssertNotEqual(t, result.ClientId, (*server.Id)(nil))

		// whole seconds, 24 h out, and the expiry the guest is stored with
		expiresAt, err := time.Parse(time.RFC3339, result.ExpiresAt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, expiresAt.Location(), time.UTC)
		if expiresAt.Before(before.Add(24*time.Hour).Truncate(time.Second)) || after.Add(24*time.Hour).Before(expiresAt) {
			t.Fatalf("expires at %s, want 24 h after %s", expiresAt, before)
		}
		guest := model.GetLagGuest(ctx, *result.NetworkId)
		connect.AssertNotEqual(t, guest, (*model.LagGuest)(nil))
		connect.AssertEqual(t, guest.ExpiresAt.Equal(expiresAt), true)

		// the jwt names the guest's network and its one client, and is live
		byJwt, err := session.ParseByJwt(ctx, result.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, byJwt.NetworkId, *result.NetworkId)
		connect.AssertEqual(t, *byJwt.ClientId, *result.ClientId)
		connect.AssertEqual(t, byJwt.NetworkName, guest.NetworkName)
		connect.AssertEqual(t, session.ValidateByJwtState(ctx, byJwt, true), nil)
		// a lag guest's token, which the api accepts only on the lag routes
		// and the connect transport refuses
		connect.AssertEqual(t, byJwt.LagGuest, true)
		// and the guest has no data to spend
		var balances int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM transfer_balance WHERE network_id = $1`, *result.NetworkId).Scan(&balances))
		})
		connect.AssertEqual(t, balances, 0)

		// a second session is a second guest
		other := lagSessionFrom(t, ctx, "198.51.100.8:5555")
		connect.AssertEqual(t, other.Error, (*LagError)(nil))
		connect.AssertNotEqual(t, *other.NetworkId, *result.NetworkId)

		// the sweep removes the guests at their expiry, the second session's a
		// moment after the first's, and the jwt stops working
		removed := model.RemoveExpiredLagGuests(ctx, expiresAt.Add(-time.Second), 100)
		connect.AssertEqual(t, removed.Removed, 0)
		connect.AssertEqual(t, session.ValidateByJwtState(ctx, byJwt, true), nil)
		removed = model.RemoveExpiredLagGuests(ctx, expiresAt.Add(time.Minute), 100)
		connect.AssertEqual(t, removed.Removed, 2)
		connect.AssertNotEqual(t, session.ValidateByJwtState(ctx, byJwt, true), nil)
	})
}

// With session creation on, the client token is tagged with the session its
// guest network minted, and is live.
func TestLagSessionWithSessions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetLagSettings(DefaultLagSettings())()
		defer session.Testing_SetSessionCreationEnabled(true)()

		result := lagSessionFrom(t, ctx, "198.51.100.17:5555")
		if result.Error != nil {
			t.Fatalf("the session was refused: %+v", result.Error)
		}
		byJwt, err := session.ParseByJwt(ctx, result.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, byJwt.SessionId, (*server.Id)(nil))
		connect.AssertNotEqual(t, byJwt.RootClientId, (*server.Id)(nil))
		connect.AssertEqual(t, session.ValidateByJwtState(ctx, byJwt, true), nil)
	})
}

// Creation is limited per /24 (/48) of the caller: past the limit a caller
// anywhere in the block is refused with the rest of the window, and a caller
// in another block is not.
func TestLagSessionLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultLagSettings()
		settings.GuestLimit = 2
		defer Testing_SetLagSettings(settings)()

		connect.AssertEqual(t, lagSessionFrom(t, ctx, "203.0.113.1:1000").Error, (*LagError)(nil))
		connect.AssertEqual(t, lagSessionFrom(t, ctx, "203.0.113.250:1000").Error, (*LagError)(nil))
		refused := lagSessionFrom(t, ctx, "203.0.113.99:1000")
		connect.AssertNotEqual(t, refused.Error, (*LagError)(nil))
		connect.AssertEqual(t, refused.Error.Code, LagErrorCodeLimit)
		if refused.Error.RetryAfterMs <= 0 || settings.GuestLimitWindow.Milliseconds() < refused.Error.RetryAfterMs {
			t.Fatalf("retry after %d ms, want within the window", refused.Error.RetryAfterMs)
		}
		connect.AssertEqual(t, refused.ByClientJwt, "")
		connect.AssertEqual(t, refused.NetworkId, (*server.Id)(nil))

		// another /24, and a /48
		connect.AssertEqual(t, lagSessionFrom(t, ctx, "203.0.114.1:1000").Error, (*LagError)(nil))
		connect.AssertEqual(t, lagSessionFrom(t, ctx, "[2001:db8:5:1::1]:1000").Error, (*LagError)(nil))
		connect.AssertEqual(t, lagSessionFrom(t, ctx, "[2001:db8:5:ffff::2]:1000").Error, (*LagError)(nil))
		refused = lagSessionFrom(t, ctx, "[2001:db8:5::3]:1000")
		connect.AssertNotEqual(t, refused.Error, (*LagError)(nil))
		connect.AssertEqual(t, refused.Error.Code, LagErrorCodeLimit)

		// a refusal created nothing
		var guests int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM lag_guest`).Scan(&guests))
		})
		connect.AssertEqual(t, guests, 5)
	})
}

// The settings default to the plan's values, and a resource value that is
// not positive keeps its default. Pure.
func TestLagSettingsDefaults(t *testing.T) {
	settings := DefaultLagSettings()
	connect.AssertEqual(t, settings.GuestTtl, 24*time.Hour)
	connect.AssertEqual(t, settings.GuestLimit, 10)
	connect.AssertEqual(t, settings.GuestLimitWindow, time.Hour)
	connect.AssertEqual(t, settings.ExtenderSample, 64)
	connect.AssertEqual(t, settings.TrackClientLimit, 12)
	connect.AssertEqual(t, settings.TrackPrefixLimit, 60)
	connect.AssertEqual(t, settings.TrackLimitWindow, time.Hour)
	connect.AssertEqual(t, settings.TrackMaxExtenders, 12)
	connect.AssertEqual(t, settings.TicketTtl, 2*time.Minute)
	connect.AssertEqual(t, settings.PingIntervalMs, 250)
	connect.AssertEqual(t, settings.DownloadMs, 5000)
	connect.AssertEqual(t, settings.UploadMs, 5000)
	connect.AssertEqual(t, settings.MaxStreams, 4)
	connect.AssertEqual(t, settings.MaxDownloadBytes, int64(2*1024*1024*1024))
	connect.AssertEqual(t, settings.MaxUploadBytes, int64(2*1024*1024*1024))
	connect.AssertEqual(t, settings.ReportMaxCount, 32)
	connect.AssertEqual(t, settings.ResultRetention, 7*24*time.Hour)

	settings = (&LagSettings{GuestLimit: -1, TicketTtl: time.Minute}).withDefaults()
	connect.AssertEqual(t, settings.GuestLimit, 10)
	connect.AssertEqual(t, settings.TicketTtl, time.Minute)
	connect.AssertEqual(t, settings.ResultRetention, 7*24*time.Hour)
}

// lag.yml is read over the defaults: the keys it sets, the defaults for the
// rest, and the defaults whole for an unreadable resource.
func TestLagSettingsResource(t *testing.T) {
	read := func(body string) *LagSettings {
		pop := server.Vault.PushSimpleResource("lag.yml", []byte(body))
		defer pop()
		return readLagSettings()
	}
	settings := read("guest_ttl: 12h\ntrack_client_limit: 3\nresult_retention: 720h\nmax_streams: 0\n")
	connect.AssertEqual(t, settings.GuestTtl, 12*time.Hour)
	connect.AssertEqual(t, settings.TrackClientLimit, 3)
	connect.AssertEqual(t, settings.ResultRetention, 30*24*time.Hour)
	connect.AssertEqual(t, settings.MaxStreams, 4)
	connect.AssertEqual(t, settings.GuestLimit, 10)

	connect.AssertEqual(t, read("guest_ttl: [not, a, duration]\n"), DefaultLagSettings())
}
