// Successful optional node output is untrusted context, not raw evidence.
package monitor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// redisOptionalNodesCalls records only the synthetic optional-read and PING calls.
type redisOptionalNodesCalls struct {
	nodes int
	host  int
}

// One visibly synthetic entry host and a complete one-port PING source keep
// the independent INFO fault and the public adapter path enabled.
func redisOptionalNodesSettings(t *testing.T, info, nodes string, sourceErr error, afterNodes func()) (SignalSettings, *redisOptionalNodesCalls) {
	t.Helper()
	calls := &redisOptionalNodesCalls{}
	source := &syntheticSource{
		redisFn: func(h HostSettings, port int, args ...string) (string, error) {
			if h.Name != "redis-privacy.example" || port != 6379 {
				t.Fatal("optional context fixture did not reach the selected entry")
			}
			switch strings.Join(args, " ") {
			case "CLUSTER INFO":
				return info, nil
			case "CLUSTER NODES":
				calls.nodes++
				if afterNodes != nil {
					afterNodes()
				}
				return nodes, sourceErr
			default:
				t.Fatal("unexpected synthetic Redis command")
				return "", errors.New("unexpected synthetic Redis command")
			}
		},
		hostFn: func(h HostSettings, command string) (string, error) {
			if h.Name != "redis-privacy.example" || !strings.Contains(command, "for p in") {
				t.Fatal("optional context fixture did not reach the selected PING source")
			}
			calls.host++
			return "redis-ping-v1 begin 1\nredis-ping-v1 6380 pong\nredis-ping-v1 end 1\n", nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "redis-privacy.example", Roles: []string{"redis-cluster"}, RedisEntryPort: 6379, RedisNodePorts: []int{6380}}}
	if settings.Validate() != nil {
		t.Fatal("optional context fixture settings are invalid")
	}
	return settings, calls
}

// redisOptionalNodesFault checks that ancillary context preserves the known PAGE.
func redisOptionalNodesFault(t *testing.T, alerts Alerts) Alert {
	t.Helper()
	if len(alerts) != 1 || alerts[0].Class != "cluster-state" || alerts[0].Severity != SeverityPage || alerts[0].Sustain != 1 {
		t.Fatal("optional node context changed the independent immediate cluster fault")
	}
	return alerts[0]
}

// Exercise both public renderers without ever placing raw evidence in a test
// failure message. A red privacy assertion is safe to retain privately.
func redisOptionalNodesPrivate(t *testing.T, alerts Alerts, forbidden ...string) {
	t.Helper()
	var encoded bytes.Buffer
	if WriteAlertsJSONL(&encoded, alerts) != nil {
		t.Fatal("synthetic public JSON rendering failed")
	}
	markdown := alerts.Markdown()
	for _, value := range forbidden {
		if strings.Contains(encoded.String(), value) || strings.Contains(markdown, value) {
			t.Fatal("successful optional node output escaped into public alert evidence")
		}
	}
}

// Common old/new API: this reaches the owning evidence privacy assertion on
// old source, after proving valid settings, independent INFO PAGE and one read.
func TestRedisClusterOptionalNodesMalformedSuccessPrivacy(t *testing.T) {
	const canary = "SYNTHETIC_PRIVATE_NODES_TOKEN_07"
	settings, calls := redisOptionalNodesSettings(t, "cluster_state:fail\ncluster_slots_fail:2\ncluster_known_nodes:1\n", "synthetic-id "+canary+" unrelated-failure-text", nil, nil)
	alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("public optional context fixture did not complete")
	}
	alert := redisOptionalNodesFault(t, alerts)
	if calls.nodes != 1 || calls.host != 1 {
		t.Fatal("optional context path was not exercised exactly once")
	}
	redisOptionalNodesPrivate(t, alerts, canary, "unrelated-failure-text")
	if alert.Evidence != "cluster nodes optional context unknown: invalid response" {
		t.Fatal("malformed optional context did not remain unknown")
	}
}

// TestRedisClusterOptionalNodesValidFlagsPublishOnlyCounts keeps raw node details private.
func TestRedisClusterOptionalNodesValidFlagsPublishOnlyCounts(t *testing.T) {
	nodes := "synthetic-a 192.0.2.10:6380@16380 myself,master,fail - 0 0 1 disconnected 0-16383\nsynthetic-b :0@0 slave,fail?,noaddr synthetic-a 0 0 1 disconnected\n"
	settings, calls := redisOptionalNodesSettings(t, "cluster_state:fail\ncluster_slots_fail:2\ncluster_known_nodes:2\n", nodes, nil, nil)
	alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("valid optional flag fixture did not complete")
	}
	alert := redisOptionalNodesFault(t, alerts)
	if calls.nodes != 1 || alert.Evidence != "cluster nodes optional flags: rows=2 fail=1 pfail=1 noaddr=1" {
		t.Fatal("valid exact flags lost their finite numeric context")
	}
	redisOptionalNodesPrivate(t, alerts, "synthetic-a", "synthetic-b", "192.0.2.10", "6380@16380", ":0@0")
}

