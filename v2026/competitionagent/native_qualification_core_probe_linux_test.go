// All syscall results and identities are synthetic; no test changes any real
// process limit or executes a native child/container.
package competitionagent

import (
	"context"
	"errors"
	"syscall"
	"testing"
)

// The fake retains call order/arguments and can cancel at an explicit boundary.
type nativeQualificationCoreTestSystem struct {
	architecture string
	uid          int
	limits       nativeQualificationCoreLimits
	queryErrno   syscall.Errno
	queryErrnos  []syscall.Errno
	queryCount   int
	setErrno     syscall.Errno
	calls        []nativeQualificationCoreCall
	cancel       context.CancelFunc
}

// No host identity is read by the fake.
func (self *nativeQualificationCoreTestSystem) Identity() (string, int, int, int) {
	return self.architecture, self.uid, 12346, 23456
}

// The fixture's current limits mirror successful unprotected mutations.
func (self *nativeQualificationCoreTestSystem) Query() (nativeQualificationCoreLimits, syscall.Errno) {
	errno := self.queryErrno
	if self.queryCount < len(self.queryErrnos) {
		errno = self.queryErrnos[self.queryCount]
	}
	self.queryCount++
	self.calls = append(self.calls, nativeQualificationCoreCall{Operation: "query", Errno: int(errno)})
	return self.limits, errno
}

// Only the disposable child's soft limit would change on a permissive profile.
func (self *nativeQualificationCoreTestSystem) SetRaw(value nativeQualificationCoreLimits) syscall.Errno {
	self.calls = append(self.calls, nativeQualificationCoreCall{Operation: "raw", Errno: int(self.setErrno)})
	if self.setErrno == 0 {
		self.limits = value
	}
	if self.cancel != nil {
		self.cancel()
	}
	return self.setErrno
}

// Native prlimit set is distinct from query admission.
func (self *nativeQualificationCoreTestSystem) SetPrlimit(value nativeQualificationCoreLimits) syscall.Errno {
	self.calls = append(self.calls, nativeQualificationCoreCall{Operation: "prlimit-set", Errno: int(self.setErrno)})
	if self.setErrno == 0 {
		self.limits = value
	}
	return self.setErrno
}

// Native and compat facts require exact denial/order plus preserved core-one.
func TestNativeQualificationCoreProbeRequiresActualDenialsAndQuery(t *testing.T) {
	for _, architecture := range []string{"amd64", "386"} {
		system := &nativeQualificationCoreTestSystem{architecture: architecture, uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}, setErrno: syscall.EPERM}
		facts, err := captureNativeQualificationCoreFacts(context.Background(), system)
		if err != nil || !validNativeQualificationCoreFacts(facts) || len(system.calls) != 5 {
			t.Fatal("complete synthetic syscall facts refused", facts, err)
		}
		for index := range facts.Calls {
			changed := facts
			changed.Calls = append([]nativeQualificationCoreCall(nil), facts.Calls...)
			changed.Calls[index].Errno = int(syscall.EACCES)
			if validNativeQualificationCoreFacts(changed) {
				t.Fatal("wrong actual errno qualified", index)
			}
		}
	}
}

// A zero exit/complete observation without the intended seccomp denials is not
// containment proof; neither are root/unsupported ABI/wrong initial limits.
func TestNativeQualificationCoreProbeRefusesPermissiveOrUnsupportedFacts(t *testing.T) {
	permissive := &nativeQualificationCoreTestSystem{architecture: "amd64", uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}}
	facts, err := captureNativeQualificationCoreFacts(context.Background(), permissive)
	if err != nil || validNativeQualificationCoreFacts(facts) {
		t.Fatal("permissive syscalls became a native pass", facts, err)
	}
	for _, system := range []*nativeQualificationCoreTestSystem{
		{architecture: "amd64", uid: 0, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}},
		{architecture: "unsupported-synthetic", uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}},
		{architecture: "amd64", uid: 12345, limits: nativeQualificationCoreLimits{Current: 0, Maximum: 1}},
	} {
		facts, err := captureNativeQualificationCoreFacts(context.Background(), system)
		if !errors.Is(err, errNativeQualificationUnsupported) || validNativeQualificationCoreFacts(facts) {
			t.Fatal("unsupported prerequisite became qualification", facts, err)
		}
	}
}

