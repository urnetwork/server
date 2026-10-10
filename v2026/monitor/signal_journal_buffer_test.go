package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func journalBufferFixture(overrides map[string]string) string {
	keys := []string{
		"observation_schema", "journald_active", "journald_active_seconds", "storage",
		"max_use", "max_file_size", "max_files", "max_file_sec", "max_retention",
		"uptime_seconds", "coverage_checked", "coverage_present",
		"boundary_entry_age_seconds", "coverage_target_seconds", "journal_file_scan_state",
		"journal_files", "journal_bytes", "journal_archived_files_5m",
		"journal_system_archived_files_5m", "journal_user_archived_files_5m",
		"journal_vacuum_policy_state", "journal_vacuum_runtime_state", "journal_vacuum_last_success_age_seconds",
		"journal_retention_build", "journal_retention_rotation_state", "journal_retention_rotations_5m",
	}
	values := map[string]string{
		"observation_schema": "6", "journald_active": "active", "journald_active_seconds": "7200",
		"storage": "persistent", "max_use": "100G", "max_file_size": "256M", "max_files": "1024",
		"max_file_sec": "5min", "max_retention": "-", "uptime_seconds": "7200",
		"coverage_checked": "1", "coverage_present": "1",
		"boundary_entry_age_seconds": "3300", "coverage_target_seconds": "3000",
		"journal_file_scan_state": "complete", "journal_files": "200", "journal_bytes": "10737418240",
		"journal_archived_files_5m": "10", "journal_system_archived_files_5m": "7",
		"journal_user_archived_files_5m": "3",
		"journal_vacuum_policy_state":    "valid", "journal_vacuum_runtime_state": "complete",
		"journal_vacuum_last_success_age_seconds": "5", "journal_retention_build": "unverified",
		"journal_retention_rotation_state": "complete", "journal_retention_rotations_5m": "0",
	}
	for key, value := range overrides {
		values[key] = value
	}
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := values[key]; ok {
			lines = append(lines, key+"="+value)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestJournalBufferSignalSyntheticProblemClasses(t *testing.T) {
	observations := map[string]string{
		"healthy": journalBufferFixture(nil),
		"drift":   journalBufferFixture(map[string]string{"max_file_sec": "1month"}),
		"short": journalBufferFixture(map[string]string{
			"coverage_present": "0", "boundary_entry_age_seconds": "1200",
		}),
		"inactive": journalBufferFixture(map[string]string{"journald_active": "failed"}),
		"young": journalBufferFixture(map[string]string{
			"uptime_seconds": "1200", "journald_active_seconds": "1200",
			"coverage_checked": "0", "coverage_present": "0", "boundary_entry_age_seconds": "1200",
		}),
		"recent-journal-restart": journalBufferFixture(map[string]string{
			"journald_active_seconds": "600", "coverage_checked": "0",
			"coverage_present": "0", "boundary_entry_age_seconds": "600",
		}),
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if !strings.Contains(command, journalBufferMarker) {
			t.Fatalf("journal buffer marker absent")
		}
		return observations[host.Name], nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "healthy", Roles: []string{"services"}},
		{Name: "drift", Roles: []string{"services"}},
		{Name: "short", Roles: []string{"services"}},
		{Name: "inactive", Roles: []string{"services"}},
		{Name: "young", Roles: []string{"services"}},
		{Name: "recent-journal-restart", Roles: []string{"services"}},
		{Name: "not-an-edge", Roles: []string{"pg-primary"}},
	}
	alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 3 {
		t.Fatalf("alerts=%d, want 3: %+v", len(alerts), alerts)
	}
	byTarget := map[string]Alert{}
	for _, alert := range alerts {
		byTarget[alert.Target] = alert
	}
	if alert := byTarget["drift"]; alert.Class != "journal-buffer-config" || alert.Severity != SeverityWarn {
		t.Fatalf("drift alert=%+v", alert)
	}
	if alert := byTarget["short"]; alert.Class != "journal-buffer-short" || !strings.Contains(alert.Markdown(), "Loki") {
		t.Fatalf("short alert=%+v", alert)
	}
	if alert := byTarget["inactive"]; alert.Class != "journal-buffer-unavailable" || alert.Severity != SeverityPage {
		t.Fatalf("inactive alert=%+v", alert)
	}
}

func TestJournalBufferSignalSyntheticMalformedIsVisibility(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return journalBufferFixture(map[string]string{"coverage_present": "secret\nunknown=value"}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge", Roles: []string{"services"}}}
	alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if strings.Contains(alert.Markdown(), "secret") {
		t.Fatalf("raw malformed output leaked: %s", alert.Markdown())
	}
}

func TestJournalBufferSignalSyntheticFileHeadroomProblem(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
	}{
		{
			name: "retained-file-count",
			overrides: map[string]string{
				"journal_files": "400",
			},
		},
		{
			name: "rotation-rate",
			overrides: map[string]string{
				"journal_files": "100", "journal_archived_files_5m": "30",
				"journal_system_archived_files_5m": "22", "journal_user_archived_files_5m": "8",
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				return journalBufferFixture(testCase.overrides), nil
			}}
			settings := syntheticSettings(source)
			settings.Hosts = []HostSettings{{Name: "edge", Roles: []string{"services"}}}
			alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, "journal-buffer-file-headroom")
			if alert.Severity != SeverityWarn ||
				!strings.Contains(alert.Markdown(), "fourfold") ||
				!strings.Contains(alert.Observed, "system_max_files=1024") ||
				strings.Contains(alert.Markdown(), "system@") {
				t.Fatalf("file-headroom alert=%+v", alert)
			}
			if strings.Contains(strings.ToLower(alert.Markdown()), "hash") ||
				strings.Contains(alert.Mechanism, "high-cardinality") ||
				strings.Contains(alert.Action, "remove only proven log amplification") {
				t.Fatal("file-count evidence attributed a rotation cause or prescribed producer reduction")
			}
			if !strings.Contains(alert.Mechanism, "do not identify the rotation trigger") ||
				!strings.Contains(alert.Action, "Resolve the rotation trigger") {
				t.Fatal("file-count alert omitted the unresolved rotation-cause boundary")
			}
		})
	}
}

