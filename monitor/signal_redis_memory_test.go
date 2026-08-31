package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestRedisMemorySignalSyntheticHighMemoryNode(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "for p in") {
				return "6380 8600000000 10000000000 8000000000 1500000000 10", nil
			}
			return "", nil
		},
		redisFn: func(HostSettings, int, ...string) (string, error) {
			return "used_memory_human:8.60G\nmaxmemory_human:10.00G\nused_memory_dataset:8000000000\nmem_clients_normal:1000000000\nmem_clients_slaves:500000000\nmem_fragmentation_ratio:1.01", nil
		},
	}
	alerts, err := NewRedisMemorySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "node-mem-high")
	if alert.Sustain != 2 {
		t.Fatalf("sustain = %d, want 2 consecutive 5-minute ticks", alert.Sustain)
	}
	if !strings.Contains(alert.Evidence, "dataset=8.00G clients=1.50G") {
		t.Fatalf("evidence did not attribute current Redis client fields: %q", alert.Evidence)
	}
}

func TestRedisMemorySignalSyntheticCriticalAndSkewedNodes(t *testing.T) {
	tests := []struct {
		name  string
		rows  string
		class string
	}{
		{name: "critical", rows: "6380 93 100 80 1 10", class: "node-mem-critical"},
		{name: "skew", rows: "6380 10 1000 9 1 10\n6381 10 1000 9 1 10\n6382 40 1000 39 1 10", class: "mem-skew"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				return test.rows, nil
			}}
			alerts, err := NewRedisMemorySignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			requireAlertClass(t, alerts, test.class)
		})
	}
}

func TestRedisMemorySignalSeparatesImpossibleTTLFromCapacityRoot(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "for p in") {
			return "6406 12884738624 12884901888 12493731078 0 1504 volatile-ttl 5793319 4012927 863380622381372 2016434 0 1043857 0", nil
		}
		return "", nil
	}}

	alerts, err := NewRedisMemorySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "node-mem-critical")
	markdown := alert.Markdown()
	for _, want := range []string{
		"policy=volatile-ttl",
		"keys=5793319",
		"expiring_keys=4012927",
		"avg_ttl_days=",
		"does not measure the residue's bytes",
		"redis-bytes for memory ownership",
		"explicit maintenance authority",
		"MEMORY USAGE evidence",
		"returns average TTL below two years",
		"SIGNALS.md §3.3a, §3.3b, and §5.4",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("critical impossible-TTL diagnosis missing %q: %s", want, markdown)
		}
	}
}

func TestRedisMemorySignalDetectsAggregateHostCapacityDeficit(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "for p in") {
			return strings.Join([]string{
				"host_memory 17179869184 536870912",
				"6380 9500000000 10000000000 9200000000 0 100 volatile-ttl 5000000 4000000 800000000000000 1000 0 10 0 9800000000",
				"6381 9500000000 10000000000 9200000000 0 100 volatile-ttl 5000000 4000000 800000000000000 1000 0 10 0 9800000000",
			}, "\n"), nil
		}
		return "", nil
	}}

	alerts, err := NewRedisMemorySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "redis-host-capacity")
	markdown := alert.Markdown()
	for _, want := range []string{
		"host_available_gib=0.5",
		"remaining_configured_headroom_gib=0.9",
		"operational_reserve_gib=8.0",
		"required_available_gib=8.9",
		"capacity_deficit_gib=8.4",
		"critical_nodes=2",
		"Do not increase maxmemory",
		"measure its bytes before counting it as capacity relief",
		"do not assume that clamping a small residue resolves this deficit",
		"unused swap is not healthy Redis capacity",
		"SIGNALS.md §3.1, §3.3a, §3.3b, and §5.4",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("aggregate host-capacity diagnosis missing %q: %s", want, markdown)
		}
	}
}

func TestRedisMemorySignalKeepsHostCapacityAlertAtNodeCeilingWithoutReserve(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "for p in") {
			return strings.Join([]string{
				"host_memory 549755813888 4294967296",
				"6380 12884901888 12884901888 12400000000 0 100 volatile-ttl 5000000 4000000 0 1000 0 10 0 13200000000",
			}, "\n"), nil
		}
		return "", nil
	}}

	alerts, err := NewRedisMemorySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "redis-host-capacity")
	markdown := alert.Markdown()
	for _, want := range []string{
		"remaining_configured_headroom_gib=0.0",
		"operational_reserve_gib=10.2",
		"required_available_gib=10.2",
		"capacity_deficit_gib=6.2",
		"at the ceilings",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("ceiling-state host-capacity diagnosis missing %q: %s", want, markdown)
		}
	}
}
