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
					"client_reliability_stats.$current_block.$shard",
					"current_reliability_shards_on_node=$current_shards",
				} {
					if !strings.Contains(command, fragment) {
						t.Fatalf("connection battery command missing %q:\n%s", fragment, command)
					}
				}
				if strings.Contains(command, "%!") {
					t.Fatalf("connection battery command contains a formatting failure:\n%s", command)
				}
				return "reliability_marker_slot=9508 reliability_marker_owner_port=6382 queried_node_owns_reliability_marker=false\n" +
					"reliability_stats_current_block=29804343 current_reliability_shards_on_node=2 previous_reliability_shards_on_node=0 reliability_shard_count=32\n" +
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
		"owns 2 of 32 current-minute and 0 previous-minute client-reliability shards",
		"each reliability transaction ends in EXPIRE",
		"rotating marker-free reliability load collision",
		"Do not roll back the marker-free writer",
		"Current/previous shard ownership rotates away",
	} {
		if !strings.Contains(markdown, detail) {
			t.Fatalf("shard-collision diagnosis missing %q:\n%s", detail, markdown)
		}
	}
	if strings.Contains(alert.Action, "restart writers normally") || strings.Contains(alert.Action, "Roll out the marker-free reliability writer") {
		t.Fatalf("shard collision received legacy or destructive action: %s", alert.Action)
	}
}
