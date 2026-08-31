package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCloseDurationSignalSyntheticActiveOverrunUsesNewestHeartbeat(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			for _, want := range []string{
				"task_id::text",
				"run_end_time > now() - interval '45 minutes'",
				"AS is_overrun",
				"latest_rank = 1",
				"(is_overrun AND band_rank = 1)",
				"ORDER BY completed_unix_s DESC",
			} {
				if !strings.Contains(query, want) {
					t.Fatalf("close-duration query lost %q: %s", want, query)
				}
			}
			if strings.Contains(query, "pending_task") || strings.Contains(query, "now()-run_at") {
				t.Fatalf("query mistakes a due time for execution start: %s", query)
			}
			return nil, nil
		},
		localFn: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			if name != "warpctl" || !strings.Contains(joined, "--query=CloseExpiredContracts") || !strings.Contains(joined, "--since=45m") || !strings.Contains(joined, "--limit=5000") {
				t.Fatalf("unexpected close heartbeat command: %s %s", name, joined)
			}
			return "[edge-2][taskworker][g1][cid:old][I][2026-08-30T14:00:00Z][task.go:1938][01a0530b-4030-6499-073c-54c1202486b0]eval active(900.00s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})\n" +
				"[edge-3][taskworker][g2][cid:new][I][2026-08-30T14:01:00Z][task.go:1938][01a0530c-65aa-153e-19d8-82ad3698cf40]eval active(130.25s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})\n" +
				"[edge-4][taskworker][g2][cid:other][I][2026-08-30T14:02:00Z][task.go:1938][01a05314-d7d0-473e-9e5d-6140352a6b5c]eval active(999.00s) github.com/urnetwork/server/taskworker/work.ReconcileNetEscrow({})", nil
		},
	}

	alerts, err := NewCloseDurationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "close-duration-overrun")
	for _, want := range []string{
		"phase=active",
		"duration_s=130",
		"task_id=01a0530c-65aa-153e-19d8-82ad3698cf40",
		"active_host=edge-3",
		"active_generation=g2",
		"active_container=new",
		"25,000-contract checkpoint",
		"executor overlap alone is not causal proof",
		"Do not raise the deadline",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("active close alert lost %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestCloseDurationSignalFallsBackToBoundedHostJournalsWhenFleetLogsFail(t *testing.T) {
	taskID := "01a05564-87a2-4f60-4188-df03664a1e24"
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if !strings.Contains(query, "task_id = '"+taskID+"'") {
				t.Fatalf("fallback heartbeat identity was not retained in the completion query: %s", query)
			}
			return nil, nil
		},
		localFn: func(name string, args ...string) (string, error) {
			if name != "warpctl" {
				t.Fatalf("unexpected local command %s %s", name, strings.Join(args, " "))
			}
			return "", errors.New("Loki query error (502)")
		},
		hostTimeoutFn: func(host HostSettings, command string, timeout time.Duration) (string, error) {
			if timeout != taskworkerJournalTimeout {
				t.Fatalf("journal fallback timeout = %s, want %s", timeout, taskworkerJournalTimeout)
			}
			for _, want := range []string{
				"--since '45 minutes ago'",
				"-n 5000",
				"-t 'warp|synthetic|taskworker|g1'",
				"-t 'warp|synthetic|taskworker|g2'",
				"--grep='CloseExpiredContracts'",
			} {
				if !strings.Contains(command, want) {
					t.Fatalf("journal fallback lost %q: %s", want, command)
				}
			}
			if host.Name != "edge-2" {
				return "", nil
			}
			return "[edge-2][taskworker][g2][cid:close][I][2026-08-31T01:31:55Z][task.go:1938][" + taskID + "]eval active(885.56s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})", nil
		},
	}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "edge-1", Roles: []string{"services"}},
		HostSettings{Name: "edge-2", Roles: []string{"services"}},
	)

	alerts, err := NewCloseDurationSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "close-duration-overrun")
	for _, want := range []string{
		"phase=active duration_s=885",
		"active_log_source=host-journal-fallback",
		"task_id=" + taskID,
		"active_host=edge-2",
		"active_generation=g2",
		"active_container=close",
		"fleet log gateway was unavailable",
		"bounded taskworker journals",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("journal fallback alert lost %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestCloseDurationSignalCompletedRunSupersedesSameHeartbeat(t *testing.T) {
	taskID := "01a052f6-5c55-e78b-110d-dad7afffe710"
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{taskID, "completed", "1367", "60", "1788099600"}}, nil
		},
		localFn: func(string, ...string) (string, error) {
			return "[edge-3][taskworker][g2][cid:old][I][2026-08-30T14:20:30Z][task.go:1938][" + taskID + "]eval active(1360.00s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})", nil
		},
	}

	alerts, err := NewCloseDurationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "close-duration-overrun")
	if !strings.Contains(alert.Observed, "phase=completed duration_s=1367") ||
		!strings.Contains(alert.Observed, "completed_age_s=60") {
		t.Fatalf("finished row did not supersede its lingering heartbeat: %+v", alert)
	}
}

