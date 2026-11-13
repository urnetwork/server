// The real Mimir command producers run against local invented source programs.
// A parseable configuration prefix never attests successful HTTP acquisition.
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

// Each invocation owns its synthetic commands and native process group. Only
// the real producer's fixed stdout frame reaches the Go reducer.
func mimirConfigAuthorityOutput(t *testing.T, command, shape string, status int, positive bool, total int64) string {
	t.Helper()
	directory := t.TempDir()
	for name, script := range map[string]string{
		"ss": `#!/bin/sh
case "$*" in
 *-ltnH*) printf '%s\n' 'LISTEN 0 4096 127.0.0.1:19191 192.0.2.1:*';;
 *) printf '%s\n' '0 0 127.0.0.1:3100 192.0.2.2:42000';;
esac
`,
		"curl": `#!/bin/sh
case "$*" in
 */api/v1/status/buildinfo)
  printf '%s\n' '{"application":"Grafana Mimir","version":"3.1.1","revision":"a3d6c90f25"}'
  ;;
 */config)
  case "$CONFIG_AUTHORITY_SHAPE" in
   empty) ;;
   zero) printf '%s\n' 'limits:' '  max_global_series_per_user: 0' '  ingestion_rate: 0' '  ingestion_burst_size: 0';;
   malformed) printf '%s\n' 'limits:' '  max_global_series_per_user: bad' '  ingestion_rate: bad' '  ingestion_burst_size: bad';;
   duplicate) printf '%s\n' 'limits:' '  max_global_series_per_user: 3000' '  ingestion_rate: 900' '  ingestion_rate: 901' '  ingestion_burst_size: 1800';;
   collision) printf '%s\n' 'config_source_status 0' 'limits:' '  max_global_series_per_user: 3000' '  ingestion_rate: 900' '  ingestion_burst_size: 1800';;
   *) printf '%s\n' 'limits:' '  max_global_series_per_user: 3000' '  ingestion_rate: 900' '  ingestion_burst_size: 1800' 'private: synthetic-private-config';;
  esac
  printf '%s\n' 'synthetic-private-config-diagnostic' >&2
  exit "$CONFIG_AUTHORITY_STATUS"
  ;;
 */metrics)
  ring=1
  if [ "$CONFIG_AUTHORITY_SHAPE" = fleet ]; then ring=3; fi
  printf '%s\n' \
   'process_start_time_seconds 1956803400.125' \
   '# HELP cortex_distributor_samples_in_total Synthetic attempted samples.' \
   '# TYPE cortex_distributor_samples_in_total counter' \
   "cortex_distributor_samples_in_total{user=\"synthetic-private-tenant\"} $CONFIG_AUTHORITY_TOTAL" \
   '# HELP cortex_distributor_received_samples_total Synthetic received samples.' \
   '# TYPE cortex_distributor_received_samples_total counter' \
   "cortex_distributor_received_samples_total{user=\"synthetic-private-tenant\"} $CONFIG_AUTHORITY_TOTAL" \
   '# HELP cortex_distributor_requests_in_total Synthetic requests.' \
   '# TYPE cortex_distributor_requests_in_total counter' \
   'cortex_distributor_requests_in_total 0' \
   "cortex_ring_members{name=\"distributor\",state=\"ACTIVE\"} $ring" \
   'cortex_ingester_memory_series 50' \
   'cortex_ingester_active_series{instance="synthetic-private-instance"} 40' \
   'cortex_ingester_memory_series_created_total{instance="synthetic-private-instance"} 100' \
   'cortex_ingester_memory_series_removed_total{instance="synthetic-private-instance"} 10' \
   'cortex_ingester_local_limits{limit="max_global_series_per_user"} 1000' \
   '# HELP cortex_discarded_samples_total Synthetic discards.' \
   '# TYPE cortex_discarded_samples_total counter'
  if [ "$CONFIG_AUTHORITY_POSITIVE" = true ]; then
   printf '%s\n' 'cortex_discarded_samples_total{reason="per_user_series_limit",user="synthetic-private-tenant"} 5' 'cortex_discarded_samples_total{reason="rate_limited",user="synthetic-private-tenant"} 7'
  fi
  ;;
 *) exit 2;;
esac
`,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	shell := "/bin/sh"
	extraEnvironment := []string{}
	switch shape {
	case "missing-status", "malformed-status", "closed-status", "awk-failure":
		// Fault injection wraps actual builtins/tools; the production command,
		// original AWK selectors and every source pipeline still execute.
		shell = "/bin/bash"
		realAwk, err := exec.LookPath("awk")
		if err != nil {
			t.Fatal("native config authority controls require awk")
		}
		faultPath := filepath.Join(directory, "shell-fault")
		fault := `printf() {
 case "$1" in
  config_source_status*)
   case "$CONFIG_AUTHORITY_SHAPE" in
    missing-status) return 0;;
    malformed-status) builtin printf 'config_source_status malformed\n'; return;;
   esac
   ;;
 esac
 builtin printf "$@"
}
curl() {
 "$CONFIG_AUTHORITY_REAL_CURL" "$@"
 fetch_status=$?
 case "$CONFIG_AUTHORITY_SHAPE:$*" in closed-status:*/config) exec 3>&-;; esac
 return "$fetch_status"
}
awk() {
 "$CONFIG_AUTHORITY_REAL_AWK" "$@"
 awk_status=$?
 if [ "$CONFIG_AUTHORITY_SHAPE" = awk-failure ]; then
  case "$*" in *'function key(value)'*|*'function yaml_key(value)'*) return 41;; esac
 fi
 return "$awk_status"
}
`
		if err := os.WriteFile(faultPath, []byte(fault), 0600); err != nil {
			t.Fatal(err)
		}
		extraEnvironment = []string{"BASH_ENV=" + faultPath, "CONFIG_AUTHORITY_REAL_AWK=" + realAwk, "CONFIG_AUTHORITY_REAL_CURL=" + filepath.Join(directory, "curl")}
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
	cmd.Env = []string{
		"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C",
		"CONFIG_AUTHORITY_SHAPE=" + shape,
		fmt.Sprintf("CONFIG_AUTHORITY_STATUS=%d", status),
		fmt.Sprintf("CONFIG_AUTHORITY_POSITIVE=%t", positive),
		fmt.Sprintf("CONFIG_AUTHORITY_TOTAL=%d", total),
	}
	cmd.Env = append(cmd.Env, extraEnvironment...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	killGroup()
	if err != nil {
		t.Fatal("synthetic Mimir producer command failed")
	}
	for _, private := range []string{"synthetic-private-config", "synthetic-private-tenant", "synthetic-private-instance"} {
		if strings.Contains(string(output)+stderr.String(), private) {
			t.Fatal("private source material escaped the native producer")
		}
	}
	if strings.Contains(string(output), "config_source_status") {
		t.Fatal("internal config completion framing escaped the public producer schema")
	}
	return string(output)
}

// Both actual producers must reject failed curl prefixes even when every
// required numeric field and the independent metrics scrape are valid.
func TestMimirConfigAuthorityRejectsFailedCompletePrefixes(t *testing.T) {
	for _, status := range []int{1, 6, 22, 28, 127, 130} {
		balanceOutput := mimirConfigAuthorityOutput(t, mimirBalanceCommand(), "complete", status, false, 100)
		balance, err := parseMimirBalanceHostSample(balanceOutput)
		if err != nil {
			t.Fatal(err)
		}
		if balance.count != 1 || len(balance.instances) != 1 || balance.instances[0].observable {
			t.Errorf("status %d: failed config prefix acquired balance authority", status)
		}
		admissionOutput := mimirConfigAuthorityOutput(t, mimirAdmissionCommand("synthetic", nil), "complete", status, true, 100)
		admission, err := parseMimirAdmissionHostSample(admissionOutput)
		if err != nil {
			t.Fatal(err)
		}
		if admission.count != 1 || len(admission.instances) != 1 || admission.instances[0].observable {
			t.Errorf("status %d: failed config prefix acquired admission authority", status)
		}
	}
}

// Completion is necessary but not sufficient: empty, duplicate, malformed and
// zero required limits retain the existing parser refusal after successful curl.
func TestMimirConfigAuthorityRetainsMalformedConfigRefusal(t *testing.T) {
	for _, shape := range []string{"empty", "duplicate", "malformed", "zero"} {
		balance, err := parseMimirBalanceHostSample(mimirConfigAuthorityOutput(t, mimirBalanceCommand(), shape, 0, false, 0))
		if err != nil {
			t.Fatal(err)
		}
		admission, err := parseMimirAdmissionHostSample(mimirConfigAuthorityOutput(t, mimirAdmissionCommand("synthetic", nil), shape, 0, false, 0))
		if err != nil {
			t.Fatal(err)
		}
		if len(balance.instances) != 1 || balance.instances[0].observable || len(admission.instances) != 1 || admission.instances[0].observable {
			t.Errorf("shape %s: invalid config became observable", shape)
		}
	}
}

// A source gap cannot replace prior host history, publish balanced health, or
// erase the distinction between a corrected current sample and a stale interval.
func TestMimirConfigAuthorityBalancePreservesHistoryAndCorrects(t *testing.T) {
	now := time.Date(2032, 3, 4, 5, 6, 0, 0, time.UTC)
	target := &host{name: "metrics-config.example"}
	complete, err := parseMimirBalanceHostSample(mimirConfigAuthorityOutput(t, mimirBalanceCommand(), "complete", 0, false, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, prior := assessMimirBalance(now, mimirBalancePersistedState{}, []mimirBalanceHostResult{{host: target, sample: complete}})
	if len(prior.Histories) != 1 {
		t.Fatal("complete zero-counter observation did not arm history")
	}
	failed, err := parseMimirBalanceHostSample(mimirConfigAuthorityOutput(t, mimirBalanceCommand(), "complete", 28, false, 1200))
	if err != nil {
		t.Fatal(err)
	}
	unknown, retained := assessMimirBalance(now.Add(time.Minute), prior, []mimirBalanceHostResult{{host: target, sample: failed}})
	if unknown.directComplete || unknown.rateComplete || !reflect.DeepEqual(retained, prior) || len(unknown.visibilityFailures) != 1 || unknown.visibilityFailures[0].stage != mimirBalanceStageChildUnavailable || mimirBalanceIsSkewed(unknown) {
		t.Fatal("failed config fetch advanced balance authority or history")
	}
	alert := alertFromFinding(syntheticSettings(nil), "11.20b", "mimir-balance", "Mimir balance", mimirBalanceCannotObserveFinding(unknown.visibilityFailures[0]))
	mimirConfigAuthorityRequirePrivateAlert(t, alert)
	if alert.Class != "cannot-observe" || alert.Target != "metrics-config.example/mimir-balance" || alert.Sustain != 2 || alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "observation_stage=child-observation-unavailable") {
		t.Fatal("config failure changed balance visibility identity")
	}
	corrected, err := parseMimirBalanceHostSample(mimirConfigAuthorityOutput(t, mimirBalanceCommand(), "complete", 0, false, 1200))
	if err != nil {
		t.Fatal(err)
	}
	healthy, next := assessMimirBalance(now.Add(2*time.Minute), retained, []mimirBalanceHostResult{{host: target, sample: corrected}})
	if !healthy.directComplete || !healthy.rateComplete || len(healthy.visibilityFailures) != 0 || mimirBalanceIsSkewed(healthy) || reflect.DeepEqual(next, prior) {
		t.Fatal("complete corrected config did not restore qualified balance")
	}
	stale, _ := assessMimirBalance(now.Add(10*time.Minute), next, []mimirBalanceHostResult{{host: target, sample: corrected}})
	if stale.rateComplete || len(stale.visibilityFailures) != 1 || stale.visibilityFailures[0].stage != mimirBalanceStageStale {
		t.Fatal("successful config fetch authorized a stale comparison")
	}
}

// Complete descriptor-backed zeros can qualify quiet recovery. A failed fetch
// resets quiet authority, preserves history and incidents, and retains a direct
// positive page from an independently complete sibling observation.
func TestMimirConfigAuthorityAdmissionRetainsIncidentsAndSiblingPositive(t *testing.T) {
	now := time.Date(2032, 3, 4, 5, 6, 0, 0, time.UTC)
	target := &host{name: "metrics-config.example"}
	complete, err := parseMimirAdmissionHostSample(mimirConfigAuthorityOutput(t, mimirAdmissionCommand("synthetic", nil), "complete", 0, false, 0))
	if err != nil {
		t.Fatal(err)
	}
	probe := &mimirAdmissionProbe{}
	probe.observe(now, []mimirAdmissionHostResult{{host: target, sample: complete}})
	healthy := probe.observe(now.Add(time.Minute), []mimirAdmissionHostResult{{host: target, sample: complete}})
	if !healthy.directComplete || !healthy.comparable || healthy.incidentActive || healthy.rateIncidentActive || healthy.headroomObservedCount != 1 {
		t.Fatal("complete descriptor-backed zeros lost quiet authority")
	}
	probe.incident = true
	probe.rateIncident = true
	probe.quietSince = now.Add(-mimirAdmissionQuietWindow)
	probe.rateQuietSince = now.Add(-mimirAdmissionQuietWindow)
	prior := cloneMimirAdmissionHistories(probe.histories)
	failed, err := parseMimirAdmissionHostSample(mimirConfigAuthorityOutput(t, mimirAdmissionCommand("synthetic", nil), "complete", 28, false, 0))
	if err != nil {
		t.Fatal(err)
	}
	unknown := probe.observe(now.Add(2*time.Minute), []mimirAdmissionHostResult{{host: target, sample: failed}})
	if unknown.directComplete || unknown.comparable || !unknown.incidentActive || !unknown.rateIncidentActive || unknown.headroomObservedCount != 0 || !reflect.DeepEqual(probe.histories, prior) || !probe.quietSince.IsZero() || !probe.rateQuietSince.IsZero() {
		t.Fatal("failed config fetch advanced quiet recovery, policy or baseline")
	}
	if len(unknown.visibilityFailures) != 1 {
		t.Fatal("failed config fetch lost admission visibility")
	}
	alert := alertFromFinding(syntheticSettings(nil), "11.20a", "mimir-admission", "Mimir admission", cannotObserveFinding(unknown.visibilityFailures[0].target, unknown.visibilityFailures[0].err))
	mimirConfigAuthorityRequirePrivateAlert(t, alert)
	if alert.Class != "cannot-observe" || alert.Target != "metrics-config.example/mimir-admission" || alert.Sustain != 2 || alert.Severity != SeverityWarn {
		t.Fatal("config failure changed admission visibility identity")
	}
	positive, err := parseMimirAdmissionHostSample(mimirConfigAuthorityOutput(t, mimirAdmissionCommand("synthetic", nil), "complete", 0, true, 0))
	if err != nil {
		t.Fatal(err)
	}
	independent := &mimirAdmissionProbe{}
	assessment := independent.observe(now, []mimirAdmissionHostResult{{host: target, sample: failed}, {host: &host{name: "metrics-sibling.example"}, sample: positive}})
	if assessment.directComplete || !assessment.incidentActive || !assessment.rateIncidentActive || assessment.initialPositive != 1 || assessment.rateInitialPositive != 1 {
		t.Fatal("unknown config suppressed an independently qualified discard positive")
	}
	series := alertFromFinding(syntheticSettings(nil), "11.20a", "mimir-admission", "Mimir admission", mimirAdmissionLimitFinding(assessment))
	rate := alertFromFinding(syntheticSettings(nil), "11.20a", "mimir-admission", "Mimir admission", mimirAdmissionRateLimitFinding(assessment))
	if series.Class != "mimir-series-limit" || rate.Class != "mimir-ingestion-rate-limit" || series.Severity != SeverityPage || rate.Severity != SeverityPage {
		t.Fatal("independent direct positives lost their public page identities")
	}
	mimirConfigAuthorityRequirePrivateAlert(t, series)
	mimirConfigAuthorityRequirePrivateAlert(t, rate)
}

// Fixed visibility and incident rendering must exclude every private producer
// fixture marker from both structured and human-readable public output.
func mimirConfigAuthorityRequirePrivateAlert(t *testing.T, alert Alert) {
	t.Helper()
	encoded, err := json.Marshal(alert)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"synthetic-private-config", "synthetic-private-tenant", "synthetic-private-instance"} {
		if strings.Contains(string(encoded)+alert.Markdown(), private) {
			t.Fatal("private source material escaped into an alert")
		}
	}
}

// A genuinely complete fleet keeps its existing under-budget skew diagnosis;
// source-completion repair must not suppress independently valid observations.
func TestMimirConfigAuthorityRetainsCompleteBalanceSkew(t *testing.T) {
	now := time.Date(2032, 3, 4, 5, 6, 0, 0, time.UTC)
	priorResults := []mimirBalanceHostResult{}
	currentResults := []mimirBalanceHostResult{}
	for index, total := range []int64{24000, 6000, 6000} {
		target := &host{name: fmt.Sprintf("metrics-skew-%d.example", index)}
		before, err := parseMimirBalanceHostSample(mimirConfigAuthorityOutput(t, mimirBalanceCommand(), "fleet", 0, false, 0))
		if err != nil {
			t.Fatal(err)
		}
		after, err := parseMimirBalanceHostSample(mimirConfigAuthorityOutput(t, mimirBalanceCommand(), "fleet", 0, false, total))
		if err != nil {
			t.Fatal(err)
		}
		priorResults = append(priorResults, mimirBalanceHostResult{host: target, sample: before})
		currentResults = append(currentResults, mimirBalanceHostResult{host: target, sample: after})
	}
	_, prior := assessMimirBalance(now, mimirBalancePersistedState{}, priorResults)
	assessment, _ := assessMimirBalance(now.Add(time.Minute), prior, currentResults)
	if !assessment.directComplete || !assessment.rateComplete || !mimirBalanceIsSkewed(assessment) || assessment.fleetAttemptedRate != 600 {
		t.Fatal("complete source lost the measured under-budget distributor skew")
	}
	alert := alertFromFinding(syntheticSettings(nil), "11.20b", "mimir-balance", "Mimir balance", mimirBalanceFinding(assessment))
	if alert.Class != "mimir-distributor-skew" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
		t.Fatal("complete balance skew changed its public identity")
	}
	mimirConfigAuthorityRequirePrivateAlert(t, alert)
}

// Missing/closed status descriptors and failed selectors cannot be masked by
// a numerically valid selector output. Faults occur in the actual shell command.
func TestMimirConfigAuthorityRequiresStatusAndSelectorCompletion(t *testing.T) {
	for _, shape := range []string{"missing-status", "malformed-status", "closed-status", "awk-failure"} {
		balance, err := parseMimirBalanceHostSample(mimirConfigAuthorityOutput(t, mimirBalanceCommand(), shape, 0, false, 100))
		if err != nil {
			t.Fatal(err)
		}
		admission, err := parseMimirAdmissionHostSample(mimirConfigAuthorityOutput(t, mimirAdmissionCommand("synthetic", nil), shape, 0, false, 100))
		if err != nil {
			t.Fatal(err)
		}
		if len(balance.instances) != 1 || balance.instances[0].observable || len(admission.instances) != 1 || admission.instances[0].observable {
			t.Errorf("shape %s: incomplete internal source authority was accepted", shape)
		}
	}
}

// Raw input cannot forge the private completion channel, even when it contains
// the exact success marker and a numerically accepted required-limits prefix.
func TestMimirConfigAuthorityRejectsRawStatusLookalike(t *testing.T) {
	balance, err := parseMimirBalanceHostSample(mimirConfigAuthorityOutput(t, mimirBalanceCommand(), "collision", 28, false, 100))
	if err != nil {
		t.Fatal(err)
	}
	admission, err := parseMimirAdmissionHostSample(mimirConfigAuthorityOutput(t, mimirAdmissionCommand("synthetic", nil), "collision", 28, false, 100))
	if err != nil {
		t.Fatal(err)
	}
	if len(balance.instances) != 1 || balance.instances[0].observable || len(admission.instances) != 1 || admission.instances[0].observable {
		t.Fatal("raw configuration forged source completion")
	}
}
