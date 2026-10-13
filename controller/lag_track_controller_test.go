package controller

import (
	"context"
	"crypto/ed25519"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Tests of the track tickets (lag.dev/PLAN.md A6, T2) and their reissue,
// against extenders holding live measurement certificates.

// Decodes and verifies a ticket text under the root key, as an extender does
// (M3).
func verifyLagTestTicket(t testing.TB, ticketText string, rootPublicKey ed25519.PublicKey, now time.Time) *protocol.LagTicketBody {
	t.Helper()
	ticket, err := connect.DecodeLagTicket(ticketText)
	if err != nil {
		t.Fatal(err)
	}
	body, err := connect.VerifyLagTicket(ticket, []ed25519.PublicKey{rootPublicKey}, now)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// A track's tickets are signed by the root key under the ticket domain, carry
// the plan's caps, the track's id and each dot's place in the request, and
// are stored for the reports. An extender that is not measurable is left
// out, and a repeated one gets one ticket.
func TestLagTrack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootPublicKey := installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		first := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1", "2001:db8:a::1")
		second := newLagServingTestExtender(t, ctx, places, lagTestPlace("Osaka", "jp", 34.7, 135.5), "192.0.2.2")
		unmeasurable := newLagTestExtender(t, ctx, "192.0.2.3")
		places["192.0.2.3"] = lagTestPlace("Lyon", "fr", 45.7, 4.8)

		clientSession := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")
		now := server.NowUtc().Truncate(time.Millisecond)
		result, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
			{ExtenderId: first.extenderId},
			{ExtenderId: unmeasurable.extenderId},
			{ExtenderId: server.NewId()},
			{ExtenderId: second.extenderId},
			{ExtenderId: first.extenderId},
		}}, clientSession, now, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*LagError)(nil))
		connect.AssertNotEqual(t, result.TrackId, (*server.Id)(nil))
		expiresAt, err := time.Parse(time.RFC3339, result.ExpiresAt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, expiresAt.Equal(now.Add(2*time.Minute).Truncate(time.Second)), true)

		connect.AssertEqual(t, len(result.Tickets), 2)
		for i, want := range []struct {
			extender *lagTestExtender
			position int
		}{
			{extender: first, position: 0},
			{extender: second, position: 3},
		} {
			ticket := result.Tickets[i]
			connect.AssertEqual(t, ticket.ExtenderId, want.extender.extenderId)
			connect.AssertEqual(t, ticket.Position, want.position)
			connect.AssertEqual(t, ticket.Host, want.extender.extenderId.String()+"."+lagTestMeasurementZone)

			body := verifyLagTestTicket(t, ticket.Ticket, rootPublicKey, now)
			connect.AssertEqual(t, len(body.TicketId), 16)
			connect.AssertEqual(t, body.ClientId, clientSession.ByJwt.ClientId.Bytes())
			connect.AssertEqual(t, body.ExtenderPublicKey, []byte(want.extender.publicKey))
			connect.AssertEqual(t, body.NetworkHost, lagTestNetworkHost)
			connect.AssertEqual(t, body.IssueTimeMs, uint64(now.UnixMilli()))
			connect.AssertEqual(t, body.ExpireTimeMs, uint64(expiresAt.UnixMilli()))
			connect.AssertEqual(t, body.TrackId, result.TrackId.Bytes())
			connect.AssertEqual(t, body.TrackPosition, uint32(want.position))
			connect.AssertEqual(t, body.PingIntervalMs, uint32(250))
			connect.AssertEqual(t, body.DownloadMs, uint32(5000))
			connect.AssertEqual(t, body.UploadMs, uint32(5000))
			connect.AssertEqual(t, body.MaxStreams, uint32(4))
			connect.AssertEqual(t, body.MaxDownloadBytes, uint64(2*1024*1024*1024))
			connect.AssertEqual(t, body.MaxUploadBytes, uint64(2*1024*1024*1024))

			// stored for the report
			ticketId := server.RequireIdFromBytes(body.TicketId)
			stored := model.GetLagTicket(ctx, ticketId)
			connect.AssertEqual(t, stored.TrackId, *result.TrackId)
			connect.AssertEqual(t, stored.Position, want.position)
			connect.AssertEqual(t, stored.ExtenderId, want.extender.extenderId)
			connect.AssertEqual(t, stored.ClientId, *clientSession.ByJwt.ClientId)
			connect.AssertEqual(t, stored.NetworkId, clientSession.ByJwt.NetworkId)
			connect.AssertEqual(t, stored.ExpiresAt.Equal(expiresAt), true)
		}
		track := model.GetLagTrack(ctx, *result.TrackId)
		connect.AssertEqual(t, track.ClientId, *clientSession.ByJwt.ClientId)
		connect.AssertEqual(t, track.StartedAt.Equal(now), true)

		// a ticket expires two minutes out, and under no other key
		ticket, err := connect.DecodeLagTicket(result.Tickets[0].Ticket)
		connect.AssertEqual(t, err, nil)
		_, err = connect.VerifyLagTicket(ticket, []ed25519.PublicKey{rootPublicKey}, expiresAt)
		connect.AssertNotEqual(t, err, nil)
		otherPublicKey, _, _ := ed25519.GenerateKey(nil)
		_, err = connect.VerifyLagTicket(ticket, []ed25519.PublicKey{otherPublicKey}, now)
		connect.AssertNotEqual(t, err, nil)

		// no extender that can be measured is unavailable
		result, err = lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
			{ExtenderId: unmeasurable.extenderId},
		}}, clientSession, now, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Code, LagErrorCodeUnavailable)
		connect.AssertEqual(t, result.TrackId, (*server.Id)(nil))

		// a track names 1 to 12 extenders
		for _, count := range []int{0, 13} {
			extenders := []*LagTrackExtenderArgs{}
			for range count {
				extenders = append(extenders, &LagTrackExtenderArgs{ExtenderId: first.extenderId})
			}
			_, err := lagTrack(&LagTrackArgs{Extenders: extenders}, clientSession, now, settings)
			if err == nil || !strings.HasPrefix(err.Error(), "400 ") {
				t.Fatalf("a track of %d answered %v, want a 400", count, err)
			}
		}
	})
}

