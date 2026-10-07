// Pins the signaling-versus-exit boundary with exact kernel-operation results
// and virtual time; no external process or scheduler race drives these tests.
package controller

import (
	"errors"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// A successful kill can precede several EPERM observations while Darwin has
// stopped granting signal references but still reports a non-zombie process.
func TestTerminateContainedProcessGroupWaitsThroughExitingMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const processGroupId = 72001
		signalCount, inspectionCount := 0, 0
		control := containedProcessGroupControl{
			signal: func(pid int, signal syscall.Signal) error {
				if pid != -processGroupId || signal != syscall.SIGKILL {
					t.Fatalf("signal target=(%d, %v), want owned group SIGKILL", pid, signal)
				}
				signalCount += 1
				if signalCount == 1 {
					return nil
				}
				return syscall.EPERM
			},
			running: func(groupId int) (bool, error) {
				inspectionCount += 1
				if groupId != processGroupId || inspectionCount != signalCount || 4 < inspectionCount {
					t.Fatalf("inspection group=%d signals=%d inspections=%d", groupId, signalCount, inspectionCount)
				}
				return inspectionCount < 4, nil
			},
		}
		started := time.Now()
		if err := control.terminate(processGroupId); err != nil {
			t.Fatalf("exiting member was mistaken for a permanent signal denial: %v", err)
		}
		if signalCount != 4 || inspectionCount != 4 || time.Since(started) != 30*time.Millisecond {
			t.Fatalf("termination signals=%d inspections=%d elapsed=%s", signalCount, inspectionCount, time.Since(started))
		}
	})
}

// The command's earlier TERM may already have begun exit before cleanup's
// first KILL; an earlier successful KILL is not required for an exit join.
func TestTerminateContainedProcessGroupWaitsAfterInitialPermissionRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inspectionCount := 0
		control := containedProcessGroupControl{
			signal: func(int, syscall.Signal) error { return syscall.EPERM },
			running: func(int) (bool, error) {
				inspectionCount += 1
				return inspectionCount == 1, nil
			},
		}
		started := time.Now()
		if err := control.terminate(72002); err != nil {
			t.Fatalf("already-exiting group was rejected: %v", err)
		}
		if inspectionCount != 2 || time.Since(started) != 10*time.Millisecond {
			t.Fatalf("exit join inspections=%d elapsed=%s", inspectionCount, time.Since(started))
		}
	})
}

// Permission refusal is never permission to leave a live member behind.
// The original deadline must end the join and retain the underlying denial.
func TestTerminateContainedProcessGroupBoundsPersistentPermissionRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		control := containedProcessGroupControl{
			signal:  func(int, syscall.Signal) error { return syscall.EPERM },
			running: func(int) (bool, error) { return true, nil },
		}
		started := time.Now()
		err := control.terminate(72003)
		if !errors.Is(err, syscall.EPERM) || !strings.Contains(err.Error(), "evaluator process group did not terminate") {
			t.Fatalf("live denied group did not preserve containment and signal failures: %v", err)
		}
		if time.Since(started) != processTermGrace {
			t.Fatalf("permission-refused join elapsed=%s, want %s", time.Since(started), processTermGrace)
		}
	})
}

// Unknown process state fails closed immediately, including when it follows
// an ambiguous permission refusal; neither underlying failure may disappear.
func TestTerminateContainedProcessGroupPreservesInspectionFailure(t *testing.T) {
	for _, signalErr := range []error{nil, syscall.ESRCH, syscall.EPERM} {
		synctest.Test(t, func(t *testing.T) {
			inspectionErr := errors.New("synthetic group inspection failure")
			control := containedProcessGroupControl{
				signal:  func(int, syscall.Signal) error { return signalErr },
				running: func(int) (bool, error) { return false, inspectionErr },
			}
			started := time.Now()
			err := control.terminate(72004)
			if !errors.Is(err, inspectionErr) || errors.Is(signalErr, syscall.EPERM) && !errors.Is(err, syscall.EPERM) {
				t.Fatalf("signal=%v inspection failure was lost: %v", signalErr, err)
			}
			if time.Since(started) != 0 {
				t.Fatalf("unknown group state was retried for %s", time.Since(started))
			}
		})
	}
}

// Only the known permission/existence ambiguity admits independent inspection;
// other signaling failures remain terminal rather than being hidden by it.
func TestTerminateContainedProcessGroupPreservesOtherSignalFailure(t *testing.T) {
	control := containedProcessGroupControl{
		signal: func(int, syscall.Signal) error { return syscall.EINVAL },
		running: func(int) (bool, error) {
			t.Fatal("unexpected inspection after an unrecognized signal failure")
			return false, nil
		},
	}
	if err := control.terminate(72005); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("unexpected signal failure was lost: %v", err)
	}
}

// Absence or a zombie-only group is already a completed termination, whatever
// the kernel's signal result. This path must not spend the grace deadline.
func TestTerminateContainedProcessGroupAcceptsProvenExit(t *testing.T) {
	for _, signalErr := range []error{nil, syscall.ESRCH, syscall.EPERM} {
		synctest.Test(t, func(t *testing.T) {
			control := containedProcessGroupControl{
				signal:  func(int, syscall.Signal) error { return signalErr },
				running: func(int) (bool, error) { return false, nil },
			}
			started := time.Now()
			if err := control.terminate(72006); err != nil {
				t.Fatalf("signal=%v rejected proven exit: %v", signalErr, err)
			}
			if time.Since(started) != 0 {
				t.Fatalf("proven exit waited for %s", time.Since(started))
			}
		})
	}
}

// A successful signal remains only a request: live members must still be
// joined, with the same finite deadline as the permission-refused path.
func TestTerminateContainedProcessGroupBoundsSuccessfulSignalWithoutExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		control := containedProcessGroupControl{
			signal:  func(int, syscall.Signal) error { return nil },
			running: func(int) (bool, error) { return true, nil },
		}
		started := time.Now()
		if err := control.terminate(72007); err == nil || err.Error() != "evaluator process group did not terminate" {
			t.Fatalf("live group did not fail containment: %v", err)
		}
		if time.Since(started) != processTermGrace {
			t.Fatalf("successful-signal join elapsed=%s, want %s", time.Since(started), processTermGrace)
		}
	})
}
