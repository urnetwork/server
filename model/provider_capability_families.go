// The capability classification of a provider by the families its
// connections prove, shared by the readers that decide providers one at a
// time from their rollup rows: the provider status and the dashboard counts
// (FP2-PROTOCOL.md §5.5, §8.1).
package model

import (
	"time"

	"github.com/urnetwork/connect"
)

// The classification of a provider whose connections prove ipFamilies
// (ClientScore.IpFamilies): advertised dualstack when they prove both, else
// ipv4. A v6-only provider is not classified (§3.6): the URL probe does not
// claim one and selector mode never draws one, so it keeps its advertised
// labels, ipv4 and dualproto as CapabilityLabels reads it, whatever its
// counts.
func classifyProviderCapabilityOfFamilies(
	counts ProviderEgressHealthCounts,
	ipFamilies uint8,
	now time.Time,
	settings *EgressIndexSettings,
) providerCapabilityClassification {
	ipFamily := (&ClientScore{IpFamilies: ipFamilies}).IpFamily()
	if ipFamily == connect.IpFamilyV6Only {
		return providerCapabilityClassification{class: providerCapabilityClassB}
	}
	return classifyProviderCapability(counts, ipFamily == connect.IpFamilyDualstack, now, settings)
}
