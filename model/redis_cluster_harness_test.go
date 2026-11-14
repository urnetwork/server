//go:build sidecar_pending

package model

// What FS-36's and FS-120's schedules run on (SDK-M8-12; §22.4's T3, Q-TEST-2, §18.7's steps 0 to 6).
//
// The cluster harness. A 3-master Redis Cluster at 8.10.2 with no replicas, as production's masters run
// (§18.7's step 5), in Docker containers named sidecar-test-redis-cluster-*: one container holds the
// network namespace and publishes each master's port on 127.0.0.1, above 50000 and off the production
// tunnel ports, and each master is a container of its own in that namespace, so the masters reach each
// other and announce themselves on loopback, and one master is killed and restarted while the others
// serve. A master keeps no persistence but the dump the harness stages: testRedisCluster.dump saves the
// master's data as it stands and sets the file aside, kill puts a held dump, or none, where the master
// loads from and ends the process with no chance to save, and start brings it back, with a new run_id,
// from that dump or empty, serving the slots its nodes.conf names. The harness drives the containers by
// docker and redis-cli inside them, and pulls nothing: the image must be on the machine. It runs only
// where SIDECAR_TEST_REDIS_CLUSTER_HARNESS says so, "run" to start a cluster and remove it at the end,
// "keep" to leave it up for a run that names it to server/test-env.sh.
//
// The grounds. A test's own process dials Redis only through the test environment's guard, which
// attests one Redis endpoint today (server/test-env.sh; QD-028's list of authorities is not in it yet),
// so a schedule runs on what the environment gives it:
//
//	twin     T2's FakeRedis under the push twins and a fake clock, as a cluster of two masters over
//	         Postgres with MG8 (sidecar_push_token_slots_test.go), where a master restarts empty or from
//	         an older dump in the fake
//	redis    the test environment's own Redis through server's client and the real Lua: one node today,
//	         whose restart no test may force, since every track shares it
//	cluster  the same, once the environment's Redis is this harness's cluster, its masters attested:
//	         the rows that restart a master then run on it through the harness
//
// The schedule's clock is the twin's fake clock, or, on a real Redis, real time plus what the schedule
// advanced: a real master's TIME does not move with it, so a held apply is aged through the writers' test
// hook and a pass is told its start.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
)

// Names the harness's mode: "run" or "keep". Anything else starts nothing.
const testRedisClusterHarnessEnvName = "SIDECAR_TEST_REDIS_CLUSTER_HARNESS"

// What the harness builds its cluster from.
type testRedisClusterSettings struct {
	// The image, which must already be on this machine: §22.4's Redis 8.10.2.
	Image string
	// What every container's name begins with; the brief's sidecar-test- prefix.
	NamePrefix string
	// The masters: 3 (§22.4), each on its own loopback port from BasePort up, above 50000.
	MasterCount int
	BasePort    int
	// The masters' cluster bus ports, from here up, inside the shared namespace only.
	BusBasePort int
	// How long the masters wait before they call one failed: 2 s, so a killed master is seen quickly.
	NodeTimeout time.Duration
	// How long a start or a restart may take to reach a cluster every master calls ok: 60 s.
	ReadyTimeout time.Duration
	// How often readiness is asked again: 100 ms.
	ReadyPollInterval time.Duration
	// The longest one docker command may take: 60 s.
	CommandTimeout time.Duration
}

func defaultTestRedisClusterSettings() *testRedisClusterSettings {
	return &testRedisClusterSettings{
		Image:             "redis:8.10.2",
		NamePrefix:        "sidecar-test-redis-cluster",
		MasterCount:       3,
		BasePort:          58731,
		BusBasePort:       48731,
		NodeTimeout:       2 * time.Second,
		ReadyTimeout:      60 * time.Second,
		ReadyPollInterval: 100 * time.Millisecond,
		CommandTimeout:    60 * time.Second,
	}
}

// Refuses a cluster the brief does not allow: a name outside sidecar-test-, or a port at or under 50000
// or one a production data tunnel may hold.
func (self *testRedisClusterSettings) validate() error {
	if !strings.HasPrefix(self.NamePrefix, "sidecar-test-") {
		return fmt.Errorf("redis cluster harness: container names must begin sidecar-test-, not %q", self.NamePrefix)
	}
	if self.MasterCount < 3 {
		return fmt.Errorf("redis cluster harness: %d masters, under a cluster's 3", self.MasterCount)
	}
	for index := range self.MasterCount {
		port := self.BasePort + index
		if port <= 50000 || 65535 < port || server.Testing_LoopbackTunnelPort(uint16(port)) {
			return fmt.Errorf("redis cluster harness: port %d is not above 50000 or is a production data tunnel port", port)
		}
	}
	if self.Image == "" || self.NodeTimeout <= 0 || self.ReadyTimeout <= 0 || self.ReadyPollInterval <= 0 || self.CommandTimeout <= 0 {
		return errors.New("redis cluster harness: settings out of range")
	}
	return nil
}

