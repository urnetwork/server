//go:build linux || darwin

package server

// go-redis over the census's sockets (redis_timer_census.go), against the
// local Redis: the pool's sockets are the census's, its per-checkout socket
// check still peeks through their syscall.Conn, and each command's write and
// read deadline and each check's clear is counted once.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisTimerCensusSocketsKeepGoRedisSocketCheck(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		production := redisConnCheckProductionOptions(t)
		key := fmt.Sprintf("cpuopt7_c1_%d", time.Now().UnixNano())
		Redis(ctx, func(client RedisClient) {
			Raise(client.Set(ctx, key, "v", time.Minute).Err())
		})
		get := func(client *redis.Client) {
			t.Helper()
			value, err := client.Get(ctx, key).Result()
			if err != nil || value != "v" {
				t.Fatalf("GET: %q, %v", value, err)
			}
		}

		census := startTestTimerCensus(t, 1)
		// the pools' own dial, under a dialer that counts each socket's checks;
		// a pool without the window checks every checkout
		dialer := &redisConnCheckDialer{dial: production.Dialer}
		client := newRedisConnCheckClient(production, dialer, redisSkipConnCheckWithin(-1), -1)
		defer client.Close()
		// dials and initializes the connection
		get(client)
		conn := dialer.dialed()[0]
		if _, ok := conn.Conn.(*redisTimerCensusSyscallConn); !ok {
			t.Fatalf("the pool's socket is %T, not the census's", conn.Conn)
		}
		checks := conn.checks.Load()
		beforeRead := census.counts(TimerCensusSiteRedisReadDeadline)
		beforeWrite := census.counts(TimerCensusSiteRedisWriteDeadline)
		const getCount = 5
		for range getCount {
			get(client)
		}
		if checked := conn.checks.Load() - checks; checked != getCount {
			t.Fatalf("%d of %d checkouts checked the socket through the census's syscall.Conn", checked, getCount)
		}
		// each command's deadline is 5 s out, the client's timeouts, and each
		// checkout's check clears both
		census.requireCounts(t, "warm GETs", TimerCensusSiteRedisReadDeadline, beforeRead, map[string]float64{"1s_10s": getCount, "stop": getCount})
		census.requireCounts(t, "warm GETs", TimerCensusSiteRedisWriteDeadline, beforeWrite, map[string]float64{"1s_10s": getCount, "stop": getCount})
	})
}