func TestJournalBufferFileHeadroomExactThreshold(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		files     string
		wantAlert bool
		wantPct   string
	}{
		{name: "exact-quarter-is-healthy", files: "256", wantPct: "current_file_capacity_pct=25.0"},
		{name: "first-file-over-quarter-alerts", files: "257", wantAlert: true, wantPct: "current_file_capacity_pct=25.1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sample, err := parseJournalBufferSample(journalBufferFixture(map[string]string{
				"journal_files": testCase.files,
			}))
			if err != nil {
				t.Fatal(err)
			}
			findings := evaluateJournalBuffer("edge", sample)
			var headroom *finding
			for i := range findings {
				if findings[i].class == "journal-buffer-file-headroom" && !findings[i].healthy {
					headroom = &findings[i]
				}
			}
			if (headroom != nil) != testCase.wantAlert {
				t.Fatalf("headroom alert present=%t, want %t: %+v", headroom != nil, testCase.wantAlert, findings)
			}
			if headroom != nil && !strings.Contains(headroom.observed, testCase.wantPct) {
				t.Fatalf("observed=%q, want %q", headroom.observed, testCase.wantPct)
			}
		})
	}
}

func TestJournalBufferSignalSyntheticFileScanUnavailableIsVisibility(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return journalBufferFixture(map[string]string{
			"journal_file_scan_state": "unavailable", "journal_files": "0", "journal_bytes": "0",
			"journal_archived_files_5m": "0", "journal_system_archived_files_5m": "0",
			"journal_user_archived_files_5m": "0",
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "edge", Roles: []string{"services"}}}
	alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if alert.Target != "edge/journal-buffer-files" {
		t.Fatalf("visibility target=%q", alert.Target)
	}
}

func TestJournalBufferSampleRejectsPreviousSchema(t *testing.T) {
	for _, schema := range []string{"3", "4", "5"} {
		_, err := parseJournalBufferSample(journalBufferFixture(map[string]string{"observation_schema": schema}))
		if err == nil || !strings.Contains(err.Error(), "unsupported observation schema") {
			t.Fatalf("schema %s: parse error=%v, want unsupported schema", schema, err)
		}
	}
}

func TestJournalBufferSignalSyntheticLowVolumeWholeFileLoss(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return journalBufferFixture(map[string]string{
			"storage": "auto", "max_file_sec": "1month",
			"coverage_present": "0", "boundary_entry_age_seconds": "900",
		}), nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{{Name: "low-volume-proxy", Roles: []string{"services"}}}
	alerts, err := NewJournalBufferSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	config := requireAlertClass(t, alerts, "journal-buffer-config")
	if !strings.Contains(config.Markdown(), "whole-file rotation") ||
		!strings.Contains(config.Evidence, "storage=auto") ||
		!strings.Contains(config.Evidence, "max_file_sec=1month") {
		t.Fatalf("configuration alert does not explain low-volume loss: %s", config.Markdown())
	}
	short := requireAlertClass(t, alerts, "journal-buffer-short")
	if !strings.Contains(short.Evidence, "age=900s, required=3000s") {
		t.Fatalf("coverage evidence=%q", short.Evidence)
	}
}

func TestJournalBufferCommandIsBoundedAndUsesEffectiveConfig(t *testing.T) {
	for _, want := range []string{
		"systemd-analyze cat-config systemd/journald.conf",
		"timeout 10s journalctl -q -b 0 --reverse -n 1",
		"--until '50 minutes ago' -n 1",
		"--output-fields=__REALTIME_TIMESTAMP -o json",
		"ActiveEnterTimestampMonotonic",
		"Storage", "SystemMaxUse", "SystemMaxFileSize", "SystemMaxFiles", "MaxFileSec", "MaxRetentionSec",
		"timeout 5s od -An -v -tu1 -N 34 /etc/machine-id",
		`journal_directory="/var/log/journal/$journal_machine_id"`,
		`timeout 10 find "$journal_directory" -xdev -mindepth 1 -maxdepth 1 -type f`,
		`\( -name '*.journal' -o -name '*.journal~' \)`,
		"journal_archived_files_5m", "journal_system_archived_files_5m", "journal_user_archived_files_5m",
	} {
		if !strings.Contains(journalBufferCommand, want) {
			t.Errorf("command lacks %q", want)
		}
	}
	for _, forbidden := range []string{"sudo", "--list-boots", "mktemp", ">/tmp", "> /tmp", "cat /var/log/journal", "set -o pipefail"} {
		if strings.Contains(journalBufferCommand, forbidden) {
			t.Errorf("command contains unsafe/unbounded boundary %q", forbidden)
		}
	}
}

