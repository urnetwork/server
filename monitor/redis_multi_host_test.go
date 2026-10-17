package monitor

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Main cluster spans two hosts: edge-6 fronts it on the entry port and
// runs nodes 6380-6411, edge-5 runs nodes 6412-6427 and no entry port. edge-5
// is listed first, as in vault/main/monitor.yml, so a probe that finds the
// entry host by inventory order fails these tests.
const (
	redisTestEdge5 = "by-us-fmt-5-edge-5"
	redisTestEdge6 = "by-us-fmt-5-edge-6"
	// LAN addresses as config settings.yml routes resolves them; each node
	// announces its host's LAN address in CLUSTER NODES
	redisTestEdge5LAN = "192.168.51.179"
	redisTestEdge6LAN = "192.168.51.193"
)

func redisTestPortRange(lo, hi int) []int {
	ports := []int{}
	for port := lo; port <= hi; port++ {
		ports = append(ports, port)
	}
	return ports
}

func redisTestEdge5Host() HostSettings {
	return HostSettings{Name: redisTestEdge5, LANAddress: redisTestEdge5LAN, Roles: []string{"minio", "redis-cluster", "vpn-client"}, RedisNodePorts: redisTestPortRange(6412, 6427)}
}

func redisTestEdge6Host() HostSettings {
	return HostSettings{Name: redisTestEdge6, LANAddress: redisTestEdge6LAN, Roles: []string{"redis-cluster", "vpn-client"}, RedisEntryPort: 6379, RedisNodePorts: redisTestPortRange(6380, 6411)}
}

func redisTwoHostSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "pg-1", Roles: []string{"pg-primary"}},
		redisTestEdge5Host(),
		redisTestEdge6Host(),
	}
	return settings
}

// redisTestPortOwner is the host that runs a node port in the two-host layout.
func redisTestPortOwner(port int) string {
	switch {
	case 6380 <= port && port <= 6411:
		return redisTestEdge6
	case 6412 <= port && port <= 6427:
		return redisTestEdge5
	}
	return ""
}

func redisTestJoinPorts(ports []int) string {
	texts := make([]string, len(ports))
	for i, port := range ports {
		texts[i] = strconv.Itoa(port)
	}
	return strings.Join(texts, " ")
}

// redisTestCallLog records which host each synthetic command reached.
type redisTestCallLog struct {
	lock  sync.Mutex
	calls []string
}

func (self *redisTestCallLog) add(call string) {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.calls = append(self.calls, call)
}

func (self *redisTestCallLog) snapshot() []string {
	self.lock.Lock()
	defer self.lock.Unlock()
	return append([]string(nil), self.calls...)
}

func redisTestAlertTargets(alerts Alerts, class string) []string {
	targets := []string{}
	for _, alert := range alerts {
		if alert.Class == class {
			targets = append(targets, alert.Target)
		}
	}
	slices.Sort(targets)
	return targets
}

func TestRedisTwoHostInventoryFindsEntryHostIndependentOfOrder(t *testing.T) {
	settings := redisTwoHostSettings(&syntheticSource{}).withDefaults()
	if err := settings.validate(); err != nil {
		t.Fatalf("two-host layout rejected: %v", err)
	}
	cfg := configFromSignalSettings(settings)
	if entry := cfg.redisEntryHost(); entry == nil || entry.name != redisTestEdge6 || entry.redisEntryPort != 6379 {
		t.Fatalf("entry host = %+v, want %s on 6379", entry, redisTestEdge6)
	}
	names := []string{}
	for _, h := range cfg.redisNodeHosts() {
		names = append(names, h.name)
	}
	if !slices.Equal(names, []string{redisTestEdge5, redisTestEdge6}) {
		t.Fatalf("node hosts = %v, want inventory order", names)
	}
	if ports := cfg.redisAllNodePorts(); len(ports) != 48 || ports[0] != 6412 || ports[47] != 6411 {
		t.Fatalf("node ports = %v, want 48 across both hosts", ports)
	}
	for _, port := range []int{6380, 6411, 6412, 6427} {
		if h := cfg.redisNodeHost(port); h == nil || h.name != redisTestPortOwner(port) {
			t.Fatalf("node %d host = %+v, want %s", port, h, redisTestPortOwner(port))
		}
	}
	if h := cfg.redisNodeHost(6379); h != nil {
		t.Fatalf("entry port resolved as a node on %s", h.name)
	}
	if h := cfg.redisNodeHostOrEntry(6379); h == nil || h.name != redisTestEdge6 {
		t.Fatalf("unlisted port did not fall back to the entry host: %+v", h)
	}
}

