package monitor

// SIGNALS.md §29.4 on fixture Mimir answers: the drive-write lines, SMART's spare and media errors, the rating
// projection and the rating-days gauge, the join of a drive to its cluster, and the blind cases.

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// meta-1's drive paths, as its admin info lists them.
func fileClusterWearTestPaths() map[string]string {
	return map[string]string{
		"/srv/file-cluster/meta-1/d1": "meta-1",
		"/srv/file-cluster/meta-1/d2": "meta-1",
	}
}

// meta-1's two drives on store-1, a boot drive no cluster names, and the drive writes, all within every line.
// usedThen is percentage used 30 days ago.
func fileClusterWearTestSamples(used float64, usedThen float64) []*sidecarStorageSample {
	samples := []*sidecarStorageSample{
		sidecarStorageTestSample(fileClusterWearCheckDwpd, 0.40, "cluster", "meta-1", "window", "24h"),
		sidecarStorageTestSample(fileClusterWearCheckDwpd, 0.41, "cluster", "meta-1", "window", "7d"),
		sidecarStorageTestSample(fileClusterWearCheckDwpd, 0.42, "cluster", "meta-1", "window", "30d"),
		sidecarStorageTestSample(fileClusterWearCheckFilesystem, 1.92e12, "host", "store-1", "device", "/dev/nvme0n1", "mountpoint", "/srv/file-cluster/meta-1/d1"),
		sidecarStorageTestSample(fileClusterWearCheckFilesystem, 1.92e12, "host", "store-1", "device", "/dev/nvme1n1p1", "mountpoint", "/srv/file-cluster/meta-1/d2"),
		sidecarStorageTestSample(fileClusterWearCheckForgivenBytes, 0.5*1.92e12, "cluster", "meta-1"),
	}
	for _, device := range []string{"nvme0n1", "nvme1n1", "nvme2n1"} {
		samples = append(samples,
			sidecarStorageTestSample(fileClusterWearCheckUsed, used, "host", "store-1", "device", device),
			sidecarStorageTestSample(fileClusterWearCheckUsedThen, usedThen, "host", "store-1", "device", device),
			sidecarStorageTestSample(fileClusterWearCheckSpare, 100, "host", "store-1", "device", device),
			sidecarStorageTestSample(fileClusterWearCheckMediaErrors, 0, "host", "store-1", "device", device),
		)
	}
	return samples
}

// Sets every sample of check on device, or of the cluster with no device, to value.
func setFileClusterWearTestValue(samples []*sidecarStorageSample, check string, device string, value float64) {
	for _, sample := range samples {
		if sample.check == check && sample.label("device") == device {
			sample.value = value
		}
	}
}

func evaluateFileClusterWear(samples []*sidecarStorageSample, gauges SidecarStorageGauges) []finding {
	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), gauges)
	return fileClusterWearProbe{}.evaluate(settings, []string{"meta-1"}, fileClusterWearTestPaths(), samples)
}

// 5% used, 1 point in 30 days: a 1 DWPD drive's rating lasts 3,000 days at that rate, over five years.
func TestFileClusterWearHealthyDrivesRaiseNothingAndPublishRatingDays(t *testing.T) {
	gauges := newFakeSidecarStorageGauges()
	requireSidecarStorageRaised(t, evaluateFileClusterWear(fileClusterWearTestSamples(5, 4), gauges))
	if days, ok := gauges.clusterRatings["meta-1"]; !ok || math.Abs(days-2850) > 0.001 {
		t.Errorf("rating days %v %v", days, ok)
	}
	// no wear in 30 days: +Inf
	requireSidecarStorageRaised(t, evaluateFileClusterWear(fileClusterWearTestSamples(5, 5), gauges))
	if days := gauges.clusterRatings["meta-1"]; !math.IsInf(days, 1) {
		t.Errorf("rating days %v", days)
	}
	// no reading 30 days old: not rated
	samples := fileClusterWearTestSamples(5, 4)
	kept := []*sidecarStorageSample{}
	for _, sample := range samples {
		if sample.check != fileClusterWearCheckUsedThen {
			kept = append(kept, sample)
		}
	}
	requireSidecarStorageRaised(t, evaluateFileClusterWear(kept, gauges))
	if _, ok := gauges.clusterRatings["meta-1"]; ok {
		t.Error("a drive with no 30-day reading was rated")
	}
}

