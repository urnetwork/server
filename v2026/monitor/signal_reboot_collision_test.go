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

func TestRebootCollisionSignalSyntheticScheduledRebootExcludesFinishedTask(t *testing.T) {
	interruptedID := "01a0558c-3405-5671-1f3d-7429f4dd08f7"
	finishedID := "01a05555-97e8-e794-e009-04721c586db9"
	journal := func(timestamp, taskID, taskName, duration string) string {
		return `{"SYSLOG_TIMESTAMP":"` + timestamp + `","MESSAGE":"I0831 02:09:40.000000 1 task.go:1938] [` + taskID + `]eval active(` + duration + `s) github.com/urnetwork/server/taskworker/work.` + taskName + `({})","CONTAINER_TAG":"warp|synthetic|taskworker|g2","CONTAINER_ID":"closeworker1","_HOSTNAME":"edge-0"}`
	}
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			for _, want := range []string{
				"task_id = '" + interruptedID + "'",
				"task_id = '" + finishedID + "'",
				"run_end_time <= to_timestamp(1788142205)",
			} {
				if !strings.Contains(query, want) {
					t.Fatalf("finished-task discriminator lost %q: %s", want, query)
				}
			}
			return []Row{{finishedID}}, nil
		},
		hostTimeoutFn: func(host HostSettings, command string, timeout time.Duration) (string, error) {
			if timeout != rebootCollisionCommandTimeout {
				t.Fatalf("reboot battery timeout = %s, want %s", timeout, rebootCollisionCommandTimeout)
			}
			for _, want := range []string{
				"by-restart.service",
				"-b -1 -o json",
				"-n 5001",
				"warp|synthetic|taskworker|g2",
			} {
				if !strings.Contains(command, want) {
					t.Fatalf("reboot battery lost %q: %s", want, command)
				}
			}
			if host.Name == "edge-1" {
				return "monitor_boot_epoch=1788138000\nmonitor_boot_age_s=3600\n" + rebootCollisionTestCompleteMarker, nil
			}
			return strings.Join([]string{
				"monitor_boot_epoch=1788142395",
				"monitor_boot_age_s=300",
				rebootPreviousEndMarker,
				`{"SYSLOG_TIMESTAMP":"2026-08-31T02:10:05.172966Z","MESSAGE":"Journal stopped"}`,
				rebootPreviousEndMarker + "_status=0",
				rebootCauseMarker,
				`{"MESSAGE":"Starting Scheduled Reboot Service..."}`,
				rebootCauseMarker + "_status=0",
				rebootTaskLogsMarker,
				journal("2026-08-31T02:09:51.757953Z", interruptedID, "CloseExpiredContracts", "541.17"),
				journal("2026-08-31T02:09:45.000000Z", finishedID, "UpdateClientScores", "3565.74"),
				rebootTaskLogsMarker + "_status=0",
				rebootCollisionTestCompleteMarker,
			}, "\n"), nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "edge-0", Roles: []string{"services"}},
		HostSettings{Name: "edge-1", Roles: []string{"services"}},
	)

	alerts, err := NewRebootCollisionSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "reboot-task-collision")
	for _, want := range []string{
		"edge-0 rebooted with 1 taskworker task",
		"reboot_source=by-restart.timer",
		"CloseExpiredContracts:541s",
		"g2/closeworker1",
		"Scheduled Reboot Service observed",
		"finished_task cross-check excludes work",
		"Do not disable the fleet reboot policy ad hoc",
		"reclaimed and reaches a real terminal result",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("reboot collision alert lost %q:\n%s", want, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, interruptedID, finishedID)
	if strings.Contains(alert.Markdown(), "UpdateClientScores") {
		t.Fatalf("task completed before shutdown was misclassified:\n%s", alert.Markdown())
	}
}

const (
	rebootCollisionTestTaskId         = "00000000-0000-4000-8000-000000000001"
	rebootCollisionTestCompleteMarker = "monitor_reboot_complete"
)

// All command inputs are local synthetic files. No real journal, clock,
// process metadata, database, or host transport is consulted.
type rebootCollisionCommandFixture struct {
	tasks          string
	taskStatus     int
	cause          string
	causeStatus    int
	boundaryStatus int
	missingBoot    bool
	oldBoot        bool
	finished       bool
	mutate         func(string) string
}

