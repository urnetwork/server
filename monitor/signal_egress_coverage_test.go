package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

func syntheticEgressCoverageSignal() Signal {
	signal := NewEgressCoverageSignal().(*signalAdapter)
	signal.probe = egressCoverageProbe{loadDesiredConfig: func() egressCoverageDesiredConfig {
		return egressCoverageDesiredConfig{}
	}}
	return signal
}

// Main-shaped: the zero-valued blackhole destination/bandwidth fields are
// intentionally omitted, just as they are in provider_egress_probe.yml.
const syntheticEgressDesiredConfig = `enabled: true
shard_count: 4
idle_delay_seconds: 300
max_time_seconds: 1800
api_url: https://private-desired-api.example.invalid
platform_url: wss://private-desired-platform.example.invalid
public_api_url: https://private-desired-public.example.invalid
bandwidth_cdn_url: https://private-desired-cdn.example.invalid/down
full:
  limit: 8
  concurrency: 2
  probe_timeout_seconds: 60
  all_destinations: false
  bandwidth: true
  bandwidth_timeout_seconds: 5
blackhole:
  limit: 250
  concurrency: 52
  probe_timeout_seconds: 15
`

func syntheticDesiredEgressConfig(t *testing.T, raw string) egressCoverageDesiredConfig {
	t.Helper()
	return inspectEgressCoverageDesiredConfig(func(value any) error {
		return yaml.Unmarshal([]byte(raw), value)
	})
}

func syntheticEgressMinimumFullTimeoutSeconds(allDestinations bool) int {
	minimum := time.Duration(fleetprobe.EgressHealthRounds(allDestinations)) * egresshealth.DefaultPerRequestTimeout
	return int((minimum + time.Second - 1) / time.Second)
}

func syntheticEgressRowsForConfig(t *testing.T, config egressCoverageConfig) []Row {
	t.Helper()
	rows := make([]Row, 0, config.shardCount)
	for index := range config.shardCount {
		rows = append(rows, syntheticEgressCoverageTaskWithArgs(t, egressCoverageTaskArgs{
			ShardIndex: index, ShardCount: config.shardCount,
			IdleDelaySeconds: config.idleDelaySeconds, MaxTimeSeconds: config.maxTimeSeconds,
			Full: config.full, Blackhole: config.blackhole,
			APIURL: config.apiURL, PlatformURL: config.platformURL,
			PublicAPIURL: config.publicAPIURL, BandwidthCDNURL: config.bandwidthCDNURL,
		}))
	}
	return rows
}

func syntheticEgressConfigSource(t *testing.T, taskRows *[]Row) *syntheticSource {
	t.Helper()
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return *taskRows, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			rows := make([]Row, 0, len(*taskRows))
			for index := range *taskRows {
				rows = append(rows, syntheticEgressCoverageActivity(egressCoverageSnapshot{
					shardIndex: index, eligible: 100, fullCurrent: 100, blackholeCurrent: 100,
					fullAgeSeconds: 10, blackholeAgeSeconds: 10,
					fullAttemptsLastHour: 1, blackholeLastHour: 1,
					staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
				}))
			}
			return rows, nil
		default:
			t.Fatalf("unexpected config-drift query")
			return nil, nil
		}
	}}
}

func requireEgressConfigPrivacy(t *testing.T, alerts Alerts, forbidden ...string) {
	t.Helper()
	for _, alert := range alerts {
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range append(forbidden, "private-desired-", "example.invalid", "private-runtime-marker") {
			if strings.Contains(alert.Markdown(), value) || strings.Contains(string(encoded), value) {
				t.Fatal("egress config finding leaked a private fixture in Markdown or JSON")
			}
		}
	}
}

func TestEgressCoverageDesiredConfigDriftAndConvergence(t *testing.T) {
	pop := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte(syntheticEgressDesiredConfig))
	defer pop()
	desired := loadEgressCoverageDesiredConfig()
	if !desired.present || !desired.enabled || desired.invalidReason != "" ||
		desired.settings.shardCount != 4 || desired.settings.blackhole.Concurrency != 52 ||
		desired.settings.blackhole.Bandwidth || desired.settings.blackhole.AllDestinations ||
		desired.settings.blackhole.BandwidthTimeoutSeconds != 0 {
		t.Fatal("Main-shaped config with omitted blackhole modes was not observable")
	}
	old := desired.settings
	old.blackhole.Concurrency = 32
	rows := syntheticEgressRowsForConfig(t, old)
	source := syntheticEgressConfigSource(t, &rows)
	signal := NewEgressCoverageSignal()
	alerts, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "egress-probe-config-drift")
	if alert.Severity != Severity(tierWarn) || alert.Sustain != 2 {
		t.Fatal("config drift must be a two-cadence WARN, independent of the capacity PAGE")
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"desired_enabled=true", "desired_shards=4", "durable_shards=4",
		"desired_blackhole_concurrency_per_shard=52", "durable_blackhole_concurrency_per_shard=32",
		"mismatched_fields=blackhole.concurrency", "not args_json", "successful ProviderEgressProbe post-steps",
		"then deploy Taskworker", "mounts that completed version", "Do not insert, delete, or hand-edit pending_task",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("config-drift Markdown is missing %q", want)
		}
	}
	if strings.Index(alert.Action, "config-updater") >= strings.Index(alert.Action, "Taskworker") {
		t.Fatal("config-updater must precede Taskworker")
	}
	requireEgressConfigPrivacy(t, alerts, rows[0][1])

	// One successful successor does not make a mixed generation converged.
	converged := syntheticEgressRowsForConfig(t, desired.settings)
	rows[0] = converged[0]
	alerts, err = signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "egress-probe-shards")
	requireAlertClass(t, alerts, "egress-probe-config-unobservable")
	for _, alert := range alerts {
		if alert.Class == "egress-probe-config-drift" {
			t.Fatal("mixed rows were reported as one complete desired/durable snapshot")
		}
	}
	requireEgressConfigPrivacy(t, alerts)
	rows = converged
	alerts, err = signal.Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatalf("complete successor convergence is not healthy: alerts=%d err=%v", len(alerts), err)
	}

	// The same probe reloads the active resource rather than caching agreement.
	popNext := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte(strings.Replace(syntheticEgressDesiredConfig, "concurrency: 52", "concurrency: 48", 1)))
	defer popNext()
	alerts, err = signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if alert := requireAlertClass(t, alerts, "egress-probe-config-drift"); !strings.Contains(alert.Observed, "desired_blackhole_concurrency_per_shard=48") {
		t.Fatal("desired configuration was cached across observations")
	}
}

