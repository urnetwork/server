package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §2.13 maps to signal_reboot_collision.go and
// signal_reboot_collision_test.go.
func NewRebootCollisionSignal() Signal {
	return &signalAdapter{
		number: "2.13",
		key:    "reboot-collision",
		name:   "Host reboot and active-task collision",
		probe:  rebootCollisionProbe{},
	}
}

type rebootCollisionProbe struct{}

const (
	rebootCollisionRecentBoot     = 20 * time.Minute
	rebootCollisionTaskLimit      = 2 * time.Minute
	rebootCollisionCommandTimeout = 12 * time.Second
)

const (
	rebootPreviousEndMarker = "monitor_previous_boot_end_json"
	rebootCauseMarker       = "monitor_reboot_cause"
	rebootTaskLogsMarker    = "monitor_task_logs"
)

func (rebootCollisionProbe) id() string             { return "host/reboot-collision" }
func (rebootCollisionProbe) tier() string           { return tierWarn }
func (rebootCollisionProbe) cadence() time.Duration { return 5 * time.Minute }

type rebootCollisionTask struct {
	name       string
	seconds    int
	taskID     string
	observedAt time.Time
	identity   warpLogIdentity
}

type rebootCollisionObservation struct {
	host             *host
	bootAt           time.Time
	bootAge          time.Duration
	previousBootEnd  time.Time
	scheduledRestart bool
	restartEvidence  string
	tasks            []rebootCollisionTask
}

type rebootCollisionHostResult struct {
	host        *host
	observation rebootCollisionObservation
	err         error
}

func rebootCollisionCommand(environment string) (string, error) {
	if !taskworkerJournalTokenRe.MatchString(environment) {
		return "", fmt.Errorf("reboot collision: unsafe environment %q", environment)
	}
	return fmt.Sprintf(`boot_epoch=$(awk '$1 == "btime" {print $2}' /proc/stat)
now_epoch=$(date +%%s)
printf 'monitor_boot_epoch=%%s\n' "$boot_epoch"
if [ -z "$boot_epoch" ]; then
  exit 0
fi
boot_age=$((now_epoch - boot_epoch))
printf 'monitor_boot_age_s=%%s\n' "$boot_age"
if [ "$boot_age" -gt %d ]; then
  exit 0
fi
printf '%s\n'
journalctl --no-pager -b -1 -o json -n 1 2>/dev/null || true
printf '%s\n'
journalctl --no-pager -b -1 -u by-restart.service --since '30 minutes ago' -n 20 -o cat 2>/dev/null || true
printf '%s\n'
journalctl --no-pager -b -1 -o json --since '30 minutes ago' -n 5000 \
  -t 'warp|%s|taskworker|g1' -t 'warp|%s|taskworker|g2' \
  --grep='eval (active|done|error)' 2>/dev/null || true`,
		int(rebootCollisionRecentBoot/time.Second),
		rebootPreviousEndMarker,
		rebootCauseMarker,
		rebootTaskLogsMarker,
		environment,
		environment,
	), nil
}

