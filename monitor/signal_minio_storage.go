package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	minioStorageProbeID = "minio/object-store"

	// Fluent Bit scrapes MinIO's v3 endpoint every 15 seconds
	// (xops files/fluent-bit/minio.conf). Three minutes tolerates a few
	// failed scrapes without accepting a stopped source as current.
	minioStorageFreshness = 3 * time.Minute
	// Request counters and the quota minimum share one window, so a quota
	// that sat under usage between two one-minute observations still trips.
	minioStorageWindow        = 5 * time.Minute
	minioStorageResponseLimit = 256 * 1024

	// MinIO RELEASE.2025-10-15T17-29-55Z (cmd/globals.go, hasSpaceFor in
	// cmd/object-api-utils.go) refuses a new object with 507
	// XMinioStorageFull once the erasure set's free space after the write
	// would be at most 1% of its total. An upload of unknown size is assumed
	// to need 1 GiB, as is any object of 512 MiB or more (sizes are doubled
	// for erasure coding).
	minioDiskFillFraction  = 0.99
	minioUnknownWriteBytes = float64(1 << 30)
	// MinIO's own diskReserveFraction is the warning band; the page band
	// leaves days, not hours, between it and the 1% floor on 3.5 TiB drives.
	minioDriveWarnFreeRatio = 0.15
	minioDrivePageFreeRatio = 0.05

	// xops minio-set-quota refuses to set a quota without a tenth of it free.
	minioQuotaWarnRatio = 0.90

	minioWriteErrorMinimum    = 10
	minioWriteErrorWarnRatio  = 0.01
	minioWriteErrorPageRatio  = 0.05
	minioReadErrorMinimum     = 10
	minioReadErrorWarnRatio   = 0.05
	minioReadErrorPageRatio   = 0.25
	minioWriteRejectMinimum   = 20
	minioWriteRejectWarnRatio = 0.10
)

// minioWriteAPIs are the S3 handler names (MinIO cmd/api-router.go) that
// store new data, so they are the requests a full drive set answers with 507
// XMinioStorageFull and a full bucket quota with 400
// XMinioAdminBucketQuotaExceeded.
var minioWriteAPIs = []string{
	"CompleteMultipartUpload", "CopyObject", "CopyObjectPart", "NewMultipartUpload",
	"PostPolicyBucket", "PutObject", "PutObjectExtract", "PutObjectPart",
}

// Signal minio-storage implements SIGNALS.md §11.6a. It reads MinIO's own v3
// gauges and counters, which Fluent Bit ships to Mimir with the minio host's
// inventory name, so the monitor never holds the root-only metrics bearer
// token or mc credentials. One query covers drive free space against MinIO's
// write floor, offline and healing drives, bucket usage against hard quotas,
// and S3 error rates for every enabled host with the `minio` role.
func NewMinioStorageSignal() Signal {
	return &signalAdapter{
		number: "11.6a", key: "minio-storage", name: "MinIO capacity, quota and write path",
		probe: minioStorageProbe{},
	}
}

type minioStorageProbe struct{}

func (minioStorageProbe) id() string             { return minioStorageProbeID }
func (minioStorageProbe) tier() string           { return tierPage }
func (minioStorageProbe) cadence() time.Duration { return time.Minute }

const (
	minioDriveFree uint8 = 1 << iota
	minioDriveTotal
	minioDriveHealth
)

type minioStorageDrive struct {
	path   string
	free   float64
	total  float64
	health float64
	mask   uint8
}

func (d *minioStorageDrive) online() bool {
	return d.mask&minioDriveHealth != 0 && d.health != 0
}

type minioStorageBucket struct {
	name        string
	usage       float64
	quota       float64
	quotaMin    float64
	hasUsage    bool
	hasQuota    bool
	hasQuotaMin bool
}

// minioStorageHost is one MinIO host's reduced observation. Request
// partitions hold five-minute increases summed over the partition's APIs.
type minioStorageHost struct {
	drives            map[string]*minioStorageDrive
	driveCount        float64
	hasDriveCount     bool
	offlineCount      float64
	hasOfflineCount   bool
	writeTolerance    float64
	hasWriteTolerance bool
	buckets           map[string]*minioStorageBucket
	usageAge          float64
	hasUsageAge       bool
	requests          map[string]float64
	driveInvalid      string
	bucketInvalid     string
}