func TestRedisClusterInventoryValidation(t *testing.T) {
	withEntry := func(h HostSettings, port int) HostSettings {
		h.RedisEntryPort = port
		return h
	}
	withPorts := func(h HostSettings, lo, hi int) HostSettings {
		h.RedisNodePorts = redisTestPortRange(lo, hi)
		return h
	}
	tests := []struct {
		name  string
		hosts []HostSettings
		want  string
	}{
		{name: "two hosts with one entry", hosts: []HostSettings{redisTestEdge5Host(), redisTestEdge6Host()}},
		{name: "single host", hosts: []HostSettings{redisTestEdge6Host()}},
		{name: "single host without entry port", hosts: []HostSettings{withEntry(redisTestEdge6Host(), 0)}},
		{name: "role without inventory", hosts: []HostSettings{{Name: "a", Roles: []string{"redis-cluster"}}, {Name: "b", Roles: []string{"redis-cluster"}}}},
		{
			name:  "two entry hosts",
			hosts: []HostSettings{withEntry(redisTestEdge5Host(), 6378), redisTestEdge6Host()},
			want:  "exactly one redis-cluster host may set redis.entry_port; found 2 (" + redisTestEdge5 + ", " + redisTestEdge6 + ")",
		},
		{
			name:  "no entry host",
			hosts: []HostSettings{redisTestEdge5Host(), withEntry(redisTestEdge6Host(), 0)},
			want:  "a Redis cluster spanning 2 redis-cluster hosts needs exactly one host with redis.entry_port; found 0",
		},
		{
			name:  "node port on both hosts",
			hosts: []HostSettings{withPorts(redisTestEdge5Host(), 6411, 6427), redisTestEdge6Host()},
			want:  "redis node port 6411 is configured on both " + redisTestEdge5 + " and " + redisTestEdge6,
		},
		{
			name:  "entry port is another host's node",
			hosts: []HostSettings{redisTestEdge5Host(), withEntry(redisTestEdge6Host(), 6412)},
			want:  "redis entry port 6412 on " + redisTestEdge6 + " is also a node port on " + redisTestEdge5,
		},
		{
			name:  "invalid node port",
			hosts: []HostSettings{withPorts(redisTestEdge5Host(), 65530, 65536), redisTestEdge6Host()},
			want:  "redis-cluster host " + redisTestEdge5 + " has invalid node port 65536",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := syntheticSettings(&syntheticSource{})
			settings.Hosts = test.hosts
			err := settings.withDefaults().validate()
			if test.want == "" {
				if err != nil {
					t.Fatalf("valid layout rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
			// a signal refuses the layout before contacting any host
			if _, err := NewRedisClusterSignal().Run(context.Background(), settings); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("signal error = %v, want %q", err, test.want)
			}
		})
	}
}

func redisTestPingReply(ports []int, statuses map[int]string) string {
	var reply strings.Builder
	fmt.Fprintf(&reply, "redis-ping-v1 begin %d\n", len(ports))
	for _, port := range ports {
		status := "pong"
		if configured, ok := statuses[port]; ok {
			status = configured
		}
		fmt.Fprintf(&reply, "redis-ping-v1 %d %s\n", port, status)
	}
	fmt.Fprintf(&reply, "redis-ping-v1 end %d\n", len(ports))
	return reply.String()
}

func TestRedisClusterSignalTwoHostsReadsClusterOnEntryAndSweepsEachHost(t *testing.T) {
	redisCalls := &redisTestCallLog{}
	sweeps := &redisTestCallLog{}
	source := &syntheticSource{
		redisFn: func(host HostSettings, port int, args ...string) (string, error) {
			redisCalls.add(fmt.Sprintf("%s:%d %s", host.Name, port, strings.Join(args, " ")))
			if strings.Join(args, " ") == "CLUSTER INFO" {
				return "cluster_state:ok\ncluster_slots_fail:0\ncluster_known_nodes:48", nil
			}
			return "", nil
		},
		hostFn: func(host HostSettings, command string) (string, error) {
			if !strings.Contains(command, "redis-ping-v1") {
				return "", fmt.Errorf("unexpected command on %s", host.Name)
			}
			if !strings.Contains(command, "for p in "+redisTestJoinPorts(host.RedisNodePorts)+"; do") {
				t.Errorf("sweep on %s did not cover exactly its own nodes: %s", host.Name, command)
			}
			sweeps.add(host.Name)
			return redisTestPingReply(host.RedisNodePorts, map[int]string{6420: "timeout"}), nil
		},
	}
	alerts, err := NewRedisClusterSignal().Run(context.Background(), redisTwoHostSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if calls := redisCalls.snapshot(); !slices.Equal(calls, []string{redisTestEdge6 + ":6379 CLUSTER INFO"}) {
		t.Fatalf("cluster-wide reads = %v, want one CLUSTER INFO through the entry host", calls)
	}
	if hosts := sweeps.snapshot(); !slices.Equal(hosts, []string{redisTestEdge5, redisTestEdge6}) {
		t.Fatalf("node sweeps = %v, want one per redis-cluster host", hosts)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want only the timed-out edge-5 node", alerts)
	}
	alert := requireAlertClass(t, alerts, "node-unreachable")
	if alert.Target != redisTestEdge5 || alert.Frame != "6420" || !strings.Contains(alert.Observed, "configured=16 literal_pong=15") {
		t.Fatalf("node-unreachable = %+v, want edge-5 port 6420 of its 16 nodes", alert)
	}
}

func TestRedisClusterSignalTwoHostsKeepsSweepingPastAnUnreachableHost(t *testing.T) {
	source := &syntheticSource{
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			return "cluster_state:ok\ncluster_slots_fail:0\ncluster_known_nodes:48", nil
		},
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name == redisTestEdge5 {
				return "", fmt.Errorf("ssh: connect to host: connection timed out")
			}
			return redisTestPingReply(host.RedisNodePorts, map[int]string{6390: "timeout"}), nil
		},
	}
	alerts, err := NewRedisClusterSignal().Run(context.Background(), redisTwoHostSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if targets := redisTestAlertTargets(alerts, "cannot-observe"); !slices.Equal(targets, []string{redisTestEdge5 + "/node-ping"}) {
		t.Fatalf("cannot-observe targets = %v, want only edge-5's sweep", targets)
	}
	if targets := redisTestAlertTargets(alerts, "node-unreachable"); !slices.Equal(targets, []string{redisTestEdge6}) {
		t.Fatalf("node-unreachable targets = %v, want edge-6 still swept", targets)
	}
}

// redisTestClusterNodes renders a CLUSTER NODES reply with one master per
// announced address:port, followed by any raw extra lines.
func redisTestClusterNodes(members []string, extra ...string) string {
	lines := []string{}
	for i, member := range members {
		_, port, _ := strings.Cut(member, ":")
		lines = append(lines, fmt.Sprintf("%040x %s@1%s master - 0 0 %d connected %d", i+1, member, port, i+1, i))
	}
	return strings.Join(append(lines, extra...), "\n")
}

// redisTestMembers lists address:port for each port.
func redisTestMembers(address string, ports []int) []string {
	members := []string{}
	for _, port := range ports {
		members = append(members, fmt.Sprintf("%s:%d", address, port))
	}
	return members
}

// redisTestTwoHostMembers is the Main cluster's membership: edge-6's nodes at
// its LAN address, edge-5's at its own.
func redisTestTwoHostMembers() []string {
	return append(redisTestMembers(redisTestEdge6LAN, redisTestPortRange(6380, 6411)), redisTestMembers(redisTestEdge5LAN, redisTestPortRange(6412, 6427))...)
}

func TestRedisTopologySignalTwoHostsReadsClusterNodesThroughEntry(t *testing.T) {
	redisCalls := &redisTestCallLog{}
	source := &syntheticSource{redisFn: func(host HostSettings, port int, args ...string) (string, error) {
		redisCalls.add(fmt.Sprintf("%s:%d %s", host.Name, port, strings.Join(args, " ")))
		return redisTestClusterNodes(redisTestTwoHostMembers()), nil
	}}
	alerts, err := NewRedisTopologySignal().Run(context.Background(), redisTwoHostSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy topology alerts = %+v", alerts)
	}
	if calls := redisCalls.snapshot(); !slices.Equal(calls, []string{redisTestEdge6 + ":6379 CLUSTER NODES"}) {
		t.Fatalf("topology reads = %v, want CLUSTER NODES through the entry host", calls)
	}
}

func redisTestMemoryRow(port int, usedBytes int64) string {
	return fmt.Sprintf("%d %d 12884901888 8000000000 0 100 volatile-ttl 1000 900 86400000 0 0 0 0 %d", port, usedBytes, usedBytes)
}

func TestRedisMemorySignalTwoHostsJudgesEachHostsCapacity(t *testing.T) {
	redisCalls := &redisTestCallLog{}
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if !strings.Contains(command, "/proc/meminfo") {
				// battery commands
				return "", nil
			}
			ports := host.RedisNodePorts
			if !strings.Contains(command, "for p in "+redisTestJoinPorts(ports)+"; do") {
				t.Errorf("memory sweep on %s did not cover its own nodes: %s", host.Name, command)
			}
			rows := []string{}
			switch host.Name {
			case redisTestEdge5:
				// 251GiB host with 4GiB available and one node at 97%
				rows = append(rows, "host_memory 269509197824 4294967296")
				for _, port := range ports {
					used := int64(9000000000)
					if port == 6420 {
						used = 12500000000
					}
					rows = append(rows, redisTestMemoryRow(port, used))
				}
			case redisTestEdge6:
				rows = append(rows, "host_memory 506806140928 107374182400")
				for _, port := range ports {
					rows = append(rows, redisTestMemoryRow(port, 9000000000))
				}
			}
			return strings.Join(rows, "\n"), nil
		},
		redisFn: func(host HostSettings, port int, args ...string) (string, error) {
			redisCalls.add(fmt.Sprintf("%s:%d", host.Name, port))
			return "", nil
		},
	}
	alerts, err := NewRedisMemorySignal().Run(context.Background(), redisTwoHostSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	capacity := requireAlertClass(t, alerts, "redis-host-capacity")
	if capacity.Target != redisTestEdge5 || !strings.Contains(capacity.Observed, "nodes=16 critical_nodes=1") {
		t.Fatalf("host capacity = %+v, want edge-5 judged on its own 16 nodes", capacity)
	}
	if targets := redisTestAlertTargets(alerts, "redis-host-capacity"); len(targets) != 1 {
		t.Fatalf("host capacity targets = %v, want only edge-5", targets)
	}
	critical := requireAlertClass(t, alerts, "node-mem-critical")
	if critical.Target != redisTestEdge5+":6420" {
		t.Fatalf("critical node target = %q, want edge-5:6420", critical.Target)
	}
	for _, alert := range alerts {
		if strings.HasPrefix(alert.Target, redisTestEdge6) {
			t.Fatalf("healthy edge-6 produced an alert: %+v", alert)
		}
	}
	calls := redisCalls.snapshot()
	if !slices.Contains(calls, redisTestEdge5+":6420") {
		t.Fatalf("critical-node battery calls = %v, want edge-5:6420", calls)
	}
	for _, call := range calls {
		host, port, _ := strings.Cut(call, ":")
		if redisTestPortOwner(atoi(port)) != host {
			t.Fatalf("battery reached node %s on the wrong host %s", port, host)
		}
	}
}

