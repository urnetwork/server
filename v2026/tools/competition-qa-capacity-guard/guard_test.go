// Exercise the actual private shell guards using only owned fake commands.
// No systemd, database, Docker, cache scan or evaluation process is contacted.
package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Each fake du call has an explicit acknowledgement and an optional owner cut.
type measurementStep struct {
	Value          string
	Format         string
	Exit           int
	NextInvocation string
	NextPid        string
	CancelOwner    bool
	Hang           bool
}

// The fake command environment is private to one guard subprocess.
type guardPlan struct {
	Du          []measurementStep
	Df          []measurementStep
	SleepAction string
	ShowFault   string
}

// Fake manager state permits a deterministic replacement immediately after
// the last failed measurement and before the guard's final signal check.
type managerState struct {
	Invocation string
	Pid        string
	Active     string
	Load       string
	DuCalls    int
	DfCalls    int
	DuStopped  int
	Sleeps     int
	Kills      []ownedSignal
}

// No native signal is issued by the fake manager.
type ownedSignal struct {
	Unit       string
	Invocation string
	Pid        string
	Arguments  []string
}

// A source identity is supplied by the independent owner's frozen manifest.
type guardSource struct {
	Name string
	Path string
	Unit string
}

// The result includes physically joined command status and finite diagnostics.
type guardResult struct {
	Code  int
	State managerState
	Log   string
}

// Helpers run inside the same independently built test executable. Race
// detection remains enabled; only per-helper race exit sleeping is disabled.
func TestMain(m *testing.M) {
	if os.Getenv("CAPACITY_GUARD_FAKE") == "1" {
		os.Exit(runFakeCommand())
	}
	os.Exit(m.Run())
}

// Read only the explicit approved source files; never discover or scan hosts.
func guardSources(t *testing.T, historical bool) []guardSource {
	t.Helper()
	prefix := "QA_GUARD_"
	if historical {
		prefix += "OLD_"
	}
	var sources []guardSource
	for _, name := range []string{"NORMAL", "RACE"} {
		path := os.Getenv(prefix + name)
		if !filepath.IsAbs(path) {
			t.Fatal("explicit frozen guard source paths are required", prefix+name)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		unit := ""
		for _, line := range strings.Split(string(content), "\n") {
			if strings.HasPrefix(line, "unit=") {
				if unit != "" {
					t.Fatal("source has more than one owner declaration")
				}
				unit = strings.TrimPrefix(line, "unit=")
			}
		}
		if !strings.HasSuffix(unit, ".service") || strings.ContainsAny(unit, " \t'\";$") {
			t.Fatal("source owner is not an exact closed service name")
		}
		sources = append(sources, guardSource{Name: name, Path: path, Unit: unit})
	}
	return sources
}

// All fake children acknowledge and exit; the timeout is only a failing test
// watchdog, never evidence that a successful lifecycle transition occurred.
func runGuard(t *testing.T, source guardSource, plan guardPlan) guardResult {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"du", "df", "systemctl", "sleep"} {
		if err := os.Symlink(executable, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	state := managerState{Invocation: strings.Repeat("a", 32), Pid: "4242", Active: "active", Load: "loaded"}
	writeFixtureJson(t, filepath.Join(root, "plan.json"), plan)
	writeFixtureJson(t, filepath.Join(root, "state.json"), state)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/bash", source.Path)
	command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "QA_EVIDENCE="+root, "CAPACITY_GUARD_FAKE=1", "CAPACITY_GUARD_ROOT="+root,
		"GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	command.WaitDelay = time.Second
	err = command.Run()
	if ctx.Err() != nil {
		t.Fatal("guard did not join before its test watchdog", ctx.Err(), output.String())
	}
	code := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatal("guard process did not return a native exit status", err, output.String())
		}
		code = exitError.ExitCode()
	}
	readFixtureJson(t, filepath.Join(root, "state.json"), &state)
	log, logErr := os.ReadFile(filepath.Join(root, "guard.log"))
	if logErr != nil && !errors.Is(logErr, os.ErrNotExist) {
		t.Fatal(logErr)
	}
	return guardResult{Code: code, State: state, Log: string(log)}
}