// One master of the harness's cluster.
type testRedisClusterMaster struct {
	index int
	port  int
	// its own container, in the shared namespace
	container string
}

// host:port, as the cluster announces it.
func (self *testRedisClusterMaster) address() string {
	return "127.0.0.1:" + strconv.Itoa(self.port)
}

// A master's data as it stood at one save, held inside its container.
type testRedisClusterDump struct {
	master *testRedisClusterMaster
	path   string
}

// The harness's cluster. Not safe for concurrent use.
type testRedisCluster struct {
	t        testing.TB
	settings *testRedisClusterSettings
	masters  []*testRedisClusterMaster
	// the container that holds the namespace and the published ports
	namespaceContainer string
	dumpCount          int
}

// One docker command, its output or its error with what it printed.
func (self *testRedisCluster) docker(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), self.settings.CommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return stdout.String(), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// The same, failing the test on an error.
func (self *testRedisCluster) mustDocker(args ...string) string {
	self.t.Helper()
	output, err := self.docker(args...)
	if err != nil {
		self.t.Fatal(err)
	}
	return output
}

// redis-cli inside master's container, on its port. A reply that is a Redis error is an error here.
func (self *testRedisCluster) cli(master *testRedisClusterMaster, args ...string) (string, error) {
	output, err := self.docker(append([]string{"exec", master.container, "redis-cli", "-p", strconv.Itoa(master.port)}, args...)...)
	if err != nil {
		return output, err
	}
	output = strings.TrimRight(output, "\r\n")
	if strings.HasPrefix(output, "ERR ") || strings.HasPrefix(output, "CROSSSLOT ") || strings.HasPrefix(output, "CLUSTERDOWN ") || strings.HasPrefix(output, "MOVED ") {
		return output, fmt.Errorf("redis-cli %s: %s", strings.Join(args, " "), output)
	}
	return output, nil
}

// The same, failing the test on an error.
func (self *testRedisCluster) mustCli(master *testRedisClusterMaster, args ...string) string {
	self.t.Helper()
	output, err := self.cli(master, args...)
	if err != nil {
		self.t.Fatal(err)
	}
	return output
}

// The names of the harness's containers that exist, running or not.
func (self *testRedisCluster) existingContainers() []string {
	self.t.Helper()
	output := self.mustDocker("ps", "-a", "--filter", "name=^"+self.settings.NamePrefix+"-", "--format", "{{.Names}}")
	return strings.Fields(output)
}

// Whether every container of the cluster is running.
func (self *testRedisCluster) running() bool {
	self.t.Helper()
	output := self.mustDocker("ps", "--filter", "name=^"+self.settings.NamePrefix+"-", "--format", "{{.Names}}")
	names := strings.Fields(output)
	if !slices.Contains(names, self.namespaceContainer) {
		return false
	}
	for _, master := range self.masters {
		if !slices.Contains(names, master.container) {
			return false
		}
	}
	return true
}

// The harness over settings, nil being the defaults, with nothing started.
func newTestRedisCluster(t testing.TB, settings *testRedisClusterSettings) *testRedisCluster {
	t.Helper()
	if settings == nil {
		settings = defaultTestRedisClusterSettings()
	}
	if err := settings.validate(); err != nil {
		t.Fatal(err)
	}
	self := &testRedisCluster{
		t:                  t,
		settings:           settings,
		namespaceContainer: settings.NamePrefix + "-net",
	}
	for index := range settings.MasterCount {
		self.masters = append(self.masters, &testRedisClusterMaster{
			index:     index,
			port:      settings.BasePort + index,
			container: settings.NamePrefix + "-" + strconv.Itoa(index),
		})
	}
	return self
}

