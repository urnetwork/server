package monitor

// SIGNALS.md §30.2 on fixture Mimir answers: past 1% of a service's requests answered 5xx warns once the
// window holds 100 requests; a service with no current series, or whose failures are not published as
// http requests, reads unobservable.

import (
	"errors"
	"slices"
	"testing"
)

// A service's partitions: requests over the window, its 5xx, and a current series.
func sidecarServiceFailureTestSamples(service string, requests float64, failures float64) []*sidecarStorageSample {
	return []*sidecarStorageSample{
		sidecarStorageTestSample(service+"/"+sidecarServiceFailurePartRequests, requests),
		sidecarStorageTestSample(service+"/"+sidecarServiceFailurePartFailures, failures),
		sidecarStorageTestSample(service+"/"+sidecarServiceFailurePartFresh, 2),
	}
}

// The probe's findings on samples, as "class target healthy|raised".
func sidecarServiceFailureTestStates(t *testing.T, settings *SidecarSloSettings, samples []*sidecarStorageSample, err error) []string {
	t.Helper()
	findings, checkErr := sidecarServiceFailuresProbe{query: fixedSidecarStorageQuery(samples, err)}.check(t.Context(), sloBurnTestEnv(settings))
	if checkErr != nil {
		t.Fatal(checkErr)
	}
	states := []string{}
	for _, finding := range findings {
		state := "raised"
		if finding.healthy {
			state = "healthy"
		}
		states = append(states, finding.class+" "+finding.target+" "+state)
	}
	slices.Sort(states)
	return states
}

// Past 1% of 100 or more requests warns; at 1%, or under 100 requests, nothing does.
func TestSidecarServiceFailuresWarnPastOnePercent(t *testing.T) {
	settings := sloBurnTestSettings("evs", "gkm")
	for _, c := range []struct {
		requests float64
		failures float64
		want     string
	}{
		{requests: 1000, failures: 10, want: "healthy"},
		{requests: 1000, failures: 11, want: "raised"},
		// too few to judge
		{requests: 99, failures: 50, want: "healthy"},
		{requests: 100, failures: 2, want: "raised"},
	} {
		samples := append(sidecarServiceFailureTestSamples("evs", c.requests, c.failures), sidecarServiceFailureTestSamples("gkm", 1000, 0)...)
		got := sidecarServiceFailureTestStates(t, settings, samples, nil)
		want := []string{"sidecar-service-failure evs " + c.want, "sidecar-service-failure gkm healthy"}
		if !slices.Equal(got, want) {
			t.Errorf("%v of %v: %q, want %q", c.failures, c.requests, got, want)
		}
	}
	// no 5xx series is no 5xx
	samples := sidecarServiceFailureTestSamples("evs", 1000, 0)[0:1]
	samples = append(samples, sidecarStorageTestSample("evs/"+sidecarServiceFailurePartFresh, 1))
	if got := sidecarServiceFailureTestStates(t, sloBurnTestSettings("evs"), samples, nil); !slices.Equal(got, []string{"sidecar-service-failure evs healthy"}) {
		t.Errorf("no 5xx series: %q", got)
	}
}

// A service publishing no current series, one whose failures are not http requests (the push sender's
// wakes), and a blind Mimir each read unobservable.
func TestSidecarServiceFailuresWithoutSeriesAreUnobservable(t *testing.T) {
	samples := sidecarServiceFailureTestSamples("evs", 1000, 0)[0:2]
	if got := sidecarServiceFailureTestStates(t, sloBurnTestSettings("evs", "wake"), samples, nil); !slices.Equal(got, []string{
		"sidecar-service-failure-unobservable evs raised",
		"sidecar-service-failure-unobservable wake raised",
	}) {
		t.Errorf("%q", got)
	}
	if got := sidecarServiceFailureTestStates(t, sloBurnTestSettings("evs"), nil, errors.New("mimir down")); !slices.Equal(got, []string{"sidecar-service-failure-unobservable evs raised"}) {
		t.Errorf("mimir down: %q", got)
	}
	findings, err := sidecarServiceFailuresProbe{}.check(t.Context(), sloBurnTestEnv(nil))
	if err != nil || len(findings) != 1 || !findings[0].configuredOff {
		t.Errorf("no block: %+v, %v", findings, err)
	}
}
