//go:build !linux

package server

// Owned execution requires Linux atomic cgroup-v2 entry and has no PID-only or
// silently uncontained fallback. Ordinary non-owned test roots are unchanged.

import (
	"context"
	"fmt"
	"os"
)

// Unsupported platforms cannot construct an execution capability.
type TestProcessCgroup struct{}

// Non-owned resolver behavior is unchanged on unsupported platforms.
func ownedTestProcessResourcePaths(MountType, string) ([]string, bool, error) {
	return nil, false, nil
}

// Refuses rather than adopting an uncontained process or existing host scope.
func CreateTestProcessCgroup(*os.File) (*TestProcessCgroup, error) {
	return nil, ErrTestProcessUnavailable
}

// Configuration validation is part of the unavailable Linux capability.
func SnapshotTestProcessConfiguration(*os.File, TestProcessConfigurationLimits) (string, error) {
	return "", ErrTestProcessUnavailable
}

// There is no portable fallback to leader PID ownership.
func (self *TestProcessCgroup) Run(context.Context, TestProcessSpec) (TestProcessResult, error) {
	return TestProcessResult{}, ErrTestProcessUnavailable
}

// No unsupported platform object can own a removable process tree.
func (self *TestProcessCgroup) Close(context.Context) error {
	return ErrTestProcessUnavailable
}

// An explicitly requested unsupported launch fails before environment init.
func loadTestProcessChild() *testProcessChildState {
	if os.Getenv("URNETWORK_OWNED_TEST_PROCESS") != "" {
		fmt.Fprintln(os.Stderr, ErrTestProcessUnavailable)
		os.Exit(125)
	}
	return nil
}

// This is unreachable without an admitted Linux child.
func writeTestProcessStatus(*os.File, testProcessStatus) error {
	return ErrTestProcessUnavailable
}
