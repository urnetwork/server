package monitor

// SIGNALS.md §3.2 maps to signal_redis_buffers.go and signal_redis_buffers_test.go.
func NewRedisBuffersSignal() Signal {
	return &signalAdapter{
		number: "3.2", key: "redis-buffers", name: "Redis dataset versus client buffers",
		probe:  newRedisMemoryProbe("redis/client-buffers"),
		accept: acceptProbeIDs("redis/client-buffers"),
	}
}