// Build JSON with the same typed fields used by the production normalizer.
func rebootCollisionTestJournal(t *testing.T, observedAt, message string) string {
	t.Helper()
	data, err := json.Marshal(taskworkerJournalEntry{
		Timestamp:    observedAt,
		Message:      message,
		ContainerTag: "warp|synthetic|taskworker|g2",
		ContainerID:  "synthetic-taskworker",
		Hostname:     "synthetic-edge.example",
	})
	if err != nil {
		t.Fatal("could not encode synthetic journal entry")
	}
	return string(data)
}

// Run the actual generated shell producer with a private PATH containing
// only stubs. Each inner query independently controls its output and exit.
func rebootCollisionCommandFindings(t *testing.T, fixture rebootCollisionCommandFixture) []finding {
	t.Helper()
	directory := t.TempDir()
	boundary := `{"SYSLOG_TIMESTAMP":"2026-08-31T02:10:05Z","MESSAGE":"Journal stopped"}`
	if fixture.missingBoot {
		boundary = ""
	}
	for name, body := range map[string]string{
		"boundary.jsonl": boundary,
		"cause.jsonl":    fixture.cause,
		"tasks.jsonl":    fixture.tasks,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body+"\n"), 0600); err != nil {
			t.Fatal("could not write synthetic journal input")
		}
	}
	journalScript := `#!/bin/sh
kind=boundary
limit=0
grep_filter=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    -u) kind=cause; shift 2 ;;
    -t) kind=tasks; shift 2 ;;
    -n) limit=$2; shift 2 ;;
    --grep=*) grep_filter=1; shift ;;
    *) shift ;;
  esac
done
printf '%s %s %s\n' "$kind" "$limit" "$grep_filter" >> "$REBOOT_TEST_DIRECTORY/calls"
case "$kind" in
  boundary) status=$REBOOT_TEST_BOUNDARY_STATUS ;;
  cause) status=$REBOOT_TEST_CAUSE_STATUS ;;
  tasks) status=$REBOOT_TEST_TASK_STATUS ;;
esac
count=0
while IFS= read -r line; do
  if [ -z "$line" ]; then continue; fi
  if [ "$grep_filter" -eq 1 ]; then
    case "$line" in
      *'eval active('*|*'eval done('*|*'eval error('*) ;;
      *) continue ;;
    esac
  fi
  if [ "$count" -ge "$limit" ]; then break; fi
  printf '%s\n' "$line"
  count=$((count + 1))
done < "$REBOOT_TEST_DIRECTORY/$kind.jsonl"
printf 'synthetic-private-stderr\n' >&2
if [ "$status" -eq 0 ] && [ "$grep_filter" -eq 1 ] && [ "$count" -eq 0 ]; then
  exit 1
fi
exit "$status"
`
	for name, script := range map[string]string{
		"awk":        "#!/bin/sh\nprintf '1788142395\\n'\n",
		"date":       "#!/bin/sh\nprintf '%s\\n' \"$REBOOT_TEST_NOW\"\n",
		"journalctl": journalScript,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal("could not write synthetic journal command")
		}
	}
	now := int64(1788142695)
	if fixture.oldBoot {
		now += 3600
	}
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if !strings.Contains(query, "run_end_time <= to_timestamp(1788142205)") {
				t.Error("finished-task lookup lost the previous-boot boundary")
			}
			if fixture.finished {
				return []Row{{rebootCollisionTestTaskId}}, nil
			}
			return nil, nil
		},
		hostTimeoutFn: func(_ HostSettings, command string, timeout time.Duration) (string, error) {
			if timeout != rebootCollisionCommandTimeout {
				t.Error("reboot command lost its existing timeout")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			process := exec.CommandContext(ctx, "/bin/sh", "-c", command)
			process.Env = []string{
				"PATH=" + directory, "LC_ALL=C",
				"REBOOT_TEST_DIRECTORY=" + directory,
				"REBOOT_TEST_NOW=" + strconv.FormatInt(now, 10),
				"REBOOT_TEST_BOUNDARY_STATUS=" + strconv.Itoa(fixture.boundaryStatus),
				"REBOOT_TEST_CAUSE_STATUS=" + strconv.Itoa(fixture.causeStatus),
				"REBOOT_TEST_TASK_STATUS=" + strconv.Itoa(fixture.taskStatus),
			}
			output, err := process.CombinedOutput()
			if err != nil {
				t.Error("synthetic reboot producer did not complete")
				return "", fmt.Errorf("synthetic reboot command failed")
			}
			if strings.Contains(string(output), "synthetic-private-stderr") {
				t.Error("journal diagnostic escaped its private source channel")
			}
			raw := string(output)
			if fixture.mutate != nil {
				raw = fixture.mutate(raw)
			}
			return raw, nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = []HostSettings{
		{Name: "synthetic-pg.example", Roles: []string{"pg-primary"}},
		{Name: "synthetic-edge.example", Roles: []string{"services"}},
	}
	settings = settings.withDefaults()
	if err := settings.validate(); err != nil {
		t.Fatal("invalid synthetic reboot settings")
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal("could not construct synthetic reboot environment")
	}
	findings, err := (rebootCollisionProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal("synthetic reboot probe failed")
	}
	calls, err := os.ReadFile(filepath.Join(directory, "calls"))
	if fixture.oldBoot {
		if !os.IsNotExist(err) {
			t.Error("old boot unexpectedly invoked journalctl")
		}
	} else if err != nil || len(strings.Split(strings.TrimSpace(string(calls)), "\n")) != 3 {
		t.Error("reboot producer changed the three-query acquisition bound")
	}
	for _, result := range findings {
		alert := alertFromFinding(settings, "2.13", "reboot-collision", "Host reboot and active-task collision", result)
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal("could not encode synthetic reboot alert")
		}
		for _, sentinel := range []string{"synthetic-private", rebootCollisionTestTaskId} {
			if strings.Contains(string(encoded), sentinel) || strings.Contains(alert.Markdown(), sentinel) {
				t.Error("reboot alert exposed a private source value")
			}
		}
	}
	return findings
}

