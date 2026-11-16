package work

// The prober's half of the uptime probe's verdict (connect/EXTENDER.md C3).
//
// A failed probe is evidence against an extender only when the prober could
// have passed it. A prober that loses its own path -- its route, its IPv6, its
// sockets -- fails every extender it probes at once, and a pass that charged
// those failures revoked the whole directory within half an hour, each
// extender then waiting on its own activator to come back (factury research
// extender-health-2026-11-11 §4 item 5). So before any failure of a pass
// counts, the pass is judged one family at a time, the way the URL probe's
// common-mode guard judges a turn (provider_url_probe_common_mode.go):
//
//   - common mode: the family's failures span its networks. One network's
//     extenders share an operator and often one outage, so the failures are
//     counted by network as well as by address: at least
//     ExtenderProbeCommonModeFailedShare of the family's addresses failed,
//     and at least that share of its networks lost half or more of their
//     addresses, with at least ExtenderProbeCommonModeMinNetworks networks and
//     ExtenderProbeCommonModeMinAddresses addresses in the pass. On Main a
//     healthy sweep fails 0 to 3% of its addresses, and the worst day seen, 1
//     November, 14 to 19%, nearly all of them on one operator's network; one
//     operator's whole fleet going down is one network of sixteen. A prober
//     fault fails nearly everything, everywhere.
//   - control failed: at least ExtenderProbeCommonModeFailedShare of the
//     family's addresses failed and the prober's own control target failed
//     over the family with them (extender_probe_control.go), after it had
//     answered in this process. This judges a family too small for the spread
//     to say anything. A control that fails while most of the family answers
//     is the control's fault, so a failed control never keeps a few dead
//     extenders listed.
//
// A held family's failures count against no one: no failure is recorded, no
// address is deactivated, no extender revoked, and no block report set aside.
// Its successes count as always, since an extender that signed a fresh
// challenge answered whatever else failed. Each held pass logs an error line,
// and the verdicts are counted (extender_probe_metrics.go).
//
// The guard keeps a dead extender listed only while most of its family fails
// with it: three quarters of the family's addresses, and with them either
// three quarters of its networks or the prober's own control target. An
// operator's outage, or a region's, short of that is judged as before, and
// its extenders still leave the directory after six probes.

import (
	"github.com/urnetwork/server"
)

const (
	// The fewest networks, and the fewest addresses, of one family a pass needs
	// before its failures say anything about the prober. With fewer they are a
	// statement about a handful of operators, and only the control target
	// judges the family.
	ExtenderProbeCommonModeMinNetworks  = 4
	ExtenderProbeCommonModeMinAddresses = 8
	// The share of a family's addresses, and of its networks, that failed at or
	// above which the family's pass is the prober's.
	ExtenderProbeCommonModeFailedShare = 0.75
	// The share of a network's addresses that failed at or above which the
	// network failed.
	ExtenderProbeCommonModeNetworkFailedShare = 0.5
)

// The verdicts of one family's pass.
const (
	// the failures count, as they always did
	extenderProbeVerdictCounted = "counted"
	// the failures span the family's networks: the prober's
	extenderProbeVerdictCommonMode = "common_mode"
	// the prober's own control target failed over the family
	extenderProbeVerdictControlFailed = "control_failed"
)

// One active address's result, as the verdict reads it.
type extenderProbeJudgedResult struct {
	networkId server.Id
	failed    bool
}

// One family's verdict, and the counts it was decided on.
type extenderProbeFamilyJudgement struct {
	ipVersion          int
	addressCount       int
	failedAddressCount int
	networkCount       int
	failedNetworkCount int
	verdict            string
}

// Whether the family's failures count against no one.
func (self *extenderProbeFamilyJudgement) held() bool {
	return self.verdict != extenderProbeVerdictCounted
}

// Judges one family's pass from its active addresses' results, and whether the
// prober's control target failed over the family. A pass that ended before a
// probe could answer leaves that probe out of results.
func judgeExtenderProbeFamily(
	ipVersion int,
	results []extenderProbeJudgedResult,
	controlFailed bool,
) *extenderProbeFamilyJudgement {
	judgement := &extenderProbeFamilyJudgement{
		ipVersion: ipVersion,
		verdict:   extenderProbeVerdictCounted,
	}
	type networkResults struct {
		addressCount       int
		failedAddressCount int
	}
	networks := map[server.Id]*networkResults{}
	for _, result := range results {
		network, ok := networks[result.networkId]
		if !ok {
			network = &networkResults{}
			networks[result.networkId] = network
		}
		judgement.addressCount += 1
		network.addressCount += 1
		if result.failed {
			judgement.failedAddressCount += 1
			network.failedAddressCount += 1
		}
	}
	judgement.networkCount = len(networks)
	for _, network := range networks {
		if ExtenderProbeCommonModeNetworkFailedShare*float64(network.addressCount) <= float64(network.failedAddressCount) {
			judgement.failedNetworkCount += 1
		}
	}

	// most of the family failed: the prober's, if the failures are spread
	// over the networks or the prober's own control failed with them. A
	// control that fails while most of the family answers is the control's
	// fault, not the prober's, and holds nothing.
	mostFailed := 0 < judgement.failedAddressCount &&
		ExtenderProbeCommonModeFailedShare*float64(judgement.addressCount) <= float64(judgement.failedAddressCount)
	spread := ExtenderProbeCommonModeMinNetworks <= judgement.networkCount &&
		ExtenderProbeCommonModeMinAddresses <= judgement.addressCount &&
		ExtenderProbeCommonModeFailedShare*float64(judgement.networkCount) <= float64(judgement.failedNetworkCount)
	switch {
	case mostFailed && controlFailed:
		judgement.verdict = extenderProbeVerdictControlFailed
	case mostFailed && spread:
		judgement.verdict = extenderProbeVerdictCommonMode
	}
	return judgement
}
