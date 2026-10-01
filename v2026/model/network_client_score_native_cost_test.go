// Local synthetic costs are work/transport controls, not production CPU attribution.
package model

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Counts the actual writer's bounded transport work; used by one test goroutine.
type nativeCostRedis struct {
	server.RedisClient
	t                   testing.TB
	encodedPages        int
	writtenPages        int
	writeBatches        int
	writeBytes          int
	maxBatchPages       int
	maxBatchBytes       int
	maxEncodedAhead     int
	beginCalls          int
	commitCalls         int
	commitPages         int
	commitManifestBytes int
	readPages           int
	readBytes           int
	maxReadBatchPages   int
	seenReads           map[string]bool
}

// Observes protocol-sized page batches without changing Redis execution.
func (self *nativeCostRedis) Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	switch script {
	case clientScoreNativeBeginScript:
		self.beginCalls++
	case clientScoreNativePagesScript:
		pages, size := (len(args)-3)/2, 0
		for index := 3; index < len(args); index += 2 {
			size += len(args[index].(string)) + len(args[index+1].([]byte))
		}
		if pages > clientScoreExportBatchSize || size > clientScoreExportBatchBytes {
			self.t.Fatalf("native writer exceeded bounded batch: pages=%d bytes=%d", pages, size)
		}
		self.maxBatchPages = max(self.maxBatchPages, pages)
		self.maxBatchBytes = max(self.maxBatchBytes, size)
		self.writtenPages += pages
		self.writeBatches++
		self.writeBytes += size
	case clientScoreNativeCommitScript:
		self.commitCalls++
		self.commitPages = (len(args) - 5) / 2
		self.commitManifestBytes = len(args[3].([]byte))
	default:
		self.t.Fatalf("unexpected native writer script")
	}
	return self.RedisClient.Eval(ctx, script, keys, args...)
}

// Wraps reads without changing command scheduling or server responses.
func (self *nativeCostRedis) Pipeline() redis.Pipeliner {
	return &nativeCostPipeline{Pipeliner: self.RedisClient.Pipeline(), audit: self}
}

// Checks page read uniqueness and bounded command counts on actual responses.
type nativeCostPipeline struct {
	redis.Pipeliner
	audit *nativeCostRedis
}

// Measures the completed batch, not speculative queued work.
func (self *nativeCostPipeline) Exec(ctx context.Context) ([]redis.Cmder, error) {
	commands, err := self.Pipeliner.Exec(ctx)
	if err != nil {
		return commands, err
	}
	self.audit.maxReadBatchPages = max(self.audit.maxReadBatchPages, len(commands))
	for _, command := range commands {
		args := command.Args()
		if command.Name() != "hmget" || len(args) != 4 || args[2] != "g" {
			self.audit.t.Fatalf("unexpected native page read: %s", command.Name())
		}
		key := fmt.Sprint(args[1], ":", args[3])
		if self.audit.seenReads[key] {
			self.audit.t.Fatalf("native refill reread page %s", key)
		}
		self.audit.seenReads[key] = true
		values, err := command.(*redis.SliceCmd).Result()
		if err != nil || len(values) != 2 {
			self.audit.t.Fatalf("native page read result: %v", err)
		}
		self.audit.readPages++
		self.audit.readBytes += len(values[1].(string))
	}
	return commands, nil
}

