// A long-lived score page cannot extend the lifetime of its post-quantum proof.
package model

import (
	"maps"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Drives the real exporter decision with fixed snapshot and proof times.
func postQuantumExpiryCapability(clientId server.Id, now time.Time, positiveAt time.Time, windowAt time.Time) *clientScoreCapability {
	return newClientScoreCapability(clientId, ClientScoreIpFamilyV4, &clientScoreEgress{}, providerCountFilter{
		now: now, healthWindowEnd: now,
		healthCounts: map[server.Id]ProviderEgressHealthCounts{clientId: {
			MeasuredAt: now, OKCount: 10, Total: 10, PostQuantumPositiveAt: &positiveAt,
		}},
		modernEvidence: map[server.Id]*ProviderModernEvidenceRecord{clientId: {MeasuredAt: windowAt, WindowResizable: true}},
	}, DefaultEgressIndexSettings(), false)
}

// Every exact deadline is stale, including modern-window expiry and stalled
// snapshots. Both class and legacy filters read decoded immutable score pages.
func TestPostQuantumExportProofBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	clientId := server.NewId()
	for _, c := range []struct {
		name                 string
		positiveAt, windowAt time.Time
		deadline             time.Time
		modern               bool
	}{
		{name: "positive expiry", positiveAt: now.Add(-ProviderEgressHealthMaxAge + time.Minute), windowAt: now, deadline: now.Add(time.Minute)},
		{name: "snapshot staleness", positiveAt: now, windowAt: now, deadline: now.Add(DefaultEgressIndexSettings().PostQuantumMaxStaleness)},
		{name: "modern window expiry", positiveAt: now, windowAt: now.Add(-ProviderModernEvidenceMaxAge + time.Minute), deadline: now.Add(time.Minute), modern: true},
	} {
		capability := postQuantumExpiryCapability(clientId, now, c.positiveAt, c.windowAt)
		score := clientScoreForTest(clientId, ClientScoreIpFamilyV4)
		capability.setPostQuantum(score)
		score.Modern, score.Online = capability.postQuantumModern, true
		payload := exportClientScores(true, RankModeQuality, map[server.Id]*ClientScore{clientId: score})
		decoded, err := decodeClientScoreSample(payload.encodeSample(0))
		if err != nil {
			t.Fatal(err)
		}
		for _, at := range []time.Time{c.deadline.Add(-time.Nanosecond), c.deadline, c.deadline.Add(time.Nanosecond)} {
			for _, selector := range []bool{false, true} {
				filter := clientScoreRequestFilter{facets: []ipFamilyFacet{ipFamilyFacetV4Only}, requirePostQuantumPositive: true, requireModern: c.modern, now: at}
				if selector {
					filter.capabilityClasses = newProviderCapabilityClassSet(providerCapabilityClassB)
				}
				scores := map[server.Id]*ClientScore{clientId: decoded[0]}
				filter.apply(scores, &findProviders2SelectionObservation{})
				if (scores[clientId] != nil) != at.Before(c.deadline) {
					t.Errorf("%s selector %t at %s: current %t", c.name, selector, at, scores[clientId] != nil)
				}
			}
		}
	}
	// Older gob snapshots contain the flag but no deadline. Before the ready
	// marker they retain legacy behavior; after it they cannot prove freshness.
	score := clientScoreForTest(clientId, ClientScoreIpFamilyV4)
	score.PostQuantumPositive, score.Modern = true, true
	for _, ready := range []bool{false, true} {
		filter := clientScoreRequestFilter{facets: []ipFamilyFacet{ipFamilyFacetV4Only}, requirePostQuantumPositive: ready, now: now}
		if (filter.rejection(clientId, score) == 0) == ready {
			t.Errorf("old snapshot: ready %t", ready)
		}
	}
}

// Named requests read the atomically published proof hash beside the legacy
// sets. Missing/malformed proof and old-writer membership removal fail closed.
func TestNamedPostQuantumExportProofBoundaries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
		clientId := server.NewId()
		capability := postQuantumExpiryCapability(clientId, now, now.Add(-ProviderEgressHealthMaxAge+time.Minute), now)
		deadline := now.Add(time.Minute)
		if err := publishProviderCapabilitySets(ctx, map[server.Id]*clientScoreCapability{clientId: capability}, map[server.Id]uint8{clientId: ClientScoreIpFamilyV4}, 5*time.Hour); err != nil {
			t.Fatal(err)
		}
		clientSession := &session.ClientSession{Ctx: ctx}
		for _, at := range []time.Time{deadline.Add(-time.Nanosecond), deadline, deadline.Add(time.Nanosecond)} {
			positive, err := readFindProviders2PostQuantumEncryption(clientSession, true, nil, []server.Id{clientId}, at)
			if err != nil {
				t.Fatal(err)
			}
			modern, err := readFindProviders2NamedModern(clientSession, true, []server.Id{clientId}, at)
			if err != nil {
				t.Fatal(err)
			}
			if positive[clientId] != at.Before(deadline) || modern[clientId] != at.Before(deadline) {
				t.Errorf("at %s: positive %v modern %v", at, positive, modern)
			}
		}
		for _, key := range []string{providerPostQuantumPositiveKey, providerPostQuantumModernKey} {
			for _, mode := range []string{"missing proof", "malformed proof", "membership removed", "ready removed"} {
				if err := publishProviderCapabilitySets(ctx, map[server.Id]*clientScoreCapability{clientId: capability}, map[server.Id]uint8{clientId: ClientScoreIpFamilyV4}, 5*time.Hour); err != nil {
					t.Fatal(err)
				}
				server.Redis(ctx, func(r server.RedisClient) {
					switch mode {
					case "missing proof":
						server.Raise(r.Del(ctx, providerPostQuantumProofKey(key)).Err())
					case "malformed proof":
						server.Raise(r.HSet(ctx, providerPostQuantumProofKey(key), clientId.String(), "not a deadline").Err())
					case "membership removed":
						server.Raise(r.SRem(ctx, key, clientId.String()).Err())
					case "ready removed":
						server.Raise(r.SRem(ctx, key, providerCapabilitySetReadyMember).Err())
					}
				})
				members, err := readProviderPostQuantumSet(ctx, key, []server.Id{clientId}, now)
				if err != nil || members[clientId] {
					t.Errorf("%s %s: %v, %v", key, mode, members, err)
				}
			}
		}
	})
}

