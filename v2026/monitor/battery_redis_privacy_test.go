// Synthetic optional-source failures exercise the actual alert and latch path.
// No fixture text, renderer output or error body is printed by these controls.
package monitor

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
)

const redisBatteryPrivateCanary = "PRIVATE_CANARY_REDIS_OPTIONAL_DIAGNOSTIC"

// Independent complete fixture, intentionally not generated from production
// schema declarations so missing or renamed source fields fail the controls.
func redisConnectionBatteryFixture(overrides map[string]string) string {
	values := map[string]string{
		"redis_connection_diagnostics_version": "1", "queried_node_port": "6382",
		"reliability_marker_slot": "123", "reliability_marker_owner_port": "6381",
		"reliability_stats_current_block": "100", "current_reliability_shards_on_node": "0",
		"previous_reliability_shards_on_node": "0", "reliability_shard_count": "32",
		"reliability_shard_lookback_blocks": "2", "reliability_shards_recent_max": "0",
		"reliability_shards_recent_max_age_blocks": "0", "reliability_shards_recent_total": "0",
		"blocked_clients": "0", "used_memory_bytes": "1000000", "mem_clients_normal_bytes": "1000",
		"hget_nominal_sample_seconds": "2", "hget_calls_delta": "0", "hget_calls_per_nominal_second": "0",
		"accept_recv_q": "0", "accept_send_q": "128", "latency_avg_ms": "0.4",
		"client_list_total": "100", "client_cmd_hget_count": "0", "client_cmd_sadd_count": "0",
		"client_cmd_expire_count": "0", "client_output_memory_bytes": "0", "client_output_memory_max_bytes": "0",
		"client_output_over_1mb_count": "0", "client_max_idle_s": "0", "client_max_age_s": "60", "max_expire_idle_s": "0",
	}
	for key, value := range overrides {
		if value == "" {
			delete(values, key)
		} else {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]string, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, key+"="+values[key])
	}
	return strings.Join(fields, "\n")
}

// Compare every public path without logging forbidden data on a failure.
func redisBatteryAssertPublicPrivate(t *testing.T, alerts []Alert) {
	t.Helper()
	var jsonl bytes.Buffer
	if err := WriteAlertsJSONL(&jsonl, alerts); err != nil {
		t.Fatal("synthetic public JSON rendering failed")
	}
	for _, public := range []string{jsonl.String(), AlertsMarkdown(alerts)} {
		if strings.Contains(public, redisBatteryPrivateCanary) {
			t.Fatal("optional source text entered public output")
		}
	}
	for _, alert := range alerts {
		if strings.Contains(alert.Evidence, redisBatteryPrivateCanary) || strings.Contains(alert.Markdown(), redisBatteryPrivateCanary) {
			t.Fatal("optional source text entered an alert or its cached evidence")
		}
	}
}

// Deliberately avoid the general helper, whose failure prints whole Alerts.
func redisBatteryPressureAlert(t *testing.T, alerts []Alert, class string) Alert {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class {
			return alert
		}
	}
	t.Fatal("optional diagnostic erased the independently measured pressure")
	return Alert{}
}

