// Due admission metadata survives the boundary into a provider-pinned URL turn.
package fleetprobe

import (
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

func TestUrlProbeProviderCarriesRollingAndSecurityAdmission(t *testing.T) {
	due := ingest.DueProvider{ClientId: "synthetic-provider", CountryCode: " ZZ ", Region: " Synthetic ",
		CycleStartedAt: time.Unix(1_800_000_000, 0).UTC(), OutcomeCount: 21, RunsNeeded: 0,
		SecurityDestinations: []egresshealth.Destination{{Name: "retired-synthetic", Class: egresshealth.ClassSite,
			Url: "https://synthetic-security.example/original", Expect: egresshealth.ExpectBody}}}
	providers := ProvidersFromDue([]ingest.DueProvider{due})
	if len(providers) != 1 {
		t.Fatalf("missing URL admission: %+v", providers)
	}
	provider := providers[0]
	if provider.ClientId != due.ClientId || provider.Place.Country != "zz" || provider.Place.Region != "Synthetic" ||
		!provider.CycleStartedAt.Equal(due.CycleStartedAt) || provider.OutcomeCount != due.OutcomeCount || provider.RunsNeeded != 0 ||
		!reflect.DeepEqual(provider.SecurityDestinations, due.SecurityDestinations) {
		t.Fatalf("URL admission metadata changed at fleet boundary: %+v", provider)
	}
}
