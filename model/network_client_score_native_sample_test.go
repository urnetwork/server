package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

func speedSampleID(n uint64) server.Id {
	var id server.Id
	binary.BigEndian.PutUint64(id[8:], n+1)
	return id
}

func speedSampleCensus() *ClientScoreNativeCensus {
	started := time.Date(2026, 10, 5, 22, 0, 0, 0, time.UTC)
	return &ClientScoreNativeCensus{SchemaVersion: 1, PublicationId: "01", SourceStartedAt: started,
		SourceCompletedAt: started.Add(time.Minute), PublishedAt: started.Add(3 * time.Minute), PolicyVersion: 1}
}

func speedSampleFixture(n int) (*ClientScoreNativeCensus, map[server.Id]bool, map[server.Id]bool, map[server.Id]ProviderEgressHealthCounts) {
	c := speedSampleCensus()
	speed, quality := map[server.Id]bool{}, map[server.Id]bool{}
	health := map[server.Id]ProviderEgressHealthCounts{}
	for i := range n {
		id := speedSampleID(uint64(i))
		speed[id] = true
		health[id] = ProviderEgressHealthCounts{OKCount: 4, Total: 5}
	}
	return c, speed, quality, health
}

func TestNativeSpeedSampleBoundedHashSelection(t *testing.T) {
	c, speed, quality, health := speedSampleFixture(80000)
	for i := range 2000 {
		quality[speedSampleID(uint64(i))] = true
	}
	before := time.Now()
	s := newClientScoreNativeSpeedSample(c, speed, quality, health)
	elapsed := time.Since(before)
	if s.PopulationCount != 78000 || len(s.Members) != 256 {
		t.Fatalf("wrong population/sample bound: %d/%d", s.PopulationCount, len(s.Members))
	}
	// Independent full-sort oracle, different storage/selection from the bounded heap.
	type item struct {
		id     server.Id
		digest [32]byte
	}
	all := []item{}
	for id := range speed {
		if quality[id] {
			continue
		}
		input := append(append([]byte(c.PublicationId), 0), id.Bytes()...)
		all = append(all, item{id, sha256.Sum256(input)})
	}
	sort.Slice(all, func(i, j int) bool {
		if x := bytes.Compare(all[i].digest[:], all[j].digest[:]); x != 0 {
			return x < 0
		}
		return all[i].id.Less(all[j].id)
	})
	for i, m := range s.Members {
		if m.ClientId != all[i].id || m.OKCount != 4 || m.Total != 5 {
			t.Fatalf("selection/evidence mismatch at rank %d", i)
		}
	}
	again := newClientScoreNativeSpeedSample(c, speed, quality, health)
	if !reflect.DeepEqual(s, again) {
		t.Fatal("selection depends on map iteration")
	}
	c.PublicationId = "02"
	next := newClientScoreNativeSpeedSample(c, speed, quality, health)
	if reflect.DeepEqual(s.Members, next.Members) {
		t.Fatal("new publication retained identical salt selection")
	}
	t.Logf("synthetic providers=80000 speed_only=78000 retained=256 selection_wall=%s", elapsed)
}

func TestNativeSpeedSampleTargetMembershipAndWirePrivacy(t *testing.T) {
	pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte("egress_index:\n  quality_ok_numerator: 4\n  quality_ok_denominator: 5\n"))
	defer pop()
	c, _, _, health := speedSampleFixture(4)
	scores := map[server.Id]*ClientScore{}
	for i := range 4 {
		id := speedSampleID(uint64(i))
		scores[id] = &ClientScore{ClientId: id, Online: true, PassesMinimums: map[RankMode]bool{RankModeSpeed: true}}
	}
	scores[speedSampleID(0)].PassesMinimums[RankModeQuality] = true
	scores[speedSampleID(1)].NetworkOnly = true
	scores[speedSampleID(2)].PassesMinimums[RankModeSpeed] = false
	targets := map[server.Id]map[server.Id]*ClientScore{speedSampleID(10): scores, speedSampleID(11): scores}
	census := newClientScoreNativeCensus(c.SourceStartedAt, c.SourceCompletedAt, c.SourceStartedAt, health, targets, targets)
	sample := census.speedOnlySample
	if sample.PopulationCount != 1 || len(sample.Members) != 1 || sample.Members[0].ClientId != speedSampleID(3) {
		t.Fatal("deduplication, private membership, or Q exclusion failed")
	}
	before, err := json.Marshal(census)
	if err != nil {
		t.Fatal(err)
	}
	census.speedOnlySample = nil
	after, err := json.Marshal(census)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || bytes.Contains(before, []byte(speedSampleID(3).String())) {
		t.Fatal("private sample changed public census wire")
	}
	if census.Buckets[RankModeSpeed].Providers != 2 || census.Buckets[RankModeQuality].Providers != 1 || census.Buckets["online"].Providers != 3 {
		t.Fatal("diagnostic changed census membership")
	}
	health[speedSampleID(3)] = ProviderEgressHealthCounts{}
	if sample.Members[0].OKCount != 4 {
		t.Fatal("sample retained mutable health map")
	}
}

