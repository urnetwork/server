// Synthetic drive inode histories exercise the §11.6b inode layer through the
// public signal. Nothing here reads a gateway or host.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// Free inodes that are free at the grid end, were lost at perDay over the
// newest over of history and were flat before it; whole counts, as node
// exporter reports them.
func minioRunwayTestInodeDecline(free, perDay float64, over time.Duration) func(int, time.Duration) float64 {
	return func(_ int, age time.Duration) float64 {
		return math.Round(free + perDay*min(age, over).Hours()/24)
	}
}

func minioRunwayTestRequire(t *testing.T, alert Alert, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("%s %s lost %q: %s", alert.Class, alert.Target, want, alert.Observed)
		}
	}
}

// The range query reads both inode gauges with the byte gauges' owned drive
// selector, each in its own partition.
func TestMinioRunwayQueryReadsDriveInodes(t *testing.T) {
	query := minioRunwayQuery("synthetic", []string{minioRunwayTestHost}, DefaultMinioRunwaySettings())
	for _, metric := range []struct{ name, check string }{
		{"node_filesystem_files_free", "drive_files_free"},
		{"node_filesystem_files", "drive_files"},
	} {
		want := "label_replace(" + metric.name + `{env="synthetic",job="node",host=~"(?:minio-1)",mountpoint=~"/mnt/blob[0-9]+"},"monitor_check","` + metric.check + `","__name__",".*")`
		if !strings.Contains(query, want) {
			t.Fatalf("MinIO runway query does not read %s as %s", metric.name, metric.check)
		}
	}
}

// The inode wall: flat drives with terabytes of free bytes, but under the page
// floor of free inodes. Only the inode layer alerts, and it pages.
func TestMinioRunwayInodeWallPagesBesideFreeBytes(t *testing.T) {
	store := minioRunwayTestHealthy()
	store.freeInodes = func(drive int, _ time.Duration) float64 { return 12*minioRunwayTestMillion + float64(1000*drive) }
	alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), store)})
	if len(alerts) != 1 {
		t.Fatalf("an inode wall beside free bytes produced %d alerts, want the inode page alone", len(alerts))
	}
	alert := storageRunwayTestAlertFor(t, alerts, "minio-inode-runway", "minio-1/inodes")
	if alert.Severity != SeverityPage || alert.Sustain != 2 || alert.unobservable ||
		!strings.Contains(alert.Symptom, "/mnt/blob1 on minio-1 has 12M free inodes, under the 15M page floor") {
		t.Fatalf("wrong inode wall alert: %s %s", alert.Severity, alert.Symptom)
	}
	minioRunwayTestRequire(t, alert, "band=page free_band=page runway_band=none ",
		"worst_drive=/mnt/blob1 free_inodes=12000000 total_inodes=375073792 used_pct=96.80 ", "runway_days=none-no-decline ",
		"least_free_drive=/mnt/blob1 least_free_inodes=12000000 census_complete=true ", "/mnt/blob8 free_inodes=12007000]")
	requireAlertOmits(t, alert, "private-source-canary")
}

// The measured 2026-11-07 shape: every drive about 42.85M free of its
// 375.07M ceiling and losing 8M a day. Above both floors, its 5.4 days to
// zero warn.
func TestMinioRunwayInodeShapeOf20261107Warns(t *testing.T) {
	store := minioRunwayTestHealthy()
	decline := minioRunwayTestInodeDecline(42.85*minioRunwayTestMillion, 8*minioRunwayTestMillion, 8*24*time.Hour)
	store.freeInodes = func(drive int, age time.Duration) float64 { return decline(drive, age) + float64(1000*drive) }
	alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), store)})
	if len(alerts) != 1 {
		t.Fatalf("the 2026-11-07 inode shape produced %d alerts, want the inode warning alone", len(alerts))
	}
	alert := storageRunwayTestAlertFor(t, alerts, "minio-inode-runway", "minio-1/inodes")
	if alert.Severity != SeverityWarn || alert.Sustain != 2 ||
		!strings.Contains(alert.Symptom, "/mnt/blob1 on minio-1 runs out of inodes in 5.4 days at its fastest measured decline, with 42.85M free") {
		t.Fatalf("wrong inode runway alert: %s %s", alert.Severity, alert.Symptom)
	}
	minioRunwayTestRequire(t, alert, "band=warn free_band=none runway_band=warn ",
		"worst_drive=/mnt/blob1 free_inodes=42850000 total_inodes=375073792 used_pct=88.58 ",
		"decline_6h_inodes_per_day=8000000 decline_24h_inodes_per_day=8000000 rate_window=6h runway_days=5.4 ")
}

