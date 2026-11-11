// One URL turn is a run batch of one provider, so the run batch guard can
// never see a failure that every provider shares. This guard judges each turn
// against the latest turns of the other networks its pass measured, and
// holds back only a failure that is unmistakably the platform's.
package work

import (
	"maps"
	"slices"
	"sync"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

const (
	// Distinct networks the window holds, and needs before it judges: the
	// latest measured turn of each of the pass's most recently measured
	// networks. A network's next turn, from any of its providers, replaces
	// its last, so one broken network never counts twice.
	providerUrlProbeCommonModeNetworkCount = 16
	// Largest share of those turns that may pass. Normal URL failure is
	// 35-55% of turns, so this is far below any working fleet; at sixteen
	// networks it admits no pass at all.
	providerUrlProbeCommonModePassShare = 0.02
)

// A failed run that no destination answered: every counted load is a
// no-contact failure (egresshealth.CheckResult.NoContactFailure), with no
// status, response byte, redirect or TLS verdict, and the run carries no
// security evidence. A run whose counted loads do not add up to its total is
// not evidence either way.
func providerUrlProbePreResponseFailure(run *egresshealth.Result) bool {
	if run == nil || run.Total < 1 || run.OkCount != 0 || run.TlsAuthenticationFailure ||
		(run.UrlProbeEvidence != nil && len(run.UrlProbeEvidence.Security) != 0) {
		return false
	}
	countedLoadCount := 0
	for _, check := range run.Checks {
		if check.NotMeasured || check.Canary {
			continue
		}
		if !check.NoContactFailure() {
			return false
		}
		countedLoadCount++
	}
	return countedLoadCount == run.Total
}

// The measured HTTP/3 leg of a run's one counted load, or nil: none was made,
// it was not measured, or the run is not one counted URL load.
func providerUrlProbeCountedH3Leg(run *egresshealth.Result) *egresshealth.CheckResult {
	if run == nil || run.Total != 1 {
		return nil
	}
	var leg *egresshealth.CheckResult
	for _, check := range run.Checks {
		if check.NotMeasured || check.Canary {
			continue
		}
		if leg != nil || !check.H3Required || check.H3 == nil || check.H3.NotMeasured {
			return nil
		}
		leg = check.H3
	}
	return leg
}

// A failed run whose only failure is its HTTP/3 leg: its URL declares HTTP/3,
// its HTTPS leg passed, and nothing answered its HTTP/3 leg's QUIC handshake
// (egresshealth.UrlProbeH3UdpSilent), the one HTTP/3 outcome that fails a
// load.
func providerUrlProbeH3OnlyFailure(run *egresshealth.Result) bool {
	leg := providerUrlProbeCountedH3Leg(run)
	return leg != nil && egresshealth.UrlProbeH3Udp(leg) == egresshealth.UrlProbeH3UdpSilent && run.OkCount == 0 && !run.TlsAuthenticationFailure
}

// Whether a measured HTTP/3 leg reached its site over QUIC: it passed, or the
// site answered it. Either proves the platform's QUIC path through the
// provider, whatever the site then did.
func providerUrlProbeH3LegReachedSite(leg *egresshealth.CheckResult) bool {
	udp := egresshealth.UrlProbeH3Udp(leg)
	return udp == egresshealth.UrlProbeH3UdpPassed || udp == egresshealth.UrlProbeH3UdpAnswered
}

// The latest measured turn of each of one pass's most recently measured
// networks. The window is common-mode when it holds
// providerUrlProbeCommonModeNetworkCount networks, at most
// providerUrlProbeCommonModePassShare of them passed, and every failure among
// them is pre-response: the fleet is failing before any destination answers,
// which is the platform's tunnel path, not each provider's. A pre-response
// failure judged in that window is held back through the run batch guard's
// release (model.ProbeRunBatchGuardClass): its claim completes for a paced
// retry with no URL outcome, quality or quota credit. A pass, a failure some
// destination answered and TLS evidence are never held, and each ends the
// episode as soon as it is recorded.
//
// The window counts networks, not providers. One network's providers share
// an operator and often one failure, and admitted together they would fill a
// window of providers by themselves: their genuine failures would be held,
// retried together and held again, never counting toward quota. A provider
// whose network is unknown, from a server that predates it, stands as its
// own network.
//
// The window starts empty with every pass and judges only once full, so a
// pass that starts inside a platform failure publishes the turns that fill
// it, and an episode that starts mid-pass publishes the turns that push the
// last pass out. Safe for concurrent use by the pass's turns.
//
// A second window holds the latest measured HTTP/3 leg of each of the pass's
// most recently measured networks (url_h3.go). Every such leg follows an
// HTTPS leg that passed, so a fleet whose HTTP/3 legs all go unanswered while
// their HTTPS legs pass is failing on the platform's own QUIC path, not on
// each provider's UDP: the provider path is the same one the HTTPS leg just
// proved. When that window is full and at most
// providerUrlProbeCommonModePassShare of its legs reached their sites (passed,
// or answered by the site), a run whose only failure is a silent HTTP/3 leg
// is held back the same way, so a prober-side HTTP/3 fault can never fail
// every provider at once. A leg that reaches its site ends the episode as
// soon as it is recorded; a real share of providers that carry no UDP leaves
// the window holding such legs, and every outcome is published.
type providerUrlProbeCommonMode struct {
	shardIndex int
	shardCount int

	stateLock sync.Mutex
	// oldest first
	networks     []providerUrlProbeCommonModeNetwork
	networkTurns map[providerUrlProbeCommonModeNetwork]providerUrlProbeCommonModeTurn
	tripped      bool
	heldCount    int
	// the HTTP/3 window: oldest first, each network's latest leg passed
	h3Networks     []providerUrlProbeCommonModeNetwork
	h3NetworkPass  map[providerUrlProbeCommonModeNetwork]bool
	h3Tripped      bool
	h3HeldCount    int
	observeH3Holds func()
}

// One network of the window: its id, or for a provider whose network is
// unknown, that provider alone.
type providerUrlProbeCommonModeNetwork struct {
	networkId        string
	providerClientId string
}

// One network's latest measured turn.
type providerUrlProbeCommonModeTurn struct {
	ok          bool
	preResponse bool
}

// An empty window for one shard pass.
func newProviderUrlProbeCommonMode(shardIndex int, shardCount int) *providerUrlProbeCommonMode {
	return &providerUrlProbeCommonMode{
		shardIndex:     shardIndex,
		shardCount:     shardCount,
		networkTurns:   map[providerUrlProbeCommonModeNetwork]providerUrlProbeCommonModeTurn{},
		h3NetworkPass:  map[providerUrlProbeCommonModeNetwork]bool{},
		observeH3Holds: func() { urlProbeH3CommonModeHeld.Inc() },
	}
}

// Records the batch's measured runs as turns of each provider's network, then
// returns the window's failed share and whether the batch is held back: it
// measured something, all of it pre-response failures, and the window is
// common-mode; or all of it HTTP/3-only failures, and the HTTP/3 window is
// common-mode. A provider absent from providerClientIdNetworkIds stands as
// its own network. A batch that measured nothing changes nothing. The start
// and end of each episode are logged.
func (self *providerUrlProbeCommonMode) judge(
	providerClientIdRuns map[string]*egresshealth.Result,
	providerClientIdNetworkIds map[string]string,
) (failureShare float64, held bool) {
	entered, released := false, false
	networkCount, passCount, preResponseCount, episodeHeldCount := 0, 0, 0, 0
	h3Entered, h3Released := false, false
	h3NetworkCount, h3PassCount, h3EpisodeHeldCount := 0, 0, 0
	h3Held := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		measuredCount, measuredPreResponseCount, measuredH3OnlyFailureCount := 0, 0, 0
		batchNetworkTurns := map[providerUrlProbeCommonModeNetwork]providerUrlProbeCommonModeTurn{}
		batchH3Pass := map[providerUrlProbeCommonModeNetwork]bool{}
		// sorted, so a batch of several providers evicts deterministically
		for _, providerClientId := range slices.Sorted(maps.Keys(providerClientIdRuns)) {
			run := providerClientIdRuns[providerClientId]
			if run == nil || run.Total < 1 {
				continue
			}
			turn := providerUrlProbeCommonModeTurn{
				ok:          0 < run.OkCount,
				preResponse: providerUrlProbePreResponseFailure(run),
			}
			measuredCount++
			if turn.preResponse {
				measuredPreResponseCount++
			}
			if providerUrlProbeH3OnlyFailure(run) {
				measuredH3OnlyFailureCount++
			}
			network := providerUrlProbeCommonModeNetwork{networkId: providerClientIdNetworkIds[providerClientId]}
			if network.networkId == "" {
				network.providerClientId = providerClientId
			}
			if leg := providerUrlProbeCountedH3Leg(run); leg != nil {
				pass := providerUrlProbeH3LegReachedSite(leg)
				if batchPass, ok := batchH3Pass[network]; ok {
					// as above: one network's providers are one turn
					pass = pass || batchPass
				}
				batchH3Pass[network] = pass
				if _, ok := self.h3NetworkPass[network]; ok {
					self.h3Networks = slices.DeleteFunc(self.h3Networks, func(windowNetwork providerUrlProbeCommonModeNetwork) bool {
						return windowNetwork == network
					})
				}
				self.h3Networks = append(self.h3Networks, network)
				self.h3NetworkPass[network] = pass
				for providerUrlProbeCommonModeNetworkCount < len(self.h3Networks) {
					delete(self.h3NetworkPass, self.h3Networks[0])
					self.h3Networks = slices.Delete(self.h3Networks, 0, 1)
				}
			}
			if batchTurn, ok := batchNetworkTurns[network]; ok {
				// a batch's providers of one network are one turn: a pass or
				// an answer from any of them is the network's
				turn.ok = turn.ok || batchTurn.ok
				turn.preResponse = turn.preResponse && batchTurn.preResponse
			}
			batchNetworkTurns[network] = turn
			if _, ok := self.networkTurns[network]; ok {
				self.networks = slices.DeleteFunc(self.networks, func(windowNetwork providerUrlProbeCommonModeNetwork) bool {
					return windowNetwork == network
				})
			}
			self.networks = append(self.networks, network)
			self.networkTurns[network] = turn
			for providerUrlProbeCommonModeNetworkCount < len(self.networks) {
				delete(self.networkTurns, self.networks[0])
				self.networks = slices.Delete(self.networks, 0, 1)
			}
		}

		networkCount = len(self.networks)
		for _, turn := range self.networkTurns {
			if turn.ok {
				passCount++
			} else if turn.preResponse {
				preResponseCount++
			}
		}
		if 0 < networkCount {
			failureShare = float64(networkCount-passCount) / float64(networkCount)
		}
		h3NetworkCount = len(self.h3Networks)
		for _, pass := range self.h3NetworkPass {
			if pass {
				h3PassCount++
			}
		}
		if measuredCount == 0 {
			return
		}
		commonMode := providerUrlProbeCommonModeNetworkCount <= networkCount &&
			float64(passCount) <= providerUrlProbeCommonModePassShare*float64(networkCount) &&
			preResponseCount == networkCount-passCount
		held = commonMode && measuredPreResponseCount == measuredCount
		if held {
			self.heldCount++
		}
		if commonMode != self.tripped {
			self.tripped = commonMode
			entered, released = commonMode, !commonMode
			episodeHeldCount = self.heldCount
			if released {
				self.heldCount = 0
			}
		}
		h3CommonMode := providerUrlProbeCommonModeNetworkCount <= h3NetworkCount &&
			float64(h3PassCount) <= providerUrlProbeCommonModePassShare*float64(h3NetworkCount)
		h3Held = !held && h3CommonMode && measuredH3OnlyFailureCount == measuredCount
		if h3Held {
			self.h3HeldCount++
			held = true
		}
		if h3CommonMode != self.h3Tripped {
			self.h3Tripped = h3CommonMode
			h3Entered, h3Released = h3CommonMode, !h3CommonMode
			h3EpisodeHeldCount = self.h3HeldCount
			if h3Released {
				self.h3HeldCount = 0
			}
		}
	}()
	if h3Held && self.observeH3Holds != nil {
		self.observeH3Holds()
	}
	if h3Entered {
		// alert class: every recent HTTP/3 leg fails after its HTTPS leg passed
		glog.Errorf(
			"[egress]URL HTTP/3 common-mode guard tripped: shard=%d/%d networks=%d h3_passes=%d; URL outcomes that failed only their HTTP/3 leg are held back from provider history and their claims retried without measured credit until an HTTP/3 leg passes\n",
			self.shardIndex,
			self.shardCount,
			h3NetworkCount,
			h3PassCount,
		)
	}
	if h3Released {
		glog.Infof(
			"[egress]URL HTTP/3 common-mode guard released: shard=%d/%d held=%d; HTTP/3 outcomes are published again\n",
			self.shardIndex,
			self.shardCount,
			h3EpisodeHeldCount,
		)
	}

	if entered {
		// alert class: the fleet is failing before any destination answers
		glog.Errorf(
			"[egress]URL common-mode guard tripped: shard=%d/%d networks=%d passes=%d pre_response_failures=%d; pre-response URL failures are held back from provider history and their claims retried without measured credit until a provider passes or a destination answers\n",
			self.shardIndex,
			self.shardCount,
			networkCount,
			passCount,
			preResponseCount,
		)
	}
	if released {
		glog.Infof(
			"[egress]URL common-mode guard released: shard=%d/%d held=%d; URL outcomes are published again\n",
			self.shardIndex,
			self.shardCount,
			episodeHeldCount,
		)
	}
	return
}
