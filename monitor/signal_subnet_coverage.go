package monitor

import (
	"context"
	"fmt"
	"time"
)

// SIGNALS.md §22.0 maps to signal_subnet_coverage.go and
// signal_subnet_coverage_test.go. This signal is deliberately a coverage
// sentinel: it must remain non-green while the independently replayed §22.1--
// §22.9 readers are unavailable, rather than pretending infrastructure health
// proves subnet economic correctness.
func NewSubnetCoverageSignal() Signal {
	return &signalAdapter{
		number: "22.0",
		key:    "subnet-coverage",
		name:   "Subnet correctness observation coverage",
		probe:  subnetCoverageProbe{},
	}
}

type subnetCoverageProbe struct{}

func (subnetCoverageProbe) id() string             { return "subnet/coverage" }
func (subnetCoverageProbe) tier() string           { return tierWarn }
func (subnetCoverageProbe) cadence() time.Duration { return 5 * time.Minute }

func (subnetCoverageProbe) check(_ context.Context, env *probeEnv) ([]finding, error) {
	status := env.cfg.stConfigStatus.normalized()
	if status == STConfigurationExplicitDisabled {
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

	missing := "finalized native/EVM checkpoint readers; complete operator, validator, fleet, and candidate census; decision-history and artifact replay; settlement and custody conservation replay; process/resource ownership exporters"
	mechanism := "The subnet desired state is not proved disabled, but the independent §22.1--§22.9 correctness readers are not registered. Infrastructure health and an empty metric response therefore cannot establish scoring, custody, or settlement correctness."
	if status == STConfigurationUnavailable || status == STConfigurationEnabledInvalid {
		mechanism = "The subnet desired-state configuration is unavailable or invalid, and the independent §22.1--§22.9 correctness readers are also absent. The monitor cannot decide whether economic work is deployed or verify its correctness."
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
		observed:  fmt.Sprintf("desired_state=%s implemented_correctness_families=0 required_correctness_families=9", state),
		evidence:  "missing_source_prerequisites=" + missing,
		action:    "Implement the missing typed readers and one focused signal/test pair per §22 family, then qualify them against finalized chain state and retained artifacts. Until then, treat scoring and settlement health as unknown; do not infer green from §17 node health or log silence.",
		verify:    "Every required family is registered, reads a complete bounded census at explicit finalized hashes, passes violated/healthy/unknown/cancellation/redaction tests, and the coverage sentinel reports healthy only after those capabilities are independently qualified.",
		playbook:  "SIGNALS.md §22.0",
	}}, nil
}
