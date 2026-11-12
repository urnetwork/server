// Capability classification of providers (FP2-PROTOCOL.md §3, §4.3): each
// provider gets an IP label (dualstack or ipv4) and a protocol label
// (dualproto or tcp), and one set of buckets judged on the legs of the
// capabilities it keeps.
//
// The labels come only from negative evidence (D7.4): a URL run that failed
// only on later legs judged the provider's, which the ingest stores as the
// run's failed_later_legs mask. A provider without such evidence keeps its
// advertised set: dualstack when its connections prove both families, else
// ipv4, and always dualproto. A provider is demoted only as far as its
// evidence requires, in the decided order (D7.2): the first class whose
// ratio passes with the runs it forgives, IPv6 dropped before UDP.
//
// The strict decision, today's hard gate, is untouched: today's facets, the
// location counts, the location stables and Connected keep reading it. The
// classified decision is today's decideProviderEgress fed with counts in
// which the class's forgiven runs are passes (classifiedEgressFacts), and it
// is what selector-mode FindProviders2 draws by.
package model

import (
	"fmt"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The IP and protocol labels, as FindProviders2 serves them
// (FindProvidersCapabilities) and selector-mode requests name them
// (FindProviders2Select).
const (
	ProviderCapabilityIpDualstack       = "dualstack"
	ProviderCapabilityIpIpv4            = "ipv4"
	ProviderCapabilityProtocolDualproto = "dualproto"
	ProviderCapabilityProtocolTcp       = "tcp"
)

// The ClientScore.CapabilityDowngrades bits (§4.3). Zero, as every entry an
// older exporter wrote decodes, is no downgrade: today's behaviour.
const (
	// the provider's IPv6 was dropped: labelled ipv4 though its connections
	// prove both families
	ClientScoreCapabilityDowngradeIpv6 uint8 = 1
	// the provider's UDP was dropped: labelled tcp
	ClientScoreCapabilityDowngradeUdp uint8 = 2
)

// The failed_later_legs bits of a URL run (§4.1), the later legs judged the
// provider's: legs made after the run's IPv4 HTTPS leg passed. The ingest's
// own constants, so the two cannot drift.
const (
	// IPv4 HTTP/3 silent, judged the provider's
	ProviderEgressFailedLaterLegIpv4Udp = egresshealth.UrlProbeFailedLaterLegIpv4Udp
	// IPv6 HTTPS failed, judged the provider's
	ProviderEgressFailedLaterLegIpv6Tcp = egresshealth.UrlProbeFailedLaterLegIpv6Tcp
	// IPv6 HTTP/3 silent after a passed IPv6 HTTPS leg, judged the provider's
	ProviderEgressFailedLaterLegIpv6Udp = egresshealth.UrlProbeFailedLaterLegIpv6Udp
)

// One of the four label pairs, in the decided order (D7.2).
type providerCapabilityClass uint8

const (
	// dualstack+dualproto, the full set
	providerCapabilityClassA providerCapabilityClass = iota
	// ipv4+dualproto
	providerCapabilityClassB
	// dualstack+tcp
	providerCapabilityClassC
	// ipv4+tcp
	providerCapabilityClassD
)

// Every class, in the decided order.
var providerCapabilityClasses = []providerCapabilityClass{
	providerCapabilityClassA,
	providerCapabilityClassB,
	providerCapabilityClassC,
	providerCapabilityClassD,
}

// The class's letter, for logs.
func (self providerCapabilityClass) String() string {
	switch self {
	case providerCapabilityClassA:
		return "A"
	case providerCapabilityClassB:
		return "B"
	case providerCapabilityClassC:
		return "C"
	case providerCapabilityClassD:
		return "D"
	default:
		return fmt.Sprintf("class(%d)", uint8(self))
	}
}

// The class's IP label.
func (self providerCapabilityClass) ip() string {
	switch self {
	case providerCapabilityClassA, providerCapabilityClassC:
		return ProviderCapabilityIpDualstack
	default:
		return ProviderCapabilityIpIpv4
	}
}

// The class's protocol label.
func (self providerCapabilityClass) protocol() string {
	switch self {
	case providerCapabilityClassA, providerCapabilityClassB:
		return ProviderCapabilityProtocolDualproto
	default:
		return ProviderCapabilityProtocolTcp
	}
}

// The class of a label pair.
func providerCapabilityClassOf(ip string, protocol string) providerCapabilityClass {
	dualstack := ip == ProviderCapabilityIpDualstack
	dualproto := protocol != ProviderCapabilityProtocolTcp
	switch {
	case dualstack && dualproto:
		return providerCapabilityClassA
	case dualproto:
		return providerCapabilityClassB
	case dualstack:
		return providerCapabilityClassC
	default:
		return providerCapabilityClassD
	}
}

// A set of classes, one bit per class.
type providerCapabilityClassSet uint8

// A set of the classes given.
func newProviderCapabilityClassSet(classes ...providerCapabilityClass) providerCapabilityClassSet {
	var set providerCapabilityClassSet
	for _, class := range classes {
		set |= 1 << class
	}
	return set
}

// Whether the set holds the class.
func (self providerCapabilityClassSet) has(class providerCapabilityClass) bool {
	return self&(1<<class) != 0
}

// The classes of both sets.
func (self providerCapabilityClassSet) intersect(other providerCapabilityClassSet) providerCapabilityClassSet {
	return self & other
}

// The classes of the set, in the decided order.
func (self providerCapabilityClassSet) classes() []providerCapabilityClass {
	classes := []providerCapabilityClass{}
	for _, class := range providerCapabilityClasses {
		if self.has(class) {
			classes = append(classes, class)
		}
	}
	return classes
}

// The downgrade bits that take a provider of the advertised family to class.
// A class above what the provider advertises cannot be reached and has none.
func providerCapabilityDowngradesOf(advertisedDualstack bool, class providerCapabilityClass) uint8 {
	var downgrades uint8
	if advertisedDualstack && class.ip() == ProviderCapabilityIpIpv4 {
		downgrades |= ClientScoreCapabilityDowngradeIpv6
	}
	if class.protocol() == ProviderCapabilityProtocolTcp {
		downgrades |= ClientScoreCapabilityDowngradeUdp
	}
	return downgrades
}

// The class a provider of the advertised family holds with the downgrades.
func providerCapabilityClassFor(advertisedDualstack bool, downgrades uint8) providerCapabilityClass {
	ip := ProviderCapabilityIpIpv4
	if advertisedDualstack && downgrades&ClientScoreCapabilityDowngradeIpv6 == 0 {
		ip = ProviderCapabilityIpDualstack
	}
	protocol := ProviderCapabilityProtocolDualproto
	if downgrades&ClientScoreCapabilityDowngradeUdp != 0 {
		protocol = ProviderCapabilityProtocolTcp
	}
	return providerCapabilityClassOf(ip, protocol)
}

// The provider's labels as FindProviders2 serves them (§4.3): ip is
// dualstack when the provider's proven family is dualstack and its IPv6 was
// not dropped, else ipv4; protocol is tcp when its UDP was dropped, else
// dualproto. A v6-only provider reads as ipv4: the two-axis model assumes
// IPv4, and selector mode never draws one (§3.6).
func (self *ClientScore) CapabilityLabels() (ip string, protocol string) {
	class := self.capabilityClass()
	return class.ip(), class.protocol()
}

// The class of the provider's labels.
func (self *ClientScore) capabilityClass() providerCapabilityClass {
	return providerCapabilityClassFor(self.IpFamily() == connect.IpFamilyDualstack, self.CapabilityDowngrades)
}

// The runs a class forgives out of the window's counts (§3.2): the failed
// runs whose later legs judged the provider's are all legs of the
// capabilities the class drops. The advertised class forgives nothing, and a
// v4-only provider has no IPv6 legs to forgive.
func providerCapabilityForgiven(counts ProviderEgressHealthCounts, advertisedDualstack bool, class providerCapabilityClass) int {
	switch {
	case advertisedDualstack && class == providerCapabilityClassB:
		return counts.ForgivenIpv4
	case advertisedDualstack && class == providerCapabilityClassC:
		return counts.ForgivenTcp
	case advertisedDualstack && class == providerCapabilityClassD:
		return counts.ForgivenBoth
	case !advertisedDualstack && class == providerCapabilityClassD:
		return counts.ForgivenTcp
	default:
		return 0
	}
}

// The classes a provider of the advertised family can hold, in the decided
// order, its advertised class first: A, B, C, D for a dual-stack provider,
// B, D for a v4-only one.
func providerCapabilityClassesFrom(advertisedDualstack bool) []providerCapabilityClass {
	if advertisedDualstack {
		return []providerCapabilityClass{
			providerCapabilityClassA,
			providerCapabilityClassB,
			providerCapabilityClassC,
			providerCapabilityClassD,
		}
	}
	return []providerCapabilityClass{
		providerCapabilityClassB,
		providerCapabilityClassD,
	}
}

// One provider's classification (§3.3).
type providerCapabilityClassification struct {
	advertisedDualstack bool
	class               providerCapabilityClass
	// the ClientScore.CapabilityDowngrades bits of class
	downgrades uint8
	// whether a class's ratio passed; false when the provider has too little
	// evidence for a verdict, or when no class passes and the labels drop
	// only the axes with negative evidence
	classPassed bool
}

// Whether the labels are below the advertised set.
func (self providerCapabilityClassification) downgraded() bool {
	return self.downgrades != 0
}

// Classifies a provider from its window counts (§3.3): the first class, in
// the decided order from its advertised class, whose ratio passes with the
// runs it forgives. A provider with too few runs for a verdict, or without
// current evidence, keeps its advertised labels: no negative measurement. A
// provider no class passes for drops only the axes with negative evidence:
// IPv6 for a run whose IPv6 HTTPS leg failed, UDP for a run whose HTTP/3 leg
// was silent. The ratio is today's comparison, so a provider that passes
// today's ratio is never downgraded.
func classifyProviderCapability(
	counts ProviderEgressHealthCounts,
	advertisedDualstack bool,
	now time.Time,
	settings *EgressIndexSettings,
) providerCapabilityClassification {
	classes := providerCapabilityClassesFrom(advertisedDualstack)
	classification := providerCapabilityClassification{
		advertisedDualstack: advertisedDualstack,
		class:               classes[0],
	}
	if counts.Total < ProviderOnlineMinMeasuredUrlRuns {
		return classification
	}
	if index := ComputeEgressIndex(&EgressHealthRun{MeasuredAt: counts.MeasuredAt, OkCount: counts.OKCount, Total: counts.Total}, now, settings); !index.Evidence {
		return classification
	}
	for _, class := range classes {
		forgiven := providerCapabilityForgiven(counts, advertisedDualstack, class)
		index := ComputeEgressIndex(&EgressHealthRun{MeasuredAt: counts.MeasuredAt, OkCount: counts.OKCount + forgiven, Total: counts.Total}, now, settings)
		if index.Evidence && index.Quality {
			classification.class = class
			classification.downgrades = providerCapabilityDowngradesOf(advertisedDualstack, class)
			classification.classPassed = true
			return classification
		}
	}
	// no class passes the ratio: drop only what has negative evidence
	var downgrades uint8
	if advertisedDualstack && 0 < counts.NegativeIp {
		downgrades |= ClientScoreCapabilityDowngradeIpv6
	}
	if 0 < counts.NegativeUdp {
		downgrades |= ClientScoreCapabilityDowngradeUdp
	}
	classification.class = providerCapabilityClassFor(advertisedDualstack, downgrades)
	classification.downgrades = downgrades
	return classification
}

// The window counts the classified decision reads (§3.4): the class's
// forgiven runs are passes, overall for the Quality and Speed ratio, the
// ranking index and the URL weight, and in the general half for the Online
// rule. Totals, minimum runs, span and the newest run are unchanged. Counts
// of a provider that is not downgraded are returned as they are.
func (self providerCapabilityClassification) classifiedCounts(counts ProviderEgressHealthCounts) ProviderEgressHealthCounts {
	if !self.downgraded() {
		return counts
	}
	classified := counts
	classified.OKCount += providerCapabilityForgiven(counts, self.advertisedDualstack, self.class)
	if counts.General != nil {
		general := *counts.General
		general.OKCount += providerCapabilityForgiven(*counts.General, self.advertisedDualstack, self.class)
		classified.General = &general
	}
	return classified
}

// The provider status's reason for each dropped capability (D11.6, §5.5),
// one sentence per label, tcp first. The counts are the runs in the window
// with negative evidence on the dropped axis: an HTTP/3 leg (mask & 5) for
// tcp, an IPv6 leg (mask & 6) for ipv4. A mask-4 run, UDP over IPv6, counts
// for whichever capability the class dropped for it, both for a provider
// that lost both.
type providerCapabilityReason struct {
	tcpCount  int
	ipv4Count int
	text      string
}

const (
	providerCapabilityReasonTcp  = "%d of these checks failed only over HTTP/3 (UDP), so this device is offered for TCP traffic only. Some app builds and some networks do not route UDP."
	providerCapabilityReasonIpv4 = "%d of these checks failed only over IPv6, so this device is offered for IPv4 traffic only."
)

// The reason of a classification over its window counts; the zero value for a
// provider that is not downgraded.
func (self providerCapabilityClassification) reason(counts ProviderEgressHealthCounts) providerCapabilityReason {
	reason := providerCapabilityReason{}
	if self.downgrades&ClientScoreCapabilityDowngradeUdp != 0 {
		reason.tcpCount = max(1, counts.NegativeUdp)
		reason.text = fmt.Sprintf(providerCapabilityReasonTcp, reason.tcpCount)
	}
	if self.downgrades&ClientScoreCapabilityDowngradeIpv6 != 0 {
		reason.ipv4Count = max(1, counts.NegativeIpv6)
		sentence := fmt.Sprintf(providerCapabilityReasonIpv4, reason.ipv4Count)
		if reason.text == "" {
			reason.text = sentence
		} else {
			reason.text += " " + sentence
		}
	}
	return reason
}
