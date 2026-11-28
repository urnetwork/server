package perf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// The harness's own view of the disposable cluster: safety checks, INFO
// snapshots and key memory. It has its own client, so its reads never take a
// turn from the pools the measured code paths use, and every dial goes through
// the test process's dial guard, which admits only attested endpoints.
type clusterAdmin struct {
	client        *redis.ClusterClient
	authority     string
	expectedNodes int
}

func guardedRedisDial(ctx context.Context, network string, address string) (net.Conn, error) {
	if err := refuseTunnelEndpoint(address); err != nil {
		return nil, err
	}
	return server.Testing_DialTestEnv(ctx, "redis", network, address)
}

func newClusterAdmin(config *perfConfig) *clusterAdmin {
	return &clusterAdmin{
		client: redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:        []string{config.clusterAuthority},
			Dialer:       guardedRedisDial,
			DialTimeout:  2 * time.Second,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
			PoolSize:     8,
			MaxRetries:   -1,
		}),
		authority:     config.clusterAuthority,
		expectedNodes: config.clusterNodes,
	}
}

func (self *clusterAdmin) close() {
	self.client.Close()
}

// Refuses anything but the empty disposable cluster run.sh started: its exact
// master count, no replicas, every node on a non-tunnel, non-loopback endpoint,
// and no key on any master. A tunnel to a live cluster fails these checks
// before the harness writes anything.
func (self *clusterAdmin) verifyDisposable(ctx context.Context) error {
	info, err := self.client.ClusterInfo(ctx).Result()
	if err != nil {
		return fmt.Errorf("cluster info: %w", err)
	}
	fields := parseInfo(info)
	if fields["cluster_state"] != "ok" {
		return fmt.Errorf("cluster_state is %q", fields["cluster_state"])
	}
	knownNodes, _ := strconv.Atoi(fields["cluster_known_nodes"])
	if knownNodes != self.expectedNodes {
		return fmt.Errorf("refusing a cluster of %d nodes; run.sh starts exactly %d", knownNodes, self.expectedNodes)
	}
	slots, err := self.client.ClusterSlots(ctx).Result()
	if err != nil {
		return fmt.Errorf("cluster slots: %w", err)
	}
	masters := map[string]bool{}
	for _, slot := range slots {
		if len(slot.Nodes) != 1 {
			return fmt.Errorf("slot range %d-%d has %d nodes; the disposable cluster has no replicas", slot.Start, slot.End, len(slot.Nodes))
		}
		if err := refuseTunnelEndpoint(slot.Nodes[0].Addr); err != nil {
			return fmt.Errorf("cluster node: %w", err)
		}
		masters[slot.Nodes[0].Addr] = true
	}
	if len(masters) != self.expectedNodes {
		return fmt.Errorf("%d masters own slots; expected %d", len(masters), self.expectedNodes)
	}
	var stored []string
	var storedLock sync.Mutex
	err = self.client.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		count, err := node.DBSize(ctx).Result()
		if err != nil {
			return err
		}
		if count != 0 {
			storedLock.Lock()
			stored = append(stored, fmt.Sprintf("%s holds %d keys", node.Options().Addr, count))
			storedLock.Unlock()
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("dbsize: %w", err)
	}
	if len(stored) != 0 {
		return fmt.Errorf("refusing a cluster that is not empty: %s", strings.Join(stored, "; "))
	}
	return nil
}

// Main's keyspace notification classes (xops redis.conf), so every script
// write pays the notification cost it pays in production.
func (self *clusterAdmin) configureLikeMain(ctx context.Context) error {
	return self.client.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		return node.ConfigSet(ctx, "notify-keyspace-events", "Kg$sx").Err()
	})
}

func (self *clusterAdmin) redisVersion(ctx context.Context) string {
	var version string
	var lock sync.Mutex
	self.client.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		info, err := node.Info(ctx, "server").Result()
		if err == nil {
			lock.Lock()
			version = parseInfo(info)["redis_version"]
			lock.Unlock()
		}
		return nil
	})
	return version
}

func parseInfo(info string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[key] = value
		}
	}
	return fields
}

type commandStat struct {
	Calls int64 `json:"calls"`
	Usec  int64 `json:"usec"`
}

