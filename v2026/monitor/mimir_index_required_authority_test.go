// Execute the actual metrics projection with isolated synthetic publishers;
// omitted measurements must remain unknown through the public consumer.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Describe metric values without depending on newly introduced parser fields.
func mimirRequiredMetrics(now time.Time, discovered, synced int, writer bool) string {
	lines := []string{
		`cortex_build_info{version="3.1.1"} 1`,
		fmt.Sprintf("process_start_time_seconds %d", now.Add(-6*time.Hour).Unix()),
		fmt.Sprintf("cortex_bucket_stores_blocks_last_successful_sync_timestamp_seconds %d", now.Add(-5*time.Minute).Unix()),
		"cortex_bucket_stores_blocks_sync_seconds_count 25",
		fmt.Sprintf("cortex_bucket_stores_tenants_discovered %d", discovered),
		fmt.Sprintf("cortex_bucket_stores_tenants_synced %d", synced),
	}
	if writer {
		lines = append(lines, fmt.Sprintf(`cortex_bucket_index_last_successful_update_timestamp_seconds{user="anonymous"} %d`, now.Add(-873*time.Second).Unix()))
	}
	return strings.Join(lines, "\n") + "\n"
}

// Remove one complete source family, keeping an explicit zero sibling intact.
func mimirRequiredOmit(body, name string) string {
	lines := []string{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+" ") && !strings.HasPrefix(line, name+"{") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// Fake loopback listeners/build-info/curl only; the production shell and awk
// execute unchanged. No real socket, SSH, Mimir or deployment config is used.
func mimirRequiredSettings(t *testing.T, bodies []string, curlStatus, awkStatus int) SignalSettings {
	t.Helper()
	directory := t.TempDir()
	listeners := []string{}
	for i, body := range bodies {
		port := 14819 + i
		listeners = append(listeners, fmt.Sprintf("LISTEN 0 4096 127.0.0.1:%d 0.0.0.0:*", port))
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%d.metrics", port)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	scripts := map[string]string{
		"ss": "#!/bin/sh\nprintf '%s\\n' \"$MIMIR_AUTH_LISTENERS\"\n",
		"curl": `#!/bin/sh
for argument; do url=$argument; done
case "$url" in
  *'/api/v1/status/buildinfo') printf '%s\n' '{"application":"Grafana Mimir"}' ;;
  *'/metrics')
    port=${url#http://127.0.0.1:}; port=${port%%/*}
    case "$port" in 14819|14820) ;; *) exit 126 ;; esac
    cat "$MIMIR_AUTH_DIRECTORY/$port.metrics"
    exit "$MIMIR_AUTH_CURL_STATUS"
    ;;
  *) exit 126 ;;
esac
`,
		"awk": `#!/bin/sh
case "$1" in
  *cortex_build_info*)
    if [ "$MIMIR_AUTH_AWK_STATUS" != 0 ]; then
      printf 'version 3.1.1\nprocess_start 1780000000\ngateway_last_sync 1780000000\ngateway_sync_count 0\ntenants_discovered 0\ntenants_synced 0\n'
      exit "$MIMIR_AUTH_AWK_STATUS"
    fi
    ;;
esac
exec /usr/bin/awk "$@"
`,
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if !strings.Contains(command, mimirIndexMarker) {
			return "", fmt.Errorf("unexpected required-metric fixture command")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		child.Env = []string{
			"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C",
			"MIMIR_AUTH_DIRECTORY=" + directory,
			"MIMIR_AUTH_LISTENERS=" + strings.Join(listeners, "\n"),
			"MIMIR_AUTH_CURL_STATUS=" + strconv.Itoa(curlStatus),
			"MIMIR_AUTH_AWK_STATUS=" + strconv.Itoa(awkStatus),
		}
		output, err := child.Output()
		if err != nil {
			t.Fatal("isolated Mimir collector fixture failed")
		}
		return string(output), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "mimir-authority.example", Roles: []string{"services"}}}
	return settings
}

// Check public visibility and privacy, not just an internal known-field flag.
func mimirRequiredUnknown(t *testing.T, alerts Alerts) {
	t.Helper()
	unknown := false
	for _, alert := range alerts {
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(alert.Markdown(), "synthetic-private-sentinel") || strings.Contains(string(encoded), "synthetic-private-sentinel") {
			t.Fatal("raw Mimir observation escaped into public output")
		}
		if alert.Class == "mimir-index-unobservable" {
			unknown = true
			if !isVisibilityAlert(alert) || alert.SignalNumber != "11.18" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
				t.Fatal("required-metric observation loss changed public authority or sustain")
			}
		}
	}
	if !unknown {
		t.Fatal("HTTP200 incomplete metrics became an observed empty or complete state")
	}
}

