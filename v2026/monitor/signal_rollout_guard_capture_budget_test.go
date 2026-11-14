package monitor

import (
	"context"
	"strings"
	"testing"
)

// The engineering observation envelope is not an existing product threshold.
// The unchanged production script is executed through the public adapter.
func TestRolloutGuardProducerEnvironmentObservationEnvelope(t *testing.T) {
	const proposedLimit = 64 * 1024
	for _, size := range []int{proposedLimit, proposedLimit + 1} {
		t.Run(map[bool]string{true: "at-cap", false: "N-plus-one"}[size == proposedLimit], func(t *testing.T) {
			settings := rolloutProducerSettings(t, map[string]rolloutProducerControl{
				"synthetic-service": {
					filesRows: true, runningRows: true, guard: rolloutGuardFull,
					environmentBytes: size, maxEnvironmentBytes: proposedLimit,
				},
			})
			alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal("source control did not reach public observation")
			}
			for _, alert := range alerts {
				if strings.Contains(alert.Markdown(), "synthetic-private-environment") {
					t.Fatal("Environment canary reached public Markdown")
				}
			}
			if size > proposedLimit {
				unknown := requireAlertClass(t, alerts, "cannot-observe")
				if !unknown.unobservable {
					t.Fatal("cutoff Environment supplied absence or recovery authority")
				}
			}
			if size == proposedLimit && len(alerts) != 0 {
				t.Fatal("complete at-cap healthy source was not preserved")
			}
		})
	}
}

// Ordinary discovery failure remains unknown, while the independent healthy
// and known-unsafe producer controls retain their original assertions.
func TestRolloutGuardObservationEnvelopeConservesProducerControls(t *testing.T) {
	TestRolloutGuardProducerDiscoveryFailureCannotBecomeUnmanaged(t)
	TestRolloutGuardProducerEnvironmentFailureCannotCertifyGuard(t)
	TestRolloutGuardProducerHealthyAndUnsafeControls(t)
	TestRolloutGuardProducerIndependentKnownGuardSurvives(t)
}

// A cutoff is source uncertainty, not a successful empty census. Bounded
// Environment/start failures preserve independently complete unsafe evidence.
func TestRolloutGuardBoundedDiscoveryAndIndependentEvidence(t *testing.T) {
	for _, stage := range []string{"files", "running", "environment", "start"} {
		t.Run(stage, func(t *testing.T) {
			control := rolloutProducerControl{filesRows: true, runningRows: true, guard: rolloutGuardFull}
			switch stage {
			case "files":
				control.filesBytes = rolloutGuardCaptureMaxBytes + 1
			case "running":
				control.runningBytes = rolloutGuardCaptureMaxBytes + 1
			case "environment":
				control.environmentBytes = rolloutGuardCaptureMaxBytes + 1
			case "start":
				control.startBytes = rolloutGuardCaptureMaxBytes + 1
			}
			settings := rolloutProducerSettings(t, map[string]rolloutProducerControl{
				"synthetic-incomplete":   control,
				"synthetic-known-unsafe": {filesRows: true, runningRows: true, guard: rolloutGuardDrainOnly},
			})
			alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal("bounded source did not reach public reducer")
			}
			requireAlertClass(t, alerts, "rollout-guard-stale")
			if stage == "start" {
				requireAlertClass(t, alerts, "rollout-guard-workers-stale")
			} else {
				requireAlertClass(t, alerts, "cannot-observe")
			}
			for _, alert := range alerts {
				requireAlertOmits(t, alert, "synthetic-private-environment", "synthetic-private-producer-error")
			}
		})
	}
}

// A stalled source reaches its owned timeout; caught TERM reaches the same
// cleanup. The harness requires direct Wait and removal of raw capture dirs.
func TestRolloutGuardCaptureStallAndCaughtCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{true: "collector-only-TERM", false: "producer-timeout"}[cancel], func(t *testing.T) {
			settings := rolloutProducerSettings(t, map[string]rolloutProducerControl{
				"synthetic-stalled": {
					filesRows: true, runningRows: true, guard: rolloutGuardFull,
					stallEnvironment: true, cancelCapture: cancel,
				},
			})
			alerts, err := NewRolloutGuardSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal("synthetic source lifecycle did not reach public visibility")
			}
			unknown := requireAlertClass(t, alerts, "cannot-observe")
			if !unknown.unobservable {
				t.Fatal("interrupted source supplied recovery authority")
			}
			for _, alert := range alerts {
				requireAlertOmits(t, alert, "synthetic-private-environment", "synthetic-private-producer-error")
			}
		})
	}
}
