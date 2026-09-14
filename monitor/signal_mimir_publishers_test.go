package monitor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mimirPublisherFixture(overrides map[string]string) string {
	keys := []string{
		"observation_schema", "expected_fronts", "alias_entries", "recognized_fronts",
		"missing_fronts", "unknown_fronts", "duplicate_fronts", "preferred_ordinal",
		"fluent_bit_active", "process_observable", "route_state", "connections_observable", "connections_total",
		"connections_unknown", "connections_preferred", "connections_distinct_fronts",
	}
	values := map[string]string{
		"observation_schema": "2", "expected_fronts": "2", "alias_entries": "2",
		"recognized_fronts": "2", "missing_fronts": "0", "unknown_fronts": "0",
		"duplicate_fronts": "0", "preferred_ordinal": "1", "fluent_bit_active": "true",
		"process_observable": "true", "route_state": "expected", "connections_observable": "true", "connections_total": "2", "connections_unknown": "0",
		"connections_preferred": "2", "connections_distinct_fronts": "1",
	}
	for key, value := range overrides {
		values[key] = value
	}
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := values[key]; ok {
			lines = append(lines, key+"="+value)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func mimirPublisherSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "front-a.invalid", LANAddress: "192.0.2.10", Roles: []string{"services", "grafana"}},
		{Name: "front-b.invalid", LANAddress: "192.0.2.11", Roles: []string{"services", "grafana"}},
		{Name: "database.invalid", LANAddress: "192.0.2.20", Roles: []string{"pg-primary"}},
		{Name: "cache.invalid", LANAddress: "192.0.2.21", Roles: []string{"redis-cluster"}},
	}
	settings.MimirPublishers = MimirPublisherSettings{LoadState: "ready", PreferredFronts: map[string]string{
		"database.invalid": "front-a.invalid", "cache.invalid": "front-b.invalid",
	}}
	return settings
}

func TestMimirPublishersHealthySyntheticTopology(t *testing.T) {
	observations := map[string]string{
		"database.invalid": mimirPublisherFixture(nil),
		"cache.invalid": mimirPublisherFixture(map[string]string{
			"preferred_ordinal": "2", "connections_preferred": "2",
		}),
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		for _, want := range []string{
			mimirPublishersMarker, "synthetic-grafana.local", "process_observable", "pid=",
			"connections_distinct_fronts", "state established", ":3100",
		} {
			if !strings.Contains(command, want) {
				t.Fatalf("publisher reducer lacks %q", want)
			}
		}
		if strings.Contains(command, "stat -c") || strings.Contains(command, "process_after_hosts") {
			t.Fatal("unrelated hosts-file timestamps cannot prove process convergence")
		}
		return observations[host.Name], nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy publisher topology alerts=%+v", alerts)
	}
}

func TestMimirPublishersDetectCommonPreferenceWithoutTraffic(t *testing.T) {
	zeroTraffic := mimirPublisherFixture(map[string]string{
		"connections_total": "0", "connections_preferred": "0", "connections_distinct_fronts": "0",
	})
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return zeroTraffic, nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	fleetFound := false
	for _, alert := range alerts {
		if alert.Class == "mimir-publisher-placement-drift" && alert.Target == "publisher-fleet" {
			fleetFound = true
			for _, want := range []string{"low traffic", "expected_distinct=2", "Do not raise Mimir limits"} {
				if !strings.Contains(alert.Markdown(), want) {
					t.Fatalf("common-preference alert lacks %q:\n%s", want, alert.Markdown())
				}
			}
		}
	}
	if !fleetFound {
		t.Fatalf("common preference missed: %+v", alerts)
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

func TestMimirPublishersDetectMembershipAndRouteDrift(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{
			name: "obsolete front",
			overrides: map[string]string{
				"alias_entries": "3", "unknown_fronts": "1",
			},
			want: "unknown_fronts=1",
		},
		{
			name:      "wrong running route input",
			overrides: map[string]string{"route_state": "different"},
			want:      "route_state=different",
		},
		{
			name:      "duplicate front",
			overrides: map[string]string{"alias_entries": "3", "duplicate_fronts": "1"},
			want:      "duplicate_fronts=1",
		},
	}
	for _, test := range tests {
		source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
			if host.Name == "database.invalid" {
				return mimirPublisherFixture(test.overrides), nil
			}
			return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
		}}
		alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		alert := requireAlertClass(t, alerts, "mimir-publisher-placement-drift")
		if !strings.Contains(alert.Markdown(), test.want) {
			t.Fatalf("%s: placement alert lacks %q:\n%s", test.name, test.want, alert.Markdown())
		}
		requireAlertOmits(t, alert, "database.invalid", "cache.invalid", "192.0.2.10", "192.0.2.20")
	}
}