func newMinioStorageHost() *minioStorageHost {
	return &minioStorageHost{
		drives:   map[string]*minioStorageDrive{},
		buckets:  map[string]*minioStorageBucket{},
		requests: map[string]float64{},
	}
}

func (h *minioStorageHost) sortedDrives() []*minioStorageDrive {
	drives := make([]*minioStorageDrive, 0, len(h.drives))
	for _, drive := range h.drives {
		drives = append(drives, drive)
	}
	sort.Slice(drives, func(i, j int) bool { return drives[i].path < drives[j].path })
	return drives
}

// driveLayerGap reports why the drive layer cannot be evaluated, or "" when
// every drive MinIO counts has a health gauge and every online drive has a
// coherent free/total pair.
func (h *minioStorageHost) driveLayerGap() string {
	if h.driveInvalid != "" {
		return h.driveInvalid
	}
	if !h.hasDriveCount && len(h.drives) == 0 {
		return "no-fresh-drive-series"
	}
	if !h.hasDriveCount || h.driveCount < 1 {
		return "drive-count-missing"
	}
	if float64(len(h.drives)) != h.driveCount {
		return fmt.Sprintf("drive-series=%d drive-count=%.0f", len(h.drives), h.driveCount)
	}
	for _, drive := range h.sortedDrives() {
		if drive.mask&minioDriveHealth == 0 {
			return "drive-health-missing"
		}
		if !drive.online() {
			continue
		}
		if drive.mask&(minioDriveFree|minioDriveTotal) != minioDriveFree|minioDriveTotal {
			return "drive-capacity-missing"
		}
		if drive.total <= 0 || drive.free > drive.total {
			return "drive-capacity-incoherent"
		}
	}
	return ""
}

func minioStorageQuery(environment string) string {
	env := strconv.Quote(environment)
	freshness := strconv.FormatInt(int64(minioStorageFreshness/time.Second), 10)
	window := "[" + strconv.FormatInt(int64(minioStorageWindow/time.Minute), 10) + "m]"
	writes := strconv.Quote(strings.Join(minioWriteAPIs, "|"))
	selector := func(metric, extra string) string {
		labels := "env=" + env + `,job="minio"`
		if extra != "" {
			labels += "," + extra
		}
		return metric + "{" + labels + "}"
	}
	// An instant-query evaluation timestamp is not proof of source
	// freshness; the raw MinIO sample must itself be recent.
	recent := func(source string) string {
		return "(timestamp(" + source + ") >= time() - " + freshness + ")"
	}
	tag := func(expression, check string) string {
		return "label_replace(" + expression + `,"monitor_check",` + strconv.Quote(check) + `,"__name__",".*")`
	}
	gauge := func(metric, check string) string {
		source := selector(metric, "")
		return tag("("+source+" and "+recent(source)+")", check)
	}
	increase := func(metric, names, check string) string {
		source := selector(metric, names)
		return tag("sum by (host) (increase("+source+window+") and "+recent(source)+")", check)
	}
	quota := selector("minio_cluster_usage_buckets_quota_total_bytes", "")
	return strings.Join([]string{
		gauge("minio_system_drive_free_bytes", "drive_free"),
		gauge("minio_system_drive_total_bytes", "drive_total"),
		gauge("minio_system_drive_health", "drive_health"),
		gauge("minio_system_drive_count", "drive_count"),
		gauge("minio_system_drive_offline_count", "drive_offline_count"),
		gauge("minio_cluster_erasure_set_write_tolerance", "set_write_tolerance"),
		gauge("minio_cluster_usage_buckets_total_bytes", "bucket_usage"),
		gauge("minio_cluster_usage_buckets_quota_total_bytes", "bucket_quota"),
		tag("(min_over_time("+quota+window+") and "+recent(quota)+")", "bucket_quota_min"),
		// The objects path reports seconds; this MinIO release writes the
		// buckets-path gauge of the same name in nanoseconds.
		gauge("minio_cluster_usage_objects_since_last_update_seconds", "usage_age"),
		increase("minio_api_requests_total", "name=~"+writes, "write_requests"),
		increase("minio_api_requests_5xx_errors_total", "name=~"+writes, "write_5xx"),
		increase("minio_api_requests_4xx_errors_total", "name=~"+writes, "write_4xx"),
		increase("minio_api_requests_total", "name!~"+writes, "read_requests"),
		increase("minio_api_requests_5xx_errors_total", "name!~"+writes, "read_5xx"),
	}, " or ")
}

