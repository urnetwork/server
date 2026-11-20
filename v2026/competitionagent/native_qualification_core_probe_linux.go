// A finite, non-root child probe records real Linux core-limit syscalls. It
// neither accepts a runtime class nor treats its own exit as containment proof.
package competitionagent

import (
	"context"
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// Linux ABI syscall numbers are protocol facts, not resource-policy tunables.
const nativeQualificationAmd64Setrlimit = 160
const nativeQualification386Setrlimit = 75

// Unsupported native prerequisites must remain distinguishable from failures;
// neither result is a positive qualification.
var errNativeQualificationUnsupported = errors.New("native qualification prerequisite unsupported")

// Native prlimit64 always uses two uint64 values, including compat processes.
type nativeQualificationCoreLimits struct {
	Current uint64 `json:"current"`
	Maximum uint64 `json:"maximum"`
}

// Only the fixed operation names below exist; no syscall/argument comes from a
// candidate, JSON command string, or caller-provided executable.
type nativeQualificationCoreCall struct {
	Operation string `json:"operation"`
	Errno     int    `json:"errno"`
}

// An observation is useful only with independent actual child/role/image/
// profile/cgroup readbacks. There is deliberately no accepted/passed Boolean.
type nativeQualificationCoreFacts struct {
	Schema       int                           `json:"schema"`
	Kind         string                        `json:"kind"`
	Architecture string                        `json:"architecture"`
	Uid          int                           `json:"uid"`
	Gid          int                           `json:"gid"`
	Pid          int                           `json:"pid"`
	Before       nativeQualificationCoreLimits `json:"before"`
	After        nativeQualificationCoreLimits `json:"after"`
	Calls        []nativeQualificationCoreCall `json:"calls"`
}

// Tests inject only the kernel interaction seam. The eventual native CLI uses
// the concrete syscall implementation, never an input-selected callback.
type nativeQualificationCoreSystem interface {
	Identity() (string, int, int, int)
	Query() (nativeQualificationCoreLimits, syscall.Errno)
	SetRaw(nativeQualificationCoreLimits) syscall.Errno
	SetPrlimit(nativeQualificationCoreLimits) syscall.Errno
}

// This receiver has no mutable shared state or process-global budget.
type nativeQualificationCoreNativeSystem struct{}

// The actual execution identity is observed, not asserted by the plan.
func (nativeQualificationCoreNativeSystem) Identity() (string, int, int, int) {
	return runtime.GOARCH, os.Geteuid(), os.Getegid(), os.Getpid()
}

// Query explicitly uses prlimit64 with new_limit=NULL.
func (nativeQualificationCoreNativeSystem) Query() (nativeQualificationCoreLimits, syscall.Errno) {
	var limits nativeQualificationCoreLimits
	_, _, errno := syscall.Syscall6(syscall.SYS_PRLIMIT64, 0, syscall.RLIMIT_CORE, 0, uintptr(unsafe.Pointer(&limits)), 0, 0)
	runtime.KeepAlive(&limits)
	return limits, errno
}

// The legacy setrlimit ABI differs on compat x86; only exact representable
// core-one limits are ever passed. Other architectures refuse explicitly.
func (nativeQualificationCoreNativeSystem) SetRaw(limits nativeQualificationCoreLimits) syscall.Errno {
	switch runtime.GOARCH {
	case "amd64":
		_, _, errno := syscall.Syscall(nativeQualificationAmd64Setrlimit, syscall.RLIMIT_CORE, uintptr(unsafe.Pointer(&limits)), 0)
		runtime.KeepAlive(&limits)
		return errno
	case "386":
		if limits.Current > uint64(^uint32(0)) || limits.Maximum > uint64(^uint32(0)) {
			return syscall.EINVAL
		}
		compat := [2]uint32{uint32(limits.Current), uint32(limits.Maximum)}
		_, _, errno := syscall.Syscall(nativeQualification386Setrlimit, syscall.RLIMIT_CORE, uintptr(unsafe.Pointer(&compat)), 0)
		runtime.KeepAlive(&compat)
		return errno
	default:
		return syscall.ENOSYS
	}
}

// Supplying a non-NULL new_limit is deliberately distinct from query-only
// prlimit64 admission, even when the requested limits equal the current ones.
func (nativeQualificationCoreNativeSystem) SetPrlimit(limits nativeQualificationCoreLimits) syscall.Errno {
	_, _, errno := syscall.Syscall6(syscall.SYS_PRLIMIT64, 0, syscall.RLIMIT_CORE, uintptr(unsafe.Pointer(&limits)), 0, 0, 0)
	runtime.KeepAlive(&limits)
	return errno
}

// Same-value and lowering calls would be permitted by ordinary kernel limit
// semantics: their EPERM witnesses are not merely failure to raise a hard cap.
// A missing profile can lower only this disposable child's soft limit, never
// its parent or hard limit; the caller must independently reject that result.
func captureNativeQualificationCoreFacts(ctx context.Context, system nativeQualificationCoreSystem) (facts nativeQualificationCoreFacts, resultErr error) {
	facts.Schema = Schema
	facts.Kind = "runtime-native-core-syscall-observation/v1"
	if system == nil {
		return facts, errNativeQualificationUnsupported
	}
	facts.Architecture, facts.Uid, facts.Gid, facts.Pid = system.Identity()
	if (facts.Architecture != "amd64" && facts.Architecture != "386") || facts.Uid <= 0 || facts.Gid < 0 || facts.Pid <= 0 {
		return facts, errNativeQualificationUnsupported
	}
	if err := ctx.Err(); err != nil {
		return facts, err
	}
	before, errno := system.Query()
	facts.Before = before
	facts.Calls = append(facts.Calls, nativeQualificationCoreCall{Operation: "prlimit-query-before", Errno: int(errno)})
	if errno != 0 {
		return facts, errors.New("native core query failed")
	}
	if before != (nativeQualificationCoreLimits{Current: 1, Maximum: 1}) {
		return facts, errNativeQualificationUnsupported
	}
	for _, operation := range []string{"setrlimit-same", "setrlimit-lower", "prlimit-set-same"} {
		if err := ctx.Err(); err != nil {
			return facts, err
		}
		limits := before
		if operation == "setrlimit-lower" {
			limits.Current = 0
		}
		var result syscall.Errno
		if operation == "prlimit-set-same" {
			result = system.SetPrlimit(limits)
		} else {
			result = system.SetRaw(limits)
		}
		facts.Calls = append(facts.Calls, nativeQualificationCoreCall{Operation: operation, Errno: int(result)})
	}
	if err := ctx.Err(); err != nil {
		return facts, err
	}
	facts.After, errno = system.Query()
	facts.Calls = append(facts.Calls, nativeQualificationCoreCall{Operation: "prlimit-query-after", Errno: int(errno)})
	if errno != 0 {
		return facts, errors.New("native final core query failed")
	}
	return facts, ctx.Err()
}

// A nil probe error alone is never a pass. The collector additionally binds
// these facts to its actual immutable probe binary and owned native boundary.
func validNativeQualificationCoreFacts(facts nativeQualificationCoreFacts) bool {
	if facts.Schema != Schema || facts.Kind != "runtime-native-core-syscall-observation/v1" || (facts.Architecture != "amd64" && facts.Architecture != "386") || facts.Uid <= 0 || facts.Gid < 0 || facts.Pid <= 0 || facts.Before != (nativeQualificationCoreLimits{Current: 1, Maximum: 1}) || facts.After != facts.Before || len(facts.Calls) != 5 {
		return false
	}
	for index, operation := range []string{"prlimit-query-before", "setrlimit-same", "setrlimit-lower", "prlimit-set-same", "prlimit-query-after"} {
		expected := int(syscall.EPERM)
		if index == 0 || index == 4 {
			expected = 0
		}
		if facts.Calls[index].Operation != operation || facts.Calls[index].Errno != expected {
			return false
		}
	}
	return true
}