func TestJournalBufferCommandReducesFileMetadataWithoutNames(t *testing.T) {
	const fileRows = "1999999990.0 20971520 system@aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-0000000000000001-0000000000000002.journal\n" +
		"1999999980.0 1024 user-1000@0000000000000003-0000000000000004.journal~\n" +
		"1999999970.0 512 system.journal\n" +
		"1999999990.0 1 private-prefix@bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-0000000000000005-0000000000000006.journal\n" +
		"1999999990.0 1 private-unfamiliar.journal\n" +
		"1999999990.0 1 system@malformed-archive.journal\n"
	output, err := runJournalBufferCommandWithClockAndFiles(t, "string", "string", "fixed", fileRows)
	if err != nil {
		t.Fatalf("file metadata reducer failed: %v\n%s", err, output)
	}
	sample, err := parseJournalBufferSample(output)
	if err != nil {
		t.Fatal(err)
	}
	if sample.journalFileScanState != "complete" || sample.journalFiles != 6 ||
		sample.journalBytes != 10738206208 || sample.journalArchivedFiles5m != 3 ||
		sample.journalSystemArchives5m != 1 || sample.journalUserArchives5m != 1 {
		t.Fatalf("file metadata sample=%+v", sample)
	}
	for _, private := range []string{"aaaaaaaa", "bbbbbbbb", "user-1000", "private-", "malformed-archive", journalBufferTestMachineID} {
		if strings.Contains(output, private) {
			t.Fatalf("file metadata reducer leaked %q: %s", private, output)
		}
	}
}

func TestJournalBufferCommandMalformedFileMetadataIsVisibility(t *testing.T) {
	const fileRows = "1999999990.0 2048 private unexpected name.journal\n"
	output, err := runJournalBufferCommandWithClockAndFiles(t, "string", "string", "fixed", fileRows)
	if err != nil {
		t.Fatalf("malformed file metadata must preserve other journal observations: %v\n%s", err, output)
	}
	sample, err := parseJournalBufferSample(output)
	if err != nil {
		t.Fatal(err)
	}
	if sample.journalFileScanState != "unavailable" {
		t.Fatalf("file scan state=%q, want unavailable", sample.journalFileScanState)
	}
	if strings.Contains(output, "private") {
		t.Fatalf("malformed file metadata leaked: %s", output)
	}
}

func TestJournalBufferCommandExcludesSiblingAndNestedDirectories(t *testing.T) {
	files := []string{
		journalBufferTestMachineID + "/system.journal",
		journalBufferTestMachineID + "/user-1000.journal",
		"system.journal",
		journalBufferTestMachineID + ".private-namespace/system.journal",
	}
	for i := 0; i < 300; i++ {
		files = append(files,
			fmt.Sprintf("99999999999999999999999999999999/user-%d.journal", i),
			fmt.Sprintf("%s/private-nested/user-%d.journal", journalBufferTestMachineID, i),
		)
	}
	output, err := runJournalBufferCommandWithFileFixture(t, "string", "string", "fixed", journalBufferFileFixture{
		machineID: journalBufferTestMachineID + "\n", files: files,
	})
	if err != nil {
		t.Fatalf("directory census failed: %v", err)
	}
	sample, err := parseJournalBufferSample(output)
	if err != nil {
		t.Fatal(err)
	}
	if sample.journalFileScanState != "complete" || sample.journalFiles != 2 || sample.journalBytes != 2048 {
		t.Fatalf("census included a sibling, nested directory, or non-regular file: %+v", sample)
	}
	for _, finding := range evaluateJournalBuffer("edge", sample) {
		if !finding.healthy {
			t.Fatalf("stale directory manufactured a finding: %s", finding.class)
		}
	}
	for _, private := range []string{journalBufferTestMachineID, "99999999", "private-", "user-1000"} {
		if strings.Contains(output, private) {
			t.Fatal("directory census leaked identity or a filename")
		}
	}
}

func TestJournalBufferCommandIncludesJournalTildeAtCapacityBoundary(t *testing.T) {
	files := []string{journalBufferTestMachineID + "/system@0000000000000001-0000000000000002.journal~"}
	for i := 0; i < 256; i++ {
		files = append(files, fmt.Sprintf("%s/user-%d.journal", journalBufferTestMachineID, i))
	}
	output, err := runJournalBufferCommandWithFileFixture(t, "string", "string", "fixed", journalBufferFileFixture{
		machineID: journalBufferTestMachineID + "\n", files: files,
	})
	if err != nil {
		t.Fatalf("directory census failed: %v", err)
	}
	sample, err := parseJournalBufferSample(output)
	if err != nil {
		t.Fatal(err)
	}
	if sample.journalFileScanState != "complete" || sample.journalFiles != 257 ||
		sample.journalArchivedFiles5m != 1 || sample.journalSystemArchives5m != 1 {
		t.Fatalf("census omitted a .journal~ file: %+v", sample)
	}
	for _, finding := range evaluateJournalBuffer("edge", sample) {
		if finding.class == "journal-buffer-file-headroom" && !finding.healthy {
			return
		}
	}
	t.Fatal(".journal~ file did not trigger the retained-file capacity boundary")
}