// Missing either side must not fabricate a mismatch; both missing must not
// manufacture empty discovery or a discovery-dependent missing writer.
func TestMimirIndexRequiredAuthorityOmission(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	full := mimirRequiredMetrics(now, 1, 1, false)
	for _, names := range [][]string{
		{"cortex_bucket_stores_tenants_discovered"},
		{"cortex_bucket_stores_tenants_synced"},
		{"cortex_bucket_stores_tenants_discovered", "cortex_bucket_stores_tenants_synced"},
		{"cortex_bucket_stores_blocks_last_successful_sync_timestamp_seconds"},
		{"process_start_time_seconds"},
		{"cortex_bucket_stores_blocks_sync_seconds_count"},
		{"cortex_build_info"},
	} {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			body := full
			for _, name := range names {
				body = mimirRequiredOmit(body, name)
			}
			settings := mimirRequiredSettings(t, []string{body}, 0, 0)
			alerts, err := NewMimirIndexSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			mimirRequiredUnknown(t, alerts)
			for _, alert := range alerts {
				if alert.Class == "mimir-store-gateway-tenants" || alert.Class == "mimir-bucket-index-stale" {
					t.Fatal("an absent required measurement invented a tenant or writer failure")
				}
			}
			env, err := newProbeEnv(settings.withDefaults())
			if err != nil {
				t.Fatal(err)
			}
			findings, err := (mimirIndexProbe{}).check(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			for _, finding := range findings {
				if finding.class == "mimir-bucket-index-stale" && finding.healthy {
					t.Fatal("partial authority established healthy empty writer state")
				}
			}
		})
	}
}

// Duplicate, invalid and unsupported source observations remain unknown while
// complete siblings and numeric zero controls keep their original meaning.
func TestMimirIndexRequiredAuthorityInvalidAndNativeStatus(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	full := mimirRequiredMetrics(now, 0, 0, false)
	for _, replacement := range []string{"NaN", "+Inf", "-1", "synthetic-private-sentinel", "0\ncortex_bucket_stores_tenants_discovered 0"} {
		body := strings.Replace(full, "cortex_bucket_stores_tenants_discovered 0", "cortex_bucket_stores_tenants_discovered "+replacement, 1)
		alerts, err := NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, []string{body}, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		mimirRequiredUnknown(t, alerts)
	}
	for _, body := range []string{
		strings.Replace(full, `version="3.1.1"`, `version="3.2.0"`, 1),
		strings.Replace(full, "process_start_time_seconds ", "process_start_time_seconds NaN\nignored ", 1),
		strings.Replace(full, "cortex_bucket_stores_blocks_last_successful_sync_timestamp_seconds ", "cortex_bucket_stores_blocks_last_successful_sync_timestamp_seconds +Inf\nignored ", 1),
		full + "cortex_bucket_index_last_successful_update_timestamp_seconds{user=\"anonymous\"} NaN\n",
		full + "cortex_bucket_index_last_successful_update_timestamp_seconds{role=\"synthetic-private-sentinel\"} 1\n",
	} {
		alerts, err := NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, []string{body}, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		mimirRequiredUnknown(t, alerts)
	}
	for _, status := range []struct{ curl, awk int }{{curl: 1}, {curl: 124}, {awk: 1}} {
		alerts, err := NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, []string{full}, status.curl, status.awk))
		if err != nil {
			t.Fatal(err)
		}
		mimirRequiredUnknown(t, alerts)
	}
}