func (minioStorageProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	minioHosts := env.cfg.hostsWithRole("minio")
	if len(minioHosts) == 0 {
		return nil, nil
	}
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("minio storage: no services host in inventory for the loopback Mimir query")
	}
	now := env.now().UTC()
	command := "curl -fsS --max-time 15 --max-filesize " + strconv.Itoa(minioStorageResponseLimit) +
		" --data-urlencode " + shellSingleQuote("query="+minioStorageQuery(env.cfg.env)) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(now.Unix(), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	out, gateway, err := shellFirstServiceGateway(ctx, env.runner, metricHosts, nil, command)
	if err != nil {
		return nil, fmt.Errorf("minio storage: query Mimir through service gateways: %w", err)
	}
	names := make([]string, 0, len(minioHosts))
	for _, h := range minioHosts {
		names = append(names, h.name)
	}
	observations, err := parseMinioStorage(out, now, names)
	if err != nil {
		return nil, err
	}

	findings := []finding{}
	for _, h := range minioHosts {
		findings = append(findings, evaluateMinioStorageHost(h.name, observations[h.name], gateway.name)...)
	}
	return findings, nil
}

// parseMinioStorage reduces the bounded Mimir response to one observation per
// configured MinIO host. Series from hosts outside the inventory, such as a
// retired store that still ships metrics, are discarded.
func parseMinioStorage(output string, now time.Time, hosts []string) (map[string]*minioStorageHost, error) {
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("minio storage: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("minio storage: Mimir status=%q result_type=%q error=%q",
			response.Status, response.Data.ResultType, response.Error)
	}
	observations := make(map[string]*minioStorageHost, len(hosts))
	for _, name := range hosts {
		observations[name] = newMinioStorageHost()
	}
	for _, series := range response.Data.Result {
		check := series.Metric["monitor_check"]
		observation := observations[series.Metric["host"]]
		if observation == nil {
			if !minioStorageKnownCheck(check) {
				return nil, fmt.Errorf("minio storage: unexpected Mimir result partition %q", check)
			}
			continue
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("minio storage: parse %s sample: %w", check, err)
		}
		age := now.Sub(observedAt)
		if age > minioStorageFreshness || age < -30*time.Second {
			continue
		}
		// Erasure-set write tolerance is negative once write quorum is lost;
		// every other partition is a byte size, count, health or increase.
		if math.IsNaN(value) || math.IsInf(value, 0) || (value < 0 && check != "set_write_tolerance") {
			if strings.HasPrefix(check, "bucket_") || check == "usage_age" {
				observation.bucketInvalid = "invalid-" + check
			} else {
				observation.driveInvalid = "invalid-" + check
			}
			continue
		}
		switch check {
		case "drive_free", "drive_total", "drive_health":
			path := series.Metric["drive"]
			if path == "" {
				observation.driveInvalid = "unlabelled-drive-series"
				continue
			}
			drive := observation.drives[path]
			if drive == nil {
				drive = &minioStorageDrive{path: path}
				observation.drives[path] = drive
			}
			bit, slot := minioDriveFree, &drive.free
			switch check {
			case "drive_total":
				bit, slot = minioDriveTotal, &drive.total
			case "drive_health":
				bit, slot = minioDriveHealth, &drive.health
				if value != 0 && value != 1 && value != 2 {
					observation.driveInvalid = "invalid-drive_health"
					continue
				}
			}
			if drive.mask&bit != 0 {
				observation.driveInvalid = "duplicate-" + check
				continue
			}
			drive.mask |= bit
			*slot = value
		case "drive_count":
			if observation.hasDriveCount {
				observation.driveInvalid = "duplicate-drive_count"
				continue
			}
			observation.driveCount, observation.hasDriveCount = value, true
		case "drive_offline_count":
			if observation.hasOfflineCount {
				observation.driveInvalid = "duplicate-drive_offline_count"
				continue
			}
			observation.offlineCount, observation.hasOfflineCount = value, true
		case "set_write_tolerance":
			// one series per erasure set; the weakest set bounds the store
			if !observation.hasWriteTolerance || value < observation.writeTolerance {
				observation.writeTolerance = value
			}
			observation.hasWriteTolerance = true
		case "bucket_usage", "bucket_quota", "bucket_quota_min":
			name := series.Metric["bucket"]
			if name == "" {
				observation.bucketInvalid = "unlabelled-bucket-series"
				continue
			}
			bucket := observation.buckets[name]
			if bucket == nil {
				bucket = &minioStorageBucket{name: name}
				observation.buckets[name] = bucket
			}
			present, slot := &bucket.hasUsage, &bucket.usage
			switch check {
			case "bucket_quota":
				present, slot = &bucket.hasQuota, &bucket.quota
			case "bucket_quota_min":
				present, slot = &bucket.hasQuotaMin, &bucket.quotaMin
			}
			if *present {
				observation.bucketInvalid = "duplicate-" + check
				continue
			}
			*present, *slot = true, value
		case "usage_age":
			if observation.hasUsageAge {
				observation.bucketInvalid = "duplicate-usage_age"
				continue
			}
			observation.usageAge, observation.hasUsageAge = value, true
		case "write_requests", "write_5xx", "write_4xx", "read_requests", "read_5xx":
			if _, duplicate := observation.requests[check]; duplicate {
				observation.driveInvalid = "duplicate-" + check
				continue
			}
			observation.requests[check] = value
		default:
			return nil, fmt.Errorf("minio storage: unexpected Mimir result partition %q", check)
		}
	}
	return observations, nil
}

