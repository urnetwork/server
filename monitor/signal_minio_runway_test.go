// Synthetic drive, MinIO and backup observations exercise the four §11.6b
// layers through the public signal. Nothing here reads a gateway or host.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	minioRunwayTestHost     = "minio-1"
	minioRunwayTestDrive    = 3840 * storageRunwayTestGigabyte
	minioRunwayTestTerabyte = 1000 * storageRunwayTestGigabyte
)

// One synthetic MinIO host's history. Functions map a grid point's age to a
// value; a nil function omits the partition.
type minioRunwayTestStore struct {
	host       string
	drives     []string
	avail      func(drive int, age time.Duration) float64
	driveOmit  func(drive int, age time.Duration) bool
	size       float64
	driveCount func(age time.Duration) float64
	logical    func(age time.Duration) float64
	usageAge   func(age time.Duration) float64
	putRate    func(age time.Duration) float64
}

// The 2026-10-22 store at rest: four 3.84 TB drives, half free, raw use about
// twice the logical bytes and a steady put rate.
func minioRunwayTestHealthy() minioRunwayTestStore {
	return minioRunwayTestStore{
		host:       minioRunwayTestHost,
		drives:     []string{"/mnt/blob1", "/mnt/blob2", "/mnt/blob3", "/mnt/blob4"},
		avail:      func(int, time.Duration) float64 { return 2000 * storageRunwayTestGigabyte },
		size:       minioRunwayTestDrive,
		driveCount: func(time.Duration) float64 { return 4 },
		logical:    func(time.Duration) float64 { return 3.5 * minioRunwayTestTerabyte },
		usageAge:   func(time.Duration) float64 { return 1800 },
		putRate:    func(time.Duration) float64 { return 90 },
	}
}

func minioRunwayTestBody(t *testing.T, grid storageRunwayGrid, stores ...minioRunwayTestStore) string {
	t.Helper()
	result := []any{}
	series := func(labels map[string]string, value func(time.Duration) float64, omit func(time.Duration) bool) {
		values := [][]any{}
		for at := grid.start; !at.After(grid.end); at = at.Add(grid.step) {
			age := grid.end.Sub(at)
			if omit != nil && omit(age) {
				continue
			}
			values = append(values, []any{at.Unix(), strconv.FormatFloat(value(age), 'f', -1, 64)})
		}
		if len(values) != 0 {
			result = append(result, map[string]any{"metric": labels, "values": values})
		}
	}
	for _, store := range stores {
		for index, mountpoint := range store.drives {
			drive := index
			omit := func(age time.Duration) bool { return store.driveOmit != nil && store.driveOmit(drive, age) }
			for _, check := range []string{"drive_avail", "drive_size"} {
				labels := map[string]string{"env": "synthetic", "job": "node", "host": store.host, "mountpoint": mountpoint,
					"device": "/dev/private-source-canary" + strconv.Itoa(drive), "fstype": "xfs", "monitor_check": check}
				value := func(age time.Duration) float64 { return store.avail(drive, age) }
				if check == "drive_size" {
					value = func(time.Duration) float64 { return store.size }
				}
				series(labels, value, omit)
			}
		}
		for check, value := range map[string]func(time.Duration) float64{
			"drive_count": store.driveCount, "logical_bytes": store.logical, "usage_age": store.usageAge, "put_rate": store.putRate,
		} {
			if value != nil {
				series(map[string]string{"host": store.host, "monitor_check": check}, value, nil)
			}
		}
	}
	body, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "matrix", "result": result}})
	if err != nil {
		t.Fatal("synthetic range encoding failed")
	}
	return string(body)
}

// The backup units' fixed systemctl frame.
func minioRunwayTestUnits(serviceLoad, serviceActive, timerLoad, timerActive, next string) string {
	return "monitor-unit service\nLoadState=" + serviceLoad + "\nActiveState=" + serviceActive +
		"\nmonitor-unit timer\nLoadState=" + timerLoad + "\nActiveState=" + timerActive + "\nNextElapseUSecRealtime=" + next + "\nmonitor-unit end\n"
}

