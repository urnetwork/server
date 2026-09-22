package monitor

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
)

const (
	taskErrorClassConcurrentSettled             = "concurrent-settled"
	taskErrorClassPostgresLocalBufferExhaustion = "postgres-local-buffer-exhaustion"
	taskErrorClassWalletInsufficient            = "wallet-insufficient"
	taskErrorClassIdleTransactionTimeout        = "idle-transaction-timeout"
	taskErrorClassConnectionCleanupDeadline     = "connection-cleanup-deadline"
	taskErrorClassProcessorRateLimit            = "processor-rate-limit"
	taskErrorClassInvalidDestinationResetFailed = "invalid-destination-reset-failed"
	taskErrorClassProcessorInvalidDestination   = "processor-invalid-destination"
	taskErrorClassProcessorBadRequest           = "processor-bad-request"
	taskErrorClassSchemaObjectMissing           = "schema-object-missing"
	taskErrorClassPostgresStatementTimeout      = "postgres-statement-timeout"
	taskErrorClassDeadlineTimeout               = "deadline-timeout"
	taskErrorClassContextCanceled               = "context-canceled"
	taskErrorClassDrained                       = "drained"
	taskErrorClassTargetNotFound                = "target-not-found"
	taskErrorClassUnclassified                  = "unclassified"
)

// observationStateUnavailableError marks a reducer that completed, but could
// not prove the required live state. It deliberately carries no raw host or
// process details: those do not belong in Alert output.
type observationStateUnavailableError struct{}

func (*observationStateUnavailableError) Error() string {
	return "required observation state is unavailable"
}

var taskDeadlineTimeoutPattern = regexp.MustCompile(`(?i)^timeout(?:\s+\[[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\])?$`)

// classifyTaskError reduces task-owned or dependency-owned error text to a
// fixed vocabulary before it reaches Alert Markdown. Callers may still use the
// original text in memory to select established operational guidance, but no
// identifier, address, stack frame, or provider-controlled suffix survives
// this boundary.
func classifyTaskError(taskName, value string) string {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	switch {
	case taskName == "AdvancePayment" && strings.Contains(lower, "; invalid destination reset error = "):
		return taskErrorClassInvalidDestinationResetFailed
	case strings.Contains(lower, "statement timeout") && strings.Contains(lower, "sqlstate 57014"):
		return taskErrorClassPostgresStatementTimeout
	case taskName == "CloseExpiredContracts" && strings.Contains(lower, "contract already closed with outcome settled:"):
		return taskErrorClassConcurrentSettled
	case taskName == "Payout" && strings.Contains(lower, "no empty local buffer available") && strings.Contains(lower, "sqlstate 53000"):
		return taskErrorClassPostgresLocalBufferExhaustion
	case strings.Contains(lower, "asset amount owned by the wal") || strings.Contains(lower, "insufficient token balance"):
		return taskErrorClassWalletInsufficient
	case taskName == "Payout" && strings.Contains(lower, "pgconn.connlockerror=conn closed"):
		return taskErrorClassIdleTransactionTimeout
	case strings.Contains(lower, "failed to deallocate cached statement(s): conn closed"):
		return taskErrorClassConnectionCleanupDeadline
	case strings.Contains(lower, "429 too many requests"):
		return taskErrorClassProcessorRateLimit
	case strings.Contains(lower, "invalid destination address"):
		return taskErrorClassProcessorInvalidDestination
	case strings.Contains(lower, "400 bad request"):
		return taskErrorClassProcessorBadRequest
	case strings.Contains(lower, "sqlstate 42703") ||
		strings.Contains(lower, "sqlstate 42p01") ||
		strings.Contains(lower, "sqlstate 42883") ||
		strings.Contains(lower, "sqlstate 42704"):
		return taskErrorClassSchemaObjectMissing
	case taskDeadlineTimeoutPattern.MatchString(trimmed):
		return taskErrorClassDeadlineTimeout
	case strings.HasPrefix(lower, "drained:"):
		return taskErrorClassDrained
	case strings.Contains(lower, "context canceled") || strings.Contains(lower, "interrupted: done"):
		return taskErrorClassContextCanceled
	case strings.Contains(lower, "target not found"):
		return taskErrorClassTargetNotFound
	default:
		return taskErrorClassUnclassified
	}
}

// fixedTaskErrorClass accepts only values produced by the bounded database
// classifier; legacy "other" becomes the public unclassified value.
func fixedTaskErrorClass(value string) string {
	switch strings.TrimSpace(value) {
	case taskErrorClassConcurrentSettled,
		taskErrorClassPostgresLocalBufferExhaustion,
		taskErrorClassWalletInsufficient,
		taskErrorClassIdleTransactionTimeout,
		taskErrorClassConnectionCleanupDeadline,
		taskErrorClassProcessorRateLimit,
		taskErrorClassInvalidDestinationResetFailed,
		taskErrorClassProcessorInvalidDestination,
		taskErrorClassProcessorBadRequest,
		taskErrorClassSchemaObjectMissing,
		taskErrorClassPostgresStatementTimeout,
		taskErrorClassDeadlineTimeout,
		taskErrorClassContextCanceled,
		taskErrorClassDrained,
		taskErrorClassTargetNotFound,
		taskErrorClassUnclassified:
		return strings.TrimSpace(value)
	case "other":
		return taskErrorClassUnclassified
	default:
		return ""
	}
}

