package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The capability probe's reads against the database: the sweep page and the
// eligibility of its due providers.

// A provider row of the sweep's population: a network client with the
// given activity and parent, a location reliability row connected or not
// (always valid), and a public provide key when public.
func capabilityProbeTestProvider(ctx context.Context, clientId server.Id, active bool, sourceClientId *server.Id, connected bool, public bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx,
			`INSERT INTO network_client (client_id, network_id, active, source_client_id) VALUES ($1, $2, $3, $4)`,
			clientId, server.NewId(), active, sourceClientId,
		))
		server.RaisePgResult(tx.Exec(ctx,
			`INSERT INTO network_client_location_reliability (client_id, update_block_number, country_location_id, client_address_hash_count, location_count, connected) VALUES ($1, 0, $2, 1, 1, $3)`,
			clientId, server.NewId(), connected,
		))
		if public {
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO provide_key (client_id, provide_mode, secret_key) VALUES ($1, $2, $3)`,
				clientId, ProvideModePublic, []byte("synthetic-public-secret"),
			))
		}
	})
}

// The sweep pages the connected, valid providers in client id order after
// the cursor, every one of them, and marks due exactly the active top-level
// public providers without evidence, with modern evidence older than the
// modern refresh time, or with other evidence older than the other refresh
// time. A short page is the population's end.
func TestCapabilityProbePageMarksDueProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		ids := make([]server.Id, 9)
		for index := range ids {
			ids[index] = server.NewId()
		}
		slices.SortFunc(ids, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		unevidenced, freshModern, oldModern, legacy, networkOnly, derived, inactive, disconnected, freshLegacy :=
			ids[0], ids[1], ids[2], ids[3], ids[4], ids[5], ids[6], ids[7], ids[8]
		for _, clientId := range []server.Id{unevidenced, freshModern, oldModern, legacy, freshLegacy} {
			capabilityProbeTestProvider(ctx, clientId, true, nil, true, true)
		}
		capabilityProbeTestProvider(ctx, networkOnly, true, nil, true, false)
		parent := server.NewId()
		capabilityProbeTestProvider(ctx, derived, true, &parent, true, true)
		capabilityProbeTestProvider(ctx, inactive, false, nil, true, true)
		capabilityProbeTestProvider(ctx, disconnected, true, nil, false, true)
		record := func(clientId server.Id, postQuantum bool, windowResizable bool, age time.Duration) {
			connect.AssertEqual(t, RecordProviderModernEvidence(ctx, clientId, modernGateTestEvidence(postQuantum, windowResizable, now.Add(-age))), nil)
		}
		record(freshModern, true, true, time.Hour)
		record(oldModern, true, true, 80*time.Hour)
		record(legacy, false, false, 30*time.Hour)
		record(freshLegacy, false, true, time.Hour)

		modernRefreshBefore := now.Add(-72 * time.Hour)
		otherRefreshBefore := now.Add(-24 * time.Hour)
		page := GetProviderCapabilityProbePage(ctx, server.Id{}, 100, modernRefreshBefore, otherRefreshBefore)
		want := []ProviderCapabilityProbeCandidate{
			{ClientId: unevidenced, Due: true},
			{ClientId: freshModern, Due: false},
			{ClientId: oldModern, Due: true},
			{ClientId: legacy, Due: true},
			{ClientId: networkOnly, Due: false},
			{ClientId: derived, Due: false},
			{ClientId: inactive, Due: false},
			{ClientId: freshLegacy, Due: false},
		}
		connect.AssertEqual(t, page, want)

		// non-modern evidence refreshed on the modern schedule is not due yet
		later := GetProviderCapabilityProbePage(ctx, server.Id{}, 100, modernRefreshBefore, modernRefreshBefore)
		connect.AssertEqual(t, later[3], ProviderCapabilityProbeCandidate{ClientId: legacy, Due: false})

		// pages continue after the cursor, and a short page is the end
		first := GetProviderCapabilityProbePage(ctx, server.Id{}, 3, modernRefreshBefore, otherRefreshBefore)
		connect.AssertEqual(t, first, want[:3])
		second := GetProviderCapabilityProbePage(ctx, first[len(first)-1].ClientId, 3, modernRefreshBefore, otherRefreshBefore)
		connect.AssertEqual(t, second, want[3:6])
		third := GetProviderCapabilityProbePage(ctx, second[len(second)-1].ClientId, 3, modernRefreshBefore, otherRefreshBefore)
		connect.AssertEqual(t, third, want[6:])
		connect.AssertEqual(t, len(GetProviderCapabilityProbePage(ctx, third[len(third)-1].ClientId, 3, modernRefreshBefore, otherRefreshBefore)), 0)

		// a page size outside the bounds is clamped, never unbounded
		connect.AssertEqual(t, len(GetProviderCapabilityProbePage(ctx, server.Id{}, 0, modernRefreshBefore, otherRefreshBefore)), 1)
	})
}

// Through the database, a provider whose accepted URL outcomes pass the ratio
// is eligible, gate on included, and one without outcomes or failing them is
// not.
func TestCapabilityProbeEligibilityReadsTheDecisionsEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		enableModernProviderGate(t)
		now := server.NowUtc().Truncate(time.Microsecond)
		passing, failing, unprobed := server.NewId(), server.NewId(), server.NewId()
		submit := func(clientId server.Id, okCount int) {
			for index := range 5 {
				ok := 0
				if index < okCount {
					ok = 1
				}
				measuredAt := now.Add(-time.Duration(index+1) * time.Minute)
				SetProviderEgressHealth(ctx, &ProviderEgressHealth{
					RunId:            server.NewId(),
					ClientId:         clientId,
					MeasuredAt:       measuredAt,
					OKCount:          ok,
					Total:            1,
					UrlProbeEvidence: fp2TestUrlProbeEvidence(measuredAt, ok == 1),
				})
			}
		}
		submit(passing, 5)
		submit(failing, 1)
		eligible := FilterProviderCapabilityProbeEligible(ctx, []server.Id{unprobed, failing, passing})
		connect.AssertEqual(t, eligible, []server.Id{passing})
		connect.AssertEqual(t, FilterProviderCapabilityProbeEligible(ctx, nil), []server.Id{})
	})
}