// Owner cancellation is checked before further mutation, even if a prior
// syscall completed normally. No fresh child/lifetime is fabricated.
func TestNativeQualificationCoreProbeStopsAtOriginalOwnerCancellation(t *testing.T) {
	before, cancelBefore := context.WithCancel(context.Background())
	cancelBefore()
	untouched := &nativeQualificationCoreTestSystem{architecture: "amd64", uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}, setErrno: syscall.EPERM}
	if facts, err := captureNativeQualificationCoreFacts(before, untouched); !errors.Is(err, context.Canceled) || len(untouched.calls) != 0 || validNativeQualificationCoreFacts(facts) {
		t.Fatal("pre-canceled owner dispatched a syscall", facts, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	system := &nativeQualificationCoreTestSystem{architecture: "amd64", uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}, setErrno: syscall.EPERM, cancel: cancel}
	facts, err := captureNativeQualificationCoreFacts(ctx, system)
	if !errors.Is(err, context.Canceled) || len(system.calls) != 2 || validNativeQualificationCoreFacts(facts) {
		t.Fatal("canceled owner dispatched another native call", facts, err, len(system.calls))
	}
}

// Query errors remain real failures at both boundaries; a complete-looking
// document with changed ordering/count/identity/limits cannot qualify either.
func TestNativeQualificationCoreProbeRejectsFailedQueriesAndMalformedFacts(t *testing.T) {
	for _, errnos := range [][]syscall.Errno{{syscall.EIO, 0}, {0, syscall.EIO}} {
		system := &nativeQualificationCoreTestSystem{architecture: "amd64", uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}, setErrno: syscall.EPERM, queryErrnos: errnos}
		facts, err := captureNativeQualificationCoreFacts(context.Background(), system)
		if err == nil || validNativeQualificationCoreFacts(facts) {
			t.Fatal("failed query became native evidence", facts, err)
		}
		if errnos[0] != 0 && len(system.calls) != 1 || errnos[1] != 0 && len(system.calls) != 5 {
			t.Fatal("query failure boundary was hidden", system.calls)
		}
	}
	system := &nativeQualificationCoreTestSystem{architecture: "amd64", uid: 12345, limits: nativeQualificationCoreLimits{Current: 1, Maximum: 1}, setErrno: syscall.EPERM}
	facts, err := captureNativeQualificationCoreFacts(context.Background(), system)
	if err != nil || !validNativeQualificationCoreFacts(facts) {
		t.Fatal("synthetic valid prerequisite failed", err)
	}
	for _, mutate := range []func(*nativeQualificationCoreFacts){
		func(value *nativeQualificationCoreFacts) { value.Schema++ },
		func(value *nativeQualificationCoreFacts) { value.Kind = "unsupported-synthetic-kind" },
		func(value *nativeQualificationCoreFacts) { value.Architecture = "unsupported-synthetic-abi" },
		func(value *nativeQualificationCoreFacts) { value.Uid = 0 },
		func(value *nativeQualificationCoreFacts) { value.Gid = -1 },
		func(value *nativeQualificationCoreFacts) { value.Pid = 0 },
		func(value *nativeQualificationCoreFacts) { value.Before.Current = 0 },
		func(value *nativeQualificationCoreFacts) { value.Before.Maximum = 0 },
		func(value *nativeQualificationCoreFacts) { value.After.Current = 0 },
		func(value *nativeQualificationCoreFacts) { value.After.Maximum = 0 },
		func(value *nativeQualificationCoreFacts) { value.Calls = value.Calls[:4] },
		func(value *nativeQualificationCoreFacts) { value.Calls = append(value.Calls, value.Calls[4]) },
		func(value *nativeQualificationCoreFacts) {
			value.Calls[0], value.Calls[1] = value.Calls[1], value.Calls[0]
		},
		func(value *nativeQualificationCoreFacts) { value.Calls[1].Operation = "unknown-synthetic-call" },
	} {
		changed := facts
		changed.Calls = append([]nativeQualificationCoreCall(nil), facts.Calls...)
		mutate(&changed)
		if validNativeQualificationCoreFacts(changed) {
			t.Fatal("malformed native syscall facts qualified", changed)
		}
	}
}
