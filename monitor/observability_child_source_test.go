// Exercise the actual five shell producers with invented, isolated sources.
// Native completion, rather than a parseable prefix, owns observation authority.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// One invented failure is applied to the native source or selector stage.
type childSourceControl struct {
	owner    string
	shape    string
	fault    string
	status   int
	positive bool
	total    int64
}

// The returned frame and native terminal result remain independent evidence.
type childSourceOutput struct {
	body      string
	err       error
	curlCalls int
}

// Every fixture executable is local. The only inherited tools are the exact
// AWK/sort selectors; no real curl, ss, journal, SSH or service can be invoked.
func childSourceExecute(t *testing.T, control childSourceControl) childSourceOutput {
	t.Helper()
	directory := t.TempDir()
	realAwk, err := exec.LookPath("awk")
	if err != nil {
		t.Fatal("child acquisition controls require awk")
	}
	realSort, err := exec.LookPath("sort")
	if err != nil {
		t.Fatal("child acquisition controls require sort")
	}
	scripts := map[string]string{
		"ss": `#!/bin/sh
case "$*" in
 *-ltnH*)
  case "$CHILD_SOURCE_SHAPE" in
   empty) ;;
   malformed-listener) printf '%s\n' 'synthetic-private-source';;
   invalid-port) printf '%s\n' 'LISTEN 0 4096 127.0.0.1:65536 192.0.2.1:*';;
   last-port) printf '%s\n' 'LISTEN 0 4096 127.0.0.1:65535 192.0.2.1:*';;
   duplicate-listeners)
    i=0
    while [ "$i" -lt 1025 ]; do
     printf '%s\n' 'LISTEN 0 4096 127.0.0.1:19191 192.0.2.1:*'
     i=$((i+1))
    done
    ;;
   *)
    printf '%s\n' 'LISTEN 0 4096 127.0.0.1:19191 192.0.2.1:*'
    case "$CHILD_SOURCE_SHAPE" in mixed|mixed-positive)
     printf '%s\n' 'LISTEN 0 4096 127.0.0.1:19192 192.0.2.1:*';;
    esac
    ;;
  esac
  if [ "$CHILD_SOURCE_FAULT" = listener ]; then exit "$CHILD_SOURCE_STATUS"; fi
  ;;
 *) ;;
esac
`,
		"curl": `#!/bin/sh
printf 'call\n' >> "$CHILD_SOURCE_TRACE"
for argument; do url=$argument; done
source_status=0
positive="$CHILD_SOURCE_POSITIVE"
case "$CHILD_SOURCE_SHAPE:$url" in
 mixed:*:19191/*|mixed-positive:*:19191/*) source_status=28; positive=false;;
 mixed-positive:*:19192/*) positive=true;;
esac
case "$url" in
 */api/v1/status/buildinfo)
  printf '%s\n' '{"application":"Grafana Mimir","version":"3.1.1","revision":"a3d6c90f25"}'
  if [ "$CHILD_SOURCE_FAULT" = build-info ]; then source_status=$CHILD_SOURCE_STATUS; fi
  ;;
 */config)
  if [ "$CHILD_SOURCE_SHAPE" != unrelated ]; then
   case "$CHILD_SOURCE_OWNER" in
    mimir-shutdown)
     flush=true
     if [ "$positive" = true ]; then flush=false; fi
     printf '%s\n' 'blocks_storage:' '    tsdb:' "        flush_blocks_on_shutdown: $flush" '    bucket_store:' '        ignore_blocks_within: 10h' '        sync_interval: 1m' 'querier:' '    query_store_after: 12h' 'limits:' '    query_ingesters_within: 13h' 'compactor:'
     if [ "$CHILD_SOURCE_SHAPE" = malformed ]; then
      printf '%s\n' '    cleanup_interval: synthetic-private-source'
     else
      printf '%s\n' '    cleanup_interval: 15m'
     fi
     ;;
    *) printf '%s\n' 'limits:' '  max_global_series_per_user: 3000' '  ingestion_rate: 900' '  ingestion_burst_size: 1800';;
   esac
  fi
  printf '%s\n' 'private: synthetic-private-config'
  if [ "$CHILD_SOURCE_FAULT" = config ]; then source_status=$CHILD_SOURCE_STATUS; fi
  if [ "$CHILD_SOURCE_SHAPE" = status-lookalike ]; then printf 'config_source_status 0\n'; fi
  ;;
 */metrics)
  if [ "$CHILD_SOURCE_SHAPE" = unrelated ]; then printf '# unrelated successful endpoint\n'; exit "$source_status"; fi
  case "$CHILD_SOURCE_OWNER" in
   loki-tailers)
    tails=0
    if [ "$positive" = true ]; then tails=-1; fi
    if [ "$CHILD_SOURCE_SHAPE" = malformed ]; then tails=synthetic-private-source; fi
    printf '%s\n' 'loki_build_info{version="3.7.3"} 1' 'process_start_time_seconds 1956803400.125' "loki_querier_tail_active $tails" 'loki_querier_tail_active_streams 0'
    ;;
   mimir-index)
    sync=$CHILD_SOURCE_FRESH_TIME
    if [ "$positive" = true ]; then sync=$CHILD_SOURCE_STALE_TIME; fi
    printf '%s\n' 'cortex_build_info{version="3.1.1"} 1' 'process_start_time_seconds 1956803400.125' "cortex_bucket_stores_blocks_last_successful_sync_timestamp_seconds $sync" 'cortex_bucket_stores_blocks_sync_seconds_count 25' 'cortex_bucket_stores_tenants_discovered 0' 'cortex_bucket_stores_tenants_synced 0'
    ;;
   *)
    printf '%s\n' 'process_start_time_seconds 1956803400.125' '# HELP cortex_distributor_samples_in_total Synthetic samples.' '# TYPE cortex_distributor_samples_in_total counter' "cortex_distributor_samples_in_total{user=\"synthetic-private-tenant\"} $CHILD_SOURCE_TOTAL" '# HELP cortex_distributor_received_samples_total Synthetic samples.' '# TYPE cortex_distributor_received_samples_total counter' "cortex_distributor_received_samples_total{user=\"synthetic-private-tenant\"} $CHILD_SOURCE_TOTAL" '# HELP cortex_distributor_requests_in_total Synthetic requests.' '# TYPE cortex_distributor_requests_in_total counter' 'cortex_distributor_requests_in_total 0' 'cortex_ring_members{name="distributor",state="ACTIVE"} 1' 'cortex_ingester_memory_series 50' 'cortex_ingester_active_series{instance="synthetic-private-tenant"} 40' 'cortex_ingester_memory_series_created_total{instance="synthetic-private-tenant"} 100' 'cortex_ingester_memory_series_removed_total{instance="synthetic-private-tenant"} 10' 'cortex_ingester_local_limits{limit="max_global_series_per_user"} 1000' '# HELP cortex_discarded_samples_total Synthetic discarded samples.' '# TYPE cortex_discarded_samples_total counter'
    if [ "$positive" = true ]; then
     printf '%s\n' 'cortex_discarded_samples_total{reason="per_user_series_limit",user="synthetic-private-tenant"} 5' 'cortex_discarded_samples_total{reason="rate_limited",user="synthetic-private-tenant"} 7'
    fi
    ;;
  esac
  printf '# private fixture synthetic-private-metric\n'
  if [ "$CHILD_SOURCE_FAULT" = metrics ]; then source_status=$CHILD_SOURCE_STATUS; fi
  ;;
 *) exit 126;;
esac
printf 'synthetic-private-diagnostic\n' >&2
exit "$source_status"
`,
		"awk": `#!/bin/sh
"$CHILD_SOURCE_REAL_AWK" "$@"
source_status=$?
case "$CHILD_SOURCE_FAULT:$*" in
 listener-selector:*\$4*) exit 41;;
 metrics-selector:*loki_build_info*) exit 42;;
 config-selector:*'function yaml_key(value)'*) exit 43;;
 absence-selector:*'function occurrences(text, needle'*) exit 44;;
esac
exit "$source_status"
`,
		"sort": `#!/bin/sh
"$CHILD_SOURCE_REAL_SORT" "$@"
source_status=$?
if [ "$CHILD_SOURCE_FAULT" = listener-sort ]; then exit 45; fi
exit "$source_status"
`,
	}
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	shell := "/bin/sh"
	extraEnvironment := []string{}
	switch control.fault {
	case "listener-status-missing", "listener-status-malformed", "listener-status-closed", "config-status-missing", "config-status-malformed", "config-status-closed":
		shell = "/bin/bash"
		faultPath := filepath.Join(directory, "shell-fault")
		body := `printf() {
 case "$CHILD_SOURCE_FAULT:$1" in
  listener-status-missing:listener_source_status*|config-status-missing:config_source_status*) return 0;;
  listener-status-malformed:listener_source_status*) builtin printf 'listener_source_status malformed\n'; return;;
  config-status-malformed:config_source_status*) builtin printf 'config_source_status malformed\n'; return;;
 esac
 builtin printf "$@"
}
ss() {
 "$CHILD_SOURCE_REAL_SS" "$@"
 source_status=$?
 if [ "$CHILD_SOURCE_FAULT" = listener-status-closed ]; then exec 3>&-; fi
 return "$source_status"
}
curl() {
 "$CHILD_SOURCE_REAL_CURL" "$@"
 source_status=$?
 case "$CHILD_SOURCE_FAULT:$*" in config-status-closed:*/config) exec 3>&-;; esac
 return "$source_status"
}
`
		if err := os.WriteFile(faultPath, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		extraEnvironment = []string{"BASH_ENV=" + faultPath}
	}
	commands := map[string]string{
		"loki-tailers":    lokiTailersScript,
		"mimir-index":     mimirIndexScript,
		"mimir-shutdown":  mimirShutdownScript,
		"mimir-balance":   mimirBalanceCommand(),
		"mimir-admission": mimirAdmissionCommand("synthetic", nil),
	}
	command, ok := commands[control.owner]
	if !ok {
		t.Fatal("unrecognized child producer control")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	killGroup := func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = killGroup
	cmd.WaitDelay = 2 * time.Second
	now := time.Date(2032, 3, 4, 5, 6, 0, 0, time.UTC)
	tracePath := filepath.Join(directory, "curl-trace")
	cmd.Env = append([]string{
		"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C",
		"CHILD_SOURCE_OWNER=" + control.owner, "CHILD_SOURCE_SHAPE=" + control.shape,
		"CHILD_SOURCE_FAULT=" + control.fault, "CHILD_SOURCE_TRACE=" + tracePath,
		fmt.Sprintf("CHILD_SOURCE_STATUS=%d", control.status),
		fmt.Sprintf("CHILD_SOURCE_POSITIVE=%t", control.positive),
		fmt.Sprintf("CHILD_SOURCE_TOTAL=%d", control.total),
		fmt.Sprintf("CHILD_SOURCE_FRESH_TIME=%d", now.Add(-5*time.Minute).Unix()),
		fmt.Sprintf("CHILD_SOURCE_STALE_TIME=%d", now.Add(-31*time.Minute).Unix()),
		"CHILD_SOURCE_REAL_AWK=" + realAwk, "CHILD_SOURCE_REAL_SORT=" + realSort,
		"CHILD_SOURCE_REAL_CURL=" + filepath.Join(directory, "curl"),
		"CHILD_SOURCE_REAL_SS=" + filepath.Join(directory, "ss"),
	}, extraEnvironment...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, runErr := cmd.Output()
	killGroup()
	if ctx.Err() != nil {
		t.Fatal("invented child producer exceeded its deterministic fixture deadline")
	}
	childSourceRequirePrivate(t, string(output)+stderr.String())
	for _, marker := range []string{"listener_source_status", "config_source_status"} {
		if strings.Contains(string(output), marker) {
			t.Fatal("internal native status marker escaped the producer frame")
		}
	}
	trace, err := os.ReadFile(tracePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return childSourceOutput{body: string(output), err: runErr, curlCalls: strings.Count(string(trace), "call\n")}
}

// Public Signal.Run and rendering consume only the actual generated frame.
// The synthetic adapter never adds completion data or repairs producer output.
func childSourceAlerts(t *testing.T, owner string, output childSourceOutput) []Alert {
	t.Helper()
	signals := map[string]Signal{
		"loki-tailers": NewLokiTailersSignal(), "mimir-index": NewMimirIndexSignal(),
		"mimir-shutdown": NewMimirShutdownSignal(), "mimir-balance": NewMimirBalanceSignal(),
		"mimir-admission": NewMimirAdmissionSignal(),
	}
	source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		return output.body, output.err
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "child-authority.example", Roles: []string{"services"}}}
	settings.StateDir = t.TempDir()
	settings.Now = func() time.Time { return time.Date(2032, 3, 4, 5, 6, 0, 0, time.UTC) }
	alerts, err := signals[owner].Run(context.Background(), settings)
	if err != nil {
		t.Fatal("public child source signal failed outside its visibility contract")
	}
	for _, alert := range alerts {
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		childSourceRequirePrivate(t, string(encoded)+alert.Markdown())
	}
	return alerts
}

// Private fixture canaries must stay out of native and rendered output.
func childSourceRequirePrivate(t *testing.T, value string) {
	t.Helper()
	for _, private := range []string{"synthetic-private-source", "synthetic-private-config", "synthetic-private-metric", "synthetic-private-tenant", "synthetic-private-diagnostic"} {
		if strings.Contains(value, private) {
			t.Fatal("private source fixture escaped its observation boundary")
		}
	}
}

// Acquisition failure retains the owning visibility identity, never absence.
func childSourceRequireUnknown(t *testing.T, owner string, output childSourceOutput) []Alert {
	t.Helper()
	alerts := childSourceAlerts(t, owner, output)
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if visibility.Target != "child-authority.example/"+owner || visibility.Sustain != 2 || visibility.Severity != SeverityWarn {
		t.Fatal("acquisition failure changed the owning visibility identity")
	}
	for _, alert := range alerts {
		switch alert.Class {
		case "loki-tail-child-missing", "mimir-child-missing", "mimir-shutdown-child-missing":
			t.Fatal("failed discovery was misreported as a completed empty census")
		}
		if strings.Contains(alert.Observed, "observation_stage=no-local-child") {
			t.Fatal("failed discovery acquired no-local-child authority")
		}
	}
	return alerts
}

// Original ss|awk|sort erases native ss/selector status and still probes
// valid prefixes; failed empty input becomes missing-child rather than unknown.
func TestChildSourceListenerCompletion(t *testing.T) {
	for _, owner := range []string{"loki-tailers", "mimir-index", "mimir-shutdown", "mimir-balance", "mimir-admission"} {
		for _, shape := range []string{"empty", "complete"} {
			for _, status := range []int{1, 28, 127, 130} {
				output := childSourceExecute(t, childSourceControl{owner: owner, shape: shape, fault: "listener", status: status})
				if output.err == nil || output.curlCalls != 0 || output.body != "" {
					t.Fatal("failed native listener prefix reached child discovery")
				}
				childSourceRequireUnknown(t, owner, output)
			}
		}
		for _, fault := range []string{"listener-selector", "listener-sort", "listener-status-missing", "listener-status-malformed", "listener-status-closed"} {
			output := childSourceExecute(t, childSourceControl{owner: owner, shape: "complete", fault: fault})
			if output.err == nil || output.curlCalls != 0 || output.body != "" {
				t.Fatal("lost listener status or failed downstream stage authorized a census")
			}
			childSourceRequireUnknown(t, owner, output)
		}
	}
}

// A complete valid prefix followed by a native failure remains unknown. This
// matrix uses the unchanged actual status-bearing source stage in every owner.
func TestChildSourceRejectsFailedValidPrefixes(t *testing.T) {
	cases := []struct{ owner, fault string }{
		{owner: "loki-tailers", fault: "metrics"}, {owner: "loki-tailers", fault: "metrics-selector"},
		{owner: "mimir-index", fault: "build-info"}, {owner: "mimir-balance", fault: "build-info"},
		{owner: "mimir-admission", fault: "build-info"}, {owner: "mimir-admission", fault: "absence-selector"},
		{owner: "mimir-shutdown", fault: "config"}, {owner: "mimir-shutdown", fault: "config-selector"},
	}
	for _, test := range cases {
		for _, status := range []int{1, 6, 22, 28, 127, 130} {
			output := childSourceExecute(t, childSourceControl{owner: test.owner, shape: "complete", fault: test.fault, status: status})
			if output.err != nil {
				t.Fatal("per-port failure discarded the bounded completed host envelope")
			}
			childSourceRequireUnknown(t, test.owner, output)
		}
	}
}

// Config body text cannot forge or replace the independent status channel.
func TestChildSourceShutdownRequiresUnforgeableStatus(t *testing.T) {
	for _, fault := range []string{"config-status-missing", "config-status-malformed", "config-status-closed"} {
		output := childSourceExecute(t, childSourceControl{owner: "mimir-shutdown", shape: "complete", fault: fault})
		childSourceRequireUnknown(t, "mimir-shutdown", output)
	}
	output := childSourceExecute(t, childSourceControl{owner: "mimir-shutdown", shape: "status-lookalike", fault: "config", status: 28})
	childSourceRequireUnknown(t, "mimir-shutdown", output)
}

// Every unknown port leaves a fully observed sibling's affirmative failure
// intact, including same-host admission pages and Loki underflow.
func TestChildSourceRetainsIndependentPositives(t *testing.T) {
	for owner, classes := range map[string][]string{
		"loki-tailers":    {"loki-tail-accounting-invalid"},
		"mimir-index":     {"mimir-store-gateway-stale"},
		"mimir-shutdown":  {"mimir-shutdown-flush-disabled", "mimir-replacement-continuity-unverified"},
		"mimir-admission": {"mimir-series-limit", "mimir-ingestion-rate-limit"},
	} {
		output := childSourceExecute(t, childSourceControl{owner: owner, shape: "mixed-positive"})
		alerts := childSourceRequireUnknown(t, owner, output)
		for _, class := range classes {
			positive := requireAlertClass(t, alerts, class)
			if owner == "mimir-admission" && positive.Severity != SeverityPage {
				t.Fatal("independent admission loss lost its page identity")
			}
		}
	}
}

// Successful empty/irrelevant discovery remains different from failed empty.
// Successful complete zero gauges do not acquire an observation failure.
func TestChildSourceHealthyZeroAndEmptyControls(t *testing.T) {
	for _, owner := range []string{"loki-tailers", "mimir-index", "mimir-shutdown", "mimir-balance", "mimir-admission"} {
		for _, shape := range []string{"complete", "empty", "last-port", "duplicate-listeners"} {
			output := childSourceExecute(t, childSourceControl{owner: owner, shape: shape})
			if output.err != nil {
				t.Fatal("complete native source lost acquisition authority")
			}
			alerts := childSourceAlerts(t, owner, output)
			if shape == "empty" {
				if output.curlCalls != 0 {
					t.Fatal("empty listener census issued child requests")
				}
				classes := map[string]string{"loki-tailers": "loki-tail-child-missing", "mimir-index": "mimir-child-missing", "mimir-shutdown": "mimir-shutdown-child-missing", "mimir-balance": "cannot-observe", "mimir-admission": "cannot-observe"}
				alert := requireAlertClass(t, alerts, classes[owner])
				if owner == "mimir-balance" && !strings.Contains(alert.Observed, "observation_stage=no-local-child") {
					t.Fatal("successful empty balance census lost its no-child distinction")
				}
				continue
			}
			for _, alert := range alerts {
				if alert.Class != "mimir-replacement-continuity-unverified" {
					t.Fatal("complete healthy measurements created a false finding")
				}
			}
			expectedCalls := map[string]int{"loki-tailers": 1, "mimir-index": 2, "mimir-shutdown": 1, "mimir-balance": 3, "mimir-admission": 3}
			if output.curlCalls != expectedCalls[owner] {
				t.Fatal("listener deduplication changed child request scope")
			}
		}
	}
	for _, owner := range []string{"loki-tailers", "mimir-shutdown"} {
		output := childSourceExecute(t, childSourceControl{owner: owner, shape: "unrelated"})
		alerts := childSourceAlerts(t, owner, output)
		for _, alert := range alerts {
			if alert.Class == "cannot-observe" {
				t.Fatal("successful irrelevant endpoint was classified as failed acquisition")
			}
		}
	}
}

// Malformed endpoints and scalars remain private observation failures.
func TestChildSourceMalformedInputsRemainUnknown(t *testing.T) {
	for _, owner := range []string{"loki-tailers", "mimir-index", "mimir-shutdown", "mimir-balance", "mimir-admission"} {
		for _, shape := range []string{"malformed-listener", "invalid-port"} {
			output := childSourceExecute(t, childSourceControl{owner: owner, shape: shape})
			childSourceRequireUnknown(t, owner, output)
			if output.curlCalls != 0 {
				t.Fatal("malformed census authorized a child request")
			}
		}
	}
	for _, owner := range []string{"loki-tailers", "mimir-shutdown"} {
		output := childSourceExecute(t, childSourceControl{owner: owner, shape: "malformed"})
		childSourceRequireUnknown(t, owner, output)
	}
}

// A discovery hole cannot erase the affected host's previous identity set,
// establish complete quiet recovery, or turn a current snapshot into a rate.
func TestChildSourceStatefulRecoveryAuthority(t *testing.T) {
	now := time.Date(2032, 3, 4, 5, 6, 0, 0, time.UTC)
	target := &host{name: "child-authority.example"}
	balanceBefore, err := parseMimirBalanceHostSample(childSourceExecute(t, childSourceControl{owner: "mimir-balance", shape: "complete"}).body)
	if err != nil {
		t.Fatal(err)
	}
	_, prior := assessMimirBalance(now, mimirBalancePersistedState{}, []mimirBalanceHostResult{{host: target, sample: balanceBefore}})
	balancePartial, err := parseMimirBalanceHostSample(childSourceExecute(t, childSourceControl{owner: "mimir-balance", shape: "mixed", total: 1200}).body)
	if err != nil {
		t.Fatal(err)
	}
	unknown, retained := assessMimirBalance(now.Add(time.Minute), prior, []mimirBalanceHostResult{{host: target, sample: balancePartial}})
	if unknown.directComplete || unknown.rateComplete || !reflect.DeepEqual(prior, retained) || mimirBalanceIsSkewed(unknown) {
		t.Fatal("partial discovery advanced balance history or health authority")
	}
	balanceCorrected, err := parseMimirBalanceHostSample(childSourceExecute(t, childSourceControl{owner: "mimir-balance", shape: "complete", total: 1200}).body)
	if err != nil {
		t.Fatal(err)
	}
	corrected, _ := assessMimirBalance(now.Add(2*time.Minute), retained, []mimirBalanceHostResult{{host: target, sample: balanceCorrected}})
	if !corrected.directComplete || !corrected.rateComplete || mimirBalanceIsSkewed(corrected) {
		t.Fatal("complete corrected source did not restore a valid rate comparison")
	}

	admissionBefore, err := parseMimirAdmissionHostSample(childSourceExecute(t, childSourceControl{owner: "mimir-admission", shape: "complete"}).body)
	if err != nil {
		t.Fatal(err)
	}
	probe := &mimirAdmissionProbe{}
	probe.observe(now, []mimirAdmissionHostResult{{host: target, sample: admissionBefore}})
	probe.incident = true
	probe.rateIncident = true
	probe.quietSince = now.Add(-mimirAdmissionQuietWindow)
	probe.rateQuietSince = now.Add(-mimirAdmissionQuietWindow)
	priorAdmission := cloneMimirAdmissionHistories(probe.histories)
	admissionUnknown, err := parseMimirAdmissionHostSample(childSourceExecute(t, childSourceControl{owner: "mimir-admission", shape: "complete", fault: "build-info", status: 28}).body)
	if err != nil {
		t.Fatal(err)
	}
	assessment := probe.observe(now.Add(time.Minute), []mimirAdmissionHostResult{{host: target, sample: admissionUnknown}})
	if assessment.directComplete || assessment.comparable || !assessment.incidentActive || !assessment.rateIncidentActive || !probe.quietSince.IsZero() || !probe.rateQuietSince.IsZero() || !reflect.DeepEqual(priorAdmission, probe.histories) {
		t.Fatal("incomplete discovery cleared an incident, lost history or advanced quiet recovery")
	}
}

// Failure of the lazy-absence qualifier cannot discard an independently
// descriptor-backed positive from this same complete metrics acquisition.
func TestChildSourceAdmissionRetainsPositiveThroughQualifierFailure(t *testing.T) {
	output := childSourceExecute(t, childSourceControl{owner: "mimir-admission", shape: "complete", fault: "absence-selector", positive: true})
	alerts := childSourceRequireUnknown(t, "mimir-admission", output)
	for _, class := range []string{"mimir-series-limit", "mimir-ingestion-rate-limit"} {
		alert := requireAlertClass(t, alerts, class)
		if alert.Severity != SeverityPage {
			t.Fatal("an independent positive lost its page through optional qualifier failure")
		}
	}
}

// Failed acquisitions cannot prove affirmative faults from their otherwise
// valid negative prefixes; successful independent children are tested above.
func TestChildSourceFailedNegativePrefixCannotProveFailure(t *testing.T) {
	for _, control := range []childSourceControl{
		{owner: "loki-tailers", shape: "complete", fault: "metrics", status: 28, positive: true},
		{owner: "mimir-shutdown", shape: "complete", fault: "config", status: 28, positive: true},
	} {
		alerts := childSourceRequireUnknown(t, control.owner, childSourceExecute(t, control))
		for _, alert := range alerts {
			if alert.Class != "cannot-observe" {
				t.Fatal("failed native body supplied an affirmative child fault")
			}
		}
	}
}

// Preserve the existing Mimir metrics completion guards while changing their
// preceding discovery stages. A failed positive body supplies no counter/page.
func TestChildSourceRetainsExistingMimirMetricsCompletion(t *testing.T) {
	for _, owner := range []string{"mimir-index", "mimir-balance", "mimir-admission"} {
		output := childSourceExecute(t, childSourceControl{owner: owner, shape: "complete", fault: "metrics", status: 28, positive: true})
		alerts := childSourceAlerts(t, owner, output)
		class := "cannot-observe"
		if owner == "mimir-index" {
			class = "mimir-index-unobservable"
		}
		requireAlertClass(t, alerts, class)
		for _, alert := range alerts {
			if alert.Class != class {
				t.Fatal("failed Mimir metrics prefix acquired affirmative measurement authority")
			}
		}
	}
}