func TestMimirPublishersDetectLiveConnectionDrift(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "database.invalid" {
			return mimirPublisherFixture(map[string]string{
				"connections_total": "4", "connections_preferred": "0", "connections_distinct_fronts": "1",
			}), nil
		}
		return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "mimir-publisher-connection-drift")
	for _, want := range []string{"connections_total=4", "connections_preferred=0", "port-3100"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("connection alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, "database.invalid", "192.0.2.10", "192.0.2.20")
}

func TestMimirPublishersMalformedObservationIsVisibilityOnly(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "database.invalid" {
			return mimirPublisherFixture(map[string]string{
				"connections_total": "private-fixture\nraw_address=192.0.2.99",
			}), nil
		}
		return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	requireAlertOmits(t, alert, "private-fixture", "raw_address", "192.0.2.99", "database.invalid")
}

func TestMimirPublishersCancellationReturnsContextErrorWithoutAlert(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "cache.invalid" {
			return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
		}
		return mimirPublisherFixture(nil), nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewMimirPublishersSignal().Run(ctx, mimirPublisherSettings(source))
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("canceled run alerts=%+v err=%v", alerts, err)
	}
}

func TestMimirPublishersMetadataAndRegistration(t *testing.T) {
	signal := NewMimirPublishersSignal()
	if signal.Number() != "11.20c" || signal.Key() != "mimir-publishers" ||
		signal.ID() != "observability/mimir-publishers" || signal.Cadence() != 5*time.Minute {
		t.Fatalf("unexpected metadata: number=%s key=%s id=%s cadence=%s", signal.Number(), signal.Key(), signal.ID(), signal.Cadence())
	}
	selected, err := IncludeSignals(NewSignals(), "mimir-publishers")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Key() != "mimir-publishers" {
		t.Fatalf("registry selection=%v", selected)
	}
}

func TestMimirPublishersDetectSwappedDistinctPreferences(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "database.invalid" {
			return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
		}
		return mimirPublisherFixture(nil), nil
	}}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), mimirPublisherSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	driftCount := 0
	for _, alert := range alerts {
		if alert.Class == "mimir-publisher-placement-drift" {
			driftCount++
		}
		requireAlertOmits(t, alert, "database.invalid", "cache.invalid", "192.0.2.10")
	}
	if driftCount != 3 {
		t.Fatalf("distinct but swapped preferences must fail both hosts and fleet: %+v", alerts)
	}
}

func TestMimirPublishersHonorExplicitSharedDesiredPreference(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return mimirPublisherFixture(nil), nil }}
	settings := mimirPublisherSettings(source)
	settings.MimirPublishers.PreferredFronts["cache.invalid"] = "front-a.invalid"
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("probe invented a placement policy: %+v err=%v", alerts, err)
	}
}

func TestMimirPublishersUnknownRuntimeAndZeroSocketsDoNotClearConnectionDrift(t *testing.T) {
	for _, overrides := range []map[string]string{
		{"fluent_bit_active": "false"},
		{"process_observable": "false", "route_state": "unobservable", "connections_observable": "false"},
		{"connections_observable": "false"},
		{"route_state": "unobservable"},
		{"connections_total": "0", "connections_preferred": "0", "connections_distinct_fronts": "0"},
	} {
		sample, err := parseMimirPublisherSample(mimirPublisherFixture(overrides))
		if err != nil {
			t.Fatal(err)
		}
		unknownFound := false
		for _, finding := range evaluateMimirPublisher("synthetic-publisher", 1, sample) {
			if finding.class == "mimir-publisher-connection-drift" {
				t.Fatalf("missing evidence emitted a typed bad/healthy verdict: %+v", finding)
			}
			if finding.class == "cannot-observe" {
				unknownFound = true
			}
		}
		if !unknownFound {
			t.Fatalf("runtime visibility loss missed: %+v", overrides)
		}
	}
}

