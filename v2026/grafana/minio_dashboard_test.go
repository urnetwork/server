// Keeps the MinIO dashboard's host disk context on MinIO's own drives.
package grafana

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// MinIO's erasure set spans one XFS filesystem per drive, mounted at
// /mnt/blob1 to /mnt/blob8 since 2026-10-24 with MinIO's data under
// /mnt/blobN. /mnt/data on the MinIO host is a different disk that holds
// Redis's snapshots.
const minioDriveMountpoints = `/mnt/blob[0-9]+`

// Every drive gets a line, plus the drives together. The per-drive max
// collapses a remount's old and new device series, so the total cannot count
// one drive twice.
func TestMinioDashboardShowsEveryDriveAndTheirTotal(t *testing.T) {
	const selector = `{env="$env",host=~"$host",job="node",mountpoint=~"` + minioDriveMountpoints + `"}`
	const avail = "max by (host, mountpoint) (node_filesystem_avail_bytes" + selector + ")"
	const size = "max by (host, mountpoint) (node_filesystem_size_bytes" + selector + ")"
	assertDiagnosticDashboardPanels(t, "minio.json", []diagnosticDashboardPanel{
		{id: 11, unit: "percent", targets: []testTarget{
			{Expr: "100 * (1 - " + avail + " / " + size + ")", LegendFormat: "{{host}} {{mountpoint}}"},
			{Expr: "100 * (1 - sum by (host) (" + avail + ") / sum by (host) (" + size + "))", LegendFormat: "{{host}} total"},
		}, descriptionParts: []string{"/mnt/blobN", "not only MinIO's objects", "any one drive is full", "about 1% free", "fullest drive"}},
	})
	dashboard := readTestDashboard(t, "minio.json")
	if panel := dashboardPanelById(dashboard, 11); panel == nil || panel.Title != "host drive space used" {
		t.Fatal("the drive space panel must be titled for the drives, not a single data disk")
	}
	for _, expression := range dashboardExpressions(dashboard) {
		if strings.Contains(expression, "/mnt/data") {
			t.Errorf("MinIO query reads /mnt/data, which is Redis's disk on the MinIO host: %s", expression)
		}
	}
	// PromQL anchors a label regex at both ends.
	drives := regexp.MustCompile(`^(?:` + minioDriveMountpoints + `)$`)
	for _, mountpoint := range []string{"/mnt/blob1", "/mnt/blob2", "/mnt/blob3", "/mnt/blob4", "/mnt/blob5", "/mnt/blob6", "/mnt/blob7", "/mnt/blob8"} {
		if !drives.MatchString(mountpoint) {
			t.Errorf("the drive selector misses %s", mountpoint)
		}
	}
	for _, mountpoint := range []string{"/", "/mnt/data"} {
		if drives.MatchString(mountpoint) {
			t.Errorf("the drive selector admits %s, which is not a MinIO drive", mountpoint)
		}
	}
}

// XFS fails every new file once a drive's inodes reach its imaxpct ceiling,
// however many bytes are free, so each drive's free inodes get a line. The
// panel reads counts rather than a share of a ceiling that xfs_growfs -m can
// raise, and its threshold lines are the monitor's floors: SIGNALS.md §11.6b
// minio-inode-runway pages under 15M free inodes and warns under 30M.
func TestMinioDashboardShowsFreeInodesPerDrive(t *testing.T) {
	const selector = `{env="$env",host=~"$host",job="node",mountpoint=~"` + minioDriveMountpoints + `"}`
	assertDiagnosticDashboardPanels(t, "minio.json", []diagnosticDashboardPanel{
		{id: 13, unit: "short", targets: []testTarget{
			{Expr: "max by (host, mountpoint) (node_filesystem_files_free" + selector + ")", LegendFormat: "{{host}} {{mountpoint}}"},
		}, descriptionParts: []string{"/mnt/blobN", "imaxpct", "ENOSPC however many bytes are free", "every MinIO write fails", "30M", "15M", "minio-inode-runway"}},
	})
	panel := dashboardPanelById(readTestDashboard(t, "minio.json"), 13)
	if panel == nil || panel.Title != "host drive inodes free" {
		t.Fatal("the free inode panel must be titled for the drives' inodes")
	}
	steps := panel.FieldConfig.Defaults.Thresholds.Steps
	floor := func(index int, color string, value float64) bool {
		return index < len(steps) && steps[index].Color == color && steps[index].Value != nil && *steps[index].Value == value
	}
	if len(steps) != 3 || steps[0].Value != nil || steps[0].Color != "red" || !floor(1, "orange", 15e6) || !floor(2, "green", 30e6) {
		t.Fatal("free inodes must be red under the 15M page floor, orange under the 30M warn floor and green above it")
	}
	if mode := panel.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode; mode != "line" {
		t.Errorf("the inode floors must be drawn as lines, got thresholdsStyle %q", mode)
	}
}

// MinIO refuses writes once any one drive is full or the drives together are
// down to about 1% free. The threshold lines must come before that, rise in
// order, be drawn on the time series, and be named in the description.
func TestMinioDriveSpaceThresholdsComeBeforeMinioRefusesWrites(t *testing.T) {
	dashboard := readTestDashboard(t, "minio.json")
	panel := dashboardPanelById(dashboard, 11)
	if panel == nil {
		t.Fatal("the drive space panel is missing")
	}
	steps := panel.FieldConfig.Defaults.Thresholds.Steps
	levels := []string{}
	for _, step := range steps {
		if step.Value == nil {
			levels = append(levels, step.Color)
		} else {
			levels = append(levels, fmt.Sprintf("%s at %g", step.Color, *step.Value))
		}
	}
	if len(steps) < 3 || steps[0].Value != nil {
		t.Fatalf("drive space needs a base step and at least two threshold lines: [%s]", strings.Join(levels, ", "))
	}
	previous := 0.0
	for _, step := range steps[1:] {
		if step.Value == nil || *step.Value <= previous || 99 <= *step.Value {
			t.Fatalf("drive space thresholds must rise and stay below MinIO's write stop at about 99%%: [%s]", strings.Join(levels, ", "))
		}
		previous = *step.Value
		if !strings.Contains(panel.Description, fmt.Sprintf("%g%%", *step.Value)) {
			t.Errorf("the description does not name the %g%% threshold line", *step.Value)
		}
	}
	if mode := panel.FieldConfig.Defaults.Custom.ThresholdsStyle.Mode; mode != "line" {
		t.Errorf("drive space thresholds must be drawn as lines, got thresholdsStyle %q", mode)
	}
}