// A true memory violation survives each independent optional failure; repeated
// reports reuse only the already-projected trip evidence.
func TestRedisOptionalNodeBatteryPrivacyRetainsPressure(t *testing.T) {
	info := "# Memory\nused_memory:93\nmaxmemory:100\nused_memory_dataset:90\nmem_clients_normal:0\nmem_clients_slaves:0\nmem_fragmentation_ratio:1.1"
	clients := "client_list_total=1 client_cmd_hget_count=0 client_cmd_sadd_count=0 client_cmd_expire_count=0 client_output_memory_bytes=0 client_output_memory_max_bytes=0 client_output_over_1mb_count=0 client_max_idle_s=0 client_max_age_s=1 max_expire_idle_s=0"
	for _, fixture := range []struct {
		info      string
		infoErr   error
		clients   string
		clientErr error
		queue     string
		queueErr  error
	}{
		{info: info, clients: clients, queue: "accept_recv_q=0 accept_send_q=128"},
		{info: info + "\nused_memory_human:" + redisBatteryPrivateCanary + "\nmaxmemory_human:" + redisBatteryPrivateCanary + "\nfuture_field:" + redisBatteryPrivateCanary, clients: clients, queue: "accept_recv_q=0 accept_send_q=128"},
		{infoErr: errors.New(redisBatteryPrivateCanary), clients: clients, queue: "accept_recv_q=0 accept_send_q=128"},
		{info: strings.Replace(info, "used_memory:93", "used_memory:"+redisBatteryPrivateCanary, 1), clients: clients, queue: "accept_recv_q=0 accept_send_q=128"},
		{info: info, clients: "name=" + redisBatteryPrivateCanary, queue: "accept_recv_q=0 accept_send_q=128"},
		{info: info, clientErr: errors.New(redisBatteryPrivateCanary), queue: "accept_recv_q=0 accept_send_q=128"},
		{info: info, clients: clients, queue: "accept_recv_q=" + redisBatteryPrivateCanary},
		{info: info, clients: clients, queueErr: errors.New(redisBatteryPrivateCanary)},
	} {
		coreCalls, infoCalls, clientCalls, queueCalls := 0, 0, 0, 0
		source := &syntheticSource{
			redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
				if len(args) != 2 || args[0] != "INFO" || args[1] != "memory" {
					t.Fatal("unexpected synthetic Redis operation")
				}
				infoCalls++
				return fixture.info, fixture.infoErr
			},
			hostFn: func(_ HostSettings, command string) (string, error) {
				switch {
				case strings.Contains(command, "for p in"):
					coreCalls++
					return "host_memory 100000000000 90000000000\n6380 10 100 9 0 10\n6381 10 100 9 0 10\n6382 93 100 90 0 10", nil
				case strings.Contains(command, "CLIENT LIST"):
					clientCalls++
					return fixture.clients, fixture.clientErr
				case strings.Contains(command, "ss -lnt"):
					queueCalls++
					return fixture.queue, fixture.queueErr
				default:
					t.Fatal("unexpected synthetic host operation")
					return "", nil
				}
			},
		}
		signal := NewRedisMemorySignal()
		for tick := 0; tick < 2; tick++ {
			alerts, err := signal.Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal("optional diagnostic became a whole-probe failure")
			}
			redisBatteryAssertPublicPrivate(t, alerts)
			alert := redisBatteryPressureAlert(t, alerts, "node-mem-critical")
			if alert.SignalID != "redis/node-mem-critical" || alert.Severity != SeverityPage || alert.Sustain != 1 ||
				!strings.Contains(alert.Symptom, "93%") || (tick > 0) != strings.Contains(alert.Evidence, "battery collected once at trip") {
				t.Fatal("optional source changed pressure severity, threshold or trip provenance")
			}
			if fixture.infoErr != nil && !strings.Contains(alert.Evidence, "INFO memory unavailable: error_class=") ||
				fixture.clientErr != nil && !strings.Contains(alert.Evidence, "client buffers unavailable: error_class=") ||
				fixture.queueErr != nil && !strings.Contains(alert.Evidence, "accept queue unavailable: error_class=") {
				t.Fatal("failed optional source became a healthy or unclassified diagnostic")
			}
		}
		if coreCalls != 2 || infoCalls != 1 || clientCalls != 1 || queueCalls != 1 {
			t.Fatal("optional privacy correction changed collection or cache bounds")
		}
	}
}

// A healthy native census never runs the optional sources at all.
func TestRedisOptionalNodeBatteryHealthyControlSkipsDiagnostics(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if !strings.Contains(command, "for p in") {
				t.Fatal("healthy memory census ran an optional battery")
			}
			return "host_memory 100000000000 90000000000\n6380 10 100 9 0 10\n6381 10 100 9 0 10\n6382 10 100 9 0 10", nil
		},
		redisFn: func(HostSettings, int, ...string) (string, error) {
			t.Fatal("healthy memory census ran optional INFO")
			return "", nil
		},
	}
	alerts, err := NewRedisMemorySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatal("healthy control produced a false memory alert")
	}
}

