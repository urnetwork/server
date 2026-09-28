// Publication counts describe one complete score-source snapshot, not metric
// delivery time or the sum of overlapping location/group cache documents.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

const clientScoreNativeCensusKey = "client_score_native_census_v1"

// ClientScoreNativeBucketCount deduplicates publicly usable identities across
// all targets. Denominator bands are accepted selected-policy outcomes in the
// source's eight-hour window, not ten-success/four-hour quota completion.
type ClientScoreNativeBucketCount struct {
	Providers    int            `json:"providers"`
	Denominators map[string]int `json:"denominators"`
}

// ClientScoreNativeCensus is published only after the complete target export.
// Eligibility can change after SourceCompletedAt; readers must display source
// age and must not label PublishedAt or metric scrape time a live recomputation.
type ClientScoreNativeCensus struct {
	SchemaVersion     int                                     `json:"schema_version"`
	PublicationId     string                                  `json:"publication_id"`
	SourceStartedAt   time.Time                               `json:"source_started_at"`
	SourceCompletedAt time.Time                               `json:"source_completed_at"`
	PublishedAt       time.Time                               `json:"published_at"`
	PolicyVersion     int                                     `json:"policy_version"`
	Population        string                                  `json:"population"`
	DenominatorWindow string                                  `json:"denominator_window"`
	Buckets           map[string]ClientScoreNativeBucketCount `json:"buckets"`
}

// One provider may appear in several locations and groups. Count admission in
// any public target once per bucket, and preserve bands even when they are zero.
func newClientScoreNativeCensus(startedAt, completedAt time.Time, healthCounts map[server.Id]ProviderEgressHealthCounts, targets ...map[server.Id]map[server.Id]*ClientScore) *ClientScoreNativeCensus {
	census := &ClientScoreNativeCensus{
		SchemaVersion: 1, PublicationId: server.NewId().String(),
		SourceStartedAt: startedAt, SourceCompletedAt: completedAt,
		PolicyVersion:     SelectedProviderUrlProbePolicyVersion(),
		Population:        "public identities represented in score targets; native tags at source evaluation",
		DenominatorWindow: "accepted selected-policy outcomes in trailing eight hours; four-hour attempt and success-quota bands unavailable",
		Buckets:           map[string]ClientScoreNativeBucketCount{},
	}
	members := map[string]map[server.Id]bool{}
	for _, bucket := range []string{RankModeQuality, RankModeSpeed, "online"} {
		members[bucket] = map[server.Id]bool{}
	}
	for _, targetSet := range targets {
		for _, scores := range targetSet {
			for clientId, score := range scores {
				if score.NetworkOnly {
					continue
				}
				if score.PassesMinimums[RankModeQuality] {
					members[RankModeQuality][clientId] = true
				}
				if score.PassesMinimums[RankModeSpeed] {
					members[RankModeSpeed][clientId] = true
				}
				if score.Online {
					members["online"][clientId] = true
				}
			}
		}
	}
	for bucket, ids := range members {
		count := ClientScoreNativeBucketCount{Providers: len(ids), Denominators: map[string]int{
			"zero": 0, "one": 0, "two": 0, "three_to_four": 0, "five_to_nine": 0, "ten_plus": 0,
		}}
		for id := range ids {
			band := "zero"
			switch total := healthCounts[id].Total; {
			case 10 <= total:
				band = "ten_plus"
			case 5 <= total:
				band = "five_to_nine"
			case 3 <= total:
				band = "three_to_four"
			case total == 2:
				band = "two"
			case total == 1:
				band = "one"
			}
			count.Denominators[band]++
		}
		census.Buckets[bucket] = count
	}
	return census
}

// GetClientScoreNativeCensus returns nil when no complete native publication
// is available. It does no database census and does not refresh source age.
func GetClientScoreNativeCensus(ctx context.Context) (census *ClientScoreNativeCensus, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		data, err := r.Get(ctx, clientScoreNativeCensusKey).Bytes()
		if err == redis.Nil {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		value := &ClientScoreNativeCensus{}
		if err := json.Unmarshal(data, value); err != nil {
			returnErr = fmt.Errorf("read native publication census: %w", err)
			return
		}
		if value.SchemaVersion != 1 || value.PublicationId == "" || value.Population == "" || value.DenominatorWindow == "" || value.SourceStartedAt.IsZero() || value.SourceCompletedAt.Before(value.SourceStartedAt) || value.PublishedAt.Before(value.SourceCompletedAt) {
			returnErr = fmt.Errorf("native publication census has incomplete provenance")
			return
		}
		for _, bucket := range []string{RankModeQuality, RankModeSpeed, "online"} {
			count, ok := value.Buckets[bucket]
			if !ok || count.Providers < 0 {
				returnErr = fmt.Errorf("native publication census has incomplete counts")
				return
			}
			total := 0
			for _, band := range []string{"zero", "one", "two", "three_to_four", "five_to_nine", "ten_plus"} {
				n, ok := count.Denominators[band]
				if !ok || n < 0 {
					returnErr = fmt.Errorf("native publication census has incomplete denominator bands")
					return
				}
				total += n
			}
			if total != count.Providers {
				returnErr = fmt.Errorf("native publication census denominator count mismatch")
				return
			}
		}
		census = value
	})
	return
}

// A failed export never calls this commit. The prior complete census keeps
// its original timestamps and expires naturally rather than being relabeled.
func writeClientScoreNativeCensus(ctx context.Context, census *ClientScoreNativeCensus, ttl time.Duration) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	census.PublishedAt = server.NowUtc()
	data, err := json.Marshal(census)
	if err != nil {
		return err
	}
	server.Redis(ctx, func(r server.RedisClient) {
		if err := ctx.Err(); err != nil {
			returnErr = err
			return
		}
		returnErr = r.Set(ctx, clientScoreNativeCensusKey, data, ttl).Err()
	})
	return
}