// The public request path enforces the proof in legacy and selector draws,
// named specs, modern requests, and Online fallback. Warm score pages cannot
// turn the old positive flag into authority once its deadline is expired.
func TestFindProviders2ExpiredPostQuantumInEveryDraw(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Cleanup(clientScorePostQuantumReadyState.reset)
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		now := time.Now()
		locationId := server.NewId()
		fresh := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		expired := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		old := nativeTestScore(RankModeQuality, ipFamilyFacetV4Only)
		capabilities := map[server.Id]*clientScoreCapability{}
		families := map[server.Id]uint8{}
		for _, score := range []*ClientScore{fresh, expired, old} {
			at, positiveAt := now, now
			if score == expired {
				at, positiveAt = now.Add(-2*time.Minute), now.Add(-ProviderEgressHealthMaxAge-time.Minute)
			}
			capability := postQuantumExpiryCapability(score.ClientId, at, positiveAt, now)
			capability.setPostQuantum(score)
			score.Modern, score.Online = true, true
			if score == old {
				score.PostQuantumValidUntil = time.Time{}
				score.PostQuantumModernValidUntil = time.Time{}
			}
			capabilities[score.ClientId], families[score.ClientId] = capability, ClientScoreIpFamilyV4
		}
		scores := []*ClientScore{fresh, expired, old}
		writeSubscriberFactsForScores(ctx, scores)
		modernTestPublishLocation(t, locationId, scores)
		if err := publishProviderCapabilitySets(ctx, capabilities, families, 5*time.Hour); err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, ClientScorePostQuantumReadyKey, "1", time.Hour).Err())
			for _, key := range []string{providerPostQuantumPositiveKey, providerPostQuantumModernKey} {
				server.Raise(r.HDel(ctx, providerPostQuantumProofKey(key), old.ClientId.String()).Err())
			}
		})
		clientScorePostQuantumReadyState.reset()
		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "proof-expiry", false, false))
		for _, selector := range []bool{false, true} {
			for _, modern := range []bool{false, true} {
				for _, named := range []bool{false, true} {
					args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &locationId}}, Count: 10, ForceCount: true, RankMode: RankModeQuality, PostQuantumEncryption: !modern, RequireModernProvider: modern}
					if selector {
						args.Select = capabilityTestSelect("dualstack", "dualproto", 10, 0, 1, 0)
					}
					if named {
						args.Specs = []*ProviderSpec{{ClientId: &fresh.ClientId}, {ClientId: &expired.ClientId}, {ClientId: &old.ClientId}}
					}
					result, err := FindProviders2(args, clientSession)
					if err != nil {
						t.Fatal(err)
					}
					ids := map[server.Id]bool{}
					for _, provider := range result.Providers {
						ids[provider.ClientId] = true
					}
					if !maps.Equal(ids, map[server.Id]bool{fresh.ClientId: true}) {
						t.Errorf("selector %t modern %t named %t: ids %v", selector, modern, named, ids)
					}
				}
			}
		}
	})
}

// A fresh ordinary URL result cannot renew a nearly expired positive from
// an earlier PQ turn. Exercise classification, gob export and the request read.
func TestPostQuantumExportCannotOutlivePositive(t *testing.T) {
	now := time.Now()
	exportedAt := now.Add(-2 * time.Minute)
	positiveAt := now.Add(-ProviderEgressHealthMaxAge - time.Minute)
	clientId := server.NewId()
	capability := newClientScoreCapability(clientId, ClientScoreIpFamilyV4,
		&clientScoreEgress{}, providerCountFilter{
			now: exportedAt, healthWindowEnd: exportedAt,
			healthCounts: map[server.Id]ProviderEgressHealthCounts{clientId: {
				MeasuredAt: exportedAt, OKCount: 10, Total: 10, PostQuantumPositiveAt: &positiveAt,
			}},
		}, DefaultEgressIndexSettings(), false)
	score := clientScoreForTest(clientId, ClientScoreIpFamilyV4)
	capability.setPostQuantum(score)
	score.Online = true
	payload := exportClientScores(true, RankModeQuality, map[server.Id]*ClientScore{clientId: score})
	decoded, err := decodeClientScoreSample(payload.encodeSample(0))
	if err != nil {
		t.Fatal(err)
	}
	capable, err := readFindProviders2PostQuantumEncryption(&session.ClientSession{Ctx: t.Context()}, true,
		map[server.Id]*ClientScore{clientId: decoded[0]}, []server.Id{clientId}, now)
	if err != nil {
		t.Fatal(err)
	}
	if capable[clientId] {
		t.Fatal("an exported positive remained eligible after its own eight-hour window despite a fresh ordinary URL result")
	}
}
