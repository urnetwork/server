package monitor

import (
	"context"
	"slices"
	"strings"
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

// The node inventory in monitor.yml is checked against the cluster's own
// membership: layouts that settings validation accepts must still alert when
// they leave members unobserved or attribute a node to the wrong host.
func TestRedisTopologySignalChecksNodeInventoryAgainstClusterNodes(t *testing.T) {
	withoutRedisBlock := func(h HostSettings) HostSettings {
		h.RedisNodePorts = nil
		return h
	}
	withPorts := func(h HostSettings, lo, hi int) HostSettings {
		h.RedisNodePorts = redisTestPortRange(lo, hi)
		return h
	}
	withoutRedisRole := func(h HostSettings) HostSettings {
		h.Roles = []string{"minio", "vpn-client"}
		h.RedisNodePorts = nil
		return h
	}
	withoutLAN := func(h HostSettings) HostSettings {
		h.LANAddress = ""
		return h
	}
	members := redisTestTwoHostMembers()
	without := func(member string) []string {
		return slices.DeleteFunc(slices.Clone(members), func(other string) bool { return other == member })
	}
	type wantAlert struct {
		class, target string
		symptom       []string
	}
	tests := []struct {
		name  string
		hosts []HostSettings
		reply string
		want  []wantAlert
	}{
		{
			name:  "inventory matches the cluster",
			hosts: []HostSettings{redisTestEdge5Host(), redisTestEdge6Host()},
			reply: redisTestClusterNodes(without(redisTestEdge6LAN+":6400"),
				// a failed member is still a member; a handshake is not yet one
				"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee 192.168.51.193:6400@16400 master,fail - 0 0 99 disconnected 5000",
				"ffffffffffffffffffffffffffffffffffffffff 192.168.51.193:6440@16440 master,handshake - 0 0 0 disconnected"),
		},
		{
			name:  "edge-5 redis block removed",
			hosts: []HostSettings{withoutRedisBlock(redisTestEdge5Host()), redisTestEdge6Host()},
			reply: redisTestClusterNodes(members),
			want: []wantAlert{{
				class: "node-inventory-mismatch", target: redisTestEdge5,
				symptom: []string{"cluster members on port(s) 6412-6427 announce its LAN address 192.168.51.179 but are not in its redis.node_ports"},
			}},
		},
		{
			name:  "edge-5 without the redis-cluster role",
			hosts: []HostSettings{withoutRedisRole(redisTestEdge5Host()), redisTestEdge6Host()},
			reply: redisTestClusterNodes(members),
			want: []wantAlert{{
				class: "node-inventory-mismatch", target: redisTestEdge5,
				symptom: []string{"port(s) 6412-6427 announce 192.168.51.179, the LAN address of " + redisTestEdge5, "redis-cluster role"},
			}},
		},
		{
			// validates once edge-5's block is gone; edge-6's node-port bridge
			// would answer on 6412-6427 with edge-5's nodes
			name:  "edge-6 range covers edge-5's bridged ports",
			hosts: []HostSettings{withoutRedisBlock(redisTestEdge5Host()), withPorts(redisTestEdge6Host(), 6380, 6427)},
			reply: redisTestClusterNodes(members),
			want: []wantAlert{
				{
					class: "node-inventory-mismatch", target: redisTestEdge6,
					symptom: []string{"its redis.node_ports 6412-6427 are not cluster members at its LAN address 192.168.51.193", "the cluster announces 6412-6427 at 192.168.51.179 (" + redisTestEdge5 + ")"},
				},
				{
					class: "node-inventory-mismatch", target: redisTestEdge5,
					symptom: []string{"port(s) 6412-6427"},
				},
			},
		},
		{
			name:  "member on an unconfigured port",
			hosts: []HostSettings{redisTestEdge5Host(), redisTestEdge6Host()},
			reply: redisTestClusterNodes(append(slices.Clone(members), redisTestEdge6LAN+":6428")),
			want: []wantAlert{{
				class: "node-inventory-mismatch", target: redisTestEdge6,
				symptom: []string{"cluster members on port(s) 6428 announce its LAN address 192.168.51.193 but are not in its redis.node_ports"},
			}},
		},
		{
			name:  "configured port that is no member",
			hosts: []HostSettings{redisTestEdge5Host(), redisTestEdge6Host()},
			reply: redisTestClusterNodes(without(redisTestEdge6LAN + ":6411")),
			want: []wantAlert{{
				class: "node-inventory-mismatch", target: redisTestEdge6,
				symptom: []string{"its redis.node_ports 6411 are not cluster members at its LAN address 192.168.51.193"},
			}},
		},
		{
			name:  "member at no inventory host's address",
			hosts: []HostSettings{redisTestEdge5Host(), redisTestEdge6Host()},
			reply: redisTestClusterNodes(append(slices.Clone(members), "192.168.51.250:6430")),
			want: []wantAlert{{
				class: "node-inventory-mismatch", target: "192.168.51.250",
				symptom: []string{"port(s) 6430 announce 192.168.51.250, which is no monitor.yml host's LAN address"},
			}},
		},
		{
			name:  "host without a LAN address cannot be compared",
			hosts: []HostSettings{withoutLAN(redisTestEdge5Host()), redisTestEdge6Host()},
			reply: redisTestClusterNodes(members),
			want: []wantAlert{{
				class: "cannot-observe", target: redisTestEdge5 + "/node-inventory",
				symptom: []string{"has no LAN address"},
			}},
		},
		{
			name:  "malformed reply cannot be compared",
			hosts: []HostSettings{redisTestEdge5Host(), redisTestEdge6Host()},
			reply: redisTestClusterNodes(members, "truncated 192.168.51.179:6412@16412 master"),
			want:  []wantAlert{{class: "cannot-observe", target: redisTestEdge6 + "/node-inventory"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := syntheticSettings(&syntheticSource{redisFn: func(host HostSettings, port int, args ...string) (string, error) {
				if host.Name != redisTestEdge6 || port != 6379 || strings.Join(args, " ") != "CLUSTER NODES" {
					t.Errorf("unexpected Redis read %s:%d %v", host.Name, port, args)
				}
				return test.reply, nil
			}})
			settings.Hosts = append([]HostSettings{{Name: "pg-1", Roles: []string{"pg-primary"}}}, test.hosts...)
			alerts, err := NewRedisTopologySignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != len(test.want) {
				t.Fatalf("alerts = %s, want %d", redisTestAlertsSummary(alerts), len(test.want))
			}
			for _, want := range test.want {
				found := false
				for _, alert := range alerts {
					if alert.Class != want.class || alert.Target != want.target {
						continue
					}
					found = true
					for _, text := range want.symptom {
						if !strings.Contains(alert.Symptom, text) {
							t.Fatalf("%s symptom %q does not name %q", want.target, alert.Symptom, text)
						}
					}
				}
				if !found {
					t.Fatalf("no %s alert on %s in %s", want.class, want.target, redisTestAlertsSummary(alerts))
				}
			}
		})
	}
}