func minioStorageKnownCheck(check string) bool {
	switch check {
	case "drive_free", "drive_total", "drive_health", "drive_count", "drive_offline_count",
		"set_write_tolerance", "bucket_usage", "bucket_quota", "bucket_quota_min", "usage_age",
		"write_requests", "write_5xx", "write_4xx", "read_requests", "read_5xx":
		return true
	}
	return false
}

func evaluateMinioStorageHost(target string, observation *minioStorageHost, gateway string) []finding {
	if gap := observation.driveLayerGap(); gap != "" {
		// Without a complete drive layer the MinIO source itself is in
		// doubt; partial bucket or request figures cannot clear the host.
		return []finding{minioStorageUnobservable(target, "drives", gap, gateway)}
	}
	findings := []finding{
		evaluateMinioDriveCapacity(target, observation, gateway),
		evaluateMinioDriveOffline(target, observation, gateway),
	}
	findings = append(findings, evaluateMinioBucketQuotas(target, observation, gateway)...)
	findings = append(findings, evaluateMinioRequestErrors(target, observation, gateway)...)
	return findings
}

func minioStorageUnobservable(target, frame, reason, gateway string) finding {
	f := finding{
		probeId: minioStorageProbeID, tier: tierWarn,
		class: "minio-metrics-unobservable", target: target, frame: frame, sustain: 2,
		observed: fmt.Sprintf("layer=%s reason=%s freshness_limit_s=%d", frame, reason, int(minioStorageFreshness/time.Second)),
		evidence: "Bounded loopback Mimir query through gateway " + gateway + "; only series labelled with this inventory host were joined.",
		playbook: "SIGNALS.md §11.6a and §11.14",
	}
	if frame == "drives" {
		f.pageSustain = 3
		f.symptom = "MinIO on " + target + " has no complete fresh drive metrics, so its capacity, drive health, quota and error state are unknown"
		f.mechanism = "Fluent Bit scrapes MinIO's authenticated v3 endpoint every 15 seconds and remote-writes it to Mimir. A stopped MinIO, a stopped or misconfigured Fluent Bit (§11.14), a rejected bearer token, or a Mimir ingestion failure leaves the drive gauges missing or stale. A missing series is unknown state, never a healthy store."
		f.baseline = "Every enabled minio host exports fresh free, total and health gauges for each drive in minio_system_drive_count."
		f.action = "On the host, check `systemctl is-active minio fluent-bit`, `curl -fsS --max-time 5 http://127.0.0.1:23900/minio/health/live`, and Fluent Bit's journal for MinIO scrape errors (401/403 means the bearer token no longer matches; re-run xops run-minio-clusters.sh rather than editing the token by hand). If MinIO itself is down, that is the incident: Loki, Mimir and blob writes are failing."
		f.verify = "Two consecutive runs return the complete fresh drive family for this host."
		return f
	}
	f.symptom = "MinIO on " + target + " publishes no bucket usage snapshot, so bucket quota headroom is unknown"
	f.mechanism = "MinIO exports per-bucket usage and hard quotas only once its data scanner has saved a usage snapshot; the quota gauge never appears without it. Until then neither the monitor nor MinIO's own quota enforcement has a usage figure."
	f.baseline = "A running store loads its last saved usage snapshot at startup and exports minio_cluster_usage_buckets_total_bytes for every scanned bucket."
	f.action = "As root on the host, check the scanner with `mc admin scanner status` and the MinIO journal for usage-save errors. Do not set or lower a bucket quota while no usage figure exists (xops minio-set-quota refuses for a non-empty bucket)."
	f.verify = "Two consecutive runs return fresh bucket usage series for this host."
	return f
}

