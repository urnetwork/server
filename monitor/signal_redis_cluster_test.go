package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestRedisClusterSignalSyntheticFailedCluster(t *testing.T) {
	source := &syntheticSource{redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
		if strings.Join(args, " ") == "CLUSTER INFO" {
			return "cluster_state:fail\ncluster_slots_fail:42\ncluster_known_nodes:32", nil
		}
		return "node-id 127.0.0.1:6380@16380 master,fail", nil
	}}
	alerts, err := NewRedisClusterSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cluster-state")
}

func TestRedisClusterSignalSyntheticWedgedNode(t *testing.T) {
	source := &syntheticSource{
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if strings.Join(args, " ") == "CLUSTER INFO" {
				return "cluster_state:ok\ncluster_slots_fail:0\ncluster_known_nodes:32", nil
			}
			return "", nil
		},
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "for p in") {
				return "6381", nil
			}
			return "", nil
		},
	}
	alerts, err := NewRedisClusterSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "node-unreachable")
}