// representativeTaskErrorClass prefers the PostgreSQL-owned cause class when
// a family has exactly one class. Mixed or older synthetic row shapes fall
// back to the same fixed renderer used by lifecycle logs.
func representativeTaskErrorClass(taskName, rawValue string, classCount int, classSummary string) string {
	if classCount == 1 {
		entry, _, _ := strings.Cut(strings.TrimSpace(classSummary), "=")
		if class := fixedTaskErrorClass(entry); class != "" {
			return class
		}
	}
	return classifyTaskError(taskName, rawValue)
}

const (
	observationErrorClassTimeout           = "observation-timeout"
	observationErrorClassCanceled          = "observation-canceled"
	observationErrorClassUnreachable       = "observation-unreachable"
	observationErrorClassCounterReset      = "observation-counter-reset"
	observationErrorClassContractMismatch  = "observation-contract-mismatch"
	observationErrorClassStateUnavailable  = "observation-state-unavailable"
	observationErrorClassBoundExceeded     = "observation-bound-exceeded"
	observationErrorClassInvalidResponse   = "observation-invalid-response"
	observationErrorClassMetricUnavailable = "observation-metric-unavailable"
	observationErrorClassAccessDenied      = "observation-access-denied"
	observationErrorClassCommandFailed     = "observation-command-failed"
	observationErrorClassSSHExit255        = "observation-ssh-exit-255"
	observationErrorClassUnclassified      = "observation-unclassified"
)

// SIGNALS.md §1.7 shared observation taxonomy and observer-route coverage gap.
// classifyObservationError gives monitor-internal observation failures the
// same fixed-output boundary as task errors. It deliberately does not return
// or embed the underlying error text.
func classifyObservationError(err error) string {
	if err == nil {
		return observationErrorClassUnclassified
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return observationErrorClassTimeout
	}
	if errors.Is(err, context.Canceled) {
		return observationErrorClassCanceled
	}

	lower := strings.ToLower(err.Error())
	var sshFailure *sshCommandError
	var exitFailure *exec.ExitError
	sshExit255 := errors.As(err, &sshFailure) && errors.As(sshFailure, &exitFailure) && exitFailure.ExitCode() == 255
	var unreachable *unreachableError
	var signupRouteMetricsUnavailable *signupRouteMetricsUnavailableError
	var stateUnavailable *observationStateUnavailableError
	if errors.As(err, &signupRouteMetricsUnavailable) {
		return observationErrorClassMetricUnavailable
	}
	if errors.As(err, &stateUnavailable) {
		return observationErrorClassStateUnavailable
	}
	if errors.As(err, &unreachable) {
		if strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") {
			return observationErrorClassTimeout
		}
		return observationErrorClassUnreachable
	}
	switch {
	case strings.Contains(lower, "monotonic") && strings.Contains(lower, "counter decreased"):
		return observationErrorClassCounterReset
	case strings.Contains(lower, "observation contract") ||
		strings.Contains(lower, "source contract") ||
		strings.Contains(lower, "counter descriptor is unavailable") ||
		strings.Contains(lower, "unsupported peer diagnostics") ||
		strings.Contains(lower, "peer diagnostics are absent"):
		return observationErrorClassContractMismatch
	case strings.Contains(lower, "durable state") &&
		(strings.Contains(lower, "unreadable") || strings.Contains(lower, "unavailable")):
		return observationErrorClassStateUnavailable
	case strings.Contains(lower, "identity bound exceeded"):
		return observationErrorClassBoundExceeded
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return observationErrorClassTimeout
	case strings.Contains(lower, "context canceled"):
		return observationErrorClassCanceled
	case strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "access denied") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "http 401") ||
		strings.Contains(lower, "http 403"):
		return observationErrorClassAccessDenied
	case sshExit255:
		return observationErrorClassSSHExit255
	case strings.Contains(lower, "exit status"):
		return observationErrorClassCommandFailed
	case strings.Contains(lower, "parse") ||
		strings.Contains(lower, "decode") ||
		strings.Contains(lower, "invalid response") ||
		strings.Contains(lower, "unexpected response") ||
		strings.HasPrefix(lower, "peer log "):
		return observationErrorClassInvalidResponse
	default:
		return observationErrorClassUnclassified
	}
}

// observationFailureAction keeps the SSH status discriminator identical for
// whole-signal and partial-target visibility without attributing a route cause.
func observationFailureAction(errorClass, fallback string) string {
	if errorClass == observationErrorClassSSHExit255 {
		return "Determine whether status 255 came from SSH transport/authentication or the remote command. Correlate failures across independent targets with bounded observer route and intended VPN-session evidence before attributing local overlay loss. Restore the proved observation path and rerun every affected signal."
	}
	return fallback
}