func TestRedisMemorySignalTwoHostsReportsAnUnreadableHostAndJudgesTheOther(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name == redisTestEdge5 {
			return "", fmt.Errorf("ssh: connect to host: connection timed out")
		}
		rows := []string{"host_memory 506806140928 107374182400"}
		for _, port := range host.RedisNodePorts {
			rows = append(rows, redisTestMemoryRow(port, 9000000000))
		}
		return strings.Join(rows, "\n"), nil
	}}
	alerts, err := NewRedisMemorySignal().Run(context.Background(), redisTwoHostSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if targets := redisTestAlertTargets(alerts, "cannot-observe"); !slices.Equal(targets, []string{redisTestEdge5 + "/node-mem"}) {
		t.Fatalf("alerts = %+v, want only edge-5 unobservable", alerts)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want only edge-5 unobservable", alerts)
	}
}

func TestRedisKeyEventsSignalTwoHostsTargetsEachNodesHost(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		ports := host.RedisNodePorts
		if !strings.Contains(command, "for p in "+redisTestJoinPorts(ports)+"; do") {
			t.Errorf("keyevent sweep on %s did not cover its own nodes: %s", host.Name, command)
		}
		rows := []string{}
		for _, port := range ports {
			classes, pubsubConns := "KE", 12
			if host.Name == redisTestEdge5 {
				classes = "-"
			}
			if port == 6415 {
				pubsubConns = 1001
			}
			rows = append(rows, fmt.Sprintf("%d %s %d", port, classes, pubsubConns))
		}
		return strings.Join(rows, "\n"), nil
	}}
	alerts, err := NewRedisKeyEventsSignal().Run(context.Background(), redisTwoHostSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	drift := requireAlertClass(t, alerts, "keyevent-config-drift")
	if drift.Target != redisTestEdge6 || drift.Frame != "16 nodes" || !strings.Contains(drift.Symptom, "(32/48)") || !strings.Contains(drift.Symptom, `6412=""`) {
		t.Fatalf("drift = %+v, want the cluster judgement on the entry host naming edge-5's nodes", drift)
	}
	if targets := redisTestAlertTargets(alerts, "pubsub-conn-shape"); !slices.Equal(targets, []string{redisTestEdge5 + ":6415"}) {
		t.Fatalf("pubsub targets = %v, want edge-5:6415", targets)
	}
}