// Band edges. The floors are counts, so a raised ceiling with most inodes
// free is healthy at once, whatever its imaxpct.
func TestMinioRunwayInodeThresholdEdges(t *testing.T) {
	const million = minioRunwayTestMillion
	for _, test := range []struct {
		name    string
		free    float64
		perDay  float64
		want    Severity
		bands   string
		symptom string
	}{
		{name: "flat over the warn floor", free: 30.1 * million},
		{name: "flat under the warn floor", free: 29.9 * million, want: SeverityWarn, bands: "band=warn free_band=warn runway_band=none ", symptom: "under the 30M warn floor"},
		{name: "flat over the page floor", free: 15.1 * million, want: SeverityWarn, bands: "band=warn free_band=warn runway_band=none "},
		{name: "flat under the page floor", free: 14.9 * million, want: SeverityPage, bands: "band=page free_band=page runway_band=none "},
		{name: "over a week", free: 200 * million, perDay: 200 * million / 7.1},
		{name: "under a week", free: 200 * million, perDay: 200 * million / 6.9, want: SeverityWarn, bands: "band=warn free_band=none runway_band=warn ", symptom: "runs out of inodes in 6.9 days"},
		{name: "over three days", free: 200 * million, perDay: 200 * million / 3.1, want: SeverityWarn, bands: "band=warn free_band=none runway_band=warn "},
		{name: "under three days", free: 200 * million, perDay: 200 * million / 2.9, want: SeverityPage, bands: "band=page free_band=none runway_band=page ", symptom: "runs out of inodes in 2.9 days"},
		{name: "page floor beside a warn runway", free: 14.9 * million, perDay: 14.9 * million / 5, want: SeverityPage, bands: "band=page free_band=page runway_band=warn ", symptom: "under the 15M page floor"},
	} {
		store := minioRunwayTestHealthy()
		store.freeInodes = minioRunwayTestInodeDecline(test.free, test.perDay, 25*time.Hour)
		alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), store)})
		if test.want == "" {
			if len(alerts) != 0 {
				t.Fatalf("%s: alerted: %s %s", test.name, alerts[0].Class, alerts[0].Observed)
			}
			continue
		}
		if len(alerts) != 1 {
			t.Fatalf("%s: got %d alerts, want the inode layer alone", test.name, len(alerts))
		}
		alert := storageRunwayTestAlertFor(t, alerts, "minio-inode-runway", "minio-1/inodes")
		if alert.Severity != test.want || !strings.Contains(alert.Symptom, test.symptom) {
			t.Fatalf("%s: severity %s, want %s: %s", test.name, alert.Severity, test.want, alert.Symptom)
		}
		minioRunwayTestRequire(t, alert, test.bands)
	}

	// xfs_growfs -m 15 two hours ago: the ceiling triples and the free inodes
	// jump from about 43M to about 793M, still falling 8M a day.
	raised := minioRunwayTestHealthy()
	raised.inodes = 3 * minioRunwayTestInodes
	raised.freeInodes = func(_ int, age time.Duration) float64 {
		lost := 8 * million * age.Hours() / 24
		if age < 2*time.Hour {
			return math.Round(793*million + lost)
		}
		return math.Round(43.5*million + lost)
	}
	if alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), raised)}); len(alerts) != 0 {
		t.Fatalf("a raised ceiling with 793M free inodes alerted: %s %s", alerts[0].Class, alerts[0].Observed)
	}
}

// Each layer is its own. Without inode series only the inode layer is
// unknown; a byte-series gap leaves the byte layers unknown while the inode
// layer still pages; a stale inode layer after a fast decline pages
// (emission rule 1) without inventing a measured violation.
func TestMinioRunwayInodeLayerIsIndependent(t *testing.T) {
	missing := minioRunwayTestHealthy()
	missing.freeInodes = nil
	alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), missing)})
	if len(alerts) != 1 {
		t.Fatalf("missing inode series produced %d alerts, want the inode layer unknown alone", len(alerts))
	}
	alert := storageRunwayTestAlertFor(t, alerts, "minio-runway-unobservable", "minio-1/inodes")
	if !alert.unobservable || alert.Severity != SeverityWarn {
		t.Fatalf("missing inode series has severity %s", alert.Severity)
	}
	minioRunwayTestRequire(t, alert, "layer=inodes reason=drive-filesystems=0 minio-drive-count=8 recent_decline=unknown")

	bytesGap := minioRunwayTestHealthy()
	bytesGap.driveOmit = func(drive int, _ time.Duration) bool { return drive == 0 }
	bytesGap.freeInodes = func(int, time.Duration) float64 { return 12 * minioRunwayTestMillion }
	alerts = minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), bytesGap)})
	minioRunwayTestRequire(t, storageRunwayTestAlertFor(t, alerts, "minio-runway-unobservable", "minio-1/drives"), "reason=drive-filesystems=7 minio-drive-count=8")
	if alert := storageRunwayTestAlertFor(t, alerts, "minio-inode-runway", "minio-1/inodes"); alert.Severity != SeverityPage {
		t.Fatalf("a byte-series gap changed the inode verdict: %s", alert.Severity)
	}
	for _, alert := range alerts {
		if alert.Class == "minio-runway-unobservable" && alert.Target == "minio-1/inodes" {
			t.Fatal("a byte-series gap left the complete inode layer unknown")
		}
	}

	stale := minioRunwayTestHealthy()
	// 2M an hour over the last twelve hours, then no sample for 45 minutes
	stale.freeInodes = minioRunwayTestInodeDecline(100*minioRunwayTestMillion, 48*minioRunwayTestMillion, 12*time.Hour)
	stale.inodeOmit = func(_ int, age time.Duration) bool { return age < 45*time.Minute }
	alerts = minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), stale)})
	if len(alerts) != 1 {
		t.Fatalf("a stale inode layer produced %d alerts, want its unknown alone", len(alerts))
	}
	alert = storageRunwayTestAlertFor(t, alerts, "minio-runway-unobservable", "minio-1/inodes")
	if alert.Severity != SeverityPage {
		t.Fatalf("a stale fast-declining inode layer did not page: %s %s", alert.Severity, alert.Observed)
	}
	minioRunwayTestRequire(t, alert, "reason=stale-drive-inode-series recent_decline_basis=measured recent_decline_inodes_per_hour=2000000")
	requireAlertOmits(t, alert, "private-source-canary")
}