func evaluateMinioDriveCapacity(target string, observation *minioStorageHost, gateway string) finding {
	online := []*minioStorageDrive{}
	for _, drive := range observation.sortedDrives() {
		if drive.online() {
			online = append(online, drive)
		}
	}
	if len(online) == 0 {
		// minio-drive-offline owns a store with no online drive
		return healthyFinding(minioStorageProbeID, tierPage, "minio-drive-capacity", target)
	}
	var setFree, setTotal float64
	worst := online[0]
	parts := make([]string, 0, len(online))
	for _, drive := range online {
		setFree += drive.free
		setTotal += drive.total
		if drive.free/drive.total < worst.free/worst.total {
			worst = drive
		}
		parts = append(parts, fmt.Sprintf("%s free_gib=%.1f total_gib=%.1f free_pct=%.2f",
			drive.path, gib(drive.free), gib(drive.total), 100*drive.free/drive.total))
	}
	setFloor := setTotal * (1 - minioDiskFillFraction)
	worstRatio := worst.free / worst.total

	band, tier, sustain := "", tierWarn, 2
	switch {
	case setFree-minioUnknownWriteBytes <= setFloor || worst.free <= minioUnknownWriteBytes/float64(len(online)):
		band, tier, sustain = "floor", tierPage, 1
	case worstRatio < minioDrivePageFreeRatio:
		band, tier = "critical", tierPage
	case worstRatio < minioDriveWarnFreeRatio:
		band = "low"
	}
	if band == "" {
		return healthyFinding(minioStorageProbeID, tierPage, "minio-drive-capacity", target)
	}

	symptom := fmt.Sprintf("MinIO drive %s on %s has %.2f%% free (band=%s)", worst.path, target, 100*worstRatio, band)
	if band == "floor" {
		symptom = "MinIO on " + target + " is at its free-space floor and refuses new objects with 507 XMinioStorageFull"
	}
	return finding{
		probeId: minioStorageProbeID, tier: tier,
		class: "minio-drive-capacity", target: target, sustain: sustain,
		symptom:   symptom,
		mechanism: "MinIO refuses a new object with 507 XMinioStorageFull once the erasure set's free space after the write would fall to 1% of the set's total (diskFillFraction 0.99), or one drive cannot hold its share. An upload of unknown size, and any object of 512 MiB or more, needs 1 GiB above that floor. Every Loki chunk flush, Mimir block upload and blob write then fails, and Mimir's bucket index goes stale behind them (§11.18). On 2026-10-11 edge-6's shared /mnt/data reached that floor.",
		baseline:  fmt.Sprintf("Every online drive keeps at least %.0f%% free (MinIO's diskReserveFraction); PAGE below %.0f%%; band=floor when the set's free space minus 1 GiB is at or under 1%% of the set's total.", 100*minioDriveWarnFreeRatio, 100*minioDrivePageFreeRatio),
		observed: fmt.Sprintf("band=%s worst_drive=%s worst_free_pct=%.2f set_free_gib=%.1f set_total_gib=%.1f set_floor_gib=%.1f gib_above_floor=%.1f online_drives=%d drives=[%s]",
			band, worst.path, 100*worstRatio, gib(setFree), gib(setTotal), gib(setFloor), gib(setFree-setFloor), len(online), strings.Join(parts, "; ")),
		evidence: "Fresh MinIO v3 minio_system_drive_free_bytes/total_bytes (MinIO's own statfs view, the input to its floor) through the loopback Mimir gateway " + gateway + ".",
		context:  "The drive gauges include anything else stored on the filesystem: a drive that fills while bucket usage stays flat holds non-MinIO data, as edge-6's shared /mnt/data did. Correlate minio-write-errors (507 is a 5xx) and minio-bucket-quota.",
		action:   "Find what is growing: compare minio_cluster_usage_buckets_total_bytes per bucket with the drive totals, and look for non-MinIO data on the drive. Shorten or repair the owning retention (Loki and Mimir compactor retention, blob TTL rules), or add capacity. Do not delete files under a drive's minio directory by hand and do not lower parity.",
		verify:   "Two consecutive runs show every online drive above 15% free and minio-write-errors quiet; after a floor episode, confirm Mimir's bucket index is fresh again (mimir-index).",
		playbook: "SIGNALS.md §11.6a",
	}
}

