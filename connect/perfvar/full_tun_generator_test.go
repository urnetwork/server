// Fixed topology discovery must retain the complete path through every entry
// point inherited from the production API generator.
package perfvar

import (
	"context"
	"errors"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
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
		destinations, err := familyGenerator.NextDestinationsWithIpFamily(c.count, c.excludeDestinations, "quality", c.ipFamily)
		if err != nil || len(destinations) != c.wantCount {
			t.Errorf("%s discovery=%v err=%v, want %d paths", c.name, destinations, err, c.wantCount)
		}
		if c.wantCount == 1 {
			if _, ok := destinations[generator.destination]; !ok {
				t.Errorf("%s discovery omitted full path %v", c.name, generator.destination)
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