// A failed read after a measured fast inode decline pages from the signal's
// memory; once the memory is older than the risk window it warns again.
func TestMinioRunwayInodeUnknownRemembersRecentDecline(t *testing.T) {
	signal := NewMinioRunwaySignal()
	now := syntheticSettings(nil).Now().UTC()
	store := minioRunwayTestHealthy()
	// 1.5M an hour over the last six hours with 340M free: 9.4 days, no band
	store.freeInodes = minioRunwayTestInodeDecline(340*minioRunwayTestMillion, 36*minioRunwayTestMillion, 6*time.Hour)
	body := minioRunwayTestBody(t, DefaultMinioRunwaySettings().Estimator.grid(now), store)
	if alerts := minioRunwayTestRun(t, signal, minioRunwayTestSources{now: now, body: body}); len(alerts) != 0 {
		t.Fatalf("a nine-day inode runway alerted: %s %s", alerts[0].Class, alerts[0].Observed)
	}
	for _, test := range []struct {
		later time.Duration
		want  Severity
		basis string
	}{
		{later: 15 * time.Minute, want: SeverityPage, basis: "reason=source-command-unavailable recent_decline_basis=remembered recent_decline_inodes_per_hour=1500000 "},
		{later: 7 * time.Hour, want: SeverityWarn, basis: "reason=source-command-unavailable recent_decline=unknown "},
	} {
		settings := minioRunwayTestSettings(t, minioRunwayTestSources{now: now.Add(test.later)})
		settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			return "", errors.New("exit status 1: private-source-canary")
		}}
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil {
			t.Fatal("a failed read returned an error instead of unknown layers")
		}
		alert := storageRunwayTestAlertFor(t, alerts, "minio-runway-unobservable", "minio-1/inodes")
		if alert.Severity != test.want {
			t.Fatalf("after %s the unknown inode layer has severity %s, want %s: %s", test.later, alert.Severity, test.want, alert.Observed)
		}
		minioRunwayTestRequire(t, alert, test.basis)
		requireAlertOmits(t, alert, "private-source-canary")
	}
}

// The MinIO dashboard's free-inode panel reads this signal's drives and draws
// its floors, so the lines an operator reads are the ones that warn and page.
func TestMinioRunwayInodeFloorsMatchTheDashboard(t *testing.T) {
	raw, err := os.ReadFile("../grafana/dashboards/minio.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Panels []struct {
			Title       string `json:"title"`
			FieldConfig struct {
				Defaults struct {
					Thresholds struct {
						Steps []struct {
							Value *float64 `json:"value"`
						} `json:"steps"`
					} `json:"thresholds"`
				} `json:"defaults"`
			} `json:"fieldConfig"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatal(err)
	}
	settings := DefaultMinioRunwaySettings()
	for _, panel := range dashboard.Panels {
		if panel.Title != "host drive inodes free" {
			continue
		}
		lines := []float64{}
		for _, step := range panel.FieldConfig.Defaults.Thresholds.Steps {
			if step.Value != nil {
				lines = append(lines, *step.Value)
			}
		}
		if !slices.Equal(lines, []float64{settings.InodePageFree, settings.InodeWarnFree}) {
			t.Fatalf("the free-inode panel draws %v, want the page and warn floors", lines)
		}
		if len(panel.Targets) != 1 || !strings.Contains(panel.Targets[0].Expr, `node_filesystem_files_free{`) ||
			!strings.Contains(panel.Targets[0].Expr, `mountpoint=~"`+settings.DriveMountpointPattern+`"`) {
			t.Fatal("the free-inode panel does not read the signal's drives")
		}
		return
	}
	t.Fatal("the MinIO dashboard has no free-inode panel")
}