func TestEgressCoverageDesiredConfigComparesEveryExecutionSetting(t *testing.T) {
	desired := syntheticDesiredEgressConfig(t, syntheticEgressDesiredConfig)
	cases := []struct {
		field   string
		prepare func(*egressCoverageConfig)
		mutate  func(*egressCoverageConfig)
	}{
		{field: "shard_count", mutate: func(c *egressCoverageConfig) { c.shardCount = 3 }},
		{field: "idle_delay_seconds", mutate: func(c *egressCoverageConfig) { c.idleDelaySeconds++ }},
		{field: "max_time_seconds", mutate: func(c *egressCoverageConfig) { c.maxTimeSeconds++ }},
		{field: "full.limit", mutate: func(c *egressCoverageConfig) { c.full.Limit++ }},
		{field: "full.concurrency", mutate: func(c *egressCoverageConfig) { c.full.Concurrency++ }},
		{field: "full.probe_timeout_seconds", mutate: func(c *egressCoverageConfig) { c.full.ProbeTimeoutSeconds++ }},
		{
			field: "full.all_destinations",
			prepare: func(c *egressCoverageConfig) {
				c.full.ProbeTimeoutSeconds = syntheticEgressMinimumFullTimeoutSeconds(true)
			},
			mutate: func(c *egressCoverageConfig) { c.full.AllDestinations = true },
		},
		{field: "full.bandwidth", mutate: func(c *egressCoverageConfig) { c.full.Bandwidth = false }},
		{field: "full.bandwidth_timeout_seconds", mutate: func(c *egressCoverageConfig) { c.full.BandwidthTimeoutSeconds++ }},
		{field: "blackhole.limit", mutate: func(c *egressCoverageConfig) { c.blackhole.Limit++ }},
		{field: "blackhole.concurrency", mutate: func(c *egressCoverageConfig) { c.blackhole.Concurrency++ }},
		{field: "blackhole.probe_timeout_seconds", mutate: func(c *egressCoverageConfig) { c.blackhole.ProbeTimeoutSeconds++ }},
		{field: "blackhole.all_destinations", mutate: func(c *egressCoverageConfig) { c.blackhole.AllDestinations = true }},
		{field: "blackhole.bandwidth", mutate: func(c *egressCoverageConfig) { c.blackhole.Bandwidth = true; c.blackhole.BandwidthTimeoutSeconds = 5 }},
		{field: "blackhole.bandwidth_timeout_seconds", mutate: func(c *egressCoverageConfig) { c.blackhole.BandwidthTimeoutSeconds++ }},
		{field: "api_url", mutate: func(c *egressCoverageConfig) { c.apiURL += "/private-runtime-marker" }},
		{field: "platform_url", mutate: func(c *egressCoverageConfig) { c.platformURL += "/private-runtime-marker" }},
		{field: "public_api_url", mutate: func(c *egressCoverageConfig) { c.publicAPIURL += "/private-runtime-marker" }},
		{field: "bandwidth_cdn_url", mutate: func(c *egressCoverageConfig) { c.bandwidthCDNURL += "/private-runtime-marker" }},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			expected := desired
			if tc.prepare != nil {
				tc.prepare(&expected.settings)
			}
			actual := expected.settings
			tc.mutate(&actual)
			rows := syntheticEgressRowsForConfig(t, actual)
			pgRows := make([]pgRow, len(rows))
			for index, row := range rows {
				pgRows[index] = pgRow(row)
			}
			geometry, err := inspectEgressCoverageTasks(pgRows)
			if err != nil {
				t.Fatal(err)
			}
			findings := egressCoverageConfigFindings("pg-1", expected, len(rows), geometry, nil)
			alerts := Alerts{}
			for _, f := range findings {
				if !f.healthy {
					alerts = append(alerts, alertFromFinding(syntheticSettings(nil), "2.19", "egress-coverage", "Provider egress probe coverage", f))
				}
			}
			alert := requireAlertClass(t, alerts, "egress-probe-config-drift")
			if !strings.Contains(alert.Observed, "mismatched_fields="+tc.field) {
				t.Fatalf("complete comparison omitted %s", tc.field)
			}
			requireEgressConfigPrivacy(t, alerts, rows[0][1])
		})
	}
}

func TestEgressCoverageDesiredConfigMissingAndInvalid(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", configHome)
	if desired := loadEgressCoverageDesiredConfig(); desired.present {
		t.Fatal("absent optional resource invented desired state")
	}
	if findings := egressCoverageConfigFindings("pg-1", egressCoverageDesiredConfig{}, 4, egressCoverageGeometry{}, nil); len(findings) != 0 {
		t.Fatal("default-only environment fabricated drift or agreement")
	}
	if err := os.Mkdir(filepath.Join(configHome, "provider_egress_probe.yml"), 0o700); err != nil {
		t.Fatal(err)
	}
	unavailable := loadEgressCoverageDesiredConfig()
	if !unavailable.present || unavailable.invalidReason != "resource-unavailable" {
		t.Fatal("unavailable desired resource was mistaken for optional absence")
	}
	findings := egressCoverageConfigFindings("pg-1", unavailable, 4, egressCoverageGeometry{}, nil)
	if len(findings) != 1 || findings[0].class != "egress-probe-config-unobservable" || findings[0].healthy {
		t.Fatal("unavailable desired resource did not emit an observation-gap finding")
	}
	for _, raw := range []string{
		"enabled: [\nprivate-parser-marker",
		"enabled: null\n",
		"enabled: true\nshard_count: 4\n",
		strings.Replace(syntheticEgressDesiredConfig, "  bandwidth: true\n", "", 1),
		strings.Replace(syntheticEgressDesiredConfig, "bandwidth_timeout_seconds: 5", "bandwidth_timeout_seconds: 0", 1),
		strings.Replace(syntheticEgressDesiredConfig, "concurrency: 52", "concurrency: 251", 1),
		syntheticEgressDesiredConfig + "private-parser-marker: private-parser-marker\n",
		strings.Replace(syntheticEgressDesiredConfig, "  concurrency: 52", "  private-parser-marker: private-parser-marker\n  concurrency: 52", 1),
	} {
		desired := syntheticDesiredEgressConfig(t, raw)
		if !desired.present || desired.invalidReason == "" {
			t.Fatal("invalid desired resource appeared observable")
		}
		findings := egressCoverageConfigFindings("pg-1", desired, 4, egressCoverageGeometry{}, nil)
		if len(findings) != 1 || findings[0].class != "egress-probe-config-unobservable" || findings[0].healthy {
			t.Fatal("invalid desired state fabricated drift or recovery")
		}
		alert := alertFromFinding(syntheticSettings(nil), "2.19", "egress-coverage", "Provider egress probe coverage", findings[0])
		requireEgressConfigPrivacy(t, Alerts{alert}, "private-parser-marker", raw)
	}
	desired := inspectEgressCoverageDesiredConfig(func(any) error { return fmt.Errorf("private-read-error-marker") })
	if desired.invalidReason != "resource-unreadable-or-malformed" {
		t.Fatal("read error was not safely classified")
	}
	findings = egressCoverageConfigFindings("pg-1", desired, 0, egressCoverageGeometry{}, nil)
	alert := alertFromFinding(syntheticSettings(nil), "2.19", "egress-coverage", "Provider egress probe coverage", findings[0])
	requireEgressConfigPrivacy(t, Alerts{alert}, "private-read-error-marker")
}

func TestEgressCoverageDesiredConfigUsesTaskworkerHealthTimeoutFloor(t *testing.T) {
	desired := syntheticDesiredEgressConfig(t, syntheticEgressDesiredConfig)
	for _, allDestinations := range []bool{false, true} {
		config := desired.settings
		config.full.AllDestinations = allDestinations
		minimum := syntheticEgressMinimumFullTimeoutSeconds(allDestinations)
		if minimum < 2 {
			t.Fatal("synthetic health geometry has no below-boundary timeout")
		}

		config.full.ProbeTimeoutSeconds = minimum - 1
		if validEgressCoverageConfig(config) {
			t.Fatal("monitor accepted a timeout that Taskworker rejects below its cold-request floor")
		}
		config.full.ProbeTimeoutSeconds = minimum
		if !validEgressCoverageConfig(config) {
			t.Fatal("monitor rejected the exact timeout floor accepted by Taskworker")
		}
	}
}

func TestEgressCoverageDanglingDesiredResourceIsUnobservable(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", configHome)
	t.Setenv("WARP_ENV", "")
	if err := os.Symlink("missing-provider-egress-config", filepath.Join(configHome, "provider_egress_probe.yml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	desired := loadEgressCoverageDesiredConfig()
	if !desired.present || desired.invalidReason != "resource-unavailable" {
		t.Fatal("dangling desired resource was mistaken for optional absence")
	}
	findings := egressCoverageConfigFindings("pg-1", desired, 0, egressCoverageGeometry{}, nil)
	if len(findings) != 1 || findings[0].class != "egress-probe-config-unobservable" || findings[0].healthy {
		t.Fatal("dangling desired resource did not retain the observation gap")
	}
}

func TestEgressCoverageDesiredEnablement(t *testing.T) {
	for _, tc := range []struct {
		name, raw      string
		rows           bool
		drift, unarmed bool
	}{
		{"enabled without rows", syntheticEgressDesiredConfig, false, true, true},
		{"disabled with old rows", "enabled: false\n", true, true, false},
		{"disabled and retired", "enabled: false\n", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pop := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte(tc.raw))
			defer pop()
			rows := []Row{}
			if tc.rows {
				rows = syntheticEgressRowsForConfig(t, syntheticDesiredEgressConfig(t, syntheticEgressDesiredConfig).settings)
			}
			alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(syntheticEgressConfigSource(t, &rows)))
			if err != nil {
				t.Fatal(err)
			}
			drift, unarmed := false, false
			for _, alert := range alerts {
				drift = drift || alert.Class == "egress-probe-config-drift"
				unarmed = unarmed || alert.Class == "egress-probe-unarmed"
			}
			if drift != tc.drift || unarmed != tc.unarmed {
				t.Fatal("enablement comparison confused absent tasks with intentional disablement")
			}
			requireEgressConfigPrivacy(t, alerts)
		})
	}
}

