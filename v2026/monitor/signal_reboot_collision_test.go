package monitor

import (
	"context"
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
				"--grep='eval (active|done|error)'",
				"warp|synthetic|taskworker|g2",
			} {
				if !strings.Contains(command, want) {
					t.Fatalf("reboot battery lost %q: %s", want, command)
				}
			}
			if host.Name == "edge-1" {
				return "monitor_boot_epoch=1788138000\nmonitor_boot_age_s=3600", nil
			}
			return strings.Join([]string{
				"monitor_boot_epoch=1788142395",
				"monitor_boot_age_s=300",
				rebootPreviousEndMarker,
				`{"SYSLOG_TIMESTAMP":"2026-08-31T02:10:05.172966Z","MESSAGE":"Journal stopped"}`,
				rebootCauseMarker,
				"Starting Scheduled Reboot Service...",
				rebootTaskLogsMarker,
				journal("2026-08-31T02:09:51.757953Z", interruptedID, "CloseExpiredContracts", "541.17"),
				journal("2026-08-31T02:09:45.000000Z", finishedID, "UpdateClientScores", "3565.74"),
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
		"Starting Scheduled Reboot Service",
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