func TestMimirPublishersPartialCoverageCannotClearOrInventFleetDrift(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "database.invalid" {
			return "private-fixture", errors.New("private-fixture")
		}
		return mimirPublisherFixture(map[string]string{"preferred_ordinal": "2"}), nil
	}}
	cfg := configFromSignalSettings(mimirPublisherSettings(source))
	findings, err := (mimirPublishersProbe{}).check(context.Background(), &probeEnv{cfg: cfg, runner: newRunner(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.target == "publisher-fleet" && finding.class != "cannot-observe" {
			t.Fatalf("partial coverage produced an affirmative fleet verdict: %+v", finding)
		}
	}
}

func TestMimirPublishersUnknownRunningRouteCannotClearFleetPlacement(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		overrides := map[string]string{"route_state": "unobservable"}
		if host.Name == "cache.invalid" {
			overrides["preferred_ordinal"] = "2"
		}
		return mimirPublisherFixture(overrides), nil
	}}
	cfg := configFromSignalSettings(mimirPublisherSettings(source))
	findings, err := (mimirPublishersProbe{}).check(context.Background(), &probeEnv{cfg: cfg, runner: newRunner(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	fleetUnknown := false
	for _, finding := range findings {
		if finding.target == "publisher-fleet" {
			if finding.class != "cannot-observe" {
				t.Fatalf("unknown runtime emitted typed healthy/bad fleet verdict: %+v", finding)
			}
			fleetUnknown = true
		}
	}
	if !fleetUnknown {
		t.Fatal("missing runtime did not create a fleet visibility finding")
	}
}

func TestMimirPublishersMissingDesiredPreferenceDoesNotContactHost(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatal("missing desired preference must fail before host contact")
		return "", nil
	}}
	settings := mimirPublisherSettings(source)
	settings.MimirPublishers = MimirPublisherSettings{LoadState: "unavailable"}
	alerts, err := NewMimirPublishersSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 3 {
		t.Fatalf("expected two host and one fleet visibility findings: %+v err=%v", alerts, err)
	}
	for _, alert := range alerts {
		if alert.Class != "cannot-observe" {
			t.Fatalf("unobservable desired state invented drift: %+v", alert)
		}
	}
}

func TestMimirPublishersRejectMalformedAndImpossibleObservations(t *testing.T) {
	for _, overrides := range []map[string]string{
		{"observation_schema": "1"}, {"route_state": "private-fixture"},
		{"connections_total": "1", "connections_preferred": "0", "connections_distinct_fronts": "2"},
		{"connections_distinct_fronts": "0"}, {"preferred_ordinal": "0"},
		{"recognized_fronts": "0"}, {"connections_unknown": "3"},
	} {
		if _, err := parseMimirPublisherSample(mimirPublisherFixture(overrides)); err == nil || strings.Contains(err.Error(), "private-fixture") {
			t.Fatalf("malformed evidence accepted or leaked: %+v err=%v", overrides, err)
		}
	}
}

// Execute the actual reducer against synthetic commands/files only; no live
// /proc environment, privilege, remote host, or production settings are used.
func runSyntheticMimirPublisherReducer(t *testing.T, overrides map[string]string, fronts []string) mimirPublisherSample {
	t.Helper()
	fixture := map[string]string{
		"hosts":      "192.0.2.10 synthetic-grafana.local # inline comment\n192.0.2.11 synthetic-grafana.local\n",
		"properties": "ActiveState=active\nSubState=running\nMainPID=77\n",
		"comm":       "fluent-bit\n", "current_pid": "77", "ss_status": "0",
		"environment": "GRAFANA_PUSH_HOST=synthetic-grafana.local\x00GRAFANA_PUSH_PORT=3100\x00PRIVATE_FIXTURE=never-output-this\x00",
		"peers": "0 0 192.0.2.20:41000 192.0.2.10:3100 users:((\"fluent-bit\",pid=77,fd=9))\n" +
			"0 0 192.0.2.20:41001 192.0.2.11:3100 users:((\"other\",pid=177,fd=9))\n",
	}
	statFields := make([]string, 22)
	for i := range statFields {
		statFields[i] = "0"
	}
	statFields[0], statFields[1], statFields[2], statFields[21] = "77", "(fluent-bit)", "S", "12345"
	fixture["stat"] = strings.Join(statFields, " ") + "\n"
	for key, value := range overrides {
		fixture[key] = value
	}
	directory := t.TempDir()
	write := func(name, data string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(directory, name), []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range fixture {
		write(name, data, 0600)
	}
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Fatal(err)
	}
	head, err := exec.LookPath("head")
	if err != nil {
		t.Fatal(err)
	}
	od, err := exec.LookPath("od")
	if err != nil {
		t.Fatal(err)
	}
	remap := `remaining=$#
while [ "$remaining" -gt 0 ]; do
  arg=$1; shift
  case "$arg" in
    /etc/hosts) arg="$FIXTURE/hosts" ;;
    /proc/77/stat) arg="$FIXTURE/stat" ;;
    /proc/77/comm) arg="$FIXTURE/comm" ;;
    /proc/77/environ) arg="$FIXTURE/environment" ;;
    /proc/*) exit 1 ;;
  esac
  set -- "$@" "$arg"
  remaining=$((remaining-1))
done
`
	write("awk", "#!/bin/sh\n"+remap+"exec "+shellSingleQuote(awk)+" \"$@\"\n", 0700)
	write("head", "#!/bin/sh\n"+remap+"exec "+shellSingleQuote(head)+" \"$@\"\n", 0700)
	write("od", "#!/bin/sh\n"+remap+"exec "+shellSingleQuote(od)+" \"$@\"\n", 0700)
	write("sudo", "#!/bin/sh\nexit 1\n", 0700)
	write("ss", "#!/bin/sh\n/bin/cat \"$FIXTURE/peers\"\nexit "+fixture["ss_status"]+"\n", 0700)
	write("systemctl", "#!/bin/sh\ncase \"$*\" in\n  *--value*) /bin/cat \"$FIXTURE/current_pid\" ;;\n  *) /bin/cat \"$FIXTURE/properties\" ;;\nesac\n", 0700)
	command, err := mimirPublisherCommand("synthetic-grafana.local", fronts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	process.Env = append(os.Environ(), "PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"), "FIXTURE="+directory)
	output, err := process.CombinedOutput()
	if err != nil {
		t.Fatalf("synthetic reducer failed: %v\n%s", err, output)
	}
	for _, private := range []string{"192.0.2.", "2001:db8:", "never-output-this", "synthetic-grafana.local", directory} {
		if strings.Contains(string(output), private) {
			t.Fatalf("reducer leaked synthetic private evidence %q", private)
		}
	}
	sample, err := parseMimirPublisherSample(string(output))
	if err != nil {
		t.Fatalf("reducer emitted invalid schema: %v\n%s", err, output)
	}
	return sample
}

