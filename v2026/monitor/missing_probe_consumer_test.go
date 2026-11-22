// The notifier's real incident-state reducer must retain a prior incident
// across unknown observations and clear only after complete recovery runs.
package monitor

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Observe actual public signal results in the same consumer state machine
// used by Slack Handle, without starting a sender or making a network call.
// Both new predicates have sustain one; the consumer's clear policy is two
// complete runs, with no absence credit from the two intervening unknowns.
func missingProbeConsumerRecovery(t *testing.T, signal Signal, incidentClass string, settingsForRun func(int) SignalSettings) {
	t.Helper()
	consumer := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	for run := 0; run < 5; run++ {
		settings := settingsForRun(run)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("source-path control did not reach the real consumer")
		}
		transitions := consumer.observeWithLock(settings.Now().Add(time.Duration(run)*time.Minute), signal.Key(), SignalExecution{Alerts: alerts})
		raised, cleared := 0, 0
		for _, transition := range transitions {
			if transition.alert.Class != incidentClass {
				continue
			}
			if transition.kind == slackRaised {
				raised++
			}
			if transition.kind == slackCleared {
				cleared++
			}
		}
		if run == 0 {
			if raised != 1 || cleared != 0 {
				t.Fatal("source-path incident did not enter consumer state exactly once")
			}
		} else if run < 4 {
			if raised != 0 || cleared != 0 {
				t.Fatal("unknown source or first recovery run changed the prior incident")
			}
		} else if raised != 0 || cleared != 1 {
			t.Fatal("two complete recovery observations did not clear the retained incident exactly once")
		}
	}
}

// The raw CSV path supplies off, two malformed observations and two supported
// on receipts. Shared CSV failure never receives healthy absence credit.
func TestPgHugePagesNativeConsumerRecovery(t *testing.T) {
	missingProbeConsumerRecovery(t, NewPgHugePagesSignal(), "pg-huge-pages-disabled", func(run int) SignalSettings {
		return pgHugePagesNativeTestSettings(t, func(string) string {
			if run == 1 || run == 2 {
				return "\"private-source-canary\n"
			}
			status := "on"
			if run == 0 {
				status = "off"
			}
			return fmt.Sprintf("pg-1,%d,primary,%s\n", syntheticSettings(nil).Now().Unix(), status)
		})
	})
}

// Actual generated-program exits for a current target with null service label
// are fed beside quiet older metrics. The owning consumer must keep its open
// rate incident until the complete current census and quiet window recover.
func TestJwtRejectionsCensusUnknownConsumerRecovery(t *testing.T) {
	missingProbeConsumerRecovery(t, NewJwtRejectionsSignal(), "jwt-signature-rejection-rate", func(run int) SignalSettings {
		rate, mode := 0.0, "healthy"
		if run == 0 {
			rate = 2
		}
		if run == 1 || run == 2 {
			mode = "null service"
		}
		return jwtRejectionsNativeSettings(t, rate, mode)
	})
}
