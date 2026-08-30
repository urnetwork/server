package monitor

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var taskActiveDurationRe = regexp.MustCompile(`eval active\(([0-9]+(?:\.[0-9]+)?)s\)`)
var taskTerminalDurationRe = regexp.MustCompile(`eval error\(([0-9]+(?:\.[0-9]+)?)s\).* = (.+)$`)
var taskRunIDRe = regexp.MustCompile(`\[([0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12})\]eval (?:active|done|error)\(`)
var warpLogTimeRe = regexp.MustCompile(`\[(\d{4}-\d{2}-\d{2}T[^]]+Z)\]`)
var taskActiveFunctionRe = regexp.MustCompile(`eval active\([0-9]+(?:\.[0-9]+)?s\) ([^\s(]+)\(`)

// taskActiveRun is the reusable identity and elapsed-time shape emitted by
// taskworker's authoritative eval-active heartbeat. pending_task.run_at is a
// due time and claim_time is a moving lease heartbeat, so neither is an
// execution-start clock.
type taskActiveRun struct {
	seconds    int
	taskID     string
	identity   warpLogIdentity
	observedAt time.Time
}

type taskTerminalRun struct {
	taskActiveRun
	errorText string
}

type executorActiveTask struct {
	name       string
	seconds    int
	taskID     string
	observedAt time.Time
}

// Eval-active heartbeats are emitted every ReleaseTimeout/3 (about ten
// seconds). Successful eval-done lines are verbosity-1 and are not present in
// the production log stream, so terminal-line suppression alone cannot remove
// a just-finished task. Four missed heartbeats is enough to stop calling the
// old line active while tolerating ordinary ingestion jitter.
const executorActiveHeartbeatFreshness = 45 * time.Second

// parseExecutorActiveTasks turns a fleet-wide eval-active window into the
// newest heartbeat for each task on one exact host/block executor. Mimir's
// runtime instance is a process metric id rather than warp's container id;
// taskworker has one container per host/block, so those two labels are the
// reusable join key between the metric and task logs.
func parseExecutorActiveTasks(logOutput, host, block string, now time.Time) []executorActiveTask {
	byTask := map[string]executorActiveTask{}
	terminalByTask := map[string]time.Time{}
	for _, line := range strings.Split(logOutput, "\n") {
		identity := parseWarpLogIdentity(line)
		if identity.service != "taskworker" || identity.host != host || identity.generation != block {
			continue
		}
		if strings.Contains(line, "]eval done(") || strings.Contains(line, "]eval error(") {
			idMatch := taskRunIDRe.FindStringSubmatch(line)
			if len(idMatch) == 2 {
				observedAt := time.Time{}
				if timeMatch := warpLogTimeRe.FindStringSubmatch(line); len(timeMatch) == 2 {
					observedAt, _ = time.Parse(time.RFC3339Nano, timeMatch[1])
				}
				if current, exists := terminalByTask[idMatch[1]]; !exists || current.Before(observedAt) {
					terminalByTask[idMatch[1]] = observedAt
				}
			}
			continue
		}
		durationMatch := taskActiveDurationRe.FindStringSubmatch(line)
		functionMatch := taskActiveFunctionRe.FindStringSubmatch(line)
		if len(durationMatch) < 2 || len(functionMatch) < 2 {
			continue
		}
		seconds, err := strconv.ParseFloat(durationMatch[1], 64)
		if err != nil {
			continue
		}
		name := functionMatch[1]
		if dot := strings.LastIndexByte(name, '.'); 0 <= dot {
			name = name[dot+1:]
		}
		candidate := executorActiveTask{name: name, seconds: int(seconds)}
		if idMatch := taskRunIDRe.FindStringSubmatch(line); len(idMatch) == 2 {
			candidate.taskID = idMatch[1]
		}
		if timeMatch := warpLogTimeRe.FindStringSubmatch(line); len(timeMatch) == 2 {
			candidate.observedAt, _ = time.Parse(time.RFC3339Nano, timeMatch[1])
		}
		key := candidate.taskID
		if key == "" {
			key = candidate.name
		}
		current, exists := byTask[key]
		if !exists || (!candidate.observedAt.IsZero() && (current.observedAt.IsZero() || current.observedAt.Before(candidate.observedAt))) ||
			(candidate.observedAt.IsZero() && current.observedAt.IsZero() && current.seconds < candidate.seconds) {
			byTask[key] = candidate
		}
	}
	runs := make([]executorActiveTask, 0, len(byTask))
	for _, run := range byTask {
		if terminalAt, terminal := terminalByTask[run.taskID]; terminal &&
			(run.observedAt.IsZero() || !terminalAt.Before(run.observedAt)) {
			continue
		}
		if !run.observedAt.IsZero() {
			age := now.Sub(run.observedAt)
			if executorActiveHeartbeatFreshness < age || age < -30*time.Second {
				continue
			}
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].seconds != runs[j].seconds {
			return runs[i].seconds > runs[j].seconds
		}
		if runs[i].name != runs[j].name {
			return runs[i].name < runs[j].name
		}
		return runs[i].taskID < runs[j].taskID
	})
	return runs
}