// Inventory, gateway, unit and slot sources for one run.
type minioRunwayTestSources struct {
	body     string
	units    string
	unitsErr error
	slots    []Row
	slotsErr error
	now      time.Time
}

func minioRunwayTestSettings(t *testing.T, sources minioRunwayTestSources) SignalSettings {
	t.Helper()
	if sources.units == "" && sources.unitsErr == nil {
		sources.units = minioRunwayTestUnits("not-found", "inactive", "not-found", "inactive", "")
	}
	if sources.slots == nil && sources.slotsErr == nil {
		sources.slots = []Row{{"slots", "pg-1", "0", "0"}}
	}
	settings := syntheticSettings(&syntheticSource{
		hostFn: func(target HostSettings, command string) (string, error) {
			switch target.Name {
			case "metrics-1":
				for _, want := range []string{"/prometheus/api/v1/query_range", `"monitor_check","drive_avail"`, `mountpoint=~"/mnt/blob[0-9]+"`, `name="PutObject"`} {
					if !strings.Contains(command, want) {
						t.Fatalf("MinIO runway query lost %q", want)
					}
				}
				return sources.body, nil
			case minioRunwayTestHost:
				if !strings.HasPrefix(command, `[ "$(hostname -s)" = 'minio-1' ] || exit 74`) || !strings.Contains(command, "'pg-basebackup.service'") {
					t.Fatal("backup unit read lost its hostname guard or unit")
				}
				return sources.units, sources.unitsErr
			}
			t.Fatalf("MinIO runway contacted an unexpected host")
			return "", nil
		},
		postgresFn: func(query string) ([]Row, error) {
			if !strings.Contains(query, `slot_name LIKE 'pgbackup\_%'`) || !strings.Contains(query, "pg_replication_slots") {
				t.Fatal("backup slot query lost its prefix or source")
			}
			return sources.slots, sources.slotsErr
		},
	})
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "metrics-1", Roles: []string{"services"}},
		HostSettings{Name: minioRunwayTestHost, Roles: []string{"minio", "redis-cluster"}})
	if !sources.now.IsZero() {
		at := sources.now
		settings.Now = func() time.Time { return at }
	}
	return settings
}

func minioRunwayTestRun(t *testing.T, signal Signal, sources minioRunwayTestSources) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), minioRunwayTestSettings(t, sources))
	if err != nil {
		t.Fatal("MinIO runway run failed")
	}
	return alerts
}

func minioRunwayTestGrid() storageRunwayGrid {
	return DefaultMinioRunwaySettings().Estimator.grid(syntheticSettings(nil).Now())
}

func requireNoAlertClass(t *testing.T, alerts Alerts, class string) {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class {
			t.Fatalf("unexpected %s alert for %s: %s", class, alert.Target, alert.Observed)
		}
	}
}

func TestMinioRunwayRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "minio-runway" {
			continue
		}
		found++
		if signal.Number() != "11.6b" || signal.Cadence() != 15*time.Minute || signal.ID() != "minio/runway" {
			t.Fatal("MinIO runway lost its catalog identity or fifteen-minute cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §11.6b MinIO drive runway reader is not registered")
	}
}

func TestMinioRunwayHealthyStore(t *testing.T) {
	alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), minioRunwayTestHealthy())})
	if len(alerts) != 0 {
		t.Fatalf("a healthy store alerted: %d alerts, first %s %s", len(alerts), alerts[0].Class, alerts[0].Observed)
	}
}