func TestEgressCoverageDisabledDesiredStateDoesNotSuppressDurableFaults(t *testing.T) {
	pop := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte("enabled: false\n"))
	defer pop()

	t.Run("mixed geometry", func(t *testing.T) {
		rows := []Row{
			syntheticEgressCoverageTask(t, 0, 3),
			syntheticEgressCoverageTask(t, 2, 3),
		}
		alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(syntheticEgressConfigSource(t, &rows)))
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "egress-probe-config-drift")
		requireAlertClass(t, alerts, "egress-probe-shards")
		for _, alert := range alerts {
			if alert.Class == "egress-probe-unarmed" {
				t.Fatal("explicit disablement was confused with an unarmed enabled rollout")
			}
		}
		requireEgressConfigPrivacy(t, alerts)
	})

	t.Run("capacity pressure", func(t *testing.T) {
		rows := []Row{syntheticEgressCoverageTask(t, 0, 1)}
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			switch {
			case strings.Contains(query, "pg_attribute"):
				return []Row{{"t", "t"}}, nil
			case strings.Contains(query, "FROM pending_task"):
				return rows, nil
			case strings.Contains(query, "WITH lifecycle_clock AS"):
				return []Row{syntheticEgressCoverageActivity(egressCoverageSnapshot{
					shardIndex: 0, eligible: 301,
					fullCurrent: 301, blackholeCurrent: 200,
					fullAgeSeconds: 10, blackholeAgeSeconds: 10,
					fullAttemptsLastHour: 100, blackholeLastHour: 100,
					staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
				})}, nil
			default:
				t.Fatalf("unexpected disabled-capacity query: %s", query)
				return nil, nil
			}
		}}
		alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		requireAlertClass(t, alerts, "egress-probe-config-drift")
		requireAlertClass(t, alerts, "egress-blackhole-capacity")
		requireEgressConfigPrivacy(t, alerts, rows[0][1])
	})
}

func TestEgressCoverageCapacityFullReservationIsConditional(t *testing.T) {
	for _, tc := range []struct {
		blackhole, full int
		want            string
	}{
		{32, 2, "independent_drain_overlap_possible=true full_reserved_blackhole_concurrency_per_shard=30 full_reserved_total_blackhole_concurrency=120 full_reserved_timeout_ceiling_per_hour=28800"},
		{52, 2, "independent_drain_overlap_possible=true full_reserved_blackhole_concurrency_per_shard=50 full_reserved_total_blackhole_concurrency=200 full_reserved_timeout_ceiling_per_hour=48000"},
		{2, 2, "independent_drain_overlap_possible=false full_reserved_blackhole_concurrency_per_shard=unavailable"},
		{1, 2, "independent_drain_overlap_possible=false full_reserved_blackhole_concurrency_per_shard=unavailable"},
	} {
		geometry := egressCoverageGeometry{shardCount: 4, blackholeConcurrency: tc.blackhole, blackholeTimeoutSeconds: 15, fullConcurrency: tc.full, fullLimit: 8, fullTimeoutSeconds: 60}
		for _, eligible := range []int64{300, 301} {
			f, present := egressBlackholeCapacityFinding("pg-1", geometry, []egressCoverageSnapshot{{eligible: eligible, blackholeCurrent: 200, blackholeLastHour: 100}})
			if present != (eligible == 301) {
				t.Fatal("reserved-slot model changed the measured-rate PAGE predicate")
			}
			if present && (!strings.Contains(f.observed, tc.want) || !strings.Contains(f.context, "not runtime capability or queue-activity attestations")) {
				t.Fatal("capacity finding misstated the conditional full-reservation model")
			}
		}
	}
}

func syntheticEgressCoverageTask(t *testing.T, shardIndex, shardCount int) Row {
	t.Helper()
	args := egressCoverageTaskArgs{
		ShardIndex: shardIndex, ShardCount: shardCount,
		IdleDelaySeconds: 300, MaxTimeSeconds: 1800,
		Full: egressCoverageBatchArgs{
			Limit: 8, Concurrency: 2, ProbeTimeoutSeconds: 60,
			Bandwidth: true, BandwidthTimeoutSeconds: 5,
		},
		Blackhole:       egressCoverageBatchArgs{Limit: 250, Concurrency: 4, ProbeTimeoutSeconds: 15},
		APIURL:          "https://api.example.invalid",
		PlatformURL:     "wss://connect.example.invalid",
		PublicAPIURL:    "https://public-api.example.invalid",
		BandwidthCDNURL: "https://cdn.example.invalid/down",
	}
	return syntheticEgressCoverageTaskWithArgs(t, args)
}

func syntheticEgressCoverageTaskWithArgs(t *testing.T, args egressCoverageTaskArgs) Row {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return Row{
		fmt.Sprintf("[\"provider_egress_probe\",%d]", args.ShardIndex),
		string(raw),
		fmt.Sprintf("%d", args.MaxTimeSeconds),
	}
}

func syntheticEgressCoverageActivity(snapshot egressCoverageSnapshot) Row {
	// Existing aggregate-only fixtures describe coincident deadlines. Tests of
	// heterogeneous cohorts supply slack from the exact SQL prefix reducer.
	slack := func(value *int64, due, age, maxAge int64) string {
		if value != nil {
			return fmt.Sprint(*value)
		}
		if due == 0 || snapshot.fullAttemptsLastHour == 0 {
			return "unavailable"
		}
		return fmt.Sprint(maxAge - age - (due*3600+snapshot.fullAttemptsLastHour-1)/snapshot.fullAttemptsLastHour)
	}
	return Row{
		fmt.Sprint(snapshot.shardIndex), fmt.Sprint(snapshot.eligible),
		fmt.Sprint(snapshot.noLocationDue), fmt.Sprint(snapshot.staleLocationDue),
		fmt.Sprint(snapshot.staleHealthDue), fmt.Sprint(snapshot.staleLocationExpiredDue),
		fmt.Sprint(snapshot.staleHealthExpiredDue), fmt.Sprint(snapshot.blackholeDue),
		fmt.Sprint(snapshot.fullAgeSeconds), fmt.Sprint(snapshot.blackholeAgeSeconds),
		fmt.Sprint(snapshot.fullCurrent), fmt.Sprint(snapshot.blackholeCurrent),
		fmt.Sprint(snapshot.fullAttemptsLastHour), fmt.Sprint(snapshot.blackholeLastHour),
		fmt.Sprint(snapshot.staleLocationOldestAgeSeconds), fmt.Sprint(snapshot.staleHealthOldestAgeSeconds),
		fmt.Sprint(snapshot.missingHealthDue), fmt.Sprint(snapshot.missingHealthExpiredDue),
		fmt.Sprint(snapshot.missingHealthOldestAgeSeconds),
		fmt.Sprint(snapshot.deferredCurrentDarkDue),
		slack(snapshot.staleLocationDeadlineSlackSeconds, snapshot.staleLocationDue, snapshot.staleLocationOldestAgeSeconds, 7*24*3600),
		slack(snapshot.staleHealthDeadlineSlackSeconds, snapshot.staleHealthDue, snapshot.staleHealthOldestAgeSeconds, 24*3600),
		slack(snapshot.missingHealthDeadlineSlackSeconds, snapshot.missingHealthDue, snapshot.missingHealthOldestAgeSeconds, 24*3600),
	}
}

func requireEgressRolloutOrder(t *testing.T, markdown string) {
	t.Helper()
	previous := -1
	for _, marker := range []string{"migration 657", "API artifact", "Taskworker artifact"} {
		index := strings.Index(markdown, marker)
		if index < 0 {
			t.Fatalf("rollout guidance lacks %q:\n%s", marker, markdown)
		}
		if index <= previous {
			t.Fatalf("rollout guidance does not order migration 657, API, then Taskworker:\n%s", markdown)
		}
		previous = index
	}
}

