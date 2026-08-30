package monitor

// Signal redis-memory implements SIGNALS.md §3.1. The shared Redis snapshot
// is filtered to node memory pressure and cross-node skew.
func NewRedisMemorySignal() Signal {
	return &signalAdapter{
		number: "3.1", key: "redis-memory", name: "Redis per-node memory table",
		probe:  newRedisMemoryProbe("redis/node-mem-critical", "redis/node-mem-high"),
		accept: acceptProbeIDs("redis/node-mem-critical", "redis/node-mem-high", "redis/mem-skew", "redis/node-mem"),
	}
}