// The 2026-10-22 shape: raw free space falling about 41 GB/h across the four
// drives while MinIO's 47%-used accounting stayed quiet, raw use far above
// twice the logical bytes, and PutObject at 520/s against 90/s.
func TestMinioRunwaySyntheticIncident(t *testing.T) {
	store := minioRunwayTestHealthy()
	store.avail = func(_ int, age time.Duration) float64 {
		return 2050*storageRunwayTestGigabyte + 10.25*storageRunwayTestGigabyte*age.Hours()
	}
	store.logical = func(time.Duration) float64 { return 988 * storageRunwayTestGigabyte }
	store.putRate = func(age time.Duration) float64 {
		if age <= 2*time.Hour {
			return 520
		}
		return 90
	}
	alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), store)})

	runway := requireAlertClass(t, alerts, "minio-drive-runway")
	if runway.Target != "minio-1/drives" || runway.Severity != SeverityWarn || runway.Sustain != 2 || runway.unobservable ||
		!strings.Contains(runway.Observed, "band=warn ") || !strings.Contains(runway.Observed, "census_complete=true") {
		t.Fatalf("wrong drive runway alert: %s %s", runway.Severity, runway.Observed)
	}
	divergence := requireAlertClass(t, alerts, "minio-accounting-divergence")
	if divergence.Target != "minio-1/accounting" || divergence.Severity != SeverityWarn ||
		!strings.Contains(divergence.Observed, "logical_bytes=988000000000") || !strings.Contains(divergence.Observed, "usage_snapshot_age_s=1800") {
		t.Fatalf("wrong divergence alert: %s", divergence.Observed)
	}
	surge := requireAlertClass(t, alerts, "minio-put-surge")
	if surge.Target != "minio-1/puts" || surge.Severity != SeverityWarn ||
		!strings.Contains(surge.Observed, "put_rate_per_second=520.0") || !strings.Contains(surge.Observed, "median_per_second=90.0") {
		t.Fatalf("wrong put surge alert: %s", surge.Observed)
	}
	requireNoAlertClass(t, alerts, "minio-runway-unobservable")
	requireNoAlertClass(t, alerts, "minio-backup-reserve")
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "private-source-canary")
	}
}

// The drive with the least room bounds the store: one drive holding foreign
// data pages while its siblings have weeks.
func TestMinioRunwayWorstDrivePages(t *testing.T) {
	store := minioRunwayTestHealthy()
	store.avail = func(drive int, age time.Duration) float64 {
		if drive == 2 {
			return 400*storageRunwayTestGigabyte + 8*storageRunwayTestGigabyte*age.Hours()
		}
		return 2000*storageRunwayTestGigabyte + storageRunwayTestGigabyte*age.Hours()/10
	}
	store.logical = func(time.Duration) float64 { return 4 * minioRunwayTestTerabyte }
	alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), store)})
	alert := requireAlertClass(t, alerts, "minio-drive-runway")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "worst_drive=/mnt/blob3 ") || !strings.Contains(alert.Observed, "least_free_drive=/mnt/blob3 ") {
		t.Fatalf("worst drive did not page: %s", alert.Observed)
	}
}

