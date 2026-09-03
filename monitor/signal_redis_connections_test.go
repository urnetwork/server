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
				return "reliability_marker_slot=123 reliability_marker_owner_port=6382 queried_node_owns_reliability_marker=true\n" +
					"96 max_idle_s=8 max_age_s=900 source=192.0.2.4 flags=N cmd=sadd lib=go-redis", nil
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
	if connectionBatteryCalls != 1 || !strings.Contains(alert.Evidence, "cmd=sadd") ||
		!strings.Contains(alert.Mechanism, "fixed-slot key") ||
		!strings.Contains(alert.Evidence, "queried_node_owns_reliability_marker=true") ||
		!strings.Contains(alert.Context, "client_reliability_stats_blocks") ||
		!strings.Contains(alert.Action, "marker-free reliability writer") ||
		!strings.Contains(alert.Verify, "SADD/EXPIRE cohorts disappear") {
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
				return "reliability_marker_slot=123 reliability_marker_owner_port=6381 queried_node_owns_reliability_marker=false\n" +
					"7 max_idle_s=231 max_age_s=3902 source=192.0.2.8 flags=N cmd=sadd lib=go-redis\n" +
					"271 max_idle_s=231 max_age_s=3146 source=192.0.2.8 flags=N cmd=ping lib=go-redis\n" +
					"171 max_idle_s=231 max_age_s=3534 source=192.0.2.8 flags=N cmd=get lib=go-redis\n" +
					"94 max_idle_s=231 max_age_s=3902 source=192.0.2.8 flags=N cmd=exec lib=go-redis", nil
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
		"does not prove the reliability-marker fingerprint",
		"node must own client_reliability_stats_blocks",
		"read node latency",
		"Do not apply the reliability-marker rollout solely from this alert",
		"stable expected hot-slot owner",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("generic connection diagnosis missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(alert.Action, "marker-free reliability writer") {
		t.Fatalf("generic PING/GET/EXEC shape received fixed-slot action: %s", alert.Action)
	}
}

func TestRedisConnectionsSignalAttributesCurrentReliabilityShardCollision(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "CLIENT LIST") {
				for _, fragment := range []string{
					"date +%s",
					"CLUSTER KEYSLOT client_reliability_stats.%s.%s",
					"current_reliability_shards_on_node=$current_shards",
					"reliability_shards_recent_max=$history_max",
					"max_expire_idle",
					"done | redis-cli --raw",
					"INFO clients",
					"INFO memory",
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
				return "reliability_marker_slot=9508 reliability_marker_owner_port=6382 queried_node_owns_reliability_marker=false\n" +
					"reliability_stats_current_block=29804343 current_reliability_shards_on_node=2 previous_reliability_shards_on_node=0 reliability_shard_count=32 reliability_shard_lookback_blocks=2 reliability_shards_recent_max=2 reliability_shards_recent_max_age_blocks=0\n" +
					"blocked_clients=0\nused_memory_bytes=1412572232\nmem_clients_normal_bytes=3716242\n" +
					"latency_avg_ms=0.279\naccept_recv_q=0 accept_send_q=65535\n" +
					"client_list_total=472 client_output_memory_bytes=0 client_output_memory_max_bytes=0\n" +
					"472 max_idle_s=5 max_age_s=3600 source=192.0.2.181 flags=N cmd=expire lib=go-redis", nil
			}
			return "6380 100 1000 90 1 10\n6381 100 1000 90 1 10\n6394 100 1000 90 1 55", nil
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
		"owned 2 of 32 client-reliability shards for the trip minute and 0 for the immediately preceding minute",
		"each reliability transaction ends in EXPIRE",
		"rotating marker-free reliability load collision",
		"Trip-time Redis controls",
		"latency_avg_ms=0.279",
		"client_memory_bytes=3716242",
		"below their alert bands",
		"Do not roll back the marker-free writer",
		"bounded shard-history collision ages out",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("shard-collision diagnosis missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(alert.Action, "restart writers normally") || strings.Contains(alert.Action, "Roll out the marker-free reliability writer") {
		t.Fatalf("shard collision received legacy or destructive action: %s", alert.Action)
	}
}

func TestRedisConnectionsSignalAttributesPeerDeltaReadAmplification(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "CLIENT LIST") {
				return "reliability_marker_slot=9508 reliability_marker_owner_port=6382 queried_node_owns_reliability_marker=false\n" +
					"reliability_stats_current_block=29808000 current_reliability_shards_on_node=0 previous_reliability_shards_on_node=0 reliability_shard_count=32 reliability_shard_lookback_blocks=2 reliability_shards_recent_max=1 reliability_shards_recent_max_age_blocks=0\n" +
					"blocked_clients=0\nused_memory_bytes=1060000000\nmem_clients_normal_bytes=2800000\n" +
					"latency_avg_ms=0.487\nhget_sample_seconds=2 hget_calls_delta=602 hget_calls_per_second=301.000\n" +
					"accept_recv_q=0 accept_send_q=65535\n" +
					"client_list_total=954 client_cmd_hget_count=669 client_output_memory_bytes=0 client_output_memory_max_bytes=0\n" +
					"375 max_idle_s=95 max_age_s=4132 source=192.0.2.180 flags=N cmd=hget lib=go-redis\n" +
					"294 max_idle_s=268 max_age_s=5096 source=192.0.2.181 flags=N cmd=hget lib=go-redis", nil
			}
			return "6380 100 1000 90 1 281\n6381 100 1000 90 1 280\n6393 100 1000 90 1 954", nil
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
		"301.000 HGET calls/second",
		"network-peer live-delta fanout",
		"one lazy peer delta per subscriber event",
		"Do not kill Redis clients or raise pool floors",
		"HGET command rate and HGET-ended client cohort collapse",
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

