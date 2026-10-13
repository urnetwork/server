package model

// An optional observer of the existing score owner, not a second scoring job.
// It issues no queries, changes no decisions and installs nothing at startup.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server/v2026"
)

type ArinShadowScoreCapture struct {
	mu       sync.Mutex
	capacity int
	closed   bool
	current  *ArinShadowScoreSnapshot
	lease    *ArinShadowScoreLease
}

type arinShadowScoreEntry struct {
	country                string
	baseQuality, baseSpeed bool
	weights                map[int]float64
	qualityMode, speedMode map[int]bool
	ambiguous              bool
}

type arinShadowScoreAttempt struct {
	owner   *ArinShadowScoreCapture
	entries map[server.Id]*arinShadowScoreEntry
	invalid bool
}

// This object is immutable after publication. Private provider membership is
// point-readable only through its exact source generation, never serialized.
type ArinShadowScoreSnapshot struct {
	owner                                           *ArinShadowScoreCapture
	generation                                      string
	sourceStartedAt, sourceCompletedAt, publishedAt time.Time
	members                                         map[server.Id]server.ArinShadowCaptureProvider
}

var currentArinShadowScoreCapture atomic.Pointer[ArinShadowScoreCapture]

func NewArinShadowScoreCapture(capacity int) (*ArinShadowScoreCapture, error) {
	if capacity < 1 || capacity > server.ArinShadowCapturePopulationLimit {
		return nil, server.ErrArinShadowInput
	}
	return &ArinShadowScoreCapture{capacity: capacity}, nil
}

// Explicit diagnostic installation. It does not activate subscriber policy,
// load candidate resources, publish a snapshot or claim Main authority.
func InstallArinShadowScoreCapture(c *ArinShadowScoreCapture) { currentArinShadowScoreCapture.Store(c) }

func (c *ArinShadowScoreCapture) Close() {
	currentArinShadowScoreCapture.CompareAndSwap(c, nil)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.current = nil
	if c.lease != nil {
		if c.lease.timer != nil {
			c.lease.timer.Stop()
		}
		c.lease.snapshot = nil
		c.lease = nil
	}
}

func beginArinShadowScoreCapture() *arinShadowScoreAttempt {
	c := currentArinShadowScoreCapture.Load()
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	return &arinShadowScoreAttempt{owner: c, entries: map[server.Id]*arinShadowScoreEntry{}}
}

func (a *arinShadowScoreAttempt) observe(score *ClientScore, country *string, facts *providerEgressFacts, enabled bool) {
	if a == nil || a.invalid || score == nil || score.NetworkOnly {
		return
	}
	if score.ClientId == (server.Id{}) || score.LookbackIndex < 0 || score.LookbackIndex >= len(ClientLookbacks) || math.IsNaN(score.IndependentReliabilityWeight) || math.IsInf(score.IndependentReliabilityWeight, 0) {
		a.invalid = true
		return
	}
	// Reuse the production decision with only the two ARIN policy inputs
	// removed. Reliability, TLS failure, URL evidence and all other gates stay.
	baseFacts := *facts
	baseFacts.arinRisk = false
	baseFacts.arinNonQuality = false
	base := decideProviderEgress(&baseFacts, enabled)
	code := normalizeCountryCode(country)
	entry := a.entries[score.ClientId]
	if entry == nil {
		if len(a.entries) >= a.owner.capacity {
			a.invalid = true
			a.entries = nil
			return
		}
		entry = &arinShadowScoreEntry{country: code, baseQuality: base.quality, baseSpeed: base.speed, weights: map[int]float64{}, qualityMode: map[int]bool{}, speedMode: map[int]bool{}}
		a.entries[score.ClientId] = entry
	} else if entry.country != code || entry.baseQuality != base.quality || entry.baseSpeed != base.speed {
		entry.ambiguous = true
	}
	_, quality := score.Scores[RankModeQuality]
	_, speed := score.Scores[RankModeSpeed]
	if weight, exists := entry.weights[score.LookbackIndex]; exists && (weight != score.IndependentReliabilityWeight || entry.qualityMode[score.LookbackIndex] != quality || entry.speedMode[score.LookbackIndex] != speed) {
		entry.ambiguous = true
	}
	entry.weights[score.LookbackIndex] = score.IndependentReliabilityWeight
	entry.qualityMode[score.LookbackIndex] = quality
	entry.speedMode[score.LookbackIndex] = speed
}