// Starts the cluster: the namespace container with the masters' ports published on 127.0.0.1, each
// master in it, then the slots shared out among them. Containers a broken run left under these names are
// removed first. Returns once every master calls the cluster ok.
func (self *testRedisCluster) up() {
	self.t.Helper()
	self.remove()
	if _, err := self.docker("image", "inspect", "--format", "{{.Id}}", self.settings.Image); err != nil {
		self.t.Fatalf("redis cluster harness: %s is not on this machine, and the harness pulls nothing: %v", self.settings.Image, err)
	}
	// tail keeps the namespace up, whichever master is down
	args := []string{"run", "-d", "--name", self.namespaceContainer}
	for _, master := range self.masters {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", master.port, master.port))
	}
	args = append(args, "--entrypoint", "tail", self.settings.Image, "-f", "/dev/null")
	self.mustDocker(args...)
	for _, master := range self.masters {
		self.mustDocker(
			"run", "-d", "--name", master.container, "--network", "container:"+self.namespaceContainer,
			self.settings.Image, "redis-server",
			"--port", strconv.Itoa(master.port),
			"--cluster-port", strconv.Itoa(self.settings.BusBasePort+master.index),
			"--bind", "127.0.0.1",
			"--protected-mode", "no",
			"--cluster-enabled", "yes",
			"--cluster-config-file", "nodes.conf",
			"--cluster-node-timeout", strconv.FormatInt(self.settings.NodeTimeout.Milliseconds(), 10),
			// the masters that are up keep serving their slots while one is down
			"--cluster-require-full-coverage", "no",
			// no persistence but the dump the harness stages
			"--save", "",
			"--appendonly", "no",
			"--dir", "/data",
			"--dbfilename", "dump.rdb",
		)
	}
	self.awaitAnswering()
	create := []string{"exec", self.masters[0].container, "redis-cli", "-p", strconv.Itoa(self.masters[0].port), "--cluster", "create"}
	for _, master := range self.masters {
		create = append(create, master.address())
	}
	create = append(create, "--cluster-replicas", "0", "--cluster-yes")
	self.mustDocker(create...)
	self.awaitReady()
}

// Stops and removes every container of the cluster that exists.
func (self *testRedisCluster) remove() {
	self.t.Helper()
	if names := self.existingContainers(); 0 < len(names) {
		self.mustDocker(append([]string{"rm", "-f", "-v"}, names...)...)
	}
}

// Waits, asking every ReadyPollInterval, until ready answers nil or ReadyTimeout passes, which fails the
// test with ready's last answer. The one wait on real time here: a container's start has no event to
// wait on.
func (self *testRedisCluster) await(what string, ready func() error) {
	self.t.Helper()
	deadline := time.Now().Add(self.settings.ReadyTimeout)
	for {
		err := ready()
		if err == nil {
			return
		}
		if !time.Now().Before(deadline) {
			self.t.Fatalf("redis cluster harness: %s: %v", what, err)
		}
		<-time.After(self.settings.ReadyPollInterval)
	}
}

// Returns once every master answers PING.
func (self *testRedisCluster) awaitAnswering() {
	self.t.Helper()
	self.await("the masters do not answer", func() error {
		for _, master := range self.masters {
			if output, err := self.cli(master, "ping"); err != nil || output != "PONG" {
				return fmt.Errorf("%s: %q %v", master.container, output, err)
			}
		}
		return nil
	})
}

// Returns once every master calls the cluster ok, knows every master and calls none failed, and the
// slots are all assigned.
func (self *testRedisCluster) awaitReady() {
	self.t.Helper()
	self.await("the cluster is not ready", func() error {
		for _, master := range self.masters {
			info, err := self.cli(master, "cluster", "info")
			if err != nil {
				return fmt.Errorf("%s: %v", master.container, err)
			}
			for _, want := range []string{
				"cluster_state:ok",
				"cluster_slots_assigned:" + strconv.Itoa(server.RedisSlotCount),
				"cluster_slots_fail:0",
				"cluster_slots_pfail:0",
				"cluster_known_nodes:" + strconv.Itoa(len(self.masters)),
			} {
				if !slices.Contains(strings.Fields(info), want) {
					return fmt.Errorf("%s: no %s yet", master.container, want)
				}
			}
			nodes, err := self.cli(master, "cluster", "nodes")
			if err != nil {
				return fmt.Errorf("%s: %v", master.container, err)
			}
			if strings.Contains(nodes, "fail") || strings.Contains(nodes, "disconnected") || strings.Contains(nodes, "handshake") {
				return fmt.Errorf("%s still sees a master down", master.container)
			}
		}
		return nil
	})
}

// The master serving slot, by what the first master that answers says of the cluster's nodes.
func (self *testRedisCluster) masterOf(slot int) *testRedisClusterMaster {
	self.t.Helper()
	var nodes string
	var err error
	for _, master := range self.masters {
		if nodes, err = self.cli(master, "cluster", "nodes"); err == nil {
			break
		}
	}
	if err != nil {
		self.t.Fatal(err)
	}
	for _, line := range strings.Split(nodes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		address, _, _ := strings.Cut(fields[1], "@")
		for _, slotRange := range fields[8:] {
			if strings.HasPrefix(slotRange, "[") {
				// a slot in migration, which the harness never starts
				continue
			}
			startText, endText, ranged := strings.Cut(slotRange, "-")
			if !ranged {
				endText = startText
			}
			start, startErr := strconv.Atoi(startText)
			end, endErr := strconv.Atoi(endText)
			if startErr != nil || endErr != nil || slot < start || end < slot {
				continue
			}
			for _, master := range self.masters {
				if master.address() == address {
					return master
				}
			}
		}
	}
	self.t.Fatalf("redis cluster harness: no master serves slot %d", slot)
	return nil
}