// CLIENT user/library text cannot create public fields or causal keys, and a
// shell error cannot remove the independently observed client-count spike.
func TestRedisOptionalConnectionBatteryPrivacyRetainsSpike(t *testing.T) {
	for _, fixture := range []struct {
		output string
		err    error
	}{
		{output: redisConnectionBatteryFixture(nil)},
		{err: errors.New(redisBatteryPrivateCanary)},
		{output: "100 max_idle_s=0 source=" + redisBatteryPrivateCanary + " cmd=hget lib=" + redisBatteryPrivateCanary},
		{output: redisConnectionBatteryFixture(nil) + "\nlib=" + redisBatteryPrivateCanary + " client_cmd_hget_count=999 hget_calls_per_nominal_second=999"},
		{output: redisConnectionBatteryFixture(map[string]string{"latency_avg_ms": redisBatteryPrivateCanary})},
		{output: redisConnectionBatteryFixture(map[string]string{"latency_avg_ms": "NaN"})},
		{output: redisConnectionBatteryFixture(map[string]string{"blocked_clients": "-1"})},
		{output: redisConnectionBatteryFixture(map[string]string{"mem_clients_normal_bytes": ""})},
	} {
		coreCalls, batteryCalls := 0, 0
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "CLIENT LIST") {
				batteryCalls++
				return fixture.output, fixture.err
			}
			if !strings.Contains(command, "for p in") {
				t.Fatal("unexpected synthetic connection source operation")
			}
			coreCalls++
			return "6380 10 100 9 0 10\n6381 10 100 9 0 10\n6382 10 100 9 0 55", nil
		}}
		signal := NewRedisConnectionsSignal()
		for tick := 0; tick < 2; tick++ {
			alerts, err := signal.Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal("optional connection failure became a whole-probe failure")
			}
			redisBatteryAssertPublicPrivate(t, alerts)
			alert := redisBatteryPressureAlert(t, alerts, "clients-spike")
			if alert.SignalID != "redis/clients-spike" || alert.Severity != SeverityWarn || alert.Sustain != 2 ||
				!strings.Contains(alert.Observed, "connected=55") || (tick > 0) != strings.Contains(alert.Evidence, "battery collected once at trip") {
				t.Fatal("optional connection data changed the count violation or cache provenance")
			}
			if strings.Contains(alert.Mechanism, "live-delta read amplification") || strings.Contains(alert.Action, "Roll out the marker-free reliability writer") {
				t.Fatal("optional malformed or generic data acquired a workload attribution")
			}
		}
		if coreCalls != 2 || batteryCalls != 1 {
			t.Fatal("connection privacy correction changed the trip-cache collection policy")
		}
	}
}

// Malformed, ambiguous, missing, nonfinite and inconsistent values stay unknown.
// A complete valid fixture still supports healthy controls without clearing the
// independent connection shape, and unknown INFO human fields are withheld.
func TestRedisOptionalConnectionBatterySchemaAuthority(t *testing.T) {
	healthy := redisConnectionBatteryFixture(nil)
	diagnostic, valid := parseRedisConnectionDiagnostics(healthy)
	if !valid {
		t.Fatal("complete numeric diagnostic was rejected")
	}
	summary, pressure := redisConnectionControlSummary(diagnostic.evidence())
	if pressure || !strings.Contains(summary, "below their alert bands") {
		t.Fatal("complete healthy pressure control lost its authority")
	}
	for _, malformed := range []string{
		healthy + "\nblocked_clients=0",
		healthy + "\nunknown=" + redisBatteryPrivateCanary,
		redisConnectionBatteryFixture(map[string]string{"blocked_clients": ""}),
		redisConnectionBatteryFixture(map[string]string{"latency_avg_ms": "Inf"}),
		redisConnectionBatteryFixture(map[string]string{"latency_avg_ms": "-1"}),
		redisConnectionBatteryFixture(map[string]string{"client_list_total": "0"}),
		redisConnectionBatteryFixture(map[string]string{"client_cmd_hget_count": "101"}),
		redisConnectionBatteryFixture(map[string]string{"client_output_memory_max_bytes": "1"}),
		redisConnectionBatteryFixture(map[string]string{"hget_calls_per_nominal_second": "1"}),
		redisConnectionBatteryFixture(map[string]string{"reliability_shards_recent_max": "33"}),
		redisConnectionBatteryFixture(map[string]string{"reliability_shard_lookback_blocks": "63"}),
		redisConnectionBatteryFixture(map[string]string{"used_memory_bytes": "9223372036854775808"}),
		redisConnectionBatteryFixture(map[string]string{"redis_connection_diagnostics_version": "2"}),
	} {
		if _, valid := parseRedisConnectionDiagnostics(malformed); valid {
			t.Fatal("malformed optional diagnostics acquired authority")
		}
		summary, pressure := redisConnectionControlSummary(malformed)
		if pressure || !strings.Contains(summary, "unavailable") {
			t.Fatal("malformed diagnostics established pressure or healthy controls")
		}
	}
}

