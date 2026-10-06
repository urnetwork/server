package monitor

// SIGNALS.md §3.6 maps to signal_redis_topology.go and signal_redis_topology_test.go.
func NewRedisTopologySignal() Signal {
	return &signalAdapter{number: "3.6", key: "redis-topology", name: "Redis cluster topology hygiene", probe: redisTopologyProbe{}}
}