func TestCloseDurationSignalCompletedRunDoesNotResurfaceStaleSameIDHeartbeat(t *testing.T) {
	taskID := "01a05446-9d23-9e06-7cd9-1e3db5d91423"
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			// The completion has aged beyond the two-minute compatibility
			// fallback, but its exact task id still makes it authoritative.
			return []Row{{taskID, "completed", "253", "131", "1788106166"}}, nil
		},
		localFn: func(string, ...string) (string, error) {
			return "[edge-1][taskworker][g2][cid:hot][I][2026-08-30T20:09:24.977311Z][task.go:1938][" + taskID + "]eval active(251.78s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})", nil
		},
	}

	alerts, err := NewCloseDurationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "close-duration-overrun")
	if !strings.Contains(alert.Observed, "phase=completed duration_s=253") ||
		!strings.Contains(alert.Observed, "completed_age_s=131") {
		t.Fatalf("stale same-id heartbeat resurfaced after completion: %+v", alert)
	}
}

func TestCloseDurationSignalDifferentSuccessorHeartbeatRemainsActive(t *testing.T) {
	oldTaskID := "01a052f6-5c55-e78b-110d-dad7afffe710"
	newTaskID := "01a0530c-65aa-153e-19d8-82ad3698cf40"
	failedTaskID := "01a05309-e28a-5167-4cda-a92d0b371ff7"
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			return []Row{{oldTaskID, "completed", "1367", "60", "1788099600"}}, nil
		},
		localFn: func(string, ...string) (string, error) {
			return "[edge-3][taskworker][g2][cid:failed][I][2026-08-30T14:21:00.125Z][task.go:1930][" + failedTaskID + "]eval error(1800.83s) (reschedule) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({}) = Timeout\n" +
				"[edge-1][taskworker][g2][cid:new][I][2026-08-30T14:22:30Z][task.go:1938][" + newTaskID + "]eval active(130.00s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})", nil
		},
	}

	alerts, err := NewCloseDurationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "close-duration-overrun")
	for _, want := range []string{
		"phase=active duration_s=130",
		"task_id=" + newTaskID,
		"active_host=edge-1",
		"precursor_failed_duration_s=1800",
		"precursor_failed_task_id=" + failedTaskID,
		`precursor_failed_error="Timeout"`,
		"precursor_failed_at=2026-08-30T14:21:00.125Z",
		"precursor_failed_host=edge-3",
		"precursor_failed_generation=g2",
		"precursor_failed_container=failed",
		"preserve the latest deadline failure alongside the current active attempt",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("active successor lost failed precursor %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestCloseDurationSignalRetainsRescheduledTimeoutAcrossShortSameIDRetry(t *testing.T) {
	taskID := "01a0530c-65aa-153e-19d8-82ad3698cf40"
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) {
			// An older successful overrun is the only finished row. The current
			// deadline attempt rescheduled and therefore remains pending.
			return []Row{{"01a052f6-5c55-e78b-110d-dad7afffe710", "completed", "1367", "1902", "1788098400"}}, nil
		},
		localFn: func(string, ...string) (string, error) {
			return "[edge-3][taskworker][g2][cid:failed][I][2026-08-30T14:52:00.815957Z][task.go:1930][" + taskID + "]eval error(1800.83s) (reschedule) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({}) = Timeout\n" +
				"[edge-1][taskworker][g2][cid:retry][I][2026-08-30T14:52:25.507527Z][task.go:1938][" + taskID + "]eval active(20.01s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})", nil
		},
	}

	alerts, err := NewCloseDurationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "close-duration-overrun")
	for _, want := range []string{
		"phase=failed duration_s=1800",
		"task_id=" + taskID,
		`failed_error="Timeout"`,
		"failed_at=2026-08-30T14:52:00.815957Z",
		"failed_host=edge-3",
		"failed_container=failed",
		"retry_phase=active",
		"retry_last_heartbeat_duration_s=20",
		"retry_observed_at=2026-08-30T14:52:25.507527Z",
		"retry_host=edge-1",
		"retry_generation=g2",
		"retry_container=retry",
		"a failed checkpoint lasting 1800s",
		"eval-error retains rescheduled deadline attempts",
		"fast peer retry is an A/B control",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("rescheduled timeout was lost after its short retry, missing %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Observed, "phase=completed duration_s=1367") ||
		strings.HasPrefix(alert.Observed, "phase=active ") {
		t.Fatalf("older completion or short retry replaced timeout: %+v", alert)
	}
}

func TestCloseDurationSignalReportsCompletedSameIDRetryWithoutErasingTimeout(t *testing.T) {
	taskID := "01a0530c-65aa-153e-19d8-82ad3698cf40"
	successorTaskID := "01a05311-f53e-ebcd-dc07-ec4835d420c5"
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if !strings.Contains(query, "task_id = '"+taskID+"'") {
				t.Fatalf("query did not retain the terminal task id after later successors: %s", query)
			}
			return []Row{
				{successorTaskID, "completed", "22", "1", "1788101600"},
				{taskID, "completed", "24", "5", "1788101545"},
				{"01a052f6-5c55-e78b-110d-dad7afffe710", "completed", "1367", "1902", "1788098400"},
			}, nil
		},
		localFn: func(string, ...string) (string, error) {
			return "[edge-3][taskworker][g2][cid:failed][I][2026-08-30T14:52:00.815957Z][task.go:1930][" + taskID + "]eval error(1800.83s) (reschedule) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({}) = Timeout\n" +
				"[edge-1][taskworker][g2][cid:retry][I][2026-08-30T14:52:25.507527Z][task.go:1938][" + taskID + "]eval active(20.01s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})\n" +
				"[edge-4][taskworker][g1][cid:successor][I][2026-08-30T14:53:15.507527Z][task.go:1938][" + successorTaskID + "]eval active(10.01s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})", nil
		},
	}

	alerts, err := NewCloseDurationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "close-duration-overrun")
	for _, want := range []string{
		"phase=failed duration_s=1800",
		"task_id=" + taskID,
		`failed_error="Timeout"`,
		"retry_phase=completed",
		"retry_duration_s=24",
		"retry_completed_age_s=5",
		"retry_completed_at=2026-08-30T14:52:25Z",
		"retry_host=edge-1",
		"retry_generation=g2",
		"retry_container=retry",
		"fast peer retry is an A/B control",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("completed retry lost timeout evidence %q:\n%s", want, alert.Markdown())
		}
	}
	if strings.Contains(alert.Observed, "phase=completed duration_s=24") ||
		strings.Contains(alert.Observed, "phase=completed duration_s=1367") {
		t.Fatalf("completion erased timeout: %+v", alert)
	}
}

func TestCloseDurationSignalSyntheticHealthy(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(string) ([]Row, error) { return nil, nil },
		localFn: func(string, ...string) (string, error) {
			return "[taskworker]eval active(20.00s) github.com/urnetwork/server/taskworker/work.CloseExpiredContracts({})", nil
		},
	}
	alerts, err := NewCloseDurationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy close heartbeat alerted: %+v", alerts)
	}
}