func evaluateMinioDriveOffline(target string, observation *minioStorageHost, gateway string) finding {
	offline, healing := []string{}, []string{}
	for _, drive := range observation.sortedDrives() {
		switch drive.health {
		case 0:
			offline = append(offline, drive.path)
		case 2:
			healing = append(healing, drive.path)
		}
	}
	offlineCount := float64(len(offline))
	if observation.hasOfflineCount && observation.offlineCount > offlineCount {
		offlineCount = observation.offlineCount
	}
	if offlineCount == 0 && len(healing) == 0 {
		return healthyFinding(minioStorageProbeID, tierPage, "minio-drive-offline", target)
	}

	tolerance := "unknown"
	if observation.hasWriteTolerance {
		tolerance = strconv.FormatFloat(observation.writeTolerance, 'f', 0, 64)
	}
	list := func(paths []string) string {
		if len(paths) == 0 {
			return "-"
		}
		return strings.Join(paths, ",")
	}
	f := finding{
		probeId: minioStorageProbeID, tier: tierPage,
		class: "minio-drive-offline", target: target, sustain: 2,
		mechanism: "MinIO keeps one erasure set over the host's drives; at EC:2 on four drives a write needs three of them and a read two. One offline drive leaves no write tolerance, a second stops every write, a third every read. A drive that failed to mount at boot (nofail) stays offline because MinIO cannot write the bare mount point. A healing drive is online but still rebuilding its shards.",
		baseline:  "minio_system_drive_health is 1 for every drive in minio_system_drive_count, and the erasure set's write tolerance is at least 1.",
		observed: fmt.Sprintf("drives=%.0f offline=%.0f healing=%d write_tolerance=%s offline_drives=%s healing_drives=%s",
			observation.driveCount, offlineCount, len(healing), tolerance, list(offline), list(healing)),
		evidence: "Fresh MinIO v3 per-drive health, node offline count and erasure-set write tolerance through the loopback Mimir gateway " + gateway + ".",
		action:   "On the host, check the drive's mount (`findmnt /mnt/blobN`), the kernel log for I/O errors, and `mc admin info` as root. Remount or replace the drive as MINIO-MIGRATION.md and playbook-minio-clusters.yml describe, then let MinIO heal it. Do not reformat a drive that still holds data and do not restart MinIO repeatedly.",
		verify:   "Two consecutive runs show every drive with health=1, no healing drive, and write tolerance of at least 1.",
		playbook: "SIGNALS.md §11.6a",
	}
	switch {
	case offlineCount > 0:
		f.symptom = fmt.Sprintf("%.0f of %.0f MinIO drives on %s are offline", offlineCount, observation.driveCount, target)
		if observation.hasWriteTolerance && observation.writeTolerance < 0 {
			f.symptom += " and the erasure set has lost write quorum"
			f.sustain = 1
		}
	default:
		f.tier = tierWarn
		f.symptom = fmt.Sprintf("%d MinIO drive(s) on %s are healing", len(healing), target)
	}
	return f
}

