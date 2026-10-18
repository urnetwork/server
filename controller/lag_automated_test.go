package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A track is automated for a signing agent before anything else, then for a
// hosting network, and otherwise is a visitor's (amendment 12).
func TestLagTrackAutomatedReason(t *testing.T) {
	asn := func(number int64) *int64 {
		return &number
	}
	signed := http.Header{}
	signed.Set("Signature-Agent", `"https://chatgpt.com"`)
	blank := http.Header{}
	blank.Set("Signature-Agent", "   ")

	for _, c := range []struct {
		name   string
		header http.Header
		asn    *int64
		want   string
	}{
		{"a signing agent", signed, nil, model.LagAutomatedReasonSignatureAgent},
		{"a signing agent in a cloud", signed, asn(16509), model.LagAutomatedReasonSignatureAgent},
		{"AWS", http.Header{}, asn(16509), model.LagAutomatedReasonDatacenterAsn},
		{"Google, where its renderer runs", http.Header{}, asn(15169), model.LagAutomatedReasonDatacenterAsn},
		{"Hetzner", http.Header{}, asn(24940), model.LagAutomatedReasonDatacenterAsn},
		{"Cloudflare, WARP included", http.Header{}, asn(13335), model.LagAutomatedReasonDatacenterAsn},
		{"a residential network", http.Header{}, asn(7922), ""},
		{"Google Fiber, an access network", http.Header{}, asn(16591), ""},
		{"no network known", http.Header{}, nil, ""},
		{"an empty Signature-Agent", blank, asn(7922), ""},
	} {
		connect.AssertEqual(t, lagTrackAutomatedReason(c.header, c.asn), c.want)
	}
}

// Without the GeoLite2 ASN database no track has a network, so none from a
// hosting or cloud network is flagged automated, and nothing else would show
// it: the first track issued so logs it, loudly, once per process
// (amendment 12). A database that does not know an address is no gap, and
// logs nothing.
func TestLagTrackLogsAMissingAsnDatabase(t *testing.T) {
	logged := []string{}
	previousLogf := lagAsnDatabaseMissingLogf
	lagAsnDatabaseMissingLogf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() {
		lagAsnDatabaseMissingLogf = previousLogf
	})
	previousLogged := lagAsnDatabaseMissingLogged.Load()
	t.Cleanup(func() {
		lagAsnDatabaseMissingLogged.Store(previousLogged)
	})
	lagAsnDatabaseMissingLogged.Store(false)

	// the database is there and does not know the address
	useIpAsnDb(t, writeTestIpAsnDb(t, geoLite2AsnDatabaseType, map[string]IpAsn{
		"192.0.2.0/24": {Number: 64500, Organization: "Example"},
	}))
	connect.AssertEqual(t, lagTrackAutomatedReason(http.Header{}, nil), "")
	connect.AssertEqual(t, len(logged), 0)

	// no database: one line however many tracks, naming the file and the
	// flag that goes unset
	useIpAsnDb(t, "")
	for range 3 {
		connect.AssertEqual(t, lagTrackAutomatedReason(http.Header{}, nil), "")
	}
	connect.AssertEqual(t, len(logged), 1)
	for _, want := range []string{geoLite2AsnResourcePath, model.LagAutomatedReasonDatacenterAsn} {
		if !strings.Contains(logged[0], want) {
			t.Fatalf("the missing database is logged as %q, without %q", logged[0], want)
		}
	}
	// a signing agent is flagged without the database, and logs nothing
	signed := http.Header{}
	signed.Set("Signature-Agent", `"https://chatgpt.com"`)
	connect.AssertEqual(t, lagTrackAutomatedReason(signed, nil), model.LagAutomatedReasonSignatureAgent)
	connect.AssertEqual(t, len(logged), 1)
}

