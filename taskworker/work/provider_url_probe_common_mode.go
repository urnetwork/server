// One URL turn is a run batch of one provider, so the run batch guard can
// never see a failure that every provider shares. This guard judges each turn
// against the latest turns of the other providers its pass measured, and
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
	// Distinct providers the window holds, and needs before it judges: the
	// latest measured turn of each of the pass's most recently measured
	// providers. A provider claimed again after its failure pacing replaces
	// its own turn, so one broken provider never counts twice.
	providerUrlProbeCommonModeProviderCount = 16
	// Largest share of those turns that may pass. Normal URL failure is
	// 35-55% of turns, so this is far below any working fleet; at sixteen
	// providers it admits no pass at all.
	providerUrlProbeCommonModePassShare = 0.02
)

// Final stages of a URL attempt that ended before any destination answered:
// the target's DNS lookup through the tunnel, or a TCP connect through it that
// never completed. A platform failure on the tunnel path ends here, e.g.
// contract setup on both tunnel legs consuming the URL DNS phase
// (url_phase_timeout.go). TLS, policy, write, response and generic timeout
// stages are left out: a peer answered, or the phase is unknown.
var providerUrlProbePreResponseStages = map[string]bool{
	"dial_dns":             true,
	"dial_dns_or_socket":   true,
	"request_dns_timeout":  true,
	"dial_tcp":             true,
	"request_dial_timeout": true,
}

// A failed run that no destination answered: every counted load ended in a
// pre-response stage with no status, response byte, redirect or TLS verdict,
// and the run carries no security evidence. A run whose counted loads do not
// add up to its total is not evidence either way.
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
		if check.Ok || check.StatusCode != 0 || check.FirstByteReceived || check.RedirectCount != 0 ||
			check.TlsAuthenticationFailure || len(check.UrlProbeSecurity) != 0 ||
			!providerUrlProbePreResponseStages[check.FailureStage] {
			return false
		}
		countedLoadCount++
	}
	return countedLoadCount == run.Total
}

// The latest measured turn of each of one pass's most recently measured
// providers. The window is common-mode when it holds
// providerUrlProbeCommonModeProviderCount providers, at most
// providerUrlProbeCommonModePassShare of them passed, and every failure among
// them is pre-response: the fleet is failing before any destination answers,
// which is the platform's tunnel path, not each provider's. A pre-response
// failure judged in that window is held back through the run batch guard's
// release (model.ProbeRunBatchGuardClass): its claim completes for a paced
// retry with no URL outcome, quality or quota credit. A pass, a failure some
// destination answered and TLS evidence are never held, and each ends the
// episode as soon as it is recorded.
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
	providerClientIds     []string
	providerClientIdTurns map[string]providerUrlProbeCommonModeTurn
	tripped               bool
	heldCount             int
}

// One provider's latest measured turn.
type providerUrlProbeCommonModeTurn struct {
	ok          bool
	preResponse bool
}

// An empty window for one shard pass.
func newProviderUrlProbeCommonMode(shardIndex int, shardCount int) *providerUrlProbeCommonMode {
	return &providerUrlProbeCommonMode{
		shardIndex:            shardIndex,
		shardCount:            shardCount,
		providerClientIdTurns: map[string]providerUrlProbeCommonModeTurn{},
	}
}

// Records the batch's measured runs, then returns the window's failed share
// and whether the batch is held back: it measured something, all of it
// pre-response failures, and the window is common-mode. A batch that measured
// nothing changes nothing. The start and end of each episode are logged.
func (self *providerUrlProbeCommonMode) judge(providerClientIdRuns map[string]*egresshealth.Result) (failureShare float64, held bool) {
	entered, released := false, false
	providerCount, passCount, preResponseCount, episodeHeldCount := 0, 0, 0, 0
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		measuredCount, measuredPreResponseCount := 0, 0
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
			if _, ok := self.providerClientIdTurns[providerClientId]; ok {
				self.providerClientIds = slices.DeleteFunc(self.providerClientIds, func(windowClientId string) bool {
					return windowClientId == providerClientId
				})
			}
			self.providerClientIds = append(self.providerClientIds, providerClientId)
			self.providerClientIdTurns[providerClientId] = turn
			for providerUrlProbeCommonModeProviderCount < len(self.providerClientIds) {
				delete(self.providerClientIdTurns, self.providerClientIds[0])
				self.providerClientIds = slices.Delete(self.providerClientIds, 0, 1)
			}
		}

		providerCount = len(self.providerClientIds)
		for _, turn := range self.providerClientIdTurns {
			if turn.ok {
				passCount++
			} else if turn.preResponse {
				preResponseCount++
			}
		}
		if 0 < providerCount {
			failureShare = float64(providerCount-passCount) / float64(providerCount)
		}
		if measuredCount == 0 {
			return
		}
		commonMode := providerUrlProbeCommonModeProviderCount <= providerCount &&
			float64(passCount) <= providerUrlProbeCommonModePassShare*float64(providerCount) &&
			preResponseCount == providerCount-passCount
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
			"[egress]URL common-mode guard tripped: shard=%d/%d providers=%d passes=%d pre_response_failures=%d; pre-response URL failures are held back from provider history and their claims retried without measured credit until a provider passes or a destination answers\n",
			self.shardIndex,
			self.shardCount,
			providerCount,
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
