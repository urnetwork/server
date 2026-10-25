// Capacity owner labels are private grouping inputs, including on healthy
// observations that log evidence before deciding whether an alert is needed.
package monitor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Each arbitrary label column independently crosses the public Run boundary.
func TestPgCapacityOwnerLabelsStayPrivateInPublicOutputs(t *testing.T) {
	for _, column := range []int{1, 2, 4, 6} {
		marker := "synthetic-private-capacity-label-" + strconv.Itoa(column)
		for _, total := range []string{"800", "940"} {
			rows := syntheticPgCapacityRows(total, "230", "540", "7")
			rows[1][column] = marker
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
			alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
			if err != nil || len(alerts) != 1 {
				t.Fatalf("column %d total %s: capacity observation failed: err=%v alerts=%d", column, total, err, len(alerts))
			}
			alert := alerts[0]
			severity, sustain := SeverityWarn, 2
			if total == "940" {
				severity, sustain = SeverityPage, 1
			}
			if alert.Class != "pg-client-capacity" || alert.Severity != severity || alert.Sustain != sustain {
				t.Fatalf("column %d total %s: capacity identity or threshold changed", column, total)
			}
			var jsonl strings.Builder
			if err := WriteAlertsJSONL(&jsonl, alerts); err != nil {
				t.Fatal(err)
			}
			for _, output := range []string{alert.Evidence, alert.Markdown(), jsonl.String()} {
				if strings.Contains(output, marker) {
					t.Errorf("column %d total %s: private label reached public capacity output", column, total)
				}
			}
		}
	}
}

// A subprocess captures the actual glog sink without mutating process-global
// logger configuration or depending on another test's logging flags.
func TestPgCapacityOwnerLabelsStayPrivateDuringHealthyLogging(t *testing.T) {
	const childEnv = "FACTURY_CAPACITY_HEALTHY_LOG_CONTROL"
	const marker = "synthetic-private-healthy-capacity-label"
	if os.Getenv(childEnv) == "1" {
		rows := syntheticPgCapacityRows("400", "31", "340", "7")
		for _, column := range []int{1, 2, 4, 6} {
			rows[1][column] = marker
		}
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return rows, nil }}
		alerts, err := NewPgCapacitySignal().Run(context.Background(), syntheticSettings(source))
		if err != nil || len(alerts) != 0 {
			t.Fatal("healthy logging control did not complete a healthy synthetic observation")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPgCapacityOwnerLabelsStayPrivateDuringHealthyLogging$", "-test.count=1", "-logtostderr=true")
	command.Env = append(os.Environ(), childEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("healthy capacity logging child failed")
	}
	if !strings.Contains(string(output), "PostgreSQL client capacity owners on pg-1") {
		t.Fatal("healthy control did not observe the actual capacity log")
	}
	if strings.Contains(string(output), marker) {
		t.Fatal("private capacity label reached healthy glog output")
	}
}

// Opaque display labels preserve exact private owner aggregation, ranking and
// wait distinctions while disclosing neither labels nor stable fingerprints.
func TestPgCapacityPrivateGroupsPreserveOwnerAggregation(t *testing.T) {
	rows := []pgRow{
		{"summary", "1024", "3", "0", "1021", "800", "230", "540", "7", "4MB", "128MB"},
		{"owner", "private-application-a", "private-role-a", "192.0.2.31", "idle", "180", "private-wait-a", "540", "3500", "", ""},
		{"owner", "private-application-a", "private-role-a", "2001:db8:31::/64", "idle", "120", "private-wait-b", "600", "3550", "", ""},
		{"owner", "private-application-b", "private-role-a", "192.0.2.31", "active", "90", "private-wait-a", "3", "1200", "", ""},
	}
	for i := range 12 {
		rows = append(rows, pgRow{"owner", fmt.Sprintf("private-extra-application-%d", i), "private-extra-role", "198.51.100.32", "idle", "1", "private-extra-wait", "1", "2", "", ""})
	}
	settings := syntheticSettings(nil)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "source.example", LANAddress: "192.0.2.31", OverlayAddress: "2001:db8:31::"})
	cfg := configFromSignalSettings(settings)
	_, owners, err := parsePgCapacityRows(rows, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 10 || owners[0].clients != 300 || owners[0].oldestState != 600 || owners[0].oldestAge != 3550 || owners[0].clientOwner != "source.example" || owners[1].clients != 90 {
		t.Fatal("private display projection changed owner merging, top ten, ranking or ages")
	}
	evidence := pgCapacityEvidence(owners)
	for _, want := range []string{"application_group=1 role_group=1 client_owner=source.example state=idle clients=300 wait_group=1 oldest_state_s=600 oldest_backend_s=3550", "application_group=2 role_group=1 client_owner=source.example state=active clients=90 wait_group=2"} {
		if !strings.Contains(evidence, want) {
			t.Errorf("private grouping lost its safe owner distinction: %s", want)
		}
	}
	for _, private := range []string{"private-application", "private-role", "private-wait", "private-extra", "192.0.2.31", "2001:db8:31::", "198.51.100.32"} {
		if strings.Contains(evidence, private) {
			t.Error("private owner grouping input reached evidence")
		}
	}
}

// Slack's existing transition, digest and summary contracts exclude Evidence.
func TestPgCapacitySlackBodiesContinueToExcludeEvidence(t *testing.T) {
	const marker = "synthetic-private-capacity-evidence"
	alert := Alert{SignalID: "pg/client-capacity", Class: "pg-client-capacity", Target: "pg-1", Evidence: marker, Severity: SeverityWarn}
	transition := slackTransition{kind: slackRaised, alert: alert}
	notifier := &SlackAlertNotifier{
		settings: DefaultSlackAlertNotifierSettings(), suppressedCount: 1,
		suppressed: []slackTransition{transition}, startDigest: []slackTransition{transition},
	}
	for _, output := range []string{slackTransitionText("synthetic", transition, 2), notifier.startDigestTextWithLock(), notifier.summaryTextWithLock()} {
		if strings.Contains(output, marker) || !strings.Contains(output, "pg-client-capacity") {
			t.Fatal("Slack capacity body changed its evidence exclusion or identity")
		}
	}
}