// master's run_id, random at each start of its process.
func (self *testRedisCluster) runId(master *testRedisClusterMaster) string {
	self.t.Helper()
	info := self.mustCli(master, "info", "server")
	for _, line := range strings.Split(info, "\n") {
		if runId, found := strings.CutPrefix(strings.TrimSpace(line), "run_id:"); found && runId != "" {
			return runId
		}
	}
	self.t.Fatalf("redis cluster harness: %s has no run_id", master.container)
	return ""
}

// Saves master's data as it stands and sets the dump aside in its container, for a later kill to stage.
func (self *testRedisCluster) dump(master *testRedisClusterMaster) *testRedisClusterDump {
	self.t.Helper()
	if output := self.mustCli(master, "save"); output != "OK" {
		self.t.Fatalf("redis cluster harness: SAVE on %s: %s", master.container, output)
	}
	self.dumpCount += 1
	held := &testRedisClusterDump{master: master, path: "/data/held-" + strconv.Itoa(self.dumpCount) + ".rdb"}
	// as the server's own user, so the restarted server can read it
	self.mustDocker("exec", "-u", "redis", master.container, "mv", "/data/dump.rdb", held.path)
	return held
}

// Kills master with no chance to save, leaving on its disk the dump it loads at its next start: held,
// which must be its own, or none, so that it comes back empty.
func (self *testRedisCluster) kill(master *testRedisClusterMaster, held *testRedisClusterDump) {
	self.t.Helper()
	self.mustDocker("exec", "-u", "redis", master.container, "rm", "-f", "/data/dump.rdb")
	if held != nil {
		if held.master != master {
			self.t.Fatalf("redis cluster harness: a dump of %s for %s", held.master.container, master.container)
		}
		self.mustDocker("exec", "-u", "redis", master.container, "cp", held.path, "/data/dump.rdb")
	}
	self.mustDocker("kill", "--signal", "KILL", master.container)
}

// Starts a killed master again, from what its kill left on its disk, and returns once the cluster is
// ready with it.
func (self *testRedisCluster) start(master *testRedisClusterMaster) {
	self.t.Helper()
	self.mustDocker("start", master.container)
	self.awaitAnswering()
	self.awaitReady()
}

