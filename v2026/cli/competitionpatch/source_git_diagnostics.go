// Fixed phase/result metadata diagnoses trusted Git without retaining or
// printing candidate paths, Git arguments, output, or command error strings.
package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// Values are source-owned protocol vocabulary, never caller-supplied strings.
type repositoryGitPhase int

const (
	repositoryGitUnknown repositoryGitPhase = iota
	repositoryGitFilters
	repositoryGitHead
	repositoryGitBranch
	repositoryGitStatus
	repositoryGitProtectedTree
	repositoryGitPatchBase
	repositoryGitHelperTree
	repositoryGitHelperIndex
	repositoryGitHelperBytes
	repositoryGitApplyCheck
	repositoryGitApplyIndex
	repositoryGitIndexCheck
	repositoryGitCommit
)

// No fallback interpolates an argument, path, repository name or patch digest.
func (self repositoryGitPhase) String() string {
	switch self {
	case repositoryGitFilters:
		return "filter_config"
	case repositoryGitHead:
		return "head_identity"
	case repositoryGitBranch:
		return "branch_identity"
	case repositoryGitStatus:
		return "worktree_status"
	case repositoryGitProtectedTree:
		return "protected_tree"
	case repositoryGitPatchBase:
		return "patch_base_tree"
	case repositoryGitHelperTree:
		return "protected_helper_tree"
	case repositoryGitHelperIndex:
		return "protected_helper_index"
	case repositoryGitHelperBytes:
		return "protected_helper_bytes"
	case repositoryGitApplyCheck:
		return "apply_check"
	case repositoryGitApplyIndex:
		return "apply_index"
	case repositoryGitIndexCheck:
		return "index_check"
	case repositoryGitCommit:
		return "commit"
	default:
		return "unknown"
	}
}

// The caller already constructs these fixed verbs; paths are never diagnostics.
func repositoryGitPhaseForArguments(args []string) repositoryGitPhase {
	if len(args) == 0 {
		return repositoryGitUnknown
	}
	switch args[0] {
	case "config":
		return repositoryGitFilters
	case "rev-parse":
		if len(args) == 2 && args[1] == "HEAD" {
			return repositoryGitHead
		}
		if len(args) == 2 && args[1] == "HEAD:connect/sim-latency" {
			return repositoryGitProtectedTree
		}
	case "symbolic-ref":
		return repositoryGitBranch
	case "status":
		return repositoryGitStatus
	case "--literal-pathspecs":
		if len(args) > 1 && args[1] == "ls-files" {
			return repositoryGitHelperIndex
		}
		if len(args) > 4 && args[1] == "ls-tree" {
			if args[4] == "HEAD" {
				return repositoryGitHelperTree
			}
			return repositoryGitPatchBase
		}
	case "hash-object":
		return repositoryGitHelperBytes
	case "apply":
		if len(args) > 1 && args[1] == "--check" {
			return repositoryGitApplyCheck
		}
		if len(args) > 1 && args[1] == "--index" {
			return repositoryGitApplyIndex
		}
	case "diff":
		return repositoryGitIndexCheck
	case "commit":
		return repositoryGitCommit
	}
	return repositoryGitUnknown
}

// A numeric exit is observed only after native wait; -1 explicitly means absent
// or signal termination. Result keeps output/cancel faults distinct from exit 1.
type repositoryGitResult int

const (
	repositoryGitWaitIo repositoryGitResult = iota
	repositoryGitExited
	repositoryGitSignaled
	repositoryGitCanceled
	repositoryGitDeadline
	repositoryGitOutputLimit
	repositoryGitStartFailed
)

// Closed result strings cannot reveal stderr or failed executable path spelling.
func (self repositoryGitResult) String() string {
	switch self {
	case repositoryGitExited:
		return "exit"
	case repositoryGitSignaled:
		return "signal"
	case repositoryGitCanceled:
		return "canceled"
	case repositoryGitDeadline:
		return "deadline"
	case repositoryGitOutputLimit:
		return "output_limit"
	case repositoryGitStartFailed:
		return "start"
	default:
		return "wait_io"
	}
}

// Error is a safe projection, while Unwrap preserves existing typed ownership.
type repositoryGitOperationError struct {
	phase    repositoryGitPhase
	result   repositoryGitResult
	exitCode int
	cause    error
}

// Only finite source vocabulary and the native numeric result are formatted.
func (self *repositoryGitOperationError) Error() string {
	return fmt.Sprintf("bounded trusted source Git operation failed (phase=%s result=%s exit=%d)", self.phase, self.result, self.exitCode)
}

// Neither original exec error strings nor stderr are retained in the chain.
func (self *repositoryGitOperationError) Unwrap() error { return self.cause }

// Fixed boundary messages stay byte-identical while retaining diagnostic and
// cancellation identities previously discarded by the caller's replacement.
type repositorySourceBoundaryError struct {
	message string
	cause   error
}

// No cause is formatted here; the outer application projects only safe metadata.
func (self *repositorySourceBoundaryError) Error() string { return self.message }

// Existing errors.Is/As consumers still see every original retained cause.
func (self *repositorySourceBoundaryError) Unwrap() error { return self.cause }

// Only trusted fixed boundary messages are supplied by the source owner.
func repositorySourceFailure(message string, causes ...error) error {
	return &repositorySourceBoundaryError{message: message, cause: errors.Join(causes...)}
}

// Exit 1 alone is not no-match when native wait hid a writer/cancellation fault.
func repositoryGitFilterAbsent(configured string, err error) bool {
	var exitError *repositoryGitExitError
	var diagnostic *repositoryGitOperationError
	return errors.As(err, &exitError) && exitError.exitCode == 1 &&
		errors.As(err, &diagnostic) && diagnostic.result == repositoryGitExited && configured == ""
}

// Cmd.Run has joined its owned output copy before this owner-local observation.
func repositoryGitFailure(ctx context.Context, args []string, command *exec.Cmd, output *repositoryGitOutput, runErr error) error {
	result, exitCode := repositoryGitWaitIo, -1
	var cause error
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			result = repositoryGitExited
			if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				result = repositoryGitSignaled
			}
			cause = &repositoryGitExitError{exitCode: exitCode}
		}
	}
	if command.Process == nil {
		result = repositoryGitStartFailed
	}
	if output.overflow {
		result = repositoryGitOutputLimit
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result = repositoryGitDeadline
	} else if errors.Is(ctx.Err(), context.Canceled) {
		result = repositoryGitCanceled
	}
	cause = errors.Join(ctx.Err(), cause)
	return &repositoryGitOperationError{phase: repositoryGitPhaseForArguments(args), result: result, exitCode: exitCode, cause: cause}
}
