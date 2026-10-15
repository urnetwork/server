// The capability probe's sweep page and eligibility against the database.
package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

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

// Through the database (review F8): a newer reading that is not modern does
// not replace modern evidence that is still fresh until the caller confirms
// the downgrade, and nothing about the row changes while it is held. A
// stale modern row, a row that is not modern, and a provider without a row
// take the reading at once; a modern reading is never held, and a reading
// at the row's own instant keeps the less modern of the two as before.
func TestRecordProviderModernEvidenceHoldsAnUnconfirmedDowngrade(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		read := func(clientId server.Id) *ProviderModernEvidenceRecord {
			return getProviderModernEvidenceRecords(ctx, []server.Id{clientId})[clientId]
		}
		record := func(clientId server.Id, postQuantum bool, windowResizable bool, observedAt time.Time, confirmed bool) bool {
			held, err := RecordProviderModernEvidenceHoldingDowngrade(ctx, clientId, modernGateTestEvidence(postQuantum, windowResizable, observedAt), confirmed)
			if err != nil {
				t.Fatal(err)
			}
			return held
		}
		freshModernAt := now.Add(-80 * time.Hour)

		modern := server.NewId()
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, modern, modernGateTestEvidence(true, true, freshModernAt)), nil)
		before := read(modern)
		for _, negative := range []struct {
			postQuantum     bool
			windowResizable bool
		}{
			{postQuantum: false, windowResizable: true},
			{postQuantum: true, windowResizable: false},
			{postQuantum: false, windowResizable: false},
		} {
			if !record(modern, negative.postQuantum, negative.windowResizable, now.Add(-time.Minute), false) {
				t.Fatalf("an unconfirmed %+v reading replaced fresh modern evidence", negative)
			}
			connect.AssertEqual(t, read(modern), before)
		}
		// a modern reading is never held, and refreshes the row
		connect.AssertEqual(t, record(modern, true, true, now.Add(-2*time.Minute), false), false)
		if refreshed := read(modern); !refreshed.PostQuantum || !refreshed.WindowResizable || !refreshed.MeasuredAt.Equal(now.Add(-2*time.Minute)) {
			t.Fatalf("a modern reading stored %+v", refreshed)
		}
		// a confirmed downgrade is written
		connect.AssertEqual(t, record(modern, false, true, now.Add(-time.Minute), true), false)
		downgraded := read(modern)
		if downgraded.PostQuantum || !downgraded.WindowResizable || !downgraded.MeasuredAt.Equal(now.Add(-time.Minute)) || downgraded.RunCount != 3 {
			t.Fatalf("a confirmed downgrade stored %+v", downgraded)
		}

		// stale modern evidence, other evidence and no evidence take the reading
		staleModern, legacy, unevidenced := server.NewId(), server.NewId(), server.NewId()
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, staleModern, modernGateTestEvidence(true, true, now.Add(-ProviderModernEvidenceMaxAge-time.Hour))), nil)
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, legacy, modernGateTestEvidence(false, true, freshModernAt)), nil)
		for _, clientId := range []server.Id{staleModern, legacy, unevidenced} {
			if record(clientId, false, false, now.Add(-time.Minute), false) {
				t.Fatal("a reading was held back from evidence that is not fresh and modern")
			}
			if stored := read(clientId); stored == nil || stored.PostQuantum || stored.WindowResizable || !stored.MeasuredAt.Equal(now.Add(-time.Minute)) {
				t.Fatalf("an unheld reading stored %+v", stored)
			}
		}

		// a conflicting reading at the modern row's own instant is not a
		// downgrade to hold: each capability keeps the less modern reading
		sameInstant := server.NewId()
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, sameInstant, modernGateTestEvidence(true, true, freshModernAt)), nil)
		connect.AssertEqual(t, record(sameInstant, true, false, freshModernAt, false), false)
		if stored := read(sameInstant); !stored.PostQuantum || stored.WindowResizable || stored.RunCount != 1 {
			t.Fatalf("a same-instant reading stored %+v", stored)
		}
	})
}