// The harness's own proof (SDK-M8-12's Provides): a 3-master cluster comes up on loopback ports above
// 50000; a command across two masters' slots is refused (WK1's ground); one master killed and restarted
// from an older dump comes back with a new run_id holding the dump's data, not what was written since,
// and one restarted with no dump comes back empty, each serving its slots again while the other masters
// kept their data and their run_ids throughout; and nothing of the cluster is left behind. Runs only
// where SIDECAR_TEST_REDIS_CLUSTER_HARNESS is "run" or "keep".
func TestRedisClusterHarnessKillsAndRestartsOneMaster(t *testing.T) {
	mode := os.Getenv(testRedisClusterHarnessEnvName)
	if mode != "run" && mode != "keep" {
		t.Skipf("%s is not run or keep: the harness starts no containers", testRedisClusterHarnessEnvName)
	}
	cluster := newTestRedisCluster(t, nil)
	cluster.up()
	if mode == "run" {
		t.Cleanup(func() {
			cluster.remove()
			if names := cluster.existingContainers(); len(names) != 0 {
				t.Errorf("containers left behind: %v", names)
			}
		})
	}
	if !cluster.running() {
		t.Fatal("the cluster's containers are not all running")
	}

	// three keys, each on another master, found by their slots
	keys := map[*testRedisClusterMaster]string{}
	for n := 0; len(keys) < len(cluster.masters); n += 1 {
		key := "{harness_" + strconv.Itoa(n) + "}value"
		master := cluster.masterOf(server.RedisHashSlot(key))
		if _, taken := keys[master]; !taken {
			keys[master] = key
		}
	}
	first, second, third := cluster.masters[0], cluster.masters[1], cluster.masters[2]
	runIds := map[*testRedisClusterMaster]string{}
	for master, key := range keys {
		cluster.mustCli(master, "set", key, "older")
		runIds[master] = cluster.runId(master)
	}

	// WK1's ground: one script naming keys of two masters is refused, and writes nothing
	if output, err := cluster.cli(first, "eval", "return redis.call('SET', KEYS[1], 'x') and redis.call('SET', KEYS[2], 'x')", "2", keys[first], keys[second]); err == nil || !strings.Contains(output, "CROSSSLOT") {
		t.Fatalf("a script across two masters: %q %v", output, err)
	}
	if value := cluster.mustCli(first, "get", keys[first]); value != "older" {
		t.Fatalf("the refused script wrote %q", value)
	}

	// the first master: a dump, then newer data, then a kill and a restart from the dump
	held := cluster.dump(first)
	cluster.mustCli(first, "set", keys[first], "newer")
	sinceKey := strings.Replace(keys[first], "}value", "}since", 1)
	cluster.mustCli(first, "set", sinceKey, "written-since-the-dump")
	cluster.kill(first, held)
	// the others serve their own slots meanwhile
	if value := cluster.mustCli(second, "get", keys[second]); value != "older" {
		t.Fatalf("the second master while the first is down: %q", value)
	}
	cluster.start(first)
	if value := cluster.mustCli(first, "get", keys[first]); value != "older" {
		t.Fatalf("after a restart from the older dump: %q", value)
	}
	if value := cluster.mustCli(first, "exists", sinceKey); value != "0" {
		t.Fatalf("a key written since the dump came back: %s", value)
	}
	if runId := cluster.runId(first); runId == runIds[first] {
		t.Fatal("the restarted master kept its run_id")
	}
	if master := cluster.masterOf(server.RedisHashSlot(keys[first])); master != first {
		t.Fatalf("the slot moved to %s", master.container)
	}
	// what the pass lists a slot by, on the restarted master's own connection
	if listed := cluster.mustCli(first, "cluster", "getkeysinslot", strconv.Itoa(server.RedisHashSlot(keys[first])), "10"); listed != keys[first] {
		t.Fatalf("the slot's keys after the restart: %q", listed)
	}

	// the second master: a kill and a restart with no dump
	cluster.kill(second, nil)
	cluster.start(second)
	if value := cluster.mustCli(second, "exists", keys[second]); value != "0" {
		t.Fatalf("after an empty restart the key is there: %s", value)
	}
	if runId := cluster.runId(second); runId == runIds[second] {
		t.Fatal("the restarted master kept its run_id")
	}
	if master := cluster.masterOf(server.RedisHashSlot(keys[second])); master != second {
		t.Fatalf("the slot moved to %s", master.container)
	}

	// the third master was never touched
	if value := cluster.mustCli(third, "get", keys[third]); value != "older" {
		t.Fatalf("the untouched master: %q", value)
	}
	if runId := cluster.runId(third); runId != runIds[third] {
		t.Fatal("the untouched master's run_id changed")
	}
	if mode == "keep" {
		addresses := []string{}
		for _, master := range cluster.masters {
			cluster.mustCli(master, "flushall")
			addresses = append(addresses, master.address())
		}
		t.Logf("the cluster stays up at %s; docker rm -f -v %s removes it", strings.Join(addresses, ","), strings.Join(cluster.existingContainers(), " "))
	}
}

// The schedule's ground: see the file's comment. Not safe for concurrent use, but for now and aged,
// which a held writer's hook reads.
type testTokenGround struct {
	t   testing.TB
	ctx context.Context
	// twin, redis or cluster
	name string
	// T2's cluster, nil on a real Redis
	twin *testPushTokenCluster
	// the harness, where the environment's Redis is its cluster; nil otherwise
	cluster  *testRedisCluster
	store    pushTokenStore
	topology pushTokenTopology
	// the slots the schedule's clients live in, the lowest, which a pass reconciles first
	slots []int
	// what the pass carries from one run to the next
	passState    *PushTokenPassState
	passSettings *SidecarPushTokenPassSettings

	// what the schedule's clock has moved past the primary's and a real master's, in nanoseconds
	skew atomic.Int64
	// each row's revision when settle last moved it on to the schedule's clock
	settledRevisions map[server.Id]int64
}

// How many of the lowest slots a ground's schedule uses, and a pass on a real Redis reconciles at most.
const testTokenGroundSlotCount = 8

// The grounds this environment gives, each prepared: the twin always, and the environment's Redis, as
// the cluster where it is the harness's.
func testTokenGrounds(t testing.TB, ctx context.Context) []*testTokenGround {
	t.Helper()
	return []*testTokenGround{newTestTokenTwinGround(t, ctx), newTestTokenRedisGround(t, ctx)}
}

