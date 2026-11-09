package monitor

// SIGNALS.md §29.3 on fixture Mimir answers: a key held past a minute pages; a clock past 25 ms or unknown
// warns, whichever way its exporter says unknown; a cluster with no MinIO series, no process clock, or no
// Mimir is blind.

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// Every source fresh and good: one MinIO host per cluster, an evs process and a Redis host.
func goodFileClusterSettleSamples() []*sidecarStorageSample {
	return []*sidecarStorageSample{
		sidecarStorageTestSample(fileClusterSettleCheckHeld, 0, "cluster", "meta-1", "host", "store-1"),
		sidecarStorageTestSample(fileClusterSettleCheckMinioKnown, 1, "cluster", "meta-1", "host", "store-1"),
		sidecarStorageTestSample(fileClusterSettleCheckMinioOffset, 0.002, "cluster", "meta-1", "host", "store-1"),
		sidecarStorageTestSample(fileClusterSettleCheckHeld, 4, "cluster", "seg-1", "host", "store-1"),
		sidecarStorageTestSample(fileClusterSettleCheckMinioKnown, 1, "cluster", "seg-1", "host", "store-1"),
		sidecarStorageTestSample(fileClusterSettleCheckMinioOffset, 0.002, "cluster", "seg-1", "host", "store-1"),
		sidecarStorageTestSample(fileClusterSettleCheckOffset, 0.001, "service", "evs", "block", "g1", "host", "edge-1", "instance", "a1"),
		sidecarStorageTestSample(fileClusterSettleCheckOffset, 0.003, "role", "redis", "host", "redis-1"),
	}
}

func evaluateFileClusterSettle(samples []*sidecarStorageSample) []finding {
	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
	return fileClusterSettleProbe{}.evaluate(settings, []string{"meta-1", "seg-1"}, samples)
}

func TestFileClusterSettleGoodSourcesRaiseNothing(t *testing.T) {
	requireSidecarStorageRaised(t, evaluateFileClusterSettle(goodFileClusterSettleSamples()))
}

// A key held past 1 min pages on its cluster, at once; at a minute it does not.
func TestFileClusterSettleKeyHeldPastAMinutePages(t *testing.T) {
	samples := goodFileClusterSettleSamples()
	samples[0].value = 60
	requireSidecarStorageRaised(t, evaluateFileClusterSettle(samples))
	samples[0].value = 61
	findings := evaluateFileClusterSettle(samples)
	requireSidecarStorageRaised(t, findings, "filecluster-key-held meta-1")
	for _, finding := range findings {
		if finding.class == fileClusterSettleClassKeyHeld && !finding.healthy && (finding.tier != tierPage || finding.sustain != 1 || !strings.Contains(finding.observed, "store-1")) {
			t.Errorf("finding %+v", finding)
		}
	}
}

// Past 25 ms, or unknown as each exporter says it (a process's +Inf, a Redis host's -1, a MinIO host's known
// flag at 0 or missing), warns on that clock alone; a process's worst instance counts.
func TestFileClusterSettleClockPastTheLineOrUnknownWarns(t *testing.T) {
	cases := []struct {
		index int
		value float64
		want  string
	}{
		{index: 6, value: 0.026, want: "filecluster-clock-offset evs g1@edge-1"},
		{index: 6, value: math.Inf(1), want: "filecluster-clock-offset evs g1@edge-1"},
		{index: 7, value: -1, want: "filecluster-clock-offset redis@redis-1"},
		{index: 7, value: 0.04, want: "filecluster-clock-offset redis@redis-1"},
		{index: 2, value: 0.03, want: "filecluster-clock-offset meta-1 minio store-1"},
		{index: 1, value: 0, want: "filecluster-clock-offset meta-1 minio store-1"},
	}
	for _, c := range cases {
		samples := goodFileClusterSettleSamples()
		samples[c.index].value = c.value
		requireSidecarStorageRaised(t, evaluateFileClusterSettle(samples), c.want)
	}

	samples := goodFileClusterSettleSamples()
	samples = append(samples, sidecarStorageTestSample(fileClusterSettleCheckOffset, math.Inf(1), "service", "evs", "block", "g1", "host", "edge-1", "instance", "b2"))
	requireSidecarStorageRaised(t, evaluateFileClusterSettle(samples), "filecluster-clock-offset evs g1@edge-1")

	// a MinIO host with no known flag is no bound
	samples = goodFileClusterSettleSamples()
	samples = append(samples[:1], samples[2:]...)
	requireSidecarStorageRaised(t, evaluateFileClusterSettle(samples), "filecluster-clock-offset meta-1 minio store-1")
}

// No MinIO series for a cluster, no process clock, or no Mimir: blind, never healthy.
func TestFileClusterSettleMissingSourcesAreBlind(t *testing.T) {
	samples := goodFileClusterSettleSamples()
	requireSidecarStorageRaised(t, evaluateFileClusterSettle(samples[3:]), "filecluster-settle-unobservable meta-1")
	requireSidecarStorageRaised(t, evaluateFileClusterSettle(append(samples[:6:6], samples[7])), "filecluster-settle-unobservable process-clocks")

	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
	probe := fileClusterSettleProbe{query: fixedSidecarStorageQuery(nil, errors.New("no services host"))}
	findings, err := probe.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings, "filecluster-settle-unobservable mimir")
	if !findings[0].isUnobservable() {
		t.Errorf("finding %+v", findings[0])
	}
}

// The query asks for each partition, fresh, in the environment, and the MinIO series of their scrape's job.
func TestFileClusterSettleQueryNamesItsSeries(t *testing.T) {
	query := fileClusterSettleQuery("synthetic", 3*time.Minute)
	for _, part := range []string{
		`urnetwork_filecluster_held_key_oldest_seconds{env="synthetic",job="file_cluster_minio"}`,
		`urnetwork_filecluster_clock_known{env="synthetic",job="file_cluster_minio"}`,
		`urnetwork_filecluster_clock_offset_seconds{env="synthetic",job="file_cluster_minio"}`,
		`urnetwork_clock_offset_seconds{env="synthetic"}`,
		`>= time() - 180`,
		`"monitor_check","held_key_oldest"`,
	} {
		if !strings.Contains(query, part) {
			t.Errorf("the query lacks %s: %s", part, query)
		}
	}
}