func formatExecutorActiveTasks(runs []executorActiveTask, limit int) string {
	if limit <= 0 || len(runs) == 0 {
		return ""
	}
	if len(runs) > limit {
		runs = runs[:limit]
	}
	parts := make([]string, 0, len(runs))
	for _, run := range runs {
		part := fmt.Sprintf("%s:%ds", run.name, run.seconds)
		if run.taskID != "" {
			part += "@" + run.taskID
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

// parseTaskActiveRun selects the newest matching heartbeat when warpctl's UTC
// timestamp is present. Untimestamped synthetic/legacy lines fall back to the
// largest elapsed value. Selecting the newest run matters for immediate
// recurring successors: an older long heartbeat must not hide a newer run.
func parseTaskActiveRun(logOutput, taskName string) taskActiveRun {
	return parseTaskActiveRunForID(logOutput, taskName, "")
}

// parseTaskActiveRunForID selects one exact task lifecycle while retaining
// parseTaskActiveRun's newest-heartbeat semantics. This matters after a retry
// completes: a later successor can be the newest function heartbeat without
// being the executor that ran the failed task id's retry.
func parseTaskActiveRunForID(logOutput, taskName, taskID string) taskActiveRun {
	active := taskActiveRun{}
	marker := "." + taskName + "("
	for _, line := range strings.Split(logOutput, "\n") {
		if taskName == "" || !strings.Contains(line, marker) {
			continue
		}
		match := taskActiveDurationRe.FindStringSubmatch(line)
		if len(match) < 2 {
			continue
		}
		seconds, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		candidate := taskActiveRun{
			seconds:  int(seconds),
			identity: parseWarpLogIdentity(line),
		}
		if idMatch := taskRunIDRe.FindStringSubmatch(line); len(idMatch) == 2 {
			candidate.taskID = idMatch[1]
		}
		if taskID != "" && candidate.taskID != taskID {
			continue
		}
		if timeMatch := warpLogTimeRe.FindStringSubmatch(line); len(timeMatch) == 2 {
			candidate.observedAt, _ = time.Parse(time.RFC3339Nano, timeMatch[1])
		}

		if !candidate.observedAt.IsZero() {
			if active.observedAt.IsZero() || active.observedAt.Before(candidate.observedAt) {
				active = candidate
			}
		} else if active.observedAt.IsZero() && active.seconds < candidate.seconds {
			active = candidate
		}
	}
	return active
}

// parseTaskTerminalRun retains rescheduled failures that never become a
// finished_task duration. This is essential for deadline attempts: their
// per-item writes may commit, but the task row stays pending and is reclaimed
// under the same id.
func parseTaskTerminalRun(logOutput, taskName string) taskTerminalRun {
	terminal := taskTerminalRun{}
	marker := "." + taskName + "("
	for _, line := range strings.Split(logOutput, "\n") {
		if taskName == "" || !strings.Contains(line, marker) {
			continue
		}
		match := taskTerminalDurationRe.FindStringSubmatch(line)
		if len(match) < 3 {
			continue
		}
		seconds, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		candidate := taskTerminalRun{
			taskActiveRun: taskActiveRun{
				seconds:  int(seconds),
				identity: parseWarpLogIdentity(line),
			},
			errorText: strings.TrimSpace(match[2]),
		}
		if idMatch := taskRunIDRe.FindStringSubmatch(line); len(idMatch) == 2 {
			candidate.taskID = idMatch[1]
		}
		if timeMatch := warpLogTimeRe.FindStringSubmatch(line); len(timeMatch) == 2 {
			candidate.observedAt, _ = time.Parse(time.RFC3339Nano, timeMatch[1])
		}

		if !candidate.observedAt.IsZero() {
			if terminal.observedAt.IsZero() || terminal.observedAt.Before(candidate.observedAt) {
				terminal = candidate
			}
		} else if terminal.observedAt.IsZero() && terminal.seconds < candidate.seconds {
			terminal = candidate
		}
	}
	return terminal
}

// completedTaskSupersedesHeartbeat identifies a lingering log line for the
// same finished run. When task ids are available they are authoritative; the
// duration/age fallback preserves compatibility with older unlabelled logs.
func completedTaskSupersedesHeartbeat(
	completedTaskID string,
	completedDurationSeconds int,
	completedAgeSeconds int,
	active taskActiveRun,
	logLookback time.Duration,
) bool {
	if active.seconds == 0 || completedAgeSeconds < 0 || int(logLookback/time.Second) < completedAgeSeconds {
		return false
	}
	if completedTaskID != "" && active.taskID != "" {
		return completedTaskID == active.taskID
	}
	return active.seconds <= completedDurationSeconds
}
