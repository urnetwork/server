// `sidecarctl evs purge-turn-on <env>` schedules the versionless sweep's turn-on pass, the one pass that deletes what
// §19.11's `format2.purge: observe` kept (§11.13's sweep row), on the env's task queue, where taskworker's evs host
// runs evs.sweep (server/taskworker/evs_sweep.go). It is the first command of the evs group. No process notices the
// purge turning on, so the owner's answer of 2026-11-10 (QD-635, questions/evsb-m3-02-sweep-pass-triggers.md) has
// the operator run this in the runbook step that sets `format2.purge: on` (EVS2-M10-34's row), and until it runs,
// what observe mode kept stays: content is kept, never lost.
//
// It reads the env's format2 gate as the env's services read sidecar.yml, and refuses unless the gate is enabled and
// its purge reads on: under observe the pass deletes nothing, so a run would report a pass that did no work, and with
// the gate off §19.11's rollout has not reached this step. A gate that does not read refuses too. Then it schedules
// evsservice.EvsSweepTurnOnTaskRequest(), due startDelay after the run, and prints what it scheduled. It never runs a
// pass in its own process. The request is once-keyed and the pass idempotent, so a second run folds into a request
// still pending, or queues a pass that finds nothing more to delete.
//
// A pass reads its purge mode from the sidecar.yml of the taskworker that runs it, which rereads the file every 60 s
// (§19.11), not from the copy this read. So the start is due two of those intervals after the run, by which time a
// taskworker that held the new file reads on. A host the config push had not reached reads observe and keeps
// everything; EVS2-M10-34's monitor row, observe's retained count flat while purge is on, warns of that, and a second
// run schedules the pass again (questions/ops-m10-01-turn-on-under-observe.md asks for the pass to wait instead).
//
// What it reads and where it schedules are server's: the config loader that reads config/<env>/sidecar.yml under
// WARP_ENV as every service does (server/sidecar_config.go), server/task's queue in the env's Postgres
// (sidecarhost.Tasks, whose Schedule once-keys a request), and the request evsservice builds. This module may link
// none of them: §4.7's license and module rules keep every server package out
// (questions/dns-m0-01-sidecarctl-server-link.md), and §4.4 lets only server link an engine package such as
// evsservice (questions/ops-m10-01-purge-turn-on-links.md). Until those are answered, the env opener refuses every env
// before it reads or schedules anything, so the command exits 1 with nothing queued, and the turn-on waits as it does
// today. The rest is built against evsPurgeTurnOnEnv, which is all such an opener has to supply.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"time"
)

// format2's purge mode that deletes, as sidecar.yml spells it. server's loader reads anything but exactly this as
// observe (server/sidecar_gates.go), and so does this.
const evsFormat2PurgeOn = "on"

// An env's name: one directory under config/ and vault/, as warp lays them out. Anything else, a path above all,
// would name another directory to server's resolver, so it is refused before anything is read.
var evsPurgeTurnOnEnvRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// The command's registration. It is the only change to the binary this command needs.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "evs purge-turn-on",
		usage:   "<env>",
		summary: "Schedule the versionless sweep's turn-on pass on the env's task queue, deleting what format2.purge: observe kept. Run it in the runbook step that sets format2.purge: on; it refuses unless the env's format2 gate is enabled and its purge reads on.",
		run:     runEvsPurgeTurnOn,
	})
}

// One read of an env's format2 gate, as its services read sidecar.yml.
type evsFormat2GateRead struct {
	// format2's `enabled`: off, an absent file and a file that does not parse all read false
	enabled bool
	// format2's purge as the loader read it: on, or observe for anything else
	purge string
}

// What a run scheduled, as server/task holds it: names and times only (host_tasks.go).
type evsScheduledTask struct {
	// the job's name, evs.sweep
	name string
	// the key under which a second request folds into this one while it is pending
	onceKey string
	// when the request is due; one folded into a pending request takes the sooner time
	dueTime time.Time
}

// One env as the command reaches it: what its services read and the queue its taskworker claims from.
type evsPurgeTurnOnEnv interface {
	// The env's format2 gate as its services read sidecar.yml through server's config loader.
	ReadFormat2Gate(ctx context.Context) (*evsFormat2GateRead, error)
	// Schedules evsservice.EvsSweepTurnOnTaskRequest() on the env's task queue, due at dueTime, and says what it
	// scheduled. It runs no pass.
	ScheduleTurnOn(ctx context.Context, dueTime time.Time) (*evsScheduledTask, error)
}

// How one run reaches its env. Tests replace the clock and the env.
type evsPurgeTurnOnSettings struct {
	// Bounds the whole run, the open, the read and the schedule, so an env whose Postgres does not answer fails the
	// runbook step rather than holding it. Non-positive expires at once, so nothing is read or scheduled.
	timeout time.Duration
	// How long after the run the pass's start is due: two of §19.11's 60 s rereads, so a taskworker holding the
	// file this read reads purge on before the pass reads it (see the file header). Non-positive is due at once.
	startDelay time.Duration
	// The time a run is judged at.
	now func() time.Time
	// Opens env as its services and its taskworker see it, refusing an env it cannot reach as itself.
	openEnv func(ctx context.Context, env string) (evsPurgeTurnOnEnv, error)
}

