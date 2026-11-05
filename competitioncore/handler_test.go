// Deterministic dispatch tests replace only native fact/placement/handoff
// boundaries. No process, cgroup, daemon, core pipe or native program is used.
package competitioncore

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A read is a test failure even when a rejected core would otherwise be empty.
type testCore struct{ reads, closes int }

func (self *testCore) Read([]byte) (int, error) {
	self.reads++
	return 0, errors.New("core must not be read")
}
func (self *testCore) Close() error { self.closes++; return nil }

type testPlacement struct {
	events  *[]string
	recheck error
	restore error
}

func (self *testPlacement) Recheck(context.Context) error {
	*self.events = append(*self.events, "placement-recheck")
	return self.recheck
}

func (self *testPlacement) Restore(context.Context) error {
	*self.events = append(*self.events, "restore")
	return self.restore
}

func (self *testPlacement) Close() error {
	*self.events = append(*self.events, "placement-close")
	return nil
}

type testProcess struct {
	events *[]string
	facts  []ProcessFacts
	fault  error
	calls  int
}

func (self *testProcess) Facts(context.Context) (ProcessFacts, error) {
	*self.events = append(*self.events, "process-facts")
	index := self.calls
	self.calls++
	if index >= len(self.facts) {
		index = len(self.facts) - 1
	}
	return self.facts[index], self.fault
}

func (self *testProcess) Close() error {
	*self.events = append(*self.events, "process-close")
	return nil
}

type testHost struct {
	settings                                                 Settings
	policy                                                   Policy
	registry                                                 Snapshot
	events                                                   []string
	placement                                                *testPlacement
	process                                                  *testProcess
	pipeLimit                                                uint64
	pattern                                                  string
	boot                                                     string
	snapshotFault, registryChanged, placeFault, handoffFault error
	handoffs                                                 int
	arguments                                                []string
	stream                                                   io.ReadCloser
}

func (self *testHost) Policy(context.Context) (Policy, error) {
	self.events = append(self.events, "policy")
	return self.policy, nil
}
func (self *testHost) PipeLimit(context.Context) (uint64, error) {
	self.events = append(self.events, "pipe-limit")
	return self.pipeLimit, nil
}
func (self *testHost) Pattern(context.Context) (string, error) {
	self.events = append(self.events, "pattern")
	return self.pattern, nil
}
func (self *testHost) BootId(context.Context) (string, error) {
	self.events = append(self.events, "boot")
	return self.boot, nil
}
func (self *testHost) Snapshot(context.Context) (Snapshot, error) {
	self.events = append(self.events, "registry")
	return self.registry, self.snapshotFault
}
func (self *testHost) Recheck(context.Context, Snapshot) error {
	self.events = append(self.events, "registry-recheck")
	return self.registryChanged
}
func (self *testHost) Place(context.Context, Policy) (placement, error) {
	self.events = append(self.events, "manager-kernel-own-pid-move")
	return self.placement, self.placeFault
}
func (self *testHost) Process(context.Context, Invocation) (process, error) {
	self.events = append(self.events, "pidfd-held-proc")
	return self.process, nil
}
func (self *testHost) Handoff(_ context.Context, _ Policy, invocation Invocation, core io.ReadCloser) error {
	self.events = append(self.events, "handoff")
	self.handoffs++
	self.arguments, self.stream = invocation.ApportArguments(), core
	return self.handoffFault
}

// The source grammar is the actual closed Apport shape, including newer %F.
func testArguments() []string {
	return []string{"-p23", "-s11", "-c18446744073709551615", "-d1", "-P4321", "-u1000", "-g1001", "-F9", "--", "!synthetic!bin!program"}
}

