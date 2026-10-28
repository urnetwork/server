// The modern-first page partition of the score index (FP2-MODERN.md).
//
// The export writes each family facet's pages modern first: its modern
// providers are shuffled and cut into pages of their own, then the rest, so a
// page holds one class only. The counts document keeps its format, a gob
// []int of page sizes in page order, and a reader that predates the
// partition shuffles a facet's pages as before. What tells a reader where the
// modern pages end is metadata bound to the pages it describes: the native
// manifest's ModernPages, which the manifest's generation and page checksums
// already bind, and for the union pages a partition document per target
// payload holding the checksum of each facet's counts document and of each
// leading modern page.
//
// A reader classifies every target it reads as partitioned, absent (an older
// export wrote it, and its scores decode as not modern) or stale (union
// only: publication is incomplete or the metadata does not describe the pages
// read). A stale target's pages join the modern phase within the same finite
// read allowance.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// One target payload's modern partition: per family facet, the counts
// document it describes and its leading modern pages, by checksum.
type clientScoreModernPartition struct {
	// Distinguishes equal-content publications when a fenced begin is retried.
	Generation string
	Facets     map[ipFamilyFacet]clientScoreModernPartitionFacet
}

type clientScoreModernPartitionFacet struct {
	// sha256 of the facet's counts document as written
	CountsChecksum [sha256.Size]byte
	// sha256 of each leading modern page as written, in page order; the
	// facet's other pages hold no modern provider
	ModernPageChecksums [][sha256.Size]byte
}

// How a reader traverses a target's pages.
type clientScorePartitionMode int

const (
	// one shuffled group per facet, modern and other pages together: the
	// traversal before the partition, used while rank_modern_first is off
	clientScorePartitionOff clientScorePartitionMode = iota
	// the modern pages of every requested facet first, then the others
	clientScorePartitionModernFirst
	// the modern pages only: a request that requires modern providers
	clientScorePartitionModernOnly
)

// The partition mode of a request: modern-only for one that requires modern
// providers, else modern first while the setting is on.
func findProviders2PartitionMode(settings *EgressIndexSettings, requireModernProvider bool) clientScorePartitionMode {
	switch {
	case requireModernProvider:
		return clientScorePartitionModernOnly
	case settings.RankModernFirst:
		return clientScorePartitionModernFirst
	default:
		return clientScorePartitionOff
	}
}

// What a reader knows of one target's partition.
const (
	clientScoreModernPartitioned = "partitioned"
	clientScoreModernAbsent      = "absent"
	clientScoreModernStale       = "stale"
)

// Targets read per source and partition state. Rollout reads the absent
// share falling to zero as proof that a new export rewrote the index
// (FP2-MODERN.md, "Rollout"). Stale includes interrupted publications.
var findProviders2ModernPartitionTargets = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_modern_partition_total",
	Help: "Score index targets FindProviders2 read by a modern-first or modern-only traversal, per source (native, union) and partition state (partitioned, absent, stale)",
}, []string{"source", "state"})

// Answers whose finite union allowance ended inside a modern phase, so the
// band filled from non-modern pages while modern pages were unread
// (FP2-MODERN.md, "A finite allowance, the one exception").
var findProviders2ModernPriorityIncomplete = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_modern_priority_incomplete_total",
	Help: "FindProviders2 answers whose union allowance ended inside a modern phase before the band's modern survivors met its need, per requested rank mode",
}, []string{"rank_mode"})

// Providers FindProviders2 answered per band and whether each was modern
// (FP2-MODERN.md, "Observability"): the modern share of answers and what
// modern-first priority changes. Named providers are not drawn and are not
// counted.
var findProviders2AnsweredModern = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_answered_modern_total",
	Help: "Providers FindProviders2 discovery answered, per requested rank mode, band (native, borrowed, online) and whether the provider was modern",
}, []string{"rank_mode", "band", "modern"})

func recordFindProviders2AnsweredModern(rankMode RankMode, band string, modern bool) {
	findProviders2AnsweredModern.WithLabelValues(findProviders2MetricRankMode(rankMode), band, strconv.FormatBool(modern)).Inc()
}

func init() {
	for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed, RankModeOnline} {
		for _, band := range []string{"native", "borrowed", "online"} {
			for _, modern := range []bool{true, false} {
				findProviders2AnsweredModern.WithLabelValues(rankMode, band, strconv.FormatBool(modern))
			}
		}
	}
	for _, source := range []string{"native", "union"} {
		for _, state := range []string{clientScoreModernPartitioned, clientScoreModernAbsent, clientScoreModernStale} {
			findProviders2ModernPartitionTargets.WithLabelValues(source, state)
		}
	}
	for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed, RankModeOnline} {
		findProviders2ModernPriorityIncomplete.WithLabelValues(rankMode)
	}
	prometheus.MustRegister(findProviders2ModernPartitionTargets, findProviders2ModernPriorityIncomplete, findProviders2AnsweredModern)
}

// The partition document of encoded facet payloads, as the export writes it
// after their pages: modernPageChecksums are the facets' leading modern pages
// as written.
func newClientScoreModernPartition(facets map[ipFamilyFacet]clientScoreFacetPayload, modernPageChecksums map[ipFamilyFacet][][sha256.Size]byte) clientScoreModernPartition {
	partition := clientScoreModernPartition{
		Facets: map[ipFamilyFacet]clientScoreModernPartitionFacet{},
	}
	for facet, payload := range facets {
		partition.Facets[facet] = clientScoreModernPartitionFacet{
			CountsChecksum:      sha256.Sum256(payload.countsBytes),
			ModernPageChecksums: modernPageChecksums[facet],
		}
	}
	return partition
}

// A union target facet's partition, from its decoded partition document and
// the counts bytes the reader used: absent without a document, stale when the
// document does not describe these counts, else the leading modern pages'
// checksums.
func clientScoreUnionFacetPartition(partitionBytes []byte, facet ipFamilyFacet, countsBytes []byte, counts []int) (state string, modernPageChecksums [][sha256.Size]byte) {
	if len(partitionBytes) == 0 {
		return clientScoreModernAbsent, nil
	}
	var partition clientScoreModernPartition
	if err := gob.NewDecoder(bytes.NewReader(partitionBytes)).Decode(&partition); err != nil {
		return clientScoreModernStale, nil
	}
	partitionFacet, ok := partition.Facets[facet]
	if !ok || partitionFacet.CountsChecksum != sha256.Sum256(countsBytes) || len(counts) < len(partitionFacet.ModernPageChecksums) {
		return clientScoreModernStale, nil
	}
	return clientScoreModernPartitioned, partitionFacet.ModernPageChecksums
}

// Published after every complete export that wrote the modern flag and
// partition, holding the instant its source load started. The client rollout
// of require_modern_provider waits for one later than the last old writer's
// exit (FP2-MODERN.md, "Rollout", step 5).
const clientScoreModernReadyKey = "client_score_modern_v1_ready"

func markClientScoreModernReady(ctx context.Context, sourceStartedAt time.Time) (returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		returnErr = r.Set(ctx, clientScoreModernReadyKey, sourceStartedAt.UTC().Format(time.RFC3339Nano), 0).Err()
	})
	return
}

// The source start of the latest complete export that wrote the modern flag
// and partition, false before any.
func GetClientScoreModernReady(ctx context.Context) (sourceStartedAt time.Time, ready bool, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, clientScoreModernReadyKey).Result()
		if errors.Is(err, redis.Nil) {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		sourceStartedAt, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			returnErr = fmt.Errorf("client score modern marker %q is not an instant: %w", value, err)
			return
		}
		ready = true
	})
	return
}
