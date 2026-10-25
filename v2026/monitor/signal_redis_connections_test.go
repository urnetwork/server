package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestRedisConnectionsSignalSyntheticClientSpike(t *testing.T) {
	redisBatteryCalls := 0
	connectionBatteryCalls := 0
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "CLIENT LIST") {
				connectionBatteryCalls++
				return redisConnectionBatteryFixture(map[string]string{
					"reliability_marker_owner_port": "6382",
					"client_list_total":             "96",
					"client_cmd_sadd_count":         "96",
					"client_max_idle_s":             "8",
					"client_max_age_s":              "900",
				}), nil
			}
			// Every row is also above the memory warning threshold. The
			// connection wrapper must not run the memory signal's battery.
			return "6380 900 1000 890 1 10\n6381 900 1000 890 1 10\n6382 900 1000 890 1 100", nil
		},
		redisFn: func(HostSettings, int, ...string) (string, error) {
			redisBatteryCalls++
			return "", nil
		},
	}
	alerts, err := NewRedisConnectionsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "clients-spike")
	if connectionBatteryCalls != 1 || !strings.Contains(alert.Evidence, "client_cmd_sadd_count=96") ||
		!strings.Contains(alert.Mechanism, "candidate legacy fixed-slot writer") ||
		!strings.Contains(alert.Evidence, "reliability_marker_owner_port=6382") ||
		!strings.Contains(alert.Mechanism, "client_reliability_stats_blocks") ||
		!strings.Contains(alert.Action, "Independently confirm the caller, accessed key and running artifact") ||
		!strings.Contains(alert.Verify, "independent caller, key and running-artifact evidence") {
		t.Fatalf("connection battery calls=%d evidence=%q", connectionBatteryCalls, alert.Evidence)
	}
	if redisBatteryCalls != 0 {
		t.Fatalf("connection-only signal ran %d memory battery command(s)", redisBatteryCalls)
	}
}

func TestRedisConnectionsSignalDoesNotMisdiagnoseGenericHotNodeAsFixedSlot(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "CLIENT LIST") {
				return redisConnectionBatteryFixture(map[string]string{
					"client_list_total":     "543",
					"client_cmd_sadd_count": "7",
					"client_max_idle_s":     "231",
					"client_max_age_s":      "3902",
				}), nil
			}
			return "6380 100 1000 90 1 10\n6381 100 1000 90 1 10\n6382 100 1000 90 1 55", nil
		},
	}
	alerts, err := NewRedisConnectionsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "clients-spike")
	markdown := alert.Markdown()
	for _, detail := range []string{
		"does not establish their workload cause",
		"theoretical key-slot assignments cannot identify callers",
		"Read node latency",
		"before any workload-specific rollout or restart",
		"independent caller, key and running-artifact evidence",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("generic connection diagnosis missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(alert.Action, "marker-free reliability writer") {
		t.Fatalf("generic PING/GET/EXEC shape received fixed-slot action: %s", alert.Action)
	}
}

