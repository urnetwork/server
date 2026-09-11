package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
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

func syntheticEgressCoverageActivity(snapshot egressCoverageSnapshot) Row {
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
					fullAgeSeconds: 3600, blackholeAgeSeconds: 4200,
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
		"interval '12 hours'",
		"interval '6 hours'",
		"interval '90 minutes'",
		"interval '3 hours'",
		"interval '1 hour'",
		"count(c.client_id) FILTER (WHERE c.attempt_at >= c.now_utc - interval '1 hour')",
		"pel.observed_at + interval '7 days' <= peh.measured_at + interval '24 hours'",
		"WHEN peh.client_id IS NULL THEN 'missing-health'",
		"min(c.observed_at) FILTER (WHERE c.urgent_lane = 'stale-location' AND c.attempt_due)",
		"min(c.measured_at) FILTER (WHERE c.urgent_lane = 'stale-health' AND c.attempt_due)",
		"min(c.observed_at) FILTER (WHERE c.urgent_lane = 'missing-health' AND c.attempt_due)",
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
			alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
				fullAgeSeconds:      100,
				blackholeAgeSeconds: 100, fullCurrent: 40, blackholeCurrent: 1000,
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one full-capacity alert: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "egress-full-capacity")
	for _, want := range []string{
		"current_percent=4.0", "due=960", "attempted_last_hour=5",
		"required_per_hour=6", "projected_drain=192h0m0s",
		"configured_total_full_concurrency=2", "gross full-probe execution capacity",
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
			"all_shards_unlocated_can_fill_batch=true", "bounded indexed evidence heads",
			"success-inclusive lower bound", "not itself a post-EDF failure",
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
	alerts, err := NewEgressCoverageSignal().Run(context.Background(), syntheticSettings(source))
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
	for name, rows := range map[string][]pgRow{
		"missing shard":                        {valid(0)},
		"negative count":                       {negative, valid(1)},
		"duplicate shard":                      {valid(0), valid(0)},
		"current exceeds":                      {tooCurrent, valid(1)},
		"last hour exceeds":                    {tooManyBlackhole, valid(1)},
		"expired exceeds due":                  {expiredExceedsDue, valid(1)},
		"due has no oldest age":                {missingOldest, valid(1)},
		"missing health expired exceeds due":   {missingHealthExpiredExceedsDue, valid(1)},
		"missing health due has no oldest age": {missingHealthOldestAbsent, valid(1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseEgressCoverageActivity(rows, 2); err == nil {
				t.Fatal("ambiguous activity rows were accepted")
			}
		})
	}
}
