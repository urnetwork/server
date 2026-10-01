package model

import (
	"context"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Local, optional source-load comparison. It does not establish production
// capacity or activate a reader; request safety and fallback have separate tests.
type readerCostHook struct {
	mu              sync.Mutex
	active          bool
	commands, bytes int
}

func (h *readerCostHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *readerCostHook) begin() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active, h.commands, h.bytes = true, 0, 0
}
func (h *readerCostHook) end() (commands, bytes int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active = false
	return h.commands, h.bytes
}
func (h *readerCostHook) observe(command redis.Cmder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active {
		return
	}
	h.commands++
	switch command := command.(type) {
	case *redis.StringCmd:
		value, _ := command.Bytes()
		h.bytes += len(value)
	case *redis.SliceCmd:
		values, _ := command.Result()
		for _, value := range values {
			if value, ok := value.(string); ok {
				h.bytes += len(value)
			}
		}
	}
}
func (h *readerCostHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		err := next(ctx, command)
		h.observe(command)
		return err
	}
}
func (h *readerCostHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		err := next(ctx, commands)
		for _, command := range commands {
			h.observe(command)
		}
		return err
	}
}

func TestClientScoreReaderSourceCosts(t *testing.T) {
	if os.Getenv("FP2_READER_BENCHMARK") != "1" {
		t.Skip("optional local source-load benchmark")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, nativeCount := range []int{10_000, 20, 0} {
			location, caller := server.NewId(), server.NewId()
			union, natives := map[ipFamilyFacet][]*ClientScore{}, map[ipFamilyFacet][]*ClientScore{}
			for i := range 10_000 {
				score := onlineBackfillScore(true, 1)
				if i < nativeCount {
					score = nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
					natives[ipFamilyFacetV4Only] = append(natives[ipFamilyFacetV4Only], score)
				}
				union[ipFamilyFacetV4Only] = append(union[ipFamilyFacetV4Only], score)
			}
			hook := &readerCostHook{}
			server.Redis(t.Context(), func(r server.RedisClient) {
				r.AddHook(hook)
				facets := nativeTestFacets(t, union)
				pipe := r.Pipeline()
				for _, facet := range ipFamilyFacets {
					payload := facets[facet]
					pipe.Set(t.Context(), clientScoreLocationFacetCountsKey(false, RankModeQuality, location, server.Id{}, facet), payload.countsBytes, time.Hour)
					for index := range payload.counts {
						pipe.Set(t.Context(), clientScoreLocationFacetSampleKey(false, RankModeQuality, location, server.Id{}, facet, index), payload.encodeSample(index), time.Hour)
					}
				}
				pipe.Set(t.Context(), clientScoreLocationAliasKey(false, RankModeQuality, location, caller), clientScoreAliasBaselineValue, time.Hour)
				_, err := pipe.Exec(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
				if err := writeClientScoreNativeSnapshot(t.Context(), r, key, time.Hour, nativeTestFacets(t, natives)); err != nil {
					t.Fatal(err)
				}
				server.Raise(r.Set(t.Context(), clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, caller)), clientScoreNativeBaseline, time.Hour).Err())
			})
			for _, enabled := range []bool{false, true} {
				durations := []time.Duration{}
				totalBytes, totalCommands, totalRows := 0, 0, 0
				for run := range 23 {
					hook.begin()
					started := time.Now()
					scores, _, err := loadPreferredClientScoresWithCursor(enabled, false, RankModeQuality, t.Context(), map[server.Id]bool{location: true}, nil, caller, 1000, []ipFamilyFacet{ipFamilyFacetV4Only})
					elapsed := time.Since(started)
					commands, bytes := hook.end()
					if err != nil {
						t.Fatal(err)
					}
					want := 1000
					if enabled {
						want = min(1000, nativeCount)
					}
					if len(scores) != want {
						t.Fatalf("source returned %d rows, want %d", len(scores), want)
					}
					if run < 3 {
						continue
					}
					durations = append(durations, elapsed)
					totalBytes += bytes
					totalCommands += commands
					totalRows += len(scores)
				}
				slices.Sort(durations)
				t.Logf("native=%t native_population=%d union_population=10000 mean_rows=%d mean_commands=%d mean_payload_bytes=%d p50_ms=%.3f p95_ms=%.3f", enabled, nativeCount, totalRows/20, totalCommands/20, totalBytes/20, float64(durations[10])/float64(time.Millisecond), float64(durations[18])/float64(time.Millisecond))
			}
		}
	})
}
