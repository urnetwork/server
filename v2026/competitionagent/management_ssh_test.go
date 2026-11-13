//go:build linux

// Managed SSH tests use private files, physical pipes and a fake PID1 owner.
// They never invoke the native manager, change cgroups, or access a daemon.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Private metadata mirrors a protected role without opening any host config.
func managementSshTestConfig(t *testing.T) (Config, ManagementResourcePolicy, ManagementSshSettings) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	settings := DefaultManagementSshSettings()
	policy := managementTestPolicy(DefaultManagementBoundarySettings())
	config := Config{Schema: Schema, BaseDirectory: root, StateDirectory: state, configPath: filepath.Join(root, "agent.json"), configSha256: strings.Repeat("a", 64),
		Agent: Pin{Path: filepath.Join(root, "agent"), Sha256: strings.Repeat("b", 64)}, SystemdRun: Pin{Path: "/usr/bin/systemd-run", Sha256: strings.Repeat("c", 64)}, Limits: DefaultLimits()}
	return config, policy, settings
}

// The plan retains its session's original timestamps, not a submitted job clock.
func managementSshTestPlan(config Config, policy ManagementResourcePolicy, settings ManagementSshSettings) managementSshPlan {
	id, now := strings.Repeat("d", 64), settings.Agent.Now().UTC()
	return managementSshPlan{Schema: Schema, Id: id, UnitName: managementSshUnitName(id), ConfigSha256: config.configSha256, Agent: config.Agent,
		CreatedAt: now, ExpiresAt: now.Add(settings.MaximumRuntime), RuntimeMaximum: settings.MaximumRuntime, MemoryBytes: settings.MemoryBytes,
		TasksMaximum: settings.TasksMaximum, ManagementCpuSet: policy.ManagementCpuSet}
}

// All six originals remain caller-owned. The wrapper sees only read/write ends.
func managementSshTestPipes(t *testing.T) (*os.File, *os.File, *os.File, *os.File, *os.File, *os.File) {
	t.Helper()
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	diagRead, diagWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, file := range []*os.File{inRead, inWrite, outRead, outWrite, diagRead, diagWrite} {
			_ = file.Close()
		}
	})
	return inRead, inWrite, outRead, outWrite, diagRead, diagWrite
}

// Tests control process completion explicitly, without wall-clock races.
type managementSshTestProcess struct {
	wait func() error
	kill func() error
}

func (self *managementSshTestProcess) Wait() error { return self.wait() }
func (self *managementSshTestProcess) Kill() error { return self.kill() }

// Native seams are replaced per owner, never through a global logger or proof.
func managementSshTestHost(config Config, policy ManagementResourcePolicy, settings ManagementSshSettings) *managementSshHost {
	host := newManagementSshHost(settings)
	host.protected = false
	host.load = func(path string) (*Config, error) {
		if path != config.configPath {
			return nil, errors.New("synthetic different role config")
		}
		copy := config
		return &copy, nil
	}
	host.admit = func(context.Context, *Config) (*ManagementResourcePolicy, Pin, error) {
		copy := policy
		return &copy, Pin{Path: "/usr/bin/systemctl", Sha256: strings.Repeat("e", 64)}, nil
	}
	host.newId = func() (string, error) { return strings.Repeat("d", 64), nil }
	host.resetFailed = func(context.Context, Pin, string) error {
		return errors.New("synthetic manager reset is not installed for this owner")
	}
	return host
}

// The child keeps its complete healthy RPC lifetime and is accounted inside
// the approved private parent; zero/overflow never disables an owner bound.
func TestManagementSshSettingsRetainHealthyLifetimeAndPrivateAccounting(t *testing.T) {
	settings := DefaultManagementSshSettings()
	if settings.validate() != nil || settings.MaximumRuntime <= DefaultLimits().MaximumJobDuration || settings.MemoryBytes >= DefaultManagementBoundarySettings().PrivateMemoryBytes {
		t.Fatal("managed SSH changed the healthy lifetime or allocated another private reserve")
	}
	for _, mutate := range []func(*ManagementSshSettings){
		func(value *ManagementSshSettings) { value.MemoryBytes = 0 },
		func(value *ManagementSshSettings) {
			value.MemoryBytes = DefaultManagementBoundarySettings().PrivateMemoryBytes + 1
		},
		func(value *ManagementSshSettings) { value.TasksMaximum = 0 },
		func(value *ManagementSshSettings) { value.MaximumControls = 0 },
		func(value *ManagementSshSettings) { value.MaximumControls = math.MaxInt },
		func(value *ManagementSshSettings) { value.MaximumRuntime = DefaultLimits().MaximumJobDuration },
		func(value *ManagementSshSettings) { value.JoinTimeout = value.Agent.CancelGrace },
		func(value *ManagementSshSettings) { value.PollInterval = 0 },
		func(value *ManagementSshSettings) { value.Agent.ConfigBytes = math.MaxInt64 },
		func(value *ManagementSshSettings) { value.Agent.ManagementCpuQuotaPercent = math.MaxInt },
	} {
		invalid := settings
		mutate(&invalid)
		if invalid.validate() == nil {
			t.Fatalf("unbounded managed SSH settings were accepted: %+v", invalid)
		}
	}
}

