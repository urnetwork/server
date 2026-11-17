package model

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.11's counts (abuse_transparency_model.go; ABUSE-M5-17) on fixture rows: each total, a held network counted
// like any other, rows outside the window left out, and no network named. On the private stack with sidecar_pending.

func TestAbuseTransparencyCountsTotals(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fromTime := server.NowUtc().Add(-time.Hour)
		networkIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		for index, networkId := range networkIds {
			Testing_CreateNetwork(ctx, networkId, "transparency-"+strings.Repeat("a", index+1), server.NewId())
		}
		heldNetworkId := networkIds[2]
		settings := DefaultAbuseSettings()
		now := server.NowUtc()

		// three requests: two US subpoenas naming two networks, one sealed; an EU order, produced
		subpoena := testFileLegalRequest(t, ctx, LegalInstrumentSubpoena, true, networkIds[0])
		testFileLegalRequest(t, ctx, LegalInstrumentSubpoena, false, networkIds[0], networkIds[1])
		order, _, err := FileLegalRequest(ctx, &LegalRequestArgs{
			RequestId: server.NewId(), Instrument: LegalInstrumentEuProductionOrder, Country: "DE", NetworkIds: []server.Id{heldNetworkId},
		}, now, DefaultLegalSettings())
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			setLegalRequestProducedInTx(ctx, tx, order.RequestId)
		})

		openCase := func(networkId *server.Id, category AbuseCategory, joinKind AbuseJoinKind, target string) server.Id {
			var caseId server.Id
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				caseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
					NetworkId:     networkId,
					Category:      category,
					Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: target},
					ReporterClass: AbuseReporterClassStaff,
					JoinKind:      joinKind,
					JoinKey:       target,
				}, now, settings)
				if err != nil {
					t.Fatal(err)
				}
			})
			return caseId
		}
		act := func(caseId server.Id, kind AbuseActionKind, target string, byMachine bool) server.Id {
			actionId := server.NewId()
			reviewerId := server.NewId()
			args := &AbuseActionArgs{ActionId: actionId, CaseId: caseId, Kind: kind, Target: target}
			if !byMachine {
				args.ReviewerId = &reviewerId
			}
			server.Tx(ctx, func(tx server.PgTx) {
				if _, _, _, err := RecordAbuseActionInTx(ctx, tx, args, now, settings); err != nil {
					t.Fatal(err)
				}
			})
			return actionId
		}
		copyrightCase := openCase(&networkIds[0], AbuseCategoryCopyright, AbuseJoinKindHost, "music.copyright.example")
		act(copyrightCase, AbuseActionKindNoticeForward, "music.copyright.example", true)
		phishingCase := openCase(&networkIds[1], AbuseCategoryPhishing, AbuseJoinKindHost, "shop.phishing.example")
		kill := act(phishingCase, AbuseActionKindHostKill, "shop.phishing.example", true)
		server.Tx(ctx, func(tx server.PgTx) {
			reviewerId := server.NewId()
			if _, _, _, _, err := RecordAbuseLiftInTx(ctx, tx, &AbuseLiftArgs{
				ActionId: server.NewId(), CaseId: phishingCase, LiftedActionId: kill, ReviewState: AbuseReviewStateLiftedWrong, ReviewerId: &reviewerId,
			}, now, settings); err != nil {
				t.Fatal(err)
			}
		})
		csamCase := openCase(&heldNetworkId, AbuseCategoryCsam, AbuseJoinKindNetwork, heldNetworkId.String())
		act(csamCase, AbuseActionKindNetworkSuspend, heldNetworkId.String(), false)
		// a CSAM report a machine acted on is no NCMEC report
		machineCsamCase := openCase(&networkIds[1], AbuseCategoryCsam, AbuseJoinKindNetwork, networkIds[1].String())
		act(machineCsamCase, AbuseActionKindNetworkSuspend, networkIds[1].String(), true)
		openCase(nil, AbuseCategoryPhishing, AbuseJoinKindHost, "root.example")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO abuse_appeal (appeal_id, case_id, network_id, user_id, reason, state, create_time)
					VALUES ($1, $2, $3, $4, 'not mine', 'pending', now()), ($5, $2, $3, $4, 'still not mine', 'denied', now())
				`,
				server.NewId(), phishingCase, networkIds[1], server.NewId(), server.NewId(),
			))
			// the held network's hold, as placement leaves part 0
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO abuse_legal_hold (network_id, case_id, part, kind, scope, place_time, report_time, expire_time)
					VALUES ($1, $2, 0, 'report_2258a', '"network"', $3, $3, $4)
				`,
				heldNetworkId, csamCase, now, now.Add(365*24*time.Hour),
			))
			// a request filed before the window
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO legal_request (request_id, case_id, instrument, country, network_ids, production_state, create_time)
					VALUES ($1, $2, 'subpoena', 'US', $3, 'pending', $4)
				`,
				server.NewId(), subpoena.CaseId, []server.Id{networkIds[1]}, fromTime.Add(-24*time.Hour),
			))
		})

		reader, err := NewAbuseReader(server.NewId(), settings)
		if err != nil {
			t.Fatal(err)
		}
		counts, err := reader.TransparencyCounts(ctx, fromTime, server.NowUtc().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		requests := []string{}
		for _, request := range counts.Requests {
			requests = append(requests, request.Instrument+"/"+request.Country+"="+strings.Repeat("|", int(request.Count)))
		}
		if !slices.Equal(requests, []string{"eu_production_order/DE=|", "subpoena/US=||"}) || counts.RequestCount != 3 {
			t.Fatalf("requests %v (%d)", requests, counts.RequestCount)
		}
		if counts.AccountsAffected != 3 || counts.Productions != 1 || counts.SealingOrders != 1 {
			t.Fatalf("accounts %d, productions %d, sealing orders %d", counts.AccountsAffected, counts.Productions, counts.SealingOrders)
		}
		keyCounts := func(keyCounts []*AbuseTransparencyKeyCount) map[string]int64 {
			counted := map[string]int64{}
			for _, keyCount := range keyCounts {
				counted[keyCount.Key] = keyCount.Count
			}
			return counted
		}
		if holds := keyCounts(counts.Holds); len(holds) != 1 || holds["report_2258a"] != 1 {
			t.Fatalf("holds %v", holds)
		}
		if actions := keyCounts(counts.AbuseActions); len(actions) != 3 || actions["notice_forward"] != 1 || actions["host_kill"] != 1 || actions["network_suspend"] != 2 {
			t.Fatalf("actions %v", actions)
		}
		if appeals := keyCounts(counts.Appeals); len(appeals) != 2 || appeals["pending"] != 1 || appeals["denied"] != 1 {
			t.Fatalf("appeals %v", appeals)
		}
		if counts.DmcaNotices != 1 || counts.NcmecReports != 1 || counts.RootIncidents != 1 {
			t.Fatalf("dmca %d, ncmec %d, root incidents %d", counts.DmcaNotices, counts.NcmecReports, counts.RootIncidents)
		}
		countBytes, err := json.Marshal(counts)
		if err != nil {
			t.Fatal(err)
		}
		for _, networkId := range networkIds {
			if strings.Contains(string(countBytes), networkId.String()) {
				t.Fatal("the counts name a network")
			}
		}
		if kinds := testAbuseAccessLogKinds(ctx, nil); !slices.Contains(kinds, string(AbuseLookupKindCounts)) {
			t.Fatalf("no access log row for the counts: %v", kinds)
		}
		if _, err := reader.TransparencyCounts(ctx, fromTime, fromTime); !errors.Is(err, ErrAbuseTransparencyWindowInvalid) {
			t.Fatalf("an empty window: %v", err)
		}
	})
}