func parseRebootCollisionObservation(target *host, raw string) (rebootCollisionObservation, error) {
	observation := rebootCollisionObservation{host: target}
	section := "meta"
	previousEndJSON := ""
	causeLines := []string{}
	taskLogLines := []string{}
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		switch line {
		case rebootPreviousEndMarker:
			section = "previous-end"
			continue
		case rebootCauseMarker:
			section = "cause"
			continue
		case rebootTaskLogsMarker:
			section = "task-logs"
			continue
		}
		switch section {
		case "meta":
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return observation, fmt.Errorf("%s: parse %s: %w", target.name, key, err)
			}
			switch key {
			case "monitor_boot_epoch":
				observation.bootAt = time.Unix(seconds, 0)
			case "monitor_boot_age_s":
				observation.bootAge = time.Duration(seconds) * time.Second
			}
		case "previous-end":
			if previousEndJSON == "" {
				previousEndJSON = line
			}
		case "cause":
			causeLines = append(causeLines, line)
		case "task-logs":
			taskLogLines = append(taskLogLines, line)
		}
	}
	if observation.bootAt.IsZero() {
		return observation, fmt.Errorf("%s: reboot battery returned no boot epoch", target.name)
	}
	if rebootCollisionRecentBoot < observation.bootAge {
		return observation, nil
	}
	if previousEndJSON == "" {
		return observation, fmt.Errorf("%s: recent boot has no readable previous-boot boundary", target.name)
	}
	previousEndEntry := taskworkerJournalEntry{}
	if err := json.Unmarshal([]byte(previousEndJSON), &previousEndEntry); err != nil {
		return observation, fmt.Errorf("%s: decode previous-boot boundary: %w", target.name, err)
	}
	previousBootEnd, err := taskworkerJournalObservedAt(previousEndEntry)
	if err != nil {
		return observation, fmt.Errorf("%s: parse previous-boot boundary: %w", target.name, err)
	}
	observation.previousBootEnd = previousBootEnd
	observation.restartEvidence = strings.Join(causeLines, "\n")
	observation.scheduledRestart = strings.Contains(
		strings.ToLower(observation.restartEvidence),
		"scheduled reboot service",
	)

	normalized, err := normalizeTaskworkerJournal(strings.Join(taskLogLines, "\n"), target.name)
	if err != nil {
		return observation, fmt.Errorf("%s: normalize previous taskworker journal: %w", target.name, err)
	}
	for _, generation := range []string{"g1", "g2"} {
		for _, run := range parseExecutorActiveTasks(normalized, target.name, generation, previousBootEnd) {
			if run.seconds < int(rebootCollisionTaskLimit/time.Second) {
				continue
			}
			active := parseTaskActiveRunForID(normalized, run.name, run.taskID)
			observation.tasks = append(observation.tasks, rebootCollisionTask{
				name:       run.name,
				seconds:    run.seconds,
				taskID:     run.taskID,
				observedAt: run.observedAt,
				identity:   active.identity,
			})
		}
	}
	sort.Slice(observation.tasks, func(i, j int) bool {
		if observation.tasks[i].seconds != observation.tasks[j].seconds {
			return observation.tasks[i].seconds > observation.tasks[j].seconds
		}
		return observation.tasks[i].taskID < observation.tasks[j].taskID
	})
	return observation, nil
}

func completedBeforeReboot(
	ctx context.Context,
	env *probeEnv,
	observations []rebootCollisionObservation,
) (map[string]bool, error) {
	conditions := []string{}
	seen := map[string]bool{}
	for _, observation := range observations {
		for _, task := range observation.tasks {
			if task.taskID == "" || seen[task.taskID] {
				continue
			}
			seen[task.taskID] = true
			conditions = append(conditions, fmt.Sprintf(
				"(task_id = '%s' AND run_end_time <= to_timestamp(%d))",
				task.taskID,
				observation.previousBootEnd.Unix(),
			))
		}
	}
	completed := map[string]bool{}
	if len(conditions) == 0 {
		return completed, nil
	}
	rows, err := env.runner.pg(ctx, `
		SELECT task_id::text
		FROM finished_task
		WHERE `+strings.Join(conditions, " OR ")+`;
	`)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		completed[row.str(0)] = true
	}
	return completed, nil
}