func TestRedisConnectionsSignalAttributesExpiredShardOwnership(t *testing.T) {
	evidence := strings.Join([]string{
		"reliability_marker_slot=9508 reliability_marker_owner_port=6382 queried_node_owns_reliability_marker=false",
		"reliability_stats_current_block=29804665 current_reliability_shards_on_node=0 previous_reliability_shards_on_node=0 reliability_shard_count=32 reliability_shard_lookback_blocks=6 reliability_shards_recent_max=5 reliability_shards_recent_max_age_blocks=4 reliability_shards_recent_total=9 reliability_shard_history=29804661:5,29804662:2 max_expire_idle_s=259",
		"blocked_clients=0",
		"used_memory_bytes=1403831800",
		"mem_clients_normal_bytes=3897298",
		"latency_avg_ms=0.319",
		"accept_recv_q=0 accept_send_q=65535",
		"client_list_total=1486 client_output_memory_bytes=0 client_output_memory_max_bytes=0",
		"531 max_idle_s=259 max_age_s=6770 source=192.0.2.181 flags=N cmd=expire lib=go-redis",
	}, "\n")

	diagnosis := diagnoseRedisConnectionSpike(evidence)
	for _, want := range []string{
		"owned 0 of 32 client-reliability shards for the trip minute and 0 for the immediately preceding minute",
		"had owned as many as 5 within the bounded 6-block history",
		"4 block(s) before the trip",
		"pools outlived the one-minute key ownership",
		"rotating marker-free reliability load collision",
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
				return "reliability_marker_slot=9508 reliability_marker_owner_port=6382 queried_node_owns_reliability_marker=false\n" +
					"reliability_stats_current_block=29804673 current_reliability_shards_on_node=0 previous_reliability_shards_on_node=1 reliability_shard_count=32 reliability_shard_lookback_blocks=6 reliability_shards_recent_max=2 reliability_shards_recent_max_age_blocks=4 reliability_shards_recent_total=5 reliability_shard_history=29804668:2,29804669:2,29804672:1 max_expire_idle_s=299\n" +
					"blocked_clients=0\nused_memory_bytes=1408354048\nmem_clients_normal_bytes=5753722\n" +
					"latency_avg_ms=0.297\naccept_recv_q=0 accept_send_q=65535\n" +
					"client_list_total=55 client_output_memory_bytes=0 client_output_memory_max_bytes=0\n" +
					"40 max_idle_s=295 max_age_s=4579 source=192.0.2.181 flags=N cmd=expire lib=go-redis", nil
			}
			snapshotCalls++
			if snapshotCalls == 1 {
				return "6380 100 1000 90 1 10\n6381 100 1000 90 1 10\n6394 100 1000 90 1 55", nil
			}
			return "6380 100 1000 90 1 10\n6381 100 1000 90 1 10\n6394 100 1000 90 1 45", nil
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

func TestRedisConnectionsSignalEscalatesAttributedActivePressure(t *testing.T) {
	evidence := strings.Join([]string{
		"reliability_marker_slot=9508 reliability_marker_owner_port=6382 queried_node_owns_reliability_marker=false",
		"reliability_stats_current_block=29804343 current_reliability_shards_on_node=3 previous_reliability_shards_on_node=2 reliability_shard_count=32",
		"blocked_clients=2",
		"used_memory_bytes=8589934592",
		"mem_clients_normal_bytes=3221225472",
		"latency_avg_ms=12.500",
		"accept_recv_q=7 accept_send_q=7",
		"client_list_total=900 client_output_memory_bytes=67108864 client_output_memory_max_bytes=41943040",
		"700 max_idle_s=5 max_age_s=3600 source=192.0.2.181 flags=N cmd=expire lib=go-redis",
	}, "\n")

	diagnosis := diagnoseRedisConnectionSpike(evidence)
	for _, want := range []string{
		"rotating marker-free reliability load collision",
		"At least one captured",
		"active pressure",
		"compatible workload-distribution repair",
	} {
		combined := diagnosis.mechanism + " " + diagnosis.context + " " + diagnosis.action
		if !strings.Contains(combined, want) {
			t.Fatalf("active-pressure diagnosis missing %q: %+v", want, diagnosis)
		}
	}
}
