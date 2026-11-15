package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	minioTestStore   = "object-store.example.test"
	minioTestGateway = "metrics-gateway.example.test"
	minioTestRetired = "retired-store.example.test"
	minioTestDrive   = 3.5 * (1 << 40)
)

type minioDriveFixture struct {
	path    string
	freePct float64
	health  float64
}

type minioBucketFixture struct {
	name     string
	usage    float64
	quota    float64 // zero: no quota
	quotaMin float64 // zero: the current quota
}

// minioStoreFixture is one host's MinIO v3 series as the monitor's Mimir
// query returns them. Request counts are five-minute increases.
type minioStoreFixture struct {
	host       string
	drives     []minioDriveFixture
	driveCount float64 // zero: len(drives)
	offline    float64
	tolerance  float64
	buckets    []minioBucketFixture
	usageAge   float64
	requests   map[string]float64
	age        time.Duration
	omit       map[string]bool
}

func healthyMinioStore() minioStoreFixture {
	return minioStoreFixture{
		host: minioTestStore,
		drives: []minioDriveFixture{
			{path: "/mnt/blob1/minio", freePct: 61, health: 1},
			{path: "/mnt/blob2/minio", freePct: 61, health: 1},
			{path: "/mnt/blob3/minio", freePct: 60, health: 1},
			{path: "/mnt/blob4/minio", freePct: 61, health: 1},
		},
		tolerance: 1,
		buckets: []minioBucketFixture{
			{name: "blob", usage: 90 * (1 << 30)},
			{name: "competition", usage: 2 * (1 << 30)},
			{name: "loki", usage: 150 * (1 << 30), quota: 400 * (1 << 30)},
			{name: "mimir", usage: 900 * (1 << 30)},
		},
		usageAge: 1800,
		requests: map[string]float64{
			"write_requests": 2400, "write_5xx": 0, "write_4xx": 1,
			"read_requests": 9000, "read_5xx": 2,
		},
	}
}

func (store minioStoreFixture) series(now time.Time) []any {
	result := []any{}
	add := func(check string, labels map[string]string, value float64) {
		if store.omit[check] {
			return
		}
		metric := map[string]string{"env": "synthetic", "job": "minio", "host": store.host, "monitor_check": check}
		for key, labelValue := range labels {
			metric[key] = labelValue
		}
		result = append(result, map[string]any{
			"metric": metric,
			"value":  []any{float64(now.Add(-store.age).Unix()), strconv.FormatFloat(value, 'f', -1, 64)},
		})
	}
	node := map[string]string{"server": "127.0.0.1:23900"}
	for index, drive := range store.drives {
		labels := map[string]string{
			"server": "127.0.0.1:23900", "drive": drive.path,
			"pool_index": "0", "set_index": "0", "drive_index": strconv.Itoa(index),
		}
		free, total := minioTestDrive*drive.freePct/100, minioTestDrive
		if drive.health == 0 {
			free, total = 0, 0
		}
		add("drive_free", labels, free)
		add("drive_total", labels, total)
		add("drive_health", labels, drive.health)
	}
	count := store.driveCount
	if count == 0 {
		count = float64(len(store.drives))
	}
	add("drive_count", node, count)
	add("drive_offline_count", node, store.offline)
	add("set_write_tolerance", map[string]string{"pool_id": "0", "set_id": "0"}, store.tolerance)
	for _, bucket := range store.buckets {
		labels := map[string]string{"bucket": bucket.name}
		add("bucket_usage", labels, bucket.usage)
		if bucket.quota > 0 {
			add("bucket_quota", labels, bucket.quota)
			quotaMin := bucket.quotaMin
			if quotaMin == 0 {
				quotaMin = bucket.quota
			}
			add("bucket_quota_min", labels, quotaMin)
		}
	}
	if len(store.buckets) != 0 {
		add("usage_age", nil, store.usageAge)
	}
	for _, check := range []string{"write_requests", "write_5xx", "write_4xx", "read_requests", "read_5xx"} {
		if value, ok := store.requests[check]; ok {
			add(check, nil, value)
		}
	}
	return result
}