func arinShadowNativeGeneration(census *ClientScoreNativeCensus) string {
	if census == nil {
		return ""
	}
	encoded, err := json.Marshal(census)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// Called only after the real native publication succeeded. The already
// assembled targets are the actual native flags; counts must equal that exact
// published census. An overflow/ambiguous owner never breaks serving work.
func (a *arinShadowScoreAttempt) publish(census *ClientScoreNativeCensus, targets ...map[server.Id]map[server.Id]*ClientScore) {
	if a == nil {
		return
	}
	a.owner.mu.Lock()
	defer a.owner.mu.Unlock()
	if a.owner.closed || currentArinShadowScoreCapture.Load() != a.owner {
		return
	}
	// Any attempted replacement invalidates a previous diagnostic generation.
	a.owner.current = nil
	if a.invalid || census == nil || census.PublicationId == "" || census.SourceStartedAt.IsZero() || census.SourceCompletedAt.Before(census.SourceStartedAt) || census.PublishedAt.Before(census.SourceCompletedAt) {
		return
	}
	snapshot := &ArinShadowScoreSnapshot{owner: a.owner, generation: arinShadowNativeGeneration(census), sourceStartedAt: census.SourceStartedAt, sourceCompletedAt: census.SourceCompletedAt, publishedAt: census.PublishedAt, members: make(map[server.Id]server.ArinShadowCaptureProvider, len(a.entries))}
	for id, entry := range a.entries {
		indexes := make([]int, 0, len(entry.weights))
		for index := range entry.weights {
			indexes = append(indexes, index)
		}
		slices.Sort(indexes)
		if len(indexes) == 0 {
			return
		}
		passes := providerReliabilityPasses(entry.weights, providerReliabilityMinimums())
		buckets := []string{"all"}
		if len(entry.country) == 2 {
			buckets = append(buckets, entry.country)
		}
		snapshot.members[id] = server.ArinShadowCaptureProvider{ClientId: id, Buckets: buckets, BaseQuality: entry.baseQuality && passes && entry.qualityMode[indexes[0]], BaseSpeed: entry.baseSpeed && passes && entry.speedMode[indexes[0]], MembershipUnavailable: entry.ambiguous}
	}
	quality, speed := 0, 0
	online := map[server.Id]bool{}
	for _, target := range targets {
		for _, scores := range target {
			for id, score := range scores {
				if score.NetworkOnly {
					continue
				}
				member, exists := snapshot.members[id]
				if !exists {
					return
				}
				if score.PassesMinimums[RankModeQuality] && !member.ActiveQuality {
					member.ActiveQuality = true
					quality++
				}
				if score.PassesMinimums[RankModeSpeed] && !member.ActiveSpeed {
					member.ActiveSpeed = true
					speed++
				}
				if score.Online {
					online[id] = true
				}
				if member.ActiveQuality && !member.BaseQuality || member.ActiveSpeed && !member.BaseSpeed {
					member.MembershipUnavailable = true
				}
				snapshot.members[id] = member
			}
		}
	}
	q, qok := census.Buckets[RankModeQuality]
	s, sok := census.Buckets[RankModeSpeed]
	o, ook := census.Buckets["online"]
	if !qok || !sok || !ook || q.Providers != quality || s.Providers != speed || o.Providers != len(online) {
		return
	}
	a.owner.current = snapshot
}

func (c *ArinShadowScoreCapture) Snapshot() *ArinShadowScoreSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// Validate checks both local replacement and the actual committed Redis
// publication. Scrape time or a same-sized newer cohort cannot satisfy it.
func (s *ArinShadowScoreSnapshot) Validate(ctx context.Context) bool {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return false
	}
	s.owner.mu.Lock()
	current := !s.owner.closed && s.owner.current == s && currentArinShadowScoreCapture.Load() == s.owner
	s.owner.mu.Unlock()
	if !current || server.NowUtc().Before(s.sourceCompletedAt) || server.NowUtc().Sub(s.sourceCompletedAt) > server.ArinShadowCaptureMaxAge {
		return false
	}
	census, err := GetClientScoreNativeCensus(ctx)
	return err == nil && arinShadowNativeGeneration(census) == s.generation
}

func (s *ArinShadowScoreSnapshot) member(clientId server.Id, connections int) server.ArinShadowCaptureProvider {
	member, exists := s.members[clientId]
	if !exists {
		return server.ArinShadowCaptureProvider{ClientId: clientId, ExpectedConnections: connections, Buckets: []string{"all"}, MembershipUnavailable: true}
	}
	member.ExpectedConnections = connections
	member.Buckets = slices.Clone(member.Buckets)
	return member
}