func TestJournalBufferCommandIdentityAndDirectoryVisibility(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		fixture journalBufferFileFixture
		want    string
	}{
		{name: "valid", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID + "\n"}, want: "complete"},
		{name: "valid-no-newline", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID}, want: "complete"},
		{name: "valid-uppercase-canonicalized", fixture: journalBufferFileFixture{machineID: strings.ToUpper(journalBufferTestMachineID)}, want: "complete"},
		{name: "missing-identity", fixture: journalBufferFileFixture{omitMachineID: true}},
		{name: "empty-identity", fixture: journalBufferFileFixture{}},
		{name: "null-identity", fixture: journalBufferFileFixture{machineID: strings.Repeat("0", 32)}},
		{name: "short-identity", fixture: journalBufferFileFixture{machineID: "0123456789abcdef"}},
		{name: "invalid-identity", fixture: journalBufferFileFixture{machineID: "../../private-machine-id-content"}},
		{name: "ambiguous-identity", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID + "\n" + journalBufferTestMachineID + "\n"}},
		{name: "extra-blank-line", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID + "\n\n"}},
		{name: "oversized-identity", fixture: journalBufferFileFixture{machineID: strings.Repeat(journalBufferTestMachineID, 10)}},
		{name: "missing-directory", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID, directoryState: "missing"}},
		{name: "symlink-directory", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID, directoryState: "symlink"}},
		{name: "not-a-directory", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID, directoryState: "file"}},
		{name: "incomplete-scan", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID, rows: "1999999990.0 2 system.journal\n", findExit: 1}},
		{name: "scan-timeout", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID, findExit: 124}},
		{name: "scan-file-bound", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID, rows: strings.Repeat("1999999990.0 2 system.journal\n", 4097)}, want: "truncated"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			output, err := runJournalBufferCommandWithFileFixture(t, "string", "string", "fixed", testCase.fixture)
			if err != nil {
				t.Fatalf("file visibility must preserve other journal observations: %v", err)
			}
			sample, err := parseJournalBufferSample(output)
			if err != nil {
				t.Fatal(err)
			}
			want := testCase.want
			if want == "" {
				want = "unavailable"
			}
			if sample.journalFileScanState != want || !sample.coveragePresent {
				t.Fatalf("file scan state=%s coverage=%t, want %s with coverage", sample.journalFileScanState, sample.coveragePresent, want)
			}
			if want != "complete" {
				if sample.journalFiles != 0 || sample.journalBytes != 0 || sample.journalArchivedFiles5m != 0 {
					t.Fatal("unknown scan retained partial file observations")
				}
				visibility := false
				for _, finding := range evaluateJournalBuffer("edge", sample) {
					if finding.class == "cannot-observe" && finding.target == "edge/journal-buffer-files" {
						visibility = true
					}
					if finding.class == "journal-buffer-file-headroom" {
						t.Fatal("unknown file census asserted a headroom result")
					}
				}
				if !visibility {
					t.Fatal("unknown file census omitted its visibility finding")
				}
			}
			for _, private := range []string{journalBufferTestMachineID, strings.ToUpper(journalBufferTestMachineID), "private-"} {
				if strings.Contains(output, private) {
					t.Fatal("identity or directory failure leaked private source data")
				}
			}
		})
	}
}