func TestEgressCoverageSignalSyntheticUnarmedRollout(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"f", "f"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return nil, nil
		default:
			t.Fatalf("unexpected query after unarmed rollout: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "egress-probe-unarmed")
	markdown := alert.Markdown()
	for _, want := range []string{
		"tls_authentication_failure schema",
		"health-deadline ordered index",
		"durable ProviderEgressProbe tasks",
		"tls_integrity_armed=false",
		"provider_egress_task_rows=0",
		"not proof that zero providers need measurement",
		"pending append-only provider-egress migrations",
		"EDF scheduler",
		"selective attempt cleanup",
		"do not insert, delete, or hand-edit pending_task",
		"not a Proxy hardware-capacity alert",
		"valid, ready, non-partial btree over exactly (measured_at, client_id)",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("unarmed alert missing %q:\n%s", want, markdown)
		}
	}
	requireEgressRolloutOrder(t, markdown)
}

func TestEgressCoverageSignalSyntheticSchemaArmedTasksAbsent(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return nil, nil
		default:
			t.Fatalf("unexpected query after schema-only rollout: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "egress-probe-unarmed")
	markdown := alert.Markdown()
	for _, want := range []string{
		"tls_integrity_armed=true",
		"health_deadline_index_armed=true",
		"provider_egress_task_rows=0",
		"schema, including migration 657, is already armed",
		"API artifact containing the EDF scheduler",
		"Taskworker artifact containing selective attempt cleanup",
		"let normal task initialization converge the shards",
		"do not repeat migrations",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("schema-armed alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "Apply the pending append-only provider-egress migrations") {
		t.Fatalf("schema-armed alert still asks for the completed migration:\n%s", markdown)
	}
	if strings.Contains(markdown, "Taskworker artifact from the intentional checkout containing the scheduler correction") {
		t.Fatalf("schema-armed alert assigns the API scheduler to Taskworker:\n%s", markdown)
	}
	requireEgressRolloutOrder(t, markdown)
}

func TestEgressCoverageSignalSyntheticHealthDeadlineIndexAbsent(t *testing.T) {
	var schemaQuery string
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			schemaQuery = query
			return []Row{{"t", "f"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			return []Row{syntheticEgressCoverageActivity(egressCoverageSnapshot{
				shardIndex: 0, eligible: 1000, noLocationDue: 960,
				fullAgeSeconds: 10, blackholeAgeSeconds: 10,
				fullCurrent: 40, blackholeCurrent: 1000,
				fullAttemptsLastHour: 5, blackholeLastHour: 1,
				staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
			})}, nil
		default:
			t.Fatalf("unexpected query while the deadline index was absent: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "egress-probe-unarmed")
	markdown := alert.Markdown()
	for _, want := range []string{
		"ordered stale-health scheduler index",
		"health_deadline_index_armed=false",
		"Apply migration 657 through the append-only migration runner",
		"API artifact containing the EDF scheduler",
		"Taskworker artifact containing selective attempt cleanup",
		"valid, ready, non-partial btree over exactly (measured_at, client_id)",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("deadline-index alert missing %q:\n%s", want, markdown)
		}
	}
	requireEgressRolloutOrder(t, markdown)
	capacity := requireAlertClass(t, alerts, "egress-full-capacity")
	if !strings.Contains(capacity.Markdown(), "projected_drain=192h0m0s") {
		t.Fatalf("index rollout warning suppressed or changed the independent capacity finding:\n%s", capacity.Markdown())
	}
	for _, want := range []string{
		providerEgressHealthDeadlineIndexDefinition,
		"index_record.indpred IS NULL",
		"index_record.indisvalid",
		"index_record.indisready",
	} {
		if !strings.Contains(schemaQuery, want) {
			t.Fatalf("schema armedness query is missing exact index guard %q:\n%s", want, schemaQuery)
		}
	}
}

func TestEgressCoverageSignalSyntheticIncompleteShardGeometry(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{
				syntheticEgressCoverageTask(t, 0, 3),
				syntheticEgressCoverageTask(t, 2, 3),
			}, nil
		default:
			t.Fatalf("activity query ran for incomplete geometry: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "egress-probe-shards")
	for _, want := range []string{
		"missing_shard_1",
		"row_count_2_want_3",
		"healthy sibling task cannot compensate",
		"Do not manually clone, delete, or rewrite task rows",
		"never copies task IDs",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("geometry alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestEgressCoverageSignalSyntheticShardLocalStalls(t *testing.T) {
	var activityQuery string
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{
				syntheticEgressCoverageTask(t, 0, 2),
				syntheticEgressCoverageTask(t, 1, 2),
			}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			activityQuery = query
			return []Row{
				syntheticEgressCoverageActivity(egressCoverageSnapshot{
					shardIndex: 0, eligible: 22000, noLocationDue: 8, blackholeDue: 250,
					deferredCurrentDarkDue: 12000,
					fullAgeSeconds:         3600, blackholeAgeSeconds: 4200,
					fullCurrent: 400, blackholeCurrent: 18000,
					fullAttemptsLastHour: 100, blackholeLastHour: 9000,
					staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
				}),
				syntheticEgressCoverageActivity(egressCoverageSnapshot{
					shardIndex: 1, eligible: 21900, fullAgeSeconds: 200,
					blackholeAgeSeconds: 120, fullCurrent: 390, blackholeCurrent: 18100,
					fullAttemptsLastHour: 100, blackholeLastHour: 9000,
					staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
				}),
			}, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want full and blackhole stalls: %+v", len(alerts), alerts)
	}
	full := requireAlertClass(t, alerts, "egress-full-stalled")
	blackhole := requireAlertClass(t, alerts, "egress-blackhole-stalled")
	for _, alert := range []Alert{full, blackhole} {
		for _, want := range []string{
			"shard-0-of-2",
			"derived_stall_bound=40m0s",
			"healthy sibling shards",
			"no provider or task identifier",
			"Do not delete provider evidence",
		} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("stall alert missing %q:\n%s", want, alert.Markdown())
			}
		}
	}
	for _, notWant := range []string{"current-dark", "deferred_current_dark_due", "candidate suppression opportunity"} {
		if strings.Contains(blackhole.Markdown(), notWant) {
			t.Fatalf("blackhole stall alert contains full-queue wording %q:\n%s", notWant, blackhole.Markdown())
		}
	}
	for _, want := range []string{
		"deferred_current_dark_due=12000",
		"old-deployment attempts",
		"desired scheduler-state accounting",
		"candidate suppression opportunity",
	} {
		if !strings.Contains(full.Markdown(), want) {
			t.Fatalf("full stall alert missing %q:\n%s", want, full.Markdown())
		}
	}
	for _, want := range []string{
		"((hashtext(nclr.client_id::text) % 2) + 2) % 2",
		"interval '84 hours'",
		"interval '12 hours'",
		"interval '6 hours'",
		"interval '90 minutes'",
		"interval '3 hours'",
		"interval '1 hour'",
		"pbc.ok = false",
		") AS current_dark",
		"count(c.client_id) FILTER (WHERE c.no_location AND c.attempt_due AND NOT c.current_dark)",
		"FILTER (WHERE NOT c.current_dark) AS latest_full",
		"c.current_dark AND c.attempt_due AND c.urgent_lane <> ''",
		"count(c.client_id) FILTER (WHERE c.attempt_at >= c.now_utc - interval '1 hour')",
		"pel.observed_at + interval '7 days' <= peh.measured_at + interval '24 hours'",
		"WHEN peh.client_id IS NULL THEN 'missing-health'",
		"min(c.observed_at) FILTER (WHERE c.urgent_lane = 'stale-location' AND c.attempt_due AND NOT c.current_dark)",
		"min(c.measured_at) FILTER (WHERE c.urgent_lane = 'stale-health' AND c.attempt_due AND NOT c.current_dark)",
		"min(c.observed_at) FILTER (WHERE c.urgent_lane = 'missing-health' AND c.attempt_due AND NOT c.current_dark)",
	} {
		if !strings.Contains(activityQuery, want) {
			t.Fatalf("activity query missing %q:\n%s", want, activityQuery)
		}
	}
}

func TestEgressCoverageSignalSyntheticHealthyNoDueWork(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			// Old evidence is allowed when the corresponding due queues are empty.
			return []Row{syntheticEgressCoverageActivity(egressCoverageSnapshot{
				shardIndex: 0, eligible: 12, fullAgeSeconds: 604800,
				blackholeAgeSeconds: 10800, fullCurrent: 12, blackholeCurrent: 12,
				fullAttemptsLastHour: 1, blackholeLastHour: 1,
				staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
			})}, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy empty due queues returned alerts: %+v", alerts)
	}
}

func TestEgressCoverageActivityQueryUsesEarliestPresentLocationDeadline(t *testing.T) {
	query := egressCoverageActivityQuery(4)
	// For a 90-hour-old location and 25-hour-old health row, both scheduler
	// heads are eligible, but health expired one hour ago while location has 78
	// hours left. The location CASE must therefore win only on <=; the health
	// fallback owns this crossed-deadline row. A missing row is a separate
	// location-anchored lane, and no-location remains disjoint from all three.
	for _, want := range []string{
		"WHEN pel.client_id IS NULL THEN 'no-location'",
		"WHEN peh.client_id IS NULL THEN 'missing-health'",
		"pel.observed_at < lifecycle_clock.now_utc - interval '84 hours'",
		"peh.measured_at >= lifecycle_clock.now_utc - interval '12 hours' OR",
		"pel.observed_at + interval '7 days' <= peh.measured_at + interval '24 hours'",
		"WHEN peh.measured_at < lifecycle_clock.now_utc - interval '12 hours' THEN 'stale-health'",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("earliest-deadline query missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, "stale_health_exclusive") {
		t.Fatalf("location-priority health classification returned:\n%s", query)
	}
}

func TestEgressCoverageSignalSyntheticBlackholeCapacity(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			// Activity is fresh, so shard liveness is healthy. At 100 checks/hour,
			// 301 providers require just over the three-hour verdict lifetime.
			return []Row{syntheticEgressCoverageActivity(egressCoverageSnapshot{
				shardIndex: 0, eligible: 301, blackholeDue: 201,
				fullAgeSeconds:      10,
				blackholeAgeSeconds: 10, fullCurrent: 10, blackholeCurrent: 200,
				fullAttemptsLastHour: 1, blackholeLastHour: 100,
				staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
			})}, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one capacity alert: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "egress-blackhole-capacity")
	for _, want := range []string{
		"66.4%",
		"projected_sweep=3h0m36s",
		"required_per_hour=101",
		"configured_total_blackhole_concurrency=4",
		"blackhole_only_timeout_ceiling_per_hour=960",
		"blackhole_only_deadline_minimum_concurrency=1",
		"configured_full_limit_per_shard=8",
		"configured_full_concurrency_per_shard=2",
		"full_probe_timeout_seconds=60",
		"does not include residence time spent on full probes",
		"becomes selectable again without a successful recheck",
		"Run §2.23 and §2.24 first",
		"do not increase concurrency first",
		"not proof that Proxy hosts need more active-client hardware",
		"Provider, network, task, endpoint, and failure identities never leave",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("capacity alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestEgressCoverageSignalSyntheticConfiguredCapacityBounds(t *testing.T) {
	const shardCount = 3
	taskRows := make([]Row, 0, shardCount)
	for shardIndex := range shardCount {
		args := egressCoverageTaskArgs{
			ShardIndex: shardIndex, ShardCount: shardCount,
			IdleDelaySeconds: 120, MaxTimeSeconds: 900,
			Full: egressCoverageBatchArgs{
				Limit: 6, Concurrency: 2, ProbeTimeoutSeconds: 60,
			},
			Blackhole: egressCoverageBatchArgs{
				Limit: 90, Concurrency: 5, ProbeTimeoutSeconds: 20,
			},
			APIURL:      "https://api.example.invalid",
			PlatformURL: "wss://connect.example.invalid",
		}
		taskRows = append(taskRows, syntheticEgressCoverageTaskWithArgs(t, args))
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return taskRows, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			rows := make([]Row, 0, 3)
			for shardIndex := 0; shardIndex < 3; shardIndex++ {
				rows = append(rows, syntheticEgressCoverageActivity(egressCoverageSnapshot{
					shardIndex: shardIndex, eligible: 2000, blackholeDue: 1500,
					fullAgeSeconds:      10,
					blackholeAgeSeconds: 10, fullCurrent: 100, blackholeCurrent: 1000,
					fullAttemptsLastHour: 1, blackholeLastHour: 200,
					staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
				}))
			}
			return rows, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "egress-blackhole-capacity")
	for _, want := range []string{
		"configured_shards=3",
		"configured_blackhole_concurrency_per_shard=5",
		"configured_total_blackhole_concurrency=15",
		"blackhole_probe_timeout_seconds=20",
		"blackhole_only_timeout_ceiling_per_hour=2700",
		"blackhole_only_deadline_minimum_concurrency=12",
		"configured_full_limit_per_shard=6",
		"configured_full_concurrency_per_shard=2",
		"full_probe_timeout_seconds=60",
		"not a whole-task ceiling",
		"capacity-test any geometry change",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("configured capacity alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestEgressCoverageSignalSyntheticBlackholeCapacityBoundary(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		eligible string
		current  string
		want     int
	}{
		{name: "exact three hours", eligible: "300", current: "299", want: 0},
		{name: "complete despite quiet hour", eligible: "301", current: "301", want: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				switch {
				case strings.Contains(query, "pg_attribute"):
					return []Row{{"t", "t"}}, nil
				case strings.Contains(query, "FROM pending_task"):
					return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
				case strings.Contains(query, "WITH lifecycle_clock AS"):
					blackholeDue := "1"
					if testCase.current == testCase.eligible {
						blackholeDue = "0"
					}
					eligible, _ := strconv.ParseInt(testCase.eligible, 10, 64)
					current, _ := strconv.ParseInt(testCase.current, 10, 64)
					due, _ := strconv.ParseInt(blackholeDue, 10, 64)
					return []Row{syntheticEgressCoverageActivity(egressCoverageSnapshot{
						shardIndex: 0, eligible: eligible, blackholeDue: due,
						fullAgeSeconds:      10,
						blackholeAgeSeconds: 10, fullCurrent: 10, blackholeCurrent: current,
						fullAttemptsLastHour: 1, blackholeLastHour: 100,
						staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
					})}, nil
				default:
					t.Fatalf("unexpected provider coverage query: %s", query)
					return nil, nil
				}
			}}
			alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != testCase.want {
				t.Fatalf("alerts = %d, want %d: %+v", len(alerts), testCase.want, alerts)
			}
		})
	}
}

func TestEgressCoverageSignalSyntheticFullCapacity(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			return []Row{syntheticEgressCoverageActivity(egressCoverageSnapshot{
				shardIndex: 0, eligible: 1000,
				noLocationDue: 900, staleLocationDue: 50, staleHealthDue: 10,
				deferredCurrentDarkDue: 30,
				fullAgeSeconds:         100,
				blackholeAgeSeconds:    100, fullCurrent: 40, blackholeCurrent: 1000,
				fullAttemptsLastHour:          5,
				blackholeLastHour:             1,
				staleLocationOldestAgeSeconds: int64((90 * time.Hour) / time.Second),
				staleHealthOldestAgeSeconds:   int64((13 * time.Hour) / time.Second),
			})}, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one full-capacity alert: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "egress-full-capacity")
	for _, want := range []string{
		"current_percent=4.0", "due=960", "deferred_current_dark_due=30", "attempted_last_hour=5",
		"required_per_hour=6", "projected_drain=192h0m0s",
		"configured_total_full_concurrency=2", "gross full-probe execution capacity",
		"desired scheduler-state accounting", "candidate suppression opportunity",
		"one-hour gross-rate rollout window clears",
		"no product decision for a maximum retry interval", "Do not invent fixed lane weights",
		"complete seven-day sweep", "identities never leave the database",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("full-capacity alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestEgressFullCapacitySevenDayBoundary(t *testing.T) {
	geometry := egressCoverageGeometry{shardCount: 1, fullConcurrency: 2}
	boundary := egressCoverageSnapshot{
		eligible: 841, fullCurrent: 1, noLocationDue: 840,
		fullAttemptsLastHour: 5,
	}
	if _, page := egressFullCapacityFinding("synthetic", geometry, []egressCoverageSnapshot{boundary}); page {
		t.Fatal("an exact seven-day projected full drain must remain healthy")
	}
	boundary.noLocationDue++
	boundary.eligible++
	if _, page := egressFullCapacityFinding("synthetic", geometry, []egressCoverageSnapshot{boundary}); !page {
		t.Fatal("a projected full drain one provider beyond the exact seven-day boundary must page")
	}
	healthDueAtCompleteLocationCoverage := egressCoverageSnapshot{
		eligible: 1000, fullCurrent: 1000, staleHealthDue: 960,
		fullAttemptsLastHour: 5,
	}
	if _, page := egressFullCapacityFinding("synthetic", geometry, []egressCoverageSnapshot{healthDueAtCompleteLocationCoverage}); !page {
		t.Fatal("complete location coverage must not suppress a stale-health full-capacity failure")
	}
}

func TestEgressCoverageSignalSyntheticFullFairnessIsCategoryLocal(t *testing.T) {
	rows := []Row{
		syntheticEgressCoverageActivity(egressCoverageSnapshot{
			shardIndex: 0, eligible: 500, noLocationDue: 100, staleLocationDue: 5,
			staleLocationExpiredDue: 5, fullAgeSeconds: 10,
			blackholeAgeSeconds: 10, fullCurrent: 395, blackholeCurrent: 500,
			fullAttemptsLastHour: 100, blackholeLastHour: 1,
			staleLocationOldestAgeSeconds: int64((170 * time.Hour) / time.Second),
			staleHealthOldestAgeSeconds:   -1,
		}),
		syntheticEgressCoverageActivity(egressCoverageSnapshot{
			shardIndex: 1, eligible: 500, noLocationDue: 100, staleHealthDue: 7,
			staleHealthExpiredDue: 7, fullAgeSeconds: 10,
			blackholeAgeSeconds: 10, fullCurrent: 393, blackholeCurrent: 500,
			fullAttemptsLastHour: 100, blackholeLastHour: 1,
			staleLocationOldestAgeSeconds: -1,
			staleHealthOldestAgeSeconds:   int64((25 * time.Hour) / time.Second),
		}),
	}
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 2), syntheticEgressCoverageTask(t, 1, 2)}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			return rows, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want one fairness alert per urgent category: %+v", len(alerts), alerts)
	}
	for _, frame := range []string{"shard-0-of-2/stale-location", "shard-1-of-2/stale-health"} {
		var alert *Alert
		for i := range alerts {
			if alerts[i].Class == "egress-full-fairness" && alerts[i].Frame == frame {
				alert = &alerts[i]
				break
			}
		}
		if alert == nil {
			t.Fatalf("missing fairness frame %s: %+v", frame, alerts)
		}
		for _, want := range []string{
			"past its absolute deadline",
			"deadline_missed=true", "expired_due=",
			"all_shards_unlocated_can_fill_batch=true", "output-bounded anti-health head",
			"cumulative deadline prefix", "not itself a post-EDF failure",
			"First attempts and retries",
			"No provider or task identifier leaves PostgreSQL",
		} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("fairness alert %s missing %q:\n%s", frame, want, alert.Markdown())
			}
		}
	}
}