func TestMimirPublisherReducerJoinsExactProcessAndIgnoresOtherSockets(t *testing.T) {
	sample := runSyntheticMimirPublisherReducer(t, nil, []string{"192.0.2.10", "192.0.2.11"})
	if sample.aliasEntries != 2 || sample.preferredOrdinal != 1 || sample.routeState != "expected" || !sample.processObservable || !sample.connectionsObservable || sample.connectionsTotal != 1 || sample.connectionsPreferred != 1 {
		t.Fatalf("process-owned socket not isolated: %+v", sample)
	}
}

func TestMimirPublisherReducerUnknownOwnersAndPidReplacementWithholdVerdicts(t *testing.T) {
	for _, overrides := range []map[string]string{
		{"peers": "0 0 192.0.2.20:41000 192.0.2.10:3100\n"},
		{"peers": "0 0 192.0.2.20:41000 malformed:3100 users:((\"fluent-bit\",pid=77,fd=9))\n"},
		{"peers": "0 0 192.0.2.20:41000 192.0.2.10:not-a-port users:((\"fluent-bit\",pid=77,fd=9))\n"},
		{"peers": "0 0 192.0.2.20:41000 192.0.2.10:3101 users:((\"fluent-bit\",pid=77,fd=9))\n"},
		{"peers": "0 0 192.0.2.20:41000 192.0.2.010:3100 users:((\"fluent-bit\",pid=77,fd=9))\n"},
		{"current_pid": "78"}, {"ss_status": "1"}, {"comm": "other-process\n"},
	} {
		sample := runSyntheticMimirPublisherReducer(t, overrides, []string{"192.0.2.10", "192.0.2.11"})
		if sample.connectionsObservable {
			t.Fatalf("ambiguous runtime accepted: %+v => %+v", overrides, sample)
		}
	}
}

func TestMimirPublisherReducerChecksLiveRouteInputsWithoutLeakingEnvironment(t *testing.T) {
	for _, fixture := range []struct{ environment, want string }{
		{environment: "GRAFANA_PUSH_HOST=other.invalid\x00GRAFANA_PUSH_PORT=3100\x00", want: "different"},
		{environment: "GRAFANA_PUSH_HOST=synthetic-grafana.local\x00GRAFANA_PUSH_PORT=3101\x00", want: "different"},
		{environment: "PRIVATE_FIXTURE=never-output-this\x00", want: "unobservable"},
		{environment: "GRAFANA_PUSH_HOST=synthetic-grafana.local\x00GRAFANA_PUSH_HOST=other.invalid\x00GRAFANA_PUSH_PORT=3100\x00", want: "unobservable"},
	} {
		sample := runSyntheticMimirPublisherReducer(t, map[string]string{"environment": fixture.environment}, []string{"192.0.2.10", "192.0.2.11"})
		if sample.routeState != fixture.want {
			t.Fatalf("route_state=%q want=%q", sample.routeState, fixture.want)
		}
	}
}

