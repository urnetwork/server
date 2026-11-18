package perf

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// TestSessionPerf drives the real session code paths at production-shaped
// rates against a disposable Redis Cluster and the local test database, and
// writes report.json and report.md to SESSION_PERF_OUT. Run it through
// run.sh, which starts and removes the cluster (README.md).
func TestSessionPerf(t *testing.T) {
	config, ok, err := loadPerfConfig()
	if !ok {
		t.Skip("SESSION_PERF_REDIS_CLUSTER names no disposable cluster; run session/perf/run.sh")
	}
	if err != nil {
		t.Fatal(err)
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		harness := startHarness(t, config)
		defer harness.stop()
		harness.run()
	})
}

type harness struct {
	t      testing.TB
	ctx    context.Context
	config *perfConfig
	admin  *clusterAdmin
	ops    *opRecorder
	report *perfReport
	// slot -> master address
	owners  []string
	restore []func()
	// the label each master carries in the report
	nodeNames map[string]string
	// built once, shared by the phases
	working *workingSet
	large   *largeNetwork
	// the cluster's CPU with no load
	idleCores     float64
	idleMainCores float64
}

func startHarness(t testing.TB, config *perfConfig) *harness {
	self := &harness{
		t:         t,
		ctx:       t.Context(),
		config:    config,
		ops:       newOpRecorder(),
		nodeNames: map[string]string{},
	}
	self.report = newPerfReport(config)
	self.admin = newClusterAdmin(config)
	self.restore = append(self.restore, self.admin.close)
	self.must(self.admin.verifyDisposable(self.ctx), "verify the disposable cluster")
	self.must(self.admin.configureLikeMain(self.ctx), "configure keyspace notifications")
	self.must(self.admin.configureSlowlog(self.ctx), "configure the slowlog")
	owners, err := self.admin.slotOwners(self.ctx)
	self.must(err, "read slot owners")
	self.owners = owners
	masters := []string{}
	for _, owner := range owners {
		if _, ok := self.nodeNames[owner]; !ok {
			self.nodeNames[owner] = ""
			masters = append(masters, owner)
		}
	}
	for index, master := range masters {
		self.nodeNames[master] = fmt.Sprintf("node%d", index+1)
	}
	self.report.Environment.RedisVersion = self.admin.redisVersion(self.ctx)
	self.report.Environment.RedisMasters = len(masters)

	// Every Redis pool of this process, the authentication pool included,
	// reopens against the disposable cluster. The test environment's own
	// Redis keeps only its per-test lease.
	pop := server.Vault.PushSimpleResource("redis.yml", []byte(fmt.Sprintf(
		"authority: %q\npassword: \"\"\ndb: 0\ncluster: true\n",
		config.clusterAuthority,
	)))
	// One test process does the fleet's work, so it gets an api process's
	// pools: Main's api Redis pools (config main/redis.yml: 256 a node, which
	// the authentication pool takes as its size) and the local configuration's
	// api PostgreSQL pool (64; the local server allows 512).
	popRedisConfig := server.Config.PushSimpleResource("redis.yml", []byte("min_connections: 8\nmax_connections: 256\n"))
	popDbConfig := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 16\nmax_connections: 64\n"))
	server.RedisReset()
	server.PgReset()
	self.report.Environment.Pools = "Redis 256 a node (authentication and main pools), PostgreSQL 64"
	self.restore = append(self.restore, func() {
		popDbConfig()
		popRedisConfig()
		pop()
		server.RedisReset()
		server.PgReset()
	})
	// Main trusts the revocation record (vault auth.yml, 2026-10-27), and the
	// harness measures session creation switched on.
	self.restore = append(self.restore, session.Testing_SetRevocationRecordEnabled(true))
	self.restore = append(self.restore, session.Testing_SetSessionCreationEnabled(true))
	// the hot network creates thousands of proxies; their caps are not the
	// subject
	self.restore = append(self.restore, model.Testing_LiftProxyClientLimits())

	self.verifyRouting()
	self.calibrateIdle()
	return self
}

// The cluster's CPU with no load (server cron, I/O threads), subtracted from
// every measured interval so a short run's per-operation cost is its own.
func (self *harness) calibrateIdle() {
	before, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	time.Sleep(5 * time.Second)
	after, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	cost := after.costSince(before)
	self.idleCores = cost.CpuSeconds / cost.Wall
	self.idleMainCores = cost.CpuMainSeconds / cost.Wall
	self.report.Environment.IdleRedisCores = self.idleCores
	self.logf("idle cluster CPU: %.4f cores (main threads %.4f)", self.idleCores, self.idleMainCores)
}

