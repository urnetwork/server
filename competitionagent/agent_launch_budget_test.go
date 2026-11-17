//go:build linux

// Startup admission is not a short control RPC, but remains inside the one
// original evaluation deadline. Context deadlines force the regression directly.
package competitionagent

import (
	"context"
	"testing"
	"time"
)

func TestAgentLaunchOwnsIndependentStartupBudget(t *testing.T) {
	fixture := newAgentTestFixture(t)
	fixture.agent.settings.ControlTimeout = time.Minute
	fixture.agent.settings.LaunchTimeout = 5 * time.Minute
	phases := 0
	fixture.containment.before = func(ctx context.Context, operation string, _ cleanupPlan, _ bool) error {
		if operation == "prepare" || operation == "start" {
			deadline, ok := ctx.Deadline()
			if !ok || !deadline.After(time.Now().Add(2*time.Minute)) {
				t.Fatal("native startup inherited the shorter RPC budget", operation, deadline)
			}
			phases++
		}
		return nil
	}
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	if phases != 2 || fixture.runner.launchCount() != 1 {
		t.Fatal("startup skipped its owned readiness boundary", phases)
	}
}

func TestAgentLaunchCannotExtendOriginalEvaluationDeadline(t *testing.T) {
	fixture := newAgentTestFixture(t)
	fixture.now = time.Now().UTC()
	fixture.manifest.Identity.StartedAt = fixture.now
	fixture.manifest.Identity.DeadlineAt = fixture.now.Add(time.Minute)
	fixture.agent.settings.ControlTimeout = 5 * time.Second
	fixture.agent.settings.LaunchTimeout = 5 * time.Minute
	phases := 0
	fixture.containment.before = func(ctx context.Context, operation string, _ cleanupPlan, _ bool) error {
		if operation == "prepare" || operation == "start" {
			deadline, ok := ctx.Deadline()
			if !ok || !deadline.Equal(fixture.manifest.Identity.DeadlineAt) {
				t.Fatal("startup changed the original evaluation clock", operation, deadline)
			}
			phases++
		}
		return nil
	}
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	if phases != 2 || fixture.runner.launchCount() != 1 {
		t.Fatal("bounded startup was not issued exactly once", phases)
	}
}