func TestJournalBufferCommandPOSIXProducerStatus(t *testing.T) {
	const partialRows = "1999999990.0 2 system.journal\n"
	for _, shellName := range []string{"sh", "dash"} {
		t.Run(shellName, func(t *testing.T) {
			shell, err := exec.LookPath(shellName)
			if err != nil {
				t.Skipf("%s is not installed", shellName)
			}
			for _, testCase := range []struct {
				name    string
				fixture journalBufferFileFixture
				want    string
				files   int
			}{
				{name: "empty-success", want: "complete"},
				{name: "files-success", fixture: journalBufferFileFixture{files: []string{
					journalBufferTestMachineID + "/system.journal",
					journalBufferTestMachineID + "/system.journal~",
				}}, want: "complete", files: 2},
				{name: "identity-without-newline", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID}, want: "complete"},
				{name: "identity-with-nul", fixture: journalBufferFileFixture{machineID: journalBufferTestMachineID + "\x00\n"}},
				{name: "identity-read-fails-after-valid-bytes", fixture: journalBufferFileFixture{machineIDReadExit: 1}},
				{name: "identity-read-times-out-after-valid-bytes", fixture: journalBufferFileFixture{machineIDReadExit: 124}},
				{name: "find-fails-after-valid-row", fixture: journalBufferFileFixture{rows: partialRows, findExit: 1}},
				{name: "find-times-out-after-valid-row", fixture: journalBufferFileFixture{rows: partialRows, findExit: 124}},
				{name: "identity-footer-missing", fixture: journalBufferFileFixture{machineIDFooterMode: "missing"}},
				{name: "identity-footer-duplicate", fixture: journalBufferFileFixture{machineIDFooterMode: "duplicate"}},
				{name: "identity-footer-truncated", fixture: journalBufferFileFixture{machineIDFooterMode: "truncated"}},
				{name: "identity-footer-nonterminal", fixture: journalBufferFileFixture{machineIDFooterMode: "nonterminal"}},
				{name: "find-footer-missing", fixture: journalBufferFileFixture{rows: partialRows, fileFooterMode: "missing"}},
				{name: "find-footer-duplicate", fixture: journalBufferFileFixture{rows: partialRows, fileFooterMode: "duplicate"}},
				{name: "find-footer-truncated", fixture: journalBufferFileFixture{rows: partialRows, fileFooterMode: "truncated"}},
				{name: "find-footer-nonterminal", fixture: journalBufferFileFixture{rows: partialRows, fileFooterMode: "nonterminal"}},
				{name: "file-bound-truncated", fixture: journalBufferFileFixture{rows: strings.Repeat(partialRows, 4097)}, want: "truncated"},
			} {
				t.Run(testCase.name, func(t *testing.T) {
					fixture := testCase.fixture
					fixture.shell = shell
					if fixture.machineID == "" {
						fixture.machineID = journalBufferTestMachineID + "\n"
					}
					output, err := runJournalBufferCommandWithFileFixture(t, "string", "string", "fixed", fixture)
					if err != nil {
						t.Fatalf("file census failure lost independent journal coverage: %v", err)
					}
					sample, err := parseJournalBufferSample(output)
					if err != nil {
						t.Fatal(err)
					}
					want := testCase.want
					if want == "" {
						want = "unavailable"
					}
					if sample.journalFileScanState != want || sample.journalFiles != testCase.files || !sample.coveragePresent {
						t.Fatalf("state=%s files=%d coverage=%t, want %s files=%d with coverage",
							sample.journalFileScanState, sample.journalFiles, sample.coveragePresent, want, testCase.files)
					}
					if want != "complete" {
						if sample.journalBytes != 0 || sample.journalArchivedFiles5m != 0 ||
							sample.journalSystemArchives5m != 0 || sample.journalUserArchives5m != 0 {
							t.Fatal("failed producer or footer retained partial census values")
						}
						visibility := false
						for _, finding := range evaluateJournalBuffer("edge", sample) {
							if finding.class == "cannot-observe" && finding.target == "edge/journal-buffer-files" {
								visibility = true
							}
							if finding.class == "journal-buffer-file-headroom" {
								t.Fatal("failed producer or footer asserted a headroom result")
							}
						}
						if !visibility {
							t.Fatal("failed producer or footer omitted its visibility finding")
						}
					}
					for _, private := range []string{journalBufferTestMachineID, "system.journal", "private-", "monitor-journal-"} {
						if strings.Contains(output, private) {
							t.Fatal("producer or footer exposed raw private input or internal protocol")
						}
					}
				})
			}
		})
	}
}

func TestJournalBufferCommandCrossSystemdCoverage(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		latestMode    string
		boundaryMode  string
		wantPresent   bool
		wantCommandOK bool
	}{
		{name: "systemd-249-string-timestamp", latestMode: "string", boundaryMode: "string", wantPresent: true, wantCommandOK: true},
		{name: "systemd-255-numeric-timestamp", latestMode: "numeric", boundaryMode: "numeric", wantPresent: true, wantCommandOK: true},
		{name: "genuinely-short", latestMode: "string", boundaryMode: "empty", wantPresent: false, wantCommandOK: true},
		{name: "latest-empty", latestMode: "empty", boundaryMode: "string"},
		{name: "latest-missing-timestamp", latestMode: "missing", boundaryMode: "string"},
		{name: "latest-malformed", latestMode: "malformed", boundaryMode: "string"},
		{name: "latest-future", latestMode: "future", boundaryMode: "string"},
		{name: "cutoff-missing-timestamp", latestMode: "string", boundaryMode: "missing"},
		{name: "cutoff-duplicate-timestamp", latestMode: "string", boundaryMode: "duplicate"},
		{name: "cutoff-malformed", latestMode: "string", boundaryMode: "malformed"},
		{name: "cutoff-too-young", latestMode: "string", boundaryMode: "young"},
		{name: "access-error", latestMode: "error", boundaryMode: "string"},
	} {
		output, err := runJournalBufferCommand(t, testCase.latestMode, testCase.boundaryMode)
		if !testCase.wantCommandOK {
			if err == nil {
				t.Fatalf(
					"%s: command succeeded with latest=%s boundary=%s:\n%s",
					testCase.name,
					testCase.latestMode,
					testCase.boundaryMode,
					output,
				)
			}
			if strings.Contains(output, "private-journal-detail") {
				t.Fatalf("%s: command leaked raw journal failure: %s", testCase.name, output)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: command failed: %v\n%s", testCase.name, err, output)
		}
		sample, err := parseJournalBufferSample(output)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if sample.coveragePresent != testCase.wantPresent {
			t.Fatalf(
				"%s: coverage_present=%t, want %t:\n%s",
				testCase.name,
				sample.coveragePresent,
				testCase.wantPresent,
				output,
			)
		}
		if testCase.wantPresent && sample.boundaryEntryAgeSeconds < 3000 {
			t.Fatalf(
				"%s: boundary age=%d, want >=3000",
				testCase.name,
				sample.boundaryEntryAgeSeconds,
			)
		}
	}
}

func TestJournalBufferCommandSystemd249PlainListBootsUsesEntryQuery(t *testing.T) {
	const systemd249ListBoots = " 0 redacted Fri 2026-09-04 19:45:54 UTC—Fri 2026-09-04 20:45:20 UTC\n"
	if strings.Contains(systemd249ListBoots, `"first_entry"`) {
		t.Fatal("systemd 249 fixture unexpectedly satisfies the retired JSON first_entry parser")
	}

	output, err := runJournalBufferCommand(t, "string", "string")
	if err != nil {
		t.Fatalf("entry-query command failed on systemd 249 fixture: %v\n%s", err, output)
	}
	sample, err := parseJournalBufferSample(output)
	if err != nil {
		t.Fatal(err)
	}
	if !sample.coveragePresent || sample.boundaryEntryAgeSeconds < 3000 {
		t.Fatalf("entry-query sample=%+v, want a valid cutoff witness", sample)
	}
}

