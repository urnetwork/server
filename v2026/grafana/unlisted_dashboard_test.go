// A dashboard tagged both public and unlisted is published like any public
// dashboard, and LoadDefaults reports it as unlisted, so that the operator
// gets its link from load-defaults: the public directory leaves it out.
package grafana

import (
	"maps"
	"slices"
	"testing"
)

// Runs the real loader against the synthetic Grafana of
// private_publication_order_test.go.
func TestLoadDefaultsMarksUnlistedPublicDashboards(t *testing.T) {
	wantUnlisted := map[string]bool{}
	for _, name := range mustReadDashboardDir(t) {
		dashboard := readTestDashboard(t, name)
		if slices.Contains(dashboard.Tags, PublicTag) {
			wantUnlisted[dashboard.Uid] = slices.Contains(dashboard.Tags, UnlistedTag)
		}
	}
	// the source has both kinds, so both are checked
	if !slices.Contains(slices.Collect(maps.Values(wantUnlisted)), true) || !slices.Contains(slices.Collect(maps.Values(wantUnlisted)), false) {
		t.Fatalf("the dashboards do not have both a listed and an unlisted public dashboard: %v", wantUnlisted)
	}

	fixture := newPrivatePublicationFixture(t)
	_, public, err := fixture.load(t)
	if err != nil {
		t.Fatal(err)
	}
	gotUnlisted := map[string]bool{}
	for _, dashboard := range public {
		if dashboard.AccessToken == "" {
			t.Errorf("%s was published without an access token", dashboard.Uid)
		}
		gotUnlisted[dashboard.Uid] = dashboard.Unlisted
	}
	if !maps.Equal(gotUnlisted, wantUnlisted) {
		t.Fatalf("published dashboards and their unlisted marks = %v, want %v", gotUnlisted, wantUnlisted)
	}
}
