package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// This is a current-policy discriminator, not an eligibility policy change.
// A short all-valid history differs from both missing and observed-bad history.
func TestPrivateHealthyShortHistoryPublicationChangesEligibility(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Synthetic City", "Synthetic Region", "Synthetic Country", "zz")
		closed := server.NowUtc().Truncate(ReliabilityBlockDuration).Add(-ReliabilityBlockDuration)
		valid := ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1, ReceiveMessageCount: 1}
		type fixture struct {
			label           string
			client, network server.Id
			observations    int64
			wantAfter       bool
		}
		providers := []fixture{
			{label: "full_healthy", observations: 721, wantAfter: true},
			{label: "short_healthy", observations: 1},
			{label: "missing", wantAfter: true},
		}
		for i := range providers {
			p := &providers[i]
			p.client, p.network = server.NewId(), server.NewId()
			address := testingConnectClientWithLocation(ctx, t, p.network, p.client, fmt.Sprintf("192.0.2.%d:0", i+101), city)
			SetProvide(ctx, p.client, egressTestPublicAndNetwork)
			egressTestHealth(ctx, p.client, closed, 3, 0)
			if p.observations > 0 {
				start := closed.Add(-time.Duration(p.observations-1) * ReliabilityBlockDuration)
				AddClientReliabilityStatsRange(ctx, p.network, p.client, address, start, closed, &valid)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var total, invalid int64
				var admitted bool
				server.Raise(conn.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE NOT valid) FROM client_reliability WHERE client_id=$1`, p.client).Scan(&total, &invalid))
				server.Raise(conn.QueryRow(ctx, "SELECT "+providerReliabilityEligibilitySql("$1::uuid"), p.client).Scan(&admitted))
				if total != p.observations || invalid != 0 || !admitted {
					t.Fatalf("%s setup: observed=%d invalid=%d before_admitted=%t", p.label, total, invalid, admitted)
				}
			})
		}
		UpdateClientReliabilityScores(ctx, closed, true)
		scores := GetAllClientReliabilityScores(ctx)
		for _, p := range providers {
			weights := map[int]float64{}
			for index, lookback := range ClientLookbacks {
				score, found := scores[index][p.client]
				if p.observations == 0 {
					if found {
						t.Fatal("missing control gained observed score")
					}
					continue
				}
				want := 1.0
				if p.label == "short_healthy" {
					want = 1.0 / float64(lookback/ReliabilityBlockDuration+1)
				}
				if !found || score.IndependentReliabilityWeight != want {
					t.Fatalf("%s score index=%d present=%t weight=%g want=%g", p.label, index, found, score.IndependentReliabilityWeight, want)
				}
				weights[index] = score.IndependentReliabilityWeight
			}
			var sqlAdmitted bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, "SELECT "+providerReliabilityEligibilitySql("$1::uuid"), p.client).Scan(&sqlAdmitted))
			})
			goAdmitted := providerReliabilityPasses(weights, providerReliabilityMinimums())
			if sqlAdmitted != p.wantAfter || goAdmitted != p.wantAfter {
				t.Fatalf("%s post-publication: SQL=%t Go=%t want=%t", p.label, sqlAdmitted, goAdmitted, p.wantAfter)
			}
			t.Logf("synthetic=%s all_raw_observations_valid=true before_admitted=true after_admitted=%t", p.label, p.wantAfter)
		}
		claimed := map[server.Id]bool{}
		for _, claim := range ClaimProviderUrlProbeDue(ctx, server.NowUtc().Add(time.Second), len(providers), 0, 1) {
			claimed[claim.ClientId] = true
		}
		for _, p := range providers {
			if claimed[p.client] != p.wantAfter {
				t.Fatalf("%s current URL claim=%t want=%t", p.label, claimed[p.client], p.wantAfter)
			}
		}
	})
}