func TestTTLLeaksSignalTwoHostsSamplesResidueWhereItRuns(t *testing.T) {
	attributions := &redisTestCallLog{}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		switch {
		case strings.Contains(command, "INFO keyspace"):
			ports := host.RedisNodePorts
			if !strings.Contains(command, fmt.Sprintf("seq %d %d", ports[0], ports[len(ports)-1])) {
				t.Errorf("TTL census on %s did not cover its own nodes: %s", host.Name, command)
			}
			rows := []string{}
			for _, port := range ports {
				switch port {
				case 6420:
					rows = append(rows, "6420 6450000 4820000 908000000000000")
				case 6425:
					rows = append(rows, "6425 unreachable")
				default:
					rows = append(rows, fmt.Sprintf("%d 100 90 86400000", port))
				}
			}
			return strings.Join(rows, "\n"), nil
		case strings.Contains(command, "EVAL_RO"):
			attributions.add(host.Name + " " + strings.Fields(command)[2])
			return "4914\n99\n47\n52\n0\n0\n0\n12694\n185\n28799985310940036\nlegacy-contracts", nil
		}
		t.Errorf("unexpected TTL command on %s: %s", host.Name, command)
		return "", nil
	}}
	alerts, err := NewTTLLeaksSignal().Run(context.Background(), redisTwoHostSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	leak := requireAlertClass(t, alerts, "ttl-leaks")
	if leak.Target != redisTestEdge6 || !strings.Contains(leak.Observed, "affected_nodes=1 parsed_nodes=47 configured_nodes=48 affected_ports=6420") || !strings.Contains(leak.Observed, "sample_port=6420") {
		t.Fatalf("ttl leak = %+v, want the fleet judgement on the entry host", leak)
	}
	if calls := attributions.snapshot(); !slices.Equal(calls, []string{redisTestEdge5 + " 6420"}) {
		t.Fatalf("attribution samples = %v, want port 6420 on edge-5", calls)
	}
	unknown := requireAlertClass(t, alerts, "cannot-observe")
	if unknown.Target != redisTestEdge5 || unknown.Frame != "6425" {
		t.Fatalf("unreachable report = %+v, want edge-5 port 6425", unknown)
	}
}

func TestRedisNonexpiringSignalTwoHostsSamplesEachNodeOnItsHost(t *testing.T) {
	candidateSample, controlSample := redisPersistentProvideFixtures()
	samples := &redisTestCallLog{}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		switch {
		case strings.Contains(command, "INFO keyspace"):
			if !strings.Contains(command, "for p in "+redisTestJoinPorts(host.RedisNodePorts)+"; do") {
				t.Errorf("keyspace census on %s did not cover exactly its own nodes: %s", host.Name, command)
			}
			rows := []string{}
			for _, port := range host.RedisNodePorts {
				if port == 6420 {
					rows = append(rows, "6420 5399000 2554000 512")
				} else {
					rows = append(rows, fmt.Sprintf("%d 4810000 2555000 512", port))
				}
			}
			return strings.Join(rows, "\n"), nil
		case strings.Contains(command, "EVAL_RO"):
			for _, port := range redisTestPortRange(6380, 6427) {
				if strings.Contains(command, fmt.Sprintf("-p %d ", port)) {
					if redisTestPortOwner(port) != host.Name {
						t.Errorf("sample for node %d ran on %s", port, host.Name)
					}
					samples.add(strconv.Itoa(port))
					if port == 6420 {
						return candidateSample, nil
					}
					return controlSample, nil
				}
			}
		}
		t.Errorf("unexpected non-expiring command on %s: %s", host.Name, command)
		return "", nil
	}}
	alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), redisTwoHostSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "nonexpiring-key-skew")
	if alert.Target != redisTestEdge5+":6420" || !strings.Contains(alert.Observed, "worst_port=6420") {
		t.Fatalf("skew = %+v, want edge-5:6420", alert)
	}
	if calls := samples.snapshot(); len(calls) != 2 || calls[0] != "6420" {
		t.Fatalf("persistent-family samples = %v, want the candidate then a control", calls)
	}
}

func TestReliabilityPipelineSignalTwoHostsSamplesEachHostsNodes(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		ports := host.RedisNodePorts
		if !strings.Contains(command, fmt.Sprintf("-c --raw -p %d SCARD client_reliability_stats_blocks", ports[0])) ||
			!strings.Contains(command, "for p in "+redisTestJoinPorts(ports)+"; do") {
			t.Errorf("reliability sample on %s did not use its own nodes: %s", host.Name, command)
		}
		var output strings.Builder
		for _, port := range ports {
			latency := "0.1 1.0 0.3 98"
			if port == 6420 {
				latency = "0.1 20.0 14.2 98"
			}
			fmt.Fprintf(&output, "%d\t9\t%s\n", port, latency)
		}
		return output.String(), nil
	}}
	alerts, err := NewReliabilityPipelineSignal().Run(context.Background(), redisTwoHostSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	targets := redisTestAlertTargets(alerts, "reliability-pipeline-degraded")
	if !slices.Equal(targets, []string{redisTestEdge5 + ":6420", redisTestEdge6}) {
		t.Fatalf("reliability targets = %v, want the cluster backlog on the entry host and the slow edge-5 node", targets)
	}
	for _, alert := range alerts {
		if alert.Target == redisTestEdge6 && !strings.Contains(alert.Observed, "blocks=9 nodes_sampled=48") {
			t.Fatalf("cluster backlog observations = %q", alert.Observed)
		}
	}
}

func TestRedisProcessSignalTwoHostsReportsEachHost(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == redisTestEdge6 {
			return "", fmt.Errorf("ssh: connect to host: connection timed out")
		}
		return "kernel_oom Out of memory: Killed process 1234 (redis-server)\nkernel_source complete\n", nil
	}}
	alerts, err := NewRedisProcessSignal().Run(context.Background(), redisTwoHostSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if targets := redisTestAlertTargets(alerts, "redis-kernel-oom"); !slices.Equal(targets, []string{redisTestEdge5}) {
		t.Fatalf("kernel OOM targets = %v, want edge-5", targets)
	}
	if targets := redisTestAlertTargets(alerts, "cannot-observe"); !slices.Equal(targets, []string{redisTestEdge6 + "/redis-process"}) {
		t.Fatalf("cannot-observe targets = %v, want edge-6", targets)
	}
}

