package model

import (
	"context"
	"sync"

	"github.com/redis/go-redis/v9"
)

type legacyHotpathRedisCountKey struct{}

// A benchmark opts in through its context, so setup, assertions and unrelated
// owners do not contaminate its counts. Task execution and detached posts keep
// context values. Counts are client hook commands and dispatches, not inferred
// network round trips on a cluster with multiple destinations.
type legacyHotpathRedisCountHook struct {
	mu         sync.Mutex
	commands   map[string]int64
	dispatches int64
}

func (h *legacyHotpathRedisCountHook) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, legacyHotpathRedisCountKey{}, h)
}

func (h *legacyHotpathRedisCountHook) add(ctx context.Context, commands []redis.Cmder) {
	if ctx.Value(legacyHotpathRedisCountKey{}) != h {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.commands == nil {
		h.commands = map[string]int64{}
	}
	h.dispatches++
	for _, command := range commands {
		h.commands[command.Name()]++
	}
}

func (h *legacyHotpathRedisCountHook) snapshot() (map[string]int64, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	commands := make(map[string]int64, len(h.commands))
	for name, count := range h.commands {
		commands[name] = count
	}
	return commands, h.dispatches
}

func (h *legacyHotpathRedisCountHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *legacyHotpathRedisCountHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		h.add(ctx, []redis.Cmder{command})
		return next(ctx, command)
	}
}

func (h *legacyHotpathRedisCountHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		h.add(ctx, commands)
		return next(ctx, commands)
	}
}