// The cost since before, less the idle cluster CPU over the interval.
func (self *harness) costSince(after, before *clusterSnapshot) *clusterCost {
	cost := after.costSince(before)
	cost.CpuSeconds = max(0, cost.CpuSeconds-self.idleCores*cost.Wall)
	cost.CpuMainSeconds = max(0, cost.CpuMainSeconds-self.idleMainCores*cost.Wall)
	idlePerNode := self.idleCores / float64(max(1, len(cost.PerNodeCpu)))
	cost.MaxNodeCpu = 0
	for addr, cpu := range cost.PerNodeCpu {
		cpu = max(0, cpu-idlePerNode*cost.Wall)
		cost.PerNodeCpu[addr] = cpu
		if cost.MaxNodeCpu < cpu {
			cost.MaxNodeCpu = cpu
			cost.MaxNodeAddr = addr
		}
	}
	return cost
}

// One authentication-pool read must land on the disposable cluster.
func (self *harness) verifyRouting() {
	before, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	for range 20 {
		self.must(session.CheckSession(self.ctx, server.NewId(), server.NewId()), "a session check on the cluster")
	}
	after, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	cost := after.costSince(before)
	if stat := cost.Commands["exists"]; stat == nil || stat.Calls < 20 {
		self.t.Fatalf("the authentication pool did not reach the disposable cluster: %v", cost.topCommands(8))
	}
}

func (self *harness) stop() {
	for index := len(self.restore) - 1; 0 <= index; index-- {
		self.restore[index]()
	}
}

func (self *harness) must(err error, what string) {
	if err != nil {
		self.t.Helper()
		self.t.Fatalf("%s: %v", what, err)
	}
}

func (self *harness) logf(format string, args ...any) {
	self.t.Logf("[perf %s] "+format, append([]any{time.Now().UTC().Format("15:04:05")}, args...)...)
}

func (self *harness) run() {
	started := time.Now()
	self.report.Environment.Started = started.UTC()
	self.logf("profile %s, cluster %s with %d masters, Redis %s", self.config.profile.name, self.config.clusterAuthority, self.report.Environment.RedisMasters, self.report.Environment.RedisVersion)
	phases := []struct {
		name string
		run  func()
	}{
		{"unit", self.runUnitPhase},
		{"capacity", self.runCapacityPhase},
		// the steady phase lists and hints the large network before the
		// storm revokes it
		{"steady", self.runSteadyPhase},
		{"storm", self.runStormPhase},
		{"signout", self.runSignOutPhase},
		{"outbox", self.runOutboxPhase},
	}
	for _, phase := range phases {
		if !self.config.runs(phase.name) {
			continue
		}
		if phase.name == "signout" && !self.config.profile.signOutAll && len(self.config.phases) == 0 {
			continue
		}
		phaseStart := time.Now()
		self.logf("phase %s", phase.name)
		phase.run()
		self.report.PhaseSeconds[phase.name] = time.Since(phaseStart).Seconds()
		self.logf("phase %s done in %s", phase.name, time.Since(phaseStart).Round(time.Second))
	}
	self.report.Environment.Seconds = time.Since(started).Seconds()
	self.finishReport()
}

// The cluster cost of fn, which makes ops calls of one kind.
func (self *harness) measure(name string, ops int, fn func()) *unitCost {
	before, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	start := time.Now()
	fn()
	wall := time.Since(start)
	after, err := self.admin.snapshot(self.ctx)
	self.must(err, "snapshot")
	cost := self.costSince(after, before)
	unit := &unitCost{Name: name, Ops: ops, WallSeconds: wall.Seconds(), Cost: cost}
	unit.derive()
	self.report.Units = append(self.report.Units, unit)
	self.logf("%s: %d ops in %s, Redis %.1f µs CPU/op (main %.1f), %.1f commands/op, %+d B", name, ops, wall.Round(time.Millisecond), unit.CpuMicrosPerOp, unit.CpuMainMicrosPerOp, unit.CommandsPerOp, cost.MemoryDelta)
	return unit
}

// The master that holds a key.
func (self *harness) nodeOf(key string) string {
	return self.nodeNames[self.owners[keySlot(key)]]
}

func (self *harness) networkSessionKey(networkId server.Id) string {
	return session.SessionKey(networkId, "z")
}

// The record key a credential's check reads.
func (self *harness) recordKey(claims *session.ByJwt) string {
	if claims.ClientId != nil && claims.RootClientId != nil {
		return session.Testing_RevocationRecordKey(claims.NetworkId, *claims.RootClientId)
	}
	if claims.SessionId != nil {
		return session.Testing_RevocationRecordKey(claims.NetworkId, *claims.SessionId)
	}
	return ""
}

// Bytes the network's session keys hold, and how many keys.
func (self *harness) networkSessionBytes(networkId server.Id) (int64, int) {
	keys, err := self.admin.keysWithPrefix(self.ctx, fmt.Sprintf("{ns_%s}", networkId))
	self.must(err, "list network session keys")
	bytes, err := self.admin.memoryUsage(self.ctx, keys)
	self.must(err, "memory usage")
	return bytes, len(keys)
}

// Bytes the network's revocation record shards hold.
func (self *harness) networkRecordBytes(networkId server.Id) int64 {
	bytes, err := self.admin.memoryUsage(self.ctx, session.Testing_RevocationRecordShardKeys(networkId))
	self.must(err, "memory usage")
	return bytes
}

