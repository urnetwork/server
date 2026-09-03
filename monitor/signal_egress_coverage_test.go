package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

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

func TestEgressCoverageSignalSyntheticUnarmedRollout(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"f"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return nil, nil
		default:
			t.Fatalf("unexpected query after unarmed rollout: %s", query)
			return nil, nil
		}
	}}
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "egress-probe-unarmed")
	for _, want := range []string{
		"tls_authentication_failure schema",
		"durable ProviderEgressProbe tasks",
		"tls_integrity_armed=false",
		"provider_egress_task_rows=0",
		"not proof that zero providers need measurement",
		"server commit 49b51eeb",
		"do not insert, delete, or hand-edit pending_task",
		"not a Proxy hardware-capacity alert",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("unarmed alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestEgressCoverageSignalSyntheticSchemaArmedTasksAbsent(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return nil, nil
		default:
			t.Fatalf("unexpected query after schema-only rollout: %s", query)
			return nil, nil
		}
	}}
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "egress-probe-unarmed")
	markdown := alert.Markdown()
	for _, want := range []string{
		"tls_integrity_armed=true",
		"provider_egress_task_rows=0",
		"schema is already armed",
		"Taskworker artifact from an intentional server checkout containing commit 49b51eeb",
		"let normal task initialization create the shards",
		"do not repeat the migration",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("schema-armed alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "Apply the append-only provider-egress migration") {
		t.Fatalf("schema-armed alert still asks for the completed migration:\n%s", markdown)
	}
}

func TestEgressCoverageSignalSyntheticIncompleteShardGeometry(t *testing.T) {
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "pg_attribute"):
			return []Row{{"t"}}, nil
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
			return []Row{{"t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{
				syntheticEgressCoverageTask(t, 0, 2),
				syntheticEgressCoverageTask(t, 1, 2),
			}, nil
		case strings.Contains(query, "WITH shards AS"):
			activityQuery = query
			return []Row{
				// shard, eligible, full due, blackhole due, newest full age,
				// newest blackhole age, current full, current blackhole
				{"0", "22000", "8", "250", "3600", "4200", "400", "18000"},
				{"1", "21900", "0", "0", "200", "120", "390", "18100"},
			}, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
	for _, want := range []string{
		"((hashtext(nclr.client_id::text) % 2) + 2) % 2",
		"interval '84 hours'",
		"interval '6 hours'",
		"interval '90 minutes'",
		"interval '3 hours'",
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
			return []Row{{"t"}}, nil
		case strings.Contains(query, "FROM pending_task"):
			return []Row{syntheticEgressCoverageTask(t, 0, 1)}, nil
		case strings.Contains(query, "WITH shards AS"):
			// Old evidence is allowed when the corresponding due queues are empty.
			return []Row{{"0", "12", "0", "0", "604800", "10800", "12", "12"}}, nil
		default:
			t.Fatalf("unexpected provider coverage query: %s", query)
			return nil, nil
		}
	}}
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy empty due queues returned alerts: %+v", alerts)
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
	for name, rows := range map[string][]pgRow{
		"missing shard": {{"0", "1", "0", "0", "1", "1", "1", "1"}},
		"negative count": {
			{"0", "1", "-1", "0", "1", "1", "1", "1"},
			{"1", "1", "0", "0", "1", "1", "1", "1"},
		},
		"duplicate shard": {
			{"0", "1", "0", "0", "1", "1", "1", "1"},
			{"0", "1", "0", "0", "1", "1", "1", "1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseEgressCoverageActivity(rows, 2); err == nil {
				t.Fatal("ambiguous activity rows were accepted")
			}
		})
	}
}