// Explicit empty gauges, owner-only writer and healthy phase skew stay quiet.
func TestMimirIndexRequiredAuthorityHealthyAndThresholds(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	numeric := strings.ReplaceAll(mimirRequiredMetrics(now, 0, 0, false), "_count 25", "_count 2.5e1")
	numeric = strings.ReplaceAll(numeric, "_discovered 0", "_discovered 0e0")
	numeric = strings.ReplaceAll(numeric, "_synced 0", "_synced 0.0")
	for _, bodies := range [][]string{
		{numeric},
		{mimirRequiredMetrics(now, 0, 0, false)},
		{mimirRequiredMetrics(now, 1, 1, true), mimirRequiredMetrics(now, 1, 1, false)},
	} {
		alerts, err := NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, bodies, 0, 0))
		if err != nil || len(alerts) != 0 {
			t.Fatal("valid zero/owner-only/873second phase-skew control changed")
		}
	}
	for _, sample := range []struct {
		age    time.Duration
		writer bool
		want   string
	}{
		{age: 30 * time.Minute},
		{age: 30*time.Minute + time.Second, want: "mimir-store-gateway-stale"},
		{age: 35 * time.Minute, writer: true},
		{age: 35*time.Minute + time.Second, writer: true, want: "mimir-bucket-index-stale"},
	} {
		body := mimirRequiredMetrics(now, 0, 0, false)
		body = strings.Replace(body, fmt.Sprintf("process_start_time_seconds %d", now.Add(-6*time.Hour).Unix()), fmt.Sprintf("process_start_time_seconds %d", now.Add(-sample.age).Unix()), 1)
		if sample.writer {
			body = strings.ReplaceAll(body, "tenants_discovered 0", "tenants_discovered 1")
			body = strings.ReplaceAll(body, "tenants_synced 0", "tenants_synced 1")
		} else {
			body = strings.Replace(body, fmt.Sprintf("last_successful_sync_timestamp_seconds %d", now.Add(-5*time.Minute).Unix()), "last_successful_sync_timestamp_seconds 0", 1)
		}
		alerts, err := NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, []string{body}, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		if sample.want == "" {
			if len(alerts) != 0 {
				t.Fatal("startup grace boundary changed")
			}
		} else {
			requireAlertClass(t, alerts, sample.want)
		}
	}
	for _, future := range []time.Duration{30 * time.Second, 31 * time.Second} {
		body := mimirRequiredMetrics(now, 1, 1, true)
		body = strings.Replace(body, strconv.FormatInt(now.Add(-5*time.Minute).Unix(), 10), strconv.FormatInt(now.Add(future).Unix(), 10), 1)
		body = strings.Replace(body, strconv.FormatInt(now.Add(-873*time.Second).Unix(), 10), strconv.FormatInt(now.Add(future).Unix(), 10), 1)
		alerts, err := NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, []string{body}, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		if future == 30*time.Second {
			if len(alerts) != 0 {
				t.Fatal("inclusive future tolerance changed")
			}
		} else {
			requireAlertClass(t, alerts, "mimir-store-gateway-stale")
			requireAlertClass(t, alerts, "mimir-bucket-index-stale")
		}
	}
}

// Required-field unknown does not erase authoritative sibling positives.
func TestMimirIndexRequiredAuthorityMixedAndWholeHostLoss(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	partial := mimirRequiredOmit(mimirRequiredMetrics(now, 1, 1, false), "cortex_bucket_stores_tenants_synced")
	bad := mimirRequiredMetrics(now, 1, 0, true)
	bad = strings.Replace(bad, strconv.FormatInt(now.Add(-5*time.Minute).Unix(), 10), strconv.FormatInt(now.Add(-31*time.Minute).Unix(), 10), 1)
	bad = strings.Replace(bad, strconv.FormatInt(now.Add(-873*time.Second).Unix(), 10), strconv.FormatInt(now.Add(-36*time.Minute).Unix(), 10), 1)
	alerts, err := NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, []string{partial, bad}, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	mimirRequiredUnknown(t, alerts)
	for _, class := range []string{"mimir-store-gateway-stale", "mimir-store-gateway-tenants", "mimir-bucket-index-stale"} {
		alert := requireAlertClass(t, alerts, class)
		if alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatal("independent positive threshold or sustain changed")
		}
	}
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return "", fmt.Errorf("synthetic source unavailable") }}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "mimir-authority.example", Roles: []string{"services"}}}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (mimirIndexProbe{}).check(context.Background(), env)
	if err != nil || len(findings) != 1 || !findings[0].isUnobservable() {
		t.Fatal("whole host failure acquired aggregate authority")
	}
}

