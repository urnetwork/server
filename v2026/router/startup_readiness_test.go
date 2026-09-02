package router

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// TestStartupReadiness covers both latch branches of the shared startup
// readiness helper (used by the api, connect, and taskworker mains): with
// the test env's pg+redis answering it latches ready; with a dead context
// (standing in for unreachable dependencies) it latches the `error not
// ready ...` status the deploy poll fails on.
func TestStartupReadiness(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// the latch is package state; restore the default for other tests
		defer warpStatusOverride.Store(nil)

		ctx := context.Background()

		// ready: checks pass and latch ok
		if err := StartupReadiness(ctx); err != nil {
			t.Fatalf("startup readiness failed against the test env: %s", err)
		}
		status, err := collectStatus(ctx)
		if err != nil || status != "ok" {
			t.Fatalf("ready latch = %q (%v), want ok", status, err)
		}

		// not ready: a dead context fails the checks and latches an error
		// status the deploy poll fails on
		deadCtx, deadCancel := context.WithCancel(ctx)
		deadCancel()
		checkErr := StartupReadiness(deadCtx)
		if checkErr == nil {
			t.Fatalf("startup readiness passed with a dead context")
		}
		status, err = collectStatus(ctx)
		if err != nil {
			t.Fatalf("not-ready latch error: %s", err)
		}
		if !strings.HasPrefix(status, "error not ready: ") {
			t.Fatalf("not-ready latch = %q, want `error not ready: ...`", status)
		}
		if !warpctlIsErrorRegex.MatchString(status) {
			t.Fatalf("not-ready latch %q does not fail the warpctl poll", status)
		}

		// a later successful pass re-latches ready (restart-in-place)
		if err := StartupReadiness(ctx); err != nil {
			t.Fatalf("re-latch failed: %s", err)
		}
		status, _ = collectStatus(ctx)
		if status != "ok" {
			t.Fatalf("re-latch = %q, want ok", status)
		}
	})
}

func TestValidateMigrationHead(t *testing.T) {
	tests := []struct {
		name            string
		databaseVersion int
		requiredVersion int
		wantError       bool
	}{
		{
			name:            "database behind new binary",
			databaseVersion: 596,
			requiredVersion: 597,
			wantError:       true,
		},
		{
			name:            "database equals binary",
			databaseVersion: 597,
			requiredVersion: 597,
		},
		{
			name:            "database ahead of rollback binary",
			databaseVersion: 597,
			requiredVersion: 590,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateMigrationHead(test.databaseVersion, test.requiredVersion)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "database migration head") {
					t.Fatalf("validateMigrationHead(%d, %d) = %v, want migration-head error", test.databaseVersion, test.requiredVersion, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateMigrationHead(%d, %d) = %v, want nil", test.databaseVersion, test.requiredVersion, err)
			}
		})
	}
}
