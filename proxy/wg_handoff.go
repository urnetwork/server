package proxy

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Wg endpoint handoff (PROXYDRAIN1.md §3.4) — the wgServer halves of the
// drain export and the replacement-instance apply.

// PrepareWgHandoff publishes this replacement's generation during startup,
// before warp redirects traffic and begins draining the old instance.
func (self *wgServer) PrepareWgHandoff(ctx context.Context) {
	if !self.settings.EnableWgHandoff {
		return
	}
	self.handoffPrepareOnce.Do(func() {
		self.handoffGeneration = server.NewId().String()
		model.BeginProxyWgHandoff(
			ctx,
			server.RequireHost(),
			server.RequireBlock(),
			self.handoffGeneration,
			self.settings.WgHandoffRequestTtl,
		)
		glog.Infof("[wg]handoff prepared generation %s\n", self.handoffGeneration)
	})
}

// ExportWgHandoff exports the recently active wg peers' learned endpoints to
// the replacement generation advertised during its startup. Runs as a drain
// before-exit hook, so the endpoints are the freshest the drained instance
// ever saw.
func (self *wgServer) ExportWgHandoff(ctx context.Context) {
	if !self.settings.EnableWgHandoff {
		return
	}
	proxyHost := server.RequireHost()
	block := server.RequireBlock()

	peerStatuses, err := self.wgProxy.PeerStatuses()
	if err != nil {
		glog.Infof("[wg]handoff export err=%s\n", err)
		return
	}

	now := server.NowUtc()
	activeStartTime := now.Add(-self.settings.WgHandoffActivityWindow)
	peers := []*model.ProxyWgHandoffPeer{}
	for _, peerStatus := range peerStatuses {
		// only peers with a session recent enough to plausibly return, and a
		// learned endpoint to initiate toward
		if peerStatus.Endpoint == nil || peerStatus.LastHandshake.Before(activeStartTime) {
			continue
		}
		peers = append(peers, &model.ProxyWgHandoffPeer{
			ClientIpv4:        peerStatus.ClientIpv4,
			Endpoint:          peerStatus.Endpoint.String(),
			LastHandshakeTime: peerStatus.LastHandshake,
		})
	}
	generation := model.CurrentProxyWgHandoffGeneration(ctx, proxyHost, block)
	if generation == "" {
		glog.Infof("[wg]handoff export: no replacement generation\n")
		return
	}

	// Publish an empty export as a completion marker too: the export doubles
	// as the drain-complete beacon that ungates the replacement's pre-warm,
	// and without it a no-peer deploy would hold the replacement at the gate
	// until its full poll budget.
	model.SetProxyWgHandoffForGeneration(ctx, proxyHost, block, generation, now, peers, self.settings.WgHandoffRequestTtl)
	glog.Infof("[wg]handoff export: %d peers generation=%s\n", len(peers), generation)
}