type nodeSnapshot struct {
	Addr           string                  `json:"addr"`
	CpuSeconds     float64                 `json:"cpu_seconds"`
	CpuMainSeconds float64                 `json:"cpu_main_seconds"`
	UsedMemory     int64                   `json:"used_memory"`
	Keys           int64                   `json:"keys"`
	Evicted        int64                   `json:"evicted_keys"`
	Expired        int64                   `json:"expired_keys"`
	Commands       map[string]*commandStat `json:"-"`
	TotalCommands  int64                   `json:"total_commands"`
	Connections    int64                   `json:"total_connections_received"`
}

type clusterSnapshot struct {
	at    time.Time
	nodes map[string]*nodeSnapshot
}

func (self *clusterAdmin) snapshot(ctx context.Context) (*clusterSnapshot, error) {
	snapshot := &clusterSnapshot{at: time.Now(), nodes: map[string]*nodeSnapshot{}}
	var lock sync.Mutex
	err := self.client.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
		info, err := node.Info(ctx, "cpu", "memory", "commandstats", "stats", "keyspace").Result()
		if err != nil {
			return err
		}
		fields := parseInfo(info)
		number := func(key string) float64 {
			value, _ := strconv.ParseFloat(fields[key], 64)
			return value
		}
		integer := func(key string) int64 {
			value, _ := strconv.ParseInt(fields[key], 10, 64)
			return value
		}
		item := &nodeSnapshot{
			Addr:           node.Options().Addr,
			CpuSeconds:     number("used_cpu_user") + number("used_cpu_sys"),
			CpuMainSeconds: number("used_cpu_user_main_thread") + number("used_cpu_sys_main_thread"),
			UsedMemory:     integer("used_memory"),
			Evicted:        integer("evicted_keys"),
			Expired:        integer("expired_keys"),
			TotalCommands:  integer("total_commands_processed"),
			Connections:    integer("total_connections_received"),
			Commands:       map[string]*commandStat{},
		}
		for key, value := range fields {
			if name, ok := strings.CutPrefix(key, "cmdstat_"); ok {
				stat := &commandStat{}
				for _, part := range strings.Split(value, ",") {
					field, number, _ := strings.Cut(part, "=")
					switch field {
					case "calls":
						stat.Calls, _ = strconv.ParseInt(number, 10, 64)
					case "usec":
						stat.Usec, _ = strconv.ParseInt(number, 10, 64)
					}
				}
				item.Commands[name] = stat
			}
			if strings.HasPrefix(key, "db") {
				for _, part := range strings.Split(value, ",") {
					if field, number, _ := strings.Cut(part, "="); field == "keys" {
						count, _ := strconv.ParseInt(number, 10, 64)
						item.Keys += count
					}
				}
			}
		}
		lock.Lock()
		snapshot.nodes[item.Addr] = item
		lock.Unlock()
		return nil
	})
	return snapshot, err
}

// What the cluster spent between two snapshots.
type clusterCost struct {
	Wall           float64                 `json:"wall_seconds"`
	CpuSeconds     float64                 `json:"cpu_seconds"`
	CpuMainSeconds float64                 `json:"cpu_main_seconds"`
	MaxNodeCpu     float64                 `json:"max_node_cpu_seconds"`
	MaxNodeAddr    string                  `json:"max_node"`
	MemoryDelta    int64                   `json:"memory_delta_bytes"`
	UsedMemory     int64                   `json:"used_memory_bytes"`
	Keys           int64                   `json:"keys"`
	Evicted        int64                   `json:"evicted_keys"`
	TotalCommands  int64                   `json:"commands"`
	Connections    int64                   `json:"connections_received"`
	Commands       map[string]*commandStat `json:"commands_by_name"`
	PerNodeCpu     map[string]float64      `json:"per_node_cpu_seconds"`
}