func evaluateMinioBucketQuotas(target string, observation *minioStorageHost, gateway string) []finding {
	if observation.bucketInvalid != "" {
		return []finding{minioStorageUnobservable(target, "bucket-usage", observation.bucketInvalid, gateway)}
	}
	if len(observation.buckets) == 0 {
		return []finding{minioStorageUnobservable(target, "bucket-usage", "no-bucket-usage-series", gateway)}
	}
	names := make([]string, 0, len(observation.buckets))
	for name := range observation.buckets {
		names = append(names, name)
	}
	sort.Strings(names)

	age := "unknown"
	if observation.hasUsageAge {
		age = strconv.FormatFloat(observation.usageAge, 'f', 0, 64)
	}
	findings := []finding{}
	torn := false
	for _, name := range names {
		bucket := observation.buckets[name]
		bucketTarget := target + "/" + name
		if !bucket.hasQuota && !bucket.hasQuotaMin {
			continue
		}
		if !bucket.hasUsage {
			// MinIO sets both gauges together; a quota alone is a torn
			// observation, not an empty bucket.
			torn = true
			continue
		}
		quotaNow, quotaMin := bucket.quota, bucket.quotaMin
		if !bucket.hasQuota {
			quotaNow = quotaMin
		}
		if !bucket.hasQuotaMin || quotaNow < quotaMin {
			quotaMin = quotaNow
		}
		if quotaNow <= 0 || quotaMin <= 0 {
			continue
		}
		observed := fmt.Sprintf("bucket=%s usage_bytes=%.0f quota_bytes=%.0f quota_min_5m_bytes=%.0f usage_gib=%.1f quota_gib=%.1f used_pct=%.1f usage_snapshot_age_s=%s",
			name, bucket.usage, quotaNow, quotaMin, gib(bucket.usage), gib(quotaNow), 100*bucket.usage/quotaNow, age)
		f := finding{
			probeId: minioStorageProbeID, class: "minio-bucket-quota", target: bucketTarget,
			observed:  observed,
			mechanism: "MinIO enforces a hard bucket quota against the data scanner's last usage figure: once usage plus the object's size reaches the quota, every PUT to the bucket fails with 400 XMinioAdminBucketQuotaExceeded, which raises no 5xx. On 2026-10-12 a 400GiB quota applied to the loki bucket while it held about 673GiB stopped Loki's chunk flushes until the quota was raised back to 1TiB.",
			baseline:  fmt.Sprintf("Every bucket with a hard quota keeps its usage under %.0f%% of it, the headroom xops minio-set-quota requires before it sets or lowers one.", 100*minioQuotaWarnRatio),
			evidence:  "Fresh MinIO v3 minio_cluster_usage_buckets_total_bytes and minio_cluster_usage_buckets_quota_total_bytes, plus the quota's five-minute minimum, through the loopback Mimir gateway " + gateway + ".",
			context:   "Usage is the scanner's snapshot (usage_snapshot_age_s), the same figure MinIO enforces against, so it can lag the bucket's real size. A quota of zero or a cleared quota exports no series.",
			action:    "Raise the quota back above usage with at least 10% headroom through xops minio-set-quota (it never sets a quota under the bucket's usage), or let the owner's retention bring usage down before lowering it. For loki, confirm the compactor's retention is deleting chunks. Never apply a quota at or below current usage.",
			verify:    "Two consecutive runs with usage under 90% of the quota, a five-minute quota minimum above usage, and minio-write-rejected quiet.",
			playbook:  "SIGNALS.md §11.6a",
		}
		switch {
		case bucket.usage >= quotaNow:
			f.tier, f.sustain = tierPage, 1
			f.symptom = "Bucket " + name + " on " + target + " is at or over its hard quota; MinIO rejects every write to it"
		case bucket.usage >= quotaMin:
			// The quota has been raised since, but a lower one was in force
			// within the window and rejected writes while it was.
			f.tier, f.sustain = tierPage, 1
			f.symptom = "Bucket " + name + " on " + target + " had a hard quota at or under its usage within the last five minutes; MinIO rejected writes to it"
		case bucket.usage >= minioQuotaWarnRatio*quotaNow:
			f.tier, f.sustain = tierWarn, 2
			f.symptom = fmt.Sprintf("Bucket %s on %s holds %.1f%% of its hard quota", name, target, 100*bucket.usage/quotaNow)
		default:
			findings = append(findings, healthyFinding(minioStorageProbeID, tierPage, "minio-bucket-quota", bucketTarget))
			continue
		}
		findings = append(findings, f)
	}
	if torn {
		findings = append(findings, minioStorageUnobservable(target, "bucket-usage", "quota-without-usage", gateway))
	}
	return findings
}