// Fixture data is synthetic and has no production identity or credentials.
func writeFixtureJson(t *testing.T, path string, value any) {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
}

// Decode exact state produced by the owned helper command sequence.
func readFixtureJson(t *testing.T, path string, target any) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, target); err != nil {
		t.Fatal(err)
	}
}

// A positive stop receipt must name only the source's original actor cut.
func requireOwnedStop(t *testing.T, source guardSource, result guardResult) {
	t.Helper()
	if result.Code == 0 || len(result.State.Kills) != 1 || !strings.Contains(result.Log, "GUARD_STOP") {
		t.Fatal(source.Name, "guard failed to fence its owned actor", result)
	}
	stop := result.State.Kills[0]
	if stop.Unit != source.Unit || stop.Invocation != strings.Repeat("a", 32) || stop.Pid != "4242" ||
		!reflect.DeepEqual(stop.Arguments, []string{"kill", "--kill-who=main", "--signal=TERM", source.Unit}) {
		t.Fatal(source.Name, "guard signaled an unowned actor or broader surface", stop)
	}
}

// Both owners retain one qualified implementation and distinct literal units.
func TestGuardVariantsShareExactLogic(t *testing.T) {
	sources := guardSources(t, false)
	var bodies []string
	for _, source := range sources {
		content, err := os.ReadFile(source.Path)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, strings.Replace(string(content), "unit="+source.Unit, "unit=synthetic-owner.service", 1))
	}
	if sources[0].Unit == sources[1].Unit || bodies[0] != bodies[1] {
		t.Fatal("normal and race guards diverged beyond their exact owned unit")
	}
}

// A plausible partial total with nonzero status must not become the baseline.
func TestGuardTransientMeasurementNeedsCompleteAcknowledgement(t *testing.T) {
	for _, source := range guardSources(t, false) {
		result := runGuard(t, source, guardPlan{Du: []measurementStep{{Value: "999999999999", Exit: 1}, {Value: "1000"}}})
		if result.Code != 0 || len(result.State.Kills) != 0 || result.State.DuCalls != 4 ||
			strings.Count(result.Log, "GUARD_MEASUREMENT_RETRY") != 1 || !strings.Contains(result.Log, "cache_growth=0") {
			t.Fatal(source.Name, "transient failed summary was accepted or valid retry lost", result)
		}
	}
}

// Exhaustion is exactly three fresh measurements and a positive owned stop.
func TestGuardPersistentMeasurementFailureFencesOwner(t *testing.T) {
	for _, source := range guardSources(t, false) {
		steps := []measurementStep{{Value: "1000", Exit: 1}, {Value: "1000", Exit: 1}, {Value: "1000", Exit: 1}}
		result := runGuard(t, source, guardPlan{Du: steps})
		requireOwnedStop(t, source, result)
		if result.State.DuCalls != 3 || strings.Count(result.Log, "command_status=1") != 3 {
			t.Fatal(source.Name, "retry bound or failed-command acknowledgement changed", result)
		}
	}
}

// Exit zero alone does not authenticate malformed, blank or overflowed totals.
func TestGuardRejectsMalformedMeasurements(t *testing.T) {
	for _, source := range guardSources(t, false) {
		for _, step := range []measurementStep{{Format: "blank"}, {Value: "garbage"}, {Value: "001"}, {Value: "-1"}, {Value: "9223372036854775808"}, {Value: "1000", Format: "extra"}} {
			result := runGuard(t, source, guardPlan{Du: []measurementStep{step, step, step}})
			requireOwnedStop(t, source, result)
			if result.State.DuCalls != 3 || strings.Count(result.Log, "GUARD_MEASUREMENT_RETRY") != 3 {
				t.Fatal(source.Name, "malformed successful output bypassed retry bounds", step, result)
			}
		}
	}
}

