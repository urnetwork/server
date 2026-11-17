// Completed production-generator/API listings reproduce the fixture's finite
// authority exhaustion without relying on overlapping pending enumerations.
package perfvar

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Both calls enter the actual non-fixed production discovery branch. The first
// real named API request confirms authorities0..3; the next hard-excludes them.
// OLD has no legitimate fifth result; NEW must return the independent donor.
// This proves authorized eligibility, not installed SDK-client admission.
func TestSdkBrowsingSerializedDiscoveryHasGenuineFifthQualityAuthority(t *testing.T) {
	if testing.Short() {
		t.Skip("actual public provider/API qualification needs shared PERFVAR services")
	}
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		fixture, err := newSdkBrowsingSetupFixture(t, newNativeBrowsingPressurePlan())
		if err != nil {
			t.Fatal("public provider construction: ", err)
		}
		defer func() {
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
		}()
		ready := fixture.readyProviderIds()
		firstSpecs := []*clientconnect.ProviderSpec{}
		excluded := []clientconnect.MultiHopId{}
		expected := map[clientconnect.Id]bool{}
		for ordinal := range 4 {
			id := ready[ordinal]
			if id == (clientconnect.Id{}) {
				t.Fatal("original quality authority was not actually ready")
			}
			firstSpecs = append(firstSpecs, &clientconnect.ProviderSpec{ClientId: &id})
			excluded = append(excluded, clientconnect.RequireMultiHopId(id))
			expected[id] = true
		}
		var calls atomic.Int32
		discovery := sdkBrowsingSetupDiscoveryFunc(func(ctx context.Context, token string, args *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
			if token != fixture.callerToken || args.RankMode != "quality" || args.ForceMinimum || len(args.Specs) != 1 || !args.Specs[0].BestAvailable {
				t.Error("serialized discovery changed caller authority, ranking or non-fixed request")
			}
			index := calls.Add(1)
			if index == 1 {
				// Restrict only this first named API scope. Do not synthesize
				// results, ignore caller exclusions, or mutate caller arguments.
				query := *args
				query.Specs = firstSpecs
				return fixture.discovery.FindProviders2(ctx, token, &query)
			}
			return fixture.discovery.FindProviders2(ctx, token, args)
		})
		apiUrl := fmt.Sprintf("http://%s:%d", fixture.environment.edgeAddress, fixture.environment.apiPort)
		settings := clientconnect.DefaultApiMultiClientGeneratorSettings()
		settings.ProviderDiscovery = discovery
		generator := clientconnect.NewApiMultiClientGenerator(fixture.setupCtx, []*clientconnect.ProviderSpec{{BestAvailable: true}},
			fixture.deviceStrategy, nil, apiUrl, fixture.callerToken, apiUrl, "serialized fixture authority", "fixture authority", "perfvar", nil,
			clientconnect.DefaultClientSettings, settings)
		defer func() {
			joinCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := generator.CloseAndWait(joinCtx); err != nil {
				t.Error("listing owner join: ", err)
			}
		}()
		first, err := generator.NextDestinationsContext(fixture.setupCtx, 4, nil, "quality")
		if err != nil || len(first) != 4 || calls.Load() != 1 {
			t.Fatalf("actual first quality authority query incomplete: count=%d calls=%d err=%v", len(first), calls.Load(), err)
		}
		for destination := range first {
			if !expected[destination.Tail()] {
				t.Fatal("first API listing substituted an unowned authority")
			}
		}
		fifth, err := generator.NextDestinationsContext(fixture.setupCtx, 1, excluded, "quality")
		if err != nil || calls.Load() != 2 {
			t.Fatalf("serialized exclusion query did not complete exactly: calls=%d err=%v", calls.Load(), err)
		}
		if len(fifth) != 1 {
			t.Fatalf("SDK_SETUP_FIFTH_QUALITY_AUTHORITY_MISSING: actual_candidates=%d want=1 after four completed authorized exclusions", len(fifth))
		}
		for destination := range fifth {
			if expected[destination.Tail()] || destination.Tail() != ready[len(ready)-1] || destination.Tail() == ready[5] {
				t.Fatal("fifth response bypassed exclusions, substituted speed, or lacked independent ready authority")
			}
		}
	})
}