func TestKeyFamiliesSignalTwoHostsScansFullestNodeOnItsHost(t *testing.T) {
	stateDir := t.TempDir()
	populateMetric(t, stateDir, "redis/family/ckey_<id>", 1000, 1000, 1000)
	scans := &redisTestCallLog{}
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if !strings.Contains(command, "INFO memory") {
				return "", nil
			}
			if !strings.Contains(command, "for p in "+redisTestJoinPorts(host.RedisNodePorts)+"; do") {
				t.Errorf("usage sweep on %s did not cover exactly its own nodes: %s", host.Name, command)
			}
			rows := []string{}
			for _, port := range host.RedisNodePorts {
				used := 4000000
				if port == 6420 {
					used = 9000000
				}
				rows = append(rows, fmt.Sprintf("%d %d 10000000", port, used))
			}
			return strings.Join(rows, "\n"), nil
		},
		hostTimeoutFn: func(host HostSettings, command string, _ time.Duration) (string, error) {
			scans.add(host.Name + " " + strings.Fields(command)[5])
			return "30000 ckey_<id>", nil
		},
	}
	settings := redisTwoHostSettings(source)
	settings.StateDir = stateDir
	alerts, err := NewKeyFamiliesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if calls := scans.snapshot(); !slices.Equal(calls, []string{redisTestEdge5 + " 6420"}) {
		t.Fatalf("key scans = %v, want the fullest node 6420 on edge-5", calls)
	}
	if alert := requireAlertClass(t, alerts, "family-growth"); alert.Target != redisTestEdge5+":6420" {
		t.Fatalf("family growth target = %q, want edge-5:6420", alert.Target)
	}
}

func TestRedisRatesSignalTwoHostsExpectsEveryNode(t *testing.T) {
	for _, test := range []struct {
		name   string
		series int
		alerts int
	}{
		{name: "all 48 nodes", series: 48},
		{name: "edge-6 only", series: 32, alerts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := newRedisRatesGrafanaServer(t, "test", "5m", "5m", 200, "")
			defer server.Close()
			settings, _ := redisRatesSyntheticSettings(t, redisRatesMimirFixture(test.series, 0, test.series))
			settings.Grafana.AdminPassword = "test"
			settings.Hosts = []HostSettings{settings.Hosts[0], redisTestEdge5Host(), redisTestEdge6Host(), settings.Hosts[2]}
			alerts, err := newRedisRatesSignal(server.Client(), server.URL).Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != test.alerts {
				t.Fatalf("alerts = %+v, want %d", alerts, test.alerts)
			}
			if test.alerts != 0 && !strings.Contains(requireAlertClass(t, alerts, "redis-rate-source-coverage").Markdown(), "expected_nodes=48") {
				t.Fatalf("coverage alert does not expect all 48 nodes: %+v", alerts)
			}
		})
	}
}

