package controller

import (
	"encoding/hex"
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// Capability eligibility of the gated tier (connect/EXTENDER-UPDATE2.md §3.2,
// connect/EXTENDER.md P and R3).
//
// A Phase B extender splices an unauthenticated tcp hello to its borrowed
// site. A client that cannot send the authenticated camouflage hello therefore
// reaches the borrowed site on that carrier instead of the extender, and fails
// its identity check. The operator names such extenders in `extender.yml`:
//
//	directory:
//	  capability_cohorts:
//	    - capability: tcp-camouflage-v1
//	      extender_public_keys_hex:
//	        - <the extender's identity public key, hex>
//
// Every record signed for a listed extender then requires the capability
// (ExtenderRecordBody.RequiredCapabilities). A release hands such a record only
// to a client that declares the capability (ExtenderReleaseArgs.Capabilities).
// The rule is narrower than the list, so that no client is left holding a
// record it cannot use:
//
//   - Only an extender that a release alone reaches carries a requirement: the
//     gated tier, ordinary or a gated canary, as the release reads it
//     (model.GetActiveGatedNetworkExtenders). An open channel cannot ask a
//     client what it can do, so an open or dns canary record never requires
//     anything. The extender's own Phase B gate never splices unless its record
//     carries the requirement.
//   - tcp-camouflage-v1 is required only of a record that carries the
//     camouflage key (RealityPublicKey); without the key, no client can
//     authenticate to the camouflaged carrier. The server does not sign the key
//     yet (connect/EXTENDER.md phase 14b), so a cohort is inert until it does.
//   - Capabilities are trimmed and compared in lower case, and signed that
//     way. The server keeps a capability it does not know as a requirement
//     that no client satisfies. A misspelled capability withholds the extender
//     from every release rather than handing it to clients that cannot use it.
//
// Capabilities decide eligibility only. The identity's count, partition and
// epoch disclosures are the same whatever a client declares (connect
// ExtenderReleasePolicy). docs/operations/extender-capability-cohort.md is the
// rollout and rollback runbook.

// The capability of a client that sends the authenticated camouflage hello and
// works with spliced extenders (contract R). Slice F's connect constant
// replaces this once it is on connect main.
const extenderCapabilityTcpCamouflageV1 = "tcp-camouflage-v1"

// The capabilities this server knows. A requirement outside it is never
// satisfied, and a declared value outside it is ignored.
var extenderKnownCapabilities = []string{
	extenderCapabilityTcpCamouflageV1,
}

// One cohort of extenders whose records require a capability.
type ExtenderCapabilityCohortConfig struct {
	Capability            string   `yaml:"capability"`
	ExtenderPublicKeysHex []string `yaml:"extender_public_keys_hex"`
}

// How much of a client's declared list is read, as connect reads one: the
// first extenderMaxOfferedCapabilityCount values, each at most
// extenderMaxOfferedCapabilityByteCount bytes.
const (
	extenderMaxOfferedCapabilityCount     = 16
	extenderMaxOfferedCapabilityByteCount = 64
)

// A capability as it is compared and signed: trimmed and lower case.
func normalizeExtenderCapability(capability string) string {
	return strings.ToLower(strings.TrimSpace(capability))
}

// The normalized values of a declared list that are read.
func extenderOfferedCapabilities(offered []string) map[string]bool {
	offeredCapabilities := map[string]bool{}
	for i, capability := range offered {
		if extenderMaxOfferedCapabilityCount <= i {
			break
		}
		if extenderMaxOfferedCapabilityByteCount < len(capability) {
			continue
		}
		offeredCapabilities[normalizeExtenderCapability(capability)] = true
	}
	return offeredCapabilities
}

// Whether `offered` covers every one of `required`, with the semantics of
// slice F's connect.ExtenderCapabilitiesSatisfied, which replaces this once it
// is on connect main: an unknown required value is unsatisfied, an empty one
// is skipped, an unknown offered value is ignored, values compare trimmed and
// case-insensitively, and only the bounded head of `offered` is read.
func extenderCapabilitiesSatisfied(required []string, offered []string) bool {
	offeredCapabilities := extenderOfferedCapabilities(offered)
	for _, capability := range required {
		capability = normalizeExtenderCapability(capability)
		if capability == "" {
			continue
		}
		if !slices.Contains(extenderKnownCapabilities, capability) || !offeredCapabilities[capability] {
			return false
		}
	}
	return true
}

// The capabilities a client declared that this server knows, each once, in
// the known order, read as extenderCapabilitiesSatisfied reads them.
// Everything else a client sends is ignored.
func extenderDeclaredCapabilities(capabilities []string) []string {
	offeredCapabilities := extenderOfferedCapabilities(capabilities)
	declared := []string{}
	for _, capability := range extenderKnownCapabilities {
		if offeredCapabilities[capability] {
			declared = append(declared, capability)
		}
	}
	return declared
}

// Whether a record of the extender reaches clients only through a release,
// the one path that can ask a client what it can use. This is the population
// model.GetActiveGatedNetworkExtenders reads.
func extenderReleaseOnly(extender *model.NetworkExtender) bool {
	if extender.DirectoryTier != connect.ExtenderDirectoryTierGated {
		return false
	}
	return extender.CanaryChannel == "" || extender.CanaryChannel == connect.ExtenderChannelGated
}

// RecordRequiredCapabilities is what a record of the extender requires, sorted:
// each configured cohort that lists its key, where the record can honor it (see
// above). `realityPublicKey` is the camouflage key the same record carries.
func (self *ExtenderConfig) RecordRequiredCapabilities(
	extender *model.NetworkExtender,
	realityPublicKey []byte,
) []string {
	required := []string{}
	if len(self.Directory.CapabilityCohorts) == 0 {
		return required
	}
	keyHex := hex.EncodeToString(extender.PublicKey)
	for _, cohort := range self.Directory.CapabilityCohorts {
		// signed as compared, so the extender's own gate and every client
		// read the same value
		capability := normalizeExtenderCapability(cohort.Capability)
		if capability == "" || !extenderCohortListsKey(cohort, keyHex) {
			continue
		}
		label := extenderCapabilityLabel(capability)
		if !extenderReleaseOnly(extender) {
			defaultExtenderCapabilityMetrics.observe(label, extenderCapabilityResultNotReleaseOnly)
			continue
		}
		if capability == extenderCapabilityTcpCamouflageV1 && len(realityPublicKey) != extenderRealityPublicKeySize {
			defaultExtenderCapabilityMetrics.observe(label, extenderCapabilityResultNoCamouflageKey)
			continue
		}
		defaultExtenderCapabilityMetrics.observe(label, extenderCapabilityResultRequired)
		if !slices.Contains(required, capability) {
			required = append(required, capability)
		}
	}
	slices.Sort(required)
	return required
}

// The length of the camouflage key a record carries, an X25519 public key
// (connect/EXTENDER.md P1, P6).
const extenderRealityPublicKeySize = 32

// The camouflage key a record of the extender carries (connect/EXTENDER.md
// P6). The server does not store the key yet (phase 14b), so no record carries
// one and no cohort requirement is set. 14b replaces this with the stored key,
// and the requirement follows the signed body. Tests install a key here.
var extenderRecordRealityPublicKey = func(extender *model.NetworkExtender) []byte {
	return nil
}

// Whether a cohort lists the key. Entries are compared as lower case hex; one
// that is not a key simply never matches.
func extenderCohortListsKey(cohort ExtenderCapabilityCohortConfig, keyHex string) bool {
	for _, listed := range cohort.ExtenderPublicKeysHex {
		if strings.ToLower(strings.TrimSpace(listed)) == keyHex {
			return true
		}
	}
	return false
}

// Reports every configured capability the server does not know, when the
// configuration is read (once per process). Such a cohort requires what no
// client can declare, so its extenders are withheld from every release until
// the entry is corrected. The value is the operator's own configuration.
func warnUnknownExtenderCapabilities(config *ExtenderConfig) {
	for _, cohort := range config.Directory.CapabilityCohorts {
		capability := normalizeExtenderCapability(cohort.Capability)
		if capability != "" && !slices.Contains(extenderKnownCapabilities, capability) {
			glog.Errorf("[extender]capability cohort %q is not a known capability; its extenders are withheld from every release\n", capability)
		}
	}
}

// How the requirement of a cohort member's record was decided
// (urnetwork_extender_record_capability_requirements_total).
const (
	// the record requires the capability
	extenderCapabilityResultRequired = "required"
	// the record carries no camouflage key, so it requires nothing
	extenderCapabilityResultNoCamouflageKey = "no_camouflage_key"
	// the extender is reached by an open channel, so it requires nothing
	extenderCapabilityResultNotReleaseOnly = "not_release_only"
)

// The capability label: a known capability, or unknown for a configured one
// the server does not know.
const extenderCapabilityLabelUnknown = "unknown"

func extenderCapabilityLabel(capability string) string {
	if slices.Contains(extenderKnownCapabilities, capability) {
		return capability
	}
	return extenderCapabilityLabelUnknown
}

type extenderCapabilityMetrics struct {
	requirements *prometheus.CounterVec
}

func newExtenderCapabilityMetrics(registerer prometheus.Registerer) *extenderCapabilityMetrics {
	metrics := &extenderCapabilityMetrics{
		requirements: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_extender_record_capability_requirements_total",
			Help: "Records signed for an extender in a configured capability cohort (extender.yml directory.capability_cohorts), by capability (unknown for a configured value the server does not know) and how its requirement was decided: " +
				"required, the record requires the capability; no_camouflage_key, the record carries no camouflage key, so tcp-camouflage-v1 is not required; " +
				"not_release_only, the extender is reached by an open channel, so nothing is required.",
		}, []string{"capability", "result"}),
	}
	for _, capability := range append(slices.Clone(extenderKnownCapabilities), extenderCapabilityLabelUnknown) {
		for _, result := range []string{
			extenderCapabilityResultRequired,
			extenderCapabilityResultNoCamouflageKey,
			extenderCapabilityResultNotReleaseOnly,
		} {
			metrics.requirements.WithLabelValues(capability, result)
		}
	}
	registerer.MustRegister(metrics.requirements)
	return metrics
}

var defaultExtenderCapabilityMetrics = newExtenderCapabilityMetrics(prometheus.DefaultRegisterer)

// Counts one cohort member's requirement decision. Nil counts nothing.
func (self *extenderCapabilityMetrics) observe(capability string, result string) {
	if self == nil {
		return
	}
	self.requirements.WithLabelValues(capability, result).Inc()
}