// Assert health directly; an empty public alert list alone does not prove
// that an incomplete observation cannot resolve an existing collision.
func requireRebootCollisionFindings(t *testing.T, findings []finding, healthy, collision, unknown bool) {
	t.Helper()
	var gotHealthy, gotCollision, gotUnknown bool
	for _, result := range findings {
		gotHealthy = gotHealthy || result.healthy
		gotCollision = gotCollision || result.class == "reboot-task-collision" && !result.healthy
		gotUnknown = gotUnknown || result.class == "cannot-observe"
	}
	if gotHealthy != healthy || gotCollision != collision || gotUnknown != unknown {
		t.Errorf("healthy=%t collision=%t unknown=%t; want %t %t %t", gotHealthy, gotCollision, gotUnknown, healthy, collision, unknown)
	}
}

// Valid boundary plus an empty failed task query was previously healthy.
func TestRebootCollisionFailedEmptyTaskJournalCannotEstablishHealth(t *testing.T) {
	for _, status := range []int{1, 2, 124, 127} {
		findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{taskStatus: status})
		requireRebootCollisionFindings(t, findings, false, false, true)
	}
}

// A valid prefix cannot prove absence of a later terminal line, nor can a
// terminal-only prefix prove the absence of other interrupted attempts.
func TestRebootCollisionFailedPartialTaskJournalCannotEstablishHealthOrCollision(t *testing.T) {
	for _, state := range []string{"active", "done"} {
		line := rebootCollisionTestJournal(t, "2026-08-31T02:09:51Z", "["+rebootCollisionTestTaskId+"]eval "+state+"(541.17s) github.com/urnetwork/server/taskworker/work.SyntheticTask({})")
		for _, status := range []int{1, 124} {
			findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{tasks: line, taskStatus: status})
			requireRebootCollisionFindings(t, findings, false, false, true)
		}
	}
}

