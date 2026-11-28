package monitor

import (
	"strings"
	"testing"
)

func TestCanonicalSiteKeyCollapsesWebsiteVariants(t *testing.T) {
	first, err := CanonicalSiteKey("https://www.portal.example.co.uk/robots.txt")
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalSiteKey("https://shop.portal.example.co.uk/news")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first != "example.co.uk" {
		t.Fatalf("canonical keys = %q, %q; want example.co.uk", first, second)
	}
}

func TestFilterCountrySiteCandidatesRemovesGlobalOverlapAndDuplicates(t *testing.T) {
	global := []string{"https://www.shared.example.com/robots.txt"}
	candidates := []CountrySiteCandidate{
		{URL: "https://shop.shared.example.com/", Source: "radar", Rank: 1},
		{URL: "https://local.example.net/", Source: "radar", Rank: 2},
		{URL: "https://www.local.example.net/news", Source: "crux"},
		{URL: "https://second.example.org/", Source: "crux"},
	}
	accepted, rejected, err := FilterCountrySiteCandidates(global, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if rejected != 2 || len(accepted) != 2 || accepted[0].URL != candidates[1].URL || accepted[1].URL != candidates[3].URL {
		t.Fatalf("accepted=%v rejected=%d", accepted, rejected)
	}
}

func TestFilterCountrySiteCandidatesRejectsUnsafeInputsWithoutPadding(t *testing.T) {
	candidates := []CountrySiteCandidate{
		{URL: "http://plain.example.com/"},
		{URL: "https://user:pass@private.example.com/"},
		{URL: "https://bad.example.com:8443/"},
		{URL: "https://valid.example.com/"},
	}
	accepted, rejected, err := FilterCountrySiteCandidates(nil, candidates)
	if err != nil || rejected != 3 || len(accepted) != 1 || accepted[0].URL != candidates[3].URL {
		t.Fatalf("accepted=%v rejected=%d err=%v", accepted, rejected, err)
	}
	_, _, err = FilterCountrySiteCandidates([]string{"not-a-url"}, candidates)
	if err == nil || !strings.Contains(err.Error(), "global site") {
		t.Fatalf("malformed global URL was not a configuration error: %v", err)
	}
}

func TestMergeCountrySiteCandidatesPrioritizesRadarWithoutGlobalOverlap(t *testing.T) {
	global := []string{"https://www.shared.example.com/robots.txt"}
	radar := []CountrySiteCandidate{
		{URL: "https://shared.example.com/", Source: "radar", Rank: 1},
		{URL: "https://first.example.net/", Source: "radar", Rank: 2},
	}
	crux := []CountrySiteCandidate{
		{URL: "https://www.first.example.net/news", Source: "crux"},
		{URL: "https://second.example.org/", Source: "crux"},
		{URL: "https://third.example.edu/", Source: "crux"},
	}
	accepted, rejected, err := MergeCountrySiteCandidates(global, radar, crux, 2)
	if err != nil {
		t.Fatal(err)
	}
	if rejected != 2 || len(accepted) != 2 || accepted[0].Source != "radar" || accepted[0].Rank != 2 || accepted[1].URL != crux[1].URL {
		t.Fatalf("accepted=%v rejected=%d", accepted, rejected)
	}
	accepted, rejected, err = MergeCountrySiteCandidates(global, radar, crux[:2], 3)
	if err != nil || rejected != 2 || len(accepted) != 2 || accepted[0].URL != radar[1].URL || accepted[1].URL != crux[1].URL {
		t.Fatalf("short list must remain short: accepted=%v rejected=%d err=%v", accepted, rejected, err)
	}
}