func TestEgressCoverageSignalSyntheticFullFairnessDeadlineProjection(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			return []Row{syntheticEgressCoverageActivity(egressCoverageSnapshot{
				shardIndex: 0, eligible: 100, noLocationDue: 8, staleHealthDue: 20,
				fullAgeSeconds:      10,
				blackholeAgeSeconds: 10, fullCurrent: 72, blackholeCurrent: 100,
				fullAttemptsLastHour: 1, blackholeLastHour: 1,
				staleLocationOldestAgeSeconds: -1,
				staleHealthOldestAgeSeconds:   int64((13 * time.Hour) / time.Second),
			})}, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "egress-full-fairness")
	for _, want := range []string{
		"optimistic_all_capacity_drain=20h0m0s", "remaining_deadline_window=11h0m0s",
		"deadline_missed=false", "deadline_at_risk=true",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("deadline fairness alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestEgressCoverageSignalSyntheticMissingHealthFairness(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			return []Row{syntheticEgressCoverageActivity(egressCoverageSnapshot{
				shardIndex: 0, eligible: 100, missingHealthDue: 3,
				missingHealthExpiredDue: 1, fullAgeSeconds: 10,
				blackholeAgeSeconds: 10, fullCurrent: 100, blackholeCurrent: 100,
				fullAttemptsLastHour: 100, blackholeLastHour: 1,
				staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
				missingHealthOldestAgeSeconds: int64((25 * time.Hour) / time.Second),
			})}, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	var alert *Alert
	for i := range alerts {
		if alerts[i].Class == "egress-full-fairness" && alerts[i].Frame == "shard-0-of-1/missing-health" {
			alert = &alerts[i]
			break
		}
	}
	if alert == nil {
		t.Fatalf("missing missing-health fairness frame: %+v", alerts)
	}
	for _, want := range []string{
		"category=missing-health", "due=3", "expired_due=1",
		"oldest_deadline_anchor_age=25h0m0s", "remaining_deadline_window=-1h0m0s",
		"location observed_at plus the existing 24-hour health lifetime",
		"does not claim that health evidence ever existed",
		"keep missing-health at zero or advancing after attempt backoff",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("missing-health fairness alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestEgressCoverageSignalSyntheticFullBoundariesAreHealthy(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t", "t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		case strings.Contains(query, "WITH lifecycle_clock AS"):
			return []Row{syntheticEgressCoverageActivity(egressCoverageSnapshot{
				shardIndex: 0, eligible: 100, noLocationDue: 8, staleHealthDue: 10, missingHealthDue: 10,
				fullAgeSeconds:      10,
				blackholeAgeSeconds: 10, fullCurrent: 82, blackholeCurrent: 100,
				fullAttemptsLastHour: 1, blackholeLastHour: 1,
				staleLocationOldestAgeSeconds: -1,
				staleHealthOldestAgeSeconds:   int64((14 * time.Hour) / time.Second),
				missingHealthOldestAgeSeconds: int64((14 * time.Hour) / time.Second),
			})}, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := syntheticEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("exact urgency projection and hard-expiry boundaries must be healthy: %+v", alerts)
	}
}