func testTokenGroundSlots() []int {
	slots := make([]int, testTokenGroundSlotCount)
	for slot := range slots {
		slots[slot] = slot
	}
	return slots
}

// The twin: a master serving the schedule's slots, whose sentinels name it, and the writers' applies
// routed to the fake for the test.
func newTestTokenTwinGround(t testing.TB, ctx context.Context) *testTokenGround {
	t.Helper()
	twin := newTestPushTokenCluster(t, time.Now())
	self := &testTokenGround{
		t:                t,
		ctx:              ctx,
		name:             "twin",
		twin:             twin,
		store:            twin.store(),
		topology:         twin,
		slots:            testTokenGroundSlots(),
		passSettings:     DefaultSidecarPushTokenPassSettings(),
		settledRevisions: map[server.Id]int64{},
	}
	twin.twoMasters(t, self.slots)
	self.passState = &PushTokenPassState{LastCompletedStartTime: self.now()}
	return self
}

// The environment's Redis: the sentinels of the schedule's slots set to the masters serving them, as a
// reconcile leaves them, so the schedule starts where WK5's premise holds. Every other slot stays found
// from the first pass on, and a pass reconciles only the lowest, the schedule's.
func newTestTokenRedisGround(t testing.TB, ctx context.Context) *testTokenGround {
	t.Helper()
	store, topology := NewPushTokenServerRedis(ctx)
	self := &testTokenGround{
		t:                t,
		ctx:              ctx,
		name:             "redis",
		store:            store,
		topology:         topology,
		slots:            testTokenGroundSlots(),
		passSettings:     DefaultSidecarPushTokenPassSettings(),
		settledRevisions: map[server.Id]int64{},
	}
	self.passSettings.ReconcileSlotCount = testTokenGroundSlotCount
	masters, err := topology.Masters(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, clustered := redisClientOf(ctx).(*redis.ClusterClient); clustered {
		if harness := attachTestRedisCluster(t, masters); harness != nil {
			self.name = "cluster"
			self.cluster = harness
		}
	}
	for _, slot := range self.slots {
		self.setSentinel(slot, self.masterOf(masters, slot).RunId)
	}
	self.passState = &PushTokenPassState{LastCompletedStartTime: self.now()}
	return self
}

// The harness over the environment's cluster, when its masters are exactly the harness's containers,
// running; nil for any other cluster, whose masters no test may kill.
func attachTestRedisCluster(t testing.TB, masters []*PushTokenMaster) *testRedisCluster {
	t.Helper()
	harness := newTestRedisCluster(t, nil)
	if len(masters) != len(harness.masters) {
		return nil
	}
	for _, master := range masters {
		if !slices.ContainsFunc(harness.masters, func(own *testRedisClusterMaster) bool { return own.address() == master.Address }) {
			return nil
		}
	}
	if !harness.running() {
		return nil
	}
	return harness
}

// The master of masters that serves slot.
func (self *testTokenGround) masterOf(masters []*PushTokenMaster, slot int) *PushTokenMaster {
	self.t.Helper()
	for _, master := range masters {
		if slices.Contains(master.Slots, slot) {
			return master
		}
	}
	self.t.Fatalf("%s: no master serves slot %d", self.name, slot)
	return nil
}

func (self *testTokenGround) setSentinel(slot int, runId string) {
	self.t.Helper()
	if err := self.store.SetPersistent(self.ctx, PushTokenSentinelKey(slot), []byte(runId)); err != nil {
		self.t.Fatal(err)
	}
}

// slot's sentinel, "" while it has none.
func (self *testTokenGround) sentinel(slot int) string {
	self.t.Helper()
	values, err := self.store.GetEach(self.ctx, []string{PushTokenSentinelKey(slot)})
	if err != nil {
		self.t.Fatal(err)
	}
	return string(values[0])
}

// Whether the ground can restart a master: the twin in its fake, the cluster through the harness.
func (self *testTokenGround) restarts() bool {
	return self.twin != nil || self.cluster != nil
}

// The schedule's clock.
func (self *testTokenGround) now() time.Time {
	if self.twin != nil {
		return self.twin.clock.Now()
	}
	return time.Now().Add(time.Duration(self.skew.Load()))
}

// How far the schedule's clock has moved past a real master's TIME: nothing on the twin, whose TIME is
// the clock.
func (self *testTokenGround) aged() time.Duration {
	if self.twin != nil {
		return 0
	}
	return time.Duration(self.skew.Load())
}

// Moves the schedule's clock on by d.
func (self *testTokenGround) advance(d time.Duration) {
	self.t.Helper()
	self.settle()
	if self.twin != nil {
		self.twin.clock.Advance(d)
	}
	self.skew.Add(int64(d))
}

// The primary's clock stands still under the schedule's, so a row is committed behind the schedule's now
// by all the schedule's clock has moved. Every row committed since the last call moves on to the schedule's
// clock, its update_time and removed_time as a primary on that clock would have written them, so that a
// pass's sweep finds what the schedule's last 2 min committed. Runs before the clock moves and before a
// pass.
func (self *testTokenGround) settle() {
	self.t.Helper()
	moved := time.Duration(self.skew.Load())
	server.Tx(self.ctx, func(tx server.PgTx) {
		revisions := map[server.Id]int64{}
		result, err := tx.Query(self.ctx, `SELECT client_id, rev FROM sidecar_push_token`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var rev int64
				server.Raise(result.Scan(&clientId, &rev))
				revisions[clientId] = rev
			}
		})
		for clientId, rev := range revisions {
			if settled, ok := self.settledRevisions[clientId]; ok && settled == rev {
				continue
			}
			self.settledRevisions[clientId] = rev
			if moved == 0 {
				continue
			}
			server.RaisePgResult(tx.Exec(
				self.ctx,
				`
					UPDATE sidecar_push_token
					SET
						update_time = update_time + $2 * interval '1 microsecond',
						removed_time = removed_time + $2 * interval '1 microsecond'
					WHERE client_id = $1
				`,
				clientId,
				moved.Microseconds(),
			))
		}
	})
}