func minioStorageFixtureJSON(t testing.TB, now time.Time, stores ...minioStoreFixture) string {
	t.Helper()
	result := []any{}
	for _, store := range stores {
		result = append(result, store.series(now)...)
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

func minioStorageSettings(t testing.TB, now time.Time, payload string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != minioTestGateway {
			t.Fatalf("MinIO storage contacted %s directly; it must read Mimir through a services gateway", host.Name)
		}
		for _, want := range []string{
			`minio_system_drive_free_bytes{env="synthetic",job="minio"}`,
			`minio_cluster_usage_buckets_quota_total_bytes{env="synthetic",job="minio"}`,
			`minio_api_requests_5xx_errors_total{env="synthetic",job="minio",name=~`,
			"time=" + strconv.FormatInt(now.Unix(), 10),
			"--max-filesize",
		} {
			if !strings.Contains(command, want) {
				t.Fatalf("MinIO storage command omitted %q: %s", want, command)
			}
		}
		return payload, nil
	}}
	return SignalSettings{
		Environment: "synthetic",
		Source:      source,
		Now:         func() time.Time { return now },
		Hosts: []HostSettings{
			{Name: minioTestGateway, Roles: []string{"services"}},
			{Name: minioTestStore, Roles: []string{"minio", "vpn-client"}},
		},
	}
}

func runMinioStorage(t *testing.T, stores ...minioStoreFixture) Alerts {
	t.Helper()
	now := time.Date(2026, 10, 12, 15, 36, 0, 0, time.UTC)
	alerts, err := NewMinioStorageSignal().Run(context.Background(), minioStorageSettings(t, now, minioStorageFixtureJSON(t, now, stores...)))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func requireNoMinioAlert(t *testing.T, alerts Alerts, class string) {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class {
			t.Fatalf("unexpected %s alert: %+v", class, alert)
		}
	}
}

func TestMinioStorageSignalSyntheticHealthy(t *testing.T) {
	// A retired store that still ships metrics is outside the minio
	// inventory and must neither alert nor stand in for the real host.
	retired := healthyMinioStore()
	retired.host = minioTestRetired
	for index := range retired.drives {
		retired.drives[index].freePct = 0.2
	}
	retired.requests = map[string]float64{"write_requests": 500, "write_5xx": 500}

	if alerts := runMinioStorage(t, healthyMinioStore(), retired); len(alerts) != 0 {
		t.Fatalf("healthy MinIO fixture alerted: %+v", alerts)
	}
}

// The 2026-10-11 shape: the drives reached MinIO's free-space floor and
// every Loki, Mimir and blob write returned 507 XMinioStorageFull.
func TestMinioStorageSignalSyntheticStorageFullIncident(t *testing.T) {
	store := healthyMinioStore()
	for index := range store.drives {
		store.drives[index].freePct = 0.5
	}
	store.requests = map[string]float64{
		"write_requests": 1800, "write_5xx": 1800, "write_4xx": 0,
		"read_requests": 7000, "read_5xx": 3,
	}
	alerts := runMinioStorage(t, store)

	capacity := requireAlertClass(t, alerts, "minio-drive-capacity")
	if capacity.Target != minioTestStore || capacity.Severity != SeverityPage || capacity.Sustain != 1 || capacity.Frame != "" {
		t.Fatalf("wrong floor alert identity: %+v", capacity)
	}
	for _, want := range []string{
		"band=floor",
		"507 XMinioStorageFull",
		"set_floor_gib=",
		"/mnt/blob1/minio free_gib=17.9 total_gib=3584.0 free_pct=0.50",
	} {
		if !strings.Contains(capacity.Markdown(), want) {
			t.Fatalf("floor alert omitted %q:\n%s", want, capacity.Markdown())
		}
	}

	writes := requireAlertClass(t, alerts, "minio-write-errors")
	if writes.Severity != SeverityPage || writes.Sustain != 2 || !strings.Contains(writes.Observed, "error_pct=100.00") {
		t.Fatalf("wrong write-error alert: %+v", writes)
	}
	requireNoMinioAlert(t, alerts, "minio-read-errors")
	requireNoMinioAlert(t, alerts, "minio-metrics-unobservable")
}