func TestRedisLatencyTrackingSumsEveryHostsReduction(t *testing.T) {
	replies := map[string]string{
		redisTestEdge5: "expected=16 enabled=16 disabled=0 invalid=0 unreachable=0",
		redisTestEdge6: "expected=32 enabled=30 disabled=1 invalid=0 unreachable=1",
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if !strings.Contains(command, "for port in "+redisTestJoinPorts(host.RedisNodePorts)+"; do") {
			t.Errorf("latency-tracking reduction on %s did not cover exactly its own nodes: %s", host.Name, command)
		}
		return replies[host.Name], nil
	}}
	env, err := newProbeEnv(redisTwoHostSettings(source).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	observation, failures, err := observeRedisLatencyTracking(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if observation != (redisLatencyTrackingObservation{expected: 48, enabled: 46, disabled: 1, unreachable: 1}) || len(failures) != 0 {
		t.Fatalf("cluster observation = %+v failures = %+v", observation, failures)
	}

	// each host's reduction must match that host's own inventory: one counting
	// another host's nodes is not summed, its own nodes count as unreachable
	replies[redisTestEdge5] = "expected=32 enabled=32 disabled=0 invalid=0 unreachable=0"
	observation, failures, err = observeRedisLatencyTracking(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if observation != (redisLatencyTrackingObservation{expected: 48, enabled: 30, disabled: 1, unreachable: 17}) {
		t.Fatalf("a reduction counting another host's nodes was accepted: %+v", observation)
	}
	if len(failures) != 1 || failures[0].host.name != redisTestEdge5 {
		t.Fatalf("failures = %+v, want edge-5's inconsistent reduction", failures)
	}

	// with no host reduced the census itself fails
	replies[redisTestEdge6] = "expected=16 enabled=16 disabled=0 invalid=0 unreachable=0"
	if _, _, err := observeRedisLatencyTracking(context.Background(), env); err == nil {
		t.Fatal("a census with no consistent host reduction was accepted")
	}
}

// redisTestUnreachable is the transport failure of a host the monitor cannot
// reach over ssh.
func redisTestUnreachable(host string) error {
	return fmt.Errorf("ssh: connect to host %s: connection timed out", host)
}

// redisTestSick marks one node per host that a fixture drives out of its
// band: 6390 on edge-6 and 6420 on edge-5.
func redisTestSick(port int) bool {
	return port == 6390 || port == 6420
}

// redisTestRows renders one fixture row per node of the host.
func redisTestRows(host HostSettings, row func(port int) string) string {
	rows := []string{}
	for _, port := range host.RedisNodePorts {
		rows = append(rows, row(port))
	}
	return strings.Join(rows, "\n")
}

// redisTestHasAlert reports whether an alert of class targets target and,
// when observed is set, carries it in its observed values.
func redisTestHasAlert(alerts Alerts, class, target, observed string) bool {
	for _, alert := range alerts {
		if alert.Class == class && alert.Target == target && strings.Contains(alert.Observed, observed) {
			return true
		}
	}
	return false
}

// redisTestAlertsSummary lists class@target for a failure message.
func redisTestAlertsSummary(alerts Alerts) string {
	summary := []string{}
	for _, alert := range alerts {
		summary = append(summary, alert.Class+"@"+alert.Target)
	}
	slices.Sort(summary)
	return strings.Join(summary, " ")
}

// One unreadable redis-cluster host must not blank a signal: the host is
// reported as unobservable on its own target, the readable host's nodes keep
// raising their own alerts, and a cluster-wide verdict judged without the
// unread host says so.
func TestRedisSignalsTwoHostsJudgeTheReadableHostWhenOneIsUnreachable(t *testing.T) {
	candidateSample, controlSample := redisPersistentProvideFixtures()
	type wantAlert struct {
		class, target, observed string
	}
	keyEvents := func(host HostSettings, command string) (string, error) {
		return redisTestRows(host, func(port int) string {
			classes, pubsubConns := "KE", 12
			if port == 6391 || port == 6413 {
				classes = "-"
			}
			if redisTestSick(port) {
				pubsubConns = 1001
			}
			return fmt.Sprintf("%d %s %d", port, classes, pubsubConns)
		}), nil
	}
	ttlCensus := func(host HostSettings, command string) (string, error) {
		if strings.Contains(command, "EVAL_RO") {
			return "4914\n99\n47\n52\n0\n0\n0\n12694\n185\n28799985310940036\nlegacy-contracts", nil
		}
		return redisTestRows(host, func(port int) string {
			if redisTestSick(port) {
				return fmt.Sprintf("%d 6450000 4820000 908000000000000", port)
			}
			return fmt.Sprintf("%d 100 90 86400000", port)
		}), nil
	}
	reliability := func(host HostSettings, command string) (string, error) {
		return redisTestRows(host, func(port int) string {
			latency := "0.1 1.0 0.3 98"
			if redisTestSick(port) {
				latency = "0.1 20.0 14.2 98"
			}
			return fmt.Sprintf("%d\t9\t%s", port, latency)
		}), nil
	}
	keyspace := func(host HostSettings, command string) (string, error) {
		if strings.Contains(command, "EVAL_RO") {
			if strings.Contains(command, "-p 6390 ") || strings.Contains(command, "-p 6420 ") {
				return candidateSample, nil
			}
			return controlSample, nil
		}
		return redisTestRows(host, func(port int) string {
			if redisTestSick(port) {
				return fmt.Sprintf("%d 5399000 2554000 512", port)
			}
			return fmt.Sprintf("%d 4810000 2555000 512", port)
		}), nil
	}
	usage := func(fullUsed, otherUsed, maxmemory int64) func(HostSettings, string) (string, error) {
		return func(host HostSettings, command string) (string, error) {
			if strings.Contains(command, "EVAL_RO") {
				return syntheticRedisByteSample, nil
			}
			return redisTestRows(host, func(port int) string {
				if redisTestSick(port) {
					return fmt.Sprintf("%d %d %d", port, fullUsed, maxmemory)
				}
				return fmt.Sprintf("%d %d %d", port, otherUsed, maxmemory)
			}), nil
		}
	}
	latencyTracking := func(host HostSettings, command string) (string, error) {
		if host.Name == "metrics.invalid" {
			return metricCardinalityFixture(map[string]int{
				"total": 70_000, "egress_buckets": 0, "egress_sums": 625, "egress_counts": 625, "redis_latency": 0,
			}, ""), nil
		}
		nodes := len(host.RedisNodePorts)
		return fmt.Sprintf("expected=%d enabled=1 disabled=%d invalid=0 unreachable=0", nodes, nodes-1), nil
	}

	tests := []struct {
		name        string
		signal      Signal
		down        string
		observation string
		hostFn      func(HostSettings, string) (string, error)
		settings    func(t *testing.T, settings *SignalSettings)
		want        []wantAlert
	}{
		{
			name: "key events, edge-5 unreachable", signal: NewRedisKeyEventsSignal(),
			down: redisTestEdge5, observation: "key-events", hostFn: keyEvents,
			want: []wantAlert{
				{class: "pubsub-conn-shape", target: redisTestEdge6 + ":6390"},
				{class: "keyevent-config-drift", target: redisTestEdge6, observed: "unobserved_hosts=" + redisTestEdge5 + " unobserved_nodes=16"},
			},
		},
		{
			name: "key events, entry host edge-6 unreachable", signal: NewRedisKeyEventsSignal(),
			down: redisTestEdge6, observation: "key-events", hostFn: keyEvents,
			want: []wantAlert{
				{class: "pubsub-conn-shape", target: redisTestEdge5 + ":6420"},
				{class: "keyevent-config-drift", target: redisTestEdge6, observed: "unobserved_hosts=" + redisTestEdge6 + " unobserved_nodes=32"},
			},
		},
		{
			name: "ttl leaks, edge-5 unreachable", signal: NewTTLLeaksSignal(),
			down: redisTestEdge5, observation: "ttl-census", hostFn: ttlCensus,
			want: []wantAlert{
				{class: "ttl-leaks", target: redisTestEdge6, observed: "parsed_nodes=32 configured_nodes=48 affected_ports=6390"},
				{class: "ttl-leaks", target: redisTestEdge6, observed: "unobserved_hosts=" + redisTestEdge5},
			},
		},
		{
			name: "reliability pipeline, entry host edge-6 unreachable", signal: NewReliabilityPipelineSignal(),
			down: redisTestEdge6, observation: "reliability-load", hostFn: reliability,
			want: []wantAlert{
				{class: "reliability-pipeline-degraded", target: redisTestEdge5 + ":6420"},
				{class: "reliability-pipeline-degraded", target: redisTestEdge6, observed: "blocks=9 nodes_sampled=16"},
				{class: "reliability-pipeline-degraded", target: redisTestEdge6, observed: "unobserved_hosts=" + redisTestEdge6},
			},
		},
		{
			name: "non-expiring skew, edge-5 unreachable", signal: NewRedisNonexpiringSignal(),
			down: redisTestEdge5, observation: "keyspace-census", hostFn: keyspace,
			want: []wantAlert{
				{class: "nonexpiring-key-skew", target: redisTestEdge6 + ":6390", observed: "observed_masters=32 unobserved_hosts=" + redisTestEdge5},
			},
		},
		{
			name: "key families, edge-5 unreachable", signal: NewKeyFamiliesSignal(),
			down: redisTestEdge5, observation: "key-families", hostFn: usage(9000000, 4000000, 10000000),
			settings: func(t *testing.T, settings *SignalSettings) {
				settings.StateDir = t.TempDir()
				populateMetric(t, settings.StateDir, "redis/family/ckey_<id>", 1000, 1000, 1000)
				settings.Source.(*syntheticSource).hostTimeoutFn = func(host HostSettings, command string, _ time.Duration) (string, error) {
					if !strings.Contains(command, "-p 6390 ") {
						t.Errorf("key scan on %s was not the fullest readable node: %s", host.Name, command)
					}
					return "30000 ckey_<id>", nil
				}
			},
			want: []wantAlert{
				{class: "family-growth", target: redisTestEdge6 + ":6390", observed: "unobserved_hosts=" + redisTestEdge5},
			},
		},
		{
			name: "redis bytes, edge-5 unreachable", signal: NewRedisBytesSignal(),
			down: redisTestEdge5, observation: "byte-families", hostFn: usage(12700000000, 8000000000, 12884901888),
			want: []wantAlert{
				{class: "score-byte-dominance", target: redisTestEdge6 + ":6390", observed: "unobserved_hosts=" + redisTestEdge5},
			},
		},
		{
			name: "cardinality latency-tracking source, edge-5 unreachable", signal: NewCardinalitySignal(),
			down: redisTestEdge5, observation: "redis-latencystats", hostFn: latencyTracking,
			settings: func(t *testing.T, settings *SignalSettings) {
				settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.invalid", Roles: []string{"services"}})
			},
			want: []wantAlert{
				{class: "redis-latencystats-source-drift", target: "redis-cluster", observed: "source_expected_nodes=48 source_enabled_nodes=1"},
				{class: "redis-latencystats-source-unobservable", target: "redis-cluster", observed: "source_unreachable_nodes=16"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{
				hostFn: func(host HostSettings, command string) (string, error) {
					if host.Name == test.down {
						return "", redisTestUnreachable(host.Name)
					}
					return test.hostFn(host, command)
				},
				redisFn: func(HostSettings, int, ...string) (string, error) { return "", nil },
			}
			settings := redisTwoHostSettings(source)
			if test.settings != nil {
				test.settings(t, &settings)
			}
			alerts, err := test.signal.Run(context.Background(), settings)
			if err != nil {
				t.Fatalf("an unreachable %s blanked the signal: %v", test.down, err)
			}
			if targets := redisTestAlertTargets(alerts, "cannot-observe"); !slices.Equal(targets, []string{test.down + "/" + test.observation}) {
				t.Fatalf("cannot-observe targets = %v, want only %s/%s", targets, test.down, test.observation)
			}
			for _, want := range test.want {
				if !redisTestHasAlert(alerts, want.class, want.target, want.observed) {
					t.Fatalf("no %s alert on %s observing %q; alerts: %s", want.class, want.target, want.observed, redisTestAlertsSummary(alerts))
				}
			}
			for _, alert := range alerts {
				if strings.HasPrefix(alert.Target, test.down+":") {
					t.Fatalf("an unread node produced an alert: %+v", alert)
				}
			}
		})
	}
}

// With no redis-cluster host readable a signal fails as the single-host
// signal always has; the cardinality signal keeps its Mimir findings and
// reports its Redis source as unavailable.
func TestRedisSignalsTwoHostsFailWhenNoHostIsReadable(t *testing.T) {
	for _, signal := range []Signal{
		NewRedisKeyEventsSignal(), NewTTLLeaksSignal(), NewReliabilityPipelineSignal(),
		NewRedisNonexpiringSignal(), NewKeyFamiliesSignal(), NewRedisBytesSignal(),
	} {
		t.Run(signal.Key(), func(t *testing.T) {
			source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
				return "", redisTestUnreachable(host.Name)
			}}
			alerts, err := signal.Run(context.Background(), redisTwoHostSettings(source))
			if err == nil || !strings.Contains(err.Error(), redisTestEdge5) || !strings.Contains(err.Error(), redisTestEdge6) {
				t.Fatalf("err = %v alerts = %s, want both hosts' failures", err, redisTestAlertsSummary(alerts))
			}
		})
	}

	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name == "metrics.invalid" {
			return metricCardinalityFixture(map[string]int{
				"total": 70_000, "egress_buckets": 0, "egress_sums": 625, "egress_counts": 625, "redis_latency": 0,
			}, ""), nil
		}
		return "", redisTestUnreachable(host.Name)
	}}
	settings := redisTwoHostSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.invalid", Roles: []string{"services"}})
	alerts, err := NewCardinalitySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if !redisTestHasAlert(alerts, "redis-latencystats-source-unobservable", "redis-cluster", "source_observation=unavailable") {
		t.Fatalf("alerts = %s, want the Redis source unavailable", redisTestAlertsSummary(alerts))
	}
	if targets := redisTestAlertTargets(alerts, "cannot-observe"); len(targets) != 0 {
		t.Fatalf("cannot-observe targets = %v, want the single unavailable-source finding", targets)
	}
}

