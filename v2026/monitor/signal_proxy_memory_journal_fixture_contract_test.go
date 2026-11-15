package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Replays the exact stale tuple through the real parser and public entry.
// This diagnostic does not modify the original three failing assertions.
func TestProxyMemoryJournalFixtureAuthorityDiscriminator(t *testing.T) {
	t.Run("unqualified-canary-tuple", func(t *testing.T) {
		sample := proxyMemoryDeltaFixtureSample()
		sample.oomLine = "synthetic-private-journal"
		frame := proxyMemoryFixture(sample)
		_, parseErr := parseProxyMemorySample(frame)
		exactReason := parseErr != nil && parseErr.Error() == "proxy memory: invalid qualified kernel OOM record"
		source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) { return frame, nil }}
		alerts, err := NewProxyMemorySignal().Run(context.Background(), proxyMemorySyntheticSettings(source))
		if err != nil || len(alerts) != 1 {
			t.Fatal("fixture diagnostic did not reach one public observation")
		}
		alert := alerts[0]
		t.Logf("fixture_contract invalid_tuple=true alert_class=%s target=%s unobservable=%t exact_parse_reason=%t", alert.Class, alert.Target, alert.unobservable, exactReason)
		if !exactReason || alert.Class != "cannot-observe" || alert.Target != "proxy-1/proxy-memory" || !alert.unobservable {
			t.Fatal("stale fixture did not reach the predicted parser and visibility boundary")
		}
		requireAlertOmits(t, alert, "synthetic-private-journal")
	})

	for _, incomplete := range []bool{false, true} {
		name := "complete-fixture-private-detail"
		if incomplete {
			name = "independent-udp-with-journal-visibility"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			sample := proxyMemoryDeltaFixtureSample()
			sample.udpInDatagrams = 10_000
			sample.udpRcvbufErrors = 500
			if incomplete {
				sample.kernelJournalReason = "byte-bound"
				sample.kernelJournalComplete = false
				sample.kernelJournalBytes = proxyKernelJournalMaxBytes + 1
			}
			canary := "synthetic-private-journal"
			source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
				return proxyMemoryFixture(sample) + "ignored_detail " + canary + "\n", nil
			}}
			settings := proxyMemorySyntheticSettings(source)
			settings.Now = func() time.Time { return now }
			signal := NewProxyMemorySignal()
			alerts, err := signal.Run(context.Background(), settings)
			wantWarm := 0
			if incomplete {
				wantWarm = 1
			}
			if err != nil || len(alerts) != wantWarm {
				t.Fatal("valid fixture changed its initial warmup or independent journal visibility")
			}
			now = now.Add(time.Minute)
			sample.udpInDatagrams += 10_000
			sample.udpRcvbufErrors += 250
			alerts, err = signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != wantWarm+1 {
				t.Fatal("independent UDP evidence was lost behind journal availability")
			}
			drops := requireAlertClass(t, alerts, "proxy-udp-receive-drops")
			if drops.Identity() != "proxy/host-memory|proxy-udp-receive-drops|proxy-1|receive-buffer" || drops.Severity != SeverityPage || drops.Sustain != 1 {
				t.Fatal("valid fixture changed the UDP positive identity or policy")
			}
			if incomplete {
				visibility := requireAlertClass(t, alerts, "cannot-observe")
				if !visibility.unobservable || visibility.Target != "proxy-1/proxy-kernel-oom" {
					t.Fatal("journal visibility lost its separate source identity")
				}
			}
			for _, alert := range alerts {
				requireAlertOmits(t, alert, canary)
			}
			var encoded strings.Builder
			if alerts.WriteJSONL(&encoded) != nil || strings.Contains(encoded.String(), canary) {
				t.Fatal("private fixture detail entered public JSON")
			}
			t.Logf("fixture_contract invalid_tuple=false journal_incomplete=%t udp_page=true privacy_preserved=true", incomplete)
		})
	}
}