func testRun(t *testing.T) Run {
	t.Helper()
	start := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	run := Run{RunId: strings.Repeat("a", 64), JobId: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Attempt: 3, BuildKey: strings.Repeat("b", 32), PlanSha256: strings.Repeat("c", 64), StartedAt: start, DeadlineAt: start.Add(3 * time.Hour)}
	var err error
	run.Cgroups, err = RunCgroups(run.JobId, run.Attempt, run.BuildKey)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func testRegistry(t *testing.T) Registry {
	t.Helper()
	return Registry{Schema: Schema, Generation: 4, BootId: "11111111-2222-3333-4444-555555555555", HandlerCgroup: HandlerCgroup, EngineCgroups: EngineCgroups(), Runs: []Run{testRun(t)}}
}

func newTestHandler(t *testing.T, cgroup string) (*Handler, *testHost, *testCore) {
	t.Helper()
	settings := DefaultSettings()
	registry := testRegistry(t)
	host := &testHost{settings: settings, registry: Snapshot{Registry: registry, digest: strings.Repeat("d", 64)}, pipeLimit: ExpectedPipeLimit,
		pattern: DispatcherPattern, boot: registry.BootId,
		policy: Policy{Schema: Schema, ExpectedPipeLimit: ExpectedPipeLimit, ManagementCpuSet: "20,22", Apport: Pin{Path: ApportPath, Sha256: strings.Repeat("e", 64)},
			HandlerUnit: Pin{Path: managerUnitPath, Sha256: strings.Repeat("d", 64)},
			Interpreter: Pin{Path: "/synthetic/interpreter", Sha256: strings.Repeat("f", 64)}}}
	host.placement = &testPlacement{events: &host.events}
	host.process = &testProcess{events: &host.events, facts: []ProcessFacts{{HostPid: 4321, NamespacePid: 23, Birth: 777, Uid: 1000, Gid: 1001, Cgroup: cgroup}}}
	return &Handler{settings: settings, host: host}, host, &testCore{}
}

// Candidate descendants close stdin and both identity leases, without reading
// any core byte, restoring unrelated resources or invoking the handoff backend.
func TestCandidateDropOwnsPipeWithoutReadOrExec(t *testing.T) {
	registry := testRegistry(t)
	handler, host, core := newTestHandler(t, registry.Runs[0].Cgroups[1]+"/docker-synthetic.scope")
	if decision := handler.Handle(t.Context(), testArguments(), core); decision != CandidateDropped || core.reads != 0 || core.closes != 1 || host.handoffs != 0 {
		t.Fatal("candidate core escaped its no-read/no-exec drop", decision, core, host.events)
	}
	want := []string{"policy", "pipe-limit", "pattern", "manager-kernel-own-pid-move", "registry", "boot", "pidfd-held-proc", "process-facts", "process-facts", "registry-recheck", "placement-recheck", "process-close", "placement-close"}
	if !reflect.DeepEqual(host.events, want) {
		t.Fatal("candidate classification bypassed the exact own-PID placement or custody ordering", host.events)
	}
}

// Missing, changed or ambiguous facts all close without reading core or exec.
func TestUnknownDropHasNoReadOrExecAcrossEveryProofFailure(t *testing.T) {
	for _, fault := range []string{"limit-zero", "limit-change", "pattern", "boot", "registry-missing", "registry-partial", "registry-change", "place", "placement-change", "process-gone", "pid-reuse", "migration"} {
		handler, host, core := newTestHandler(t, "/system.slice/synthetic-ordinary.service")
		switch fault {
		case "limit-zero":
			host.pipeLimit = 0
		case "limit-change":
			host.pipeLimit++
		case "pattern":
			host.pattern = OriginalPattern
		case "boot":
			host.boot = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		case "registry-missing":
			host.snapshotFault = errors.New("missing registry")
		case "registry-partial":
			host.registry.Registry.Runs[0].Cgroups = host.registry.Registry.Runs[0].Cgroups[:30]
		case "registry-change":
			host.registryChanged = errors.New("new root generation")
		case "place":
			host.placeFault = errors.New("manager proof unavailable")
		case "placement-change":
			host.placement.recheck = errors.New("cgroup proof changed")
		case "process-gone":
			host.process.fault = errors.New("proc disappeared")
		case "pid-reuse", "migration":
			second := host.process.facts[0]
			if fault == "pid-reuse" {
				second.Birth++
			} else {
				second.Cgroup = host.registry.Registry.Runs[0].Cgroups[0]
			}
			host.process.facts = append(host.process.facts, second)
		}
		if decision := handler.Handle(t.Context(), testArguments(), core); decision != UnknownDropped || core.reads != 0 || core.closes != 1 || host.handoffs != 0 {
			t.Fatal("uncertain core became ordinary or touched stdin/exec", fault, decision, core, host.events)
		}
	}
}

// Positive ordinary testing injects exec only; it does not qualify native
// Apport script/interpreter lifetime, resource restoration or stdin semantics.
func TestOrdinarySeamRetainsExactClosedArgumentsAndOriginalStream(t *testing.T) {
	handler, host, core := newTestHandler(t, "/system.slice/synthetic-ordinary.service")
	arguments := testArguments()
	arguments[7] = "-F"
	if decision := handler.Handle(t.Context(), arguments, core); decision != OrdinaryHanded || host.handoffs != 1 || core.reads != 0 || core.closes != 1 || host.stream != core || !reflect.DeepEqual(host.arguments, arguments) {
		t.Fatal("positive ordinary seam changed argv/stdin or omitted restoration", decision, host.events)
	}
	if !reflect.DeepEqual(host.events[len(host.events)-7:], []string{"placement-recheck", "restore", "process-facts", "registry-recheck", "handoff", "process-close", "placement-close"}) {
		t.Fatal("ordinary handoff did not restore before its final generation checks", host.events)
	}
}

// Missing restore/exec implementations never become a native pass flag.
func TestOrdinaryRestoreOrHandoffFailureStillClosesUnreadCore(t *testing.T) {
	for _, restore := range []bool{true, false} {
		handler, host, core := newTestHandler(t, "/")
		if restore {
			host.placement.restore = ErrOrdinaryHandoffUnimplemented
		} else {
			host.handoffFault = ErrOrdinaryHandoffUnimplemented
		}
		if decision := handler.Handle(t.Context(), testArguments(), core); decision != OrdinaryDropped || core.reads != 0 || core.closes != 1 || restore && host.handoffs != 0 {
			t.Fatal("unimplemented ordinary custody read or forwarded a core", decision, core, host.events)
		}
	}
}

// Every scoped private engine role, and the dispatcher itself, is also dropped.
func TestPrivateEngineAndHandlerCgroupsCannotBecomeOrdinary(t *testing.T) {
	for _, cgroup := range append(EngineCgroups(), HandlerCgroup) {
		handler, host, core := newTestHandler(t, cgroup+"/synthetic-child")
		if decision := handler.Handle(t.Context(), testArguments(), core); decision != CandidateDropped || host.handoffs != 0 || core.reads != 0 {
			t.Fatal("private role escaped its exact component scope", cgroup, decision)
		}
	}
}

// A source plan cannot be activated merely by supplying synthetic metadata.
func TestProductionBackendsHaveExplicitImplementationFailures(t *testing.T) {
	settings := DefaultSettings()
	if nativeManagerProof(t.Context(), Policy{}, settings) == nil {
		t.Fatal("production manager backend manufactured proof without its fixed root policy")
	}
	if !errors.Is((nativeHost{}).Handoff(t.Context(), Policy{}, Invocation{}, &testCore{}), ErrOrdinaryHandoffUnimplemented) {
		t.Fatal("production handoff backend manufactured script custody")
	}
	if _, err := (nativeJoinVerifier{}).Verify(t.Context(), Run{}, 1); !errors.Is(err, ErrCleanupJoinUnimplemented) {
		t.Fatal("production cleanup backend manufactured joined helper proof")
	}
}

// Declared script/interpreter pins, including changed pins, are not an exec
// capability. The native draft must still refuse every metadata-only handoff.
func TestNativeOrdinaryHandoffCannotBeEnabledByPinSubstitution(t *testing.T) {
	_, host, core := newTestHandler(t, "/system.slice/synthetic-ordinary.service")
	invocation, err := ParseInvocation(testArguments(), DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Policy){
		func(*Policy) {},
		func(policy *Policy) { policy.Apport.Sha256 = strings.Repeat("1", 64) },
		func(policy *Policy) { policy.Interpreter.Sha256 = strings.Repeat("2", 64) },
		func(policy *Policy) { policy.Interpreter.Path = "/synthetic/other-interpreter" },
	} {
		policy := host.policy
		change(&policy)
		if err := (nativeHost{}).Handoff(t.Context(), policy, invocation, core); !errors.Is(err, ErrOrdinaryHandoffUnimplemented) {
			t.Fatal("metadata pins became native script/interpreter exec authority", err)
		}
	}
	if core.reads != 0 {
		t.Fatal("unimplemented native handoff read core bytes")
	}
}

func TestManagementCpuPolicyRejectsMalformedFirstRangeEndpoint(t *testing.T) {
	for _, value := range []string{"x-2", "-1", "+0-2", "00-2", "1-0", "2,2", "0-999999999"} {
		if validCpuSet(value, DefaultSettings().MaximumCpuIndex) {
			t.Fatal("malformed CPU policy passed through a replaced parse error", value)
		}
	}
}

// Source parser constraints include the actual 127-byte kernel pattern limit.
func TestClosedInvocationRejectsOverridesAndPreservesKernelGrammar(t *testing.T) {
	settings := DefaultSettings()
	if len(DispatcherPattern) > 127 || !strings.HasPrefix(DispatcherPattern, "|"+DispatcherPath+" ") {
		t.Fatal("prospective dispatcher pattern exceeds kernel grammar")
	}
	for _, mutate := range []func([]string) []string{
		func(args []string) []string { return append(args, "--config=/synthetic/other") },
		func(args []string) []string { args[0] = "--config=/synthetic/other"; return args },
		func(args []string) []string { args[4] = "-P0"; return args },
		func(args []string) []string { args[4] = "-P04321"; return args },
		func(args []string) []string { args[7] = "-F1048577"; return args },
		func(args []string) []string { args[8] = "--exec"; return args },
		func(args []string) []string { args[9] = strings.Repeat("x", settings.ArgumentBytes+1); return args },
	} {
		if _, err := ParseInvocation(mutate(testArguments()), settings); err == nil {
			t.Fatal("core invocation admitted an override or malformed kernel identity")
		}
	}
	invocation, err := ParseInvocation(testArguments(), settings)
	if err != nil || !reflect.DeepEqual(invocation.ApportArguments(), testArguments()) {
		t.Fatal("closed ordinary argv was not retained", err)
	}
}