func TestNativeSpeedSampleEncodingAndRefusals(t *testing.T) {
	c, speed, quality, health := speedSampleFixture(300)
	c.speedOnlySample = newClientScoreNativeSpeedSample(c, speed, quality, health)
	raw, _ := json.Marshal(c)
	data, err := encodeClientScoreNativeSpeedSample(c, raw)
	if err != nil || len(data) == 0 || len(data) > 65536 {
		t.Fatalf("valid bounded diagnostic refused: bytes=%d err=%v", len(data), err)
	}
	var sample clientScoreNativeSpeedSample
	if err := json.Unmarshal(data, &sample); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if sample.CensusSHA256 != hex.EncodeToString(digest[:]) || !sample.PublishedAt.Equal(c.PublishedAt) || !sample.SourceCompletedAt.Equal(c.SourceCompletedAt) {
		t.Fatal("source/census/publication lineage changed")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*clientScoreNativeSpeedSample)
	}{
		{"generation", func(s *clientScoreNativeSpeedSample) { s.PublicationId = "other" }},
		{"source", func(s *clientScoreNativeSpeedSample) { s.SourceCompletedAt = s.SourceCompletedAt.Add(time.Second) }},
		{"policy", func(s *clientScoreNativeSpeedSample) { s.PolicyVersion++ }},
		{"population", func(s *clientScoreNativeSpeedSample) { s.PopulationCount = 255 }},
		{"limit", func(s *clientScoreNativeSpeedSample) { s.SampleLimit = 257 }},
		{"duplicate", func(s *clientScoreNativeSpeedSample) { s.Members[1] = s.Members[0] }},
		{"zero_id", func(s *clientScoreNativeSpeedSample) { s.Members[0].ClientId = server.Id{} }},
		{"negative", func(s *clientScoreNativeSpeedSample) { s.Members[0].OKCount = -1 }},
		{"impossible", func(s *clientScoreNativeSpeedSample) { s.Members[0].OKCount = s.Members[0].Total + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := *c.speedOnlySample
			changed.Members = append([]clientScoreNativeSpeedSampleMember(nil), changed.Members...)
			tc.mutate(&changed)
			clone := *c
			clone.speedOnlySample = &changed
			if _, err := encodeClientScoreNativeSpeedSample(&clone, raw); err == nil {
				t.Fatal("malformed diagnostic admitted")
			}
		})
	}
	empty := newClientScoreNativeSpeedSample(c, map[server.Id]bool{}, nil, nil)
	clone := *c
	clone.speedOnlySample = empty
	if out, err := encodeClientScoreNativeSpeedSample(&clone, raw); err != nil || !bytes.Contains(out, []byte(`"members":[]`)) {
		t.Fatal("empty complete sample not explicit")
	}
	clone.speedOnlySample = nil
	if out, err := encodeClientScoreNativeSpeedSample(&clone, raw); err != nil || out != nil {
		t.Fatal("older census gained fabricated membership")
	}
}

// One explicitly owned loopback Redis database, with no Config/profile reads.
// Refuse pre-existing keys and remove only this test's two keys on completion.
func TestNativeSpeedSampleRedisCommitGuard(t *testing.T) {
	if os.Getenv("ARIN_SPEED_SAMPLE_REDIS_FIXTURE") != "owned-loopback-26379-db15" {
		t.Fatal("explicit owned Redis fixture handoff required")
	}
	r := redis.NewClient(&redis.Options{Addr: "127.0.0.1:26379", DB: 15, MaxRetries: -1, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = r.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	keys := []string{clientScoreNativeCensusKey, clientScoreNativeSpeedSampleKey}
	count, err := r.Exists(ctx, keys...).Result()
	if err != nil || count != 0 {
		t.Fatalf("fixture keys unavailable: count=%d err=%v", count, err)
	}
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		if err := r.Del(clean, keys...).Err(); err != nil {
			t.Errorf("owned key cleanup: %v", err)
		}
	})
	c, s, q, h := speedSampleFixture(300)
	c.speedOnlySample = newClientScoreNativeSpeedSample(c, s, q, h)
	raw, _ := json.Marshal(c)
	if err := writeClientScoreNativeSpeedSample(ctx, r, c, raw, time.Minute); err == nil {
		t.Fatal("diagnostic accepted before census commit")
	}
	if n, err := r.Exists(ctx, clientScoreNativeSpeedSampleKey).Result(); err != nil || n != 0 {
		t.Fatal("failed generation created diagnostic")
	}
	if err := r.Set(ctx, clientScoreNativeCensusKey, raw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := writeClientScoreNativeSpeedSample(ctx, r, c, raw, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, clientScoreNativeSpeedSampleKey).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := encodeClientScoreNativeSpeedSample(c, raw)
	if err != nil || !bytes.Equal(got, expected) {
		t.Fatal("committed diagnostic mismatch")
	}
	if ttl, err := r.PTTL(ctx, clientScoreNativeSpeedSampleKey).Result(); err != nil || ttl <= 0 || ttl > time.Minute {
		t.Fatalf("diagnostic ttl=%s err=%v", ttl, err)
	}
	if err := r.Set(ctx, clientScoreNativeCensusKey, []byte(`{"new_generation":true}`), time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := writeClientScoreNativeSpeedSample(ctx, r, c, raw, time.Minute); err == nil {
		t.Fatal("old census overwrote a newer generation")
	}
	after, err := r.Get(ctx, clientScoreNativeSpeedSampleKey).Bytes()
	if err != nil || !bytes.Equal(got, after) {
		t.Fatal("superseded write changed diagnostic")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := writeClientScoreNativeSpeedSample(canceled, r, c, raw, time.Minute); err != context.Canceled {
		t.Fatalf("cancellation not retained: %v", err)
	}
	if err := writeClientScoreNativeSpeedSample(ctx, r, c, raw, 0); err == nil {
		t.Fatal("nonpositive ttl admitted")
	}
}
