// Exercises place-aware scoring at the full-batch guard and publication boundary.
// All provider identities, destinations and regions are synthetic; no I/O is run.
package work

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"

	"github.com/urnetwork/server/model"
)

// One due provider, its unmodified measured result, and its expected scored counts.
type testEgressGuardPlaceRow struct {
	due       ingest.DueProvider
	run       *egresshealth.Result
	wantTotal int
	wantOk    int
}

// Three measured runs, each with one scored pass and one incompatible failure.
func testEgressGuardPlaceRows(country string, region string) []testEgressGuardPlaceRow {
	rows := []testEgressGuardPlaceRow{}
	for i := range 3 {
		rows = append(rows, testEgressGuardPlaceRow{
			due: ingest.DueProvider{
				ClientId:    fmt.Sprintf("synthetic-provider-%d", i),
				CountryCode: country,
				Region:      region,
			},
			run: &egresshealth.Result{
				ExitIp: "192.0.2.1",
				Checks: []egresshealth.CheckResult{
					{Name: "synthetic-scored-site", Class: "site", Ok: true},
					{Name: "synthetic-blocked-site", Class: "site"},
				},
				Total:   2,
				OkCount: 1,
				ByClass: map[egresshealth.Class]egresshealth.ClassSummary{
					"site": {Total: 2, Ok: 1},
				},
			},
			wantTotal: 1,
			wantOk:    1,
		})
	}
	return rows
}

// Scores the synthetic failed site only outside the supplied incompatible place.
func testEgressGuardPlaceScoring(country string, region string) *model.ProviderEgressHealthScoring {
	return model.NewProviderEgressHealthScoring([]*model.ProviderEgressDestination{
		{Name: "synthetic-scored-site", Class: "site", Active: true},
		{
			Name:         "synthetic-blocked-site",
			Class:        "site",
			Active:       true,
			Incompatible: []model.ProviderEgressDestinationPlace{{Country: country, Region: region}},
		},
	})
}

// Runs the real owner with synthetic reporters. Reverse result arrival makes
// provider identity, not due-list position, determine the scored place.
func testEgressGuardPlaceBatch(t *testing.T, rows []testEgressGuardPlaceRow, scoring *model.ProviderEgressHealthScoring, wantTripped bool) {
	t.Helper()
	rawRuns := []*egresshealth.Result{}
	for _, row := range rows {
		rawRuns = append(rawRuns, row.run)
	}
	rawBefore, err := json.Marshal(rawRuns)
	if err != nil {
		t.Fatal(err)
	}
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	due := []ingest.DueProvider{}
	providerRows := map[string]testEgressGuardPlaceRow{}
	for _, row := range rows {
		due = append(due, row.due)
		providerRows[row.due.ClientId] = row
	}
	inner := newRecordingEgressProbeIngest()
	tallies := []model.ProviderEgressRunTally{}
	pass := &providerEgressProbePass{
		fullSink: testFullBatchSink(inner),
		loadScoring: func(context.Context) (*model.ProviderEgressHealthScoring, *model.ProviderEgressSiteSettings) {
			return scoring, model.DefaultProviderEgressSiteSettings()
		},
		recordTally: func(_ context.Context, _ time.Time, tally model.ProviderEgressRunTally, _ []model.ProviderEgressSiteLoad) {
			tallies = append(tallies, tally)
		},
		runFull: func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			for i := len(providers) - 1; 0 <= i; i-- {
				provider := providers[i]
				row, ok := providerRows[provider.ClientId]
				if !ok {
					return prober.Summary{}, fmt.Errorf("unexpected synthetic provider")
				}
				if err := options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, row.run); err != nil {
					return prober.Summary{}, err
				}
				if err := options.Submit.Submit(ctx, provider.ClientId, row.run.ExitIp, time.Unix(0, 0).UTC()); err != nil {
					return prober.Summary{}, err
				}
				if err := options.Attempts.ReportAttempt(ctx, provider.ClientId, ""); err != nil {
					return prober.Summary{}, err
				}
			}
			return prober.Summary{Attempted: len(providers), Submitted: len(providers)}, nil
		},
	}
	outcome := pass.runFullBatch(context.Background(), args, nil, nil, due)
	if outcome.err != nil {
		t.Fatalf("synthetic batch failed: %v", outcome.err)
	}
	if outcome.guardTripped != wantTripped {
		t.Fatalf("guard tripped = %t, want %t using each provider's scored place", outcome.guardTripped, wantTripped)
	}
	wantCalls := []string{}
	for i := len(rows) - 1; 0 <= i; i-- {
		row := rows[i]
		clientId := row.due.ClientId
		if wantTripped {
			wantCalls = append(wantCalls, "attempt "+clientId+" "+model.ProbeRunBatchGuardClass)
			continue
		}
		scored := inner.health[clientId]
		if row.wantTotal == 0 {
			if scored != nil {
				t.Fatalf("an unscored run replaced a provider's real health: %s", clientId)
			}
		} else {
			if scored == nil || scored.Total != row.wantTotal || scored.OkCount != row.wantOk {
				t.Fatalf("%s published scored result = %+v, want total %d ok %d", clientId, scored, row.wantTotal, row.wantOk)
			}
			wantCalls = append(wantCalls, "health "+clientId)
		}
		wantCalls = append(wantCalls, "submit "+clientId+" "+row.run.ExitIp, "attempt "+clientId+" ")
	}
	if !slices.Equal(inner.calls, wantCalls) {
		t.Fatalf("publication order = %q, want %q", inner.calls, wantCalls)
	}
	if wantTripped {
		if outcome.summary.Submitted != 0 || outcome.summary.Failed != len(rows) || len(tallies) != 0 || len(inner.health) != 0 {
			t.Fatalf("a tripped guard leaked publication: summary %+v tallies %d health %d", outcome.summary, len(tallies), len(inner.health))
		}
	} else if outcome.summary.Submitted != len(rows) || outcome.summary.Failed != 0 || len(tallies) != len(rows) {
		t.Fatalf("accepted batch summary %+v tallies %d, want %d published providers", outcome.summary, len(tallies), len(rows))
	}
	rawAfter, err := json.Marshal(rawRuns)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rawBefore, rawAfter) {
		t.Fatal("guard or publication changed the raw measured results")
	}
}

