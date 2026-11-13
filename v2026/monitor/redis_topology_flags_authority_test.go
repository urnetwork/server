// Topology authority belongs to exact flags and a complete cluster reply.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A single synthetic addressed member makes inventory and topology agree.
func redisTopologyFlagsSettings(reply *string) SignalSettings {
	settings := syntheticSettings(&syntheticSource{redisFn: func(HostSettings, int, ...string) (string, error) {
		return *reply, nil
	}})
	settings.Hosts[1] = HostSettings{
		Name: "redis-topology.example", Roles: []string{"redis-cluster"},
		LANAddress: "192.0.2.1", RedisEntryPort: 6379,
		RedisNodePorts: []int{6380},
	}
	return settings
}

// Optional hostname words must not fabricate phantoms or suppress membership.
func TestRedisTopologyFlagsMasterNoaddrHostname(t *testing.T) {
	reply := "test-master 192.0.2.1:6380@16380,redis-noaddr.example master - 0 0 1 connected 0-16383"
	settings := redisTopologyFlagsSettings(&reply)
	settings.StateDir = t.TempDir()
	alerts, err := NewRedisTopologySignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("addressed master hostname changed topology: alerts=%+v err=%v", alerts, err)
	}
	store, err := newBaselineStore(settings.StateDir + "/baseline")
	if err != nil {
		t.Fatal(err)
	}
	samples := store.metricSamples["redis/replica-count"]
	if len(samples) != 1 || samples[0].v != 0 {
		t.Fatalf("master replica baseline = %+v, want one zero", samples)
	}
}

// A hostname cannot satisfy armed replica coverage or enter its count baseline.
func TestRedisTopologyFlagsMasterSlaveHostname(t *testing.T) {
	reply := "test-master 192.0.2.1:6380@16380,redis-slave.example master - 0 0 1 connected"
	settings := redisTopologyFlagsSettings(&reply)
	settings.Hosts[1].RedisExpectedReplicas = 1
	settings.StateDir = t.TempDir()
	alerts, err := NewRedisTopologySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Class != "replica-cover" || !strings.Contains(alerts[0].Observed, "replica_count=0 expected=1") {
		t.Fatalf("master hostname fabricated coverage: %+v", alerts)
	}
	store, err := newBaselineStore(settings.StateDir + "/baseline")
	if err != nil {
		t.Fatal(err)
	}
	samples := store.metricSamples["redis/replica-count"]
	if len(samples) != 1 || samples[0].v != 0 {
		t.Fatalf("master replica baseline = %+v, want one zero", samples)
	}
}

// The genuine slave flag survives a noaddr word in the optional hostname.
func TestRedisTopologyFlagsSlaveNoaddrHostname(t *testing.T) {
	reply := "test-replica 192.0.2.1:6380@16380,replica-noaddr.example slave test-master 0 0 1 connected"
	settings := redisTopologyFlagsSettings(&reply)
	settings.Hosts[1].RedisExpectedReplicas = 1
	settings.StateDir = t.TempDir()
	alerts, err := NewRedisTopologySignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("genuine replica was lost: alerts=%+v err=%v", alerts, err)
	}
	store, err := newBaselineStore(settings.StateDir + "/baseline")
	if err != nil {
		t.Fatal(err)
	}
	samples := store.metricSamples["redis/replica-count"]
	if len(samples) != 1 || samples[0].v != 1 {
		t.Fatalf("replica baseline = %+v, want one replica", samples)
	}
}

// Exact flags, zero client ports, failed members, and handshake exclusion keep
// their independent meanings; lookalike words outside flags supply no authority.
func TestRedisTopologyFlagsExactControls(t *testing.T) {
	cases := []struct {
		name    string
		row     string
		phantom bool
		replica bool
	}{
		{name: "noaddr", row: "test-phantom :0@0 slave,noaddr test-master 0 0 1 disconnected", phantom: true},
		{name: "addressed-noaddr", row: "test-phantom 192.0.2.2:6381@16381 master,noaddr - 0 0 1 disconnected", phantom: true},
		{name: "zero-port", row: "test-phantom 192.0.2.2:0@16380 master - 0 0 1 disconnected", phantom: true},
		{name: "slave", row: "test-replica 192.0.2.2:6381@16381 myself,slave test-master 0 0 1 connected", replica: true},
		{name: "failed-slave", row: "test-replica 192.0.2.2:6381@16381 slave,fail test-master 0 0 1 disconnected", replica: true},
		{name: "handshake", row: "test-joining 192.0.2.2:6381@16381 slave,handshake test-master 0 0 1 disconnected"},
		{name: "other-fields", row: "test-noaddr-slave 192.0.2.2:6381@16381 master slave-noaddr-master 0 0 1 connected"},
		{name: "flag-substrings", row: "test-member 192.0.2.2:6381@16381 master,notnoaddr,notslave - 0 0 1 connected"},
	}
	for _, c := range cases {
		reply := "test-master 192.0.2.1:6380@16380 master - 0 0 1 connected\n" + c.row
		settings := redisTopologyFlagsSettings(&reply)
		settings.Hosts[1].RedisExpectedReplicas = 1
		alerts, err := NewRedisTopologySignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		phantom, missingReplica := false, false
		for _, alert := range alerts {
			phantom = phantom || alert.Class == "phantom-nodes"
			missingReplica = missingReplica || alert.Class == "replica-cover"
		}
		if phantom != c.phantom || missingReplica == c.replica {
			t.Errorf("%s: phantom=%t missingReplica=%t, want phantom=%t replica=%t", c.name, phantom, missingReplica, c.phantom, c.replica)
		}
	}
}