// No caller command, environment, unit, namespace or durable failure hook can
// enter the fixed launcher argv; --serve is the only selected child role.
func TestManagementSshArgumentsAreClosedAndNeverReapDurableEvaluation(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	plan := managementSshTestPlan(config, policy, settings)
	marker := filepath.Join(config.StateDirectory, "managed-controls", plan.Id+".admit")
	t.Setenv("SSH_ORIGINAL_COMMAND", "synthetic arbitrary command")
	t.Setenv("DOCKER_HOST", "tcp://synthetic.example:1")
	arguments, err := managementSshArguments(config, plan, marker, settings)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, "\n")
	for _, required := range []string{"--pipe", "--wait", "--remain-after-exit", "--service-type=exec", "--property=Slice=" + managementPrivateSliceName,
		"--property=MemoryMax=268435456", "--property=TasksMax=128", "--property=ConditionPathExists=" + marker,
		"--property=CapabilityBoundingSet=" + managementSshCapabilities, "--property=AmbientCapabilities="} {
		if !strings.Contains(joined, required+"\n") {
			t.Fatalf("managed SSH omitted fixed placement: %s", required)
		}
	}
	for _, forbidden := range []string{"synthetic arbitrary command", "tcp://", "OnFailure", "--recover-active", "--execute", "--docker-owner", "--collect", "--scope", "--no-block", "--shell"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("managed SSH selected another owner or environment: %s", forbidden)
		}
	}
	if got := strings.Join(arguments[len(arguments)-5:], " "); got != "-- "+config.Agent.Path+" --config "+config.configPath+" --serve" {
		t.Fatalf("managed SSH changed its exact final argv: %s", got)
	}
	if _, err := managementSshArguments(config, plan, marker+"-other", settings); err == nil {
		t.Fatal("another marker path acquired root launch authority")
	}
	for _, path := range []string{"/synthetic/agent*", "/synthetic/agent?", "/synthetic/agent[0]", "/synthetic/agent:other", "/synthetic/agent,other", "/synthetic/agent#other", "/synthetic/agent;other"} {
		if managementSshAuthorityPath(path) {
			t.Fatalf("rendered root authority retained expansion syntax: %s", path)
		}
	}
}