// Exercise the actual public completeness consumer without a sender, network,
// sleeps or a fabricated visibility bit. Unknown runs cannot advance a clear.
func TestMimirIndexRequiredAuthorityPublicConsumer(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	run := func(body string) Alerts {
		t.Helper()
		alerts, err := NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, []string{body}, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}
	bad := run(mimirRequiredMetrics(now, 1, 0, true))
	incident := requireAlertClass(t, bad, "mimir-store-gateway-tenants")
	unknown := run(mimirRequiredOmit(mimirRequiredMetrics(now, 1, 1, true), "cortex_bucket_stores_tenants_discovered"))
	mimirRequiredUnknown(t, unknown)
	notifier := &SlackAlertNotifier{
		settings:              SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2},
		keyIncidents:          map[string]*slackIncident{},
		signalKeyCompleteRuns: map[string]int{},
	}
	notifier.observeWithLock(now, "mimir-index", SignalExecution{Alerts: bad})
	key := slackIncidentKey(incident)
	for i := 0; i < 3; i++ {
		notifier.observeWithLock(now, "mimir-index", SignalExecution{Alerts: unknown})
		retained, exists := notifier.keyIncidents[key]
		if !exists || retained.signalKeyAbsentRuns["mimir-index"] != 0 {
			t.Fatal("unknown public source advanced incident recovery")
		}
	}
	healthy := run(mimirRequiredMetrics(now, 0, 0, false))
	if len(healthy) != 0 {
		t.Fatal("explicit observed0/0 became unknown")
	}
	notifier.observeWithLock(now, "mimir-index", SignalExecution{Alerts: healthy})
	if _, exists := notifier.keyIncidents[key]; !exists {
		t.Fatal("single complete run cleared incident")
	}
	notifier.observeWithLock(now, "mimir-index", SignalExecution{Alerts: healthy})
	if _, exists := notifier.keyIncidents[key]; exists {
		t.Fatal("complete healthy source failed existing clear policy")
	}
}

// Observed zero on one side cannot supply the other side's absent authority.
func TestMimirIndexRequiredAuthorityZeroSibling(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	for _, field := range []string{"cortex_bucket_stores_tenants_discovered", "cortex_bucket_stores_tenants_synced"} {
		body := mimirRequiredOmit(mimirRequiredMetrics(now, 0, 0, false), field)
		alerts, err := NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, []string{body}, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		mimirRequiredUnknown(t, alerts)
		for _, alert := range alerts {
			if alert.Class == "mimir-store-gateway-tenants" {
				t.Fatal("observed zero created an unobserved partner")
			}
		}
	}
}

// Missing success authority never acquires startup grace; a separately known
// stale timestamp remains useful even when its diagnostic count is unknown.
func TestMimirIndexRequiredAuthorityIndependentSyncAndGrace(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	body := mimirRequiredMetrics(now, 0, 0, false)
	body = strings.Replace(body, strconv.FormatInt(now.Add(-6*time.Hour).Unix(), 10), strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10), 1)
	body = mimirRequiredOmit(body, "cortex_bucket_stores_blocks_last_successful_sync_timestamp_seconds")
	alerts, err := NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, []string{body}, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	mimirRequiredUnknown(t, alerts)
	body = mimirRequiredOmit(mimirRequiredMetrics(now, 0, 0, false), "cortex_bucket_stores_blocks_sync_seconds_count")
	body = strings.Replace(body, strconv.FormatInt(now.Add(-5*time.Minute).Unix(), 10), strconv.FormatInt(now.Add(-31*time.Minute).Unix(), 10), 1)
	alerts, err = NewMimirIndexSignal().Run(context.Background(), mimirRequiredSettings(t, []string{body}, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	mimirRequiredUnknown(t, alerts)
	stale := requireAlertClass(t, alerts, "mimir-store-gateway-stale")
	if !strings.Contains(stale.Observed, "sync_count=unknown") {
		t.Fatal("missing diagnostic count became measured zero")
	}
}

// Retain the existing mixed-child boundary: a ready bad child still reports
// known positives while an unreadable sibling retains its own visibility.
func TestMimirIndexRequiredAuthorityMixedUnreadableChild(t *testing.T) {
	now := syntheticSettings(&syntheticSource{}).Now()
	ready := mimirIndexFixture(mimirIndexFixtureOptions{
		count: 1, metricsReady: true,
		processStart: now.Add(-6 * time.Hour), gatewayLastSync: now.Add(-31 * time.Minute),
		gatewaySyncCount: 25, tenantsDiscovered: 1, tenantsSynced: 1,
		indexUpdated: now.Add(-36 * time.Minute),
	})
	frame := strings.TrimSuffix(ready, "mimir_count 1\n") + "instance_begin 14820\nmetrics_ready 0\ninstance_end\nmimir_count 2\n"
	alerts := runMimirIndexSynthetic(t, map[string]string{"mimir-authority.example": frame})
	mimirRequiredUnknown(t, alerts)
	requireAlertClass(t, alerts, "mimir-store-gateway-stale")
	requireAlertClass(t, alerts, "mimir-bucket-index-stale")
}
