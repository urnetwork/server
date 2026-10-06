// Publication counts describe one complete score-source snapshot, not metric
// delivery time or the sum of overlapping location/group cache documents.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math/bits"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

const clientScoreNativeCensusKey = "client_score_native_census_v1"

// ClientScoreNativeBucketCount deduplicates publicly usable identities across
// all targets. Denominator bands are accepted selected-policy outcomes in the
// source's eight-hour window, not ten-success/four-hour quota completion.
type ClientScoreNativeBucketCount struct {
	Providers    int            `json:"providers"`
	Denominators map[string]int `json:"denominators"`
}

// These provider counts apply only the configured success ratio. PublicOnline
// uses the existing deduplicated public online cohort; SourceMap also includes
// evidence for providers outside that cohort. Neither is native bucket supply.
// The source keeps the existing selected-policy, total_count=1 history predicate
// and its measured_at bounds; it does not add a url_probe filter.
type ClientScoreNativeEgressRatioCount struct {
	Providers  int `json:"providers"`
	Observed   int `json:"observed"`
	Passed     int `json:"passed"`
	Failed     int `json:"failed"`
	NoEvidence int `json:"no_evidence"`
}

func (count *ClientScoreNativeEgressRatioCount) UnmarshalJSON(data []byte) error {
	var fields struct {
		Providers  *int `json:"providers"`
		Observed   *int `json:"observed"`
		Passed     *int `json:"passed"`
		Failed     *int `json:"failed"`
		NoEvidence *int `json:"no_evidence"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields.Providers == nil || fields.Observed == nil || fields.Passed == nil || fields.Failed == nil || fields.NoEvidence == nil {
		return fmt.Errorf("native publication census has incomplete ratio counts")
	}
	*count = ClientScoreNativeEgressRatioCount{*fields.Providers, *fields.Observed, *fields.Passed, *fields.Failed, *fields.NoEvidence}
	return nil
}

// A nil field in an older census is unknown. New publishers report either a
// complete ratio observation or an explicit unavailable reason, preserving the
// existing native publication when this optional diagnostic is unavailable.
type ClientScoreNativeEgressRatio struct {
	UnavailableReason    string                             `json:"unavailable_reason,omitempty"`
	WindowStartExclusive time.Time                          `json:"window_start_exclusive,omitzero"`
	WindowEndInclusive   time.Time                          `json:"window_end_inclusive,omitzero"`
	OKNumerator          int                                `json:"ok_numerator"`
	OKDenominator        int                                `json:"ok_denominator"`
	PublicOnline         *ClientScoreNativeEgressRatioCount `json:"public_online,omitempty"`
	SourceMap            *ClientScoreNativeEgressRatioCount `json:"source_map,omitempty"`
}

func (ratio *ClientScoreNativeEgressRatio) UnmarshalJSON(data []byte) error {
	type value ClientScoreNativeEgressRatio
	var decoded value
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if decoded.UnavailableReason == "" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		for _, name := range []string{"window_start_exclusive", "window_end_inclusive", "ok_numerator", "ok_denominator", "public_online", "source_map"} {
			if field, ok := fields[name]; !ok || string(field) == "null" {
				return fmt.Errorf("native publication census has incomplete ratio provenance")
			}
		}
	}
	*ratio = ClientScoreNativeEgressRatio(decoded)
	return nil
}

// ClientScoreNativeCensus is published only after the complete target export.
// Eligibility can change after SourceCompletedAt; readers must display source
// age and must not label PublishedAt or metric scrape time a live recomputation.
type ClientScoreNativeCensus struct {
	// Kept private and omitted from the existing count wire format.
	speedOnlySample   *clientScoreNativeSpeedSample
	SchemaVersion     int                                     `json:"schema_version"`
	PublicationId     string                                  `json:"publication_id"`
	SourceStartedAt   time.Time                               `json:"source_started_at"`
	SourceCompletedAt time.Time                               `json:"source_completed_at"`
	PublishedAt       time.Time                               `json:"published_at"`
	PolicyVersion     int                                     `json:"policy_version"`
	Population        string                                  `json:"population"`
	DenominatorWindow string                                  `json:"denominator_window"`
	Buckets           map[string]ClientScoreNativeBucketCount `json:"buckets"`
	EgressRatio       *ClientScoreNativeEgressRatio           `json:"egress_ratio,omitempty"`
}

// One provider may appear in several locations and groups. Count admission in
// any public target once per bucket, and preserve bands even when they are zero.
func newClientScoreNativeCensus(startedAt, completedAt, healthWindowEnd time.Time, healthCounts map[server.Id]ProviderEgressHealthCounts, targets ...map[server.Id]map[server.Id]*ClientScore) *ClientScoreNativeCensus {
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
	census.EgressRatio = newClientScoreNativeEgressRatio(startedAt, completedAt, healthWindowEnd, egressIndexSettings(), healthCounts, members["online"])
	census.speedOnlySample = newClientScoreNativeSpeedSample(census, members[RankModeSpeed], members[RankModeQuality], healthCounts)
	return census
}

func newClientScoreNativeEgressRatio(startedAt, completedAt, windowEnd time.Time, settings *EgressIndexSettings, healthCounts map[server.Id]ProviderEgressHealthCounts, online map[server.Id]bool) *ClientScoreNativeEgressRatio {
	unavailable := func(reason string) *ClientScoreNativeEgressRatio {
		return &ClientScoreNativeEgressRatio{UnavailableReason: reason}
	}
	if windowEnd.IsZero() || windowEnd.Before(startedAt) || completedAt.Before(windowEnd) {
		return unavailable("source_window_invalid")
	}
	if settings == nil || settings.QualityOkNumerator < 0 || settings.QualityOkDenominator <= 0 || settings.QualityOkDenominator < settings.QualityOkNumerator {
		return unavailable("ratio_configuration_invalid")
	}
	ratio := &ClientScoreNativeEgressRatio{
		WindowStartExclusive: windowEnd.Add(-ProviderEgressHealthMaxAge), WindowEndInclusive: windowEnd,
		OKNumerator: settings.QualityOkNumerator, OKDenominator: settings.QualityOkDenominator,
		PublicOnline: &ClientScoreNativeEgressRatioCount{}, SourceMap: &ClientScoreNativeEgressRatioCount{},
	}
	add := func(out *ClientScoreNativeEgressRatioCount, counts ProviderEgressHealthCounts) bool {
		if counts.Total < 0 || counts.OKCount < 0 || counts.Total < counts.OKCount {
			return false
		}
		out.Providers++
		if counts.Total == 0 {
			out.NoEvidence++
			return true
		}
		out.Observed++
		// Compare exact products without overflowing int, including large
		// configured fractions. N=0 never becomes a pass, even for a zero bar.
		leftHigh, leftLow := bits.Mul64(uint64(ratio.OKDenominator), uint64(counts.OKCount))
		rightHigh, rightLow := bits.Mul64(uint64(ratio.OKNumerator), uint64(counts.Total))
		if rightHigh < leftHigh || (rightHigh == leftHigh && rightLow <= leftLow) {
			out.Passed++
		} else {
			out.Failed++
		}
		return true
	}
	for _, counts := range healthCounts {
		if !add(ratio.SourceMap, counts) {
			return unavailable("health_counts_invalid")
		}
	}
	for id := range online {
		if !add(ratio.PublicOnline, healthCounts[id]) {
			return unavailable("health_counts_invalid")
		}
	}
	return ratio
}

func validateClientScoreNativeEgressRatio(census *ClientScoreNativeCensus) error {
	ratio := census.EgressRatio
	if ratio == nil {
		return nil // Older complete publications remain readable; ratio unknown.
	}
	if ratio.UnavailableReason != "" {
		switch ratio.UnavailableReason {
		case "source_window_invalid", "ratio_configuration_invalid", "health_counts_invalid":
			if ratio.PublicOnline == nil && ratio.SourceMap == nil && ratio.WindowStartExclusive.IsZero() && ratio.WindowEndInclusive.IsZero() && ratio.OKNumerator == 0 && ratio.OKDenominator == 0 {
				return nil
			}
		}
		return fmt.Errorf("native publication census has invalid ratio unavailability")
	}
	if ratio.WindowEndInclusive.IsZero() || ratio.WindowEndInclusive.Before(census.SourceStartedAt) || census.SourceCompletedAt.Before(ratio.WindowEndInclusive) || !ratio.WindowStartExclusive.Equal(ratio.WindowEndInclusive.Add(-ProviderEgressHealthMaxAge)) || ratio.OKNumerator < 0 || ratio.OKDenominator <= 0 || ratio.OKDenominator < ratio.OKNumerator || census.PolicyVersion <= 0 {
		return fmt.Errorf("native publication census has incomplete ratio provenance")
	}
	for _, count := range []*ClientScoreNativeEgressRatioCount{ratio.PublicOnline, ratio.SourceMap} {
		// Subtraction after bounds checks avoids overflow in malformed totals.
		if count == nil || count.Providers < 0 || count.Observed < 0 || count.Observed > count.Providers || count.NoEvidence != count.Providers-count.Observed || count.Passed < 0 || count.Passed > count.Observed || count.Failed != count.Observed-count.Passed {
			return fmt.Errorf("native publication census has invalid ratio counts")
		}
	}
	if ratio.PublicOnline.Providers != census.Buckets["online"].Providers || ratio.PublicOnline.Observed > ratio.SourceMap.Observed || ratio.PublicOnline.Passed > ratio.SourceMap.Passed || ratio.PublicOnline.Failed > ratio.SourceMap.Failed {
		return fmt.Errorf("native publication census has inconsistent ratio populations")
	}
	return nil
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
				if n > count.Providers-total {
					returnErr = fmt.Errorf("native publication census denominator count mismatch")
					return
				}
				total += n
			}
			if total != count.Providers {
				returnErr = fmt.Errorf("native publication census denominator count mismatch")
				return
			}
		}
		if err := validateClientScoreNativeEgressRatio(value); err != nil {
			returnErr = err
			return
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
		if returnErr == nil {
			// Optional private evidence never changes the completed census result.
			_ = writeClientScoreNativeSpeedSample(ctx, r, census, data, ttl)
		}
	})
	return
}