func TestMimirPublisherReducerCanonicalizesNumericIpv6Evidence(t *testing.T) {
	sample := runSyntheticMimirPublisherReducer(t, map[string]string{
		"hosts": "2001:0DB8:0:0:0:0:0:10 synthetic-grafana.local\n2001:db8::11 synthetic-grafana.local\n",
		"peers": "0 0 [2001:db8::20]:41000 [2001:0db8:0:0:0:0:0:10]:3100 users:((\"fluent-bit\",pid=77,fd=9))\n",
	}, []string{"2001:db8::10", "2001:db8::11"})
	if sample.recognizedFronts != 2 || sample.connectionsUnknown != 0 || sample.connectionsPreferred != 1 {
		t.Fatalf("equivalent IPv6 forms created false drift: %+v", sample)
	}
}

func TestMimirPublisherReducerBoundsSocketEvidence(t *testing.T) {
	row := "0 0 192.0.2.20:41000 192.0.2.10:3100 users:((\"fluent-bit\",pid=77,fd=9))\n"
	sample := runSyntheticMimirPublisherReducer(t, map[string]string{"peers": strings.Repeat(row, 1025)}, []string{"192.0.2.10", "192.0.2.11"})
	if sample.connectionsObservable {
		t.Fatal("oversized socket evidence must be unknown, not healthy")
	}
}

func TestMimirPublisherReducerCanonicalizesIpv4MappedPeers(t *testing.T) {
	for _, address := range []string{"::ffff:192.0.2.10", "0:0:0:0:0:ffff:c000:20a"} {
		sample := runSyntheticMimirPublisherReducer(t, map[string]string{
			"peers": "0 0 192.0.2.20:41000 [" + address + "]:3100 users:((\"fluent-bit\",pid=77,fd=9))\n",
		}, []string{"192.0.2.10", "192.0.2.11"})
		if !sample.connectionsObservable || sample.connectionsUnknown != 0 || sample.connectionsPreferred != 1 {
			t.Fatalf("equivalent IPv4-mapped peer created false drift: %+v", sample)
		}
	}
}

func TestMimirPublisherFrontInventoryRejectsUnusableAndDuplicateAddresses(t *testing.T) {
	for _, address := range []string{"", "private-fixture", "0.0.0.0", "127.0.0.1", "::", "ff02::1", "fe80::1%example", "192.0.2.11"} {
		settings := mimirPublisherSettings(&syntheticSource{})
		settings.Hosts[0].LANAddress = address
		if _, err := mimirPublisherFrontAddresses(configFromSignalSettings(settings)); err == nil {
			t.Fatalf("unusable/duplicate active front address accepted: %q", address)
		}
	}
}

func TestMimirPublisherReducerDoesNotNormalizeAmbiguousIpv4AliasesToHealthy(t *testing.T) {
	sample := runSyntheticMimirPublisherReducer(t, map[string]string{
		"hosts": "192.0.2.010 synthetic-grafana.local\n192.0.2.11 synthetic-grafana.local\n",
	}, []string{"192.0.2.10", "192.0.2.11"})
	if sample.recognizedFronts != 1 || sample.unknownFronts != 1 || sample.preferredOrdinal != 0 {
		t.Fatalf("ambiguous IPv4 alias manufactured healthy membership: %+v", sample)
	}
}

func TestMimirPublisherReducerIgnoresPidTextInsideAnotherProcessName(t *testing.T) {
	for _, comm := range []string{"pid=77,", "helper \\\"pid=77,\\\""} {
		sample := runSyntheticMimirPublisherReducer(t, map[string]string{
			"peers": "0 0 192.0.2.20:41000 192.0.2.10:3100 users:((\"" + comm + "\",pid=177,fd=7))\n",
		}, []string{"192.0.2.10", "192.0.2.11"})
		if !sample.connectionsObservable || sample.connectionsTotal != 0 || sample.connectionsPreferred != 0 {
			t.Fatalf("quoted process name spoofed owner metadata: %+v", sample)
		}
	}
}

func TestMimirPublisherReducerAcceptsOwnedTupleAmongKnownOtherOwners(t *testing.T) {
	sample := runSyntheticMimirPublisherReducer(t, map[string]string{
		"peers": "0 0 192.0.2.20:41000 192.0.2.10:3100 users:((\"other\",pid=177,fd=7),(\"fluent-bit\",pid=77,fd=8))\n",
	}, []string{"192.0.2.10", "192.0.2.11"})
	if !sample.connectionsObservable || sample.connectionsTotal != 1 || sample.connectionsPreferred != 1 {
		t.Fatalf("real owner was lost in a multi-owner socket: %+v", sample)
	}
}