// redisTestProbeFindings runs a probe directly, so healthy findings are
// visible.
func redisTestProbeFindings(t *testing.T, check probe, settings SignalSettings) []finding {
	t.Helper()
	settings = settings.withDefaults()
	if err := settings.validate(); err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := check.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

// A cluster-wide healthy verdict needs every host: nodes on an unread host
// may hold the drift, residue, backlog latency, or skew.
func TestRedisProbesTwoHostsWithholdClusterHealthWhileAHostIsUnread(t *testing.T) {
	healthy := func(host HostSettings, command string) (string, error) {
		switch {
		case strings.Contains(command, "notify-keyspace-events"):
			return redisTestRows(host, func(port int) string { return fmt.Sprintf("%d KE 12", port) }), nil
		case strings.Contains(command, "SCARD client_reliability_stats_blocks"):
			return redisTestRows(host, func(port int) string { return fmt.Sprintf("%d\t3\t0.1 1.0 0.3 98", port) }), nil
		case strings.Contains(command, "CLUSTER NODES"):
			return redisTestRows(host, func(port int) string { return fmt.Sprintf("%d 4810000 2555000 512", port) }), nil
		case strings.Contains(command, "INFO keyspace"):
			return redisTestRows(host, func(port int) string { return fmt.Sprintf("%d 100 90 86400000", port) }), nil
		}
		return "", fmt.Errorf("unexpected command on %s", host.Name)
	}
	tests := []struct {
		probe probe
		class string
	}{
		{probe: redisKeyEventProbe{}, class: "keyevent-config-drift"},
		{probe: redisTTLLeaksProbe{}, class: "ttl-leaks"},
		{probe: redisReliabilityLoadProbe{}, class: "reliability-pipeline-degraded"},
		{probe: redisNonexpiringProbe{}, class: "nonexpiring-key-skew"},
	}
	for _, test := range tests {
		for _, down := range []string{"", redisTestEdge5} {
			t.Run(test.class+"/down="+down, func(t *testing.T) {
				findings := redisTestProbeFindings(t, test.probe, redisTwoHostSettings(&syntheticSource{
					hostFn: func(host HostSettings, command string) (string, error) {
						if host.Name == down {
							return "", redisTestUnreachable(host.Name)
						}
						return healthy(host, command)
					},
				}))
				healthyVerdict, unread := false, false
				for _, finding := range findings {
					if finding.healthy && finding.class == test.class && finding.target == redisTestEdge6 {
						healthyVerdict = true
					} else if !finding.healthy && finding.class == "cannot-observe" && strings.HasPrefix(finding.target, redisTestEdge5+"/") {
						unread = true
					} else if !finding.healthy {
						t.Fatalf("healthy fixture produced %+v", finding)
					}
				}
				if healthyVerdict != (down == "") || unread != (down != "") {
					t.Fatalf("healthy verdict = %t, unread report = %t with %q unreachable", healthyVerdict, unread, down)
				}
			})
		}
	}

	// the latency-tracking census counts an unread host's nodes unreachable
	sample := metricCardinalitySample{totalSeries: 70_000, egressHealthSumSeries: 625, egressHealthCountSeries: 625}
	for _, test := range []struct {
		source  redisLatencyTrackingObservation
		healthy bool
	}{
		{source: redisLatencyTrackingObservation{expected: 48, disabled: 48}, healthy: true},
		{source: redisLatencyTrackingObservation{expected: 48, disabled: 32, unreachable: 16}},
	} {
		healthyVerdict := false
		for _, finding := range evaluateMetricCardinality("metrics.invalid", sample, test.source, nil) {
			if finding.class == "redis-latencystats-source-drift" {
				if !finding.healthy {
					t.Fatalf("disabled nodes reported as drift: %+v", finding)
				}
				healthyVerdict = true
			}
		}
		if healthyVerdict != test.healthy {
			t.Fatalf("source %+v: healthy drift verdict = %t, want %t", test.source, healthyVerdict, test.healthy)
		}
	}
}

// The family series is the fullest node's sample across the fleet. With a
// host unread the fullest readable node is still judged against that series,
// but its sample is not recorded into it.
func TestKeyFamiliesSignalTwoHostsRecordsNoBaselineFromAPartialFleet(t *testing.T) {
	const metric = "redis/family/ckey_<id>"
	for _, test := range []struct {
		name     string
		down     string
		recorded int
	}{
		{name: "every host read", recorded: 4},
		{name: "edge-5 unreachable", down: redisTestEdge5, recorded: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateDir := t.TempDir()
			populateMetric(t, stateDir, metric, 1000, 1000, 1000)
			source := &syntheticSource{
				hostFn: func(host HostSettings, command string) (string, error) {
					if host.Name == test.down {
						return "", redisTestUnreachable(host.Name)
					}
					return redisTestRows(host, func(port int) string {
						if redisTestSick(port) {
							return fmt.Sprintf("%d 9000000 10000000", port)
						}
						return fmt.Sprintf("%d 4000000 10000000", port)
					}), nil
				},
				hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
					return "30000 ckey_<id>", nil
				},
			}
			settings := redisTwoHostSettings(source)
			settings.StateDir = stateDir
			alerts, err := NewKeyFamiliesSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			requireAlertClass(t, alerts, "family-growth")
			store, err := newBaselineStore(stateDir + "/baseline")
			if err != nil {
				t.Fatal(err)
			}
			if samples := store.trailingWindow(metric, 24*time.Hour); len(samples) != test.recorded {
				t.Fatalf("family baseline holds %d samples, want %d", len(samples), test.recorded)
			}
		})
	}
}