// Tracks are limited per client and per /24 (/48), with the rest of the
// window to wait.
func TestLagTrackLimits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		settings.TrackClientLimit = 2
		settings.TrackPrefixLimit = 3
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		extender := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1")
		args := &LagTrackArgs{Extenders: []*LagTrackExtenderArgs{{ExtenderId: extender.extenderId}}}
		track := func(clientSession *session.ClientSession) *LagTrackResult {
			result, err := lagTrack(args, clientSession, server.NowUtc(), settings)
			connect.AssertEqual(t, err, nil)
			return result
		}
		assertLimited := func(result *LagTrackResult) {
			t.Helper()
			if result.Error == nil || result.Error.Code != LagErrorCodeLimit {
				t.Fatalf("want a limit refusal, got %+v", result)
			}
			if result.Error.RetryAfterMs <= 0 || settings.TrackLimitWindow.Milliseconds() < result.Error.RetryAfterMs {
				t.Fatalf("retry after %d ms, want within the window", result.Error.RetryAfterMs)
			}
		}

		// per client
		guest := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")
		connect.AssertEqual(t, track(guest).Error, (*LagError)(nil))
		connect.AssertEqual(t, track(guest).Error, (*LagError)(nil))
		assertLimited(track(guest))

		// per /24: the guest's two and its refusal were not all counted
		// there; a second client in the block has one left
		other := newLagTestGuestSession(t, ctx, "198.51.100.99:1234")
		connect.AssertEqual(t, track(other).Error, (*LagError)(nil))
		assertLimited(track(other))
		// and another block is its own
		connect.AssertEqual(t, track(newLagTestGuestSession(t, ctx, "198.51.101.1:1234")).Error, (*LagError)(nil))
	})
}

