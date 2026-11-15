// Fixed topology discovery must retain the complete path through every entry
// point inherited from the production API generator.
package perfvar

import (
	"context"
	"errors"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
)

// A real embedded API generator has the same direct provider spec as full-TUN
// construction. Its fixed spec resolves locally, without network or DB work.
func newFixedMultiHopGeneratorForTest(t *testing.T, hopCount int) *fixedMultiHopApiGenerator {
	t.Helper()
	pathIds := make([]clientconnect.Id, hopCount)
	for hopIndex := range pathIds {
		pathIds[hopIndex] = clientconnect.NewId()
	}
	destination := clientconnect.RequireMultiHopId(pathIds...)
	providerId := pathIds[len(pathIds)-1]
	apiGenerator := clientconnect.NewApiMultiClientGeneratorWithDefaults(
		t.Context(),
		[]*clientconnect.ProviderSpec{{ClientId: &providerId}},
		nil,
		nil,
		"https://api.example",
		"",
		"wss://platform.example",
		"test device",
		"test device",
		"test",
		nil,
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := apiGenerator.CloseAndWait(ctx); err != nil {
			t.Errorf("close fixed topology API generator: %v", err)
		}
	})
	return &fixedMultiHopApiGenerator{
		ApiMultiClientGenerator: apiGenerator,
		destination:             destination,
	}
}

// Interface dispatch follows the same family-aware method selected by the
// production window; a promoted base method would return just the provider.
func TestFixedMultiHopApiGeneratorPreservesDiscoveryPath(t *testing.T) {
	for _, hopCount := range []int{2, 3, 5, 9} {
		generator := newFixedMultiHopGeneratorForTest(t, hopCount)
		var baseGenerator clientconnect.MultiClientGenerator = generator
		familyGenerator := baseGenerator.(clientconnect.MultiClientGeneratorWithIpFamily)
		calls := []struct {
			name string
			call func() (map[clientconnect.MultiHopId]clientconnect.DestinationStats, error)
		}{
			{
				name: "plain",
				call: func() (map[clientconnect.MultiHopId]clientconnect.DestinationStats, error) {
					return baseGenerator.NextDestinations(4, nil, "quality")
				},
			},
			{
				name: "context",
				call: func() (map[clientconnect.MultiHopId]clientconnect.DestinationStats, error) {
					return generator.NextDestinationsContext(t.Context(), 4, nil, "quality")
				},
			},
			{
				name: "family",
				call: func() (map[clientconnect.MultiHopId]clientconnect.DestinationStats, error) {
					return familyGenerator.NextDestinationsWithIpFamily(
						4, nil, "quality", clientconnect.IpFamilyFilterV4Capable,
					)
				},
			},
			{
				name: "family-context",
				call: func() (map[clientconnect.MultiHopId]clientconnect.DestinationStats, error) {
					return generator.NextDestinationsWithIpFamilyContext(
						t.Context(), 4, nil, "quality", clientconnect.IpFamilyFilterV4Capable,
					)
				},
			},
		}
		for _, c := range calls {
			destinations, err := c.call()
			if err != nil {
				t.Errorf("%d-hop %s discovery: %v", hopCount, c.name, err)
				continue
			}
			stats, ok := destinations[generator.destination]
			if !ok || len(destinations) != 1 {
				t.Errorf("%d-hop %s discovery=%v, want only full path %v", hopCount, c.name, destinations, generator.destination)
			}
			if stats.IpFamily != clientconnect.IpFamilyLegacy {
				t.Errorf("%d-hop %s discovery advertised unproven family %q", hopCount, c.name, stats.IpFamily)
			}
		}
	}
}