// A backup that runs, holds its slot or is due within a day without the room
// its own admission needs above the reserve warns; with room it is quiet.
func TestMinioRunwayBackupReserve(t *testing.T) {
	now := syntheticSettings(nil).Now()
	soon := "@" + strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10)
	later := "@" + strconv.FormatInt(now.Add(72*time.Hour).Unix(), 10)
	tight := minioRunwayTestHealthy()
	tight.avail = func(int, time.Duration) float64 { return 2056 * storageRunwayTestGigabyte }
	room := minioRunwayTestHealthy()
	room.avail = func(int, time.Duration) float64 { return 2700 * storageRunwayTestGigabyte }
	for _, test := range []struct {
		name    string
		store   minioRunwayTestStore
		units   string
		slots   string
		trigger string
	}{
		{name: "running", store: tight, units: minioRunwayTestUnits("loaded", "active", "loaded", "inactive", ""), slots: "1", trigger: "triggers=service-active,replication-slot"},
		{name: "slot only", store: tight, units: minioRunwayTestUnits("loaded", "inactive", "loaded", "inactive", ""), slots: "1", trigger: "triggers=replication-slot "},
		{name: "timer due", store: tight, units: minioRunwayTestUnits("loaded", "inactive", "loaded", "active", soon), slots: "0", trigger: "triggers=timer-due "},
		{name: "timer later", store: tight, units: minioRunwayTestUnits("loaded", "inactive", "loaded", "active", later), slots: "0"},
		{name: "idle", store: tight, units: minioRunwayTestUnits("loaded", "inactive", "loaded", "inactive", "n/a"), slots: "0"},
		{name: "room", store: room, units: minioRunwayTestUnits("loaded", "active", "loaded", "inactive", ""), slots: "1"},
	} {
		test.store.logical = func(time.Duration) float64 { return 3.6 * minioRunwayTestTerabyte }
		alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{
			body: minioRunwayTestBody(t, minioRunwayTestGrid(), test.store), units: test.units,
			slots: []Row{{"slots", "pg-1", test.slots, "0"}},
		})
		if test.trigger == "" {
			requireNoAlertClass(t, alerts, "minio-backup-reserve")
			requireNoAlertClass(t, alerts, "minio-runway-unobservable")
			continue
		}
		alert := requireAlertClass(t, alerts, "minio-backup-reserve")
		if alert.Target != "minio-1/backup" || alert.Severity != SeverityWarn || alert.Sustain != 1 ||
			!strings.Contains(alert.Observed, test.trigger) || !strings.Contains(alert.Observed, "required_at_least_bytes=4000000000000") {
			t.Fatalf("%s: wrong backup alert: %s", test.name, alert.Observed)
		}
	}
}

// A drive MinIO counts but the node metrics omit leaves the census, the
// accounting and the backup layer unknown, while the put layer still judges.
func TestMinioRunwayIncompleteCensusStaysUnknown(t *testing.T) {
	store := minioRunwayTestHealthy()
	store.drives = store.drives[:3]
	store.putRate = func(age time.Duration) float64 {
		if age <= 2*time.Hour {
			return 900
		}
		return 90
	}
	alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{
		body:  minioRunwayTestBody(t, minioRunwayTestGrid(), store),
		units: minioRunwayTestUnits("loaded", "active", "loaded", "inactive", ""), slots: []Row{{"slots", "pg-1", "1", "0"}},
	})
	unknown := map[string]bool{}
	for _, alert := range alerts {
		if alert.Class == "minio-runway-unobservable" {
			if !alert.unobservable || !strings.Contains(alert.Observed, "drive-filesystems=3 minio-drive-count=4") {
				t.Fatalf("census gap lost its reason: %s", alert.Observed)
			}
			unknown[alert.Target] = true
		}
	}
	if !unknown["minio-1/drives"] || !unknown["minio-1/accounting"] || !unknown["minio-1/backup"] || unknown["minio-1/puts"] {
		t.Fatalf("census gap reached the wrong layers: %v", unknown)
	}
	requireAlertClass(t, alerts, "minio-put-surge")
	requireNoAlertClass(t, alerts, "minio-drive-runway")
}

// A stale drive layer after a fast decline pages per emission rule 1.
func TestMinioRunwayUnknownDriveLayerPagesAfterFastDecline(t *testing.T) {
	store := minioRunwayTestHealthy()
	store.avail = func(_ int, age time.Duration) float64 {
		return 3000*storageRunwayTestGigabyte + 6*storageRunwayTestGigabyte*age.Hours()
	}
	store.size = 8000 * storageRunwayTestGigabyte
	store.driveOmit = func(_ int, age time.Duration) bool { return age < 45*time.Minute }
	store.logical = func(time.Duration) float64 { return 9 * minioRunwayTestTerabyte }
	alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), store)})
	for _, alert := range alerts {
		if alert.Class == "minio-runway-unobservable" && alert.Target == "minio-1/drives" {
			if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "reason=stale-drive-series") {
				t.Fatalf("a stale fast-declining drive layer did not page: %s %s", alert.Severity, alert.Observed)
			}
			return
		}
	}
	t.Fatal("stale drive layer was not reported unknown")
}