func TestInspectEgressCoverageTasksRedactsMalformedArguments(t *testing.T) {
	secret := "do-not-copy-this-secret"
	_, err := inspectEgressCoverageTasks([]pgRow{{"wrong", "{" + secret, "1800"}})
	if err == nil {
		t.Fatal("malformed task arguments were accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("malformed task payload leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "malformed_args") {
		t.Fatalf("malformed task error lost its structural class: %v", err)
	}
}

func TestInspectEgressCoverageTasksRejectsUnknownExecutionSettings(t *testing.T) {
	secret := "do-not-copy-this-unknown-setting"
	row := syntheticEgressCoverageTask(t, 0, 1)
	row[1] = strings.TrimSuffix(row[1], "}") + `,"future_execution_endpoint":"` + secret + `"}`
	_, err := inspectEgressCoverageTasks([]pgRow{pgRow(row)})
	if err == nil {
		t.Fatal("unknown task setting was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("unknown task setting leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "row_1_malformed_args") {
		t.Fatalf("unknown task setting lost its structural class: %v", err)
	}
}

func TestInspectEgressCoverageTasksRejectsMixedCompleteExecutionSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*egressCoverageTaskArgs)
	}{
		{name: "all destinations", mutate: func(args *egressCoverageTaskArgs) {
			args.Full.AllDestinations = !args.Full.AllDestinations
			args.Full.ProbeTimeoutSeconds = syntheticEgressMinimumFullTimeoutSeconds(args.Full.AllDestinations)
		}},
		{name: "bandwidth enabled", mutate: func(args *egressCoverageTaskArgs) {
			args.Full.Bandwidth = !args.Full.Bandwidth
		}},
		{name: "bandwidth timeout", mutate: func(args *egressCoverageTaskArgs) {
			args.Full.BandwidthTimeoutSeconds++
		}},
		{name: "blackhole all destinations", mutate: func(args *egressCoverageTaskArgs) {
			args.Blackhole.AllDestinations = !args.Blackhole.AllDestinations
		}},
		{name: "blackhole bandwidth", mutate: func(args *egressCoverageTaskArgs) {
			args.Blackhole.Bandwidth = true
			args.Blackhole.BandwidthTimeoutSeconds = 5
		}},
		{name: "public API endpoint", mutate: func(args *egressCoverageTaskArgs) {
			args.PublicAPIURL = "https://other-public-api.example.invalid"
		}},
		{name: "bandwidth CDN endpoint", mutate: func(args *egressCoverageTaskArgs) {
			args.BandwidthCDNURL = "https://other-cdn.example.invalid/down"
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			first := syntheticEgressCoverageTask(t, 0, 2)
			var secondArgs egressCoverageTaskArgs
			if err := json.Unmarshal([]byte(syntheticEgressCoverageTask(t, 1, 2)[1]), &secondArgs); err != nil {
				t.Fatal(err)
			}
			testCase.mutate(&secondArgs)
			second := syntheticEgressCoverageTaskWithArgs(t, secondArgs)
			_, err := inspectEgressCoverageTasks([]pgRow{pgRow(first), pgRow(second)})
			if err == nil || !strings.Contains(err.Error(), "row_2_mixed_settings") {
				t.Fatalf("mixed %s setting was not rejected: %v", testCase.name, err)
			}
			if strings.Contains(err.Error(), "example.invalid") {
				t.Fatalf("mixed %s setting leaked endpoint values: %v", testCase.name, err)
			}
		})
	}
}

