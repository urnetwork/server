package server

// Process ownership is separate from database authority. These helpers own a
// complete compiled test root, never a captured callback or a store lease.
// Callers must supply an explicit cgroup capability and immutable configuration.

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"time"
)

var (
	ErrTestProcessUnavailable    = errors.New("owned test process capability is unavailable")
	ErrTestProcessGenerationUsed = errors.New("owned test process generation was already consumed")
	ErrTestProcessUnjoined       = errors.New("owned test process tree was not joined")
)

// Limits are explicit caller-provided bounds, including the complete tree.
type TestProcessConfigurationLimits struct {
	MaxFiles int
	MaxDepth int
	MaxBytes int64
}

// Source files must be privately owned/read-only, without aliases or settings
// environment overrides. Run copies their checked bytes into a kernel-sealed
// archive; workers receive that immutable archive, not the source directory.
type TestProcessConfiguration struct {
	Directory *os.File
	SHA256    string
	Limits    TestProcessConfigurationLimits
}

// One invocation retains the caller's original overall context deadline.
// JoinReserve is taken OUT OF that deadline; it never adds execution time.
// Standard streams are file descriptors, avoiding unjoinable writer callbacks.
type TestProcessSpec struct {
	Executable       *os.File
	ExecutableSHA256 string
	ExecutableBytes  int64
	Root             string
	WorkingDirectory string
	Environment      []string
	Configuration    TestProcessConfiguration
	JoinReserve      time.Duration
	Parallel         int
	Stdin            *os.File
	Stdout           *os.File
	Stderr           *os.File
	// Optional private pipe retained by a surviving outer manager. Only the
	// guardian receives its write end; it carries no credential or store grant.
	TerminalStatus *os.File
}

// Joined means the guardian was waited, it reaped every owned descendant, and
// the whole containment is empty. It grants no PG/Redis cleanup authority.
type TestProcessResult struct {
	Started           bool
	Joined            bool
	PID               int
	ExitCode          int
	GenerationClaimed bool
	ReapedProcesses   int
}

// All package variables initialize before env.go's init. Linux admission
// therefore validates the descriptor-routed configuration before settings are
// read; atomic cgroup entry occurs earlier still, in the kernel's clone3.
var testProcessChild = loadTestProcessChild()

// A child owns this private status descriptor for its process lifetime.
// The single successful CAS serializes the only post-initialization message.
type testProcessChildState struct {
	root     string
	deadline time.Time
	token    string
	status   *os.File
	claimed  atomic.Bool
}

// Reports an execution capability only, never service or database authority.
func IsOwnedTestProcess() bool {
	return testProcessChild != nil
}

// Consumes one process generation before its caller's effects. Callers which
// need stores must independently validate an actual owned namespace grant.
func ClaimTestProcessGeneration(root string) error {
	child := testProcessChild
	if child == nil {
		return ErrTestProcessUnavailable
	}
	if root != child.root {
		return errors.New("owned test process root differs")
	}
	if !time.Now().Before(child.deadline) {
		return context.DeadlineExceeded
	}
	if !child.claimed.CompareAndSwap(false, true) {
		return ErrTestProcessGenerationUsed
	}
	if !time.Now().Before(child.deadline) {
		return context.DeadlineExceeded
	}
	return writeTestProcessStatus(child.status, testProcessStatus{
		Kind: "claimed", Token: child.token, Root: child.root, PID: os.Getpid(),
	})
}

// Status is private bounded IPC, not test stdout and not a service lease.
type testProcessStatus struct {
	Kind  string `json:"kind"`
	Token string `json:"token"`
	Root  string `json:"root"`
	PID   int    `json:"pid"`
}