func TestRedisConnectionsSignalProposesCurrentReliabilityShardCandidate(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "CLIENT LIST") {
				for _, fragment := range []string{
					"date +%s",
					"CLUSTER KEYSLOT client_reliability_stats.%s.%s",
					"current_reliability_shards_on_node=%s",
					"reliability_shards_recent_max=%s",
					"max_expire_idle",
					"done | redis-cli --raw",
					"info_integers clients Clients",
					"info_integers memory Memory",
					"INFO commandstats",
					"--latency",
					"hget_calls_delta",
					"ss -lnt",
					"client_cmd_hget_count",
					"client_output_memory_max_bytes",
				} {
					if !strings.Contains(command, fragment) {
						t.Fatalf("connection battery command missing %q:\n%s", fragment, command)
					}
				}
				if strings.Contains(command, "%!") {
					t.Fatalf("connection battery command contains a formatting failure:\n%s", command)
				}
				return redisConnectionBatteryFixture(map[string]string{
					"current_reliability_shards_on_node": "2",
					"reliability_shards_recent_max":      "2",
					"reliability_shards_recent_total":    "2",
					"used_memory_bytes":                  "1412572232",
					"mem_clients_normal_bytes":           "3716242",
					"latency_avg_ms":                     "0.279",
					"accept_send_q":                      "65535",
					"client_list_total":                  "472",
					"client_cmd_expire_count":            "472",
					"client_max_idle_s":                  "5",
					"max_expire_idle_s":                  "5",
					"client_max_age_s":                   "3600",
				}), nil
			}
			return "6380 100 1000 90 1 10\n6381 100 1000 90 1 10\n6382 100 1000 90 1 55", nil
		},
	}
	alerts, err := NewRedisConnectionsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "clients-spike")
	markdown := alert.Markdown()
	for _, detail := range []string{
		"At the connection-spike trip",
		"assigned this node 2 of 32 expected client-reliability shard names for the trip minute and 0 for the preceding minute",
		"not proof that those keys exist, were accessed or caused the connections",
		"candidate rotating reliability-workload fingerprint",
		"Trip-time Redis controls",
		"latency_avg_ms=0.279",
		"client_memory_bytes=3716242",
		"below their alert bands",
		"before any workload-specific rollout or restart",
		"independent caller, key and running-artifact evidence",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("shard-collision diagnosis missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(alert.Action, "restart writers normally") || strings.Contains(alert.Action, "Roll out the marker-free reliability writer") {
		t.Fatalf("shard collision received legacy or destructive action: %s", alert.Action)
	}
}

func TestRedisConnectionsSignalProposesPeerDeltaCandidate(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "CLIENT LIST") {
				return redisConnectionBatteryFixture(map[string]string{
					"used_memory_bytes":             "1060000000",
					"mem_clients_normal_bytes":      "2800000",
					"latency_avg_ms":                "0.487",
					"hget_calls_delta":              "602",
					"hget_calls_per_nominal_second": "301.000",
					"accept_send_q":                 "65535",
					"client_list_total":             "954",
					"client_cmd_hget_count":         "669",
					"client_max_idle_s":             "268",
					"client_max_age_s":              "5096",
				}), nil
			}
			return "6380 100 1000 90 1 281\n6381 100 1000 90 1 280\n6382 100 1000 90 1 954", nil
		},
	}
	alerts, err := NewRedisConnectionsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "clients-spike")
	markdown := alert.Markdown()
	for _, want := range []string{
		"669 of 954 clients ending in HGET",
		"nominal two-second budget=301.000",
		"Network-peer live-delta fanout is one candidate workload",
		"unrelated HGET traffic can produce the same cohort",
		"Do not kill Redis clients or raise pool floors",
		"neither a measured command rate nor workload or pressure authority",
		"below their alert bands",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("peer-delta diagnosis missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(alert.Action, "marker-free reliability writer") {
		t.Fatalf("peer-delta amplification received reliability-marker action: %s", alert.Action)
	}
}

func TestRedisConnectionsSignalQualifiesEarlierShardNameCalculation(t *testing.T) {
	evidence := redisConnectionBatteryFixture(map[string]string{
		"reliability_shard_lookback_blocks":        "6",
		"reliability_shards_recent_max":            "5",
		"reliability_shards_recent_max_age_blocks": "4",
		"reliability_shards_recent_total":          "9",
		"max_expire_idle_s":                        "259",
		"used_memory_bytes":                        "1403831800",
		"mem_clients_normal_bytes":                 "3897298",
		"latency_avg_ms":                           "0.319",
		"accept_send_q":                            "65535",
		"client_list_total":                        "1486",
		"client_cmd_expire_count":                  "531",
		"client_max_idle_s":                        "259",
		"client_max_age_s":                         "6770",
	})

	diagnosis := diagnoseRedisConnectionSpike(evidence)
	for _, want := range []string{
		"assigned this node 0 of 32 expected client-reliability shard names for the trip minute and 0 for the preceding minute",
		"bounded 6-block lookback through that same current slot map gives a maximum of 5 names",
		"age 4 block(s)",
		"not an observation of historical slot ownership or workload",
		"candidate rotating reliability-workload fingerprint",
		"below their alert bands",
	} {
		combined := diagnosis.mechanism + " " + diagnosis.context + " " + diagnosis.action
		if !strings.Contains(combined, want) {
			t.Fatalf("historical-collision diagnosis missing %q: %+v", want, diagnosis)
		}
	}
}

