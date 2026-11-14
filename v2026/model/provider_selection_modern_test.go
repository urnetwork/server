package model

// Modern and post-quantum providers as the score export and the named reads
// decide them before the post-quantum marker, over real providers
// (FP2-MODERN.md, "Definition"; FP2-PROTOCOL.md §12.4): by the probe's
// evidence alone, whatever provide keys a provider holds, never by its own
// declaration, and with neither key never (owner, 2026-11-13: "They must have
// a positive quality probe handshake to count").

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

var modernTestNetworkOnlyKeys = map[ProvideMode][]byte{
	ProvideModeNetwork: []byte("network-secret"),
}

// Records a probe's observation of the provider measured at observedAt:
// post-quantum or classical, with resizable windows or without.
func modernTestMeasureAt(ctx context.Context, clientId server.Id, postQuantum bool, windowResizable bool, observedAt time.Time) {
	keyExchange := "X25519"
	if postQuantum {
		keyExchange = "X25519MLKEM768"
	}
	maxReceiveWindowByteCount := ByteCount(0)
	if windowResizable {
		maxReceiveWindowByteCount = 4 * 1024 * 1024
	}
	server.Raise(RecordProviderModernEvidence(ctx, clientId, ProviderModernEvidence{
		E2eNegotiated:             true,
		KeyExchange:               keyExchange,
		PostQuantum:               postQuantum,
		WindowAdvertised:          windowResizable,
		MaxReceiveWindowByteCount: maxReceiveWindowByteCount,
		ObservedAt:                observedAt,
	}))
}

// The same, measured now.
func modernTestMeasure(ctx context.Context, clientId server.Id, postQuantum bool, windowResizable bool) {
	modernTestMeasureAt(ctx, clientId, postQuantum, windowResizable, server.NowUtc())
}

// Whether a request answered the provider.
func modernTestAnswered(providers []*FindProvidersProvider, clientId server.Id) bool {
	for _, provider := range providers {
		if provider.ClientId == clientId {
			return true
		}
	}
	return false
}

// A provider named by id is modern, or post-quantum, by its evidence row
// alone. A network-only provider that declares post-quantum encryption but
// was never measured is neither: that is the hole the owner closed on
// 2026-11-13, where the named read took a network-only provider's claim for
// evidence. One measured modern is both, whether it declares or not.
func TestRequireModernProviderClassifiesNamedProvidersByTheirEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Named Modern City", "Named Modern Region", "United States", "us")
		connectProvider := func(modes map[ProvideMode][]byte) server.Id {
			return egressTestConnect(ctx, t, city, egressTestFast, modes, nil).clientId
		}
		postQuantumDeclared := ProvideCapabilities{PostQuantumEncryption: true}

		// Public and Network, measured modern by the probe
		measured := connectProvider(nil)
		egressTestHealth(ctx, measured, server.NowUtc(), 60, 0)
		// Public and Network, declaring but never measured
		declaredOnly := connectProvider(nil)
		SetProvideWithCapabilities(ctx, declaredOnly, egressTestPublicAndNetwork, postQuantumDeclared)
		// Network only, declaring but never measured: its claim alone
		networkDeclared := connectProvider(modernTestNetworkOnlyKeys)
		SetProvideWithCapabilities(ctx, networkDeclared, modernTestNetworkOnlyKeys, postQuantumDeclared)
		// Network only, measured modern, declaring nothing
		networkMeasured := connectProvider(modernTestNetworkOnlyKeys)
		modernTestMeasure(ctx, networkMeasured, true, true)
		// Network only, declaring, measured post-quantum without resizable
		// windows: post-quantum, not modern
		networkPostQuantum := connectProvider(modernTestNetworkOnlyKeys)
		SetProvideWithCapabilities(ctx, networkPostQuantum, modernTestNetworkOnlyKeys, postQuantumDeclared)
		modernTestMeasure(ctx, networkPostQuantum, true, false)
		// Network only, declaring, its newest observation classical
		networkClassical := connectProvider(modernTestNetworkOnlyKeys)
		SetProvideWithCapabilities(ctx, networkClassical, modernTestNetworkOnlyKeys, postQuantumDeclared)
		modernTestMeasure(ctx, networkClassical, true, true)
		modernTestMeasure(ctx, networkClassical, false, true)
		// Network only, declaring, measured modern eight days ago: stale
		networkStale := connectProvider(modernTestNetworkOnlyKeys)
		SetProvideWithCapabilities(ctx, networkStale, modernTestNetworkOnlyKeys, postQuantumDeclared)
		modernTestMeasureAt(ctx, networkStale, true, true, server.NowUtc().Add(-ProviderModernEvidenceMaxAge-24*time.Hour))
		networkUndeclared := connectProvider(modernTestNetworkOnlyKeys)
		// Public with a declaration and evidence, then a return-traffic
		// provide with neither key: neither the declaration nor the evidence
		// left behind counts
		retained := connectProvider(nil)
		SetProvideWithCapabilities(ctx, retained, map[ProvideMode][]byte{ProvideModePublic: []byte("public-secret")}, postQuantumDeclared)
		modernTestMeasure(ctx, retained, true, true)
		SetProvideWithCapabilities(ctx, retained, map[ProvideMode][]byte{ProvideModeStream: []byte("stream-secret")}, ProvideCapabilities{})
		connect.AssertEqual(t, provideCapabilityTestDeclared(ctx, t, retained), true)

		wantModern := map[server.Id]bool{
			measured:           true,
			declaredOnly:       false,
			networkDeclared:    false,
			networkMeasured:    true,
			networkPostQuantum: false,
			networkClassical:   false,
			networkStale:       false,
			networkUndeclared:  false,
			retained:           false,
		}
		wantPostQuantum := maps.Clone(wantModern)
		wantPostQuantum[networkPostQuantum] = true
		clientIds := slices.Collect(maps.Keys(wantModern))

		now := server.NowUtc()
		modernClientIds, err := readProviderModernClientIds(ctx, clientIds, now)
		connect.AssertEqual(t, err, nil)
		postQuantumClientIds, err := readProviderPostQuantumEvidenceClientIds(ctx, clientIds, now)
		connect.AssertEqual(t, err, nil)
		for _, clientId := range clientIds {
			if modernClientIds[clientId] != wantModern[clientId] {
				t.Errorf("named read: modern=%t, want %t", modernClientIds[clientId], wantModern[clientId])
			}
			if postQuantumClientIds[clientId] != wantPostQuantum[clientId] {
				t.Errorf("named read: post-quantum=%t, want %t", postQuantumClientIds[clientId], wantPostQuantum[clientId])
			}
		}

		// named alone, and named beside a discovery spec, by each field; the
		// post-quantum marker is not set
		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "named-modern", false, false))
		for _, clientId := range clientIds {
			for _, specs := range [][]*ProviderSpec{
				{{ClientId: &clientId}},
				{{ClientId: &clientId}, {LocationId: &city.LocationId}},
			} {
				result, err := FindProviders2(&FindProviders2Args{Specs: specs, RankMode: RankModeSpeed, RequireModernProvider: true}, clientSession)
				connect.AssertEqual(t, err, nil)
				if answered := modernTestAnswered(result.Providers, clientId); answered != wantModern[clientId] {
					t.Errorf("require_modern_provider: named provider modern=%t answered=%t with %d specs", wantModern[clientId], answered, len(specs))
				}
				result, err = FindProviders2(&FindProviders2Args{Specs: specs, RankMode: RankModeSpeed, PostQuantumEncryption: true}, clientSession)
				connect.AssertEqual(t, err, nil)
				if answered := modernTestAnswered(result.Providers, clientId); answered != wantPostQuantum[clientId] {
					t.Errorf("post_quantum_encryption: named provider post-quantum=%t answered=%t with %d specs", wantPostQuantum[clientId], answered, len(specs))
				}
			}
			// a request that requires neither answers it
			result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{ClientId: &clientId}}, RankMode: RankModeSpeed}, clientSession)
			connect.AssertEqual(t, err, nil)
			if clientId != retained {
				connect.AssertEqual(t, len(result.Providers), 1)
			}
		}

		// an empty read reads nothing
		emptyClientIds, err := readProviderPostQuantumEvidenceClientIds(ctx, nil, now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(emptyClientIds), 0)

		// a read that cannot run is the request's error, returned rather than
		// raised
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		_, err = readProviderModernClientIds(canceledCtx, []server.Id{measured}, now)
		if err == nil {
			t.Fatal("a canceled named modern read did not fail")
		}
		_, err = readProviderPostQuantumEvidenceClientIds(canceledCtx, []server.Id{measured}, now)
		if err == nil {
			t.Fatal("a canceled named post-quantum read did not fail")
		}
	})
}

