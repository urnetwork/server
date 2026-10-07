// Differential SQL coverage preserves the existing tally meanings across
// mixed outcomes, duplicate names, canaries, places and UTC-day boundaries.
package model

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The unchanged direct writer is the oracle. Two coalesced pages must produce
// the same native SQL projections, including their conflicting-row additions.
func TestProviderEgressTallyBatchMatchesDirectWriter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		day := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
		first := ProviderEgressPlace{CountryCode: " ZZ ", Region: " Synthetic first "}
		second := ProviderEgressPlace{CountryCode: "ZY", Region: "Synthetic second"}
		records := []ProviderEgressTallyRecord{
			{MeasuredAt: day, Run: ProviderEgressRunTally{Place: first, Healthy: true}, Loads: []ProviderEgressSiteLoad{
				{Name: "alpha.example", Ok: true, Healthy: true},
				{Name: " alpha.example ", Healthy: true},
				{Name: "beta.example", Canary: true, Ok: true},
			}},
			{MeasuredAt: day.Add(time.Hour), Run: ProviderEgressRunTally{Place: second, EchoFailed: true}, Loads: []ProviderEgressSiteLoad{
				{Name: "alpha.example", Ok: true},
				{Name: "beta.example", Healthy: true},
			}},
			{MeasuredAt: day.Add(2 * time.Hour), Run: ProviderEgressRunTally{Place: first, EchoFailed: true}, Loads: []ProviderEgressSiteLoad{
				{Name: "alpha.example"},
				{Name: "beta.example", Canary: true},
				{Name: ""},
			}},
			{MeasuredAt: day.Add(3 * time.Hour), Run: ProviderEgressRunTally{Place: first, Healthy: true, EchoFailed: true}, Loads: []ProviderEgressSiteLoad{
				{Name: "alpha.example", Healthy: true},
				{Name: "beta.example", Ok: true, Healthy: true},
			}},
			{MeasuredAt: time.Date(2026, 10, 6, 23, 30, 0, 0, time.FixedZone("synthetic", -2*60*60)), Run: ProviderEgressRunTally{Place: first, Healthy: true}, Loads: []ProviderEgressSiteLoad{
				{Name: "alpha.example", Canary: true, Ok: true},
				{Name: "alpha.example", Canary: true},
				{Name: "beta.example", Ok: true},
			}},
			{MeasuredAt: day.Add(24 * time.Hour), Run: ProviderEgressRunTally{Place: second, Healthy: true}, Loads: nil},
		}
		type projection struct {
			places []ProviderEgressPlaceTally
			sites  []ProviderEgressSiteTally
			days   []ProviderEgressSiteDayTotal
		}
		read := func(start time.Time) projection {
			return projection{
				places: GetProviderEgressPlaceTallies(ctx, start),
				sites:  GetProviderEgressSiteTallies(ctx, start),
				days:   GetProviderEgressSiteDayTotals(ctx, start, []string{"alpha.example", "beta.example"}),
			}
		}
		for _, record := range records {
			AddProviderEgressRunTally(ctx, record.MeasuredAt, record.Run, record.Loads)
		}
		starts := []time.Time{day, day.Add(24 * time.Hour), day.Add(48 * time.Hour)}
		want := make([]projection, len(starts))
		for i, start := range starts {
			want[i] = read(start)
		}
		if len(want[0].places) != 2 || len(want[0].sites) != 4 || len(want[0].days) != 4 || len(want[1].sites) != 2 || len(want[2].places) != 0 {
			t.Fatal("direct-writer oracle did not cover mixed places, sites and UTC days")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_site_tally; DELETE FROM provider_egress_place_tally`))
		})
		for _, page := range [][]ProviderEgressTallyRecord{records[:3], records[3:]} {
			batch, err := PrepareProviderEgressTallyBatch(page)
			if err != nil {
				t.Fatal(err)
			}
			server.Tx(ctx, func(tx server.PgTx) { AddProviderEgressTallyBatchInTx(ctx, tx, batch) })
		}
		for i, start := range starts {
			if got := read(start); !reflect.DeepEqual(want[i], got) {
				t.Fatalf("coalesced SQL counters differ from direct writer at %s: want=%+v got=%+v", start.Format(time.DateOnly), want[i], got)
			}
		}
	})
}
