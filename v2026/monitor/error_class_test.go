package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestClassifyTaskErrorUsesFixedAllowlist(t *testing.T) {
	tests := []struct {
		name string
		task string
		raw  string
		want string
	}{
		{"statement timeout", "ReconcileNetEscrow", "wrapped: canceling statement due to statement timeout (SQLSTATE 57014)", taskErrorClassPostgresStatementTimeout},
		{"concurrent settlement", "CloseExpiredContracts", "force close contract synthetic: Contract already closed with outcome settled: synthetic", taskErrorClassConcurrentSettled},
		{"local buffer", "Payout", "no empty local buffer available (SQLSTATE 53000)", taskErrorClassPostgresLocalBufferExhaustion},
		{"wallet", "AdvancePayment", "insufficient token balance", taskErrorClassWalletInsufficient},
		{"idle transaction", "Payout", "pgconn.ConnLockError=conn closed", taskErrorClassIdleTransactionTimeout},
		{"cleanup deadline", "UpdateReliabilities", "failed to deallocate cached statement(s): conn closed", taskErrorClassConnectionCleanupDeadline},
		{"rate limit", "AdvancePayment", "429 Too Many Requests", taskErrorClassProcessorRateLimit},
		{"invalid destination", "AdvancePayment", "invalid destination address", taskErrorClassProcessorInvalidDestination},
		{"bad request", "AdvancePayment", "400 Bad Request", taskErrorClassProcessorBadRequest},
		{"missing schema", "SyntheticTask", "undefined relation (SQLSTATE 42P01)", taskErrorClassSchemaObjectMissing},
		{"deadline", "SyntheticTask", "Timeout", taskErrorClassDeadlineTimeout},
		{"deadline with synthetic id", "SyntheticTask", "Timeout [aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa]", taskErrorClassDeadlineTimeout},
		{"drain", "SyntheticTask", "Drained: context canceled", taskErrorClassDrained},
		{"canceled", "SyntheticTask", "context canceled", taskErrorClassContextCanceled},
		{"target", "SyntheticTask", "Target not found", taskErrorClassTargetNotFound},
		{"empty", "SyntheticTask", "", taskErrorClassUnclassified},
		{"hostile unknown", "SyntheticTask", "provider supplied Timeout for 192.0.2.90 task=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa pointer=0xdeadbeef goroutine synthetic.Stack", taskErrorClassUnclassified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyTaskError(test.task, test.raw); got != test.want {
				t.Fatalf("classifyTaskError() = %q, want %q", got, test.want)
			}
			if test.raw != "" && strings.Contains(test.want, test.raw) {
				t.Fatalf("fixed class unexpectedly contains raw input %q", test.raw)
			}
		})
	}
}

func TestRepresentativeTaskErrorClassUsesBoundedDatabaseClass(t *testing.T) {
	raw := "force close contract synthetic: Contract already closed with outcome settled: synthetic\nhostile cleanup address=2001:db8::90"
	if got := representativeTaskErrorClass("CloseExpiredContracts", raw, 1, "other=1"); got != taskErrorClassUnclassified {
		t.Fatalf("representative class = %q, want %q", got, taskErrorClassUnclassified)
	}
	if got := representativeTaskErrorClass("AdvancePayment", raw, 1, "wallet-insufficient=4"); got != taskErrorClassWalletInsufficient {
		t.Fatalf("representative class = %q, want %q", got, taskErrorClassWalletInsufficient)
	}
	if got := representativeTaskErrorClass("AdvancePayment", raw, 1, "hostile-provider-class=1"); got != taskErrorClassUnclassified {
		t.Fatalf("unknown database class = %q, want %q", got, taskErrorClassUnclassified)
	}
	for _, class := range []string{
		taskErrorClassPostgresStatementTimeout,
		taskErrorClassDeadlineTimeout,
		taskErrorClassDrained,
		taskErrorClassTargetNotFound,
	} {
		if got := representativeTaskErrorClass("SyntheticTask", "hostile conflicting raw input", 1, class+"=1"); got != class {
			t.Errorf("representative class = %q, want database class %q", got, class)
		}
	}
}

