package server

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestRedisCommandTtlWarning covers the two unit-conversion signatures: a raw
// `time.Duration` surviving into command args (serialized as nanoseconds),
// and expiry-bearing commands whose effective ttl exceeds the 120-day limit.
func TestRedisCommandTtlWarning(t *testing.T) {
	now := time.Now()
	nanosecondsLeak := int64(8 * time.Hour / time.Nanosecond)

	warns := func(args ...any) bool {
		return redisCommandTtlWarning(args, now) != ""
	}

	// healthy commands
	connect.AssertEqual(t, warns("expire", "k", int64(28800)), false)
	connect.AssertEqual(t, warns("pexpire", "k", int64(28800000)), false)
	connect.AssertEqual(t, warns("setex", "k", int64(60), "v"), false)
	connect.AssertEqual(t, warns("set", "k", "v", "ex", int64(3600)), false)
	connect.AssertEqual(t, warns("set", "k", "v", "px", int64(3600000)), false)
	connect.AssertEqual(t, warns("expireat", "k", now.Add(time.Hour).Unix()), false)
	connect.AssertEqual(t, warns("pexpireat", "k", now.Add(time.Hour).UnixMilli()), false)
	connect.AssertEqual(t, warns("getex", "k", "ex", int64(60)), false)
	connect.AssertEqual(t, warns("get", "k"), false)
	connect.AssertEqual(t, warns("eval", "script", 1, "k", int64(28800)), false)
	// a 30-day ttl is long but sane
	connect.AssertEqual(t, warns("expire", "k", int64(30*24*3600)), false)

	// the historical stream-key leak: the 8h ttl in nanoseconds
	connect.AssertEqual(t, warns("expire", "k", nanosecondsLeak), true)
	connect.AssertEqual(t, warns("pexpire", "k", nanosecondsLeak), true)
	connect.AssertEqual(t, warns("set", "k", "v", "ex", nanosecondsLeak), true)
	connect.AssertEqual(t, warns("set", "k", "v", "px", nanosecondsLeak), true)
	connect.AssertEqual(t, warns("getex", "k", "px", nanosecondsLeak), true)
	connect.AssertEqual(t, warns("setex", "k", nanosecondsLeak, "v"), true)
	connect.AssertEqual(t, warns("expireat", "k", now.Unix()+nanosecondsLeak), true)
	connect.AssertEqual(t, warns("pexpireat", "k", now.UnixMilli()+nanosecondsLeak), true)

	// string-typed args (raw Do passthrough) parse too
	connect.AssertEqual(t, warns("expire", "k", "28800000000000"), true)
	connect.AssertEqual(t, warns("expire", "k", "28800"), false)

	// a raw time.Duration in ANY position is the exact original mistake —
	// the eval arg that go-redis wrote as nanoseconds
	connect.AssertEqual(t, warns("eval", "script", 1, "k", 8*time.Hour), true)
	connect.AssertEqual(t, warns("expire", "k", 8*time.Hour), true)

	// boundary: exactly the limit is allowed, one second past is not
	limitSeconds := int64(redisTtlWarnLimit / time.Second)
	connect.AssertEqual(t, warns("expire", "k", limitSeconds), false)
	connect.AssertEqual(t, warns("expire", "k", limitSeconds+1), true)
}
