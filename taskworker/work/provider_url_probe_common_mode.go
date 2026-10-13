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
type providerUrlProbeCommonMode struct {
	shardIndex int
	shardCount int

	stateLock sync.Mutex
	// oldest first
	networks     []providerUrlProbeCommonModeNetwork
	networkTurns map[providerUrlProbeCommonModeNetwork]providerUrlProbeCommonModeTurn
	tripped      bool
	heldCount    int
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
		shardIndex:   shardIndex,
		shardCount:   shardCount,
		networkTurns: map[providerUrlProbeCommonModeNetwork]providerUrlProbeCommonModeTurn{},
	}
}

// Records the batch's measured runs as turns of each provider's network, then
// returns the window's failed share and whether the batch is held back: it
// measured something, all of it pre-response failures, and the window is
// common-mode. A provider absent from providerClientIdNetworkIds stands as
// its own network. A batch that measured nothing changes nothing. The start
// and end of each episode are logged.
func (self *providerUrlProbeCommonMode) judge(
	providerClientIdRuns map[string]*egresshealth.Result,
	providerClientIdNetworkIds map[string]string,
) (failureShare float64, held bool) {
	entered, released := false, false
	networkCount, passCount, preResponseCount, episodeHeldCount := 0, 0, 0, 0
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		measuredCount, measuredPreResponseCount := 0, 0
		batchNetworkTurns := map[providerUrlProbeCommonModeNetwork]providerUrlProbeCommonModeTurn{}
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
			network := providerUrlProbeCommonModeNetwork{networkId: providerClientIdNetworkIds[providerClientId]}
			if network.networkId == "" {
				network.providerClientId = providerClientId
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
	}()

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
