package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mimirShutdownFixture(port int, flush bool) string {
	fixture := fmt.Sprintf(
		"instance_begin %d\n"+
			"flush %t\n"+
			"query_store_after 12h0m0s\n"+
			"query_ingesters_within 13h0m0s\n"+
			"ignore_blocks_within 10h0m0s\n"+
			"bucket_sync_interval 1m0s\n"+
			"compactor_cleanup_interval 15m0s\n"+
			"instance_end\n",
		port, flush,
	)
	return fixture + "mimir_count 1\n"
}

func runMimirShutdownSynthetic(t *testing.T, fixtures map[string]string) []Alert {
	t.Helper()
	var commands atomic.Int64
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		commands.Add(1)
		if !strings.Contains(command, mimirShutdownMarker) || !strings.Contains(command, "/config") {
			return "", fmt.Errorf("unexpected Mimir shutdown command on %s: %s", host.Name, command)
		}
		fixture, ok := fixtures[host.Name]
		if !ok {
			return "", fmt.Errorf("unexpected Mimir host %s", host.Name)
		}
		return fixture, nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = make([]HostSettings, 0, len(fixtures)+1)
	for host := range fixtures {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: host, Roles: []string{"services"}})
	}
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-only", Roles: []string{"pg-primary"}})

	alerts, err := NewMimirShutdownSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if commands.Load() != int64(len(fixtures)) {
		t.Fatalf("Mimir shutdown commands = %d, want %d services hosts", commands.Load(), len(fixtures))
	}
	return alerts
}

func TestMimirShutdownSignalFlushHealthyFleetRequiresReplacementDecision(t *testing.T) {
	alerts := runMimirShutdownSynthetic(t, map[string]string{
		"edge-0": mimirShutdownFixture(14819, true),
		"edge-1": mimirShutdownFixture(14818, true),
	})
	if len(alerts) != 1 || alerts[0].Class != "mimir-replacement-continuity-unverified" {
		t.Fatalf("flush-safe but replacement-unverified fleet alerts = %+v", alerts)
	}
}

