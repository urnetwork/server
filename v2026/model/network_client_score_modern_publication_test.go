// Interrupted and overlapping exports use the real encoder, bounded writer,
// and union reader. Barriers replace scheduling and retain the old bug's state.
package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Publish one canonical union payload, optionally pausing at an emitted command.
func writeModernPublicationTestPayload(t testing.TB, ctx context.Context, r server.RedisClient, locationId server.Id, scores []*ClientScore, intercept func(clientScoreRedisSet, func(clientScoreRedisSet) error) error) error {
	t.Helper()
	scoreKVs := map[server.Id]*ClientScore{}
	for _, score := range scores {
		scoreKVs[score.ClientId] = score
	}
	target := clientScoreTarget{id: locationId, clientScores: scoreKVs}
	keys := newClientScoreTargetKeys(false, RankModeQuality, target)
	return writeClientScoreRedisStream(ctx, r, time.Hour, func(emit func(clientScoreRedisSet) error) error {
		return emitClientScoreTargetFanout([]server.Id{{}}, scoreKVs, nil, keys,
			func(scores map[server.Id]*ClientScore) clientScoreExportPayload {
				return exportClientScores(false, RankModeQuality, scores)
			}, false, false,
			func(set clientScoreRedisSet) error {
				if intercept != nil {
					return intercept(set, emit)
				}
				return emit(set)
			})
	})
}

// Recognizes completed metadata in both the old and fenced stream shapes.
func isModernPublicationTestCommit(set clientScoreRedisSet, key string) bool {
	if set.key != key {
		return false
	}
	var partition clientScoreModernPartition
	return gob.NewDecoder(bytes.NewReader(set.value)).Decode(&partition) == nil
}

// A zero-modern predecessor and its replacement have identical count bytes.
// Stopping before commit must leave a dirty partition, never the old zero proof.
func TestModernPublicationInterruptedSameCountsKeepsModernRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		locationId := server.NewId()
		partitionKey := clientScoreLocationModernKey(false, RankModeQuality, locationId, server.Id{})
		countsKey := clientScoreLocationFacetCountsKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetV4Only)
		interrupted := errors.New("synthetic stop before partition commit")
		modernScores := modernTestScores(400, true, ipFamilyFacetV4Only, false)
		server.Redis(ctx, func(r server.RedisClient) {
			if err := writeModernPublicationTestPayload(t, ctx, r, locationId, modernTestScores(400, false, ipFamilyFacetV4Only, false), nil); err != nil {
				t.Fatal(err)
			}
			before, err := r.Get(ctx, countsKey).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			err = writeModernPublicationTestPayload(t, ctx, r, locationId, modernScores, func(set clientScoreRedisSet, emit func(clientScoreRedisSet) error) error {
				if isModernPublicationTestCommit(set, partitionKey) {
					if err := emit(clientScoreRedisSet{flush: true}); err != nil {
						return err
					}
					return interrupted
				}
				return emit(set)
			})
			if !errors.Is(err, interrupted) {
				t.Fatalf("writer did not reach the commit boundary: %v", err)
			}
			after, err := r.Get(ctx, countsKey).Bytes()
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("fixture changed the count document: err=%v", err)
			}
		})
		scores, _, err := loadClientScoresWithPartition(false, RankModeQuality, ctx, map[server.Id]bool{locationId: true}, nil, server.Id{}, 1000,
			[]ipFamilyFacet{ipFamilyFacetV4Only}, clientScorePartitionModernOnly)
		if err != nil || len(scores) != len(modernScores) {
			t.Fatalf("interrupted same-count export hid modern rows: got=%d want=%d err=%v", len(scores), len(modernScores), err)
		}
		for _, score := range modernScores {
			if scores[score.ClientId] == nil || !scores[score.ClientId].Modern {
				t.Fatal("modern-only recovery omitted a replacement row")
			}
		}
	})
}

// B takes over after A writes its first page. A must not overwrite B's remaining
// pages or install its metadata when its producer resumes.
func TestModernPublicationSupersededWriterCannotFinish(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		locationId := server.NewId()
		firstPageKey := clientScoreLocationFacetSampleKey(false, RankModeQuality, locationId, server.Id{}, ipFamilyFacetV4Only, 0)
		partitionKey := clientScoreLocationModernKey(false, RankModeQuality, locationId, server.Id{})
		winningScores := modernTestScores(400, true, ipFamilyFacetV4Only, false)
		server.Redis(ctx, func(r server.RedisClient) {
			var winnerMetadata []byte
			superseded := false
			err := writeModernPublicationTestPayload(t, ctx, r, locationId, modernTestScores(400, false, ipFamilyFacetV4Only, false), func(set clientScoreRedisSet, emit func(clientScoreRedisSet) error) error {
				if err := emit(set); err != nil {
					return err
				}
				if set.key == firstPageKey && !superseded {
					superseded = true
					if err := emit(clientScoreRedisSet{flush: true}); err != nil {
						return err
					}
					if err := writeModernPublicationTestPayload(t, ctx, r, locationId, winningScores, nil); err != nil {
						return err
					}
					var err error
					winnerMetadata, err = r.Get(ctx, partitionKey).Bytes()
					return err
				}
				return nil
			})
			if !superseded || err == nil {
				t.Fatalf("superseded writer completed: reached=%t err=%v", superseded, err)
			}
			metadata, readErr := r.Get(ctx, partitionKey).Bytes()
			if readErr != nil || !bytes.Equal(metadata, winnerMetadata) {
				t.Fatalf("superseded writer changed the winner's metadata: %v", readErr)
			}
		})
		scores, _, err := loadClientScoresWithPartition(false, RankModeQuality, ctx, map[server.Id]bool{locationId: true}, nil, server.Id{}, 1000,
			[]ipFamilyFacet{ipFamilyFacetV4Only}, clientScorePartitionModernOnly)
		if err != nil || len(scores) != len(winningScores) {
			t.Fatalf("superseded writer changed the winner's pages: got=%d want=%d err=%v", len(scores), len(winningScores), err)
		}
		for _, score := range winningScores {
			if scores[score.ClientId] == nil {
				t.Fatal("superseded writer replaced a winning row")
			}
		}
	})
}
