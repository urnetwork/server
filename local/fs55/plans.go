package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// FS-55's four phases (§22.8, R-OPS-5, R-PHASE-8; connect/CODESTYLE.md's deploy
// order), over two releases: N, the release running when a Gn-out's group
// applies, and N+1, the release that registers it.
//
//  1. migrate with N and start N: N's smoke holds
//  2. migrate with N+1 while N serves: N's smoke still holds, and N's query
//     shapes still plan on the migrated schema with sequential scans off
//  3. restart N on N+1's schema, the rollback: N's smoke holds, since a release
//     accepts a database ahead of its own head (validateMigrationHead)
//  4. start N+1 with the group's gate on: N+1's smoke holds, the group's own
//     routes included
//
// A phase that fails ends the run; the phases after it report not run, never
// pass.

// One release: how to migrate a database with it and start it serving there.
type fs55Release interface {
	// N or N+1, for the report.
	Name() string
	// Applies this release's migrations to the scratch database.
	Migrate(ctx context.Context) error
	// Starts this release serving on the scratch database, its sidecar.yml the
	// text given, "" for none, and returns once it answers or fails.
	Start(ctx context.Context, sidecarYaml string) (fs55Process, error)
	// The query shapes this release runs, which CODESTYLE has planned on the
	// migrated schema with sequential scans off.
	QueryShapes() []string
}

// A started release.
type fs55Process interface {
	// Where its HTTP routes answer, with no trailing slash.
	BaseUrl() string
	// Stops it and returns once it has exited.
	Stop() error
}

// Plans a query shape on the scratch database with sequential scans off and
// returns why it cannot, nil when it plans.
type fs55Explainer func(ctx context.Context, query string) error

// One run: the releases, the group the run is about, the routes each smoke
// asks, and the gate that turns the group on in phase 4.
type fs55Plan struct {
	groupName string
	n         fs55Release
	n1        fs55Release
	// asked of every serving release
	routes []fs55Route
	// asked of N+1 alone in phase 4, with its gate on
	groupRoutes []fs55Route
	// N+1's sidecar.yml in phase 4
	gateYaml string
	explain  fs55Explainer
	settings *fs55Settings
}

// The harness's tunables.
type fs55Settings struct {
	// How long one phase may take, start and smoke included. Past it the phase
	// fails, so a wedged release cannot hold a Gn-out open.
	PhaseTimeout time.Duration
	// How long a smoke waits for one route's answer.
	RouteTimeout time.Duration
	// How often a starting release's /status is asked whether it is ready.
	ReadyPollInterval time.Duration
	// How long a stopped release may drain before it is killed.
	StopTimeout time.Duration
}

func defaultFs55Settings() *fs55Settings {
	return &fs55Settings{
		PhaseTimeout:      10 * time.Minute,
		RouteTimeout:      30 * time.Second,
		ReadyPollInterval: 250 * time.Millisecond,
		StopTimeout:       time.Minute,
	}
}

// One phase's outcome.
type fs55PhaseResult struct {
	number int
	name   string
	// passed, failed or not run
	outcome string
	// why it failed, or what it checked
	details []string
}

const (
	fs55Passed = "passed"
	fs55Failed = "failed"
	fs55NotRun = "not run"
)

// A run's outcome, phase by phase.
type fs55Report struct {
	groupName string
	nName     string
	n1Name    string
	phases    []*fs55PhaseResult
}

// Whether every phase passed.
func (self *fs55Report) Passed() bool {
	for _, phase := range self.phases {
		if phase.outcome != fs55Passed {
			return false
		}
	}
	return len(self.phases) == 4
}

// The report as Markdown, the form server/local/sidecar-gate reads.
func (self *fs55Report) Write(out io.Writer) {
	fmt.Fprintf(out, "## FS-55: %s, %s to %s\n\n", self.groupName, self.nName, self.n1Name)
	fmt.Fprintf(out, "| phase | outcome |\n|---|---|\n")
	for _, phase := range self.phases {
		fmt.Fprintf(out, "| %d. %s | %s |\n", phase.number, phase.name, phase.outcome)
	}
	for _, phase := range self.phases {
		if len(phase.details) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n### %d. %s\n\n", phase.number, phase.name)
		for _, detail := range phase.details {
			fmt.Fprintf(out, "- %s\n", detail)
		}
	}
	verdict := "FS-55 passed"
	if !self.Passed() {
		verdict = "FS-55 failed"
	}
	fmt.Fprintf(out, "\n%s\n", verdict)
}