// A known country exclusion cannot falsely hold back an otherwise passing batch.
func TestProviderEgressGuardPlaceExcludesCountryFailures(t *testing.T) {
	testEgressGuardPlaceBatch(t, testEgressGuardPlaceRows(" ZZ ", ""), testEgressGuardPlaceScoring("zz", ""), false)
}

// Region exclusions use the same normalized place as publication.
func TestProviderEgressGuardPlaceExcludesRegionFailures(t *testing.T) {
	testEgressGuardPlaceBatch(t, testEgressGuardPlaceRows("zz", " Synthetic Region "), testEgressGuardPlaceScoring("zz", "synthetic region"), false)
}

// Distinct places stay associated with their providers despite reversed arrival.
func TestProviderEgressGuardPlaceRetainsProviderIdentity(t *testing.T) {
	rows := testEgressGuardPlaceRows("zz", "")
	destinations := []*model.ProviderEgressDestination{}
	for i := range rows {
		region := fmt.Sprintf("synthetic-region-%d", i)
		name := fmt.Sprintf("synthetic-blocked-site-%d", i)
		rows[i].due.Region = region
		rows[i].run.Checks[1].Name = name
		destinations = append(destinations, &model.ProviderEgressDestination{
			Name: name, Class: "site", Active: true,
			Incompatible: []model.ProviderEgressDestinationPlace{{Country: "zz", Region: region}},
		})
	}
	testEgressGuardPlaceBatch(t, rows, model.NewProviderEgressHealthScoring(destinations), false)
}

// Excluded passes must not dilute real failures below the unchanged guard line.
func TestProviderEgressGuardPlaceExcludedPassesCannotHideFailures(t *testing.T) {
	rows := testEgressGuardPlaceRows("zz", "")
	destinations := []*model.ProviderEgressDestination{}
	for i := range 3 {
		destinations = append(destinations, &model.ProviderEgressDestination{
			Name: fmt.Sprintf("synthetic-blocked-site-%d", i), Class: "site", Active: true,
			Incompatible: []model.ProviderEgressDestinationPlace{{Country: "zz"}},
		})
	}
	for i := range rows {
		rows[i].run.Checks = []egresshealth.CheckResult{
			{Name: "synthetic-scored-site", Class: "site"},
			{Name: "synthetic-blocked-site-0", Class: "site", Ok: true},
			{Name: "synthetic-blocked-site-1", Class: "site", Ok: true},
			{Name: "synthetic-blocked-site-2", Class: "site", Ok: true},
		}
		rows[i].run.Total, rows[i].run.OkCount = 4, 3
		rows[i].run.ByClass["site"] = egresshealth.ClassSummary{Total: 4, Ok: 3}
	}
	testEgressGuardPlaceBatch(t, rows, model.NewProviderEgressHealthScoring(destinations), true)
}

// A wholly excluded run cannot satisfy the minimum measured-run requirement.
func TestProviderEgressGuardPlaceExcludedRunDoesNotMeetMinimum(t *testing.T) {
	rows := testEgressGuardPlaceRows("zz", "")
	for i := range rows {
		rows[i].run.Checks = []egresshealth.CheckResult{{Name: "synthetic-scored-site", Class: "site"}}
		rows[i].run.Total, rows[i].run.OkCount = 1, 0
		rows[i].run.ByClass["site"] = egresshealth.ClassSummary{Total: 1}
		rows[i].wantTotal, rows[i].wantOk = 1, 0
	}
	rows[2].run.Checks[0].Name = "synthetic-blocked-site"
	rows[2].wantTotal = 0
	testEgressGuardPlaceBatch(t, rows, testEgressGuardPlaceScoring("zz", ""), false)
}

// Valid failures in another country still trip and suppress negative publication.
func TestProviderEgressGuardPlaceValidFailuresStillTrip(t *testing.T) {
	testEgressGuardPlaceBatch(t, testEgressGuardPlaceRows("xy", ""), testEgressGuardPlaceScoring("zz", ""), true)
}

// Missing country evidence must never be guessed to match an excluded country.
func TestProviderEgressGuardPlaceUnknownCountryStillCounts(t *testing.T) {
	testEgressGuardPlaceBatch(t, testEgressGuardPlaceRows("", ""), testEgressGuardPlaceScoring("zz", ""), true)
}

// A country alone cannot establish a region-specific exclusion.
func TestProviderEgressGuardPlaceUnknownRegionStillCounts(t *testing.T) {
	testEgressGuardPlaceBatch(t, testEgressGuardPlaceRows("zz", ""), testEgressGuardPlaceScoring("zz", "synthetic region"), true)
}

// No scoring inventory retains the existing counted-load compatibility behavior.
func TestProviderEgressGuardPlaceMissingScoringStillCounts(t *testing.T) {
	testEgressGuardPlaceBatch(t, testEgressGuardPlaceRows("zz", ""), nil, true)
}