// Native timeout must terminate and physically join each acknowledged hung
// command. This exercises the real timeout utility, not a fake success timer.
func TestGuardHungMeasurementIsBoundedAndJoined(t *testing.T) {
	source := guardSources(t, false)[0]
	step := measurementStep{Hang: true}
	result := runGuard(t, source, guardPlan{Du: []measurementStep{step, step, step}})
	requireOwnedStop(t, source, result)
	if result.State.DuCalls != 3 || result.State.DuStopped != 3 || strings.Count(result.Log, "command_status=124") != 3 {
		t.Fatal("hung command was not stopped, joined and refused on every attempt", result)
	}
}

// Replacement after the final failed command must revoke signal authority.
func TestGuardNeverSignalsSuccessorInvocationOrPid(t *testing.T) {
	for _, source := range guardSources(t, false) {
		for _, last := range []measurementStep{{Exit: 1, NextInvocation: strings.Repeat("b", 32)}, {Exit: 1, NextPid: "4343"}} {
			result := runGuard(t, source, guardPlan{Du: []measurementStep{{Exit: 1}, {Exit: 1}, last}})
			if result.Code == 0 || len(result.State.Kills) != 0 || !strings.Contains(result.Log, "GUARD_REFUSE_SIGNAL") || result.State.DuCalls != 3 {
				t.Fatal(source.Name, "a successor inherited old signal authority", result)
			}
		}
	}
}

// Cancellation of the guarded owner ends retries without signaling any actor.
func TestGuardOwnerCancellationStopsFurtherMeasurements(t *testing.T) {
	for _, source := range guardSources(t, false) {
		result := runGuard(t, source, guardPlan{Du: []measurementStep{{Exit: 1, CancelOwner: true}}})
		if result.Code != 0 || result.State.DuCalls != 1 || len(result.State.Kills) != 0 {
			t.Fatal(source.Name, "measurement continued after positive owner termination", result)
		}
	}
}

// A signal sent by the owned sleep helper cancels the actual guard shell;
// its trap must fence only the still-live original fake actor before joining.
func TestGuardCancellationFencesOnlyOwnedActor(t *testing.T) {
	for _, source := range guardSources(t, false) {
		result := runGuard(t, source, guardPlan{SleepAction: "cancel_guard"})
		requireOwnedStop(t, source, result)
		if result.Code != 130 || result.State.Sleeps != 1 || !strings.Contains(result.Log, "reason=guard_cancelled") {
			t.Fatal(source.Name, "actual shell cancellation lost custody or was reported green", result)
		}
	}
}

// All original reserve/growth limits still stop only the admitted owner.
func TestGuardPreservesAllResourceBounds(t *testing.T) {
	for _, source := range guardSources(t, false) {
		for _, plan := range []guardPlan{
			{Df: []measurementStep{{Value: "35433480191"}}},
			{Du: []measurementStep{{Value: "1000"}, {Value: "2147484648"}}},
			{Du: []measurementStep{{Value: "1000"}, {Value: "1000"}, {Value: "1342177280"}}},
		} {
			result := runGuard(t, source, plan)
			requireOwnedStop(t, source, result)
			if !strings.Contains(result.Log, "reason=resource_limit") {
				t.Fatal(source.Name, "an original resource bound changed", result)
			}
		}
	}
}

// Unknown or malformed manager output never grants even initial authority.
func TestGuardUnknownManagerCannotAuthorizeSignal(t *testing.T) {
	for _, source := range guardSources(t, false) {
		for _, fault := range []string{"missing", "duplicate"} {
			result := runGuard(t, source, guardPlan{ShowFault: fault})
			if result.Code == 0 || result.State.DuCalls != 0 || len(result.State.Kills) != 0 || !strings.Contains(result.Log, "GUARD_REFUSE_SIGNAL") {
				t.Fatal(source.Name, "unknown manager projection became authority", result)
			}
		}
	}
}