func TestInspectEgressCoverageTasksRejectsInvalidBandwidthTimeout(t *testing.T) {
	row := syntheticEgressCoverageTask(t, 0, 1)
	var args egressCoverageTaskArgs
	if err := json.Unmarshal([]byte(row[1]), &args); err != nil {
		t.Fatal(err)
	}
	args.Full.BandwidthTimeoutSeconds = 0
	row = syntheticEgressCoverageTaskWithArgs(t, args)
	_, err := inspectEgressCoverageTasks([]pgRow{pgRow(row)})
	if err == nil || !strings.Contains(err.Error(), "row_1_invalid_settings") {
		t.Fatalf("enabled bandwidth with no timeout was not rejected: %v", err)
	}
}

func TestParseEgressCoverageActivityRejectsAmbiguousRows(t *testing.T) {
	valid := func(shardIndex int) pgRow {
		return pgRow(syntheticEgressCoverageActivity(egressCoverageSnapshot{
			shardIndex: shardIndex, eligible: 1,
			fullAgeSeconds: -1, blackholeAgeSeconds: -1,
			fullCurrent: 1, blackholeCurrent: 1, blackholeLastHour: 1,
			staleLocationOldestAgeSeconds: -1, staleHealthOldestAgeSeconds: -1,
			missingHealthOldestAgeSeconds: -1,
		}))
	}
	negative := valid(0)
	negative[2] = "-1"
	tooCurrent := valid(0)
	tooCurrent[10] = "2"
	tooManyBlackhole := valid(0)
	tooManyBlackhole[13] = "2"
	expiredExceedsDue := valid(0)
	expiredExceedsDue[5] = "1"
	missingOldest := valid(0)
	missingOldest[3] = "1"
	missingHealthExpiredExceedsDue := valid(0)
	missingHealthExpiredExceedsDue[17] = "1"
	missingHealthOldestAbsent := valid(0)
	missingHealthOldestAbsent[16] = "1"
	deferredExceedsEligible := valid(0)
	deferredExceedsEligible[19] = "2"
	dueAndDeferredOverlap := valid(0)
	dueAndDeferredOverlap[2] = "1"
	dueAndDeferredOverlap[19] = "1"
	for name, rows := range map[string][]pgRow{
		"missing shard":                          {valid(0)},
		"negative count":                         {negative, valid(1)},
		"duplicate shard":                        {valid(0), valid(0)},
		"current exceeds":                        {tooCurrent, valid(1)},
		"last hour exceeds":                      {tooManyBlackhole, valid(1)},
		"expired exceeds due":                    {expiredExceedsDue, valid(1)},
		"due has no oldest age":                  {missingOldest, valid(1)},
		"missing health expired exceeds due":     {missingHealthExpiredExceedsDue, valid(1)},
		"missing health due has no oldest age":   {missingHealthOldestAbsent, valid(1)},
		"deferred current dark exceeds eligible": {deferredExceedsEligible, valid(1)},
		"due and deferred overlap":               {dueAndDeferredOverlap, valid(1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseEgressCoverageActivity(rows, 2); err == nil {
				t.Fatal("ambiguous activity rows were accepted")
			}
		})
	}
}

type syntheticEgressDeadlineRow struct {
	ShardIndex     int    `json:"shard_index"`
	Lane           string `json:"urgent_lane"`
	DeadlineMicros int64  `json:"deadline_offset_microseconds"`
	AttemptDue     bool   `json:"attempt_due"`
	CurrentDark    bool   `json:"current_dark"`
}

