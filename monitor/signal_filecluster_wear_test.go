package monitor

// SIGNALS.md §29.4 through the probe's own query, on a fake Mimir holding the series as the storage hosts and the
// budget loop ship them: the drive-write lines, SMART's spare and media errors, the rating projection and the
// rating-days gauge, the join of a drive to its cluster, the blind cases, and the SMART log read under job="node",
// the label the host's shared Fluent Bit gives every series it ships (owner decisions 509 and 579), and under no
// other job.

import (
	"context"
	"errors"
	"maps"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// One series as Mimir holds it.
type fileClusterWearTestSeries struct {
	metric string
	labels map[string]string
	// its value now
	value float64
	// its value 30 days ago, where it has one
	then    float64
	hasThen bool
	// its newest sample is past the freshness bound: a selector kept to fresh series misses it, while its value
	// 30 days ago still reads
	stale bool
}

// One series of metric with labels as pairs.
func newFileClusterWearTestSeries(metric string, value float64, labels ...string) *fileClusterWearTestSeries {
	series := &fileClusterWearTestSeries{metric: metric, labels: map[string]string{}, value: value}
	for i := 0; i+1 < len(labels); i += 2 {
		series.labels[labels[i]] = labels[i+1]
	}
	return series
}

// The series as one sample of an answer tagged check, its name among its labels as Mimir keeps it.
func (self *fileClusterWearTestSeries) sample(check string, value float64) *sidecarStorageSample {
	labels := maps.Clone(self.labels)
	labels["__name__"] = self.metric
	return &sidecarStorageSample{check: check, labels: labels, value: value}
}

// A SMART log series, as xops's smart-textfile.sh names them.
func isFileClusterWearTestSmart(series *fileClusterWearTestSeries) bool {
	return strings.HasPrefix(series.metric, "urnetwork_filecluster_smart_")
}

// What Mimir holds for meta-1, every line within bounds: store-1's SMART log for meta-1's two drives and its boot
// drive, and the host's filesystems, all labelled as the host's shared Fluent Bit ships them (env, host and
// job="node"); the budget loop's drive writes; and P0's forgiven excess, half a drive-write over the 30 days.
// used and usedThen are each drive's percentage used now and 30 days ago.
func fileClusterWearTestSeriesSet(used float64, usedThen float64) []*fileClusterWearTestSeries {
	node := func(metric string, value float64, labels ...string) *fileClusterWearTestSeries {
		return newFileClusterWearTestSeries(metric, value, append([]string{"env", "synthetic", "host", "store-1", "job", "node"}, labels...)...)
	}
	worker := func(metric string, value float64, labels ...string) *fileClusterWearTestSeries {
		return newFileClusterWearTestSeries(metric, value, append([]string{"env", "synthetic", "job", "taskworker", "service", "taskworker"}, labels...)...)
	}
	forgivenRevocation := worker("urnetwork_evs_budget_p0_forgiven_bytes_total", 0.8*1.92e12, "cluster", "meta-1", "kind", "revocation")
	forgivenRevocation.then, forgivenRevocation.hasThen = 0.5*1.92e12, true
	forgivenLog := worker("urnetwork_evs_budget_p0_forgiven_bytes_total", 0.2*1.92e12, "cluster", "meta-1", "kind", "log")
	forgivenLog.then, forgivenLog.hasThen = 0, true
	series := []*fileClusterWearTestSeries{
		worker("urnetwork_filecluster_drive_dwpd", 0.40, "cluster", "meta-1", "window", "24h"),
		worker("urnetwork_filecluster_drive_dwpd", 0.41, "cluster", "meta-1", "window", "7d"),
		worker("urnetwork_filecluster_drive_dwpd", 0.42, "cluster", "meta-1", "window", "30d"),
		forgivenRevocation,
		forgivenLog,
		node("node_filesystem_size_bytes", 1.92e12, "device", "/dev/nvme0n1", "fstype", "xfs", "mountpoint", "/srv/file-cluster/meta-1/d1"),
		node("node_filesystem_size_bytes", 1.92e12, "device", "/dev/nvme1n1p1", "fstype", "xfs", "mountpoint", "/srv/file-cluster/meta-1/d2"),
		// the boot drive's root, a path no cluster names
		node("node_filesystem_size_bytes", 2.4e11, "device", "/dev/nvme2n1p2", "fstype", "ext4", "mountpoint", "/"),
	}
	for _, device := range []string{"nvme0n1", "nvme1n1", "nvme2n1"} {
		percentageUsed := node("urnetwork_filecluster_smart_percentage_used", used, "device", device)
		percentageUsed.then, percentageUsed.hasThen = usedThen, true
		series = append(series,
			percentageUsed,
			node("urnetwork_filecluster_smart_available_spare", 100, "device", device),
			node("urnetwork_filecluster_smart_media_errors", 0, "device", device),
		)
	}
	return series
}

// The set with an input of each drive alert past its line: media errors on nvme0n1, nvme1n1's spare under 10%,
// and nvme1n1's 3 points in 30 days, at which its whole rating lasts 1,000 days.
func fileClusterWearTestFaultySeriesSet() []*fileClusterWearTestSeries {
	series := fileClusterWearTestSeriesSet(5, 4)
	setFileClusterWearTestValue(series, "urnetwork_filecluster_smart_media_errors", "nvme0n1", 2)
	setFileClusterWearTestValue(series, "urnetwork_filecluster_smart_available_spare", "nvme1n1", 9)
	setFileClusterWearTestValue(series, "urnetwork_filecluster_smart_percentage_used", "nvme1n1", 40)
	setFileClusterWearTestThen(series, "urnetwork_filecluster_smart_percentage_used", "nvme1n1", 37)
	return series
}

// Sets the value now of each series of metric on device.
func setFileClusterWearTestValue(series []*fileClusterWearTestSeries, metric string, device string, value float64) {
	for _, s := range series {
		if s.metric == metric && s.labels["device"] == device {
			s.value = value
		}
	}
}

// Sets the value 30 days ago of each series of metric on device.
func setFileClusterWearTestThen(series []*fileClusterWearTestSeries, metric string, device string, then float64) {
	for _, s := range series {
		if s.metric == metric && s.labels["device"] == device {
			s.then, s.hasThen = then, true
		}
	}
}

// The series that keep is true of.
func filterFileClusterWearTestSeries(series []*fileClusterWearTestSeries, keep func(*fileClusterWearTestSeries) bool) []*fileClusterWearTestSeries {
	kept := []*fileClusterWearTestSeries{}
	for _, s := range series {
		if keep(s) {
			kept = append(kept, s)
		}
	}
	return kept
}

// Copies of the set's SMART series labelled job, the set itself unchanged.
func fileClusterWearTestSmartUnder(series []*fileClusterWearTestSeries, job string) []*fileClusterWearTestSeries {
	copies := []*fileClusterWearTestSeries{}
	for _, s := range filterFileClusterWearTestSeries(series, isFileClusterWearTestSmart) {
		moved := *s
		moved.labels = maps.Clone(s.labels)
		moved.labels["job"] = job
		copies = append(copies, &moved)
	}
	return copies
}

// A fake Mimir holding series. It answers the query term by term as Mimir would, each selector's matchers applied
// to the series' labels, so a series the query does not select is not read. It knows the shapes
// sidecar_storage.go's helpers build: a selector kept to its fresh series, a selector offset by the rating window
// (each series' value then), and a sum by labels of a counter's increase over the window. A term or selector of
// any other shape fails the test, so a changed query cannot pass by the fake's leave.
func fileClusterWearTestMimir(t *testing.T, series []*fileClusterWearTestSeries) sidecarStorageQuery {
	termPattern := regexp.MustCompile(`^label_replace\((.+),"monitor_check","([a-z0-9_]+)","__name__","\.\*"\)$`)
	freshPattern := regexp.MustCompile(`^\((.+) and \(timestamp\((.+)\) >= time\(\) - [0-9]+\)\)$`)
	offsetPattern := regexp.MustCompile(`^(.+) offset [0-9]+s$`)
	increasePattern := regexp.MustCompile(`^sum by \(([a-z_]+(?:,[a-z_]+)*)\) \(increase\((.+)\[[0-9]+s\]\)\)$`)
	metricPattern := regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	matcherPattern := regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)(=~|!~|!=|=)("(?:[^"\\]|\\.)*")$`)

	// text split at each separator outside brackets and quotes
	split := func(text string, separator string) []string {
		parts := []string{}
		depth := 0
		quoted := false
		start := 0
		for i := 0; i < len(text); i++ {
			switch c := text[i]; {
			case quoted && c == '\\':
				i += 1
			case c == '"':
				quoted = !quoted
			case quoted:
			case c == '(' || c == '{' || c == '[':
				depth += 1
			case c == ')' || c == '}' || c == ']':
				depth -= 1
			case depth == 0 && strings.HasPrefix(text[i:], separator):
				parts = append(parts, text[start:i])
				start = i + len(separator)
				i = start - 1
			}
		}
		return append(parts, text[start:])
	}

	// the series a selector names: its metric, with every matcher true of the series' labels, a label the
	// series lacks reading ""
	selectSeries := func(selector string) []*fileClusterWearTestSeries {
		t.Helper()
		open := strings.Index(selector, "{")
		if open < 0 || !strings.HasSuffix(selector, "}") || !metricPattern.MatchString(selector[:open]) {
			t.Fatalf("the fake Mimir does not know the selector %s", selector)
		}
		matchers := []func(map[string]string) bool{}
		for _, text := range split(selector[open+1:len(selector)-1], ",") {
			match := matcherPattern.FindStringSubmatch(text)
			if match == nil {
				t.Fatalf("the fake Mimir does not know the matcher %s in %s", text, selector)
			}
			name, operator := match[1], match[2]
			value, err := strconv.Unquote(match[3])
			if err != nil {
				t.Fatalf("matcher %s: %v", text, err)
			}
			switch operator {
			case "=":
				matchers = append(matchers, func(labels map[string]string) bool { return labels[name] == value })
			case "!=":
				matchers = append(matchers, func(labels map[string]string) bool { return labels[name] != value })
			default:
				// PromQL anchors a regular expression matcher at both ends
				pattern, err := regexp.Compile("^(?:" + value + ")$")
				if err != nil {
					t.Fatalf("matcher %s: %v", text, err)
				}
				negated := operator == "!~"
				matchers = append(matchers, func(labels map[string]string) bool { return pattern.MatchString(labels[name]) != negated })
			}
		}
		selected := []*fileClusterWearTestSeries{}
		for _, s := range series {
			matches := s.metric == selector[:open]
			for _, matcher := range matchers {
				matches = matches && matcher(s.labels)
			}
			if matches {
				selected = append(selected, s)
			}
		}
		return selected
	}

	return func(ctx context.Context, env *probeEnv, query string) ([]*sidecarStorageSample, error) {
		t.Helper()
		samples := []*sidecarStorageSample{}
		for _, term := range split(query, " or ") {
			match := termPattern.FindStringSubmatch(term)
			if match == nil {
				t.Fatalf("the fake Mimir does not know the term %s", term)
			}
			inner, check := match[1], match[2]
			if fresh := freshPattern.FindStringSubmatch(inner); fresh != nil && fresh[1] == fresh[2] {
				for _, s := range selectSeries(fresh[1]) {
					if !s.stale {
						samples = append(samples, s.sample(check, s.value))
					}
				}
			} else if offset := offsetPattern.FindStringSubmatch(inner); offset != nil {
				for _, s := range selectSeries(offset[1]) {
					if s.hasThen {
						samples = append(samples, s.sample(check, s.then))
					}
				}
			} else if increase := increasePattern.FindStringSubmatch(inner); increase != nil {
				byNames := strings.Split(increase[1], ",")
				groupKeys := []string{}
				groupSamples := map[string]*sidecarStorageSample{}
				for _, s := range selectSeries(increase[2]) {
					labels := map[string]string{}
					groupKey := ""
					for _, name := range byNames {
						labels[name] = s.labels[name]
						groupKey += name + "=" + strconv.Quote(s.labels[name]) + ","
					}
					group, ok := groupSamples[groupKey]
					if !ok {
						group = &sidecarStorageSample{check: check, labels: labels}
						groupSamples[groupKey] = group
						groupKeys = append(groupKeys, groupKey)
					}
					// the series' increase over the window: it holds no reset
					group.value += s.value - s.then
				}
				for _, groupKey := range groupKeys {
					samples = append(samples, groupSamples[groupKey])
				}
			} else {
				t.Fatalf("the fake Mimir does not know the expression %s", inner)
			}
		}
		return samples, nil
	}
}