func TestMinioStorageDriveCapacityBands(t *testing.T) {
	tests := []struct {
		name     string
		freePct  []float64
		band     string
		severity Severity
		sustain  int
		worst    string
	}{
		{name: "healthy", freePct: []float64{40, 40, 40, 40}},
		{name: "reserve", freePct: []float64{12, 12, 13, 12}, band: "low", severity: SeverityWarn, sustain: 2, worst: "/mnt/blob1/minio"},
		{name: "critical", freePct: []float64{4, 4, 4, 4.5}, band: "critical", severity: SeverityPage, sustain: 2, worst: "/mnt/blob1/minio"},
		{name: "one drive holds other data", freePct: []float64{40, 3, 40, 40}, band: "critical", severity: SeverityPage, sustain: 2, worst: "/mnt/blob2/minio"},
		{name: "floor", freePct: []float64{0.9, 0.9, 0.9, 0.9}, band: "floor", severity: SeverityPage, sustain: 1, worst: "/mnt/blob1/minio"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := healthyMinioStore()
			for index, free := range test.freePct {
				store.drives[index].freePct = free
			}
			alerts := runMinioStorage(t, store)
			if test.band == "" {
				requireNoMinioAlert(t, alerts, "minio-drive-capacity")
				return
			}
			alert := requireAlertClass(t, alerts, "minio-drive-capacity")
			if alert.Severity != test.severity || alert.Sustain != test.sustain ||
				!strings.Contains(alert.Observed, "band="+test.band) ||
				!strings.Contains(alert.Observed, "worst_drive="+test.worst+" ") {
				t.Fatalf("wrong capacity alert: %+v", alert)
			}
		})
	}
}

// The 2026-10-12 shape: a 400GiB hard quota applied to a loki bucket that
// held about 673GiB, then raised back to 1TiB.
func TestMinioStorageSignalSyntheticQuotaIncident(t *testing.T) {
	const gibibyte = float64(1 << 30)
	tests := []struct {
		name     string
		loki     minioBucketFixture
		severity Severity
		sustain  int
		symptom  string
		observed []string
	}{
		{
			name:     "quota under usage",
			loki:     minioBucketFixture{name: "loki", usage: 673 * gibibyte, quota: 400 * gibibyte},
			severity: SeverityPage, sustain: 1, symptom: "is at or over its hard quota",
			observed: []string{"usage_bytes=722628247552", "quota_bytes=429496729600", "used_pct=168."},
		},
		{
			name:     "raised back within the window",
			loki:     minioBucketFixture{name: "loki", usage: 673 * gibibyte, quota: 1024 * gibibyte, quotaMin: 400 * gibibyte},
			severity: SeverityPage, sustain: 1, symptom: "within the last five minutes",
			observed: []string{"quota_bytes=1099511627776", "quota_min_5m_bytes=429496729600"},
		},
		{
			name:     "inside the last tenth",
			loki:     minioBucketFixture{name: "loki", usage: 370 * gibibyte, quota: 400 * gibibyte},
			severity: SeverityWarn, sustain: 2, symptom: "holds 92.5% of its hard quota",
			observed: []string{"usage_snapshot_age_s=1800"},
		},
		{name: "retention keeps it under", loki: minioBucketFixture{name: "loki", usage: 150 * gibibyte, quota: 400 * gibibyte}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := healthyMinioStore()
			store.buckets[2] = test.loki
			alerts := runMinioStorage(t, store)
			if test.symptom == "" {
				requireNoMinioAlert(t, alerts, "minio-bucket-quota")
				return
			}
			alert := requireAlertClass(t, alerts, "minio-bucket-quota")
			if alert.Target != minioTestStore+"/loki" || alert.Severity != test.severity || alert.Sustain != test.sustain ||
				!strings.Contains(alert.Symptom, test.symptom) {
				t.Fatalf("wrong quota alert: %+v", alert)
			}
			for _, want := range test.observed {
				if !strings.Contains(alert.Observed, want) {
					t.Fatalf("quota alert omitted %q: %s", want, alert.Observed)
				}
			}
			if !strings.Contains(alert.Mechanism, "400 XMinioAdminBucketQuotaExceeded") {
				t.Fatalf("quota alert does not name the rejection MinIO returns: %s", alert.Mechanism)
			}
		})
	}
}

func TestMinioStorageBucketsWithoutQuotaNeverAlertOnSize(t *testing.T) {
	store := healthyMinioStore()
	store.buckets = []minioBucketFixture{{name: "mimir", usage: 5000 * (1 << 30)}}
	requireNoMinioAlert(t, runMinioStorage(t, store), "minio-bucket-quota")
}