// A fixed IPv4 topology cannot be rediscovered after exclusion, exceed a zero
// request, or satisfy a request for unproven IPv6 capability.
func TestFixedMultiHopApiGeneratorFiltersDiscovery(t *testing.T) {
	generator := newFixedMultiHopGeneratorForTest(t, 3)
	var familyGenerator clientconnect.MultiClientGeneratorWithIpFamily = generator
	cases := []struct {
		name                string
		count               int
		excludeDestinations []clientconnect.MultiHopId
		ipFamily            clientconnect.IpFamilyFilter
		wantCount           int
	}{
		{name: "default", count: 1, wantCount: 1},
		{name: "v4 capable", count: 1, ipFamily: clientconnect.IpFamilyFilterV4Capable, wantCount: 1},
		{name: "v4 only", count: 1, ipFamily: clientconnect.IpFamilyFilterV4Only, wantCount: 1},
		{name: "v6 capable", count: 1, ipFamily: clientconnect.IpFamilyFilterV6Capable},
		{name: "v6 only", count: 1, ipFamily: clientconnect.IpFamilyFilterV6Only},
		{name: "dualstack", count: 1, ipFamily: clientconnect.IpFamilyFilterDualstack},
		{name: "unknown family", count: 1, ipFamily: clientconnect.IpFamilyFilter("unknown-test-family")},
		{name: "zero count", count: 0},
		{name: "negative count", count: -1},
		{name: "excluded", count: 1, excludeDestinations: []clientconnect.MultiHopId{generator.destination}},
		{
			name:                "unrelated exclusion",
			count:               1,
			excludeDestinations: []clientconnect.MultiHopId{clientconnect.RequireMultiHopId(clientconnect.NewId())},
			wantCount:           1,
		},
	}
	for _, c := range cases {
		for _, contextAware := range []bool{false, true} {
			var destinations map[clientconnect.MultiHopId]clientconnect.DestinationStats
			var err error
			if contextAware {
				destinations, err = generator.NextDestinationsWithIpFamilyContext(t.Context(), c.count, c.excludeDestinations, "quality", c.ipFamily)
			} else {
				destinations, err = familyGenerator.NextDestinationsWithIpFamily(c.count, c.excludeDestinations, "quality", c.ipFamily)
			}
			if err != nil || len(destinations) != c.wantCount {
				t.Errorf("%s context=%t discovery=%v err=%v, want %d paths", c.name, contextAware, destinations, err, c.wantCount)
			}
			if c.wantCount == 1 {
				if _, ok := destinations[generator.destination]; !ok {
					t.Errorf("%s context=%t discovery omitted full path %v", c.name, contextAware, generator.destination)
				}
			}
		}
	}
}