func TestTaskFailureSummarySQLMirrorsFixedTaskErrorVocabulary(t *testing.T) {
	for _, class := range []string{
		taskErrorClassConcurrentSettled,
		taskErrorClassPostgresLocalBufferExhaustion,
		taskErrorClassWalletInsufficient,
		taskErrorClassIdleTransactionTimeout,
		taskErrorClassConnectionCleanupDeadline,
		taskErrorClassProcessorRateLimit,
		taskErrorClassProcessorInvalidDestination,
		taskErrorClassProcessorBadRequest,
		taskErrorClassSchemaObjectMissing,
		taskErrorClassPostgresStatementTimeout,
		taskErrorClassDeadlineTimeout,
		taskErrorClassContextCanceled,
		taskErrorClassDrained,
		taskErrorClassTargetNotFound,
	} {
		if !strings.Contains(taskFailureSummarySQL, "THEN '"+class+"'") {
			t.Errorf("task failure SQL is missing fixed class %q", class)
		}
	}
	for _, fragment := range []string{
		"LIKE '%statement timeout%'",
		"LIKE '%sqlstate 57014%'",
		"~* '^timeout([[:space:]]+\\[[0-9a-f]",
		"lower(trim(coalesce(reschedule_error,''))) LIKE 'drained:%'",
		"LIKE '%target not found%'",
		"LIKE '%429 too many requests%'",
		"LIKE '%400 bad request%'",
	} {
		if !strings.Contains(taskFailureSummarySQL, fragment) {
			t.Errorf("task failure SQL is missing classifier fragment %q", fragment)
		}
	}
	drainedIndex := strings.Index(taskFailureSummarySQL, "THEN 'drained'")
	canceledIndex := strings.Index(taskFailureSummarySQL, "THEN 'context-canceled'")
	if drainedIndex < 0 || canceledIndex < 0 || canceledIndex < drainedIndex {
		t.Fatal("task failure SQL must classify drained errors before their context-canceled suffix")
	}
}

func TestClassifyObservationErrorNeverReturnsRawText(t *testing.T) {
	hostile := "provider supplied task=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa address=192.0.2.91 pointer=0xdeadbeef goroutine synthetic.Stack"
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, observationErrorClassUnclassified},
		{"deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), observationErrorClassTimeout},
		{"canceled", fmt.Errorf("wrapped: %w", context.Canceled), observationErrorClassCanceled},
		{"unreachable", &unreachableError{host: "synthetic-edge-a", err: errors.New("connection refused")}, observationErrorClassUnreachable},
		{"unreachable timeout", &unreachableError{host: "synthetic-edge-a", err: errors.New("timeout after synthetic duration")}, observationErrorClassTimeout},
		{"counter reset", errors.New("a monotonic counter decreased within one process generation"), observationErrorClassCounterReset},
		{"contract", errors.New("installed helper predates this observation contract"), observationErrorClassContractMismatch},
		{"state", errors.New("durable state is unreadable"), observationErrorClassStateUnavailable},
		{"bound", errors.New("current child identity bound exceeded"), observationErrorClassBoundExceeded},
		{"access", errors.New("permission denied: " + hostile), observationErrorClassAccessDenied},
		{"http access", errors.New("service returned HTTP 401"), observationErrorClassAccessDenied},
		{"command", errors.New("exit status 1: " + hostile), observationErrorClassCommandFailed},
		{"response", errors.New("decode synthetic response: " + hostile), observationErrorClassInvalidResponse},
		{"unknown", errors.New(hostile), observationErrorClassUnclassified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyObservationError(test.err); got != test.want {
				t.Fatalf("classifyObservationError() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReliabilityDiagnosticRendersOnlyObservationClass(t *testing.T) {
	hostile := "provider supplied task=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa address=2001:db8::92 pointer=0xdeadbeef goroutine synthetic.Stack"
	finding := finding{context: "synthetic context."}
	applyReliabilityTaskDiagnostic(&finding, reliabilityTaskDiagnostic{}, errors.New("decode failure: "+hostile))
	rendered := strings.Join([]string{finding.mechanism, finding.observed, finding.context, finding.action, finding.verify}, " ")
	if !strings.Contains(rendered, "diagnostic_error_class="+observationErrorClassInvalidResponse) {
		t.Fatalf("diagnostic output lost fixed error class: %s", rendered)
	}
	for _, forbidden := range []string{"provider supplied", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "2001:db8::92", "0xdeadbeef", "synthetic.Stack"} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("diagnostic output leaked %q: %s", forbidden, rendered)
		}
	}
}

func TestCannotObserveFindingRendersOnlyObservationClass(t *testing.T) {
	hostile := "provider supplied task=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa address=192.0.2.93 pointer=0xdeadbeef goroutine synthetic.Stack"
	finding := cannotObserveFinding("synthetic-target", errors.New("decode failure: "+hostile))
	alert := alertFromFinding(
		syntheticSettings(&syntheticSource{}),
		"0.0",
		"synthetic-visibility",
		"Synthetic visibility",
		finding,
	)
	if !strings.Contains(alert.Markdown(), "error_class="+observationErrorClassInvalidResponse) {
		t.Fatalf("cannot-observe alert lost its fixed error class:\n%s", alert.Markdown())
	}
	requireAlertOmits(t, alert, "provider supplied", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "192.0.2.93", "0xdeadbeef", "synthetic.Stack")
}
