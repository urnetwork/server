package monitor

import (
	"context"
	"testing"
)

func TestRedisTopologySignalSyntheticPhantomNode(t *testing.T) {
	source := &syntheticSource{redisFn: func(HostSettings, int, ...string) (string, error) {
		return "deadbeef 127.0.0.1:0@0 master,noaddr - 0 0 0 disconnected", nil
	}}
	alerts, err := NewRedisTopologySignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "phantom-nodes")
}

func TestRedisTopologySignalSyntheticReplicaCoverLoss(t *testing.T) {
	source := &syntheticSource{redisFn: func(HostSettings, int, ...string) (string, error) {
		return "master-id 127.0.0.1:6380@16380 master - 0 0 1 connected", nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts[1].RedisExpectedReplicas = 1
	alerts, err := NewRedisTopologySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "replica-cover")
}
