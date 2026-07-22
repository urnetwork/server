package proxy

// Server-level wg endpoint handoff e2e (PROXYDRAIN1.md §3.4), the deploy
// analog of TestProxyWgRestartReconnect: the wg server is restarted with all
// in-memory wireguard state lost, but this time the drained instance
// EXPORTED its peer endpoints and the replacement APPLIES them and initiates
// from the server side. The SAME wireguard client — never reconfigured, still
// holding the dead session — must resume traffic without waiting out its own
// ~15s dead-session detection.

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestProxyWgHandoffFastReconnect(t *testing.T) {
	if testing.Short() {
		return
	}
	// see TestProxy: fixed ports cannot be rebound on a rerun in the same process
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fmt.Printf("[progress]start TestProxyWgHandoffFastReconnect\n")
		h := setupProxyTest(t)
		defer h.cancel()

		// the handoff store is keyed by (host, block)
		proxyHost := fmt.Sprintf("wghandoff%d", time.Now().UnixNano()%1000000)
		os.Setenv("WARP_HOST", proxyHost)
		os.Setenv("WARP_BLOCK", "g1")
		defer os.Unsetenv("WARP_HOST")
		defer os.Setenv("WARP_BLOCK", "test")

		// a long-lived wg client + netstack that survive the server restart
		wgCtx, wgCancel := context.WithCancel(h.ctx)
		defer wgCancel()
		transport, closeWgClient := startWgClient(t, wgCtx, h.proxyClient.WgConfig)
		defer closeWgClient()

		// establish the session and confirm traffic
		requireProxyGet(t, "wg-handoff-before", transport)

		// The replacement advertises its generation before warp redirects and
		// starts the old drain.
		generation := server.NewId().String()
		model.BeginProxyWgHandoff(h.ctx, proxyHost, "g1", generation, h.proxySettings.WgHandoffRequestTtl)

		// ---- the drained instance's half: export at drain end, tagged for
		// the already-started replacement generation ----
		h.wg.ExportWgHandoff(h.ctx)
		exportTime, exportedPeers := model.TakeProxyWgHandoffForGeneration(
			h.ctx,
			proxyHost,
			"g1",
			generation,
		)
		if exportTime.IsZero() || len(exportedPeers) != 1 {
			t.Fatalf("drained export = %v/%d peers, want generation-tagged handoff", exportTime, len(exportedPeers))
		}

		// stop the wg server, releasing its socket and all in-memory state
		fmt.Printf("[progress]restarting wg server\n")
		h.wgCancel()
		waitFor(t, 15*time.Second, "wg port release", func() bool {
			pc, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", InternalWgPort))
			if err != nil {
				return false
			}
			pc.Close()
			return true
		})
		// drop pooled tcp connections through the dead tunnel so the first
		// post-restart attempt dials fresh
		transport.CloseIdleConnections()
		restartTime := time.Now()

		// ---- the replacement instance: restore peers, apply the handoff ----
		handoffSettings := *h.proxySettings
		// fast convergence detection for the test (production paces at 5s)
		handoffSettings.WgHandoffInitiatePace = 200 * time.Millisecond
		handoffSettings.WgHandoffPollInterval = 20 * time.Millisecond
		// bound a regression to well under the test timeout
		handoffSettings.WgHandoffPollTimeout = 30 * time.Second
		wg2Ctx, wg2Cancel := context.WithCancel(h.ctx)
		defer wg2Cancel()
		wg2 := NewWgServer(wg2Ctx, wg2Cancel, h.proxyDeviceManager, &handoffSettings)

		// restore the peers the way the notification startup full sync does
		if err := wg2.SyncProxyClients([]*model.ProxyClient{h.proxyClient}, server.NowUtc()); err != nil {
			t.Fatalf("wg: restore clients after restart: %v", err)
		}

		// Apply starts BEFORE the old generation's export becomes visible,
		// matching warp's replacement-ready -> redirect -> old-drain order.
		// It must wait (the export is the old instance's drain-complete
		// beacon that also ungates the pre-warm) instead of doing a one-shot
		// read and returning.
		wg2.handoffGeneration = generation
		wg2.handoffPrepareOnce.Do(func() {})
		applied := make(chan (<-chan int), 1)
		go func() {
			applied <- wg2.ApplyWgHandoff(h.ctx)
		}()
		select {
		case <-applied:
			t.Fatalf("handoff apply returned early before export")
		case <-time.After(100 * time.Millisecond):
		}
		model.SetProxyWgHandoffForGeneration(
			h.ctx,
			proxyHost,
			"g1",
			generation,
			exportTime,
			exportedPeers,
			h.proxySettings.WgHandoffRequestTtl,
		)

		// The delayed export is consumed and the apply returns (ungating the
		// caller's pre-warm) while the initiation phase runs in the
		// background...
		var reestablished <-chan int
		select {
		case reestablished = <-applied:
		case <-time.After(10 * time.Second):
			t.Fatal("replacement did not consume delayed handoff export")
		}
		// ...then endpoints are seeded and server-side initiation restores
		// the live session.
		var reestablishedCount int
		select {
		case reestablishedCount = <-reestablished:
		case <-time.After(10 * time.Second):
			t.Fatal("initiation phase did not complete")
		}
		reestablishElapsed := time.Since(restartTime)
		if reestablishedCount != 1 {
			t.Fatalf("handoff re-established %d peers, want 1", reestablishedCount)
		}
		fmt.Printf("[progress]wg session re-established %v after restart (server-initiated)\n", reestablishElapsed.Round(100*time.Millisecond))

		// the session came back server-initiated: well inside the client's
		// ~15s dead-session detection window
		if 10*time.Second <= reestablishElapsed {
			t.Fatalf("handoff re-establish took %v, want well under the client's dead-session detection", reestablishElapsed)
		}

		// and the tunnel carries traffic again
		requireProxyGet(t, "wg-handoff-after", transport)
	})
}

// The poll-expiry race (REVIEW2-UPDATE1 §5): the apply's poll budget can
// expire while the export GETDEL is in flight, surfacing the ctx deadline as
// a raise through the redis wrapper — which previously escaped ApplyWgHandoff
// and logged as "Unexpected error" instead of the clean no-export return.
// takeHandoffExport must absorb a raise at expiry (any variant: deadline from
// the command, Done from the wrapper's retry gate) as the no-export outcome,
// and must NOT absorb raises while the poll ctx is live.
func TestProxyWgHandoffPollExpiry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		proxyHost := fmt.Sprintf("pollexpiry%d", time.Now().UnixNano()%1000000)
		s := &wgServer{
			settings:          DefaultProxySettings(),
			handoffGeneration: server.NewId().String(),
		}

		// a live ctx passes through normally: absent export, zero result
		exportTime, peers := s.takeHandoffExport(ctx, proxyHost, "g1")
		if !exportTime.IsZero() || peers != nil {
			t.Fatalf("take on absent export = %v/%d, want zero", exportTime, len(peers))
		}

		// an already-expired poll ctx models the deadline firing mid-read:
		// the redis wrapper raises, and the take must return the clean
		// no-export outcome instead of letting the raise escape
		expiredCtx, expiredCancel := context.WithDeadline(ctx, time.Now().Add(-1*time.Second))
		defer expiredCancel()
		if r := server.HandleError(func() {
			exportTime, peers = s.takeHandoffExport(expiredCtx, proxyHost, "g1")
		}); r != nil {
			t.Fatalf("take at poll expiry raised: %v", r)
		}
		if !exportTime.IsZero() || peers != nil {
			t.Fatalf("take at poll expiry = %v/%d, want zero", exportTime, len(peers))
		}
	})
}