func TestJournalBufferCommandValidatesAgainstPostQueryClock(t *testing.T) {
	// The two journal queries cross from T to T+1. A clock captured before the
	// reads rejects latest=T+1 as future and computes an exact cutoff as 2999s.
	// The post-query clock accepts both without weakening the future-row guard.
	output, err := runJournalBufferCommandWithClock(t, "present", "exact", "cross")
	if err != nil {
		t.Fatalf("valid rollover records were rejected: %v\n%s", err, output)
	}
	sample, err := parseJournalBufferSample(output)
	if err != nil {
		t.Fatal(err)
	}
	if !sample.coveragePresent || sample.boundaryEntryAgeSeconds != 3000 {
		t.Fatalf("rollover sample=%+v, want exact 3000-second coverage", sample)
	}

	output, err = runJournalBufferCommandWithClock(t, "future", "exact", "cross")
	if err == nil {
		t.Fatalf("timestamp newer than the post-query clock was accepted:\n%s", output)
	}
}

func runJournalBufferCommand(t *testing.T, latestMode, boundaryMode string) (string, error) {
	return runJournalBufferCommandWithClock(t, latestMode, boundaryMode, "fixed")
}

func runJournalBufferCommandWithClock(t *testing.T, latestMode, boundaryMode, clockMode string) (string, error) {
	return runJournalBufferCommandWithClockAndFiles(t, latestMode, boundaryMode, clockMode, "")
}

// A prior suspend increases /proc/uptime but not systemd's activation clock.
// A newly restarted journal still needs its own complete refill grace.
func TestJournalBufferRestartGraceUsesMonotonicClockAfterSuspend(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		startMonotonicUS string
		wantAge          int
		wantChecked      bool
	}{
		{name: "recent restart after prior suspend", startMonotonicUS: "99900000000", wantAge: 100},
		{name: "one second before grace", startMonotonicUS: "95801000000", wantAge: 4199},
		{name: "one microsecond before grace", startMonotonicUS: "95800000001", wantAge: 4199},
		{name: "exact grace", startMonotonicUS: "95800000000", wantAge: 4200, wantChecked: true},
		{name: "one second after grace", startMonotonicUS: "95799000000", wantAge: 4201, wantChecked: true},
	} {
		output, err := runJournalBufferCommandWithFileFixture(t, "string", "empty", "fixed", journalBufferFileFixture{
			machineID: journalBufferTestMachineID, uptimeSeconds: "169000",
			monotonicNowUS: "100000000000", journaldStartMonotonicUS: testCase.startMonotonicUS,
		})
		if err != nil {
			t.Fatalf("%s: reducer failed: %v\n%s", testCase.name, err, output)
		}
		sample, err := parseJournalBufferSample(output)
		if err != nil {
			t.Fatalf("%s: observation did not parse: %v", testCase.name, err)
		}
		if sample.journaldActiveSeconds != testCase.wantAge || sample.coverageChecked != testCase.wantChecked || sample.coveragePresent {
			t.Fatalf("%s: age=%d checked=%t present=%t, want age=%d checked=%t with no boundary",
				testCase.name, sample.journaldActiveSeconds, sample.coverageChecked, sample.coveragePresent,
				testCase.wantAge, testCase.wantChecked)
		}
		short := false
		for _, finding := range evaluateJournalBuffer("synthetic-edge", sample) {
			short = short || finding.class == "journal-buffer-short" && !finding.healthy
		}
		if short != testCase.wantChecked {
			t.Fatalf("%s: short-buffer finding=%t, want %t", testCase.name, short, testCase.wantChecked)
		}
	}
}

func TestJournalBufferRestartAgeClockFailureIsNotCoverageEvidence(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		fixture journalBufferFileFixture
	}{
		{name: "clock failure", fixture: journalBufferFileFixture{monotonicClockExit: 1}},
		{name: "clock timeout", fixture: journalBufferFileFixture{monotonicClockExit: 124}},
		{name: "malformed clock", fixture: journalBufferFileFixture{monotonicNowUS: "private-clock-detail"}},
		{name: "negative clock", fixture: journalBufferFileFixture{monotonicNowUS: "-1"}},
		{name: "zero clock", fixture: journalBufferFileFixture{monotonicNowUS: "0"}},
		{name: "oversized clock", fixture: journalBufferFileFixture{monotonicNowUS: "10000000000000000"}},
		{name: "malformed activation", fixture: journalBufferFileFixture{journaldStartMonotonicUS: "private-start-detail"}},
		{name: "zero activation", fixture: journalBufferFileFixture{journaldStartMonotonicUS: "0"}},
		{name: "oversized activation", fixture: journalBufferFileFixture{journaldStartMonotonicUS: "10000000000000000"}},
		{name: "activation one microsecond in future", fixture: journalBufferFileFixture{journaldStartMonotonicUS: "100000000001"}},
	} {
		output, err := runJournalBufferCommandWithFileFixture(t, "string", "empty", "fixed", testCase.fixture)
		if err == nil || strings.Contains(output, "observation_schema=") || strings.Contains(output, "private-") {
			t.Fatalf("%s: unavailable clock emitted evidence or leaked source: exit=%v bytes=%d", testCase.name, err, len(output))
		}
	}
}

