package server

// redis ttl sanity warnings. A ttl beyond `redisTtlWarnLimit` (120 days) is
// treated as a unit-conversion mistake and logged — nothing in the system
// intentionally keeps a longer redis ttl. The signature this guards against
// (found 2026-07-20): a Go `time.Duration` passed as a raw command/eval arg
// is serialized by go-redis as its int64 NANOSECONDS, so a Lua
// `redis.call('EXPIRE', key, ttl)` fed the 8h stream ttl became
// `EXPIRE <key> 28800000000000` (~913,000 years) and ~1.1M orphaned stream
// keys never aged out.
//
// Two checks, on every command (including each command of a pipeline):
//  1. any arg that is still a `time.Duration` — the typed command builders
//     always convert durations before the args reach the wire, so a Duration
//     surviving into the args is a raw Do/Eval passthrough about to be
//     written as nanoseconds;
//  2. expiry-bearing commands whose effective ttl exceeds the limit.
//
// Warnings are rate-limited per command name. Detection only — the command
// is never blocked (refusing writes on a heuristic would turn a log bug into
// an outage).

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"context"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/glog/v2026"
)

const redisTtlWarnLimit = 120 * 24 * time.Hour

type redisTtlWarnHook struct{}

func (self redisTtlWarnHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self redisTtlWarnHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		warnRedisCommandTtl(cmd)
		return next(ctx, cmd)
	}
}

func (self redisTtlWarnHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			warnRedisCommandTtl(cmd)
		}
		return next(ctx, cmds)
	}
}

var redisTtlWarnState = struct {
	stateLock sync.Mutex
	// command name -> last warning time
	lastWarnTimes map[string]time.Time
}{
	lastWarnTimes: map[string]time.Time{},
}

func warnRedisCommandTtl(cmd redis.Cmder) {
	warning := redisCommandTtlWarning(cmd.Args(), time.Now())
	if warning == "" {
		return
	}

	name := strings.ToLower(cmd.Name())
	warn := func() bool {
		redisTtlWarnState.stateLock.Lock()
		defer redisTtlWarnState.stateLock.Unlock()
		now := time.Now()
		if lastWarnTime, ok := redisTtlWarnState.lastWarnTimes[name]; ok && now.Sub(lastWarnTime) < time.Minute {
			return false
		}
		redisTtlWarnState.lastWarnTimes[name] = now
		return true
	}()
	if warn {
		glog.Warningf("[redis][ttl]%s\n", warning)
	}
}

// redisCommandTtlWarning returns a description of a suspect ttl in the
// command args, or "" when the command looks sane
func redisCommandTtlWarning(args []any, now time.Time) string {
	if len(args) == 0 {
		return ""
	}
	name := strings.ToLower(redisArgString(args[0]))

	for i, arg := range args {
		if d, ok := arg.(time.Duration); ok {
			return fmt.Sprintf(
				"raw time.Duration arg %d (%s) on %q key=%q — go-redis serializes this as int64 nanoseconds; convert to seconds/ms first",
				i, d, name, redisFirstKey(args),
			)
		}
	}

	limitSeconds := int64(redisTtlWarnLimit / time.Second)
	limitMs := int64(redisTtlWarnLimit / time.Millisecond)
	nowSeconds := now.Unix()
	nowMs := now.UnixMilli()

	suspect := func(unit string, value int64, limit int64) string {
		if value <= limit {
			return ""
		}
		return fmt.Sprintf(
			"%q key=%q ttl %d%s exceeds %s — likely a unit conversion issue",
			name, redisFirstKey(args), value, unit, redisTtlWarnLimit,
		)
	}

	switch name {
	case "expire", "setex":
		if v, ok := redisArgInt64(args, 2); ok {
			return suspect("s", v, limitSeconds)
		}
	case "pexpire", "psetex", "restore":
		if v, ok := redisArgInt64(args, 2); ok {
			return suspect("ms", v, limitMs)
		}
	case "expireat":
		if v, ok := redisArgInt64(args, 2); ok {
			return suspect("s-from-now", v-nowSeconds, limitSeconds)
		}
	case "pexpireat":
		if v, ok := redisArgInt64(args, 2); ok {
			return suspect("ms-from-now", v-nowMs, limitMs)
		}
	case "set", "getex":
		for i := 1; i < len(args)-1; i += 1 {
			switch strings.ToLower(redisArgString(args[i])) {
			case "ex":
				if v, ok := redisArgInt64(args, i+1); ok {
					return suspect("s", v, limitSeconds)
				}
			case "px":
				if v, ok := redisArgInt64(args, i+1); ok {
					return suspect("ms", v, limitMs)
				}
			case "exat":
				if v, ok := redisArgInt64(args, i+1); ok {
					return suspect("s-from-now", v-nowSeconds, limitSeconds)
				}
			case "pxat":
				if v, ok := redisArgInt64(args, i+1); ok {
					return suspect("ms-from-now", v-nowMs, limitMs)
				}
			}
		}
	}
	return ""
}

func redisFirstKey(args []any) string {
	if len(args) < 2 {
		return ""
	}
	key := redisArgString(args[1])
	if 80 < len(key) {
		key = key[:80]
	}
	return key
}

func redisArgString(arg any) string {
	switch v := arg.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func redisArgInt64(args []any, i int) (int64, bool) {
	if len(args) <= i {
		return 0, false
	}
	switch v := args[i].(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case uint64:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