func TestMinioStorageSignalSyntheticDriveOffline(t *testing.T) {
	tests := []struct {
		name      string
		health    []float64
		offline   float64
		tolerance float64
		severity  Severity
		sustain   int
		observed  string
	}{
		{name: "one drive offline", health: []float64{1, 1, 0, 1}, offline: 1, tolerance: 0, severity: SeverityPage, sustain: 2, observed: "offline=1 healing=0 write_tolerance=0 offline_drives=/mnt/blob3/minio"},
		{name: "write quorum lost", health: []float64{1, 0, 0, 1}, offline: 2, tolerance: -1, severity: SeverityPage, sustain: 1, observed: "offline=2 healing=0 write_tolerance=-1 offline_drives=/mnt/blob2/minio,/mnt/blob3/minio"},
		{name: "node count only", health: []float64{1, 1, 1, 1}, offline: 1, tolerance: 0, severity: SeverityPage, sustain: 2, observed: "offline=1 healing=0"},
		{name: "healing", health: []float64{1, 1, 2, 1}, tolerance: 1, severity: SeverityWarn, sustain: 2, observed: "offline=0 healing=1 write_tolerance=1 offline_drives=- healing_drives=/mnt/blob3/minio"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := healthyMinioStore()
			for index, health := range test.health {
				store.drives[index].health = health
			}
			store.offline, store.tolerance = test.offline, test.tolerance
			alerts := runMinioStorage(t, store)
			alert := requireAlertClass(t, alerts, "minio-drive-offline")
			if alert.Target != minioTestStore || alert.Severity != test.severity || alert.Sustain != test.sustain ||
				!strings.Contains(alert.Observed, test.observed) {
				t.Fatalf("wrong offline alert: %+v", alert)
			}
			// an offline drive's zero capacity is not a full drive
			requireNoMinioAlert(t, alerts, "minio-drive-capacity")
		})
	}
}

func TestMinioStorageRequestErrorBands(t *testing.T) {
	tests := []struct {
		name     string
		requests map[string]float64
		class    string
		severity Severity
	}{
		{name: "quota rejections", requests: map[string]float64{"write_requests": 400, "write_4xx": 160, "read_requests": 900}, class: "minio-write-rejected", severity: SeverityWarn},
		{name: "few rejections", requests: map[string]float64{"write_requests": 40, "write_4xx": 12, "read_requests": 900}},
		{name: "slow down", requests: map[string]float64{"write_requests": 2000, "write_5xx": 40, "read_requests": 900}, class: "minio-write-errors", severity: SeverityWarn},
		{name: "single burst", requests: map[string]float64{"write_requests": 30, "write_5xx": 9, "read_requests": 900}},
		{name: "reads failing", requests: map[string]float64{"write_requests": 100, "read_requests": 900, "read_5xx": 300}, class: "minio-read-errors", severity: SeverityPage},
		{name: "reads degraded", requests: map[string]float64{"write_requests": 100, "read_requests": 900, "read_5xx": 60}, class: "minio-read-errors", severity: SeverityWarn},
		{name: "idle after restart", requests: map[string]float64{}},
		// errors are a subset of requests; a missing request partition must
		// not turn a failing write path into an idle one
		{name: "errors without requests", requests: map[string]float64{"write_5xx": 50, "read_requests": 900}, class: "minio-write-errors", severity: SeverityPage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := healthyMinioStore()
			store.requests = test.requests
			alerts := runMinioStorage(t, store)
			for _, class := range []string{"minio-write-errors", "minio-read-errors", "minio-write-rejected"} {
				if class != test.class {
					requireNoMinioAlert(t, alerts, class)
				}
			}
			if test.class == "" {
				return
			}
			alert := requireAlertClass(t, alerts, test.class)
			if alert.Target != minioTestStore || alert.Severity != test.severity || alert.Sustain != 2 {
				t.Fatalf("wrong %s alert: %+v", test.class, alert)
			}
		})
	}
}

func TestMinioStorageSignalSyntheticMissingAndStaleAreNotGreen(t *testing.T) {
	full := healthyMinioStore()
	for index := range full.drives {
		full.drives[index].freePct = 0.5
	}
	tests := []struct {
		name   string
		store  func() minioStoreFixture
		frame  string
		reason string
	}{
		{name: "no series", store: func() minioStoreFixture {
			// only a host outside the minio inventory ships MinIO series
			store := healthyMinioStore()
			store.host = minioTestRetired
			return store
		}, frame: "drives", reason: "reason=no-fresh-drive-series"},
		{name: "stale evaluation", store: func() minioStoreFixture {
			store := full
			store.age = 4 * time.Minute
			return store
		}, frame: "drives", reason: "reason=no-fresh-drive-series"},
		{name: "drive missing from the count", store: func() minioStoreFixture {
			store := full
			store.driveCount = 5
			return store
		}, frame: "drives", reason: "reason=drive-series=4 drive-count=5"},
		{name: "drive count missing", store: func() minioStoreFixture {
			store := full
			store.omit = map[string]bool{"drive_count": true}
			return store
		}, frame: "drives", reason: "reason=drive-count-missing"},
		{name: "health missing", store: func() minioStoreFixture {
			store := full
			store.omit = map[string]bool{"drive_health": true}
			return store
		}, frame: "drives", reason: "reason=drive-health-missing"},
		{name: "no usage snapshot yet", store: func() minioStoreFixture {
			store := healthyMinioStore()
			store.buckets = nil
			return store
		}, frame: "bucket-usage", reason: "reason=no-bucket-usage-series"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alerts := runMinioStorage(t, test.store())
			alert := requireAlertClass(t, alerts, "minio-metrics-unobservable")
			if alert.Target != minioTestStore || alert.Frame != test.frame || alert.Severity != SeverityWarn ||
				alert.Sustain != 2 || !strings.Contains(alert.Observed, test.reason) {
				t.Fatalf("wrong unobservable alert: %+v", alert)
			}
			wantPage := 0
			if test.frame == "drives" {
				wantPage = 3
				// stale or partial drive figures must not become a verdict
				requireNoMinioAlert(t, alerts, "minio-drive-capacity")
				requireNoMinioAlert(t, alerts, "minio-bucket-quota")
				requireNoMinioAlert(t, alerts, "minio-write-errors")
			}
			if alert.PageSustain != wantPage {
				t.Fatalf("unobservable %s page sustain = %d, want %d", test.frame, alert.PageSustain, wantPage)
			}
		})
	}
}

