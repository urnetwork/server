// The connect service's census counts its transports' sites in service
// (timer_census.go, CPUOPT7.md §11): an H1 session's heartbeat resets and read
// deadlines, and an H3 session's heartbeat resets and quic-go's stream read
// timer, through the hook the census installs. The heartbeat interval is an
// hour, so every reset is counted in ge_10s and no heartbeat is written.
package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Waits until site has counted at least one more than before in horizon.
func waitTimerCensusCount(t testing.TB, what string, site server.TimerCensusSite, horizon string, before float64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for server.Testing_TimerCensusCount(site, horizon) <= before {
		if deadline.Before(time.Now()) {
			t.Fatalf("%s: %s counted nothing in %s", what, site, horizon)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTimerCensusCountsTheTransportSites(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		stop, err := startTimerCensus(&server.TimerCensusSettings{SamplePeriod: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		testServer := newProviderIntentTestServerWithSettings(t, ctx, func(settings *ConnectHandlerSettings) {
			settings.MinPingTimeout = time.Hour
			settings.MaxPingTimeout = time.Hour
			settings.ConnectionTestConfig = &TestConfig{}
		})

		h1Ping := timerCensusCounts(server.TimerCensusSiteH1PingTimer)
		h1Read := timerCensusCounts(server.TimerCensusSiteH1ReadDeadline)
		_, h1Jwt := testServer.createClient()
		testServer.dialH1(t, h1Jwt, false)
		waitTimerCensusCount(t, "an h1 session", server.TimerCensusSiteH1PingTimer, "ge_10s", h1Ping["ge_10s"])
		waitTimerCensusCount(t, "an h1 session", server.TimerCensusSiteH1ReadDeadline, "ge_10s", h1Read["ge_10s"])

		h3Ping := timerCensusCounts(server.TimerCensusSiteH3PingTimer)
		quicRead := timerCensusCounts(server.TimerCensusSiteQuicStreamReadTimer)
		_, h3Jwt := testServer.createClient()
		h3 := testServer.dialH3(t, h3Jwt, false)
		waitTimerCensusCount(t, "an h3 session", server.TimerCensusSiteH3PingTimer, "ge_10s", h3Ping["ge_10s"])
		// the server's reader waits under its read deadline; a frame from the
		// client ends the wait, and its timer is stopped pending
		h3.stream.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if err := h3.framer.Write(h3.stream, make([]byte, 0)); err != nil {
			t.Fatal(err)
		}
		waitTimerCensusCount(t, "an h3 frame", server.TimerCensusSiteQuicStreamReadTimer, "stop", quicRead["stop"])

		// the heartbeats are an hour out, so no reset counted in another bucket
		for _, site := range []server.TimerCensusSite{server.TimerCensusSiteH1PingTimer, server.TimerCensusSiteH3PingTimer} {
			before := h1Ping
			if site == server.TimerCensusSiteH3PingTimer {
				before = h3Ping
			}
			for horizon, count := range timerCensusCounts(site) {
				if horizon != "ge_10s" && count != before[horizon] {
					t.Errorf("%s counted %v in %s", site, count-before[horizon], horizon)
				}
			}
		}
	})
}