func (self *harness) networkKeyBreakdown(networkId server.Id) map[string]int64 {
	keys, err := self.admin.keysWithPrefix(self.ctx, fmt.Sprintf("{ns_%s}", networkId))
	self.must(err, "list network session keys")
	prefix := fmt.Sprintf("{ns_%s}", networkId)
	groups := map[string][]string{}
	for _, key := range keys {
		suffix := strings.TrimPrefix(key, prefix)
		group := suffix
		if name, _, ok := strings.Cut(suffix, ":"); ok {
			group = name + ":"
		}
		groups[group] = append(groups[group], key)
	}
	out := map[string]int64{}
	for group, members := range groups {
		bytes, err := self.admin.memoryUsage(self.ctx, members)
		self.must(err, "memory usage")
		out[group] = bytes
		out[group+"#count"] = int64(len(members))
	}
	return out
}

func (self *harness) liveSessionCount(networkId server.Id) int64 {
	var count int64
	err := server.RedisAuth(self.ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		count, err = r.ZCount(ctx, session.SessionKey(networkId, "z"), fmt.Sprintf("(%d", time.Now().UnixMilli()), "+inf").Result()
		return err
	})
	self.must(err, "count live sessions")
	return count
}

func (self *harness) heldCount(networkId server.Id) int64 {
	var count int64
	err := server.RedisAuth(self.ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		count, err = r.ZCard(ctx, session.SessionKey(networkId, "held")).Result()
		return err
	})
	self.must(err, "count held sessions")
	return count
}

// Slow commands, for the node time one script holds.
func (self *clusterAdmin) configureSlowlog(ctx context.Context) error {
	return self.client.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		if err := node.ConfigSet(ctx, "slowlog-log-slower-than", "1000").Err(); err != nil {
			return err
		}
		return node.ConfigSet(ctx, "slowlog-max-len", "4096").Err()
	})
}

func (self *clusterAdmin) slowlogReset(ctx context.Context) error {
	return self.client.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		return node.Do(ctx, "SLOWLOG", "RESET").Err()
	})
}

type slowEntry struct {
	Node       string  `json:"node"`
	Ms         float64 `json:"ms"`
	Command    string  `json:"command"`
	ScriptHint string  `json:"script,omitempty"`
	Keys       int     `json:"keys,omitempty"`
}

func (self *clusterAdmin) slowlog(ctx context.Context, names map[string]string) ([]*slowEntry, error) {
	entries := []*slowEntry{}
	var lock sync.Mutex
	err := self.client.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		logs, err := node.SlowLogGet(ctx, 4096).Result()
		if err != nil {
			return err
		}
		lock.Lock()
		defer lock.Unlock()
		for _, log := range logs {
			entry := &slowEntry{Node: names[node.Options().Addr], Ms: float64(log.Duration.Microseconds()) / 1000}
			if 0 < len(log.Args) {
				entry.Command = strings.ToLower(log.Args[0])
			}
			if entry.Command == "eval" && 2 < len(log.Args) {
				fmt.Sscan(log.Args[2], &entry.Keys)
				fifthKey := ""
				if 7 < len(log.Args) {
					fifthKey = log.Args[7]
				}
				entry.ScriptHint = scriptName(log.Args[1], entry.Keys, fifthKey)
			}
			entries = append(entries, entry)
		}
		return nil
	})
	return entries, err
}

// Names a session script. The slowlog keeps only the first 128 bytes of each
// argument, and every network session script starts with the same common
// prelude, so those are told apart by their key count: mint 8, inventory and
// review 4, list 4+3n and revoke 5+3n for n sessions. A revoke of one session
// also has 8 keys; its fifth key is the operation receipt, a mint's is the
// session's metadata.
func scriptName(text string, keys int, fifthKey string) string {
	switch {
	case strings.HasPrefix(text, "\nfor i=3,#ARGV do redis.call('HSET',KEYS[1],ARGV[i],'x')"):
		return "record_tombstone"
	case strings.HasPrefix(text, "\nlocal t=redis.call('TIME')"):
		return "record_projection"
	case strings.HasPrefix(text, "\nif ARGV[1]=='del' then"):
		return "record_network"
	case !strings.HasPrefix(text, "\nlocal now=tonumber(ARGV[1])"):
		return "other"
	case keys == 3:
		return "last_use_sample"
	case keys == 8 && strings.Contains(fifthKey, "}op:"):
		return "revoke"
	case keys == 8:
		return "mint"
	case keys == 4:
		return "inventory_or_review"
	case 4 < keys && (keys-4)%3 == 0:
		return "list"
	case 5 < keys && (keys-5)%3 == 0:
		return "revoke"
	}
	return "session_other"
}

// What a test machine is, for the report.
func machineDescription() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s/%s, %d logical CPUs, host %s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), filepath.Base(host))
}
