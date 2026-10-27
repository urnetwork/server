// Publisher connection visibility retains only the same validated sample's
// finite prerequisites; unknown evidence never becomes a connection verdict.
package monitor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// Check the public identity and rendered tuple at the causal loss boundary.
func requireMimirPublisherConnectionVisibility(t *testing.T, alerts Alerts, tuple string) Alert {
	t.Helper()
	var selected Alert
	count := 0
	for _, alert := range alerts {
		if alert.Target == "pg-primary/mimir-publisher/connections" {
			selected = alert
			count++
		}
		if alert.Target == "pg-primary/mimir-publisher" && alert.Class == "mimir-publisher-connection-drift" {
			t.Fatal("unknown connection prerequisites produced a connection-drift verdict")
		}
		requireAlertOmits(t, alert, "database.invalid", "cache.invalid", "front-a.invalid", "front-b.invalid", "192.0.2.", "synthetic-private")
	}
	if count != 1 {
		t.Fatalf("connection visibility count=%d, want 1", count)
	}
	if selected.Identity() != "monitor/visibility|cannot-observe|pg-primary/mimir-publisher/connections|" ||
		selected.Severity != SeverityWarn || selected.Sustain != 2 || selected.PageSustain != 0 ||
		selected.SignalNumber != "11.20c" || selected.SignalKey != "mimir-publishers" || !selected.unobservable {
		t.Fatal("connection visibility changed its identity, severity, sustain, or unknown state")
	}
	want := "error_class=observation-state-unavailable " + tuple
	if selected.Observed != want {
		t.Fatalf("connection visibility discarded valid prerequisite tuple: got %q, want %q", selected.Observed, want)
	}
	if !strings.Contains(selected.Markdown(), want) {
		t.Fatal("connection visibility Markdown omitted the validated prerequisite tuple")
	}
	if strings.Contains(selected.Mechanism, "source command, parser, or network path failed") {
		t.Fatal("completed publisher reducer was misattributed to a command, parser, or network failure")
	}
	return selected
}

// Every admitted gate alternative has a finite, same-attempt discriminator.
func TestMimirPublishersConnectionVisibilityRetainsPrerequisiteTuple(t *testing.T) {
	controls := []struct {
		name      string
		overrides map[string]string
		tuple     string
	}{
		{
			name:      "socket ownership unavailable",
			overrides: map[string]string{"connections_observable": "false"},
			tuple:     "fluent_bit_active=true process_observable=true route_state=expected connections_observable=false connections_total=2",
		},
		{
			name:      "inactive process",
			overrides: map[string]string{"fluent_bit_active": "false"},
			tuple:     "fluent_bit_active=false process_observable=true route_state=expected connections_observable=true connections_total=2",
		},
		{
			name:      "unobservable generation",
			overrides: map[string]string{"process_observable": "false", "route_state": "unobservable", "connections_observable": "false"},
			tuple:     "fluent_bit_active=true process_observable=false route_state=unobservable connections_observable=false connections_total=2",
		},
		{
			name:      "known different route",
			overrides: map[string]string{"route_state": "different"},
			tuple:     "fluent_bit_active=true process_observable=true route_state=different connections_observable=true connections_total=2",
		},
		{
			name:      "unknown route",
			overrides: map[string]string{"route_state": "unobservable"},
			tuple:     "fluent_bit_active=true process_observable=true route_state=unobservable connections_observable=true connections_total=2",
		},
		{
			name:      "zero owned connections",
			overrides: map[string]string{"connections_total": "0", "connections_preferred": "0", "connections_distinct_fronts": "0"},
			tuple:     "fluent_bit_active=true process_observable=true route_state=expected connections_observable=true connections_total=0",
		},
		{
			name: "simultaneous missing prerequisites",
			overrides: map[string]string{
				"fluent_bit_active": "false", "process_observable": "false", "route_state": "unobservable",
				"connections_observable": "false", "connections_total": "0", "connections_preferred": "0", "connections_distinct_fronts": "0",
			},
			tuple: "fluent_bit_active=false process_observable=false route_state=unobservable connections_observable=false connections_total=0",
		},
	}
	for _, control := range controls {
		var calls atomic.Int32
		source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
			calls.Add(1)
			if host.Name == "database.invalid" {
				return mimirPublisherFixture(control.overrides), nil
			}
			return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
		}}
		alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
		if err != nil {
			t.Fatalf("%s: synthetic signal failed", control.name)
		}
		requireMimirPublisherConnectionVisibility(t, alerts, control.tuple)
		if calls.Load() != 2 {
			t.Fatalf("%s: diagnostic output changed source call count=%d, want 2", control.name, calls.Load())
		}
		placementDrift := false
		for _, alert := range alerts {
			placementDrift = placementDrift || alert.Target == "pg-primary/mimir-publisher" && alert.Class == "mimir-publisher-placement-drift"
		}
		if placementDrift != (control.overrides["route_state"] == "different") {
			t.Fatalf("%s: independent placement verdict changed", control.name)
		}
	}
}

