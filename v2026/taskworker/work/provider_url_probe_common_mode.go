// One URL turn is a run batch of one provider, so the run batch guard can
// never see a failure that every provider shares. This guard judges each turn
// against the latest turns of the other networks its pass measured, and
// holds back only a failure that is unmistakably the platform's.
package work

import (
	"maps"
	"slices"
	"sync"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
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

// The one counted load of a run, or nil: the run is not one counted URL load.
func providerUrlProbeCountedLoad(run *egresshealth.Result) *egresshealth.CheckResult {
	if run == nil || run.Total != 1 {
		return nil
	}
	var load *egresshealth.CheckResult
	for i := range run.Checks {
		check := &run.Checks[i]
		if check.NotMeasured || check.Canary {
			continue
		}
		if load != nil {
			return nil
		}
		load = check
	}
	return load
}

// The measured HTTP/3 leg of a run's one counted load, or nil: none was made,
// it was not measured, or the run is not one counted URL load.
func providerUrlProbeCountedH3Leg(run *egresshealth.Result) *egresshealth.CheckResult {
	load := providerUrlProbeCountedLoad(run)
	if load == nil || !load.H3Required || load.H3 == nil || load.H3.NotMeasured {
		return nil
	}
	return load.H3
}

// The measured IPv6 HTTPS leg of a run's one counted load, or nil: none was
// made, it was not measured, or the run is not one counted URL load.
func providerUrlProbeCountedIpv6Leg(run *egresshealth.Result) *egresshealth.CheckResult {
	load := providerUrlProbeCountedLoad(run)
	if load == nil || load.Ipv6 == nil || egresshealth.UrlProbeIpv6Tcp(load.Ipv6) == egresshealth.UrlProbeIpv6TcpNotMeasured {
		return nil
	}
	return load.Ipv6
}

// The measured IPv6 HTTP/3 leg of a run's one counted load, or nil.
func providerUrlProbeCountedIpv6H3Leg(run *egresshealth.Result) *egresshealth.CheckResult {
	leg := providerUrlProbeCountedIpv6Leg(run)
	if leg == nil || leg.H3 == nil || leg.H3.NotMeasured || egresshealth.UrlProbeH3Udp(leg.H3) == egresshealth.UrlProbeH3UdpNotMeasured {
		return nil
	}
	return leg.H3
}

// The legs a later-leg window judges, and a failed run's failures among them.
const (
	providerUrlProbeLegH3      = "h3"
	providerUrlProbeLegIpv6Tcp = "ipv6_tcp"
	providerUrlProbeLegIpv6Udp = "ipv6_udp"
)

// The later legs a failed run failed, when they are its only failures: its
// URL load's IPv4 HTTPS leg passed, with no TLS finding, and every failure came
// after it -- a silent IPv4 HTTP/3 leg (h3), a failed IPv6 HTTPS leg
// (ipv6_tcp), a silent IPv6 HTTP/3 leg (ipv6_udp). Nil for any other run.
func providerUrlProbeLaterLegFailures(run *egresshealth.Result) []string {
	load := providerUrlProbeCountedLoad(run)
	if load == nil || run.OkCount != 0 || run.TlsAuthenticationFailure || !load.HttpsOk() {
		return nil
	}
	failures := []string{}
	if leg := providerUrlProbeCountedH3Leg(run); leg != nil && egresshealth.UrlProbeH3Udp(leg) == egresshealth.UrlProbeH3UdpSilent {
		failures = append(failures, providerUrlProbeLegH3)
	}
	if leg := providerUrlProbeCountedIpv6Leg(run); leg != nil {
		switch egresshealth.UrlProbeIpv6Tcp(leg) {
		case egresshealth.UrlProbeIpv6TcpFailed:
			failures = append(failures, providerUrlProbeLegIpv6Tcp)
		case egresshealth.UrlProbeIpv6TcpPassed:
			if udp := providerUrlProbeCountedIpv6H3Leg(run); udp != nil && egresshealth.UrlProbeH3Udp(udp) == egresshealth.UrlProbeH3UdpSilent {
				failures = append(failures, providerUrlProbeLegIpv6Udp)
			}
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return failures
}

// A failed run whose only failure is its HTTP/3 leg: its URL declares HTTP/3,
// its HTTPS leg passed, and nothing answered its HTTP/3 leg's QUIC handshake
// (egresshealth.UrlProbeH3UdpSilent), the one HTTP/3 outcome that fails a
// load.
func providerUrlProbeH3OnlyFailure(run *egresshealth.Result) bool {
	return slices.Equal(providerUrlProbeLaterLegFailures(run), []string{providerUrlProbeLegH3})
}

// Whether a measured HTTP/3 leg reached its site over QUIC: it passed, or the
// site answered it. Either proves the platform's QUIC path through the
// provider, whatever the site then did.
func providerUrlProbeH3LegReachedSite(leg *egresshealth.CheckResult) bool {
	udp := egresshealth.UrlProbeH3Udp(leg)
	return udp == egresshealth.UrlProbeH3UdpPassed || udp == egresshealth.UrlProbeH3UdpAnswered
}

// One later leg's window: the latest measured leg of each of the pass's most
// recently measured networks, and whether it reached its site. A network's
// next leg, from any of its providers, replaces its last; a batch's providers
// of one network are one leg, which reached its site if any of theirs did.
type providerUrlProbeLegWindow struct {
	// oldest first
	networks    []providerUrlProbeCommonModeNetwork
	networkPass map[providerUrlProbeCommonModeNetwork]bool
	batchPass   map[providerUrlProbeCommonModeNetwork]bool
	tripped     bool
	heldCount   int
}

func newProviderUrlProbeLegWindow() *providerUrlProbeLegWindow {
	return &providerUrlProbeLegWindow{networkPass: map[providerUrlProbeCommonModeNetwork]bool{}}
}

// Starts a batch: its providers of one network are one leg.
func (self *providerUrlProbeLegWindow) beginBatch() {
	self.batchPass = map[providerUrlProbeCommonModeNetwork]bool{}
}

// Records the network's latest leg.
func (self *providerUrlProbeLegWindow) record(network providerUrlProbeCommonModeNetwork, pass bool) {
	if batchPass, ok := self.batchPass[network]; ok {
		pass = pass || batchPass
	}
	self.batchPass[network] = pass
	if _, ok := self.networkPass[network]; ok {
		self.networks = slices.DeleteFunc(self.networks, func(windowNetwork providerUrlProbeCommonModeNetwork) bool {
			return windowNetwork == network
		})
	}
	self.networks = append(self.networks, network)
	self.networkPass[network] = pass
	for providerUrlProbeCommonModeNetworkCount < len(self.networks) {
		delete(self.networkPass, self.networks[0])
		self.networks = slices.Delete(self.networks, 0, 1)
	}
}

// The window's networks and how many of their legs reached their sites.
func (self *providerUrlProbeLegWindow) counts() (networkCount int, passCount int) {
	for _, pass := range self.networkPass {
		if pass {
			passCount++
		}
	}
	return len(self.networks), passCount
}

// Whether the window is full and at most providerUrlProbeCommonModePassShare
// of its legs reached their sites.
func (self *providerUrlProbeLegWindow) commonMode() bool {
	networkCount, passCount := self.counts()
	return providerUrlProbeCommonModeNetworkCount <= networkCount &&
		float64(passCount) <= providerUrlProbeCommonModePassShare*float64(networkCount)
}

// Records the window's state change, and returns whether it entered or left
// common mode, with the held count of an episode that ended.
func (self *providerUrlProbeLegWindow) transition(commonMode bool) (entered bool, released bool, episodeHeldCount int) {
	if commonMode == self.tripped {
		return false, false, 0
	}
	self.tripped = commonMode
	episodeHeldCount = self.heldCount
	if !commonMode {
		self.heldCount = 0
	}
	return commonMode, !commonMode, episodeHeldCount
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
//
// Two more windows do the same for a dual-stack provider's IPv6 legs
// (url_family.go): the latest measured IPv6 HTTPS leg of each recent network,
// which passed or failed, and the latest measured IPv6 HTTP/3 leg, which
// reached its site or was silent. Every IPv6 leg follows an IPv4 HTTPS leg
// that passed through the same provider, so a fleet whose IPv6 legs all fail
// while their IPv4 legs pass is failing on the platform's own IPv6 path (the
// probe tunnel's, or the platform's routing of it). A run whose only failures
// are later legs -- a silent IPv4 HTTP/3 leg, a failed IPv6 HTTPS leg, a silent
// IPv6 HTTP/3 leg -- is held back when the window of every leg it failed is
// common-mode.
type providerUrlProbeCommonMode struct {
	shardIndex int
	shardCount int

	stateLock sync.Mutex
	// oldest first
	networks     []providerUrlProbeCommonModeNetwork
	networkTurns map[providerUrlProbeCommonModeNetwork]providerUrlProbeCommonModeTurn
	tripped      bool
	heldCount    int
	// the later legs' windows, by leg (providerUrlProbeLegH3 and the IPv6 legs)
	legWindows map[string]*providerUrlProbeLegWindow
	// counts a held run, once for each later leg it failed
	observeLegHolds func(leg string)
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

// The later legs, in the order their windows are judged and logged.
var providerUrlProbeLaterLegs = []string{providerUrlProbeLegH3, providerUrlProbeLegIpv6Tcp, providerUrlProbeLegIpv6Udp}

// An empty window for one shard pass.
func newProviderUrlProbeCommonMode(shardIndex int, shardCount int) *providerUrlProbeCommonMode {
	legWindows := map[string]*providerUrlProbeLegWindow{}
	for _, leg := range providerUrlProbeLaterLegs {
		legWindows[leg] = newProviderUrlProbeLegWindow()
	}
	return &providerUrlProbeCommonMode{
		shardIndex:   shardIndex,
		shardCount:   shardCount,
		networkTurns: map[providerUrlProbeCommonModeNetwork]providerUrlProbeCommonModeTurn{},
		legWindows:   legWindows,
		observeLegHolds: func(leg string) {
			switch leg {
			case providerUrlProbeLegH3:
				urlProbeH3CommonModeHeld.Inc()
			case providerUrlProbeLegIpv6Tcp:
				urlProbeIpv6CommonModeHeld.WithLabelValues("tcp").Inc()
			case providerUrlProbeLegIpv6Udp:
				urlProbeIpv6CommonModeHeld.WithLabelValues("udp").Inc()
			}
		},
	}
}

// The measured leg of one later leg's window in a run, or nil.
func providerUrlProbeLaterLeg(run *egresshealth.Result, leg string) *egresshealth.CheckResult {
	switch leg {
	case providerUrlProbeLegH3:
		return providerUrlProbeCountedH3Leg(run)
	case providerUrlProbeLegIpv6Tcp:
		return providerUrlProbeCountedIpv6Leg(run)
	case providerUrlProbeLegIpv6Udp:
		return providerUrlProbeCountedIpv6H3Leg(run)
	}
	return nil
}

// Whether a measured later leg reached its site: an HTTP/3 leg passed or was
// answered, an IPv6 HTTPS leg passed.
func providerUrlProbeLaterLegReachedSite(leg string, check *egresshealth.CheckResult) bool {
	if leg == providerUrlProbeLegIpv6Tcp {
		return egresshealth.UrlProbeIpv6Tcp(check) == egresshealth.UrlProbeIpv6TcpPassed
	}
	return providerUrlProbeH3LegReachedSite(check)
}

// One later-leg window's trip or release, and what it held, for the log.
type providerUrlProbeLegEpisode struct {
	leg              string
	entered          bool
	released         bool
	networkCount     int
	passCount        int
	episodeHeldCount int
}

// Records the batch's measured runs as turns of each provider's network, then
// returns the window's failed share and whether the batch is held back: it
// measured something, all of it pre-response failures, and the window is
// common-mode; or all of it later-leg failures (providerUrlProbeLaterLegFailures)
// each of whose windows is common-mode. A provider absent from
// providerClientIdNetworkIds stands as its own network. A batch that measured
// nothing changes nothing. The start and end of each episode are logged.
func (self *providerUrlProbeCommonMode) judge(
	providerClientIdRuns map[string]*egresshealth.Result,
	providerClientIdNetworkIds map[string]string,
) (failureShare float64, held bool) {
	entered, released := false, false
	networkCount, passCount, preResponseCount, episodeHeldCount := 0, 0, 0, 0
	episodes := []providerUrlProbeLegEpisode{}
	heldLegs := []string{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		measuredCount, measuredPreResponseCount := 0, 0
		measuredFailures := [][]string{}
		batchNetworkTurns := map[providerUrlProbeCommonModeNetwork]providerUrlProbeCommonModeTurn{}
		for _, window := range self.legWindows {
			window.beginBatch()
		}
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
			measuredFailures = append(measuredFailures, providerUrlProbeLaterLegFailures(run))
			network := providerUrlProbeCommonModeNetwork{networkId: providerClientIdNetworkIds[providerClientId]}
			if network.networkId == "" {
				network.providerClientId = providerClientId
			}
			for _, leg := range providerUrlProbeLaterLegs {
				if check := providerUrlProbeLaterLeg(run, leg); check != nil {
					// as below: one network's providers are one turn
					self.legWindows[leg].record(network, providerUrlProbeLaterLegReachedSite(leg, check))
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

		legCommonMode := map[string]bool{}
		for _, leg := range providerUrlProbeLaterLegs {
			legCommonMode[leg] = self.legWindows[leg].commonMode()
		}
		// every measured run failed only later legs, each in a common-mode
		// window
		legHeld := !held
		for _, failures := range measuredFailures {
			if len(failures) == 0 {
				legHeld = false
				break
			}
			for _, leg := range failures {
				legHeld = legHeld && legCommonMode[leg]
			}
		}
		if legHeld {
			held = true
			seen := map[string]bool{}
			for _, failures := range measuredFailures {
				for _, leg := range failures {
					if !seen[leg] {
						seen[leg] = true
						heldLegs = append(heldLegs, leg)
						self.legWindows[leg].heldCount++
					}
				}
			}
		}
		for _, leg := range providerUrlProbeLaterLegs {
			window := self.legWindows[leg]
			legEntered, legReleased, legEpisodeHeldCount := window.transition(legCommonMode[leg])
			if legEntered || legReleased {
				legNetworkCount, legPassCount := window.counts()
				episodes = append(episodes, providerUrlProbeLegEpisode{
					leg: leg, entered: legEntered, released: legReleased,
					networkCount: legNetworkCount, passCount: legPassCount, episodeHeldCount: legEpisodeHeldCount,
				})
			}
		}
	}()
	if self.observeLegHolds != nil {
		for _, leg := range heldLegs {
			self.observeLegHolds(leg)
		}
	}
	for _, episode := range episodes {
		switch {
		case episode.leg == providerUrlProbeLegH3 && episode.entered:
			// alert class: every recent HTTP/3 leg fails after its HTTPS leg passed
			glog.Errorf(
				"[egress]URL HTTP/3 common-mode guard tripped: shard=%d/%d networks=%d h3_passes=%d; URL outcomes that failed only their HTTP/3 leg are held back from provider history and their claims retried without measured credit until an HTTP/3 leg passes\n",
				self.shardIndex,
				self.shardCount,
				episode.networkCount,
				episode.passCount,
			)
		case episode.leg == providerUrlProbeLegH3 && episode.released:
			glog.Infof(
				"[egress]URL HTTP/3 common-mode guard released: shard=%d/%d held=%d; HTTP/3 outcomes are published again\n",
				self.shardIndex,
				self.shardCount,
				episode.episodeHeldCount,
			)
		case episode.entered:
			// alert class: every recent IPv6 leg of a transport fails after its
			// IPv4 HTTPS leg passed
			glog.Errorf(
				"[egress]URL IPv6 common-mode guard tripped: shard=%d/%d leg=%s networks=%d passes=%d; URL outcomes that failed only their IPv6 legs are held back from provider history and their claims retried without measured credit until an IPv6 leg reaches its site\n",
				self.shardIndex,
				self.shardCount,
				episode.leg,
				episode.networkCount,
				episode.passCount,
			)
		case episode.released:
			glog.Infof(
				"[egress]URL IPv6 common-mode guard released: shard=%d/%d leg=%s held=%d; IPv6 outcomes are published again\n",
				self.shardIndex,
				self.shardCount,
				episode.leg,
				episode.episodeHeldCount,
			)
		}
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