// A dropped dot reconnects within its track: the track's id with the dot's
// extender and position reissues a ticket in that track at that position,
// outside the track limits and up to the per-track cap, for the client the
// track was issued to within the hour. A report under either ticket of the
// dot is accepted.
func TestLagTrackReissue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootPublicKey := installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		settings.TrackClientLimit = 1
		settings.TrackPrefixLimit = 1
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		useIpAsnDb(t, "")
		first := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1")
		second := newLagServingTestExtender(t, ctx, places, lagTestPlace("Osaka", "jp", 34.7, 135.5), "192.0.2.2")

		guest := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")
		issuedAt := server.NowUtc().Truncate(time.Millisecond)
		issued, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
			{ExtenderId: first.extenderId},
			{ExtenderId: second.extenderId},
		}}, guest, issuedAt, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, issued.Error, (*LagError)(nil))
		trackId := *issued.TrackId
		// the guest's one track this hour is spent
		spent, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{{ExtenderId: first.extenderId}}}, guest, issuedAt, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, spent.Error.Code, LagErrorCodeLimit)

		position := func(position int) *int {
			return &position
		}
		reissue := func(clientSession *session.ClientSession, now time.Time, trackId server.Id, extenders ...*LagTrackExtenderArgs) *LagTrackResult {
			t.Helper()
			result, err := lagTrack(&LagTrackArgs{TrackId: &trackId, Extenders: extenders}, clientSession, now, settings)
			connect.AssertEqual(t, err, nil)
			return result
		}

		// the second dot dropped, five minutes in
		now := issuedAt.Add(5 * time.Minute)
		result := reissue(guest, now, trackId, &LagTrackExtenderArgs{ExtenderId: second.extenderId, Position: position(1)})
		connect.AssertEqual(t, result.Error, (*LagError)(nil))
		connect.AssertEqual(t, *result.TrackId, trackId)
		connect.AssertEqual(t, len(result.Tickets), 1)
		connect.AssertEqual(t, result.Tickets[0].ExtenderId, second.extenderId)
		connect.AssertEqual(t, result.Tickets[0].Position, 1)
		connect.AssertEqual(t, result.Tickets[0].Host, second.extenderId.String()+"."+lagTestMeasurementZone)
		body := verifyLagTestTicket(t, result.Tickets[0].Ticket, rootPublicKey, now)
		connect.AssertEqual(t, body.TrackId, trackId.Bytes())
		connect.AssertEqual(t, body.TrackPosition, uint32(1))
		connect.AssertEqual(t, body.IssueTimeMs, uint64(now.UnixMilli()))
		reissuedTicketId := server.RequireIdFromBytes(body.TicketId)
		originalTicketId := server.RequireIdFromBytes(verifyLagTestTicket(t, issued.Tickets[1].Ticket, rootPublicKey, issuedAt).TicketId)
		connect.AssertNotEqual(t, reissuedTicketId, originalTicketId)
		connect.AssertEqual(t, len(model.GetLagTrackTickets(ctx, trackId)), 3)
		connect.AssertEqual(t, model.GetLagTrack(ctx, trackId).ReissueCount, 1)

		// a report under the reissued ticket and one under the dropped session's
		// ticket are both the dot's, at its place in the track
		for _, ticketId := range []server.Id{reissuedTicketId, originalTicketId} {
			reportBody := newLagTestReportBody(second, model.GetLagTicket(ctx, ticketId), now.Add(-time.Minute))
			reported := postLagTestReports(t, second, now, signLagTestReport(t, reportBody, second.privateKey))
			connect.AssertEqual(t, reported.Accepted, 1)
		}
		var reportCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM lag_result WHERE track_id = $1 AND track_position = 1`, trackId).Scan(&reportCount))
		})
		connect.AssertEqual(t, reportCount, 2)

		// a dot the track does not hold, at another place or another extender
		for _, extenderArgs := range []*LagTrackExtenderArgs{
			{ExtenderId: second.extenderId, Position: position(0)},
			{ExtenderId: server.NewId(), Position: position(1)},
		} {
			connect.AssertEqual(t, reissue(guest, now, trackId, extenderArgs).Error.Code, LagErrorCodeUnavailable)
		}
		// one bad dot refuses the whole reissue
		connect.AssertEqual(t, reissue(guest, now, trackId,
			&LagTrackExtenderArgs{ExtenderId: first.extenderId, Position: position(0)},
			&LagTrackExtenderArgs{ExtenderId: first.extenderId, Position: position(5)},
		).Error.Code, LagErrorCodeUnavailable)
		// an unknown track, another client's, and a track past the hour
		connect.AssertEqual(t, reissue(guest, now, server.NewId(), &LagTrackExtenderArgs{ExtenderId: first.extenderId, Position: position(0)}).Error.Code, LagErrorCodeUnavailable)
		stranger := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")
		connect.AssertEqual(t, reissue(stranger, now, trackId, &LagTrackExtenderArgs{ExtenderId: first.extenderId, Position: position(0)}).Error.Code, LagErrorCodeUnavailable)
		connect.AssertEqual(t, reissue(guest, issuedAt.Add(time.Hour+time.Second), trackId, &LagTrackExtenderArgs{ExtenderId: first.extenderId, Position: position(0)}).Error.Code, LagErrorCodeUnavailable)
		// nothing above reissued anything
		connect.AssertEqual(t, model.GetLagTrack(ctx, trackId).ReissueCount, 1)

		// a reissue names each extender with its position
		_, err = lagTrack(&LagTrackArgs{TrackId: &trackId, Extenders: []*LagTrackExtenderArgs{{ExtenderId: first.extenderId}}}, guest, now, settings)
		if err == nil || !strings.HasPrefix(err.Error(), "400 ") {
			t.Fatalf("a reissue without a position answered %v, want a 400", err)
		}

		// one reissue per position: the second dot reconnected already, so
		// another reissue of it is refused, alone or beside another dot, and
		// stores nothing
		connect.AssertEqual(t, reissue(guest, now, trackId, &LagTrackExtenderArgs{ExtenderId: second.extenderId, Position: position(1)}).Error.Code, LagErrorCodeLimit)
		connect.AssertEqual(t, reissue(guest, now, trackId,
			&LagTrackExtenderArgs{ExtenderId: first.extenderId, Position: position(0)},
			&LagTrackExtenderArgs{ExtenderId: second.extenderId, Position: position(1)},
		).Error.Code, LagErrorCodeLimit)
		connect.AssertEqual(t, model.GetLagTrack(ctx, trackId).ReissueCount, 1)
		connect.AssertEqual(t, len(model.GetLagTrackTickets(ctx, trackId)), 3)
		// the first dot still has its one
		connect.AssertEqual(t, reissue(guest, now, trackId, &LagTrackExtenderArgs{ExtenderId: first.extenderId, Position: position(0)}).Error, (*LagError)(nil))
		connect.AssertEqual(t, reissue(guest, now, trackId, &LagTrackExtenderArgs{ExtenderId: first.extenderId, Position: position(0)}).Error.Code, LagErrorCodeLimit)
		connect.AssertEqual(t, model.GetLagTrack(ctx, trackId).ReissueCount, 2)
		connect.AssertEqual(t, len(model.GetLagTrackTickets(ctx, trackId)), 4)
	})
}

// A track gets at most track_reissue_max reissued tickets in all, however
// many positions it has, and a call that would pass the cap is refused whole.
func TestLagTrackReissueCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		settings.TrackReissueMax = 2
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		extenders := []*lagTestExtender{}
		for i := range 3 {
			extenders = append(extenders, newLagServingTestExtender(t, ctx, places, lagTestPlace("City", "de", 50, float64(i)), "192.0.2."+string(rune('1'+i))))
		}

		guest := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")
		now := server.NowUtc()
		issued, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
			{ExtenderId: extenders[0].extenderId},
			{ExtenderId: extenders[1].extenderId},
			{ExtenderId: extenders[2].extenderId},
		}}, guest, now, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(issued.Tickets), 3)
		trackId := *issued.TrackId
		reissue := func(positions ...int) *LagTrackResult {
			t.Helper()
			args := &LagTrackArgs{TrackId: &trackId}
			for _, position := range positions {
				args.Extenders = append(args.Extenders, &LagTrackExtenderArgs{ExtenderId: extenders[position].extenderId, Position: &position})
			}
			result, err := lagTrack(args, guest, now, settings)
			connect.AssertEqual(t, err, nil)
			return result
		}

		// three dots would pass the cap of two: refused whole
		connect.AssertEqual(t, reissue(0, 1, 2).Error.Code, LagErrorCodeLimit)
		connect.AssertEqual(t, model.GetLagTrack(ctx, trackId).ReissueCount, 0)
		// two in one call fill it
		result := reissue(0, 1)
		connect.AssertEqual(t, result.Error, (*LagError)(nil))
		connect.AssertEqual(t, len(result.Tickets), 2)
		// and the third dot finds it full
		connect.AssertEqual(t, reissue(2).Error.Code, LagErrorCodeLimit)
		connect.AssertEqual(t, model.GetLagTrack(ctx, trackId).ReissueCount, 2)
		connect.AssertEqual(t, len(model.GetLagTrackTickets(ctx, trackId)), 5)
	})
}

// Only an extender whose certificate is issued, unexpired and not withdrawn
// is offered and ticketed: one withdrawn, one its owner opted out (C9,
// decision 18), one whose request went back to pending, and one with a
// certificate but no request to answer are not, whatever certificate they
// still hold. The change applies at once, through the cached serving set.
func TestLagExtendersOnlyIssued(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)

		issued := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1")
		withdrawn := newLagTestExtender(t, ctx, "192.0.2.2")
		places["192.0.2.2"] = lagTestPlace("Osaka", "jp", 34.7, 135.5)
		withdrawnCert := addLagTestCert(t, ctx, withdrawn)
		pending := newLagServingTestExtender(t, ctx, places, lagTestPlace("Lyon", "fr", 45.7, 4.8), "192.0.2.3")
		optedOut := newLagServingTestExtender(t, ctx, places, lagTestPlace("Quito", "ec", -0.2, -78.5), "192.0.2.5")
		unrequested := newLagTestExtender(t, ctx, "192.0.2.4")
		places["192.0.2.4"] = lagTestPlace("Lima", "pe", -12.0, -77.0)

		guest := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")
		served := func(reset bool) []server.Id {
			t.Helper()
			if reset {
				Testing_ResetLagServing()
			}
			result, err := LagExtenders(guest)
			connect.AssertEqual(t, err, nil)
			extenderIds := []server.Id{}
			for _, extender := range result.Extenders {
				extenderIds = append(extenderIds, extender.ExtenderId)
			}
			return extenderIds
		}
		ticketed := func(reset bool) []server.Id {
			t.Helper()
			if reset {
				Testing_ResetLagServing()
			}
			result, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
				{ExtenderId: issued.extenderId},
				{ExtenderId: withdrawn.extenderId},
				{ExtenderId: pending.extenderId},
				{ExtenderId: unrequested.extenderId},
				{ExtenderId: optedOut.extenderId},
			}}, guest, server.NowUtc(), settings)
			connect.AssertEqual(t, err, nil)
			extenderIds := []server.Id{}
			for _, ticket := range result.Tickets {
				extenderIds = append(extenderIds, ticket.ExtenderId)
			}
			return extenderIds
		}
		sameIds := func(got []server.Id, want ...*lagTestExtender) {
			t.Helper()
			wantIds := map[server.Id]bool{}
			for _, extender := range want {
				wantIds[extender.extenderId] = true
			}
			gotIds := map[server.Id]bool{}
			for _, extenderId := range got {
				gotIds[extenderId] = true
			}
			connect.AssertEqual(t, gotIds, wantIds)
		}

		// before anything changes, the four with an issued certificate serve
		sameIds(served(true), issued, withdrawn, pending, optedOut)
		sameIds(ticketed(false), issued, withdrawn, pending, optedOut)

		// a certificate with no request to answer, its name published: a
		// request names its key
		unrequestedNames := []string{ExtenderMeasurementName(unrequested.extenderId, lagTestMeasurementZone)}
		model.AddNetworkExtenderMeasurementCert(ctx, &model.NetworkExtenderMeasurementCert{
			CertId:     server.NewId(),
			ExtenderId: unrequested.extenderId,
			Names:      unrequestedNames,
			Csr:        []byte{1},
			PublicKey:  []byte{2},
			ChainPem:   "-",
			SerialHex:  "01",
			NotBefore:  server.NowUtc().Add(-time.Hour),
			NotAfter:   server.NowUtc().Add(60 * 24 * time.Hour),
			IssueTime:  server.NowUtc().Add(-time.Hour),
			CapCounted: true,
			RenewTime:  server.NowUtc().Add(40 * 24 * time.Hour),
		})
		model.PublishNetworkExtenderMeasurementNames(ctx, unrequestedNames, server.NowUtc())
		// withdrawn, as an owner's opt-out or a deactivation withdraws it
		connect.AssertEqual(t, model.WithdrawNetworkExtenderMeasurementCert(ctx, withdrawnCert.CertId, "the owner opted out of measurement", server.NowUtc()), true)
		// a new key's request waits, while the old key's certificate is live
		model.SetNetworkExtenderMeasurementRequest(
			ctx,
			pending.extenderId,
			[]byte{3},
			server.NewId().Bytes(),
			[]string{ExtenderMeasurementName(pending.extenderId, lagTestMeasurementZone)},
			server.NowUtc(),
		)
		// its owner opted out, at its next activation
		connect.AssertEqual(t, model.OptOutNetworkExtenderMeasurement(ctx, optedOut.publicKey, "the owner opted out of measurement", server.NowUtc()), 1)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, pending.extenderId).State, model.NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, withdrawn.extenderId).State, model.NetworkExtenderMeasurementStateWithdrawn)

		// at once, while the serving set read before the changes is cached
		sameIds(served(false), issued)
		sameIds(ticketed(false), issued)
		// and when it is read again
		sameIds(served(true), issued)
		sameIds(ticketed(true), issued)
	})
}

// An extender whose certificate is issued but whose name the dns reconcile
// has not yet published is neither offered nor ticketed: a browser that
// looked the name up before it existed would cache that it does not
// (lag.dev/PLAN.md A4, C8). Once published, it is both.
func TestLagExtendersOnlyPublished(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)

		published := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1")
		unpublished := newLagTestExtender(t, ctx, "192.0.2.2")
		places["192.0.2.2"] = lagTestPlace("Osaka", "jp", 34.7, 135.5)
		cert := addLagTestUnpublishedCert(t, ctx, unpublished)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, unpublished.extenderId).State, model.NetworkExtenderMeasurementStateIssued)

		guest := newLagTestGuestSession(t, ctx, "198.51.100.30:1234")
		offered := func() []server.Id {
			t.Helper()
			Testing_ResetLagServing()
			result, err := LagExtenders(guest)
			connect.AssertEqual(t, err, nil)
			extenderIds := []server.Id{}
			for _, extender := range result.Extenders {
				extenderIds = append(extenderIds, extender.ExtenderId)
			}
			return extenderIds
		}
		ticketed := func() []server.Id {
			t.Helper()
			Testing_ResetLagServing()
			result, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
				{ExtenderId: published.extenderId},
				{ExtenderId: unpublished.extenderId},
			}}, guest, server.NowUtc(), settings)
			connect.AssertEqual(t, err, nil)
			extenderIds := []server.Id{}
			for _, ticket := range result.Tickets {
				extenderIds = append(extenderIds, ticket.ExtenderId)
			}
			return extenderIds
		}

		connect.AssertEqual(t, offered(), []server.Id{published.extenderId})
		connect.AssertEqual(t, ticketed(), []server.Id{published.extenderId})

		connect.AssertEqual(t, model.PublishNetworkExtenderMeasurementNames(ctx, cert.Names, server.NowUtc()), 1)
		both := offered()
		slices.SortFunc(both, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		want := []server.Id{published.extenderId, unpublished.extenderId}
		slices.SortFunc(want, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		connect.AssertEqual(t, both, want)
		connect.AssertEqual(t, ticketed(), []server.Id{published.extenderId, unpublished.extenderId})
	})
}
