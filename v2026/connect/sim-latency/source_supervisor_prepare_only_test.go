// Actual command parsing keeps preparation-only opt-in and bounded to one
// reconciliation before any configuration, credential or source access.
package main

import (
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
)

// Required paths and digests are visibly synthetic and are never executed.
func sourceSupervisorPreparationArguments() []string {
	return []string{"supervise", "--enable-automation", "--owner-id=synthetic-owner", "--state-dir=/synthetic/private-state",
		"--worker-command=/synthetic/worker", "--worker-sha256=" + strings.Repeat("a", 64),
		"--prepare-command=/synthetic/prepare", "--prepare-sha256=" + strings.Repeat("b", 64)}
}

// The native usage accepts the narrow mode without model/build brokers, while
// the validator enforces the explicit one-reconciliation execution boundary.
func TestSourceSupervisorPreparationOnlyParsesWithOnce(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	arguments := append(sourceSupervisorPreparationArguments(), "--prepare-only", "--once")
	opts, err := parser.ParseArgs(simulationUsage(), arguments, "synthetic-version")
	if err != nil {
		t.Fatal("preparation-only CLI rejected", err)
	}
	preparationOnly, err := sourceSupervisorPreparationMode(opts)
	if err != nil || !preparationOnly || !optBool(opts, "--once") || optString(opts, "--review-command", "") != "" || optString(opts, "--build-command", "") != "" {
		t.Fatal("bounded preparation-only mode not selected", err)
	}
}

// Standalone preparation-only cannot become an indefinitely running process;
// mode validation needs no main settings, token, model or database access.
func TestSourceSupervisorPreparationOnlyRejectsRepeatingMode(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	arguments := append(sourceSupervisorPreparationArguments(), "--prepare-only")
	opts, err := parser.ParseArgs(simulationUsage(), arguments, "synthetic-version")
	if err != nil {
		t.Fatal("usage did not reach explicit bounded-mode validation", err)
	}
	if preparationOnly, err := sourceSupervisorPreparationMode(opts); err == nil || preparationOnly || !strings.Contains(err.Error(), "requires --once") {
		t.Fatal("repeating preparation-only accepted", err)
	}
}

// Existing one-shot and repeating automation remain normal unless the new
// flag was explicitly supplied; --once alone is not preparation-only.
func TestSourceSupervisorPreparationOnlyDefaultsToNormalAutomation(t *testing.T) {
	for _, once := range []bool{false, true} {
		parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
		arguments := sourceSupervisorPreparationArguments()
		if once {
			arguments = append(arguments, "--once")
		}
		opts, err := parser.ParseArgs(simulationUsage(), arguments, "synthetic-version")
		if err != nil {
			t.Fatal("existing supervisor usage changed", err)
		}
		if preparationOnly, err := sourceSupervisorPreparationMode(opts); err != nil || preparationOnly {
			t.Fatal("preparation-only became a default", err)
		}
	}
}
