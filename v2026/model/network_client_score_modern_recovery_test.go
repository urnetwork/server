// Exercises modern-only recovery through the published union pages, request
// filtering and bounded cursor refill. Publication changes are explicit writes.
package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Counts reads of immutable synthetic sample keys without changing Redis.
type modernRecoveryReadHook struct {
	active       atomic.Bool
	readCountKVs map[string]*atomic.Int64
}

// Leaves transport setup unchanged.
func (self *modernRecoveryReadHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

// Observes one completed command.
func (self *modernRecoveryReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		err := next(ctx, command)
		self.observe([]redis.Cmder{command})
		return err
	}
}

// Observes each completed pipeline command once.
func (self *modernRecoveryReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		err := next(ctx, commands)
		self.observe(commands)
		return err
	}
}

// Ignores all keys outside this fixture's samples.
func (self *modernRecoveryReadHook) observe(commands []redis.Cmder) {
	if !self.active.Load() {
		return
	}
	for _, command := range commands {
		if command.Name() != "get" || len(command.Args()) != 2 {
			continue
		}
		key, _ := command.Args()[1].(string)
		if count := self.readCountKVs[key]; count != nil {
			count.Add(1)
		}
	}
}

// Encodes a replacement page in the export's gob format.
func modernRecoveryPageBytes(t testing.TB, scores []*ClientScore) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(scores); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// Changes a modern page after its partition was published, leaving a modern
// provider in a page the old partition classifies as non-modern. The public
// request must recover that provider and refuse the changed non-modern row.
func testModernOnlyRecoversUnionPage(t *testing.T, missing bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		modernTestProviderConfig(t, modernTestUnionReaderConfig)
		location := server.NewId()
		original := modernTestScore(true, ipFamilyFacetV4Only, true)
		recovered := modernTestScore(false, ipFamilyFacetV4Only, true)
		modernTestPublishLocation(t, location, []*ClientScore{original, recovered})
		modernKey := clientScoreLocationFacetSampleKey(true, RankModeSpeed, location, server.Id{}, ipFamilyFacetV4Only, 0)
		otherKey := clientScoreLocationFacetSampleKey(true, RankModeSpeed, location, server.Id{}, ipFamilyFacetV4Only, 1)
		original.Modern, recovered.Modern = false, true
		hook := &modernRecoveryReadHook{readCountKVs: map[string]*atomic.Int64{
			modernKey: {}, otherKey: {},
		}}
		server.Redis(ctx, func(r server.RedisClient) {
			if missing {
				if err := r.Del(ctx, modernKey).Err(); err != nil {
					t.Fatal(err)
				}
			} else if err := r.Set(ctx, modernKey, modernRecoveryPageBytes(t, []*ClientScore{original}), time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
			if err := r.Set(ctx, otherKey, modernRecoveryPageBytes(t, []*ClientScore{recovered}), time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
			r.AddHook(hook)
		})
		hook.active.Store(true)
		t.Cleanup(func() { hook.active.Store(false) })

		result := modernTestFind(t, &FindProviders2Args{
			Specs: []*ProviderSpec{{LocationId: &location}}, RankMode: RankModeSpeed,
			ForceMinimum: true, ForceCount: true, Count: 1, RequireModernProvider: true,
		})
		if len(result.Providers) != 1 || result.Providers[0].ClientId != recovered.ClientId {
			t.Fatalf("missing=%t: modern-only request lost the modern provider in the former non-modern page: %+v", missing, result.Providers)
		}
		for key, count := range hook.readCountKVs {
			if count.Load() != 1 {
				t.Fatalf("sample %s read %d times, want one", key, count.Load())
			}
		}
	})
}

// A checksum mismatch restores the pages modern-only selection omitted.
func TestModernOnlyRecoversChangedUnionPage(t *testing.T) {
	testModernOnlyRecoversUnionPage(t, false)
}

// A missing expected modern page invalidates the same partition proof.
func TestModernOnlyRecoversMissingUnionPage(t *testing.T) {
	testModernOnlyRecoversUnionPage(t, true)
}