func formatRebootCollisionTasks(tasks []rebootCollisionTask) string {
	parts := make([]string, 0, len(tasks))
	for _, task := range tasks {
		part := fmt.Sprintf("%s:%ds", task.name, task.seconds)
		if task.identity.generation != "" {
			part += fmt.Sprintf("(%s/%s)", task.identity.generation, task.identity.container)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ",")
}

func (rebootCollisionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	command, err := rebootCollisionCommand(strings.TrimSpace(env.cfg.env))
	if err != nil {
		return nil, err
	}
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, nil
	}
	results := make(chan rebootCollisionHostResult, len(hosts))
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
				results <- rebootCollisionHostResult{host: target, err: ctx.Err()}
				return
			}
			out, err := env.runner.sshTimeout(ctx, target, command, "", rebootCollisionCommandTimeout)
			if err != nil {
				results <- rebootCollisionHostResult{host: target, err: err}
				return
			}
			observation, err := parseRebootCollisionObservation(target, out)
			results <- rebootCollisionHostResult{host: target, observation: observation, err: err}
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]rebootCollisionHostResult, 0, len(hosts))
	observations := []rebootCollisionObservation{}
	for result := range results {
		ordered = append(ordered, result)
		if result.err == nil {
			observations = append(observations, result.observation)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].host.name < ordered[j].host.name })
	completed, err := completedBeforeReboot(ctx, env, observations)
	if err != nil {
		return nil, fmt.Errorf("exclude tasks completed before reboot: %w", err)
	}

	findings := []finding{}
	for _, result := range ordered {
		if result.err != nil {
			findings = append(findings, cannotObserveFinding(result.host.name+"/reboot-collision", result.err))
			continue
		}
		observation := result.observation
		interrupted := []rebootCollisionTask{}
		for _, task := range observation.tasks {
			if !completed[task.taskID] {
				interrupted = append(interrupted, task)
			}
		}
		if len(interrupted) == 0 {
			findings = append(findings, healthyFinding(
				"host/reboot-collision",
				tierWarn,
				"reboot-task-collision",
				result.host.name,
			))
			continue
		}

		rebootSource := "unattributed"
		mechanism := "The host crossed a boot boundary while taskworker still emitted fresh long-task heartbeats. Those attempts have neither a terminal log nor a finished row before shutdown, so their scheduler-level work was interrupted and must be reclaimed."
		action := "Identify and coordinate the reboot source, then bound or checkpoint every listed task below the maintenance interruption budget. Do not delete its pending task row or treat process exit as successful task completion."
		if observation.scheduledRestart {
			rebootSource = "by-restart.timer"
			mechanism = "The configured by-restart maintenance timer initiated an orderly host reboot while taskworker still emitted fresh long-task heartbeats. Systemd stopped the containers before those attempts reached a terminal log or finished row, so scheduler-level progress was abandoned even when child writes had committed."
			action = "Deploy the bounded/checkpointed implementations and coordinate a taskworker drain with future maintenance windows. Do not disable the fleet reboot policy ad hoc, delete pending task rows, or raise task deadlines to hide the collision."
		}
		taskSummary := formatRebootCollisionTasks(interrupted)
		findings = append(findings, finding{
			probeId: "host/reboot-collision", tier: tierWarn,
			class: "reboot-task-collision", target: result.host.name, frame: rebootSource, sustain: 1,
			symptom: fmt.Sprintf(
				"%s rebooted with %d taskworker task(s) still active beyond %ds",
				result.host.name,
				len(interrupted),
				int(rebootCollisionTaskLimit/time.Second),
			),
			mechanism: mechanism,
			baseline:  "A host reboot has no taskworker heartbeat older than 120 seconds at its previous-boot boundary unless that exact attempt reached finished_task before shutdown.",
			observed: fmt.Sprintf(
				"boot_at=%s boot_age_s=%d previous_boot_end=%s reboot_source=%s interrupted_tasks=%s",
				observation.bootAt.UTC().Format(time.RFC3339),
				int(observation.bootAge/time.Second),
				observation.previousBootEnd.UTC().Format(time.RFC3339Nano),
				rebootSource,
				taskSummary,
			),
			evidence: strings.TrimSpace(strings.Join([]string{
				"previous-boot service evidence:\n" + observation.restartEvidence,
				"fresh non-terminal task heartbeats: " + taskSummary,
			}, "\n")),
			context:  "The finished_task cross-check excludes work that completed between its last informational heartbeat and shutdown. A surviving pending row and later retry are recovery, not proof that the interrupted attempt completed; correlate the resulting lease delay and backlog without blaming an orderly reboot on OOM or a crash.",
			action:   action,
			verify:   "Every interrupted task attempt is reclaimed and reaches a real terminal result, its affected backlog drains, and the next maintenance boot has no fresh task heartbeat beyond 120 seconds at shutdown.",
			playbook: "SIGNALS.md §2.13 and §8.1",
		})
	}
	return findings, nil
}
