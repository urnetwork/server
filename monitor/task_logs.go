package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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

const taskworkerJournalTimeout = 12 * time.Second
const taskLifecycleGatewayTimeout = 12 * time.Second

var taskworkerJournalTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type taskworkerJournalEntry struct {
	Message           string `json:"MESSAGE"`
	Timestamp         string `json:"SYSLOG_TIMESTAMP"`
	RealtimeTimestamp string `json:"__REALTIME_TIMESTAMP"`
	ContainerTag      string `json:"CONTAINER_TAG"`
	SyslogIdentifier  string `json:"SYSLOG_IDENTIFIER"`
	ContainerID       string `json:"CONTAINER_ID"`
	ContainerIDFull   string `json:"CONTAINER_ID_FULL"`
	Hostname          string `json:"_HOSTNAME"`
}

func taskworkerJournalObservedAt(entry taskworkerJournalEntry) (time.Time, error) {
	observedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(entry.Timestamp))
	if err == nil {
		return observedAt, nil
	}
	micros, parseErr := strconv.ParseInt(strings.TrimSpace(entry.RealtimeTimestamp), 10, 64)
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("no parseable timestamp")
	}
	return time.UnixMicro(micros), nil
}

// normalizeTaskworkerJournal restores the fleet-log envelope that task
// lifecycle parsers consume. journalctl's cat format discards both the
// timestamp and the container tag; in a multi-run lookback that makes an older
// larger elapsed value look newer than the current task. JSON output preserves
// the authoritative journal timestamp and Docker metadata.
func normalizeTaskworkerJournal(raw, fallbackHost string) (string, error) {
	normalized := []string{}
	for lineNumber, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		entry := taskworkerJournalEntry{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return "", fmt.Errorf("taskworker journal line %d: decode JSON: %w", lineNumber+1, err)
		}

		observedAt, err := taskworkerJournalObservedAt(entry)
		if err != nil {
			return "", fmt.Errorf("taskworker journal line %d: %w", lineNumber+1, err)
		}

		tag := strings.TrimSpace(entry.ContainerTag)
		if tag == "" {
			tag = strings.TrimSpace(entry.SyslogIdentifier)
		}
		tagParts := strings.Split(tag, "|")
		if len(tagParts) != 4 || tagParts[2] != "taskworker" ||
			(tagParts[3] != "g1" && tagParts[3] != "g2") {
			return "", fmt.Errorf("taskworker journal line %d: unexpected container tag %q", lineNumber+1, tag)
		}

		hostname := strings.TrimSpace(entry.Hostname)
		if hostname == "" {
			hostname = fallbackHost
		}
		containerID := strings.TrimSpace(entry.ContainerID)
		if containerID == "" {
			containerID = strings.TrimSpace(entry.ContainerIDFull)
			if 12 < len(containerID) {
				containerID = containerID[:12]
			}
		}
		if hostname == "" || containerID == "" || strings.TrimSpace(entry.Message) == "" {
			return "", fmt.Errorf("taskworker journal line %d: incomplete host/container/message identity", lineNumber+1)
		}

		normalized = append(normalized, fmt.Sprintf(
			"[%s][taskworker][%s][cid:%s][I][%s]%s",
			hostname,
			tagParts[3],
			containerID,
			observedAt.UTC().Format(time.RFC3339Nano),
			strings.TrimSpace(entry.Message),
		))
	}
	return strings.Join(normalized, "\n"), nil
}