// The hosting list names real autonomous systems, each with its network, and
// every reason a track can have is one the stats publish.
func TestLagDatacenterAsns(t *testing.T) {
	if len(lagDatacenterAsns) < 20 {
		t.Errorf("the hosting list has %d networks", len(lagDatacenterAsns))
	}
	for number, name := range lagDatacenterAsns {
		if number <= 0 || 4294967295 < number {
			t.Errorf("AS%d is not an autonomous system number", number)
		}
		if name == "" {
			t.Errorf("AS%d has no network named", number)
		}
	}
	connect.AssertEqual(t, model.LagAutomatedReasons, []string{
		model.LagAutomatedReasonSignatureAgent,
		model.LagAutomatedReasonDatacenterAsn,
	})
}

// The automated gauge publishes every reason, zero included, and folds a
// reason this server does not know into "other".
func TestStatsLagAutomatedValues(t *testing.T) {
	values := statsLagAutomatedValues(map[string]int64{
		model.LagAutomatedReasonDatacenterAsn: 3,
		"later_reason":                        2,
	})
	got := map[string]float64{}
	for _, value := range values {
		got[value.labelValues[0]] = value.value
	}
	connect.AssertEqual(t, got, map[string]float64{
		model.LagAutomatedReasonSignatureAgent: 0,
		model.LagAutomatedReasonDatacenterAsn:  3,
		"other":                                2,
	})
	connect.AssertEqual(t, len(statsLagAutomatedValues(map[string]int64{})), len(model.LagAutomatedReasons))
}