const journalBufferTestMachineID = "0123456789abcdef0123456789abcdef"

type journalBufferFileFixture struct {
	machineID                string
	omitMachineID            bool
	machineIDReadExit        int
	machineIDFooterMode      string
	fileFooterMode           string
	directoryState           string
	files                    []string
	rows                     string
	findExit                 int
	shell                    string
	uptimeSeconds            string
	journaldStartMonotonicUS string
	monotonicNowUS           string
	monotonicClockExit       int
}

func runJournalBufferCommandWithClockAndFiles(t *testing.T, latestMode, boundaryMode, clockMode, fileRows string) (string, error) {
	return runJournalBufferCommandWithFileFixture(t, latestMode, boundaryMode, clockMode, journalBufferFileFixture{
		machineID: journalBufferTestMachineID + "\n", rows: fileRows,
	})
}

func runJournalBufferCommandWithFileFixture(t *testing.T, latestMode, boundaryMode, clockMode string, files journalBufferFileFixture) (string, error) {
	t.Helper()
	bin := t.TempDir()
	writeExecutable := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable("systemctl", "#!/bin/sh\n"+journalVacuumUnitFixture+`
case "$1" in
  is-active) echo active ;;
  show) printf '%s\n' "${JOURNALD_START_MONOTONIC_US:-95800000000}" ;;
  *) exit 1 ;;
esac
`)
	writeExecutable("systemd-analyze", `#!/bin/sh
cat <<'EOF'
[Journal]
Storage=persistent
SystemMaxUse=100G
SystemMaxFileSize=256M
SystemMaxFiles=1024
MaxFileSec=5min
EOF
`)
	writeExecutable("sha256sum", "#!/bin/sh\n"+journalVacuumHashFixture)
	writeExecutable("dpkg-query", "#!/bin/sh\nprintf '%s' unverified\n")
	writeExecutable("python3", `#!/bin/sh
[ "${JOURNAL_MONOTONIC_CLOCK_STATUS:-0}" -eq 0 ] || exit "$JOURNAL_MONOTONIC_CLOCK_STATUS"
printf '%s\n' "${JOURNAL_MONOTONIC_NOW_US:-100000000000}"
`)
	writeExecutable("awk", `#!/bin/sh
case "$*" in
  *'/proc/uptime'*)
    case "$*" in *1000000*) printf '%s000000\n' "${JOURNAL_UPTIME_SECONDS:-100000}" ;; *) printf '%s\n' "${JOURNAL_UPTIME_SECONDS:-100000}" ;; esac
    exit 0
    ;;
esac
exec /usr/bin/awk "$@"
`)
	writeExecutable("timeout", `#!/bin/sh
shift
exec "$@"
`)
	writeExecutable("od", `#!/bin/sh
/usr/bin/od "$@"
journal_test_read_status=$?
[ "$journal_test_read_status" -eq 0 ] || exit "$journal_test_read_status"
if [ "$JOURNAL_MACHINE_ID_READ_STATUS" -ne 0 ]; then
  echo 'private-machine-id-read-error' >&2
fi
exit "$JOURNAL_MACHINE_ID_READ_STATUS"
`)
	journalRoot := filepath.Join(bin, "journal")
	if err := os.Mkdir(journalRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	machineIDPath := filepath.Join(bin, "machine-id")
	if !files.omitMachineID {
		if err := os.WriteFile(machineIDPath, []byte(files.machineID), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	journalDirectory := filepath.Join(journalRoot, journalBufferTestMachineID)
	switch files.directoryState {
	case "missing":
	case "symlink":
		if err := os.Symlink(journalRoot, journalDirectory); err != nil {
			t.Fatal(err)
		}
	case "file":
		if err := os.WriteFile(journalDirectory, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		if err := os.Mkdir(journalDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		// Journald ignores directories and symlinks even with journal suffixes.
		if err := os.Mkdir(filepath.Join(journalDirectory, "private-directory.journal"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(machineIDPath, filepath.Join(journalDirectory, "private-link.journal~")); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range files.files {
		path := filepath.Join(journalRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	realFind, err := exec.LookPath("find")
	if err != nil {
		t.Fatal(err)
	}
	// Native find executes the production path/depth/type/name selection. Only
	// GNU -printf is adapted for macOS, with deterministic synthetic metadata.
	writeExecutable("find", `#!/bin/bash
if [ -n "$JOURNAL_FIND_OUTPUT" ] || [ "$JOURNAL_FIND_STATUS" -ne 0 ]; then
  printf '%s' "$JOURNAL_FIND_OUTPUT"
  if [ "$JOURNAL_FIND_STATUS" -ne 0 ]; then
    echo 'private-directory-visibility-error' >&2
  fi
  exit "$JOURNAL_FIND_STATUS"
fi
find_args=()
while [ "$#" -gt 0 ]; do
  if [ "$1" = -printf ]; then
    [ "$2" = '%T@ %b %f\n' ] || exit 1
    find_args+=(-exec "$JOURNAL_FIND_METADATA" '{}' +)
    shift 2
  else
    find_args+=("$1")
    shift
  fi
done
exec "$JOURNAL_REAL_FIND" "${find_args[@]}"
`)
	writeExecutable("journal-file-metadata", `#!/bin/sh
for journal_path do
  printf '1999999990.0 2 %s\n' "${journal_path##*/}"
done
`)
	commandSource := strings.ReplaceAll(journalBufferCommand, "/var/log/journal", journalRoot)
	commandSource = strings.ReplaceAll(commandSource, "/etc/machine-id", machineIDPath)
	// Inject only status-footer write faults; all producer commands and actual
	// production reducers still execute in the selected account-shell fixture.
	commandSource = `printf() {
  journal_test_footer_mode=
  case "$1" in
    'monitor-journal-machine-id-status=%d\n') journal_test_footer_mode=$JOURNAL_MACHINE_ID_FOOTER_MODE ;;
    'monitor-journal-file-scan-status=%d\n') journal_test_footer_mode=$JOURNAL_FILE_FOOTER_MODE ;;
  esac
  case "$journal_test_footer_mode" in
    missing) return 1 ;;
    duplicate) command printf "$@" ;;
    truncated)
      case "$1" in
        'monitor-journal-machine-id-status=%d\n') command printf 'monitor-journal-machine-id-status=' ;;
        *) command printf 'monitor-journal-file-scan-status=' ;;
      esac
      return 1
      ;;
    nonterminal)
      command printf "$@"
      command printf 'private-after-terminal-footer\n'
      return 0
      ;;
  esac
  command printf "$@"
}
` + commandSource
	// The command and its fake journalctl child must share one clock. Sampling
	// the real clock twice made a loaded test host manufacture a future latest
	// record when process startup crossed more than one second. Cross mode makes
	// the query advance that shared clock once, deterministically reproducing
	// the production second-boundary race.
	writeExecutable("date", `#!/bin/sh
if [ "$#" -eq 1 ] && [ "$1" = +%s ]; then
  if [ "$JOURNAL_CLOCK_MODE" = cross ] && [ -e "$JOURNAL_CLOCK_STATE" ]; then
    echo 2000000001
  else
    echo 2000000000
  fi
  exit 0
fi
exec /bin/date "$@"
`)
	writeExecutable("journalctl", `#!/bin/sh
case " $* " in
  *' --grep='*) exit 1 ;;
  *' --list-boots '*)
    echo ' 0 redacted Fri 2026-09-04 19:45:54 UTC—Fri 2026-09-04 20:45:20 UTC'
    exit 0
    ;;
esac
mode=$JOURNAL_LATEST_MODE
age=1
case " $* " in
  *' --until '*) mode=$JOURNAL_BOUNDARY_MODE; age=3300 ;;
esac
if [ "$JOURNAL_CLOCK_MODE" = cross ]; then
  touch "$JOURNAL_CLOCK_STATE"
fi
now=$(date +%s)
case "$mode" in
  present) age=0 ;;
  exact) age=3000 ;;
esac
timestamp=$(( (now - age) * 1000000 ))
case "$mode" in
  string|present|exact) printf '{"__REALTIME_TIMESTAMP":"%s"}\n' "$timestamp" ;;
  numeric) printf '{"__REALTIME_TIMESTAMP":%s}\n' "$timestamp" ;;
  future) printf '{"__REALTIME_TIMESTAMP":"%s"}\n' "$(( (now + 1) * 1000000 ))" ;;
  young) printf '{"__REALTIME_TIMESTAMP":"%s"}\n' "$(( (now - 2999) * 1000000 ))" ;;
  empty) : ;;
  missing) echo '{"MESSAGE":"private-journal-detail"}' ;;
  duplicate) printf '{"__REALTIME_TIMESTAMP":"%s","__REALTIME_TIMESTAMP":"%s"}\n' "$timestamp" "$timestamp" ;;
  malformed) echo 'private-journal-detail' ;;
  error) echo 'private-journal-detail' >&2; exit 1 ;;
  *) exit 2 ;;
esac
`)

	shell := files.shell
	if shell == "" {
		shell = "sh"
	}
	command := exec.Command(shell, "-c", commandSource)
	command.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"),
		"JOURNAL_LATEST_MODE="+latestMode,
		"JOURNAL_BOUNDARY_MODE="+boundaryMode,
		"JOURNAL_CLOCK_MODE="+clockMode,
		"JOURNAL_UPTIME_SECONDS="+files.uptimeSeconds,
		"JOURNALD_START_MONOTONIC_US="+files.journaldStartMonotonicUS,
		"JOURNAL_MONOTONIC_NOW_US="+files.monotonicNowUS,
		fmt.Sprintf("JOURNAL_MONOTONIC_CLOCK_STATUS=%d", files.monotonicClockExit),
		"JOURNAL_CLOCK_STATE="+filepath.Join(bin, "query-seen"),
		"JOURNAL_FIND_OUTPUT="+files.rows,
		fmt.Sprintf("JOURNAL_FIND_STATUS=%d", files.findExit),
		fmt.Sprintf("JOURNAL_MACHINE_ID_READ_STATUS=%d", files.machineIDReadExit),
		"JOURNAL_MACHINE_ID_FOOTER_MODE="+files.machineIDFooterMode,
		"JOURNAL_FILE_FOOTER_MODE="+files.fileFooterMode,
		"JOURNAL_REAL_FIND="+realFind,
		"JOURNAL_FIND_METADATA="+filepath.Join(bin, "journal-file-metadata"),
	)
	output, err := command.CombinedOutput()
	return string(output), err
}
