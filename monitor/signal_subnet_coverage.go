package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §22.0 maps to signal_subnet_coverage.go and
// signal_subnet_coverage_test.go. This signal is deliberately a coverage
// sentinel: it stays non-green while any independently replayed §22.1--§22.9
// family reader is unregistered, rather than pretending infrastructure health
// proves subnet economic correctness. It retires family by family: a family
// leaves the warning once a signal with its section number is registered.
// The registered readers then report their own unknown sources; this
// sentinel never certifies an economic invariant.
func NewSubnetCoverageSignal() Signal {
	return &signalAdapter{
		number: "22.0",
		key:    "subnet-coverage",
		name:   "Subnet correctness observation coverage",
		probe:  subnetCoverageProbe{registered: subnetRegisteredFamilies},
	}
}

// The nine correctness families and the readers each one needs.
var subnetCorrectnessFamilies = []struct {
	number  string
	missing string
}{
	{number: "22.1", missing: "finalized chain, deployment, policy, and governance readers"},
	{number: "22.2", missing: "pool, validator, fleet, and commitment readers"},
	{number: "22.3", missing: "deposit, conviction, and reserve replay"},
	{number: "22.4", missing: "validation-path, attempt, and quality replay"},
	{number: "22.5", missing: "candidate-census, head, promotion, and tier replay"},
	{number: "22.6", missing: "weight-lifecycle, consensus, and dividend replay"},
	{number: "22.7", missing: "capture, root, claim, carry, and conservation replay"},
	{number: "22.8", missing: "artifact, anchor, history, and audit replay"},
	{number: "22.9", missing: "process, resource, quota, funding, and adversarial readers"},
}

// The family section numbers of the production registry. A partial reader
// with a lettered number, such as §22.8a, is not a family.
var subnetRegisteredFamiliesOnce sync.Once
var subnetRegisteredFamiliesValue map[string]bool

func subnetRegisteredFamilies() map[string]bool {
	subnetRegisteredFamiliesOnce.Do(func() {
		subnetRegisteredFamiliesValue = map[string]bool{}
		for _, signal := range NewSignals() {
			subnetRegisteredFamiliesValue[signal.Number()] = true
		}
	})
	return subnetRegisteredFamiliesValue
}

// The registry view is injectable so retirement can be tested per family.
type subnetCoverageProbe struct {
	registered func() map[string]bool
}

func (subnetCoverageProbe) id() string             { return "subnet/coverage" }
func (subnetCoverageProbe) tier() string           { return tierWarn }
func (subnetCoverageProbe) cadence() time.Duration { return 5 * time.Minute }

func (self subnetCoverageProbe) check(_ context.Context, env *probeEnv) ([]finding, error) {
	status := env.cfg.stConfigStatus.normalized()
	if status == STConfigurationExplicitDisabled {
		return []finding{healthyFinding(
			"subnet/coverage",
			tierWarn,
			"subnet-monitor-coverage",
			"subnet-correctness",
		)}, nil
	}

	registered := map[string]bool{}
	if self.registered != nil {
		registered = self.registered()
	}
	missingNumbers := []string{}
	missingPrerequisites := []string{}
	for _, family := range subnetCorrectnessFamilies {
		if !registered[family.number] {
			missingNumbers = append(missingNumbers, family.number)
			missingPrerequisites = append(missingPrerequisites, "§"+family.number+" "+family.missing)
		}
	}
	if len(missingNumbers) == 0 {
		return []finding{healthyFinding(
			"subnet/coverage",
			tierWarn,
			"subnet-monitor-coverage",
			"subnet-correctness",
		)}, nil
	}

	state := string(status)
	if status == STConfigurationUnknown {
		if env.cfg.verificationEnabled {
			state = "enabled-with-unknown-configuration-authority"
		} else {
			state = "unknown"
		}
	}

	mechanism := "The subnet desired state is not proved disabled, but some independent §22.1--§22.9 correctness readers are not registered. Infrastructure health and an empty metric response therefore cannot establish scoring, custody, or settlement correctness for those families."
	if status == STConfigurationUnavailable || status == STConfigurationEnabledInvalid {
		mechanism = "The subnet desired-state configuration is unavailable or invalid, and some independent §22.1--§22.9 correctness readers are also absent. The monitor cannot decide whether economic work is deployed or verify its correctness."
	}

	return []finding{{
		probeId:   "subnet/coverage",
		tier:      tierWarn,
		class:     "subnet-monitor-coverage",
		target:    "subnet-correctness",
		sustain:   1,
		symptom:   "Subnet correctness coverage is incomplete",
		mechanism: mechanism,
		baseline:  "An enabled subnet has bounded, authenticated, independently replayed observers for every §22.1--§22.9 family; an intentionally undeployed subnet is explicitly disabled by its authoritative configuration.",
		observed:  fmt.Sprintf("desired_state=%s implemented_correctness_families=%d required_correctness_families=%d missing_families=%s", state, len(subnetCorrectnessFamilies)-len(missingNumbers), len(subnetCorrectnessFamilies), strings.Join(missingNumbers, ",")),
		evidence:  "missing_source_prerequisites=" + strings.Join(missingPrerequisites, "; "),
		action:    "Implement the missing typed readers and one focused signal/test pair per §22 family, then qualify them against finalized chain state and retained artifacts. Until then, treat those families' scoring and settlement health as unknown; do not infer green from §17 node health or log silence.",
		verify:    "Each missing family is registered and decides its invariants from a complete bounded census at explicit finalized hashes, with violated/healthy/unknown/cancellation/redaction tests. Production paging additionally waits until each reader is independently qualified against testnet evidence and peer review (§22.10).",
		playbook:  "SIGNALS.md §22.0",
	}}, nil
}