// The export sets the flag on every page it writes from the probe's evidence,
// whatever provide keys the provider holds: a network-only provider is modern
// only when measured modern, never by its declaration. It marks the index
// modern-ready once the export completes.
func TestUpdateClientScoresPublishesTheModernFlag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Modern Export City", "Modern Export Region", "United States", "us")

		modernPublic := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, modernPublic, city, 0, "us")
		otherPublic := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, otherPublic, city, 0, "us")
		egressTestForgetModernEvidence(ctx, otherPublic.clientId)
		// declaring, never measured: its claim alone
		networkDeclared := egressTestConnect(ctx, t, city, egressTestFast, modernTestNetworkOnlyKeys, nil)
		SetProvideWithCapabilities(ctx, networkDeclared.clientId, modernTestNetworkOnlyKeys, ProvideCapabilities{PostQuantumEncryption: true})
		// measured modern, declaring nothing
		networkMeasured := egressTestConnect(ctx, t, city, egressTestFast, modernTestNetworkOnlyKeys, nil)
		modernTestMeasure(ctx, networkMeasured.clientId, true, true)
		networkUndeclared := egressTestConnect(ctx, t, city, egressTestFast, modernTestNetworkOnlyKeys, nil)

		exportStart := server.NowUtc()
		egressTestPasses(ctx, t)

		want := map[server.Id]bool{
			modernPublic.clientId:      true,
			otherPublic.clientId:       false,
			networkDeclared.clientId:   false,
			networkMeasured.clientId:   true,
			networkUndeclared.clientId: false,
		}
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			for _, forceMinimum := range []bool{false, true} {
				clientScores := egressTestCachedScores(ctx, t, city, rankMode, forceMinimum)
				for clientId, modern := range want {
					clientScore := clientScores[clientId]
					if clientScore == nil {
						t.Fatalf("mode %s force_minimum %t: a provider is missing from the pages", rankMode, forceMinimum)
					}
					if clientScore.Modern != modern {
						t.Errorf("mode %s force_minimum %t: network-only=%t modern=%t, want %t", rankMode, forceMinimum, clientScore.NetworkOnly, clientScore.Modern, modern)
					}
				}
			}
		}

		sourceStartedAt, ready, err := GetClientScoreModernReady(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, ready, true)
		if sourceStartedAt.Before(exportStart.Add(-time.Second)) || server.NowUtc().Before(sourceStartedAt) {
			t.Fatalf("modern-ready marker %s is not the latest export's start", sourceStartedAt)
		}
	})
}