// §19.8's lines on the busiest drive: 24 h past 0.8 warns, a 7-day mean over 0.5 warns and over 0.75 is the
// business-hours class alone.
func TestFileClusterWearDriveWriteLines(t *testing.T) {
	cases := []struct {
		day  float64
		week float64
		want []string
	}{
		{day: 0.8, week: 0.5},
		{day: 0.81, week: 0.5, want: []string{"filecluster-drive-dwpd-day meta-1"}},
		{day: 0.6, week: 0.51, want: []string{"filecluster-drive-dwpd-week meta-1"}},
		{day: 0.6, week: 0.75, want: []string{"filecluster-drive-dwpd-week meta-1"}},
		{day: 0.9, week: 0.76, want: []string{"filecluster-drive-dwpd-business meta-1", "filecluster-drive-dwpd-day meta-1"}},
	}
	for _, c := range cases {
		samples := fileClusterWearTestSamples(5, 4)
		for _, sample := range samples {
			if sample.check == fileClusterWearCheckDwpd {
				switch sample.label("window") {
				case "24h":
					sample.value = c.day
				case "7d":
					sample.value = c.week
				}
			}
		}
		requireSidecarStorageRaised(t, evaluateFileClusterWear(samples, nil), c.want...)
	}
}

// SMART's media errors and spare under 10% warn on the drive, a partition taken back to its disk; the boot
// drive no cluster's path names is not judged.
func TestFileClusterWearSmartWarnsOnTheClustersDrives(t *testing.T) {
	samples := fileClusterWearTestSamples(5, 4)
	setFileClusterWearTestValue(samples, fileClusterWearCheckMediaErrors, "nvme0n1", 2)
	setFileClusterWearTestValue(samples, fileClusterWearCheckSpare, "nvme1n1", 9)
	setFileClusterWearTestValue(samples, fileClusterWearCheckSpare, "nvme2n1", 1)
	setFileClusterWearTestValue(samples, fileClusterWearCheckMediaErrors, "nvme2n1", 7)
	requireSidecarStorageRaised(t, evaluateFileClusterWear(samples, nil),
		"filecluster-drive-media-errors store-1/nvme0n1", "filecluster-drive-spare-low store-1/nvme1n1")
}

// A drive whose whole rating lasts under five years at its 30-day rate: 3 points in 30 days is 1,000 days.
// The finding shows P0's forgiven excess against the ~420 drive-writes.
func TestFileClusterWearRatingShortPagesInBusinessHours(t *testing.T) {
	samples := fileClusterWearTestSamples(40, 37)
	setFileClusterWearTestValue(samples, fileClusterWearCheckUsed, "nvme1n1", 40)
	setFileClusterWearTestValue(samples, fileClusterWearCheckUsedThen, "nvme1n1", 39)
	gauges := newFakeSidecarStorageGauges()
	findings := evaluateFileClusterWear(samples, gauges)
	requireSidecarStorageRaised(t, findings, "filecluster-drive-rating-short store-1/nvme0n1")
	for _, finding := range findings {
		if finding.class == fileClusterWearClassRatingShort && !finding.healthy && !strings.Contains(finding.observed, "forgiven_drive_writes_[2592000s]=0.500 of ~420") {
			t.Errorf("finding %+v", finding)
		}
	}
	if days := gauges.clusterRatings["meta-1"]; math.Abs(days-600) > 0.001 {
		t.Errorf("rating days %v, the first drive's 60 points at 0.1 a day", days)
	}
}

// No drive writes or no SMART log for the cluster, Mimir down, or admin info unread: blind.
func TestFileClusterWearMissingSourcesAreBlind(t *testing.T) {
	samples := fileClusterWearTestSamples(5, 4)
	requireSidecarStorageRaised(t, evaluateFileClusterWear(samples[1:], nil), "filecluster-wear-unobservable meta-1")
	requireSidecarStorageRaised(t, evaluateFileClusterWear(samples[:6], nil), "filecluster-wear-unobservable meta-1")

	source := newFakeFileClusterAdminSource()
	settings := sidecarStorageTestSettings(source, nil)
	probe := fileClusterWearProbe{query: fixedSidecarStorageQuery(nil, errors.New("no services host"))}
	findings, err := probe.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings, "filecluster-wear-unobservable mimir")

	source.errs["meta-1"] = errors.New("connection refused")
	probe = fileClusterWearProbe{query: fixedSidecarStorageQuery(fileClusterWearTestSamples(5, 4), nil)}
	findings, err = probe.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings, "filecluster-wear-unobservable meta-1", "filecluster-wear-unobservable meta-1")
}

// The query joins drives to the paths admin info lists, and reads percentage used 30 days ago.
func TestFileClusterWearQueryNamesItsSeries(t *testing.T) {
	query := fileClusterWearQuery("synthetic", DefaultSidecarStorageSettings(), []string{"/srv/file-cluster/meta-1/d1"})
	for _, part := range []string{
		`urnetwork_filecluster_drive_dwpd{env="synthetic"}`,
		`urnetwork_filecluster_smart_percentage_used{env="synthetic",job="file_cluster"} offset 2592000s`,
		`urnetwork_filecluster_smart_available_spare{env="synthetic",job="file_cluster"}`,
		`node_filesystem_size_bytes{env="synthetic",mountpoint=~"/srv/file-cluster/meta-1/d1"}`,
		`increase(urnetwork_evs_budget_p0_forgiven_bytes_total{env="synthetic"}[2592000s])`,
	} {
		if !strings.Contains(query, part) {
			t.Errorf("the query lacks %s: %s", part, query)
		}
	}
}