// A host excluded by the monitor's host-scope policy is a deliberate pause,
// not an unreachable host: the readable host's nodes are still judged, the
// exclusion is reported once as partial scope, and no visibility alert names
// the excluded host.
func TestRedisKeyEventsSignalTwoHostsJudgesEdge6WhileEdge5IsExcluded(t *testing.T) {
	contacted := &redisTestCallLog{}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		contacted.add(host.Name)
		return redisTestRows(host, func(port int) string {
			pubsubConns := 12
			if redisTestSick(port) {
				pubsubConns = 1001
			}
			return fmt.Sprintf("%d KE %d", port, pubsubConns)
		}), nil
	}}
	settings, err := ExcludeHosts(redisTwoHostSettings(source), redisTestEdge5)
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewRedisKeyEventsSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if hosts := contacted.snapshot(); !slices.Equal(hosts, []string{redisTestEdge6}) {
		t.Fatalf("contacted hosts = %v, want only edge-6", hosts)
	}
	if !redisTestHasAlert(alerts, "pubsub-conn-shape", redisTestEdge6+":6390", "pubsub_conns=1001") {
		t.Fatalf("alerts = %s, want edge-6's pubsub page", redisTestAlertsSummary(alerts))
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	if targets := redisTestAlertTargets(alerts, "cannot-observe"); len(targets) != 0 {
		t.Fatalf("cannot-observe targets = %v, want the exclusion reported only as partial scope", targets)
	}
}
