package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// Records the COUNT of every HSCAN a context carrying it sends; other
// contexts' commands pass through untouched.
type redisScanCountRecorderKey struct{}

type redisScanCountRecorder struct {
	mutex  sync.Mutex
	counts []int64
}

func (self *redisScanCountRecorder) record(count int64) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.counts = append(self.counts, count)
}

func (self *redisScanCountRecorder) recorded() []int64 {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]int64{}, self.counts...)
}

type redisScanCountHook struct{}

func (redisScanCountHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (redisScanCountHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if recorder, ok := ctx.Value(redisScanCountRecorderKey{}).(*redisScanCountRecorder); ok && cmd.Name() == "hscan" {
			args := cmd.Args()
			for i := 0; i+1 < len(args); i++ {
				if name, ok := args[i].(string); ok && strings.EqualFold(name, "count") {
					if count, ok := args[i+1].(int64); ok {
						recorder.record(count)
					}
				}
			}
		}
		return next(ctx, cmd)
	}
}

func (redisScanCountHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// Each drain step holds the owning node for its whole reply, so a drain pages
// a closed hash in steps of at most 1000 fields. Steps of 5000 held a node for
// 15-26 ms on 2026-10-25.
func TestRedisDrainsScanBoundedSteps(t *testing.T) {
	const maxStepFields = 1000
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		recorder := &redisScanCountRecorder{}
		ctx := context.WithValue(t.Context(), redisScanCountRecorderKey{}, recorder)
		server.Redis(ctx, func(r server.RedisClient) {
			r.AddHook(redisScanCountHook{})
		})

		// a closed block of each drain
		now := server.NowUtc().Add(time.Hour)
		RollupClientReliabilityStats(ctx, now)
		drainClientDataUsageShard(ctx, reliabilityBlockNumber(now)-2, 0)

		counts := recorder.recorded()
		if len(counts) == 0 {
			t.Fatal("no drain step was observed")
		}
		for _, count := range counts {
			if maxStepFields < count {
				t.Fatalf("a drain step asked for %d fields, bound %d", count, maxStepFields)
			}
		}
	})
}