// The adjacent context-aware discovery method shares the fixed-path policy
// and refuses work when its caller has already canceled.
func TestFixedMultiHopApiGeneratorContextDiscovery(t *testing.T) {
	generator := newFixedMultiHopGeneratorForTest(t, 3)
	destinations, err := generator.NextDestinationsContext(
		t.Context(), 1, []clientconnect.MultiHopId{generator.destination}, "quality",
	)
	if err != nil || len(destinations) != 0 {
		t.Errorf("excluded context discovery=%v err=%v, want no paths", destinations, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	destinations, err = generator.NextDestinationsContext(ctx, 1, nil, "quality")
	if !errors.Is(err, context.Canceled) || len(destinations) != 0 {
		t.Errorf("canceled context discovery=%v err=%v, want no paths and context cancellation", destinations, err)
	}
}

// The embedded API has a direct-provider spec. Interface dispatch must still
// return this wrapper's full topology; this is the isolated pre-fix control.
func TestFixedMultiHopApiGeneratorFamilyContextKeepsFullPath(t *testing.T) {
	generator := newFixedMultiHopGeneratorForTest(t, 3)
	var familyContext interface {
		NextDestinationsWithIpFamilyContext(context.Context, int, []clientconnect.MultiHopId, string, clientconnect.IpFamilyFilter) (map[clientconnect.MultiHopId]clientconnect.DestinationStats, error)
	} = generator
	destinations, err := familyContext.NextDestinationsWithIpFamilyContext(t.Context(), 1, nil, "quality", clientconnect.IpFamilyFilterV4Capable)
	if _, fullPath := destinations[generator.destination]; err != nil || len(destinations) != 1 || !fullPath {
		t.Fatalf("family-context discovery=%v err=%v, want only full path %v", destinations, err, generator.destination)
	}
}

// Cancellation precedes discovery even when a fixed spec could answer locally.
func TestFixedMultiHopApiGeneratorFamilyContextCancellation(t *testing.T) {
	generator := newFixedMultiHopGeneratorForTest(t, 3)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	destinations, err := generator.NextDestinationsWithIpFamilyContext(ctx, 1, nil, "quality", clientconnect.IpFamilyFilterV4Capable)
	if !errors.Is(err, context.Canceled) || len(destinations) != 0 {
		t.Fatalf("canceled family-context discovery=%v err=%v, want no paths and caller cancellation", destinations, err)
	}
}

// The production window chooses capability discovery before family discovery.
// Its real embedded fixed-provider API must not replace the configured stream path.
func TestFixedMultiHopApiGeneratorCapabilitiesKeepFullPath(t *testing.T) {
	for _, hopCount := range []int{2, 3, 5, 9} {
		generator := newFixedMultiHopGeneratorForTest(t, hopCount)
		var base clientconnect.MultiClientGenerator = generator
		capabilities, ok := base.(clientconnect.MultiClientGeneratorWithCapabilities)
		if !ok {
			t.Fatal("fixed topology generator lost production capability interface dispatch")
		}
		destinations, selectorMode, err := capabilities.NextDestinationsWithCapabilities(
			4, nil, "quality", clientconnect.IpFamilyFilterV4Capable, clientconnect.FindProvidersSelect{},
		)
		if err != nil || selectorMode {
			t.Fatalf("%d-hop fixed capability discovery: selector=%t error=%v", hopCount, selectorMode, err)
		}
		if _, fullPath := destinations[generator.destination]; len(destinations) != 1 || !fullPath {
			t.Fatalf("capability discovery bypassed explicit %d-hop path: destination_count=%d full_path=%t", hopCount, len(destinations), fullPath)
		}
	}
}

// Context-aware capability discovery follows the same actual production interface.
func TestFixedMultiHopApiGeneratorCapabilityContextKeepsFullPath(t *testing.T) {
	for _, hopCount := range []int{2, 3, 5, 9} {
		generator := newFixedMultiHopGeneratorForTest(t, hopCount)
		var capabilities clientconnect.MultiClientGeneratorWithCapabilities = generator
		destinations, selectorMode, err := capabilities.NextDestinationsWithCapabilitiesContext(
			t.Context(), 4, nil, "quality", clientconnect.IpFamilyFilterV4Capable, clientconnect.FindProvidersSelect{},
		)
		stats, fullPath := destinations[generator.destination]
		if err != nil || selectorMode || len(destinations) != 1 || !fullPath {
			t.Fatalf("%d-hop capability context discovery omitted its explicit path: count=%d full=%t selector=%t err=%v", hopCount, len(destinations), fullPath, selectorMode, err)
		}
		if stats.IpFamily != clientconnect.IpFamilyLegacy || stats.Capabilities != (clientconnect.ProviderCapabilities{}) {
			t.Fatal("fixed capability discovery advertised unproved IP or protocol labels")
		}
		if selectorMode, known := capabilities.SelectorMode(); selectorMode || known {
			t.Fatal("fixed topology advertised a platform selector-mode answer it never requested")
		}
	}
}

// Count, whole-path exclusion and real family evidence are identical on both capability forms.
func TestFixedMultiHopApiGeneratorCapabilityFilters(t *testing.T) {
	generator := newFixedMultiHopGeneratorForTest(t, 3)
	var capabilities clientconnect.MultiClientGeneratorWithCapabilities = generator
	cases := []struct {
		name     string
		count    int
		excluded []clientconnect.MultiHopId
		family   clientconnect.IpFamilyFilter
		want     int
	}{
		{name: "v4", count: 4, family: clientconnect.IpFamilyFilterV4Capable, want: 1},
		{name: "v6", count: 1, family: clientconnect.IpFamilyFilterV6Capable},
		{name: "dualstack", count: 1, family: clientconnect.IpFamilyFilterDualstack},
		{name: "unknown", count: 1, family: clientconnect.IpFamilyFilter("unknown-test-family")},
		{name: "zero", count: 0},
		{name: "negative", count: -1},
		{name: "excluded", count: 1, excluded: []clientconnect.MultiHopId{generator.destination}},
		{name: "unrelated", count: 1, excluded: []clientconnect.MultiHopId{clientconnect.RequireMultiHopId(clientconnect.NewId())}, want: 1},
	}
	for _, c := range cases {
		for _, cooperative := range []bool{false, true} {
			var destinations map[clientconnect.MultiHopId]clientconnect.DestinationStats
			var selectorMode bool
			var err error
			if cooperative {
				destinations, selectorMode, err = capabilities.NextDestinationsWithCapabilitiesContext(t.Context(), c.count, c.excluded, "quality", c.family, clientconnect.FindProvidersSelect{})
			} else {
				destinations, selectorMode, err = capabilities.NextDestinationsWithCapabilities(c.count, c.excluded, "quality", c.family, clientconnect.FindProvidersSelect{})
			}
			if err != nil || selectorMode || len(destinations) != c.want {
				t.Fatalf("%s cooperative=%t count=%d selector=%t err=%v, want=%d", c.name, cooperative, len(destinations), selectorMode, err, c.want)
			}
			if c.want != 0 {
				if _, fullPath := destinations[generator.destination]; !fullPath {
					t.Fatal("filtered capability discovery returned a direct provider instead of its path")
				}
			}
		}
	}
}

// A canceled discovery request must not fall through to the embedded API generator.
func TestFixedMultiHopApiGeneratorCapabilityContextCancellation(t *testing.T) {
	generator := newFixedMultiHopGeneratorForTest(t, 3)
	var capabilities clientconnect.MultiClientGeneratorWithCapabilities = generator
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	destinations, selectorMode, err := capabilities.NextDestinationsWithCapabilitiesContext(
		ctx, 1, nil, "quality", clientconnect.IpFamilyFilterV4Capable, clientconnect.FindProvidersSelect{},
	)
	if !errors.Is(err, context.Canceled) || selectorMode || len(destinations) != 0 {
		t.Fatal("canceled capability discovery returned a destination or lost caller cancellation")
	}
}

// Sticky redials use the same full path through old and capability-aware interfaces.
func TestFixedMultiHopApiGeneratorNamedProviderKeepsFullPath(t *testing.T) {
	for _, hopCount := range []int{2, 3, 5, 9} {
		generator := newFixedMultiHopGeneratorForTest(t, hopCount)
		_, providerId := generator.destination.SplitTail()
		var named clientconnect.MultiClientGeneratorWithClientId = generator
		var capabilities clientconnect.MultiClientGeneratorWithCapabilities = generator
		for _, capable := range []bool{false, true} {
			for _, excluded := range []bool{false, true} {
				exclusions := []clientconnect.MultiHopId{}
				if excluded {
					exclusions = append(exclusions, generator.destination)
				}
				var destinations map[clientconnect.MultiHopId]clientconnect.DestinationStats
				var err error
				if capable {
					var selectorMode bool
					destinations, selectorMode, err = capabilities.NextDestinationsForClientIdWithCapabilities(providerId, exclusions, "quality", clientconnect.FindProvidersSelect{})
					if selectorMode {
						t.Fatal("fixed named provider advertised a platform selector answer")
					}
				} else {
					destinations, err = named.NextDestinationsForClientId(providerId, exclusions, "quality")
				}
				if err != nil {
					t.Fatal("fixed named-provider discovery tried the embedded API", err)
				}
				if excluded {
					if len(destinations) != 0 {
						t.Fatal("excluded full path was rediscovered by provider id")
					}
				} else if _, fullPath := destinations[generator.destination]; len(destinations) != 1 || !fullPath {
					t.Fatal("named provider bypassed its configured full path")
				}
			}
		}
		foreignId := clientconnect.NewId()
		oldDestinations, oldErr := named.NextDestinationsForClientId(foreignId, nil, "quality")
		newDestinations, _, newErr := capabilities.NextDestinationsForClientIdWithCapabilities(foreignId, nil, "quality", clientconnect.FindProvidersSelect{})
		if oldErr != nil || newErr != nil || len(oldDestinations) != 0 || len(newDestinations) != 0 {
			t.Fatal("fixed named discovery admitted another provider or called the API")
		}
	}
}