// Valid metadata still skips non-modern pages, and absent metadata still
// fails closed even when another target needs recovery.
func TestModernOnlyRecoveryPreservesUnreadPartitionClasses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		location, absentLocation := server.NewId(), server.NewId()
		original := modernTestScore(true, ipFamilyFacetV4Only, true)
		recovered := modernTestScore(false, ipFamilyFacetV4Only, true)
		modernTestPublishLocation(t, location, []*ClientScore{original, recovered})
		modernTestPublishLocation(t, absentLocation, []*ClientScore{modernTestScore(true, ipFamilyFacetV4Only, true)})
		modernKey := clientScoreLocationFacetSampleKey(true, RankModeSpeed, location, server.Id{}, ipFamilyFacetV4Only, 0)
		otherKey := clientScoreLocationFacetSampleKey(true, RankModeSpeed, location, server.Id{}, ipFamilyFacetV4Only, 1)
		absentKey := clientScoreLocationFacetSampleKey(true, RankModeSpeed, absentLocation, server.Id{}, ipFamilyFacetV4Only, 0)
		hook := &modernRecoveryReadHook{readCountKVs: map[string]*atomic.Int64{
			modernKey: {}, otherKey: {}, absentKey: {},
		}}
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Del(ctx, clientScoreLocationModernKey(true, RankModeSpeed, absentLocation, server.Id{})).Err(); err != nil {
				t.Fatal(err)
			}
			r.AddHook(hook)
		})
		hook.active.Store(true)
		t.Cleanup(func() { hook.active.Store(false) })
		load := func(observation *findProviders2LoadObservation) (map[server.Id]*ClientScore, *clientScoreCursor) {
			scores, cursor, err := loadClientScoresWithPartition(true, RankModeSpeed, ctx,
				map[server.Id]bool{location: true, absentLocation: true}, nil, server.Id{}, 1000,
				[]ipFamilyFacet{ipFamilyFacetV4Only}, clientScorePartitionModernOnly, observation)
			if err != nil {
				t.Fatal(err)
			}
			return scores, cursor
		}
		observation := &findProviders2LoadObservation{}
		scores, cursor := load(observation)
		connect.AssertEqual(t, len(scores), 1)
		connect.AssertEqual(t, scores[original.ClientId] != nil, true)
		connect.AssertEqual(t, cursor.hasMore(), false)
		connect.AssertEqual(t, cursor.hasOtherPhase(), false)
		connect.AssertEqual(t, observation.unreadNonModernRows, 2)
		connect.AssertEqual(t, hook.readCountKVs[otherKey].Load(), int64(0))
		connect.AssertEqual(t, hook.readCountKVs[absentKey].Load(), int64(0))

		original.Modern, recovered.Modern = false, true
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Set(ctx, modernKey, modernRecoveryPageBytes(t, []*ClientScore{original}), time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
			if err := r.Set(ctx, otherKey, modernRecoveryPageBytes(t, []*ClientScore{recovered}), time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		})
		observation = &findProviders2LoadObservation{}
		_, cursor = load(observation)
		connect.AssertEqual(t, cursor.hasMore(), true)
		connect.AssertEqual(t, cursor.readCount, 1)
		connect.AssertEqual(t, cursor.modernReadCount, 1)
		connect.AssertEqual(t, observation.unreadNonModernRows, 1)
		cursor.markModernStale()
		connect.AssertEqual(t, observation.unreadNonModernRows, 1)
		scores, err := cursor.read(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, len(scores), 1)
		connect.AssertEqual(t, scores[recovered.ClientId] != nil, true)
		connect.AssertEqual(t, cursor.hasMore(), false)
		connect.AssertEqual(t, cursor.readCount, 2)
		connect.AssertEqual(t, cursor.modernReadCount, 2)
		connect.AssertEqual(t, hook.readCountKVs[otherKey].Load(), int64(1))
		connect.AssertEqual(t, hook.readCountKVs[absentKey].Load(), int64(0))
	})
}

// Recovery consumes the existing finite allowance without resetting counters
// or revisiting pages. Enough rejected pages remain to expose a budget reset.
func TestModernOnlyRecoveryKeepsFiniteRowBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		location := server.NewId()
		original := modernTestScore(true, ipFamilyFacetV4Only, true)
		modernTestPublishLocation(t, location, append([]*ClientScore{original}, modernTestScores(4000, false, ipFamilyFacetV4Only, true)...))
		modernKey := clientScoreLocationFacetSampleKey(true, RankModeSpeed, location, server.Id{}, ipFamilyFacetV4Only, 0)
		original.Modern = false
		server.Redis(ctx, func(r server.RedisClient) {
			if err := r.Set(ctx, modernKey, modernRecoveryPageBytes(t, []*ClientScore{original}), time.Hour).Err(); err != nil {
				t.Fatal(err)
			}
		})
		observation := &findProviders2LoadObservation{}
		_, cursor, err := loadClientScoresWithPartition(true, RankModeSpeed, ctx,
			map[server.Id]bool{location: true}, nil, server.Id{}, 1000,
			[]ipFamilyFacet{ipFamilyFacetV4Only}, clientScorePartitionModernOnly, observation)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, cursor.readCount, 1)
		connect.AssertEqual(t, observation.unreadNonModernRows, 0)
		seenPageKVs := map[string]bool{modernKey: true}
		for cursor.hasMore() {
			additionalRows := findProviders2RefillLoadCount(20, 0, cursor.readCount) - cursor.readCount
			if additionalRows <= 0 {
				break
			}
			cursor.markModernStale()
			for _, page := range cursor.take(additionalRows) {
				if seenPageKVs[page.key] {
					t.Fatalf("recovery selected page %s twice", page.key)
				}
				seenPageKVs[page.key] = true
			}
		}
		maximum := findProviders2LoadCount(20, 2400)
		if cursor.readCount < maximum || maximum+ClientScoreSampleCount <= cursor.readCount || !cursor.hasMore() {
			t.Fatalf("recovery changed the finite allowance: rows=%d cap=%d more=%t", cursor.readCount, maximum, cursor.hasMore())
		}
		connect.AssertEqual(t, cursor.modernReadCount, cursor.readCount)
		connect.AssertEqual(t, cursor.nextPage, len(seenPageKVs))
		connect.AssertEqual(t, observation.unreadNonModernRows, 0)
	})
}
