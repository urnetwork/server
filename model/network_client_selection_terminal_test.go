// Exercises the public discovery paths against target pages with no drawable member.
package model

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestFindProviders2SelectionZeroWithoutDrawableCandidate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newOfflineTestFixture(ctx, t, "Terminal")
		// Every currently live candidate is explicitly excluded. The remaining
		// Quality native was published while connected and disconnected before
		// this request. Its live Quality read revokes every draw.
		excluded := append([]server.Id{fixture.onlineOnly}, fixture.natives...)
		excluded = append(excluded, fixture.speedOnly...)
		protocol := ProviderCapabilityProtocolAny
		for _, selection := range []*FindProviders2Select{nil, {Protocol: &protocol}} {
			args := &FindProviders2Args{Specs: egressTestLocationSpec(fixture.city), ExcludeClientIds: excluded, Select: selection}
			labels := map[string]string{"request_class": "default_minimum", "ip_family": "any", "rank_mode": "quality", "outcome": "zero", "reason": "no_drawable_candidates"}
			before := selectionMetricSum(t, "urnetwork_findproviders2_selection_outcomes_total", labels)
			clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(server.NewId(), server.NewId(), "selection-terminal-test", false, false))
			result, err := FindProviders2(args, clientSession)
			if err != nil || len(result.Providers) != 0 {
				t.Fatalf("selector enabled=%t: expected a completed empty response, err=%v", selection != nil, err)
			}
			if selectionMetricSum(t, "urnetwork_findproviders2_selection_outcomes_total", labels) != before+1 {
				t.Errorf("selector enabled=%t: live revocation did not produce the no-drawable reason", selection != nil)
			}
		}
	})
}

// Draw membership can outlive a positive draw and can be revoked by a later
// native page. A zero result is an invariant only if a drawable member survives.
func TestFindProviders2SelectionTerminalRevocations(t *testing.T) {
	id := server.NewId()
	scores := map[server.Id]*ClientScore{id: {ClientId: id}}
	revoked := map[server.Id]bool{id: true}
	for _, tc := range []struct {
		name                              string
		n                                 int
		hard, offline, incapable, quality map[server.Id]bool
		onlineFallback                    bool
		want                              string
	}{
		{name: "unselected", n: 1, want: "drawable_not_selected"},
		{name: "zero quota", want: "no_drawable_candidates"},
		{name: "later risk", n: 1, hard: revoked, want: "no_drawable_candidates"},
		{name: "later offline", n: 1, offline: revoked, onlineFallback: true, want: "no_drawable_candidates"},
		{name: "later capability", n: 1, incapable: revoked, want: "no_drawable_candidates"},
		{name: "native quality refusal", n: 1, quality: revoked, want: "no_drawable_candidates"},
		{name: "online admission survives native quality refusal", n: 1, quality: revoked, onlineFallback: true, want: "drawable_not_selected"},
	} {
		var pools findProviders2DrawPools
		pools.record(scores, tc.n, RankModeQuality)
		if tc.onlineFallback {
			pools.record(scores, tc.n, RankModeOnline)
		}
		observation := findProviders2SelectionObservation{
			args: &FindProviders2Args{}, targetKind: "country", rankMode: RankModeQuality,
			loaded: 1, discovery: true, passedRequestFilters: true,
			drawable: pools.hasSurvivor(tc.hard, tc.offline, tc.incapable, tc.quality),
		}
		observation.complete(0)
		if observation.reason != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, observation.reason, tc.want)
		}
	}
}