// The probe's findings for the fake admin source's metadata cluster, meta-1, through its own query to a fake Mimir
// holding series.
func checkFileClusterWear(t *testing.T, series []*fileClusterWearTestSeries, gauges SidecarStorageGauges) []finding {
	t.Helper()
	settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), gauges)
	probe := fileClusterWearProbe{query: fileClusterWearTestMimir(t, series)}
	findings, err := probe.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

// Fails the test unless the unobservable findings' frames are want's, in any order.
func requireFileClusterWearBlindFrames(t *testing.T, findings []finding, want ...string) {
	t.Helper()
	got := []string{}
	for _, finding := range findings {
		if finding.class == fileClusterWearClassUnobservable && !finding.healthy {
			got = append(got, finding.frame)
		}
	}
	slices.Sort(got)
	if want == nil {
		want = []string{}
	}
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("blind frames %v, want %v", got, want)
	}
}

// 5% used, 1 point in 30 days: a 1 DWPD drive's rating lasts 3,000 days at that rate, over five years.
func TestFileClusterWearHealthyDrivesRaiseNothingAndPublishRatingDays(t *testing.T) {
	gauges := newFakeSidecarStorageGauges()
	requireSidecarStorageRaised(t, checkFileClusterWear(t, fileClusterWearTestSeriesSet(5, 4), gauges))
	if days, ok := gauges.clusterRatings["meta-1"]; !ok || math.Abs(days-2850) > 0.001 {
		t.Errorf("rating days %v %v", days, ok)
	}
	// no wear in 30 days: +Inf
	requireSidecarStorageRaised(t, checkFileClusterWear(t, fileClusterWearTestSeriesSet(5, 5), gauges))
	if days := gauges.clusterRatings["meta-1"]; !math.IsInf(days, 1) {
		t.Errorf("rating days %v", days)
	}
	// no reading 30 days old: not rated
	series := fileClusterWearTestSeriesSet(5, 4)
	for _, s := range series {
		if s.metric == "urnetwork_filecluster_smart_percentage_used" {
			s.hasThen = false
		}
	}
	requireSidecarStorageRaised(t, checkFileClusterWear(t, series, gauges))
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
		series := fileClusterWearTestSeriesSet(5, 4)
		for _, s := range series {
			if s.metric == "urnetwork_filecluster_drive_dwpd" {
				switch s.labels["window"] {
				case "24h":
					s.value = c.day
				case "7d":
					s.value = c.week
				}
			}
		}
		requireSidecarStorageRaised(t, checkFileClusterWear(t, series, nil), c.want...)
	}
}