func TestRedisConnectionsSignalKeepsTripFrameDistinctFromSustainedCount(t *testing.T) {
	snapshotCalls := 0
	batteryCalls := 0
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "CLIENT LIST") {
				batteryCalls++
				return redisConnectionBatteryFixture(map[string]string{
					"previous_reliability_shards_on_node":      "1",
					"reliability_shard_lookback_blocks":        "6",
					"reliability_shards_recent_max":            "2",
					"reliability_shards_recent_max_age_blocks": "4",
					"reliability_shards_recent_total":          "5",
					"max_expire_idle_s":                        "299",
					"used_memory_bytes":                        "1408354048",
					"mem_clients_normal_bytes":                 "5753722",
					"latency_avg_ms":                           "0.297",
					"accept_send_q":                            "65535",
					"client_list_total":                        "55",
					"client_cmd_expire_count":                  "40",
					"client_max_idle_s":                        "299",
					"client_max_age_s":                         "4579",
				}), nil
			}
			snapshotCalls++
			if snapshotCalls == 1 {
				return "6380 100 1000 90 1 10\n6381 100 1000 90 1 10\n6382 100 1000 90 1 55", nil
			}
			return "6380 100 1000 90 1 10\n6381 100 1000 90 1 10\n6382 100 1000 90 1 45", nil
		},
	}
	signal := NewRedisConnectionsSignal()
	first, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if got := requireAlertClass(t, first, "clients-spike").Observed; !strings.Contains(got, "connected=55") {
		t.Fatalf("trip observation = %q; want connected=55", got)
	}
	second, err := signal.Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, second, "clients-spike")
	markdown := alert.Markdown()
	for _, want := range []string{
		"connected=45",
		"client_list_total=55",
		"battery collected once at trip",
		"At the connection-spike trip",
		"cached battery precedes the alert symptom's later sustained client ratio",
		"different CLIENT LIST total is expected",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("sustained alert does not distinguish trip and current frames; missing %q:\n%s", want, markdown)
		}
	}
	if batteryCalls != 1 {
		t.Fatalf("battery ran %d times; want exactly once across sustained samples", batteryCalls)
	}
}

func TestRedisConnectionsSignalRetainsPressureWithoutWorkloadAttribution(t *testing.T) {
	evidence := redisConnectionBatteryFixture(map[string]string{
		"current_reliability_shards_on_node":  "3",
		"previous_reliability_shards_on_node": "2",
		"reliability_shards_recent_max":       "3",
		"reliability_shards_recent_total":     "5",
		"blocked_clients":                     "2",
		"used_memory_bytes":                   "8589934592",
		"mem_clients_normal_bytes":            "3221225472",
		"latency_avg_ms":                      "12.500",
		"accept_recv_q":                       "7",
		"accept_send_q":                       "7",
		"client_list_total":                   "900",
		"client_cmd_expire_count":             "700",
		"client_output_memory_bytes":          "67108864",
		"client_output_memory_max_bytes":      "41943040",
		"client_output_over_1mb_count":        "2",
		"client_max_idle_s":                   "5",
		"max_expire_idle_s":                   "5",
		"client_max_age_s":                    "3600",
	})

	diagnosis := diagnoseRedisConnectionSpike(evidence)
	for _, want := range []string{
		"candidate rotating reliability-workload fingerprint",
		"At least one captured",
		"active Redis pressure",
		"retaining the workload cause as unknown",
	} {
		combined := diagnosis.mechanism + " " + diagnosis.context + " " + diagnosis.action
		if !strings.Contains(combined, want) {
			t.Fatalf("active-pressure diagnosis missing %q: %+v", want, diagnosis)
		}
	}
}
