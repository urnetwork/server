// One synthetic corpus exercises the task, full-text SQL, and log boundaries.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

type invalidDestinationResetTestCase struct {
	name     string
	task     string
	raw      string
	class    string
	logMatch bool
}

// The current format is the controller's two wrapped errors; legacy stored
// errors remain readable. Neither a lone suffix nor a mixed format proves it.
func invalidDestinationResetTestCases() []invalidDestinationResetTestCase {
	current := fmt.Errorf("[%s]Payment create transaction error: %w; invalid destination reset error: %w",
		"synthetic-payment", errors.New("Invalid destination address."), errors.New("synthetic persistence boundary")).Error()
	legacy := "[synthetic-payment]Payment create transaction error = Invalid destination address.; invalid destination reset error = synthetic persistence boundary"
	cases := []invalidDestinationResetTestCase{
		{name: "current producer", task: "AdvancePayment", raw: current, class: taskErrorClassInvalidDestinationResetFailed, logMatch: true},
		{name: "legacy producer", task: "AdvancePayment", raw: legacy, class: taskErrorClassInvalidDestinationResetFailed, logMatch: true},
		{name: "uppercase", task: "AdvancePayment", raw: strings.ToUpper(current), class: taskErrorClassInvalidDestinationResetFailed, logMatch: true},
		{name: "multiline", task: "AdvancePayment", raw: strings.Replace(current, "Invalid destination address.", "Invalid destination address.\nsynthetic processor detail", 1), class: taskErrorClassInvalidDestinationResetFailed, logMatch: true},
		{name: "beyond representative sample", task: "AdvancePayment", raw: strings.Replace(current, "Invalid destination address.", "Invalid destination address. "+strings.Repeat("synthetic processor detail ", 12), 1), class: taskErrorClassInvalidDestinationResetFailed, logMatch: true},
		{name: "empty", task: "AdvancePayment", raw: "", class: taskErrorClassUnclassified},
		{name: "healthy lookalike", task: "AdvancePayment", raw: "guarded reset succeeded; invalid destination reset error", class: taskErrorClassUnclassified},
		{name: "ordinary rejection", task: "AdvancePayment", raw: "Payment create transaction error: Invalid destination address.", class: taskErrorClassProcessorInvalidDestination},
		{name: "current suffix only", task: "AdvancePayment", raw: "Invalid destination address.; invalid destination reset error: synthetic", class: taskErrorClassProcessorInvalidDestination},
		{name: "legacy suffix only", task: "AdvancePayment", raw: "Invalid destination address.; invalid destination reset error = synthetic", class: taskErrorClassProcessorInvalidDestination},
		{name: "mixed legacy prefix", task: "AdvancePayment", raw: strings.Replace(current, "transaction error: ", "transaction error = ", 1), class: taskErrorClassProcessorInvalidDestination},
		{name: "mixed current prefix", task: "AdvancePayment", raw: strings.Replace(legacy, "transaction error = ", "transaction error: ", 1), class: taskErrorClassProcessorInvalidDestination},
		{name: "empty current suffix", task: "AdvancePayment", raw: "Payment create transaction error: Invalid destination address.; invalid destination reset error: ", class: taskErrorClassProcessorInvalidDestination},
		{name: "empty legacy suffix", task: "AdvancePayment", raw: "Payment create transaction error = Invalid destination address.; invalid destination reset error = ", class: taskErrorClassProcessorInvalidDestination},
		{name: "whitespace suffix", task: "AdvancePayment", raw: "Payment create transaction error: Invalid destination address.; invalid destination reset error: \t\n", class: taskErrorClassProcessorInvalidDestination},
		// Logs carry the controller-owned message; durable task classification
		// additionally requires the AdvancePayment function identity.
		{name: "foreign task", task: "SyntheticTask", raw: current, class: taskErrorClassProcessorInvalidDestination, logMatch: true},
	}
	for _, suffix := range []string{
		"Invalid payment.", "statement timeout (SQLSTATE 57014)",
		"failed to deallocate cached statement(s): conn closed", "context canceled",
	} {
		cases = append(cases, invalidDestinationResetTestCase{
			name: "reset precedence " + suffix, task: "AdvancePayment",
			raw:   strings.Replace(current, "synthetic persistence boundary", suffix, 1),
			class: taskErrorClassInvalidDestinationResetFailed, logMatch: true,
		})
	}
	return cases
}

// Exercise the actual task reducer and registered log class against one corpus.
func TestInvalidDestinationResetProducerFormats(t *testing.T) {
	var resetClass *logClass
	for i := range logClasses {
		if logClasses[i].name == "payout-invalid-destination-reset-failed" {
			resetClass = &logClasses[i]
			break
		}
	}
	if resetClass == nil {
		t.Fatal("missing reset-failure log class")
	}
	for _, testCase := range invalidDestinationResetTestCases() {
		if got := classifyTaskError(testCase.task, testCase.raw); got != testCase.class {
			t.Errorf("%s: task class=%q, want %q", testCase.name, got, testCase.class)
		}
		if got := resetClass.re.MatchString(testCase.raw); got != testCase.logMatch {
			t.Errorf("%s: log reset match=%t, want %t", testCase.name, got, testCase.logMatch)
		}
	}
}

// Run the production aggregate on complete errors, including ones whose reset
// marker is beyond its bounded representative sample.
func TestInvalidDestinationResetSqlMatchesProducerFormats(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE TEMP TABLE pending_task (
					function_name text,
					reschedule_error_count integer,
					run_at timestamp,
					claim_time timestamp,
					reschedule_error text,
					run_max_time_seconds integer
				) ON COMMIT DROP
			`))
			for _, testCase := range invalidDestinationResetTestCases() {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pg_temp.pending_task`))
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO pg_temp.pending_task (
						function_name, reschedule_error_count, run_at, reschedule_error, run_max_time_seconds
					) VALUES ($1, 1, now() + interval '1 hour', $2, 120)
				`, "synthetic.example."+testCase.task, testCase.raw))
				var task, classSummary string
				var classCount int
				server.Raise(tx.QueryRow(ctx, `SELECT task, cause_class_count, cause_summary FROM (`+
					strings.TrimSuffix(strings.TrimSpace(taskFailureSummarySQL), ";")+`) AS summary`).Scan(&task, &classCount, &classSummary))
				class, _, _ := strings.Cut(classSummary, "=")
				if task != testCase.task || classCount != 1 || fixedTaskErrorClass(class) != testCase.class {
					t.Errorf("%s: task=%q classes=%d summary=%q, want %q", testCase.name, task, classCount, classSummary, testCase.class)
				}
			}
		})
	})
}

// A failed query's partial output cannot establish a reset event or clear it.
func TestInvalidDestinationResetUnavailableLogsRemainUnknown(t *testing.T) {
	source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return invalidDestinationResetTestCases()[0].raw, errors.New("synthetic log query unavailable")
	}}
	env, err := newProbeEnv(syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (logWindowProbe{}).check(context.Background(), env)
	if err == nil || len(findings) != 0 {
		t.Fatalf("unavailable log window became an observation: findings=%d err=%v", len(findings), err)
	}
}