// A new client id in the n-th of the schedule's slots.
func (self *testTokenGround) clientInSlot(n int) server.Id {
	return testPushTokenClientInSlot(self.slots[n])
}

// A new active client of a new network, with no row, in the n-th of the schedule's slots.
func (self *testTokenGround) newClient(n int) server.Id {
	self.t.Helper()
	networkId := server.NewId()
	clientId := self.clientInSlot(n)
	Testing_CreateNetwork(self.ctx, networkId, testPushTokenNetworkName(networkId), server.NewId())
	Testing_CreateDevice(self.ctx, networkId, server.NewId(), clientId, "", "")
	return clientId
}

// What clientId's tok and tokrev hold, "" for absent.
func (self *testTokenGround) tokenKeys(clientId server.Id) (tok string, tokrev string) {
	self.t.Helper()
	values, err := self.store.GetEach(self.ctx, []string{
		pushservice.HintTokenKey(connect.Id(clientId)),
		pushservice.WakeTokenRevisionKey(connect.Id(clientId)),
	})
	if err != nil {
		self.t.Fatal(err)
	}
	return string(values[0]), string(values[1])
}

// What tok holds for row: its platform and token, "" for a tombstone or no row.
func testTokenValue(row *SidecarPushTokenRow) string {
	if row == nil || row.Tombstone() {
		return ""
	}
	return string(pushservice.WakeTokenValue(row.Platform, row.Token))
}

// Fails unless clientId's tok equals its row as committed now, the newest, at that row's revision: WK5's
// "tok equals the newest row (absent for a tombstone)". A client with no row has neither key.
func (self *testTokenGround) expectNewest(clientId server.Id, when string) *SidecarPushTokenRow {
	self.t.Helper()
	row, err := GetPushToken(self.ctx, clientId)
	if err != nil {
		self.t.Fatal(err)
	}
	wantRev := ""
	if row != nil {
		wantRev = strconv.FormatInt(row.Rev, 10)
	}
	if tok, tokrev := self.tokenKeys(clientId); tok != testTokenValue(row) || tokrev != wantRev {
		self.t.Fatalf("%s, %s: tok %q at tokrev %q, and the newest row is %s", self.name, when, tok, tokrev, row)
	}
	return row
}

// One pass at the schedule's now, from the state the last left. On a real Redis each pass has its own
// store and topology, as taskworker's has: one node's keys are listed by one SCAN a topology.
func (self *testTokenGround) pass() *PushTokenPassReport {
	self.t.Helper()
	if self.twin == nil {
		self.store, self.topology = NewPushTokenServerRedis(self.ctx)
	}
	self.settle()
	next, report, err := RunPushTokenPass(self.ctx, self.passState, self.now(), true, self.store, self.topology, self.passSettings, nil)
	if err != nil {
		self.t.Fatalf("%s: pass: %v", self.name, err)
	}
	self.passState = next
	return report
}

// Routes the writers' applies to the ground: to the twin's fake, or, on a real Redis, where they already
// go. Returns what undoes it.
func (self *testTokenGround) routeWriters() (restore func()) {
	if self.twin == nil {
		return func() {}
	}
	process := sidecarPushTokenRedis
	sidecarPushTokenRedis = func() pushservice.HintRedis { return self.twin.redis }
	return func() { sidecarPushTokenRedis = process }
}