// The config-bound marker is a live prerequisite after PID1 condition checking,
// not permission retained after transport withdrawal or an arbitrary plan path.
func TestManagementSshAdmissionMarkerClosesLateChildAndPinsOriginalClock(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	registry, err := openManagementSshRegistry(config, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	plan := managementSshTestPlan(config, policy, settings)
	if err := registry.create(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	actual, _, _, err := readManagementSshAdmission(registry, config, policy, plan.UnitName, settings)
	if err != nil || !actual.CreatedAt.Equal(plan.CreatedAt) || !actual.ExpiresAt.Equal(plan.ExpiresAt) {
		t.Fatalf("live control receipt changed its original session lifetime: %+v %v", actual, err)
	}
	changed := config
	changed.configSha256 = strings.Repeat("0", 64)
	if _, _, _, err := readManagementSshAdmission(registry, changed, policy, plan.UnitName, settings); err == nil {
		t.Fatal("changed protected config retained old admission permission")
	}
	if err := registry.withdraw(plan.Id); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readManagementSshAdmission(registry, config, policy, plan.UnitName, settings); err == nil {
		t.Fatal("late child used the old condition result after marker withdrawal")
	}
	if err := registry.create(t.Context(), plan); err == nil {
		t.Fatal("the same control receipt was replayed")
	}
}

// Physical descriptors are duplicated without consuming a byte. Admission can
// fail before launch while the caller's original pipe remains fully usable.
func TestManagementSshNeverReadsFramesBeforePlacement(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	host := managementSshTestHost(config, policy, settings)
	entered, release := make(chan struct{}), make(chan struct{})
	refusal := errors.New("synthetic private management unavailable")
	host.admit = func(context.Context, *Config) (*ManagementResourcePolicy, Pin, error) {
		close(entered)
		<-release
		return nil, Pin{}, refusal
	}
	starts := 0
	host.start = func(context.Context, *Config, managementSshPlan, string, []*os.File) (managementSshProcess, error) {
		starts++
		return nil, refusal
	}
	inRead, inWrite, _, outWrite, _, diagWrite := managementSshTestPipes(t)
	payload := []byte("synthetic unread frame")
	if _, err := inWrite.Write(payload); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- host.serve(t.Context(), config.configPath, inRead, outWrite, diagWrite) }()
	<-entered
	observed := make([]byte, len(payload))
	if _, err := io.ReadFull(inRead, observed); err != nil || !bytes.Equal(observed, payload) {
		t.Fatal("wrapper consumed protocol bytes before placement")
	}
	close(release)
	if err := <-result; !errors.Is(err, refusal) || starts != 0 {
		t.Fatal("refused placement launched or changed its owning error", err)
	}
	if _, err := inWrite.Write([]byte("x")); err != nil {
		t.Fatal("wrapper closed its caller-owned input")
	}
	if _, err := io.ReadFull(inRead, observed[:1]); err != nil || observed[0] != 'x' {
		t.Fatal("caller pipe no longer worked after refused admission")
	}
}

// The active/exited observation must cause Stop while --wait is pending.
// Wait cannot complete until Stop, so reversing that ownership would deadlock.
func TestManagementSshStopsExitedServiceBeforeWaitingForMonitor(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	host := managementSshTestHost(config, policy, settings)
	started, stopped, waitReturned, retired := &atomic.Bool{}, &atomic.Bool{}, &atomic.Bool{}, &atomic.Bool{}
	waitEntered, stopRelease := make(chan struct{}), make(chan struct{})
	var stopOnce sync.Once
	host.start = func(_ context.Context, _ *Config, plan managementSshPlan, marker string, files []*os.File) (managementSshProcess, error) {
		if _, err := os.Stat(marker); err != nil || len(files) != 3 {
			t.Error("manager issue preceded fsynced marker/physical descriptor capture")
		}
		started.Store(true)
		return &managementSshTestProcess{wait: func() error { close(waitEntered); <-stopRelease; waitReturned.Store(true); return nil },
			kill: func() error { stopOnce.Do(func() { close(stopRelease) }); return nil }}, nil
	}
	host.observe = func(_ context.Context, _ *Config, _ *ManagementResourcePolicy, _ Pin, plan managementSshPlan) (managementSshUnit, error) {
		return managementSshUnit{exists: started.Load() && !retired.Load(), completed: started.Load(), inactive: stopped.Load(), invocationId: strings.Repeat("e", 32)}, nil
	}
	host.stop = func(_ context.Context, _ Pin, unit string) error {
		if !validManagementSshUnit(unit) || validUnitName(unit) {
			t.Error("transport cancellation targeted a durable evaluation service")
		}
		<-waitEntered
		stopped.Store(true)
		stopOnce.Do(func() { close(stopRelease) })
		return nil
	}
	host.empty = func(context.Context, string) (bool, error) { return stopped.Load(), nil }
	host.resetFailed = func(context.Context, Pin, string) error { retired.Store(true); return nil }
	host.beforeJoinedReturn = func() {
		if !waitReturned.Load() {
			t.Error("wrapper returned before its physical monitor wait joined")
		}
	}
	inRead, _, _, outWrite, _, diagWrite := managementSshTestPipes(t)
	if err := host.serve(t.Context(), config.configPath, inRead, outWrite, diagWrite); err != nil || !stopped.Load() || !waitReturned.Load() {
		t.Fatal("exited control service wait/stop ownership did not join", err)
	}
	entries, err := os.ReadDir(filepath.Join(config.StateDirectory, "managed-controls"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "registry.lock" {
		t.Fatal("positively joined ephemeral custody was not retired", err)
	}
}

// Transport withdrawal closes only its control marker/unit. A previously
// committed evaluation retains its durable identity and original deadline.
func TestManagementSshCancellationJoinsOnlyItsControlUnit(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	now := settings.Agent.Now().UTC()
	settings.Agent.Now = func() time.Time { return now }
	host := managementSshTestHost(config, policy, settings)
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	durableRun := RunId("11111111-1111-4111-8111-111111111111", 1, 1)
	durable := Identity{HostId: "synthetic-executor", RunId: durableRun, JobId: "11111111-1111-4111-8111-111111111111", RoundId: "22222222-2222-4222-8222-222222222222",
		RoundEpoch: 2, Cycle: 1, Attempt: 1, StartedAt: now, DeadlineAt: now.Add(DefaultLimits().MaximumJobDuration)}
	durableBytes, err := json.Marshal(durable)
	if err != nil {
		t.Fatal(err)
	}
	durablePath := filepath.Join(config.StateDirectory, "synthetic-durable-slot.json")
	if err := os.WriteFile(durablePath, durableBytes, 0400); err != nil {
		t.Fatal(err)
	}
	started, stopped, retired := false, false, false
	waitRelease := make(chan struct{})
	var stopOnce sync.Once
	var marker string
	host.start = func(_ context.Context, _ *Config, _ managementSshPlan, admission string, _ []*os.File) (managementSshProcess, error) {
		started, marker = true, admission
		return &managementSshTestProcess{wait: func() error { <-waitRelease; return nil }, kill: func() error { stopOnce.Do(func() { close(waitRelease) }); return nil }}, nil
	}
	host.observe = func(context.Context, *Config, *ManagementResourcePolicy, Pin, managementSshPlan) (managementSshUnit, error) {
		if started && !stopped {
			cancel()
		}
		return managementSshUnit{exists: started && !retired, inactive: stopped, invocationId: strings.Repeat("e", 32)}, nil
	}
	host.stop = func(_ context.Context, _ Pin, unit string) error {
		if unit == unitName(durableRun) || !validManagementSshUnit(unit) {
			t.Error("transport withdrawal selected the durable evaluator")
		}
		if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Error("control stop preceded late-admission withdrawal")
		}
		stopped = true
		stopOnce.Do(func() { close(waitRelease) })
		return nil
	}
	host.empty = func(context.Context, string) (bool, error) { return stopped, nil }
	host.resetFailed = func(context.Context, Pin, string) error { retired = true; return nil }
	inRead, _, _, outWrite, _, diagWrite := managementSshTestPipes(t)
	if err := host.serve(parent, config.configPath, inRead, outWrite, diagWrite); !errors.Is(err, context.Canceled) || errors.Is(err, ErrManagedSshUnknown) || !stopped {
		t.Fatal("canceled transport did not positively join only its own control", err)
	}
	after, err := os.ReadFile(durablePath)
	if err != nil || !bytes.Equal(after, durableBytes) {
		t.Fatal("transport cancellation reset the durable run or deadline")
	}
}

// Once the observed invocation changes, later stop/empty/monitor evidence can
// physically join the control but cannot erase its ambiguous generation.
func TestManagementSshChangedGenerationRetainsUnknownAfterPhysicalJoin(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	host := managementSshTestHost(config, policy, settings)
	started, stopped, observations := false, false, 0
	waitRelease := make(chan struct{})
	var stopOnce sync.Once
	host.start = func(context.Context, *Config, managementSshPlan, string, []*os.File) (managementSshProcess, error) {
		started = true
		return &managementSshTestProcess{wait: func() error { <-waitRelease; return nil }, kill: func() error { stopOnce.Do(func() { close(waitRelease) }); return nil }}, nil
	}
	host.observe = func(context.Context, *Config, *ManagementResourcePolicy, Pin, managementSshPlan) (managementSshUnit, error) {
		if !started || stopped {
			return managementSshUnit{}, nil
		}
		observations++
		invocation := strings.Repeat("e", 32)
		if observations > 1 {
			invocation = strings.Repeat("f", 32)
		}
		return managementSshUnit{exists: true, invocationId: invocation}, nil
	}
	host.stop = func(context.Context, Pin, string) error {
		stopped = true
		stopOnce.Do(func() { close(waitRelease) })
		return nil
	}
	host.empty = func(context.Context, string) (bool, error) { return stopped, nil }
	inRead, _, _, outWrite, _, diagWrite := managementSshTestPipes(t)
	if err := host.serve(t.Context(), config.configPath, inRead, outWrite, diagWrite); !errors.Is(err, ErrManagedSshUnknown) || !stopped {
		t.Fatal("changed invocation became positive retained cleanup", err)
	}
	registry, err := openManagementSshRegistry(config, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	closed, err := registry.read(strings.Repeat("d", 64) + ".closed")
	var receipt managementSshClosed
	if err != nil || decodeStrict(closed, &receipt) != nil || receipt.Outcome != "unknown" || receipt.InvocationId != strings.Repeat("e", 32) {
		t.Fatal("changed invocation lost its original unknown receipt", err)
	}
}

// Lost acknowledgement never turns missing into inactive. Explicitly ending
// the join owner retains closed metadata, original clocks and bounded custody.
func TestManagementSshLostLaunchAckRetainsUnknownWithoutReplay(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	settings.MaximumControls = 1
	host := managementSshTestHost(config, policy, settings)
	joinCreated := make(chan context.CancelFunc, 1)
	host.joinContext = func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		joinCreated <- cancel
		return ctx, cancel
	}
	var issued managementSshPlan
	starts := 0
	host.start = func(_ context.Context, _ *Config, plan managementSshPlan, _ string, _ []*os.File) (managementSshProcess, error) {
		issued, starts = plan, starts+1
		return &managementSshTestProcess{wait: func() error { return errors.New("synthetic lost manager acknowledgement") }, kill: func() error { return nil }}, nil
	}
	host.observe = func(context.Context, *Config, *ManagementResourcePolicy, Pin, managementSshPlan) (managementSshUnit, error) {
		return managementSshUnit{}, nil
	}
	host.stop = func(context.Context, Pin, string) error {
		return errors.New("synthetic missing unit is not a stop ack")
	}
	host.empty = func(context.Context, string) (bool, error) { return true, nil }
	inRead, _, _, outWrite, _, diagWrite := managementSshTestPipes(t)
	result := make(chan error, 1)
	go func() { result <- host.serve(t.Context(), config.configPath, inRead, outWrite, diagWrite) }()
	finishJoin := <-joinCreated
	if _, err := os.Lstat(filepath.Join(config.StateDirectory, "managed-controls", issued.Id+".admit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unknown control retained admission for a late child")
	}
	finishJoin()
	if err := <-result; !errors.Is(err, ErrManagedSshUnknown) {
		t.Fatal("lost start acknowledgement was treated as a positive join", err)
	}
	registry, err := openManagementSshRegistry(config, settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	content, err := registry.read(issued.Id + ".json")
	var retained managementSshPlan
	if err != nil || decodeStrict(content, &retained) != nil || retained.Id != issued.Id || !retained.CreatedAt.Equal(issued.CreatedAt) || !retained.ExpiresAt.Equal(issued.ExpiresAt) {
		t.Fatal("unknown control lost or reset its immutable receipt")
	}
	closed, err := registry.read(issued.Id + ".closed")
	var receipt managementSshClosed
	if err != nil || decodeStrict(closed, &receipt) != nil || receipt.Outcome != "unknown" {
		t.Fatal("unknown control was mislabeled joined")
	}
	host.newId = func() (string, error) { return strings.Repeat("f", 64), nil }
	if err := host.serve(t.Context(), config.configPath, inRead, outWrite, diagWrite); err == nil || starts != 1 {
		t.Fatal("full unknown-control registry replayed or admitted another manager start")
	}
}

// Unsupported/typed-nil/nonphysical transports fail before root metadata,
// native proof or any unauthenticated data access.
func TestManagementSshRejectsUnsupportedAndWrongDirectionStreams(t *testing.T) {
	inRead, inWrite, _, outWrite, _, diagWrite := managementSshTestPipes(t)
	for _, streams := range []struct {
		input  io.Reader
		output io.Writer
	}{
		{input: (*os.File)(nil), output: outWrite},
		{input: &bytes.Buffer{}, output: outWrite},
		{input: inRead, output: &bytes.Buffer{}},
		{input: inWrite, output: outWrite},
		{input: inRead, output: inRead},
	} {
		if files, err := captureManagementSshFiles(streams.input, streams.output, diagWrite); err == nil {
			for _, file := range files {
				_ = file.Close()
			}
			t.Fatal("unsupported transport acquired a pre-framing copy owner")
		}
	}
}

// This single test subprocess models PID1's inherited pipes and --wait. It
// echoes bounded bytes, reports active/exited, then waits for the exact stop.
func TestManagementSshInheritedPipeHelper(t *testing.T) {
	if os.Getenv("URNETWORK_TEST_MANAGED_SSH_PIPE") != "1" {
		t.Skip("private inherited-descriptor subprocess only")
	}
	status, stop := os.NewFile(3, "synthetic-exited"), os.NewFile(4, "synthetic-stop")
	defer status.Close()
	defer stop.Close()
	buffer := make([]byte, len("synthetic framed bytes"))
	if _, err := io.ReadFull(os.Stdin, buffer); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stdout.Write(buffer); err != nil {
		t.Fatal(err)
	}
	if _, err := status.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(stop, buffer[:1]); err != nil || buffer[0] != 1 {
		t.Fatal("synthetic monitor did not receive its exact stop")
	}
	os.Exit(0) // Suppress the test harness's own stdout outside the bounded synthetic payload.
}

// The real child process cannot terminate before Stop writes its inherited
// release descriptor. The wrapper must join it and all passed FD ownership.
func TestManagementSshJoinsActualInheritedPipeProcessAfterExactStop(t *testing.T) {
	config, policy, settings := managementSshTestConfig(t)
	host := managementSshTestHost(config, policy, settings)
	inRead, inWrite, outRead, outWrite, _, diagWrite := managementSshTestPipes(t)
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stopRead, stopWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer statusRead.Close()
	defer statusWrite.Close()
	defer stopRead.Close()
	defer stopWrite.Close()
	started, completed, stopped, waited, retired := &atomic.Bool{}, &atomic.Bool{}, &atomic.Bool{}, &atomic.Bool{}, &atomic.Bool{}
	childReady, exited := make(chan struct{}), make(chan struct{})
	var command *exec.Cmd
	host.start = func(_ context.Context, _ *Config, _ managementSshPlan, _ string, files []*os.File) (managementSshProcess, error) {
		command = exec.Command(os.Args[0], "-test.run=^TestManagementSshInheritedPipeHelper$", "-test.count=1")
		command.Env = append(controlEnvironment(), "URNETWORK_TEST_MANAGED_SSH_PIPE=1")
		command.Stdin, command.Stdout, command.Stderr = files[0], files[1], files[2]
		command.ExtraFiles = []*os.File{statusWrite, stopRead}
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := command.Start(); err != nil {
			return nil, err
		}
		// Only the child keeps these ends after exec; failure must make the
		// status reader observe EOF rather than a leaked parent writer.
		_ = statusWrite.Close()
		_ = stopRead.Close()
		started.Store(true)
		close(childReady)
		return &managementSshTestProcess{wait: func() error { err := command.Wait(); waited.Store(true); return err }, kill: func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }}, nil
	}
	host.observe = func(context.Context, *Config, *ManagementResourcePolicy, Pin, managementSshPlan) (managementSshUnit, error) {
		return managementSshUnit{exists: started.Load() && !retired.Load(), completed: completed.Load(), inactive: stopped.Load(), invocationId: strings.Repeat("e", 32)}, nil
	}
	host.stop = func(_ context.Context, _ Pin, unit string) error {
		if !validManagementSshUnit(unit) || !completed.Load() {
			return errors.New("synthetic stop selected another owner or preceded active/exited")
		}
		if _, err := stopWrite.Write([]byte{1}); err != nil {
			return err
		}
		stopped.Store(true)
		return nil
	}
	host.empty = func(context.Context, string) (bool, error) { return stopped.Load(), nil }
	host.resetFailed = func(context.Context, Pin, string) error { retired.Store(true); return nil }
	host.beforeJoinedReturn = func() {
		if !waited.Load() || command.ProcessState == nil || !command.ProcessState.Exited() {
			t.Error("managed SSH returned while its real monitor process retained work")
		}
	}
	go func() {
		defer close(exited)
		var value [1]byte
		if _, err := io.ReadFull(statusRead, value[:]); err == nil && value[0] == 1 {
			completed.Store(true)
		}
	}()
	result := make(chan error, 1)
	go func() { result <- host.serve(t.Context(), config.configPath, inRead, outWrite, diagWrite) }()
	<-childReady
	payload := []byte("synthetic framed bytes")
	if _, err := inWrite.Write(payload); err != nil {
		t.Fatal(err)
	}
	observed := make([]byte, len(payload))
	if _, err := io.ReadFull(outRead, observed); err != nil || !bytes.Equal(payload, observed) {
		t.Fatal("inherited physical pipes changed control bytes")
	}
	<-exited
	if err := <-result; err != nil || !waited.Load() || !stopped.Load() {
		t.Fatal("inherited control monitor did not join after exact stop", err)
	}
}