// A claimed agent and a hosting network are conservatively stored as
// automated; a visitor is not. An unverified agent claim cannot impose a
// shared quota on independent authenticated clients.
func TestLagTrackAutomated(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		settings.TrackSignatureLimit = 2
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		useIpAsnDb(t, writeTestIpAsnDb(t, geoLite2AsnDatabaseType, map[string]IpAsn{
			"198.51.100.0/24": {Number: 7922, Organization: "Example Access"},
			"203.0.113.0/24":  {Number: 16509, Organization: "AMAZON-02"},
		}))
		extender := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1")

		issue := func(clientAddress string, signatureAgent string) *LagTrackResult {
			clientSession := newLagTestGuestSession(t, ctx, clientAddress)
			if signatureAgent != "" {
				clientSession.Header = map[string][]string{"Signature-Agent": {signatureAgent}}
			}
			result, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
				{ExtenderId: extender.extenderId},
			}}, clientSession, server.NowUtc(), settings)
			connect.AssertEqual(t, err, nil)
			return result
		}
		row := func(result *LagTrackResult) (automated bool, reason *string) {
			connect.AssertEqual(t, result.Error, (*LagError)(nil))
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT automated, automated_reason FROM lag_track WHERE track_id = $1`,
					*result.TrackId,
				).Scan(&automated, &reason))
			})
			return
		}

		automated, reason := row(issue("198.51.100.30:1234", ""))
		connect.AssertEqual(t, automated, false)
		connect.AssertEqual(t, reason, (*string)(nil))

		automated, reason = row(issue("203.0.113.30:1234", ""))
		connect.AssertEqual(t, automated, true)
		connect.AssertEqual(t, *reason, model.LagAutomatedReasonDatacenterAsn)

		// a synthetic claimed agent; its name is not an authenticated identity
		agent := fmt.Sprintf(`"https://agent-%s.example"`, server.NewId())
		first := issue("198.51.100.31:1234", agent)
		automated, reason = row(first)
		connect.AssertEqual(t, automated, true)
		connect.AssertEqual(t, *reason, model.LagAutomatedReasonSignatureAgent)
		connect.AssertEqual(t, model.GetLagTrack(ctx, *first.TrackId).AutomatedReason, model.LagAutomatedReasonSignatureAgent)
		connect.AssertEqual(t, model.GetLagTrack(ctx, *first.TrackId).Automated(), true)

		// repeated claims stay automated without spending a global quota,
		// including a third independent client from another network
		row(issue("198.51.100.32:1234", agent))
		automated, reason = row(issue("203.0.113.31:1234", agent))
		connect.AssertEqual(t, automated, true)
		connect.AssertEqual(t, *reason, model.LagAutomatedReasonSignatureAgent)

		// changing the claimed name does not change admission either
		row(issue("198.51.100.33:1234", fmt.Sprintf(`"https://agent-%s.example"`, server.NewId())))
	})
}

// The lag gauges leave automated tracks out, and the automated gauge counts
// their results by reason (amendment 12).
func TestStatsRefreshLagLeavesOutAutomated(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		addTrack := func(reason string, countryCode string) server.Id {
			guest := model.CreateLagGuestNetwork(ctx, now.Add(-30*time.Minute), now.Add(24*time.Hour))
			trackId := server.NewId()
			model.CreateLagTrack(ctx, &model.LagTrack{
				TrackId:         trackId,
				NetworkId:       guest.NetworkId,
				ClientId:        server.NewId(),
				StartedAt:       now.Add(-20 * time.Minute),
				Client:          &model.LagTrackClient{CountryCode: countryCode},
				AutomatedReason: reason,
			}, []*model.LagTicket{})
			idle := float32(12)
			download := float32(80)
			upload := float32(20)
			connect.AssertEqual(t, model.AddLagResult(ctx, &model.LagResult{
				LagResultId:             server.NewId(),
				TrackId:                 trackId,
				TicketId:                server.NewId(),
				ExtenderId:              server.NewId(),
				ExtenderIpFamily:        4,
				SessionStartTime:        now.Add(-11 * time.Minute),
				WindowStartTime:         now.Add(-11 * time.Minute),
				PingOffsetMs:            []int32{0},
				PingMicros:              []int32{12000},
				IntervalMs:              250,
				DownloadReceivedBytes:   []int64{},
				DownloadSentBytes:       []int64{},
				UploadBytes:             []int64{},
				EndReason:               "complete",
				OutsideLoadLatencyP50Ms: &idle,
				DownloadP50Mbps:         &download,
				UploadP50Mbps:           &upload,
				CreatedAt:               now.Add(-10 * time.Minute),
			}), true)
			return trackId
		}
		addTrack("", "au")
		addTrack(model.LagAutomatedReasonDatacenterAsn, "us")
		addTrack(model.LagAutomatedReasonDatacenterAsn, "us")
		addTrack(model.LagAutomatedReasonSignatureAgent, "us")

		statsRefreshLag(ctx, now)

		connect.AssertEqual(t, testutil.ToFloat64(statsLagResults1hGauge.gauge.WithLabelValues("complete", "ipv4")), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagTracks1hGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagGuests1hGauge.gauge), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagVisitors24hByCountryGauge.gauge.WithLabelValues("au")), float64(1))
		connect.AssertEqual(t, testutil.CollectAndCount(statsLagVisitors24hByCountryGauge.gauge), 1)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagAutomatedResults1hGauge.gauge.WithLabelValues(model.LagAutomatedReasonDatacenterAsn)), float64(2))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagAutomatedResults1hGauge.gauge.WithLabelValues(model.LagAutomatedReasonSignatureAgent)), float64(1))
	})
}

// A track's source is cli or mcp when X-Lag-Client names one of them, in any
// case, and web otherwise, a browser's missing header included.
func TestLagTrackSourceHeader(t *testing.T) {
	for _, c := range []struct {
		value string
		want  string
	}{
		{"", model.LagSourceWeb},
		{"cli", model.LagSourceCli},
		{"mcp", model.LagSourceMcp},
		{" MCP ", model.LagSourceMcp},
		{"CLI", model.LagSourceCli},
		{"browser", model.LagSourceWeb},
		{"web", model.LagSourceWeb},
	} {
		header := http.Header{}
		if c.value != "" {
			header.Set("X-Lag-Client", c.value)
		}
		connect.AssertEqual(t, lagTrackSource(header), c.want)
	}
}

// Each X-Lag-Client value stores its source with the track, and the results
// per hour split by source, automated tracks left out.
func TestLagTrackSourceStoredAndCounted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installLagTestExtenderConfig(t, true)
		settings := DefaultLagSettings()
		defer Testing_SetLagSettings(settings)()
		places := map[string]*model.LagPlace{}
		useLagAddressPlaces(t, places)
		useIpAsnDb(t, writeTestIpAsnDb(t, geoLite2AsnDatabaseType, map[string]IpAsn{
			"198.51.100.0/24": {Number: 7922, Organization: "Example Access"},
		}))
		extender := newLagServingTestExtender(t, ctx, places, lagTestPlace("Hamburg", "de", 53.5, 10.0), "192.0.2.1")

		issue := func(clientAddress string, client string) server.Id {
			clientSession := newLagTestGuestSession(t, ctx, clientAddress)
			if client != "" {
				clientSession.Header = map[string][]string{"X-Lag-Client": {client}}
			}
			result, err := lagTrack(&LagTrackArgs{Extenders: []*LagTrackExtenderArgs{
				{ExtenderId: extender.extenderId},
			}}, clientSession, server.NowUtc(), settings)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, (*LagError)(nil))
			return *result.TrackId
		}
		for _, c := range []struct {
			address string
			client  string
			want    string
		}{
			{"198.51.100.30:1234", "", model.LagSourceWeb},
			{"198.51.100.31:1234", "cli", model.LagSourceCli},
			{"198.51.100.32:1234", "mcp", model.LagSourceMcp},
			{"198.51.100.33:1234", "something else", model.LagSourceWeb},
		} {
			trackId := issue(c.address, c.client)
			connect.AssertEqual(t, model.GetLagTrack(ctx, trackId).Source, c.want)
			// a cli or mcp track is a visitor's own run, never automated
			connect.AssertEqual(t, model.GetLagTrack(ctx, trackId).Automated(), false)
		}

		// results by source, an automated track's left out
		now := server.NowUtc()
		addResult := func(source string, automatedReason string) {
			trackId := server.NewId()
			model.CreateLagTrack(ctx, &model.LagTrack{
				TrackId:         trackId,
				NetworkId:       server.NewId(),
				ClientId:        server.NewId(),
				StartedAt:       now.Add(-20 * time.Minute),
				AutomatedReason: automatedReason,
				Source:          source,
			}, []*model.LagTicket{})
			connect.AssertEqual(t, model.AddLagResult(ctx, &model.LagResult{
				LagResultId:           server.NewId(),
				TrackId:               trackId,
				TicketId:              server.NewId(),
				ExtenderId:            server.NewId(),
				ExtenderIpFamily:      4,
				SessionStartTime:      now.Add(-11 * time.Minute),
				WindowStartTime:       now.Add(-11 * time.Minute),
				PingOffsetMs:          []int32{0},
				PingMicros:            []int32{12000},
				IntervalMs:            250,
				DownloadReceivedBytes: []int64{},
				DownloadSentBytes:     []int64{},
				UploadBytes:           []int64{},
				EndReason:             "no_load",
				CreatedAt:             now.Add(-10 * time.Minute),
			}), true)
		}
		addResult(model.LagSourceWeb, "")
		addResult(model.LagSourceWeb, "")
		addResult(model.LagSourceCli, "")
		addResult(model.LagSourceMcp, "")
		addResult(model.LagSourceMcp, "")
		addResult(model.LagSourceMcp, "")
		addResult(model.LagSourceCli, model.LagAutomatedReasonDatacenterAsn)

		connect.AssertEqual(t, model.CountLagResultsBySourceSince(ctx, now.Add(-time.Hour)), map[string]int64{
			model.LagSourceWeb: 2,
			model.LagSourceCli: 1,
			model.LagSourceMcp: 3,
		})
		statsRefreshLag(ctx, now)
		connect.AssertEqual(t, testutil.ToFloat64(statsLagResults1hBySourceGauge.gauge.WithLabelValues(model.LagSourceWeb)), float64(2))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagResults1hBySourceGauge.gauge.WithLabelValues(model.LagSourceCli)), float64(1))
		connect.AssertEqual(t, testutil.ToFloat64(statsLagResults1hBySourceGauge.gauge.WithLabelValues(model.LagSourceMcp)), float64(3))
		connect.AssertEqual(t, testutil.CollectAndCount(statsLagResults1hBySourceGauge.gauge), len(model.LagSources))
	})
}