func evaluateMinioRequestErrors(target string, observation *minioStorageHost, gateway string) []finding {
	window := int(minioStorageWindow / time.Minute)
	evidence := "Fresh MinIO v3 minio_api_requests_total and its 4xx/5xx error counters, increased over " +
		strconv.Itoa(window) + " minutes and summed over the partition's S3 APIs, through the loopback Mimir gateway " + gateway + ". Write APIs: " +
		strings.Join(minioWriteAPIs, ", ") + "."
	type partition struct {
		class, errors, requests string
		minimum                 float64
		warnRatio, pageRatio    float64
	}
	partitions := []partition{
		{"minio-write-errors", "write_5xx", "write_requests", minioWriteErrorMinimum, minioWriteErrorWarnRatio, minioWriteErrorPageRatio},
		{"minio-read-errors", "read_5xx", "read_requests", minioReadErrorMinimum, minioReadErrorWarnRatio, minioReadErrorPageRatio},
		{"minio-write-rejected", "write_4xx", "write_requests", minioWriteRejectMinimum, minioWriteRejectWarnRatio, 0},
	}
	findings := make([]finding, 0, len(partitions))
	for _, p := range partitions {
		errors, requests := observation.requests[p.errors], observation.requests[p.requests]
		if requests < errors {
			// errors are a subset of requests; extrapolation can only
			// make the two increases disagree slightly
			requests = errors
		}
		if errors < p.minimum || requests <= 0 || errors/requests < p.warnRatio {
			findings = append(findings, healthyFinding(minioStorageProbeID, tierPage, p.class, target))
			continue
		}
		ratio := errors / requests
		tier := tierWarn
		if p.pageRatio > 0 && ratio >= p.pageRatio {
			tier = tierPage
		}
		observed := fmt.Sprintf("requests_%dm=%.0f errors_%dm=%.0f error_pct=%.2f", window, requests, window, errors, 100*ratio)
		f := finding{
			probeId: minioStorageProbeID, tier: tier, class: p.class, target: target, sustain: 2,
			observed: observed, evidence: evidence, playbook: "SIGNALS.md §11.6a",
		}
		switch p.class {
		case "minio-write-errors":
			f.symptom = fmt.Sprintf("%.1f%% of MinIO write requests on %s failed with 5xx in the last %d minutes", 100*ratio, target, window)
			f.mechanism = "MinIO counts 5xx per API without the status code. 507 XMinioStorageFull (drives at the free-space floor) is one; write-quorum loss, 503 SlowDown from a full request queue, and internal errors are the others. Loki, Mimir and blob writers retry, then drop or back up."
			f.baseline = fmt.Sprintf("Under %.0f%% of write requests fail with 5xx over %d minutes (or fewer than %d failures); PAGE at %.0f%%.", 100*p.warnRatio, window, int(p.minimum), 100*p.pageRatio)
			f.context = "With minio-drive-capacity at band=floor these are 507s, as on 2026-10-11; with minio-drive-offline they are quorum loss. Otherwise the MinIO journal names the error code."
			f.action = "Read `journalctl -u minio` on the host for the error codes in this window and correlate drive capacity and offline drives. Do not restart MinIO to clear the errors."
			f.verify = "Two consecutive runs with write 5xx under 1% and Loki, Mimir and blob writers flushing again."
		case "minio-read-errors":
			f.symptom = fmt.Sprintf("%.1f%% of MinIO read and list requests on %s failed with 5xx in the last %d minutes", 100*ratio, target, window)
			f.mechanism = "Reads fail with 5xx when the erasure set loses read quorum, a drive returns I/O errors or timeouts, or MinIO sheds load with 503 SlowDown. Loki and Mimir queries over stored chunks and blocks then fail."
			f.baseline = fmt.Sprintf("Under %.0f%% of non-write requests fail with 5xx over %d minutes (or fewer than %d failures); PAGE at %.0f%%.", 100*p.warnRatio, window, int(p.minimum), 100*p.pageRatio)
			f.context = "404s for absent objects are 4xx and are not counted here."
			f.action = "Read `journalctl -u minio` on the host for the error codes, check minio-drive-offline and the kernel log for drive errors. Do not restart MinIO to clear the errors."
			f.verify = "Two consecutive runs with read 5xx under 5%."
		default:
			f.symptom = fmt.Sprintf("%.1f%% of MinIO write requests on %s were rejected with 4xx in the last %d minutes", 100*ratio, target, window)
			f.mechanism = "A write API answering 4xx is refusing the request itself: 400 XMinioAdminBucketQuotaExceeded for a full hard quota, 403 for credentials or policy, or a malformed or conditional request. Quota rejections raise no 5xx, so this partition is the symptom-side control for minio-bucket-quota."
			f.baseline = fmt.Sprintf("Under %.0f%% of write requests are rejected with 4xx over %d minutes (or fewer than %d rejections).", 100*p.warnRatio, window, int(p.minimum))
			f.context = "Correlate minio-bucket-quota first. MinIO's v3 bucket-level counters are not scraped (they need ?buckets=), so this partition cannot name the bucket."
			f.action = "Check bucket quotas against usage, recent quota or policy changes, and the blob service account's credentials; the MinIO journal and audit log name the bucket and code. Never apply a quota under current usage."
			f.verify = "Two consecutive runs with write 4xx under 10% and no minio-bucket-quota finding."
		}
		findings = append(findings, f)
	}
	return findings
}