// Byte units, uniqueness and all required INFO values are checked before
// formatting. Unknown human-readable fields carry no diagnostic authority.
func TestRedisOptionalNodeMemorySchemaAuthority(t *testing.T) {
	healthy := "# Memory\nused_memory:1000\nmaxmemory:2000\nused_memory_dataset:900\nmem_clients_normal:20\nmem_clients_slaves:30\nmem_fragmentation_ratio:1.25"
	evidence, valid := redisNodeMemoryEvidence(healthy+"\nused_memory_human:"+redisBatteryPrivateCanary, 6382)
	if !valid || !strings.Contains(evidence, "used_bytes=1000") || !strings.Contains(evidence, "client_bytes=50") || strings.Contains(evidence, redisBatteryPrivateCanary) {
		t.Fatal("complete numeric INFO failed its safe projection")
	}
	for _, malformed := range []string{
		strings.TrimPrefix(healthy, "# Memory\n"),
		healthy + "\nused_memory:1",
		strings.Replace(healthy, "mem_clients_slaves:30\n", "", 1),
		strings.Replace(healthy, "used_memory:1000", "used_memory:-1", 1),
		strings.Replace(healthy, "used_memory:1000", "used_memory:1.5", 1),
		strings.Replace(healthy, "used_memory:1000", "used_memory:"+redisBatteryPrivateCanary, 1),
		strings.Replace(healthy, "mem_fragmentation_ratio:1.25", "mem_fragmentation_ratio:NaN", 1),
		strings.Replace(healthy, "mem_fragmentation_ratio:1.25", "mem_fragmentation_ratio:Inf", 1),
		strings.Replace(healthy, "mem_fragmentation_ratio:1.25", "mem_fragmentation_ratio:-1", 1),
		strings.Replace(strings.Replace(healthy, "mem_clients_normal:20", "mem_clients_normal:9223372036854775807", 1), "mem_clients_slaves:30", "mem_clients_slaves:1", 1),
	} {
		if evidence, valid := redisNodeMemoryEvidence(malformed, 6382); valid || evidence != "" {
			t.Fatal("malformed optional INFO became a numeric memory observation")
		}
	}
}

// These complete numeric observations are also produced by unrelated HGET,
// SADD and EXPIRE workloads. No fixture supplies caller/key/artifact evidence;
// the public alert must keep the count violation without inventing that join.
func TestRedisOptionalConnectionBatteryCandidatesRequireIndependentAuthority(t *testing.T) {
	var sharedAction string
	for _, fixture := range []struct {
		values    map[string]string
		candidate string
	}{
		{values: map[string]string{"client_cmd_hget_count": "80", "hget_calls_delta": "602", "hget_calls_per_nominal_second": "301"}, candidate: "Network-peer live-delta fanout is one candidate workload"},
		{values: map[string]string{"client_cmd_hget_count": "80"}, candidate: "Network-peer live-delta fanout is one candidate workload"},
		{values: map[string]string{"client_cmd_sadd_count": "1", "reliability_marker_owner_port": "6382"}, candidate: "candidate legacy fixed-slot writer fingerprint"},
		{values: map[string]string{"client_cmd_expire_count": "1", "current_reliability_shards_on_node": "2", "reliability_shards_recent_max": "2", "reliability_shards_recent_total": "2"}, candidate: "candidate rotating reliability-workload fingerprint"},
	} {
		evidence := redisConnectionBatteryFixture(fixture.values)
		if _, valid := parseRedisConnectionDiagnostics(evidence); !valid {
			t.Fatal("candidate counterexample lacks complete numeric source authority")
		}
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "CLIENT LIST") {
				return evidence, nil
			}
			return "6380 10 100 9 0 10\n6381 10 100 9 0 10\n6382 10 100 9 0 55", nil
		}}
		alerts, err := NewRedisConnectionsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal("candidate counterexample became a whole-probe failure")
		}
		redisBatteryAssertPublicPrivate(t, alerts)
		alert := redisBatteryPressureAlert(t, alerts, "clients-spike")
		if !strings.Contains(alert.Observed, "connected=55") || alert.Severity != SeverityWarn || alert.Sustain != 2 ||
			!strings.Contains(alert.Mechanism, fixture.candidate) ||
			!strings.Contains(alert.Action, "Independently confirm the caller, accessed key and running artifact before any workload-specific rollout or restart") ||
			!strings.Contains(alert.Context, "below their alert bands") ||
			!strings.Contains(alert.Context, "neither a measured command rate nor workload or pressure authority") {
			t.Fatal("valid candidate fingerprint acquired root-cause, action or measured-rate authority")
		}
		if sharedAction == "" {
			sharedAction = alert.Action
		} else if alert.Action != sharedAction {
			t.Fatal("candidate shape or nominal HGET arithmetic selected a workload-specific action")
		}
		var jsonl bytes.Buffer
		if err := WriteAlertsJSONL(&jsonl, alerts); err != nil {
			t.Fatal("candidate counterexample JSON rendering failed")
		}
		for _, public := range []string{jsonl.String(), AlertsMarkdown(alerts)} {
			for _, unproved := range []string{
				"attributes the connection shape to network-peer", "The SADD cohort identifies the legacy",
				"identify a rotating marker-free reliability load collision", "Deploy the Connect version",
				"Roll out the marker-free reliability writer", "restart writers normally",
				"execute the compatible workload-distribution repair", "hget_sample_seconds=", "hget_calls_per_second=",
			} {
				if strings.Contains(public, unproved) {
					t.Fatal("public candidate evidence retained an unsupported diagnosis or rate claim")
				}
			}
		}
	}
}