// SMART's media errors and spare under 10% warn on the drive, a partition taken back to its disk; the boot
// drive no cluster's path names is not judged.
func TestFileClusterWearSmartWarnsOnTheClustersDrives(t *testing.T) {
	series := fileClusterWearTestSeriesSet(5, 4)
	setFileClusterWearTestValue(series, "urnetwork_filecluster_smart_media_errors", "nvme0n1", 2)
	setFileClusterWearTestValue(series, "urnetwork_filecluster_smart_available_spare", "nvme1n1", 9)
	setFileClusterWearTestValue(series, "urnetwork_filecluster_smart_available_spare", "nvme2n1", 1)
	setFileClusterWearTestValue(series, "urnetwork_filecluster_smart_media_errors", "nvme2n1", 7)
	requireSidecarStorageRaised(t, checkFileClusterWear(t, series, nil),
		"filecluster-drive-media-errors store-1/nvme0n1", "filecluster-drive-spare-low store-1/nvme1n1")
}

// A drive whose whole rating lasts under five years at its 30-day rate: 3 points in 30 days is 1,000 days.
// The finding shows P0's forgiven excess, summed over its kinds, against the ~420 drive-writes.
func TestFileClusterWearRatingShortPagesInBusinessHours(t *testing.T) {
	series := fileClusterWearTestSeriesSet(40, 37)
	setFileClusterWearTestValue(series, "urnetwork_filecluster_smart_percentage_used", "nvme1n1", 40)
	setFileClusterWearTestThen(series, "urnetwork_filecluster_smart_percentage_used", "nvme1n1", 39)
	gauges := newFakeSidecarStorageGauges()
	findings := checkFileClusterWear(t, series, gauges)
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

// The SMART log as the host's shared Fluent Bit ships it, under job="node" (owner decisions 509 and 579): each of
// the three drive alerts fires on its own input, and nothing reads as blind.
func TestFileClusterWearReadsTheSmartLogUnderJobNode(t *testing.T) {
	findings := checkFileClusterWear(t, fileClusterWearTestFaultySeriesSet(), nil)
	requireSidecarStorageRaised(t, findings,
		"filecluster-drive-media-errors store-1/nvme0n1",
		"filecluster-drive-spare-low store-1/nvme1n1",
		"filecluster-drive-rating-short store-1/nvme1n1")
	requireFileClusterWearBlindFrames(t, findings)
}

// A SMART log under job="file_cluster", the label of a second Fluent Bit input the hosts do not run, is not read:
// alone it leaves meta-1's drives with no SMART log, so the cluster reads blind and none of its alerts fires, and
// beside the healthy log under job="node" its faults raise nothing.
func TestFileClusterWearDoesNotReadTheSmartLogUnderJobFileCluster(t *testing.T) {
	faulty := fileClusterWearTestFaultySeriesSet()
	moved := append(filterFileClusterWearTestSeries(faulty, func(s *fileClusterWearTestSeries) bool { return !isFileClusterWearTestSmart(s) }),
		fileClusterWearTestSmartUnder(faulty, "file_cluster")...)
	findings := checkFileClusterWear(t, moved, nil)
	requireSidecarStorageRaised(t, findings, "filecluster-wear-unobservable meta-1")
	requireFileClusterWearBlindFrames(t, findings, "smart")

	// after the healthy series, so a selector that read both would take the faulty readings
	both := append(fileClusterWearTestSeriesSet(5, 4), fileClusterWearTestSmartUnder(faulty, "file_cluster")...)
	requireSidecarStorageRaised(t, checkFileClusterWear(t, both, nil))
}

// meta-1 reads blind on its SMART log only while the series are absent: with them under job="node" nothing is
// blind, and without them the cluster is blind on its SMART frame alone.
func TestFileClusterWearSmartBlindOnlyWhileItsSeriesAreAbsent(t *testing.T) {
	series := fileClusterWearTestSeriesSet(5, 4)
	findings := checkFileClusterWear(t, series, nil)
	requireSidecarStorageRaised(t, findings)
	requireFileClusterWearBlindFrames(t, findings)

	findings = checkFileClusterWear(t, filterFileClusterWearTestSeries(series, func(s *fileClusterWearTestSeries) bool { return !isFileClusterWearTestSmart(s) }), nil)
	requireSidecarStorageRaised(t, findings, "filecluster-wear-unobservable meta-1")
	requireFileClusterWearBlindFrames(t, findings, "smart")
}

// No drive writes for a window, Mimir down, or admin info unread: blind.
func TestFileClusterWearMissingSourcesAreBlind(t *testing.T) {
	series := fileClusterWearTestSeriesSet(5, 4)
	withoutDay := filterFileClusterWearTestSeries(series, func(s *fileClusterWearTestSeries) bool {
		return !(s.metric == "urnetwork_filecluster_drive_dwpd" && s.labels["window"] == "24h")
	})
	findings := checkFileClusterWear(t, withoutDay, nil)
	requireSidecarStorageRaised(t, findings, "filecluster-wear-unobservable meta-1")
	requireFileClusterWearBlindFrames(t, findings, "dwpd")

	source := newFakeFileClusterAdminSource()
	settings := sidecarStorageTestSettings(source, nil)
	probe := fileClusterWearProbe{query: fixedSidecarStorageQuery(nil, errors.New("no services host"))}
	findings, err := probe.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings, "filecluster-wear-unobservable mimir")

	// without admin info no drive path joins a filesystem, so the SMART log reaches no drive of the cluster
	source.errs["meta-1"] = errors.New("connection refused")
	probe = fileClusterWearProbe{query: fileClusterWearTestMimir(t, series)}
	findings, err = probe.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings, "filecluster-wear-unobservable meta-1", "filecluster-wear-unobservable meta-1")
	requireFileClusterWearBlindFrames(t, findings, "admin-info", "smart")
}