// A concrete healthy/failover sample stays healthy; each known drift still warns.
func TestMimirPublishersConnectionVisibilityHealthyAndDriftControls(t *testing.T) {
	controls := []struct {
		overrides map[string]string
		drift     bool
	}{
		{overrides: nil, drift: false},
		{overrides: map[string]string{"connections_preferred": "1", "connections_distinct_fronts": "2"}, drift: false},
		{overrides: map[string]string{"connections_total": "1", "connections_preferred": "1"}, drift: false},
		{overrides: map[string]string{"connections_unknown": "1", "connections_preferred": "1"}, drift: true},
		{overrides: map[string]string{"connections_preferred": "0"}, drift: true},
	}
	for i, control := range controls {
		source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
			if host.Name == "database.invalid" {
				return mimirPublisherFixture(control.overrides), nil
			}
			return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
		}}
		alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
		if err != nil {
			t.Fatalf("control %d: synthetic signal failed", i)
		}
		wantCount := 0
		if control.drift {
			wantCount = 1
		}
		if len(alerts) != wantCount {
			t.Fatalf("control %d: alert count=%d, want %d", i, len(alerts), wantCount)
		}
		for _, alert := range alerts {
			if alert.Identity() != "observability/mimir-publishers|mimir-publisher-connection-drift|pg-primary/mimir-publisher|" || alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.unobservable {
				t.Fatalf("control %d: concrete connection-drift contract changed", i)
			}
		}
	}
}

// Invalid current authority cannot borrow a prior tuple or suppress a known sibling.
func TestMimirPublishersConnectionVisibilityRejectsUnqualifiedSamplesAndKeepsSiblingDrift(t *testing.T) {
	private := "synthetic-private credential=synthetic-secret peer=192.0.2.99"
	databaseOutput := mimirPublisherFixture(map[string]string{"connections_observable": "false"})
	var databaseErr error
	var calls atomic.Int32
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		calls.Add(1)
		if host.Name == "database.invalid" {
			return databaseOutput, databaseErr
		}
		return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2", "connections_unknown": "1", "connections_preferred": "1"}), nil
	}}
	signal := NewMimirPublishersSignal()
	settings := mimirPublisherSettings(source)
	if alerts, err := signal.Run(context.Background(), settings); err != nil || len(alerts) != 2 {
		t.Fatal("prior valid unknown and positive sibling fixture was not admitted")
	}
	controls := []struct {
		name   string
		output string
		err    error
	}{
		{name: "source failed with complete output", output: mimirPublisherFixture(nil), err: errors.New(private)},
		{name: "absent sample", output: ""},
		{name: "stale schema", output: mimirPublisherFixture(map[string]string{"observation_schema": "1"})},
		{name: "partial frame", output: strings.Replace(mimirPublisherFixture(nil), "route_state=expected\n", "", 1)},
		{name: "duplicate field", output: mimirPublisherFixture(nil) + "route_state=expected\n"},
		{name: "private route value", output: mimirPublisherFixture(map[string]string{"route_state": private})},
		{name: "inconsistent counts", output: mimirPublisherFixture(map[string]string{"connections_total": "1"})},
		{name: "stale inventory denominator", output: mimirPublisherFixture(map[string]string{"expected_fronts": "3", "missing_fronts": "1"})},
		{name: "oversized frame", output: strings.Repeat(private, 4096)},
	}
	for i, control := range controls {
		databaseOutput, databaseErr = control.output, control.err
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: synthetic signal failed", control.name)
		}
		unknown, fleetUnknown, siblingDrift := 0, 0, 0
		for _, alert := range alerts {
			requireAlertOmits(t, alert, private, "synthetic-private", "synthetic-secret", "database.invalid", "cache.invalid", "192.0.2.")
			switch alert.Target {
			case "pg-primary/mimir-publisher", "publisher-fleet":
				if alert.Class != "cannot-observe" || !alert.unobservable || alert.Severity != SeverityWarn || alert.Sustain != 2 {
					t.Fatalf("%s: unqualified sample produced an affirmative verdict", control.name)
				}
				for _, field := range []string{"fluent_bit_active=", "process_observable=", "route_state=", "connections_observable=", "connections_total="} {
					if strings.Contains(alert.Observed, field) {
						t.Fatalf("%s: unqualified sample reused a prerequisite field", control.name)
					}
				}
				if alert.Target == "publisher-fleet" {
					fleetUnknown++
				} else {
					unknown++
				}
			case "redis-cluster/mimir-publisher":
				if alert.Class != "mimir-publisher-connection-drift" || alert.unobservable {
					t.Fatalf("%s: known sibling drift lost its verdict", control.name)
				}
				siblingDrift++
			default:
				t.Fatalf("%s: unqualified sample reached a connection verdict", control.name)
			}
		}
		if unknown != 1 || fleetUnknown != 1 || siblingDrift != 1 || calls.Load() != int32(2*(i+2)) {
			t.Fatalf("%s: partial coverage, sibling drift, or fixed read count changed", control.name)
		}
	}
}