// TestRedisClusterOptionalNodesWordsOutsideFlagsDoNotCount rejects substring authority.
func TestRedisClusterOptionalNodesWordsOutsideFlagsDoNotCount(t *testing.T) {
	nodes := "synthetic-fail-noaddr 192.0.2.11:6380@16380,fail-noaddr.example master - 0 0 1 connected 0-16383\n"
	settings, _ := redisOptionalNodesSettings(t, "cluster_state:fail\ncluster_slots_fail:0\ncluster_known_nodes:1\n", nodes, nil, nil)
	alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("flag-column fixture did not complete")
	}
	alert := redisOptionalNodesFault(t, alerts)
	if alert.Evidence != "cluster nodes optional flags: rows=1 fail=0 pfail=0 noaddr=0" {
		t.Fatal("words outside the flags column acquired flag authority")
	}
	redisOptionalNodesPrivate(t, alerts, "192.0.2.11", "fail-noaddr.example", "synthetic-fail-noaddr")
}

// TestRedisClusterOptionalNodesPartialAndInvalidContextStayUnknown withholds partial counts.
func TestRedisClusterOptionalNodesPartialAndInvalidContextStayUnknown(t *testing.T) {
	const row = "synthetic-a 192.0.2.12:6380@16380 master,fail - 0 0 1 disconnected\n"
	for _, nodes := range []string{"", " \n", "synthetic-id SYNTHETIC_PRIVATE_BADROW fail", row + "broken SYNTHETIC_PRIVATE_BADROW fail\n", strings.Replace(row, "master,fail", "master,notfail", 1), strings.Replace(row, "master,fail", "master,fail,fail", 1), strings.Repeat("x", 64*1024+1)} {
		settings, calls := redisOptionalNodesSettings(t, "cluster_state:fail\ncluster_slots_fail:2\ncluster_known_nodes:1\n", nodes, nil, nil)
		alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal("invalid optional context fixture did not complete")
		}
		alert := redisOptionalNodesFault(t, alerts)
		if calls.nodes != 1 || alert.Evidence != "cluster nodes optional context unknown: invalid response" {
			t.Fatal("partial or invalid optional context acquired complete flag counts")
		}
		redisOptionalNodesPrivate(t, alerts, "SYNTHETIC_PRIVATE_BADROW", "192.0.2.12", "synthetic-a")
	}
}

// TestRedisClusterOptionalNodesSourceErrorPreservesFaultAndPrivacy retains the fixed error class.
func TestRedisClusterOptionalNodesSourceErrorPreservesFaultAndPrivacy(t *testing.T) {
	settings, _ := redisOptionalNodesSettings(t, "cluster_state:fail\ncluster_slots_fail:2\ncluster_known_nodes:1\n", "", errors.New("permission denied SYNTHETIC_PRIVATE_NODES_ERROR"), nil)
	alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("optional error discarded the independent fault")
	}
	alert := redisOptionalNodesFault(t, alerts)
	if !strings.HasPrefix(alert.Evidence, "cluster nodes failed: error_class=") {
		t.Fatal("optional error lost its fixed error classification")
	}
	redisOptionalNodesPrivate(t, alerts, "SYNTHETIC_PRIVATE_NODES_ERROR")
}

// TestRedisClusterOptionalNodesHealthyNeverRequestsContext preserves the healthy no-read path.
func TestRedisClusterOptionalNodesHealthyNeverRequestsContext(t *testing.T) {
	settings, calls := redisOptionalNodesSettings(t, "cluster_state:ok\ncluster_slots_fail:0\ncluster_known_nodes:1\n", "synthetic-id SYNTHETIC_PRIVATE_UNUSED fail", nil, nil)
	alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 || calls.nodes != 0 || calls.host != 1 {
		t.Fatal("healthy independent source changed or requested unnecessary optional context")
	}
}

// TestRedisClusterOptionalNodesCancellationDiscardsPartialFault preserves owner cancellation.
func TestRedisClusterOptionalNodesCancellationDiscardsPartialFault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings, calls := redisOptionalNodesSettings(t, "cluster_state:fail\ncluster_slots_fail:2\ncluster_known_nodes:1\n", "synthetic-id SYNTHETIC_PRIVATE_CANCELED fail", nil, cancel)
	alerts, err := NewRedisClusterSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls.nodes != 1 || calls.host != 0 {
		t.Fatal("owner cancellation published the previously observed fault or continued the sweep")
	}
}

// TestRedisClusterOptionalNodesSlotsFaultSurvivesUnknownContext keeps failed-slot authority.
func TestRedisClusterOptionalNodesSlotsFaultSurvivesUnknownContext(t *testing.T) {
	settings, _ := redisOptionalNodesSettings(t, "cluster_state:ok\ncluster_slots_fail:1\ncluster_known_nodes:1\n", "broken SYNTHETIC_PRIVATE_SLOT_CONTEXT fail", nil, nil)
	alerts, err := NewRedisClusterSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("optional context discarded a known failed-slot fault")
	}
	alert := redisOptionalNodesFault(t, alerts)
	if !strings.Contains(alert.Observed, "slots_fail=1") || alert.Evidence != "cluster nodes optional context unknown: invalid response" {
		t.Fatal("independent failed-slot authority changed")
	}
	redisOptionalNodesPrivate(t, alerts, "SYNTHETIC_PRIVATE_SLOT_CONTEXT")
}