// slot loses its sentinel, as a slot does that a master newly serves: the next pass finds it.
func (self *testTokenGround) loseSentinel(slot int) {
	self.t.Helper()
	if err := self.store.Delete(self.ctx, []string{PushTokenSentinelKey(slot)}); err != nil {
		self.t.Fatal(err)
	}
}

// Every key is lost, as in a flush or a site's loss: the twin's fake emptied, or the test's own
// database of the environment's Redis, every master's on a cluster.
func (self *testTokenGround) flush() {
	self.t.Helper()
	if self.twin != nil {
		self.twin.redis.LoseEverything()
		return
	}
	var err error
	switch client := redisClientOf(self.ctx).(type) {
	case *redis.ClusterClient:
		err = client.ForEachMaster(self.ctx, func(ctx context.Context, master *redis.Client) error {
			return master.FlushDB(ctx).Err()
		})
	default:
		err = client.FlushDB(self.ctx).Err()
	}
	if err != nil {
		self.t.Fatal(err)
	}
}

// Deletes every token row, so one ground's rows are not the next's.
func (self *testTokenGround) clearRows() {
	self.t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `DELETE FROM sidecar_push_token`))
	})
}

// What a master held at one dump of the twin: the keys of its slots.
type testTokenDump struct {
	keyValues map[string][]byte
	held      *testRedisClusterDump
}

// The dump of the master serving the schedule's slots, as it stands.
func (self *testTokenGround) dump() *testTokenDump {
	self.t.Helper()
	if self.twin != nil {
		keyValues := map[string][]byte{}
		for key, value := range self.twin.redis.Dump() {
			if slices.Contains(self.slots, server.RedisHashSlot(key)) {
				keyValues[key] = value
			}
		}
		return &testTokenDump{keyValues: keyValues}
	}
	if self.cluster == nil {
		self.t.Fatalf("%s: no master to dump", self.name)
	}
	return &testTokenDump{held: self.cluster.dump(self.cluster.masterOf(self.slots[0]))}
}

// The master serving the schedule's slots is killed and restarts, from dump or, with none, empty: it
// comes back with a new run_id, holding what the dump held.
func (self *testTokenGround) restart(dump *testTokenDump) {
	self.t.Helper()
	if self.twin != nil {
		master := self.twin.master("192.0.2.11:7000")
		self.twin.restart(master.Address, master.RunId+"+", nil)
		if dump != nil {
			for key, value := range dump.keyValues {
				if err := self.twin.redis.Set(self.ctx, key, value, 0); err != nil {
					self.t.Fatal(err)
				}
			}
		}
		return
	}
	if self.cluster == nil {
		self.t.Fatalf("%s: no master to restart", self.name)
	}
	master := self.cluster.masterOf(self.slots[0])
	var held *testRedisClusterDump
	if dump != nil {
		held = dump.held
	}
	self.cluster.kill(master, held)
	self.cluster.start(master)
}

// Says plainly that a row's master restart did not run on a real cluster here.
func (self *testTokenGround) logNoRestart(row string) {
	self.t.Helper()
	self.t.Logf("%s on a real master's restart did not run: the test environment's Redis is one node every track shares, and server/test-env.sh attests no cluster's masters (QD-028); the twin's restart ran", row)
}

// T3's cluster rows wait: the test environment attests one Redis endpoint, so no test process may dial
// the harness's three masters until server/test-env.sh takes QD-028's list of authorities. This test
// skips while that holds and says so; once the environment's Redis is the harness's cluster it checks
// the ground FS-36's and FS-120's rows then run on: 3 masters serving every slot, each with a run_id.
func TestTokenGroundOnThe3MasterCluster(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ground := newTestTokenRedisGround(t, ctx)
		if ground.cluster == nil {
			t.Skip("FS-36's and FS-120's rows on T3's 3-master cluster did not run: the test environment's Redis is not the harness's cluster, since server/test-env.sh attests one Redis endpoint (QD-028); they ran on T2's twin over Postgres and on the environment's one node")
		}
		masters, err := ground.topology.Masters(ctx)
		if err != nil {
			t.Fatal(err)
		}
		served := map[int]bool{}
		for _, master := range masters {
			if master.RunId == "" {
				t.Fatalf("%s has no run_id", master.Address)
			}
			for _, slot := range master.Slots {
				served[slot] = true
			}
		}
		if len(masters) != 3 || len(served) != server.RedisSlotCount {
			t.Fatalf("%d masters serving %d slots", len(masters), len(served))
		}
	})
}