// The finite sentinel distinguishes exactly 5,000 complete records from a
// saturated read; do not rerun with a larger or unbounded journal query.
func TestRebootCollisionTaskJournalSaturationCannotEstablishHealth(t *testing.T) {
	line := rebootCollisionTestJournal(t, "2026-08-31T02:10:00Z", "["+rebootCollisionTestTaskId+"]eval done(541.17s) github.com/urnetwork/server/taskworker/work.SyntheticTask({})")
	for _, count := range []int{5000, 5001, 5010} {
		findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{tasks: strings.Repeat(line+"\n", count)})
		requireRebootCollisionFindings(t, findings, count == 5000, false, count != 5000)
	}
}

// A successful empty tag window is healthy; missing previous-boot authority
// remains unknown even when the task query itself is empty and successful.
func TestRebootCollisionSuccessfulEmptyJournalAndMissingBootAreDistinct(t *testing.T) {
	findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{})
	requireRebootCollisionFindings(t, findings, true, false, false)
	for _, fixture := range []rebootCollisionCommandFixture{
		{missingBoot: true, boundaryStatus: 1},
		{missingBoot: true},
		{boundaryStatus: 1},
	} {
		findings = rebootCollisionCommandFindings(t, fixture)
		requireRebootCollisionFindings(t, findings, false, false, true)
	}
}

// Complete task evidence still detects real collisions and respects exact
// finished-row and terminal-log suppression through the actual producer.
func TestRebootCollisionCompleteTaskJournalRetainsLifecycleDiscriminators(t *testing.T) {
	active := rebootCollisionTestJournal(t, "2026-08-31T02:09:51Z", "["+rebootCollisionTestTaskId+"]eval active(541.17s) github.com/urnetwork/server/taskworker/work.SyntheticTask({})")
	done := rebootCollisionTestJournal(t, "2026-08-31T02:10:00Z", "["+rebootCollisionTestTaskId+"]eval done(550s) github.com/urnetwork/server/taskworker/work.SyntheticTask({})")
	for _, fixture := range []rebootCollisionCommandFixture{
		{tasks: active},
		{tasks: active + "\n" + done},
		{tasks: active, finished: true},
	} {
		findings := rebootCollisionCommandFindings(t, fixture)
		collision := !fixture.finished && fixture.tasks == active
		requireRebootCollisionFindings(t, findings, !collision, collision, false)
	}
}

// Cause failure cannot discard an independently complete collision, and its
// successful-looking private prefix must not establish scheduled attribution.
func TestRebootCollisionCauseFailureRetainsIndependentCollision(t *testing.T) {
	active := rebootCollisionTestJournal(t, "2026-08-31T02:09:51Z", "["+rebootCollisionTestTaskId+"]eval active(541.17s) github.com/urnetwork/server/taskworker/work.SyntheticTask({})")
	cause := `{"MESSAGE":"Starting Scheduled Reboot Service... synthetic-private-cause"}`
	for _, tasks := range []string{"", active} {
		findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{tasks: tasks, cause: cause, causeStatus: 1})
		requireRebootCollisionFindings(t, findings, false, tasks != "", true)
		for _, result := range findings {
			if result.class == "reboot-task-collision" && result.frame != "unattributed" {
				t.Error("failed cause journal established reboot attribution")
			}
		}
	}
	findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{tasks: active, cause: cause})
	requireRebootCollisionFindings(t, findings, false, true, false)
	for _, result := range findings {
		if result.class == "reboot-task-collision" && result.frame != "by-restart.timer" {
			t.Error("complete cause journal lost scheduled reboot attribution")
		}
	}
}

// A malformed or saturated attribution window has the same authority limit
// as a failed cause read, even if a scheduled-reboot prefix was returned.
func TestRebootCollisionIncompleteCauseRetainsIndependentCollision(t *testing.T) {
	active := rebootCollisionTestJournal(t, "2026-08-31T02:09:51Z", "["+rebootCollisionTestTaskId+"]eval active(541.17s) github.com/urnetwork/server/taskworker/work.SyntheticTask({})")
	cause := `{"MESSAGE":"Starting Scheduled Reboot Service... synthetic-private-cause"}`
	for _, body := range []string{
		`{"synthetic-private-cause":`,
		cause + "\n" + `{"MESSAGE":null}`,
		strings.Repeat(cause+"\n", 21),
	} {
		findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{tasks: active, cause: body})
		requireRebootCollisionFindings(t, findings, false, true, true)
		for _, result := range findings {
			if result.class == "reboot-task-collision" && result.frame != "unattributed" {
				t.Error("incomplete cause journal established reboot attribution")
			}
		}
	}
}