// The unchanged historical source demonstrably exits on the same failed du
// without issuing the required actor stop; this is not a build/setup failure.
func TestGuardHistoricalPartialFailureLosesMonitoring(t *testing.T) {
	for _, source := range guardSources(t, true) {
		result := runGuard(t, source, guardPlan{Du: []measurementStep{{Value: "1000", Exit: 1}}})
		if result.Code != 1 || result.State.DuCalls != 1 || len(result.State.Kills) != 0 || result.State.Active != "active" || strings.Contains(result.Log, "GUARD_STOP") {
			t.Fatal(source.Name, "historical source did not reproduce the unguarded-actor defect", result)
		}
	}
}

// Execute one fake command and persist all acknowledgements before returning.
func runFakeCommand() int {
	root := os.Getenv("CAPACITY_GUARD_ROOT")
	var plan guardPlan
	var state managerState
	read := func(name string, target any) error {
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return err
		}
		return json.Unmarshal(content, target)
	}
	write := func() error {
		content, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, "state.json"), content, 0600)
	}
	if root == "" || read("plan.json", &plan) != nil || read("state.json", &state) != nil {
		return 97
	}
	arguments := os.Args[1:]
	switch filepath.Base(os.Args[0]) {
	case "du", "df":
		name := filepath.Base(os.Args[0])
		steps, index := plan.Du, state.DuCalls
		if name == "df" {
			steps, index = plan.Df, state.DfCalls
			state.DfCalls++
		} else {
			state.DuCalls++
		}
		step := measurementStep{Value: "1000"}
		if name == "df" {
			step.Value = "40000000000"
		}
		if index < len(steps) {
			step = steps[index]
		}
		if step.NextInvocation != "" {
			state.Invocation = step.NextInvocation
		}
		if step.NextPid != "" {
			state.Pid = step.NextPid
		}
		if step.CancelOwner {
			state.Active, state.Pid = "inactive", "0"
		}
		if write() != nil {
			return 98
		}
		if step.Hang {
			stopped := make(chan os.Signal, 1)
			signal.Notify(stopped, syscall.SIGTERM)
			<-stopped
			signal.Stop(stopped)
			state.DuStopped++
			if write() != nil {
				return 98
			}
			return 0
		}
		if step.Format == "blank" {
			fmt.Fprintln(os.Stdout)
		} else if name == "df" {
			fmt.Fprintf(os.Stdout, "Avail\n%s\n", step.Value)
		} else if len(arguments) != 0 {
			fmt.Fprintf(os.Stdout, "%s\t%s\n", step.Value, arguments[len(arguments)-1])
			if step.Format == "extra" {
				fmt.Fprintln(os.Stdout, "unexpected second total")
			}
		} else {
			return 96
		}
		return step.Exit
	case "systemctl":
		if len(arguments) < 2 {
			return 95
		}
		if arguments[0] == "show" {
			if strings.Join(arguments, " ") == "show "+arguments[1]+" -p ActiveState --value" {
				fmt.Fprintln(os.Stdout, state.Active)
				return 0
			}
			fmt.Fprintf(os.Stdout, "ActiveState=%s\nInvocationID=%s\nLoadState=%s\n", state.Active, state.Invocation, state.Load)
			if plan.ShowFault != "missing" {
				fmt.Fprintf(os.Stdout, "MainPID=%s\n", state.Pid)
			}
			if plan.ShowFault == "duplicate" {
				fmt.Fprintln(os.Stdout, "LoadState=loaded")
			}
			return 0
		}
		if arguments[0] == "kill" {
			state.Kills = append(state.Kills, ownedSignal{Unit: arguments[len(arguments)-1], Invocation: state.Invocation, Pid: state.Pid, Arguments: arguments})
			state.Active, state.Pid = "inactive", "0"
			if write() != nil {
				return 98
			}
			return 0
		}
		return 94
	case "sleep":
		state.Sleeps++
		if plan.SleepAction != "cancel_guard" {
			state.Active, state.Pid = "inactive", "0"
		}
		if write() != nil {
			return 98
		}
		if plan.SleepAction == "cancel_guard" {
			if err := syscall.Kill(os.Getppid(), syscall.SIGTERM); err != nil {
				return 93
			}
		}
		return 0
	default:
		return 92
	}
}