// Missing MinIO metrics, short put history and invalid answers stay unknown
// without certifying any layer.
func TestMinioRunwayMissingSourcesStayUnknown(t *testing.T) {
	missingLogical := minioRunwayTestHealthy()
	missingLogical.logical = nil
	shortHistory := minioRunwayTestHealthy()
	shortHistory.putRate = nil
	for _, test := range []struct {
		name   string
		body   string
		target string
		reason string
	}{
		{name: "logical", body: minioRunwayTestBody(t, minioRunwayTestGrid(), missingLogical), target: "minio-1/accounting", reason: "reason=missing-logical-bytes"},
		{name: "puts", body: minioRunwayTestBody(t, minioRunwayTestGrid(), shortHistory), target: "minio-1/puts", reason: "reason=missing-put-rate"},
		{name: "answer", body: `{"status":"error","error":"private-source-canary"}`, target: "minio-1/drives", reason: "reason=incomplete-or-invalid-response"},
	} {
		alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: test.body})
		found := false
		for _, alert := range alerts {
			requireAlertOmits(t, alert, "private-source-canary")
			if alert.Class == "minio-runway-unobservable" && alert.Target == test.target {
				found = alert.unobservable && strings.Contains(alert.Observed, test.reason)
			}
		}
		if !found {
			t.Fatalf("%s: missing source did not stay unknown", test.name)
		}
	}
	short := minioRunwayTestHealthy()
	short.putRate = func(age time.Duration) float64 { return 90 }
	body := minioRunwayTestBody(t, minioRunwayTestGrid(), short)
	settings := DefaultMinioRunwaySettings()
	settings.PutSurgeMinimumHistory = 8 * 24 * time.Hour
	alerts := minioRunwayTestRun(t, NewMinioRunwaySignalWithSettings(settings), minioRunwayTestSources{body: body})
	if alert := requireAlertClass(t, alerts, "minio-runway-unobservable"); alert.Target != "minio-1/puts" || !strings.Contains(alert.Observed, "insufficient-put-history") {
		t.Fatalf("short put history was judged: %s", alert.Observed)
	}
}

// A failed or malformed backup unit read, or an unqualified slot read, is
// unknown for the backup layer only.
func TestMinioRunwayBackupSourceFailuresStayUnknown(t *testing.T) {
	body := minioRunwayTestBody(t, minioRunwayTestGrid(), minioRunwayTestHealthy())
	loaded := minioRunwayTestUnits("loaded", "active", "loaded", "inactive", "")
	for _, test := range []struct {
		name    string
		sources minioRunwayTestSources
		reason  string
	}{
		{name: "unit read", sources: minioRunwayTestSources{body: body, unitsErr: errors.New("exit status 1: private-source-canary")}, reason: "backup-unit-unavailable"},
		{name: "unit frame", sources: minioRunwayTestSources{body: body, units: "monitor-unit service\nLoadState=loaded\n"}, reason: "incomplete-unit-frame"},
		{name: "unit extra", sources: minioRunwayTestSources{body: body, units: strings.Replace(loaded, "ActiveState=active", "ActiveState=active\nprivate-source-canary=1", 1)}, reason: "unexpected-unit-line"},
		{name: "unit state", sources: minioRunwayTestSources{body: body, units: minioRunwayTestUnits("loaded", "private-source-canary", "loaded", "inactive", "")}, reason: "unknown-service-state"},
		{name: "elapse", sources: minioRunwayTestSources{body: body, units: minioRunwayTestUnits("loaded", "inactive", "loaded", "active", "Thu 2026-10-22")}, reason: "invalid-timer-elapse"},
		{name: "slot read", sources: minioRunwayTestSources{body: body, units: loaded, slotsErr: errors.New("private-source-canary")}, reason: "replication-slots-unavailable"},
		{name: "slot host", sources: minioRunwayTestSources{body: body, units: loaded, slots: []Row{{"slots", "pg-2", "1", "0"}}}, reason: "replication-slot-frame-invalid"},
		{name: "slot replica", sources: minioRunwayTestSources{body: body, units: loaded, slots: []Row{{"slots", "pg-1", "1", "1"}}}, reason: "replication-slot-frame-invalid"},
	} {
		alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), test.sources)
		if len(alerts) != 1 {
			t.Fatalf("%s: got %d alerts, want the backup layer unknown only", test.name, len(alerts))
		}
		alert := requireAlertClass(t, alerts, "minio-runway-unobservable")
		if alert.Target != "minio-1/backup" || !strings.Contains(alert.Observed, "reason="+test.reason) {
			t.Fatalf("%s: wrong backup unknown: %s", test.name, alert.Observed)
		}
		requireAlertOmits(t, alert, "private-source-canary")
	}
}