// The bounded tag query also returns non-lifecycle messages. They count
// toward the cap but cannot manufacture an active-task heartbeat.
func TestRebootCollisionCompleteNonLifecycleTaskJournalRemainsHealthy(t *testing.T) {
	line := rebootCollisionTestJournal(t, "2026-08-31T02:09:51Z", "synthetic-private-unrelated-message")
	findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{tasks: line})
	requireRebootCollisionFindings(t, findings, true, false, false)
}

// Completion status and framing are mandatory even if every available
// payload line is parseable. Unknown alerts retain no private source text.
func TestRebootCollisionRejectsIncompleteOrAmbiguousProducerFrames(t *testing.T) {
	for _, mutate := range []func(string) string{
		func(raw string) string { return strings.ReplaceAll(raw, rebootTaskLogsMarker+"_status=0", "") },
		func(raw string) string { return strings.ReplaceAll(raw, rebootCollisionTestCompleteMarker, "") },
		func(raw string) string {
			return strings.ReplaceAll(raw, rebootTaskLogsMarker+"_status=0", rebootTaskLogsMarker+"_status=synthetic-private-status")
		},
		func(raw string) string {
			return strings.ReplaceAll(raw, rebootTaskLogsMarker+"_status=0", rebootTaskLogsMarker+"_status=0\n"+rebootTaskLogsMarker+"_status=0")
		},
		func(raw string) string { return raw + "synthetic-private-trailing-data\n" },
		func(raw string) string { return "monitor_boot_age_s=1\n" + raw },
		func(raw string) string {
			return strings.ReplaceAll(raw, "monitor_boot_age_s=300", "monitor_boot_age_s=9223372036854775807")
		},
	} {
		findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{mutate: mutate})
		requireRebootCollisionFindings(t, findings, false, false, true)
	}
}

// Corrupt source records must not become a healthy empty normalized stream,
// and raw JSON/identity errors must not enter public Alert strings.
func TestRebootCollisionMalformedTaskJournalIsPrivateAndUnknown(t *testing.T) {
	for _, tasks := range []string{
		`{"synthetic-private-token":`,
		`{"SYSLOG_TIMESTAMP":"2026-08-31T02:09:51Z","MESSAGE":"synthetic-private-message","CONTAINER_TAG":"synthetic-private-tag"}`,
		rebootCollisionTestJournal(t, "synthetic-private-time", "synthetic-private-message"),
	} {
		findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{tasks: tasks})
		requireRebootCollisionFindings(t, findings, false, false, true)
	}
}

// The old-boot fast path needs its own terminal frame but performs no journal
// acquisition; the command's scoped queries remain finite and grep-free.
func TestRebootCollisionOldBootCompletesWithoutJournalReads(t *testing.T) {
	findings := rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{oldBoot: true})
	requireRebootCollisionFindings(t, findings, true, false, false)
	findings = rebootCollisionCommandFindings(t, rebootCollisionCommandFixture{
		oldBoot: true,
		mutate: func(raw string) string {
			return strings.ReplaceAll(raw, rebootCollisionTestCompleteMarker, "")
		},
	})
	requireRebootCollisionFindings(t, findings, false, false, true)
	command, err := rebootCollisionCommand("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"-b -1 -o json -n 1", "--since '30 minutes ago' -n 5001", "-t 'warp|synthetic|taskworker|g1' -t 'warp|synthetic|taskworker|g2'"} {
		if !strings.Contains(command, required) {
			t.Error("reboot command lost a bounded journal discriminator")
		}
	}
	if strings.Contains(command, "--grep") || strings.Contains(command, "|| true") || strings.Contains(command, "-n all") {
		t.Error("reboot command contains ambiguous status handling or an unbounded read")
	}
}