func TestMinioStorageSignalWithoutMinioRoleIsDark(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		t.Fatalf("MinIO storage ran a command on %s without a minio host", host.Name)
		return "", nil
	}}
	settings := SignalSettings{
		Environment: "synthetic",
		Source:      source,
		Hosts:       []HostSettings{{Name: minioTestGateway, Roles: []string{"services"}}},
	}
	alerts, err := NewMinioStorageSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("Run without a minio host = %+v, %v; want no alerts and no error", alerts, err)
	}
}

func TestMinioStorageSignalMimirFailuresAreObservationErrors(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 36, 0, 0, time.UTC)
	unexpected := fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"host":%q,"monitor_check":"drive_freee"},"value":[%d,"1"]}]}}`,
		minioTestStore, now.Unix())
	for name, payload := range map[string]string{
		"mimir error":           `{"status":"error","error":"synthetic query failure","data":{"resultType":"","result":[]}}`,
		"matrix result":         `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		"unexpected partition":  unexpected,
		"truncated response":    `{"status":"success","data":{"resultType":"vector","result":[`,
		"unlabelled partition":  `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"monitor_check":"bogus"},"value":[0,"1"]}]}}`,
		"non-numeric timestamp": fmt.Sprintf(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"host":%q,"monitor_check":"drive_count"},"value":["x","1"]}]}}`, minioTestStore),
	} {
		t.Run(name, func(t *testing.T) {
			alerts, err := NewMinioStorageSignal().Run(context.Background(), minioStorageSettings(t, now, payload))
			if err == nil {
				t.Fatalf("malformed Mimir response became an observation: %+v", alerts)
			}
		})
	}
}

func TestMinioStorageQueryRequiresFreshSourcesAndWriteAPIs(t *testing.T) {
	query := minioStorageQuery("synthetic")
	if got := strings.Count(query, "timestamp("); got != 15 {
		t.Fatalf("query has %d source-freshness bounds, want one per partition (15): %s", got, query)
	}
	if got := strings.Count(query, ">= time() - 180)"); got != 15 {
		t.Fatalf("query freshness bound is not 180s on every partition: %s", query)
	}
	writes := `"CompleteMultipartUpload|CopyObject|CopyObjectPart|NewMultipartUpload|PostPolicyBucket|PutObject|PutObjectExtract|PutObjectPart"`
	for _, want := range []string{
		`min_over_time(minio_cluster_usage_buckets_quota_total_bytes{env="synthetic",job="minio"}[5m])`,
		`increase(minio_api_requests_5xx_errors_total{env="synthetic",job="minio",name=~` + writes + `}[5m])`,
		`increase(minio_api_requests_4xx_errors_total{env="synthetic",job="minio",name=~` + writes + `}[5m])`,
		`increase(minio_api_requests_total{env="synthetic",job="minio",name!~` + writes + `}[5m])`,
		"sum by (host) (",
		`minio_cluster_usage_objects_since_last_update_seconds{env="synthetic",job="minio"}`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query omitted %q: %s", want, query)
		}
	}
	// This MinIO release writes the buckets-path age gauge in nanoseconds.
	if strings.Contains(query, "minio_cluster_usage_buckets_since_last_update_seconds") {
		t.Fatalf("query reads the nanosecond-valued buckets age gauge: %s", query)
	}
}
