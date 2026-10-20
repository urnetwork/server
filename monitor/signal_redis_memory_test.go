package monitor

import (
	"context"
	"strings"
	"testing"
)

// The optional INFO battery needs its native header and numeric counters.
// Human-readable fields cannot replace them or erase the independent warning.
func TestRedisMemorySignalSyntheticHighMemoryNode(t *testing.T) {
	legacyMemory := "used_memory_human:8.60G\nmaxmemory_human:10.00G\nused_memory_dataset:8000000000\nmem_clients_normal:1000000000\nmem_clients_slaves:500000000\nmem_fragmentation_ratio:1.01"
	completeMemory := "# Memory\nused_memory:8600000000\nmaxmemory:10000000000\n" + legacyMemory
	for _, test := range []struct {
		name      string
		memory    string
		available bool
	}{
		{name: "complete numeric memory", memory: completeMemory, available: true},
		{name: "legacy human counters", memory: legacyMemory},
		{name: "missing header", memory: strings.TrimPrefix(completeMemory, "# Memory\n")},
		{name: "missing used bytes", memory: strings.Replace(completeMemory, "used_memory:8600000000\n", "", 1)},
		{name: "missing ceiling bytes", memory: strings.Replace(completeMemory, "maxmemory:10000000000\n", "", 1)},
	} {
		infoCalls := 0
		source := &syntheticSource{
			hostFn: func(_ HostSettings, command string) (string, error) {
				if strings.Contains(command, "for p in") {
					return "6380 8600000000 10000000000 8000000000 1500000000 10", nil
				}
				return "", nil
			},
			redisFn: func(_ HostSettings, port int, args ...string) (string, error) {
				if port != 6380 || len(args) != 2 || args[0] != "INFO" || args[1] != "memory" {
					t.Fatalf("%s: unexpected optional memory source request", test.name)
				}
				infoCalls++
				return test.memory, nil
			},
		}
		alerts, err := NewRedisMemorySignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatalf("%s: optional memory observation became a signal error", test.name)
		}
		alert := requireAlertClass(t, alerts, "node-mem-high")
		if alert.Sustain != 2 {
			t.Fatalf("%s: sustain = %d, want 2 consecutive 5-minute ticks", test.name, alert.Sustain)
		}
		if alert.Target != "redis-1:6380" || alert.SignalID != "redis/node-mem-high" || alert.Severity != SeverityWarn ||
			!strings.Contains(alert.Observed, "dataset=8.00G clients=1.50G") {
			t.Fatalf("%s: optional memory fields changed the independently measured pressure", test.name)
		}
		if test.available {
			want := "node 6380 memory: used_bytes=8600000000 maxmemory_bytes=10000000000 dataset_bytes=8000000000 client_bytes=1500000000 fragmentation_ratio=1.01"
			if !strings.Contains(alert.Evidence, want) || strings.Contains(alert.Evidence, "INFO memory unavailable") {
				t.Fatalf("%s: complete INFO did not preserve exact numeric memory evidence", test.name)
			}
		} else if !strings.Contains(alert.Evidence, "node 6380 INFO memory unavailable: error_class=observation-invalid-response") ||
			strings.Contains(alert.Evidence, "node 6380 memory:") {
			t.Fatalf("%s: incomplete INFO acquired optional memory evidence authority", test.name)
		}
		if infoCalls != 1 {
			t.Fatalf("%s: optional memory battery made %d INFO calls, want 1", test.name, infoCalls)
		}
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

	settings := syntheticSettings(source)
	settings.Hosts[1].RedisNodePorts = []int{6406}
	alerts, err := NewRedisMemorySignal().Run(context.Background(), settings)
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

	settings := syntheticSettings(source)
	settings.Hosts[1].RedisNodePorts = []int{6380, 6381}
	alerts, err := NewRedisMemorySignal().Run(context.Background(), settings)
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

	settings := syntheticSettings(source)
	settings.Hosts[1].RedisNodePorts = []int{6380}
	alerts, err := NewRedisMemorySignal().Run(context.Background(), settings)
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
