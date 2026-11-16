// Private diagnostic membership for one complete native census. This record is
// never consumed by serving eligibility and must not be exposed by public APIs.
package model

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/urnetwork/server/v2026"
)

const clientScoreNativeSpeedSampleLimit = 256
const clientScoreNativeSpeedSampleMaxBytes = 64 * 1024

// The hash tag equals the entire existing census key, so both keys use the
// same Redis Cluster slot without moving or renaming the public count record.
const clientScoreNativeSpeedSampleKey = "{client_score_native_census_v1}:speed_only_sample_v1"

type clientScoreNativeSpeedSampleMember struct {
	ClientId server.Id `json:"client_id"`
	OKCount  int       `json:"ok_count"`
	Total    int       `json:"total"`
}

type clientScoreNativeSpeedSample struct {
	SchemaVersion     int                                  `json:"schema_version"`
	PublicationId     string                               `json:"publication_id"`
	SourceStartedAt   time.Time                            `json:"source_started_at"`
	SourceCompletedAt time.Time                            `json:"source_completed_at"`
	PublishedAt       time.Time                            `json:"published_at"`
	PolicyVersion     int                                  `json:"policy_version"`
	CensusSHA256      string                               `json:"census_sha256"`
	Population        string                               `json:"population"`
	PopulationCount   int                                  `json:"population_count"`
	Sampling          string                               `json:"sampling"`
	SampleLimit       int                                  `json:"sample_limit"`
	Members           []clientScoreNativeSpeedSampleMember `json:"members"`
}

type clientScoreNativeSpeedSampleCandidate struct {
	id     server.Id
	digest [sha256.Size]byte
}

func (a clientScoreNativeSpeedSampleCandidate) less(b clientScoreNativeSpeedSampleCandidate) bool {
	if order := bytes.Compare(a.digest[:], b.digest[:]); order != 0 {
		return order < 0
	}
	return a.id.Less(b.id)
}

// The largest retained hash is the root. Memory never grows with the number of
// providers: the already-existing census maps are traversed once, keeping256.
type clientScoreNativeSpeedSampleHeap []clientScoreNativeSpeedSampleCandidate

func (h clientScoreNativeSpeedSampleHeap) Len() int           { return len(h) }
func (h clientScoreNativeSpeedSampleHeap) Less(i, j int) bool { return h[j].less(h[i]) }
func (h clientScoreNativeSpeedSampleHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *clientScoreNativeSpeedSampleHeap) Push(v any) {
	*h = append(*h, v.(clientScoreNativeSpeedSampleCandidate))
}
func (h *clientScoreNativeSpeedSampleHeap) Pop() any {
	old := *h
	value := old[len(old)-1]
	*h = old[:len(old)-1]
	return value
}

func newClientScoreNativeSpeedSample(census *ClientScoreNativeCensus, speed, quality map[server.Id]bool, health map[server.Id]ProviderEgressHealthCounts) *clientScoreNativeSpeedSample {
	sample := &clientScoreNativeSpeedSample{
		SchemaVersion: 1, PublicationId: census.PublicationId,
		SourceStartedAt: census.SourceStartedAt, SourceCompletedAt: census.SourceCompletedAt,
		PolicyVersion: census.PolicyVersion,
		Population:    "deduplicated public native speed without native quality at source evaluation",
		Sampling:      "lowest sha256(publication_id || NUL || 16-byte client_id), then client_id",
		SampleLimit:   clientScoreNativeSpeedSampleLimit,
		Members:       []clientScoreNativeSpeedSampleMember{},
	}
	selected := &clientScoreNativeSpeedSampleHeap{}
	salt := append([]byte(census.PublicationId), 0)
	for id, included := range speed {
		if !included || quality[id] {
			continue
		}
		sample.PopulationCount++
		digest := sha256.New()
		_, _ = digest.Write(salt)
		_, _ = digest.Write(id.Bytes())
		candidate := clientScoreNativeSpeedSampleCandidate{id: id}
		copy(candidate.digest[:], digest.Sum(nil))
		if selected.Len() < clientScoreNativeSpeedSampleLimit {
			heap.Push(selected, candidate)
		} else if candidate.less((*selected)[0]) {
			(*selected)[0] = candidate
			heap.Fix(selected, 0)
		}
	}
	sort.Slice(*selected, func(i, j int) bool { return (*selected)[i].less((*selected)[j]) })
	for _, candidate := range *selected {
		counts := health[candidate.id]
		sample.Members = append(sample.Members, clientScoreNativeSpeedSampleMember{
			ClientId: candidate.id, OKCount: counts.OKCount, Total: counts.Total,
		})
	}
	return sample
}

// A superseded or failed census cannot install its diagnostic. Existing native
// count bytes are unchanged. Readers must join the exact census hash/generation
// and bracket their bounded SQL observation with a second matching Redis read.
const clientScoreNativeSpeedSampleCommitScript = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[2], ARGV[2], 'PX', ARGV[3])
return 1
`

func encodeClientScoreNativeSpeedSample(census *ClientScoreNativeCensus, censusBytes []byte) ([]byte, error) {
	if census == nil || census.speedOnlySample == nil {
		return nil, nil
	}
	sample := *census.speedOnlySample
	if sample.PublicationId == "" || sample.PublicationId != census.PublicationId ||
		!sample.SourceStartedAt.Equal(census.SourceStartedAt) || !sample.SourceCompletedAt.Equal(census.SourceCompletedAt) ||
		sample.PolicyVersion != census.PolicyVersion || census.PublishedAt.Before(census.SourceCompletedAt) ||
		sample.SampleLimit != clientScoreNativeSpeedSampleLimit || sample.PopulationCount < 0 ||
		len(sample.Members) != min(sample.PopulationCount, clientScoreNativeSpeedSampleLimit) {
		return nil, fmt.Errorf("native speed sample provenance or bound invalid")
	}
	seen := map[server.Id]bool{}
	for _, member := range sample.Members {
		if seen[member.ClientId] || member.ClientId == (server.Id{}) || member.Total < 0 || member.OKCount < 0 || member.Total < member.OKCount {
			return nil, fmt.Errorf("native speed sample membership invalid")
		}
		seen[member.ClientId] = true
	}
	digest := sha256.Sum256(censusBytes)
	sample.CensusSHA256 = hex.EncodeToString(digest[:])
	sample.PublishedAt = census.PublishedAt
	data, err := json.Marshal(sample)
	if err != nil {
		return nil, err
	}
	if len(data) > clientScoreNativeSpeedSampleMaxBytes {
		return nil, fmt.Errorf("native speed sample exceeds byte bound")
	}
	return data, nil
}

// Diagnostic failure cannot revoke or fail an otherwise completed publication.
// This context budget adds no retries; the Redis client's transport timeouts
// still apply. Missing/mismatched diagnostics remain unknown to the reader.
func writeClientScoreNativeSpeedSample(ctx context.Context, r server.RedisClient, census *ClientScoreNativeCensus, censusBytes []byte, ttl time.Duration) error {
	data, err := encodeClientScoreNativeSpeedSample(census, censusBytes)
	if err != nil || data == nil {
		return err
	}
	if ttl < time.Millisecond {
		return fmt.Errorf("native speed sample ttl invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := r.Eval(ctx, clientScoreNativeSpeedSampleCommitScript,
		[]string{clientScoreNativeCensusKey, clientScoreNativeSpeedSampleKey}, censusBytes, data, ttl.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return fmt.Errorf("native speed sample census superseded")
	}
	return nil
}