// The real reducer retains collected counts when its process fence fails;
// the tuple must retain that uncertainty rather than certify those sockets.
func TestMimirPublishersConnectionVisibilityFencedGenerationRemainsUnknown(t *testing.T) {
	sample := runSyntheticMimirPublisherReducer(t, map[string]string{"current_pid": "78"}, []string{"192.0.2.10", "192.0.2.11"})
	if sample.processObservable || sample.connectionsObservable || sample.routeState != "unobservable" || sample.connectionsTotal != 1 {
		t.Fatal("synthetic generation fence did not preserve collected count and invalidate authority")
	}
	settings := mimirPublisherSettings(&syntheticSource{})
	alerts := Alerts{}
	for _, result := range evaluateMimirPublisher("pg-primary/mimir-publisher", 1, sample) {
		if result.class == "mimir-publisher-connection-drift" {
			t.Fatal("stale generation produced a connection verdict")
		}
		if !result.healthy {
			alerts = append(alerts, alertFromFinding(settings, "11.20c", "mimir-publishers", "Mimir publisher convergence", result))
		}
	}
	alert := requireMimirPublisherConnectionVisibility(t, alerts, "fluent_bit_active=true process_observable=false route_state=unobservable connections_observable=false connections_total=1")
	if !strings.Contains(alert.Context, "Counts alone do not prove current owned connections") {
		t.Fatal("fenced generation count lost its explicit authority qualifier")
	}
	requireAlertOmits(t, alert, "MainPID=", "process_start=", "pid=77", "pid=78", "12345", "never-output-this")
}

// The same incident survives repeated unknown samples; only concrete healthy
// findings can satisfy the existing recovery window.
func TestMimirPublishersConnectionVisibilityUnknownCannotResolvePriorDrift(t *testing.T) {
	parse := func(overrides map[string]string) mimirPublisherSample {
		t.Helper()
		sample, err := parseMimirPublisherSample(mimirPublisherFixture(overrides))
		if err != nil {
			t.Fatal("synthetic sample was rejected")
		}
		return sample
	}
	broken := parse(map[string]string{"connections_preferred": "0"})
	unknown := parse(map[string]string{"connections_observable": "false"})
	healthy := parse(nil)
	manager := newTicketManager("synthetic", nil)
	manager.now = syntheticSettings(nil).Now
	identity := "observability/mimir-publishers|mimir-publisher-connection-drift|synthetic-publisher|"
	manager.ingest(context.Background(), evaluateMimirPublisher("synthetic-publisher", 1, broken))
	if incident := manager.tickets[identity]; incident == nil || !incident.open {
		t.Fatal("known connection-drift fixture did not open the original incident")
	}
	for range manager.resolveTicks + 1 {
		manager.ingest(context.Background(), evaluateMimirPublisher("synthetic-publisher", 1, unknown))
		if incident := manager.tickets[identity]; incident == nil || !incident.open || incident.healthyStreak != 0 {
			t.Fatal("unknown connection prerequisites cleared or advanced recovery of the original incident")
		}
	}
	for range manager.resolveTicks {
		manager.ingest(context.Background(), evaluateMimirPublisher("synthetic-publisher", 1, healthy))
	}
	if manager.tickets[identity] != nil {
		t.Fatal("complete healthy connection samples no longer satisfy the existing recovery window")
	}
}