// The settings of a run. A caller changes its own copy.
func defaultEvsPurgeTurnOnSettings() *evsPurgeTurnOnSettings {
	return &evsPurgeTurnOnSettings{
		// one file read and one Postgres write, each well under a second on a healthy env, with room for a pool's
		// first connect
		timeout:    30 * time.Second,
		startDelay: 2 * time.Minute,
		now:        time.Now,
		openEnv:    openServerEvsPurgeTurnOnEnv,
	}
}

// The env as server would reach it: its format2 gate through server.CurrentSidecarConfig().Format2(), read under a
// WARP_ENV that names env, and evsservice.EvsSweepTurnOnTaskRequest() scheduled through sidecarhost.Tasks on its
// Postgres. None of that links into this module (see the file header), so this refuses every env, and the command
// reads and schedules nothing.
func openServerEvsPurgeTurnOnEnv(ctx context.Context, env string) (evsPurgeTurnOnEnv, error) {
	return nil, fmt.Errorf(
		"nothing was scheduled: reading %s's format2 gate through server's config loader and scheduling on its task queue need server's packages and evsservice, which sidecarctl may not link until the open questions dns-m0-01-sidecarctl-server-link and ops-m10-01-purge-turn-on-links are answered (C-23, §4.4, §4.7)",
		env,
	)
}

// `sidecarctl evs purge-turn-on <env>`, against the env as server reaches it.
func runEvsPurgeTurnOn(args []string, out io.Writer) error {
	return runEvsPurgeTurnOnWithSettings(args, defaultEvsPurgeTurnOnSettings(), out)
}

// Parses the command line and turns the purge on in the env it names.
func runEvsPurgeTurnOnWithSettings(args []string, settings *evsPurgeTurnOnSettings, out io.Writer) error {
	flags := flag.NewFlagSet("evs purge-turn-on", flag.ContinueOnError)
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return sidecarctlUsageErrorf("want <env>, got %q", positionals)
	}
	env := positionals[0]
	if !evsPurgeTurnOnEnvRegex.MatchString(env) {
		return sidecarctlUsageErrorf("%q is not an env's name, one directory under config/", env)
	}
	return turnOnEvsPurge(env, settings, out)
}

// Schedules the turn-on pass on env once its format2 gate is enabled with purge on, and prints what it scheduled to
// out. Every refusal and failure returns an error, and only a schedule the queue took prints anything.
func turnOnEvsPurge(env string, settings *evsPurgeTurnOnSettings, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), settings.timeout)
	defer cancel()
	// checked before each step, so a run past its bound starts nothing more, whatever the env does with ctx
	expired := func() error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("nothing was scheduled on %s: the run's %v bound passed: %w", env, settings.timeout, err)
		}
		return nil
	}
	if err := expired(); err != nil {
		return err
	}
	envReach, err := settings.openEnv(ctx, env)
	if err != nil {
		return err
	}
	if envReach == nil {
		return fmt.Errorf("nothing was scheduled: %s opened as no env", env)
	}
	if err := expired(); err != nil {
		return err
	}
	gate, err := envReach.ReadFormat2Gate(ctx)
	if err == nil && gate == nil {
		err = errors.New("the read answered no gate")
	}
	if err != nil {
		return fmt.Errorf("%s's format2 gate does not read, so nothing was scheduled: %w", env, err)
	}
	if !gate.enabled {
		return fmt.Errorf("format2 is not enabled in %s's sidecar.yml, so §19.11's rollout has not reached the purge's turn-on; nothing was scheduled", env)
	}
	if gate.purge != evsFormat2PurgeOn {
		return fmt.Errorf("format2.purge reads %q in %s's sidecar.yml, and a turn-on pass deletes nothing until it reads on; set purge: on in this runbook step, then run this again; nothing was scheduled", gate.purge, env)
	}
	if err := expired(); err != nil {
		return err
	}
	dueTime := settings.now().Add(max(settings.startDelay, 0))
	scheduled, err := envReach.ScheduleTurnOn(ctx, dueTime)
	if err == nil && scheduled == nil {
		err = errors.New("the queue answered no request")
	}
	if err != nil {
		return fmt.Errorf("the turn-on pass was not scheduled on %s's task queue: %w", env, err)
	}
	fmt.Fprintf(out, "scheduled %s under once key %s on %s's task queue, due %s\n",
		scheduled.name, scheduled.onceKey, env, scheduled.dueTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "taskworker's evs host then scans each metadata cluster and passes over each repo at format 2, deleting what purge: observe kept wherever its own sidecar.yml reads purge: on; a run while this is pending folds into it\n")
	return nil
}