// One 100k-native target has 500 bounded pages, not 100k Redis keys or one
// unbounded payload. A normal 20-provider request consumes only its existing
// 1000-row initial sample. Worst-case post-filter exhaustion visits every page
// exactly once, in bounded read batches. This intentionally measures the
// actual publisher/cursor against local Redis, not the SQL source query, API
// end-to-end latency, or a complete multi-location publication pass.
func TestNativeCost100kPublisherAndCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const providerCount = 100_000
		const pageCount = providerCount / ClientScoreSampleCount
		ctx := t.Context()
		location := server.NewId()
		key := clientScoreNativeKey(clientScoreLocationCountsKey(false, RankModeQuality, location, server.Id{}))
		network := server.NewId()
		validUntil := server.NowUtc().Add(time.Hour)
		counts := make([]int, pageCount)
		for index := range counts {
			counts[index] = ClientScoreSampleCount
		}
		server.Redis(ctx, func(r server.RedisClient) {
			audit := &nativeCostRedis{RedisClient: r, t: t, seenReads: map[string]bool{}}
			facets := map[ipFamilyFacet]clientScoreFacetPayload{}
			for _, facet := range ipFamilyFacets {
				facets[facet] = clientScoreFacetPayload{counts: []int{}}
			}
			facets[ipFamilyFacetV4Only] = clientScoreFacetPayload{
				counts: counts,
				encodeSample: func(index int) []byte {
					page := make([]*ClientScore, ClientScoreSampleCount)
					for offset := range page {
						id := server.Id{}
						binary.BigEndian.PutUint64(id[8:], uint64(index*ClientScoreSampleCount+offset+1))
						page[offset] = &ClientScore{
							ClientId: id, NetworkId: network, Online: true,
							IpFamilies: ClientScoreIpFamilyV4, EgressValidUntil: &validUntil,
							ReliabilityWeight: 1, IndependentReliabilityWeight: 1,
							PassesMinimums: map[RankMode]bool{RankModeQuality: true},
							Scores:         map[RankMode]int{RankModeQuality: 0, RankModeSpeed: 0},
							Tiers:          map[RankMode]int{RankModeQuality: 0, RankModeSpeed: 0},
							ScaledWeights:  map[RankMode]float32{RankModeQuality: 1, RankModeSpeed: 1},
						}
					}
					audit.encodedPages++
					audit.maxEncodedAhead = max(audit.maxEncodedAhead, audit.encodedPages-audit.writtenPages)
					return gobEncodeForTest(t, page)
				},
			}
			var before, published, consumed runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			if err := writeClientScoreNativeSnapshot(ctx, audit, key, time.Hour, facets); err != nil {
				t.Fatal(err)
			}
			publishDuration := time.Since(start)
			runtime.ReadMemStats(&published)
			if audit.encodedPages != pageCount || audit.writtenPages != pageCount || audit.beginCalls != 1 || audit.commitCalls != 1 || audit.commitPages != pageCount {
				t.Fatalf("native publication work mismatch: encoded=%d written=%d begin=%d commit=%d commit-pages=%d", audit.encodedPages, audit.writtenPages, audit.beginCalls, audit.commitCalls, audit.commitPages)
			}
			if audit.maxEncodedAhead > clientScoreExportBatchSize+1 {
				t.Fatalf("native encoder retained an unbounded pending page set: %d", audit.maxEncodedAhead)
			}
			rawPointer, err := r.Get(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			pointer, err := parseClientScoreNativePointer(rawPointer)
			if err != nil {
				t.Fatal(err)
			}
			fields, err := r.HLen(ctx, clientScoreNativeSlotKey(key, pointer.slot)).Result()
			if err != nil || fields != pageCount+2 {
				t.Fatalf("native storage is not page-bounded: fields=%d err=%v", fields, err)
			}

			start = time.Now()
			initialRows := findProviders2LoadCount(20, 0)
			loaded, cursor, available, err := loadNativeClientScoresWithCursor(ctx, RankModeQuality,
				map[server.Id]bool{location: true}, nil, server.Id{}, initialRows,
				[]ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}, nil)
			initialDuration := time.Since(start)
			if err != nil || !available || len(loaded) != initialRows || cursor.readCount != initialRows || cursor.nextPage != initialRows/ClientScoreSampleCount || len(cursor.pages) != pageCount {
				t.Fatalf("100k market amplified initial native work: available=%t rows=%d cursor=%+v err=%v", available, len(loaded), cursor, err)
			}
			for _, page := range cursor.pages[:cursor.nextPage] {
				audit.seenReads[page.nativeKey+":"+page.key] = true
			}
			seen := make(map[server.Id]bool, providerCount)
			accept := func(scores map[server.Id]*ClientScore) {
				for id, score := range scores {
					ordinal := binary.BigEndian.Uint64(id[8:])
					if ordinal < 1 || providerCount < ordinal || seen[id] || !score.PassesMinimums[RankModeQuality] {
						t.Fatalf("native cursor changed or repeated identity %s", id)
					}
					seen[id] = true
				}
			}
			accept(loaded)
			start = time.Now()
			refillRows := findProviders2LoadCount(20, providerCount)
			for cursor.hasMore() {
				batch, err := cursor.readWithClient(ctx, audit, refillRows)
				if err != nil {
					t.Fatal(err)
				}
				if len(batch) > refillRows+ClientScoreSampleCount-1 {
					t.Fatalf("native refill batch grew without bound: rows=%d limit=%d", len(batch), refillRows)
				}
				accept(batch)
			}
			exhaustDuration := time.Since(start)
			runtime.ReadMemStats(&consumed)
			if len(seen) != providerCount || cursor.readCount != providerCount || len(audit.seenReads) != pageCount || audit.readPages != pageCount-initialRows/ClientScoreSampleCount {
				t.Fatalf("native source exhaustion is not exact: identities=%d rows=%d pages=%d", len(seen), cursor.readCount, len(audit.seenReads))
			}
			if audit.maxReadBatchPages > (refillRows+ClientScoreSampleCount-1)/ClientScoreSampleCount {
				t.Fatalf("native pipeline grew without bound: pages=%d", audit.maxReadBatchPages)
			}
			t.Logf("local synthetic native100k: write=%s initial=%s exhaust-remainder=%s encoded-pages=%d write-batches=%d write-bytes=%d max-write-pages=%d max-write-bytes=%d max-encoded-ahead=%d manifest-bytes=%d initial-rows=%d refill-max-pages=%d remainder-read-bytes=%d publish-total-alloc=%d read-total-alloc=%d; allocations are process counters, not owned heap peaks or Main CPU", publishDuration, initialDuration, exhaustDuration, audit.encodedPages, audit.writeBatches, audit.writeBytes, audit.maxBatchPages, audit.maxBatchBytes, audit.maxEncodedAhead, audit.commitManifestBytes, initialRows, audit.maxReadBatchPages, audit.readBytes, published.TotalAlloc-before.TotalAlloc, consumed.TotalAlloc-published.TotalAlloc)
		})
	})
}
