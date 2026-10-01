package monitor

// SIGNALS.md §3.5 maps to signal_redis_connections.go and signal_redis_connections_test.go.
func NewRedisConnectionsSignal() Signal {
	return &signalAdapter{
		number: "3.5", key: "redis-connections", name: "Redis connection shape", probe: newRedisMemoryProbe("redis/clients-spike"),
		accept: acceptProbeIDs("redis/clients-spike"),
	}
}