// Runs the four phases in order and stops at the first that fails.
func runFs55(ctx context.Context, plan *fs55Plan) *fs55Report {
	report := &fs55Report{groupName: plan.groupName, nName: plan.n.Name(), n1Name: plan.n1.Name()}
	names := []string{
		"migrate with N and start N",
		"migrate with N+1 while N serves",
		"restart N on N+1's schema (rollback)",
		"start N+1 with the group's gate on",
	}
	for index, name := range names {
		report.phases = append(report.phases, &fs55PhaseResult{number: index + 1, name: name, outcome: fs55NotRun})
	}

	var running fs55Process
	stop := func() error {
		if running == nil {
			return nil
		}
		process := running
		running = nil
		return process.Stop()
	}
	defer stop()

	phase := func(index int, body func(ctx context.Context, result *fs55PhaseResult) error) bool {
		result := report.phases[index]
		phaseCtx, cancel := context.WithTimeout(ctx, plan.settings.PhaseTimeout)
		defer cancel()
		if err := body(phaseCtx, result); err != nil {
			result.outcome = fs55Failed
			result.details = append(result.details, err.Error())
			return false
		}
		result.outcome = fs55Passed
		return true
	}
	smoke := func(ctx context.Context, result *fs55PhaseResult, release string, routes []fs55Route) error {
		results := smokeRoutes(ctx, running.BaseUrl(), routes, plan.settings.RouteTimeout)
		failures := []string{}
		for _, routeResult := range results {
			result.details = append(result.details, fmt.Sprintf("%s %s", release, routeResult))
			if !routeResult.held() {
				failures = append(failures, routeResult.String())
			}
		}
		if 0 < len(failures) {
			return fmt.Errorf("%s's smoke did not hold: %s", release, strings.Join(failures, "; "))
		}
		return nil
	}

	if !phase(0, func(ctx context.Context, result *fs55PhaseResult) error {
		if err := plan.n.Migrate(ctx); err != nil {
			return fmt.Errorf("N's migrations: %w", err)
		}
		process, err := plan.n.Start(ctx, "")
		if err != nil {
			return fmt.Errorf("N did not start: %w", err)
		}
		running = process
		return smoke(ctx, result, "N", plan.routes)
	}) {
		return report
	}

	if !phase(1, func(ctx context.Context, result *fs55PhaseResult) error {
		if err := plan.n1.Migrate(ctx); err != nil {
			return fmt.Errorf("N+1's migrations while N serves: %w", err)
		}
		if err := smoke(ctx, result, "N", plan.routes); err != nil {
			return err
		}
		for _, query := range plan.n.QueryShapes() {
			if err := plan.explain(ctx, query); err != nil {
				return fmt.Errorf("N's query shape does not plan on N+1's schema with sequential scans off: %s: %w", query, err)
			}
			result.details = append(result.details, "N's query shape plans with sequential scans off: "+query)
		}
		return nil
	}) {
		return report
	}

	if !phase(2, func(ctx context.Context, result *fs55PhaseResult) error {
		if err := stop(); err != nil {
			return fmt.Errorf("N did not stop: %w", err)
		}
		process, err := plan.n.Start(ctx, "")
		if err != nil {
			return fmt.Errorf("N did not restart on N+1's schema: %w", err)
		}
		running = process
		return smoke(ctx, result, "N", plan.routes)
	}) {
		return report
	}

	phase(3, func(ctx context.Context, result *fs55PhaseResult) error {
		if err := stop(); err != nil {
			return fmt.Errorf("N did not stop: %w", err)
		}
		process, err := plan.n1.Start(ctx, plan.gateYaml)
		if err != nil {
			return fmt.Errorf("N+1 did not start with the group's gate on: %w", err)
		}
		running = process
		if len(plan.groupRoutes) == 0 {
			result.details = append(result.details, plan.groupName+" names no route of its own yet, so phase 4 asks only the common routes")
		}
		return smoke(ctx, result, "N+1", append(append([]fs55Route{}, plan.routes...), plan.groupRoutes...))
	})
	return report
}