func (self *clusterSnapshot) costSince(before *clusterSnapshot) *clusterCost {
	cost := &clusterCost{
		Wall:       self.at.Sub(before.at).Seconds(),
		Commands:   map[string]*commandStat{},
		PerNodeCpu: map[string]float64{},
	}
	for addr, after := range self.nodes {
		prior, ok := before.nodes[addr]
		if !ok {
			prior = &nodeSnapshot{Commands: map[string]*commandStat{}}
		}
		cpu := after.CpuSeconds - prior.CpuSeconds
		cost.CpuSeconds += cpu
		cost.CpuMainSeconds += after.CpuMainSeconds - prior.CpuMainSeconds
		cost.PerNodeCpu[addr] = cpu
		if cost.MaxNodeCpu < cpu {
			cost.MaxNodeCpu = cpu
			cost.MaxNodeAddr = addr
		}
		cost.MemoryDelta += after.UsedMemory - prior.UsedMemory
		cost.UsedMemory += after.UsedMemory
		cost.Keys += after.Keys
		cost.Evicted += after.Evicted - prior.Evicted
		cost.TotalCommands += after.TotalCommands - prior.TotalCommands
		cost.Connections += after.Connections - prior.Connections
		for name, stat := range after.Commands {
			priorStat := prior.Commands[name]
			if priorStat == nil {
				priorStat = &commandStat{}
			}
			delta := &commandStat{Calls: stat.Calls - priorStat.Calls, Usec: stat.Usec - priorStat.Usec}
			if delta.Calls == 0 {
				continue
			}
			sum := cost.Commands[name]
			if sum == nil {
				sum = &commandStat{}
				cost.Commands[name] = sum
			}
			sum.Calls += delta.Calls
			sum.Usec += delta.Usec
		}
	}
	// the harness's own reads are not the measured code's work
	for _, name := range []string{"info", "cluster|info", "cluster|slots", "cluster|nodes", "memory|usage", "dbsize", "cluster|getkeysinslot", "config|set", "config|get", "cluster|shards"} {
		if stat := cost.Commands[name]; stat != nil {
			cost.TotalCommands -= stat.Calls
		}
	}
	return cost
}

// Top commands by calls, for the report.
func (self *clusterCost) topCommands(count int) []string {
	names := make([]string, 0, len(self.Commands))
	for name := range self.Commands {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return self.Commands[names[i]].Calls > self.Commands[names[j]].Calls
	})
	if count < len(names) {
		names = names[:count]
	}
	out := []string{}
	for _, name := range names {
		stat := self.Commands[name]
		out = append(out, fmt.Sprintf("%s %d (%.1f µs)", name, stat.Calls, float64(stat.Usec)/float64(max(1, stat.Calls))))
	}
	return out
}

// The bytes a set of keys holds, by MEMORY USAGE with every element sampled.
// Absent keys count zero.
func (self *clusterAdmin) memoryUsage(ctx context.Context, keys []string) (int64, error) {
	var total int64
	for start := 0; start < len(keys); start += 512 {
		end := min(len(keys), start+512)
		commands, err := self.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, key := range keys[start:end] {
				pipe.MemoryUsage(ctx, key, 0)
			}
			return nil
		})
		if err != nil && !errors.Is(err, redis.Nil) {
			return 0, err
		}
		for _, command := range commands {
			value, err := command.(*redis.IntCmd).Result()
			if errors.Is(err, redis.Nil) {
				continue
			}
			if err != nil {
				return 0, err
			}
			total += value
		}
	}
	return total, nil
}

// Every key in a slot that has the prefix, from the slot's master.
func (self *clusterAdmin) keysWithPrefix(ctx context.Context, prefix string) ([]string, error) {
	slot := keySlot(prefix)
	node, err := self.client.MasterForKey(ctx, prefix)
	if err != nil {
		return nil, err
	}
	count, err := node.ClusterCountKeysInSlot(ctx, int(slot)).Result()
	if err != nil {
		return nil, err
	}
	keys, err := node.ClusterGetKeysInSlot(ctx, int(slot), int(count)).Result()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, key := range keys {
		if strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	return out, nil
}

// slot -> master address
func (self *clusterAdmin) slotOwners(ctx context.Context) ([]string, error) {
	slots, err := self.client.ClusterSlots(ctx).Result()
	if err != nil {
		return nil, err
	}
	owners := make([]string, 16384)
	for _, slot := range slots {
		for index := slot.Start; index <= slot.End; index++ {
			owners[index] = slot.Nodes[0].Addr
		}
	}
	return owners, nil
}

// Redis Cluster key slot: CRC16 (XMODEM) of the hash tag, modulo 16384.
func keySlot(key string) uint16 {
	if start := strings.IndexByte(key, '{'); 0 <= start {
		if end := strings.IndexByte(key[start+1:], '}'); 0 < end {
			key = key[start+1 : start+1+end]
		}
	}
	var crc uint16
	for i := 0; i < len(key); i++ {
		crc ^= uint16(key[i]) << 8
		for bit := 0; bit < 8; bit++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc % 16384
}
