package server

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type redisCommandProbeCountedConn struct {
	net.Conn
	bytes *atomic.Int64
}

func (c *redisCommandProbeCountedConn) Write(data []byte) (int, error) {
	n, err := c.Conn.Write(data)
	c.bytes.Add(int64(n))
	return n, err
}

// Fixed worker cohorts start together over real go-redis RESP connections.
// Report command ownership, wire bytes and throughput; no wall-time threshold
// is a correctness assertion and no artificial sleep supplies the result.
func BenchmarkRedisCommandProbeCallbacks(b *testing.B) {
	for _, workers := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("workers_%d", workers), func(b *testing.B) {
			peer := &deadlineRedisPeer{stop: make(chan struct{})}
			defer peer.close()
			options := deadlineTestRedisOptions(peer)
			options.PoolSize, options.MaxActiveConns = workers, workers
			var wireBytes atomic.Int64
			options.Dialer = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := peer.dial(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return &redisCommandProbeCountedConn{Conn: conn, bytes: &wireBytes}, nil
			}
			client := redis.NewClient(options)
			defer client.Close()
			ctx := b.Context()
			if err := client.Ping(ctx).Err(); err != nil {
				b.Fatal(err)
			}
			pool := &safeRedisClient{client: client}
			var next atomic.Int64
			var ready, joined sync.WaitGroup
			ready.Add(workers)
			joined.Add(workers)
			start := make(chan struct{})
			errors := make(chan any, workers)
			for range workers {
				go func() {
					defer joined.Done()
					ready.Done()
					<-start
					if recovered := redisCommandProbePanic(func() {
						for next.Add(1) <= int64(b.N) {
							redisWithClient(ctx, pool, func(r RedisClient) {
								Raise(r.Set(ctx, "loaded-command", "value", 0).Err())
							}, OptNoRetry())
						}
					}); recovered != nil {
						errors <- recovered
					}
				}()
			}
			ready.Wait()
			beforePing, beforeWork, beforeBytes := peer.count("ping"), peer.count("set"), wireBytes.Load()
			b.ResetTimer()
			began := time.Now()
			close(start)
			joined.Wait()
			elapsed := time.Since(began)
			b.StopTimer()
			close(errors)
			for err := range errors {
				b.Fatal(err)
			}
			work := peer.count("set") - beforeWork
			if work != int64(b.N) {
				b.Fatalf("loaded callback ownership changed: work=%d want=%d", work, b.N)
			}
			b.ReportMetric(float64(peer.count("ping")-beforePing)/float64(work), "PING/work")
			b.ReportMetric(float64(wireBytes.Load()-beforeBytes)/float64(work), "wire-B/work")
			b.ReportMetric(float64(work)/elapsed.Seconds(), "work/s")
			b.ReportMetric(float64(work), "work-commands")
			b.ReportMetric(float64(peer.dials.Load()), "connections")
		})
	}
}