// Execute the production prefix CTEs against values only: no production table,
// identity, clock, write, or duplicate Go implementation of the SQL reducer.
func syntheticEgressDeadlineSlacks(t *testing.T, ctx context.Context, conn server.PgConn, input []syntheticEgressDeadlineRow, rates map[int]int64) map[int][3]*int64 {
	t.Helper()
	if input == nil {
		input = []syntheticEgressDeadlineRow{}
	}
	rateRows := []map[string]int64{}
	for shard, rate := range rates {
		rateRows = append(rateRows, map[string]int64{"shard_index": int64(shard), "full_attempted_last_hour": rate})
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	rateJSON, err := json.Marshal(rateRows)
	if err != nil {
		t.Fatal(err)
	}
	query := `WITH fixture AS (
	 SELECT * FROM jsonb_to_recordset($1::jsonb) AS input(
	  shard_index integer, urgent_lane text, deadline_offset_microseconds bigint,
	  attempt_due boolean, current_dark boolean)
	), classified AS (
	 SELECT shard_index, urgent_lane, attempt_due, current_dark,
	  timestamp '2026-01-01 00:00:00' + (deadline_offset_microseconds::text || ' microseconds')::interval -
	   CASE WHEN urgent_lane = 'stale-location' THEN interval '7 days' ELSE interval '24 hours' END AS observed_at,
	  timestamp '2026-01-01 00:00:00' + (deadline_offset_microseconds::text || ' microseconds')::interval - interval '24 hours' AS measured_at
	 FROM fixture
	), snapshot AS (
	 SELECT shard_index, full_attempted_last_hour, timestamp '2026-01-01 00:00:00' AS now_utc
	 FROM jsonb_to_recordset($2::jsonb) AS rates(shard_index integer, full_attempted_last_hour bigint)
	)` + egressCoverageDeadlineCTEs + `
	 SELECT shard_index,
	  COALESCE(stale_location_deadline_slack_seconds::text, 'unavailable'),
	  COALESCE(stale_health_deadline_slack_seconds::text, 'unavailable'),
	  COALESCE(missing_health_deadline_slack_seconds::text, 'unavailable')
	 FROM snapshot LEFT JOIN deadline_slack USING (shard_index) ORDER BY shard_index`
	rows, err := conn.Query(ctx, query, string(inputJSON), string(rateJSON))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := map[int][3]*int64{}
	for rows.Next() {
		var shard int
		var fields [3]string
		if err := rows.Scan(&shard, &fields[0], &fields[1], &fields[2]); err != nil {
			t.Fatal(err)
		}
		var values [3]*int64
		for i, field := range fields {
			if field != "unavailable" {
				value, err := strconv.ParseInt(field, 10, 64)
				if err != nil {
					t.Fatal(err)
				}
				values[i] = &value
			}
		}
		result[shard] = values
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestEgressCoverageDeadlinePrefixSQL(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("deadline-prefix query fixtures require the attested local test environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	row := func(shard int, lane string, deadlineMicros int64) syntheticEgressDeadlineRow {
		return syntheticEgressDeadlineRow{ShardIndex: shard, Lane: lane, DeadlineMicros: deadlineMicros, AttemptDue: true}
	}
	server.Db(ctx, func(conn server.PgConn) {
		var previousLegacyRow Row
		for _, coincident := range []bool{false, true} {
			t.Run(fmt.Sprintf("aggregate-equivalent-coincident-%t", coincident), func(t *testing.T) {
				input := []syntheticEgressDeadlineRow{row(0, "stale-health", 5590*1e6)}
				for i := 1; i < 188; i++ {
					deadline := int64(43140)
					if coincident {
						deadline = 5590
					}
					input = append(input, row(0, "stale-health", deadline*1e6))
				}
				// Independent EDF oracle: fixed throughput, one attempt per row.
				misses := 0
				for i, candidate := range input {
					if int64(i+1)*3600*1e6 > candidate.DeadlineMicros*78 {
						misses++
					}
				}
				if (misses > 0) != coincident {
					t.Fatalf("counterexample EDF misses=%d coincident=%t", misses, coincident)
				}
				values := syntheticEgressDeadlineSlacks(t, ctx, conn, input, map[int]int64{0: 78})[0]
				want := int64(5543)
				if coincident {
					want = -3087
				}
				if values[1] == nil || *values[1] != want {
					t.Fatalf("deadline-prefix slack=%v, want %d", values[1], want)
				}
				snapshot := egressCoverageSnapshot{
					eligible: 188, staleHealthDue: 188, fullAttemptsLastHour: 78,
					fullCurrent: 188, blackholeCurrent: 188, blackholeLastHour: 1,
					fullAgeSeconds: 10, blackholeAgeSeconds: 10,
					staleLocationOldestAgeSeconds: -1, missingHealthOldestAgeSeconds: -1,
					staleHealthOldestAgeSeconds: 80810, staleHealthDeadlineSlackSeconds: values[1],
				}
				activity := syntheticEgressCoverageActivity(snapshot)
				if previousLegacyRow != nil && strings.Join(previousLegacyRow[:20], "|") != strings.Join(activity[:20], "|") {
					t.Fatal("counterexample did not preserve the original twenty aggregate columns")
				}
				previousLegacyRow = activity
				if 24*3600-snapshot.staleHealthOldestAgeSeconds >= (snapshot.staleHealthDue*3600+77)/78 {
					t.Fatal("fixture does not reproduce the former false at-risk predicate")
				}
				source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
					switch {
					case strings.Contains(query, "pg_attribute"):
						return []Row{{"t", "t"}}, nil
					case strings.Contains(query, "FROM pending_task"):
						return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
					case strings.Contains(query, "WITH lifecycle_clock AS"):
						if !strings.Contains(query, egressCoverageDeadlineCTEs) {
							t.Fatal("production activity query omitted the exercised deadline-prefix reducer")
						}
						return []Row{activity}, nil
					default:
						t.Fatal("unexpected query")
						return nil, nil
					}
				}}
				alerts, err := syntheticEgressCoverageSignal().Run(ctx, syntheticSettings(source))
				if err != nil {
					t.Fatal(err)
				}
				if !coincident {
					if len(alerts) != 0 {
						t.Fatal("feasible EDF cohort produced a false alert")
					}
					return
				}
				alert := requireAlertClass(t, alerts, "egress-full-fairness")
				for _, required := range []string{"minimum_deadline_prefix_slack_seconds=-3087", "deadline_missed=false", "deadline_at_risk=true", "conditional on unchanged throughput", "not the at-risk predicate"} {
					if !strings.Contains(alert.Markdown(), required) {
						t.Fatalf("deadline-prefix alert omitted %q", required)
					}
				}
				for _, unsupported := range []string{"no unknown allocation can make the deadline", "cannot meet the oldest row's remaining window", "do-not-copy-provider"} {
					if strings.Contains(alert.Markdown(), unsupported) {
						t.Fatalf("deadline-prefix alert contains unsupported or private text %q", unsupported)
					}
				}
			})
		}
		for _, test := range []struct {
			name   string
			micros []int64
			rate   int64
			want   string
		}{
			{"exact-zero", []int64{3600 * 1e6}, 1, "0"},
			{"plus-one-second", []int64{3601 * 1e6}, 1, "1"},
			{"minus-one-second", []int64{3599 * 1e6}, 1, "-1"},
			{"plus-one-microsecond", []int64{3600*1e6 + 1}, 1, "0"},
			{"minus-one-microsecond", []int64{3600*1e6 - 1}, 1, "-1"},
			{"fractional-drain-feasible", []int64{514300000}, 7, "0"},
			{"fractional-drain-infeasible", []int64{514280000}, 7, "-1"},
			{"interior-prefix", []int64{3600 * 1e6, 5400 * 1e6, 5400 * 1e6, 43000 * 1e6}, 1, "-5400"},
			{"zero-rate", []int64{3600 * 1e6}, 0, "unavailable"},
			{"empty-category", nil, 1, "unavailable"},
		} {
			t.Run(test.name, func(t *testing.T) {
				input := []syntheticEgressDeadlineRow{}
				for _, micros := range test.micros {
					input = append(input, row(0, "stale-health", micros))
				}
				value := syntheticEgressDeadlineSlacks(t, ctx, conn, input, map[int]int64{0: test.rate})[0][1]
				actual := "unavailable"
				if value != nil {
					actual = fmt.Sprint(*value)
				}
				if actual != test.want {
					t.Fatalf("slack=%s want=%s", actual, test.want)
				}
			})
		}
		t.Run("shard-category-and-eligibility-isolation", func(t *testing.T) {
			input := []syntheticEgressDeadlineRow{
				row(0, "stale-health", 3600*1e6), row(1, "stale-health", 3600*1e6),
				row(0, "stale-location", 3600*1e6), row(0, "stale-location", 3600*1e6),
				row(0, "missing-health", 7200*1e6), row(0, "no-location", -1),
				{ShardIndex: 0, Lane: "stale-health", DeadlineMicros: -1, AttemptDue: false},
				{ShardIndex: 0, Lane: "stale-health", DeadlineMicros: -1, AttemptDue: true, CurrentDark: true},
			}
			values := syntheticEgressDeadlineSlacks(t, ctx, conn, input, map[int]int64{0: 1, 1: 100})
			for _, expected := range []struct {
				shard, category int
				slack           int64
			}{{0, 0, -3600}, {0, 1, 0}, {0, 2, 3600}, {1, 1, 3564}} {
				value := values[expected.shard][expected.category]
				if value == nil || *value != expected.slack {
					t.Fatalf("partitioned slack mismatch shard=%d category=%d", expected.shard, expected.category)
				}
			}
			if values[1][0] != nil || values[1][2] != nil {
				t.Fatal("category observations leaked across shards")
			}
		})
	})
}

func TestEgressCoverageDeadlineSlackUnknownAndExpiredBoundaries(t *testing.T) {
	base := egressCoverageSnapshot{eligible: 1, staleHealthDue: 1, fullAttemptsLastHour: 1, staleHealthOldestAgeSeconds: 23 * 3600}
	for name, field := range map[string]string{
		"missing": "unavailable", "empty": "", "malformed": "do-not-copy-provider", "overflow": "9223372036854775808",
	} {
		t.Run(name, func(t *testing.T) {
			row := syntheticEgressCoverageActivity(base)
			row[21] = field
			_, err := parseEgressCoverageActivity([]pgRow{pgRow(row)}, 1)
			if err == nil || strings.Contains(err.Error(), "do-not-copy-provider") {
				t.Fatal("missing/invalid slack did not fail closed with a structural error")
			}
		})
	}
	for _, snapshot := range []egressCoverageSnapshot{{eligible: 1}, {eligible: 1, staleHealthDue: 1, staleHealthOldestAgeSeconds: 23 * 3600}} {
		row := syntheticEgressCoverageActivity(snapshot)
		row[21] = "0"
		if _, err := parseEgressCoverageActivity([]pgRow{pgRow(row)}, 1); err == nil {
			t.Fatal("slack accepted without due work or a gross rate")
		}
	}
	legacy := syntheticEgressCoverageActivity(base)[:20]
	if _, err := parseEgressCoverageActivity([]pgRow{pgRow(legacy)}, 1); err == nil {
		t.Fatal("legacy aggregate-only response accepted as deadline evidence")
	}
	base.fullAttemptsLastHour = 0
	base.staleHealthExpiredDue = 1
	base.staleHealthOldestAgeSeconds = 25 * 3600
	parsed, err := parseEgressCoverageActivity([]pgRow{pgRow(syntheticEgressCoverageActivity(base))}, 1)
	if err != nil {
		t.Fatal(err)
	}
	findings := egressFullFairnessFindings("synthetic", egressCoverageGeometry{shardCount: 1, fullLimit: 8}, parsed)
	if len(findings) != 1 || !strings.Contains(findings[0].observed, "deadline_missed=true deadline_at_risk=false") || !strings.Contains(findings[0].observed, "minimum_deadline_prefix_slack_seconds=unavailable_zero_gross_attempts") {
		t.Fatal("zero-rate forecast suppressed a proved expiry or claimed numeric slack")
	}
}