// The query selects the SMART log under job="node" and under no other job, joins drives to the paths admin info
// lists, and reads percentage used 30 days ago.
func TestFileClusterWearQueryNamesItsSeries(t *testing.T) {
	query := fileClusterWearQuery("synthetic", DefaultSidecarStorageSettings(), []string{"/srv/file-cluster/meta-1/d1"})
	for _, part := range []string{
		`urnetwork_filecluster_drive_dwpd{env="synthetic"}`,
		`urnetwork_filecluster_smart_percentage_used{env="synthetic",job="node"} offset 2592000s`,
		`urnetwork_filecluster_smart_percentage_used{env="synthetic",job="node"} and`,
		`urnetwork_filecluster_smart_available_spare{env="synthetic",job="node"}`,
		`urnetwork_filecluster_smart_media_errors{env="synthetic",job="node"}`,
		`node_filesystem_size_bytes{env="synthetic",mountpoint=~"/srv/file-cluster/meta-1/d1"}`,
		`increase(urnetwork_evs_budget_p0_forgiven_bytes_total{env="synthetic"}[2592000s])`,
	} {
		if !strings.Contains(query, part) {
			t.Errorf("the query lacks %s: %s", part, query)
		}
	}
	if strings.Contains(query, `job="file_cluster"`) {
		t.Errorf("the query still selects job=\"file_cluster\": %s", query)
	}
}