func TestMimirShutdownSignalAggregatesDisabledFleet(t *testing.T) {
	alerts := runMimirShutdownSynthetic(t, map[string]string{
		"crisp": mimirShutdownFixture(14818, false),
		"edge-0": strings.TrimSuffix(mimirShutdownFixture(14818, true), "mimir_count 1\n") +
			strings.TrimSuffix(mimirShutdownFixture(14819, false), "mimir_count 1\n") +
			"mimir_count 2\n",
		"edge-1": mimirShutdownFixture(14819, true),
	})
	if len(alerts) != 2 {
		t.Fatalf("disabled and replacement-unverified Mimir configuration produced %d alerts, want 2: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "mimir-shutdown-flush-disabled")
	if alert.Class != "mimir-shutdown-flush-disabled" || alert.Target != "mimir-fleet" ||
		alert.SignalNumber != "11.21" || alert.SignalKey != "mimir-shutdown" || alert.Sustain != 1 {
		t.Fatalf("wrong Mimir shutdown alert identity: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"crisp:14818=false", "edge-0:14819=false", "ephemeral",
		"mimir_instances=4 disabled_instances=2",
		"7176ccd", "§8.13", "§11.20", "Historical fixed Mimir gaps are unrecoverable",
		"six allowlisted non-secret Boolean/duration fields", "never leave the host",
		"replacement-read continuity", "120-second",
		"3,600-second", "60-second timeout stops only the Warpctl controller",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("Mimir shutdown alert lacks %q:\n%s", want, markdown)
		}
	}
}

func TestMimirShutdownSignalFalseFlushDoesNotResolveIndependentHorizonClasses(t *testing.T) {
	zeroHorizon := strings.Replace(
		mimirShutdownFixture(14819, false),
		"query_store_after 12h0m0s", "query_store_after 0s", 1,
	)
	alerts := runMimirShutdownSynthetic(t, map[string]string{
		"edge-0": zeroHorizon,
		"edge-1": mimirShutdownFixture(14818, false),
	})
	if len(alerts) != 3 {
		t.Fatalf("independent false-flush findings = %d, want 3: %+v", len(alerts), alerts)
	}
	for _, class := range []string{
		"mimir-shutdown-flush-disabled",
		"mimir-noncompacted-query-risk",
		"mimir-replacement-continuity-unverified",
	} {
		alert := requireAlertClass(t, alerts, class)
		if alert.Target != "mimir-fleet" {
			t.Errorf("%s target = %q, want mimir-fleet", class, alert.Target)
		}
	}
}

func TestMimirShutdownSignalSyntheticMissingChild(t *testing.T) {
	alerts := runMimirShutdownSynthetic(t, map[string]string{
		"edge-0": "mimir_count 0\n",
	})
	if len(alerts) != 1 {
		t.Fatalf("missing Mimir child produced %d alerts, want 1: %+v", len(alerts), alerts)
	}
	alert := alerts[0]
	if alert.Class != "mimir-shutdown-child-missing" || alert.Target != "edge-0" || alert.Sustain != 2 {
		t.Fatalf("wrong missing-child alert: %+v", alert)
	}
	for _, want := range []string{
		"six allowlisted shutdown/recent-store fields",
		"six allowlisted non-secret Boolean/duration fields",
		"never returns the full rendered configuration",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("missing-child alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestMimirShutdownSignalCurrentDefaultsRequireReplacementDecision(t *testing.T) {
	alerts := runMimirShutdownSynthetic(t, map[string]string{
		"edge-0": mimirShutdownFixture(14819, true),
	})
	if len(alerts) != 1 {
		t.Fatalf("unarmed default configuration produced %d alerts, want 1: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "mimir-replacement-continuity-unverified")
	markdown := alert.Markdown()
	for _, want := range []string{
		"query_store_after=12h0m0s",
		"query_ingesters_within=13h0m0s",
		"ignore_blocks_within=10h0m0s",
		"bucket_sync_interval=1m0s",
		"compactor_cleanup_interval=15m0s",
		"operator architecture decision",
		"Do not automatically set query_store_after or ignore_blocks_within to zero",
	} {
		if !strings.Contains(markdown, want) {
			t.Errorf("replacement decision alert lacks %q:\n%s", want, markdown)
		}
	}
}

func TestMimirShutdownSignalClassifiesZeroHorizonAsNoncompactedQueryRisk(t *testing.T) {
	fixture := strings.Replace(
		mimirShutdownFixture(14819, true),
		"query_store_after 12h0m0s", "query_store_after 0s", 1,
	)
	alerts := runMimirShutdownSynthetic(t, map[string]string{"edge-0": fixture})
	alert := requireAlertClass(t, alerts, "mimir-noncompacted-query-risk")
	for _, want := range []string{
		"query_store_after=0s",
		"Store-gateway does not deduplicate chunks",
		"same-timestamp merge may choose either replica",
		"temporarily filled dashboard gap",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("non-compacted query alert lacks %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestMimirShutdownSignalPreservesDisabledResultThroughObservationLoss(t *testing.T) {
	source := &syntheticSource{hostFn: func(host HostSettings, _ string) (string, error) {
		if host.Name == "unreadable" {
			return "", fmt.Errorf("synthetic SSH loss")
		}
		return mimirShutdownFixture(14819, false), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "edge-0", Roles: []string{"services"}},
		{Name: "unreadable", Roles: []string{"services"}},
	}
	alerts, err := NewMimirShutdownSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]Alert{}
	for _, alert := range alerts {
		classes[alert.Class] = alert
	}
	if _, ok := classes["mimir-shutdown-flush-disabled"]; !ok {
		t.Fatalf("confirmed disabled setting was discarded: %+v", alerts)
	}
	if visibility, ok := classes["cannot-observe"]; !ok || visibility.Target != "unreadable/mimir-shutdown" {
		t.Fatalf("observation loss was discarded: %+v", alerts)
	}
}

func TestParseMimirShutdownHostSampleRejectsMalformedFraming(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		needle string
	}{
		{"count mismatch", strings.Replace(mimirShutdownFixture(14819, true), "mimir_count 1", "mimir_count 2", 1), "count=2 instances=1"},
		{"missing field", "instance_begin 14819\nflush true\ninstance_end\nmimir_count 1\n", "omitted required config field"},
		{"invalid boolean", strings.Replace(mimirShutdownFixture(14819, true), "flush true", "flush yes", 1), "invalid flush"},
		{"invalid duration", strings.Replace(mimirShutdownFixture(14819, true), "query_store_after 12h0m0s", "query_store_after secret-value", 1), "invalid query_store_after"},
		{"unknown output", "rendered_secret value\nmimir_count 0\n", "unknown field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseMimirShutdownHostSample(test.input)
			if err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("parse error = %v, want %q", err, test.needle)
			}
		})
	}
}

func runMimirShutdownScript(t *testing.T, curlScript string) (string, mimirShutdownHostSample) {
	t.Helper()
	binDir := t.TempDir()
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("ss", `#!/bin/sh
printf '%s\n' \
  'LISTEN 0 4096 127.0.0.1:14788 0.0.0.0:*' \
  'LISTEN 0 4096 127.0.0.1:14819 0.0.0.0:*'
`)
	writeExecutable("curl", curlScript)

	command := exec.Command("sh", "-c", mimirShutdownScript)
	command.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Mimir shutdown script: %v\n%s", err, output)
	}
	sample, err := parseMimirShutdownHostSample(string(output))
	if err != nil {
		t.Fatalf("parse Mimir shutdown script output: %v\n%s", err, output)
	}
	return string(output), sample
}

func TestMimirShutdownScriptAcceptsLiveRelativeIndentation(t *testing.T) {
	output, sample := runMimirShutdownScript(t, `#!/bin/sh
case "${*}" in
  *':14819/config')
	printf '%s\n' \
	  'memberlist:' \
	  '    join_members:' \
	  '        - ignored-host' \
	  '    codec: {name: ignored}' \
	  'querier:' \
	  '    query_store_after: 12h0m0s' \
	  'limits:' \
	  '    query_ingesters_within: 13h0m0s' \
	  'blocks_storage:' \
	  '    bucket_store:' \
	  '        sync_interval: 1m0s' \
	  '    s3:' \
	  '        secret_access_key: should-never-leave-host' \
	  '    tsdb:' \
	  '        flush_blocks_on_shutdown: false' \
	  'blocks_storage:' \
	  '        ignore_blocks_within: 10h0m0s' \
	  'compactor:' \
	  '    cleanup_interval: 15m0s'
	;;
  *':14788/config')
    printf '%s\n' \
      'auth_enabled: false' \
      'storage_config:' \
      '  boltdb_shipper: {}'
    ;;
  *) exit 2 ;;
esac
`)
	if strings.Contains(output, "should-never-leave-host") || strings.Contains(output, "auth_enabled") ||
		strings.Contains(output, "ignored-host") || strings.Contains(output, "codec") {
		t.Fatalf("Mimir shutdown script leaked rendered configuration:\n%s", output)
	}
	if sample.count != 1 || len(sample.instances) != 1 || sample.instances[0].port != 14819 ||
		sample.instances[0].flush || sample.instances[0].queryStoreAfter != 12*time.Hour ||
		sample.instances[0].queryIngestersWithin != 13*time.Hour ||
		sample.instances[0].ignoreBlocksWithin != 10*time.Hour ||
		sample.instances[0].bucketSyncInterval != time.Minute ||
		sample.instances[0].compactorCleanupInterval != 15*time.Minute {
		t.Fatalf("Mimir shutdown script sample = %+v\n%s", sample, output)
	}
}

func TestMimirShutdownScriptRejectsSiblingKeysWithoutLeakingValues(t *testing.T) {
	output, sample := runMimirShutdownScript(t, `#!/bin/sh
case "${*}" in
  *':14819/config')
	printf '%s\n' \
	  'unrelated:' \
	  '    query_store_after: sibling-value-must-stay-local' \
	  '    query_ingesters_within: sibling-value-must-stay-local' \
	  '    cleanup_interval: sibling-value-must-stay-local' \
	  'querier:' \
	  '    nested:' \
	  '        query_store_after: sibling-value-must-stay-local' \
	  '    query_store_after: 12h0m0s' \
	  'limits:' \
	  '    query_ingesters_within: 13h0m0s' \
	  'blocks_storage:' \
	  '    unrelated:' \
	  '        flush_blocks_on_shutdown: sibling-value-must-stay-local' \
	  '        ignore_blocks_within: sibling-value-must-stay-local' \
	  '        sync_interval: sibling-value-must-stay-local' \
	  '    bucket_store:' \
	  '        ignore_blocks_within: 10h0m0s' \
	  '        sync_interval: 1m0s' \
	  '    tsdb:' \
	  '        ignore_blocks_within: sibling-value-must-stay-local' \
	  '        flush_blocks_on_shutdown: true' \
	  'compactor:' \
	  '    nested:' \
	  '        cleanup_interval: sibling-value-must-stay-local' \
	  '    cleanup_interval: 15m0s' \
	  'object_store:' \
	  '    secret_access_key: sibling-value-must-stay-local'
	;;
  *':14788/config')
	printf '%s\n' \
	  'wrong_root:' \
	  '    flush_blocks_on_shutdown: false' \
	  '    query_store_after: 1h0m0s' \
	  '    query_ingesters_within: 2h0m0s' \
	  '    ignore_blocks_within: 3h0m0s' \
	  '    sync_interval: 4m0s' \
	  '    cleanup_interval: 5m0s'
	;;
  *) exit 2 ;;
esac
`)
	if strings.Contains(output, "sibling-value-must-stay-local") || strings.Contains(output, "secret_access_key") {
		t.Fatalf("Mimir shutdown script leaked a sibling or secret value:\n%s", output)
	}
	if sample.count != 1 || len(sample.instances) != 1 || sample.instances[0].port != 14819 ||
		!sample.instances[0].flush || sample.instances[0].queryStoreAfter != 12*time.Hour ||
		sample.instances[0].queryIngestersWithin != 13*time.Hour ||
		sample.instances[0].ignoreBlocksWithin != 10*time.Hour ||
		sample.instances[0].bucketSyncInterval != time.Minute ||
		sample.instances[0].compactorCleanupInterval != 15*time.Minute {
		t.Fatalf("Mimir shutdown sibling-key sample = %+v\n%s", sample, output)
	}
}