// Series of a host outside the minio inventory never certify or alert, and a
// store without a minio host is dark rather than certified.
func TestMinioRunwayInventoryAuthority(t *testing.T) {
	retired := minioRunwayTestHealthy()
	retired.host = "retired-1"
	retired.avail = func(int, time.Duration) float64 { return 0 }
	alerts := minioRunwayTestRun(t, NewMinioRunwaySignal(), minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), minioRunwayTestHealthy(), retired)})
	if len(alerts) != 0 {
		t.Fatal("a retired store's series alerted or displaced the inventory host")
	}
	settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatal("a store without a minio host was read")
		return "", nil
	}})
	if alerts, err := NewMinioRunwaySignal().Run(context.Background(), settings); err != nil || len(alerts) != 0 {
		t.Fatal("an inventory without a minio host produced findings")
	}
	excluded := minioRunwayTestSettings(t, minioRunwayTestSources{body: minioRunwayTestBody(t, minioRunwayTestGrid(), minioRunwayTestHealthy())})
	excluded.ExcludedHosts = []string{minioRunwayTestHost}
	alerts, err := NewMinioRunwaySignal().Run(context.Background(), excluded)
	if err != nil || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" {
		t.Fatal("an excluded MinIO host was read instead of staying scope-unknown")
	}
}

// Ended callers receive lifecycle with original causes from the gateway or
// the backup read; nothing is published.
func TestMinioRunwayCancellationKeepsOriginalCause(t *testing.T) {
	body := minioRunwayTestBody(t, minioRunwayTestGrid(), minioRunwayTestHealthy())
	for _, stage := range []string{"before", "gateway", "units"} {
		ctx, cancel := context.WithCancel(context.Background())
		cause := errors.New("private-source-canary")
		settings := minioRunwayTestSettings(t, minioRunwayTestSources{body: body})
		settings.Source = &syntheticSource{hostFn: func(target HostSettings, command string) (string, error) {
			if stage == "before" {
				t.Fatal("canceled work contacted a source")
			}
			if target.Name == "metrics-1" && stage == "gateway" {
				cancel()
				return body, cause
			}
			if target.Name == minioRunwayTestHost {
				cancel()
				return "", cause
			}
			return body, nil
		}}
		if stage == "before" {
			cancel()
		}
		alerts, err := NewMinioRunwaySignal().Run(ctx, settings)
		cancel()
		if len(alerts) != 0 || !errors.Is(err, context.Canceled) || (stage != "before" && !errors.Is(err, cause)) {
			t.Fatalf("%s: canceled run lost its causes or published findings", stage)
		}
		if strings.Contains(err.Error(), "private-source-canary") {
			t.Fatal("lifecycle error exposed its private source")
		}
	}
}
