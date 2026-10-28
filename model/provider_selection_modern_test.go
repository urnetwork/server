package model

// Modern providers as the score export and the named read decide them, over
// real providers (FP2-MODERN.md, "Definition"): with a Public key by the
// capability probe's evidence, with only a Network key by the declaration,
// and with neither key never, whatever declaration an earlier provide left.

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

var modernTestNetworkOnlyKeys = map[ProvideMode][]byte{
	ProvideModeNetwork: []byte("network-secret"),
}

func TestRequireModernProviderClassifiesNamedProvidersByTheirKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Named Modern City", "Named Modern Region", "United States", "us")
		connectProvider := func(modes map[ProvideMode][]byte) server.Id {
			return egressTestConnect(ctx, t, city, egressTestFast, modes, nil).clientId
		}

		// Public and Network, measured modern by the probe
		measured := connectProvider(nil)
		egressTestHealth(ctx, measured, server.NowUtc(), 60, 0)
		// Public and Network, declaring but never measured: Public needs evidence
		declaredOnly := connectProvider(nil)
		SetProvideWithCapabilities(ctx, declaredOnly, egressTestPublicAndNetwork, ProvideCapabilities{PostQuantumEncryption: true})
		// Network only, declaring: the probe cannot reach it
		networkDeclared := connectProvider(modernTestNetworkOnlyKeys)
		SetProvideWithCapabilities(ctx, networkDeclared, modernTestNetworkOnlyKeys, ProvideCapabilities{PostQuantumEncryption: true})
		networkUndeclared := connectProvider(modernTestNetworkOnlyKeys)
		// Public with a declaration, then a return-traffic provide with
		// neither key, which leaves the declaration as it was
		retained := connectProvider(nil)
		SetProvideWithCapabilities(ctx, retained, map[ProvideMode][]byte{ProvideModePublic: []byte("public-secret")}, ProvideCapabilities{PostQuantumEncryption: true})
		SetProvideWithCapabilities(ctx, retained, map[ProvideMode][]byte{ProvideModeStream: []byte("stream-secret")}, ProvideCapabilities{})
		capableClientIds, err := readProvidePostQuantumEncryption(ctx, []server.Id{retained})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, capableClientIds[retained], true)

		want := map[server.Id]bool{
			measured:          true,
			declaredOnly:      false,
			networkDeclared:   true,
			networkUndeclared: false,
			retained:          false,
		}
		modernClientIds, err := readProviderModernClientIds(ctx, slices.Collect(maps.Keys(want)), server.NowUtc())
		connect.AssertEqual(t, err, nil)
		for clientId, modern := range want {
			connect.AssertEqual(t, modernClientIds[clientId], modern)
		}

		// named alone, and named beside a discovery spec
		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "named-modern", false, false))
		for clientId, modern := range want {
			for _, specs := range [][]*ProviderSpec{
				{{ClientId: &clientId}},
				{{ClientId: &clientId}, {LocationId: &city.LocationId}},
			} {
				result, err := FindProviders2(&FindProviders2Args{Specs: specs, RankMode: RankModeSpeed, RequireModernProvider: true}, clientSession)
				connect.AssertEqual(t, err, nil)
				answered := false
				for _, provider := range result.Providers {
					if provider.ClientId == clientId {
						answered = true
					}
				}
				if answered != modern {
					t.Errorf("named provider modern=%t answered=%t with %d specs", modern, answered, len(specs))
				}
			}
			// a request that does not require modern providers answers it
			result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{ClientId: &clientId}}, RankMode: RankModeSpeed}, clientSession)
			connect.AssertEqual(t, err, nil)
			if clientId != retained {
				connect.AssertEqual(t, len(result.Providers), 1)
			}
		}

		// a read that cannot run is the request's error
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		_, err = readProviderModernClientIds(canceledCtx, []server.Id{measured}, server.NowUtc())
		if err == nil {
			t.Fatal("a canceled named read did not fail")
		}
		// the declaration read beside it returns the same failure rather than
		// raising it
		_, err = readProvidePostQuantumEncryption(canceledCtx, []server.Id{measured})
		if err == nil {
			t.Fatal("a canceled declaration read did not fail")
		}
	})
}

// The export sets the flag on every page it writes: from the probe's evidence
// for a Public provider and from the declaration for a network-only one, and
// marks the index modern-ready once the export completes.
func TestUpdateClientScoresPublishesTheModernFlag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Modern Export City", "Modern Export Region", "United States", "us")

		modernPublic := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, modernPublic, city, 0, "us")
		otherPublic := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, otherPublic, city, 0, "us")
		egressTestForgetModernEvidence(ctx, otherPublic.clientId)
		networkDeclared := egressTestConnect(ctx, t, city, egressTestFast, modernTestNetworkOnlyKeys, nil)
		SetProvideWithCapabilities(ctx, networkDeclared.clientId, modernTestNetworkOnlyKeys, ProvideCapabilities{PostQuantumEncryption: true})
		networkUndeclared := egressTestConnect(ctx, t, city, egressTestFast, modernTestNetworkOnlyKeys, nil)

		exportStart := server.NowUtc()
		egressTestPasses(ctx, t)

		want := map[server.Id]bool{
			modernPublic.clientId:      true,
			otherPublic.clientId:       false,
			networkDeclared.clientId:   true,
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
						t.Errorf("mode %s force_minimum %t: modern=%t, want %t", rankMode, forceMinimum, clientScore.Modern, modern)
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