// readTaskworkerJournal is the bounded, host-local fallback for task lifecycle
// reads when the fleet log gateway is unavailable. It deliberately reads only
// taskworker's two generation identifiers and lets journald apply the task
// name filter before returning output, so a failed observability front cannot
// turn recovery into an unbounded fleet log transfer.
func readTaskworkerJournal(
	ctx context.Context,
	env *probeEnv,
	taskName string,
	lookback time.Duration,
	limit int,
) (string, error) {
	if env == nil || env.cfg == nil || env.runner == nil {
		return "", fmt.Errorf("taskworker journal: probe environment is unavailable")
	}
	environment := strings.TrimSpace(env.cfg.env)
	if !taskworkerJournalTokenRe.MatchString(environment) {
		return "", fmt.Errorf("taskworker journal: unsafe environment %q", environment)
	}
	if !taskworkerJournalTokenRe.MatchString(taskName) {
		return "", fmt.Errorf("taskworker journal: unsafe task name %q", taskName)
	}
	if lookback <= 0 {
		return "", fmt.Errorf("taskworker journal: lookback must be positive")
	}
	if limit <= 0 || 10000 < limit {
		return "", fmt.Errorf("taskworker journal: limit %d is outside 1..10000", limit)
	}
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return "", fmt.Errorf("taskworker journal: no services hosts in inventory")
	}

	lookbackMinutes := int((lookback + time.Minute - 1) / time.Minute)
	command := fmt.Sprintf(
		"journalctl --no-pager -o json --since '%d minutes ago' -n %d "+
			"-t 'warp|%s|taskworker|g1' -t 'warp|%s|taskworker|g2' "+
			"--grep='%s' 2>/dev/null",
		lookbackMinutes,
		limit,
		environment,
		environment,
		taskName,
	)

	type result struct {
		host string
		out  string
		err  error
	}
	results := make(chan result, len(hosts))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for _, configuredHost := range hosts {
		target := configuredHost
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- result{host: target.name, err: ctx.Err()}
				return
			}
			out, err := env.runner.sshTimeout(ctx, target, command, "", taskworkerJournalTimeout)
			if err == nil {
				out, err = normalizeTaskworkerJournal(out, target.name)
			}
			results <- result{host: target.name, out: strings.TrimSpace(out), err: err}
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]result, 0, len(hosts))
	for observed := range results {
		ordered = append(ordered, observed)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].host < ordered[j].host })

	outputs := make([]string, 0, len(ordered))
	errors := make([]string, 0, len(ordered))
	successes := 0
	for _, observed := range ordered {
		if observed.err != nil {
			errors = append(errors, observed.host+": "+observed.err.Error())
			continue
		}
		successes++
		if observed.out != "" {
			outputs = append(outputs, observed.out)
		}
	}
	output := strings.Join(outputs, "\n")
	if successes == 0 {
		return output, fmt.Errorf("taskworker journal: every host failed: %s", strings.Join(errors, "; "))
	}
	if len(errors) != 0 {
		return output, fmt.Errorf("taskworker journal: partial fleet read: %s", strings.Join(errors, "; "))
	}
	return output, nil
}

// readTaskLifecycleLog prefers the fleet log gateway and falls back to the
// same bounded host-local journals when observability is degraded. Keeping
// this transport decision here lets task probes share identical chronology,
// limits, and error semantics.
func readTaskLifecycleLog(
	ctx context.Context,
	env *probeEnv,
	taskName string,
	lookback time.Duration,
	limit int,
) (output string, source string, returnErr error) {
	return readTaskLifecycleLogWithGatewayTimeout(
		ctx,
		env,
		taskName,
		lookback,
		limit,
		taskLifecycleGatewayTimeout,
	)
}

func readTaskLifecycleLogWithGatewayTimeout(
	ctx context.Context,
	env *probeEnv,
	taskName string,
	lookback time.Duration,
	limit int,
	gatewayTimeout time.Duration,
) (output string, source string, returnErr error) {
	if lookback <= 0 {
		return "", "", fmt.Errorf("task lifecycle log: lookback must be positive")
	}
	if gatewayTimeout <= 0 {
		return "", "", fmt.Errorf("task lifecycle log: gateway timeout must be positive")
	}
	lookbackMinutes := int((lookback + time.Minute - 1) / time.Minute)
	gatewayCtx, cancelGateway := context.WithTimeout(ctx, gatewayTimeout)
	output, gatewayErr := env.runner.warpctl(
		gatewayCtx,
		"logs", env.cfg.env, "taskworker",
		fmt.Sprintf("--since=%dm", lookbackMinutes),
		fmt.Sprintf("--limit=%d", limit),
		"--query="+taskName,
		"--utc",
	)
	gatewayContextErr := gatewayCtx.Err()
	cancelGateway()
	if gatewayErr == nil && gatewayContextErr == nil {
		return output, "warpctl", nil
	}
	if gatewayContextErr != nil {
		gatewayErr = fmt.Errorf("bounded %s lookup: %w", gatewayTimeout, gatewayContextErr)
	}

	journalOutput, journalErr := readTaskworkerJournal(ctx, env, taskName, lookback, limit)
	if journalOutput != "" || journalErr == nil {
		return journalOutput, "host-journal-fallback", journalErr
	}
	return "", "unavailable", fmt.Errorf(
		"fleet log gateway: %v; host journal fallback: %w",
		gatewayErr,
		journalErr,
	)
}

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
	if active.seconds == 0 {
		return false
	}
	if completedTaskID != "" && active.taskID != "" {
		// Task ids are unique lifecycle identities. Once this exact run has a
		// finished row, no retained heartbeat for it can become active again
		// merely because the completion aged past the unlabelled fallback.
		return completedTaskID == active.taskID
	}
	if completedAgeSeconds < 0 || int(logLookback/time.Second) < completedAgeSeconds {
		return false
	}
	return active.seconds <= completedDurationSeconds
}