// ApplyWgHandoff consumes the handoff exported by the drained instance and
// re-establishes its wg sessions from this side. Call after the initial peer
// sync (the peers must be registered for the seeds to apply).
//
// It BLOCKS until the old instance's generation-tagged export appears or
// `WgHandoffPollTimeout` passes. The export is written at the old instance's
// drain END (an empty peer set is the completion marker), so the successful
// wait doubles as the old instance's drain-complete beacon: the caller
// sequences the device pre-warm after this call returns
// (cli/proxy/main.go), which keeps the reused persisted window identities
// from running live in both containers during the drain grace
// (REVIEW2-UPDATE1 §4.4). Poll-budget expiry is the old-instance-crashed
// fallback: return without a handoff and let the caller proceed.
//
// Once the export is consumed, the endpoints are seeded on this instance's
// restored peers and server-side handshake initiations run IN THE
// BACKGROUND until each peer completes a new handshake or
// `WgHandoffInitiateTimeout` passes — that budget starts when the export
// appears, not at boot, so a long serialized host drain ahead of this block
// cannot eat it (REVIEW2-UPDATE1 §5.6). Initiations sent while the old
// instance's conntrack entries still pin the clients are harmlessly
// blackholed — the client's replies re-DNAT to the old instance until
// warpctl's flush — so the loop keeps initiating on `WgHandoffInitiatePace`
// (the device also retries on its own timers) and observes convergence via
// each peer's last-handshake time.
//
// The returned channel delivers the number of peers that re-established on
// this instance once the initiation phase completes (0 when there was no
// export, no active peers, or nothing seedable).
func (self *wgServer) ApplyWgHandoff(ctx context.Context) <-chan int {
	reestablished := make(chan int, 1)
	if !self.settings.EnableWgHandoff {
		reestablished <- 0
		return reestablished
	}
	proxyHost := server.RequireHost()
	block := server.RequireBlock()
	self.PrepareWgHandoff(ctx)

	// The new instance becomes ready before warp redirects and starts the old
	// drain, so the export normally does not exist yet. Wait for this exact
	// generation instead of doing a one-shot GETDEL (or consuming a stale
	// prior generation).
	pollCtx, pollCancel := context.WithTimeout(ctx, self.settings.WgHandoffPollTimeout)
	defer pollCancel()
	pollInterval := self.settings.WgHandoffPollInterval
	if pollInterval <= 0 {
		pollInterval = 1 * time.Second
	}
	var exportTime time.Time
	var peers []*model.ProxyWgHandoffPeer
	for exportTime.IsZero() {
		exportTime, peers = self.takeHandoffExport(pollCtx, proxyHost, block)
		if !exportTime.IsZero() {
			break
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			glog.Infof("[wg]handoff apply: no export for generation %s within %s\n", self.handoffGeneration, self.settings.WgHandoffPollTimeout)
			reestablished <- 0
			return reestablished
		case <-timer.C:
		}
	}
	if len(peers) == 0 {
		glog.Infof("[wg]handoff apply: export contained no active peers\n")
		reestablished <- 0
		return reestablished
	}

	endpoints := map[netip.Addr]*net.UDPAddr{}
	for _, peer := range peers {
		endpoint, err := net.ResolveUDPAddr("udp", peer.Endpoint)
		if err != nil {
			continue
		}
		endpoints[peer.ClientIpv4] = endpoint
	}

	appliedCount, err := self.wgProxy.SeedEndpoints(endpoints)
	if err != nil {
		glog.Infof("[wg]handoff apply seed err=%s\n", err)
		reestablished <- 0
		return reestablished
	}
	glog.Infof("[wg]handoff apply: %d/%d endpoints seeded (exported %s ago)\n", appliedCount, len(peers), server.NowUtc().Sub(exportTime).Round(time.Second))
	if appliedCount == 0 {
		reestablished <- 0
		return reestablished
	}

	// The export is in hand — the caller is free to pre-warm — so the
	// initiation loop runs in the background on its own budget.
	go server.HandleError(func() {
		reestablishedCount := 0
		defer func() {
			reestablished <- reestablishedCount
		}()
		initiateCtx, initiateCancel := context.WithTimeout(ctx, self.settings.WgHandoffInitiateTimeout)
		defer initiateCancel()
		reestablishedCount = self.driveHandshakeInitiations(initiateCtx, endpoints)
	})
	return reestablished
}

// takeHandoffExport reads the generation-tagged export, treating a poll
// budget expiry that fires during the redis read as the clean "no export"
// outcome: at the deadline the GETDEL can surface context.DeadlineExceeded
// as a raise through the redis wrapper, which previously escaped and logged
// as an unexpected error (REVIEW2-UPDATE1 §5). Any raise while the poll ctx
// is still live propagates unchanged.
func (self *wgServer) takeHandoffExport(
	pollCtx context.Context,
	proxyHost string,
	block string,
) (exportTime time.Time, peers []*model.ProxyWgHandoffPeer) {
	defer func() {
		if r := recover(); r != nil {
			if pollCtx.Err() != nil {
				// the budget expired mid-read; the poll loop's ctx-done exit
				// handles it as the no-export outcome
				exportTime = time.Time{}
				peers = nil
				return
			}
			panic(r)
		}
	}()
	exportTime, peers = model.TakeProxyWgHandoffForGeneration(
		pollCtx,
		proxyHost,
		block,
		self.handoffGeneration,
	)
	return
}

// driveHandshakeInitiations initiates toward each seeded endpoint until the
// peer completes a NEW handshake (later than the loop start) or initiateCtx
// ends, and returns the number of peers that re-established.
func (self *wgServer) driveHandshakeInitiations(
	initiateCtx context.Context,
	endpoints map[netip.Addr]*net.UDPAddr,
) int {
	pending := map[netip.Addr]bool{}
	for addr := range endpoints {
		pending[addr] = true
	}
	reestablishedCount := 0
	startTime := server.NowUtc()
	for {
		for addr := range pending {
			if err := self.wgProxy.InitiateHandshake(addr); err != nil {
				if glog.V(1) {
					glog.Infof("[wg]handoff initiate %s err=%s\n", addr, err)
				}
			}
		}

		select {
		case <-initiateCtx.Done():
			glog.Infof("[wg]handoff apply: initiate budget ended with %d peers pending\n", len(pending))
			return reestablishedCount
		case <-time.After(self.settings.WgHandoffInitiatePace):
		}

		peerStatuses, err := self.wgProxy.PeerStatuses()
		if err != nil {
			continue
		}
		for addr := range pending {
			if peerStatus, ok := peerStatuses[addr]; ok {
				if startTime.Before(peerStatus.LastHandshake) {
					// re-established on this instance
					delete(pending, addr)
					reestablishedCount += 1
					glog.Infof("[wg]handoff re-established %s in %s\n", addr, server.NowUtc().Sub(startTime).Round(time.Millisecond))
				}
			}
		}
		if len(pending) == 0 {
			glog.Infof("[wg]handoff apply: all peers re-established in %s\n", server.NowUtc().Sub(startTime).Round(time.Second))
			return reestablishedCount
		}
	}
}