// Malformed replies cannot clear either topology class or advance a prior
// baseline; a subsequent complete reply restores both observation and history.
func TestRedisTopologyFlagsUnknownPreservesBaseline(t *testing.T) {
	valid := "test-replica 192.0.2.1:6380@16380 slave test-master 0 0 1 connected"
	for _, expected := range []int{0, 1} {
		for _, malformed := range []string{
			"", " \n\t", "truncated 192.0.2.1:6380@16380 master",
			valid + "\ntruncated 192.0.2.2:6381@16381 slave",
			valid + "\nbroken invalid-address master - 0 0 1 connected",
			valid + "\nbroken 192.0.2.2:invalid@16381 slave - 0 0 1 connected",
			valid + "\nbroken 192.0.2.2:65536@16381 slave - 0 0 1 connected",
		} {
			reply := malformed
			settings := redisTopologyFlagsSettings(&reply)
			settings.Hosts[1].RedisExpectedReplicas = expected
			settings.StateDir = t.TempDir()
			store, err := newBaselineStore(settings.StateDir + "/baseline")
			if err != nil {
				t.Fatal(err)
			}
			store.record("redis/replica-count", time.Now(), 7)
			env, err := newProbeEnv(settings.withDefaults())
			if err != nil {
				t.Fatal(err)
			}
			findings, err := (redisTopologyProbe{}).check(context.Background(), env)
			if err != nil || len(findings) != 1 || !findings[0].isUnobservable() || findings[0].healthy {
				t.Fatalf("malformed reply produced topology authority: findings=%+v err=%v", findings, err)
			}
			alerts, err := NewRedisTopologySignal().Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
				t.Fatalf("malformed reply lost public visibility: alerts=%+v err=%v", alerts, err)
			}
			store, err = newBaselineStore(settings.StateDir + "/baseline")
			if err != nil {
				t.Fatal(err)
			}
			samples := store.metricSamples["redis/replica-count"]
			if len(samples) != 1 || samples[0].v != 7 {
				t.Fatalf("unknown reply advanced baseline: %+v", samples)
			}
			reply = valid
			alerts, err = NewRedisTopologySignal().Run(context.Background(), settings)
			if err != nil || len(alerts) != 0 {
				t.Fatalf("corrected reply did not recover: alerts=%+v err=%v", alerts, err)
			}
			store, err = newBaselineStore(settings.StateDir + "/baseline")
			if err != nil {
				t.Fatal(err)
			}
			samples = store.metricSamples["redis/replica-count"]
			if len(samples) != 2 || samples[1].v != 1 {
				t.Fatalf("complete reply did not restore baseline: %+v", samples)
			}
		}
	}
}

// A source error remains an error, with no successful count recorded.
func TestRedisTopologyFlagsSourceFailurePreservesBaseline(t *testing.T) {
	sourceErr := errors.New("synthetic topology source failure")
	settings := syntheticSettings(&syntheticSource{redisFn: func(HostSettings, int, ...string) (string, error) {
		return "test-master 192.0.2.1:6380@16380 master - 0 0 1 connected", sourceErr
	}})
	settings.StateDir = t.TempDir()
	alerts, err := NewRedisTopologySignal().Run(context.Background(), settings)
	if !errors.Is(err, sourceErr) || len(alerts) != 0 {
		t.Fatalf("source error changed: alerts=%+v err=%v", alerts, err)
	}
	store, err := newBaselineStore(settings.StateDir + "/baseline")
	if err != nil {
		t.Fatal(err)
	}
	if len(store.metricSamples["redis/replica-count"]) != 0 {
		t.Fatal("source failure advanced replica baseline")
	}
}
