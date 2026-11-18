package alt

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// glog cuts a longer message (logsink.MaxLogMessageLen).
const exitProfileTestGlogMessageLimit = 15_000

// Every line the exit profile writes, in order. Safe for concurrent use.
type exitProfileTestLog struct {
	stateLock sync.Mutex
	lines     []string
	// signaled after new lines
	changed chan struct{}
}

func newExitProfileTestLog() *exitProfileTestLog {
	return &exitProfileTestLog{
		changed: make(chan struct{}, 1),
	}
}

func (self *exitProfileTestLog) Logf(format string, args ...any) {
	self.append(fmt.Sprintf(format, args...))
	select {
	case self.changed <- struct{}{}:
	default:
	}
}

func (self *exitProfileTestLog) append(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *exitProfileTestLog) Lines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

// Waits for a line that starts with prefix.
func (self *exitProfileTestLog) WaitFor(t testing.TB, prefix string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		for _, line := range self.Lines() {
			if strings.HasPrefix(line, prefix) {
				return
			}
		}
		select {
		case <-self.changed:
		case <-timeout:
			t.Fatalf("no exit profile line starts with %q", prefix)
		}
	}
}

var (
	exitProfileTestHeaderPattern = regexp.MustCompile(
		`^\[alt\]\[exitprofile\](baseline|sample \d+/\d+) .*goroutines=(\d+) stack_inuse_mib=[0-9.]+ heap_alloc_mib=[0-9.]+ sys_mib=[0-9.]+ mallocs=\d+ profiled=(\d+) stacks=(\d+)\n$`,
	)
	exitProfileTestStackPattern = regexp.MustCompile(
		`^\[alt\]\[exitprofile\](baseline|sample \d+/\d+) stack (\d+)/(\d+) goroutines=(\d+) frames=(\d+)\+?: (.+)\n$`,
	)
	exitProfileTestOtherPattern = regexp.MustCompile(
		`^\[alt\]\[exitprofile\](baseline|sample \d+/\d+) other stacks=(\d+) goroutines=(\d+)\n$`,
	)
	exitProfileTestElisionPattern = regexp.MustCompile(`^\[(\d+) frames elided\]$`)
)

// One profile as logged.
type exitProfileTestProfile struct {
	label      string
	goroutines int
	profiled   int
	distinct   int
	stacks     []*exitProfileTestStack
	// the stacks not shown and their goroutines
	otherStacks     int
	otherGoroutines int
}

// One stack line of a profile.
type exitProfileTestStack struct {
	rank       int
	goroutines int
	// the stack's depth, as the line counts it
	depth int
	// the frames the line names, innermost first
	frames []string
	// the frames left out between them
	elided int
}

func (self *exitProfileTestStack) Names(function string) bool {
	for _, frame := range self.frames {
		if strings.Contains(frame, "."+function+" (") {
			return true
		}
	}
	return false
}

// Groups the lines by profile, in order, and fails on any line that is not a
// bounded line of one of the profile's formats.
func parseExitProfileTestLog(t testing.TB, lines []string) []*exitProfileTestProfile {
	t.Helper()
	profiles := []*exitProfileTestProfile{}
	for _, line := range lines {
		if exitProfileTestGlogMessageLimit < len(line) {
			t.Fatalf("a %d byte line is over glog's limit: %.200q", len(line), line)
		}
		if strings.HasPrefix(line, "[alt][exitprofile]enabled ") {
			continue
		}
		if groups := exitProfileTestHeaderPattern.FindStringSubmatch(line); groups != nil {
			goroutines, _ := strconv.Atoi(groups[2])
			profiled, _ := strconv.Atoi(groups[3])
			distinct, _ := strconv.Atoi(groups[4])
			profiles = append(profiles, &exitProfileTestProfile{
				label:      groups[1],
				goroutines: goroutines,
				profiled:   profiled,
				distinct:   distinct,
			})
			continue
		}
		if len(profiles) == 0 {
			t.Fatalf("a line before any profile header: %q", line)
		}
		profile := profiles[len(profiles)-1]
		if groups := exitProfileTestStackPattern.FindStringSubmatch(line); groups != nil {
			if groups[1] != profile.label {
				t.Fatalf("a %s stack inside the %s profile: %q", groups[1], profile.label, line)
			}
			stack := &exitProfileTestStack{}
			stack.rank, _ = strconv.Atoi(groups[2])
			stack.goroutines, _ = strconv.Atoi(groups[4])
			stack.depth, _ = strconv.Atoi(groups[5])
			for _, frame := range strings.Split(groups[6], " < ") {
				if elision := exitProfileTestElisionPattern.FindStringSubmatch(frame); elision != nil {
					stack.elided, _ = strconv.Atoi(elision[1])
					continue
				}
				stack.frames = append(stack.frames, frame)
			}
			profile.stacks = append(profile.stacks, stack)
			continue
		}
		if groups := exitProfileTestOtherPattern.FindStringSubmatch(line); groups != nil {
			if groups[1] != profile.label {
				t.Fatalf("%s other stacks inside the %s profile: %q", groups[1], profile.label, line)
			}
			profile.otherStacks, _ = strconv.Atoi(groups[2])
			profile.otherGoroutines, _ = strconv.Atoi(groups[3])
			continue
		}
		t.Fatalf("not an exit profile line: %q", line)
	}
	return profiles
}

func exitProfileTestLabels(profiles []*exitProfileTestProfile) []string {
	labels := []string{}
	for _, profile := range profiles {
		labels = append(labels, profile.label)
	}
	return labels
}

// Waits for the sampler to finish its schedule.
func waitForExitProfileSampler(t testing.TB, profile *exitProfile) {
	t.Helper()
	select {
	case <-profile.samplerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the sampler did not finish its schedule")
	}
}

// Stop must return within timeout.
func stopExitProfileWithin(t testing.TB, profile *exitProfile, timeout time.Duration) {
	t.Helper()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		profile.Stop()
	}()
	select {
	case <-stopped:
	case <-time.After(timeout):
		t.Fatalf("Stop held the exit for over %s", timeout)
	}
}

// Waits until count goroutines are parked on a receive in function. A
// goroutine that has signaled its WaitGroup can still be on its way there.
func waitForParkedExitProfileTestGoroutines(t testing.TB, function string, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		parked := parkedExitProfileTestGoroutines(function)
		if parked == count {
			return
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("%d goroutines are parked in %s, want %d", parked, function, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func parkedExitProfileTestGoroutines(function string) int {
	records := make([]runtime.StackRecord, 2*runtime.NumGoroutine()+1024)
	n, ok := runtime.GoroutineProfile(records)
	if !ok {
		return -1
	}
	parked := 0
	for _, record := range records[:n] {
		frames := runtime.CallersFrames(record.Stack())
		receiving := false
		for {
			frame, more := frames.Next()
			if receiving && strings.HasSuffix(frame.Function, "."+function) {
				parked += 1
				break
			}
			receiving = frame.Function == "runtime.chanrecv1"
			if !more {
				break
			}
		}
	}
	return parked
}

// Goroutines a test parks, then releases and waits out, so no profile in a
// later test sees them.
type exitProfileTestPark struct {
	parked  sync.WaitGroup
	exited  sync.WaitGroup
	release chan struct{}
}

func newExitProfileTestPark() *exitProfileTestPark {
	return &exitProfileTestPark{
		release: make(chan struct{}),
	}
}

// Counts one goroutine about to start.
func (self *exitProfileTestPark) Add() {
	self.parked.Add(1)
	self.exited.Add(1)
}

func (self *exitProfileTestPark) Release() {
	close(self.release)
	self.exited.Wait()
}

//go:noinline
func parkExitProfileTestBurst(park *exitProfileTestPark) {
	defer park.exited.Done()
	park.parked.Done()
	<-park.release
}

//go:noinline
func parkExitProfileTestSurvivor(park *exitProfileTestPark) {
	defer park.exited.Done()
	park.parked.Done()
	<-park.release
}

//go:noinline
func startExitProfileTestDeep(depth int, park *exitProfileTestPark) {
	defer park.exited.Done()
	parkExitProfileTestDeep(depth, park)
}

//go:noinline
func parkExitProfileTestDeep(depth int, park *exitProfileTestPark) {
	if depth == 0 {
		park.parked.Done()
		<-park.release
		return
	}
	parkExitProfileTestDeep(depth-1, park)
}

// Off unless the operator asks for it: an unset, false or unreadable value
// takes no profile, and the exit runs as it does without one.
func TestExitProfileIsOffByDefault(t *testing.T) {
	t.Setenv(exitProfileEnvName, "")
	os.Unsetenv(exitProfileEnvName)
	if profile := newExitProfileFromEnv(); profile != nil {
		t.Fatal("the exit profile is on with the environment unset")
	}

	for _, value := range []string{"", "0", "false", "no", "on"} {
		log := newExitProfileTestLog()
		profile := newExitProfile(value, defaultExitProfileSettings(), log.Logf)
		if profile != nil {
			t.Fatalf("%q turned the exit profile on", value)
		}
		events := []string{}
		exitAfterDrain(
			func() { events = append(events, "drain") },
			func() { events = append(events, "cancel") },
			profile,
		)
		profile.Stop()
		if !slices.Equal(events, []string{"drain", "cancel"}) {
			t.Fatalf("%q exit = %v", value, events)
		}
		lines := log.Lines()
		if _, err := strconv.ParseBool(value); err == nil || value == "" {
			if len(lines) != 0 {
				t.Fatalf("%q logged %q", value, lines)
			}
		} else if len(lines) != 1 || lines[0] != "[alt][exitprofile]disabled: URNETWORK_ALT_EXIT_PROFILE is not a boolean\n" {
			t.Fatalf("%q logged %q", value, lines)
		}
	}

	t.Setenv(exitProfileEnvName, "1")
	profile := newExitProfileFromEnv()
	if profile == nil {
		t.Fatalf("%s=1 left the exit profile off", exitProfileEnvName)
	}
	profile.Stop()
}

// The schedule the operator reads the samples against.
func TestExitProfileDefaultSettings(t *testing.T) {
	settings := defaultExitProfileSettings()
	if settings.TopStacks != 40 || settings.StackFrames != 24 {
		t.Fatalf("top stacks = %d, stack frames = %d", settings.TopStacks, settings.StackFrames)
	}
	wantSampleOffsets := []time.Duration{
		100 * time.Millisecond,
		300 * time.Millisecond,
		time.Second,
		3 * time.Second,
	}
	if !slices.Equal(settings.SampleOffsets, wantSampleOffsets) {
		t.Fatalf("sample offsets = %v", settings.SampleOffsets)
	}
	if settings.StopTimeout <= 0 || 10*time.Second < settings.StopTimeout {
		t.Fatalf("stop timeout = %s", settings.StopTimeout)
	}
}

// One profile before the cancel and one at each sample offset, each naming at
// most TopStacks stacks of at most StackFrames frames. A deeper stack keeps its
// innermost frames and its root.
func TestExitProfileLogsTheBaselineAndEverySampleBounded(t *testing.T) {
	park := newExitProfileTestPark()
	defer park.Release()
	// six distinct stacks, more than one profile shows, each deeper than one
	// line names; the groups differ in size, so their ranks are fixed
	groupSizes := []int{}
	parkedCount := 0
	for group := range 6 {
		groupSize := 100 + 10*group
		groupSizes = append(groupSizes, groupSize)
		parkedCount += groupSize
		for range groupSize {
			park.Add()
			go startExitProfileTestDeep(12+group, park)
		}
	}
	park.parked.Wait()
	waitForParkedExitProfileTestGoroutines(t, "parkExitProfileTestDeep", parkedCount)

	log := newExitProfileTestLog()
	settings := &exitProfileSettings{
		TopStacks:   3,
		StackFrames: 4,
		SampleOffsets: []time.Duration{
			0,
			time.Millisecond,
			2 * time.Millisecond,
			3 * time.Millisecond,
		},
		StopTimeout: 10 * time.Second,
	}
	profile := newExitProfile("1", settings, log.Logf)
	if profile == nil {
		t.Fatal("the exit profile is off")
	}
	defer profile.Stop()
	exitAfterDrain(func() {}, func() {}, profile)
	waitForExitProfileSampler(t, profile)

	profiles := parseExitProfileTestLog(t, log.Lines())
	wantLabels := []string{"baseline", "sample 1/4", "sample 2/4", "sample 3/4", "sample 4/4"}
	if labels := exitProfileTestLabels(profiles); !slices.Equal(labels, wantLabels) {
		t.Fatalf("profiles = %v, want %v", labels, wantLabels)
	}
	for _, profile := range profiles {
		if profile.goroutines < parkedCount || profile.profiled < parkedCount {
			t.Fatalf("%s counts %d goroutines and profiles %d, want at least %d", profile.label, profile.goroutines, profile.profiled, parkedCount)
		}
		if len(profile.stacks) != settings.TopStacks {
			t.Fatalf("%s logs %d stacks, want %d", profile.label, len(profile.stacks), settings.TopStacks)
		}
		// the rest are summed on one line
		shownGoroutines := 0
		for _, stack := range profile.stacks {
			shownGoroutines += stack.goroutines
		}
		if profile.distinct < len(groupSizes) ||
			profile.otherStacks != profile.distinct-settings.TopStacks ||
			profile.otherGoroutines != profile.profiled-shownGoroutines {
			t.Fatalf(
				"%s has %d distinct stacks of %d goroutines, and sums %d other stacks of %d goroutines after %d shown",
				profile.label,
				profile.distinct,
				profile.profiled,
				profile.otherStacks,
				profile.otherGoroutines,
				shownGoroutines,
			)
		}
		for i, stack := range profile.stacks {
			if stack.rank != i+1 {
				t.Fatalf("%s stack %d is ranked %d", profile.label, i+1, stack.rank)
			}
			if settings.StackFrames < len(stack.frames) {
				t.Fatalf("%s stack %d names %d frames, over %d", profile.label, stack.rank, len(stack.frames), settings.StackFrames)
			}
			// the largest groups, which are also the deepest
			group := len(groupSizes) - 1 - i
			if stack.goroutines != groupSizes[group] {
				t.Fatalf("%s stack %d has %d goroutines, want %d", profile.label, stack.rank, stack.goroutines, groupSizes[group])
			}
			if !strings.Contains(stack.frames[0], ".parkExitProfileTestDeep (") {
				t.Fatalf("%s stack %d starts at %s", profile.label, stack.rank, stack.frames[0])
			}
			if 0 < i && profile.stacks[i-1].depth <= stack.depth {
				t.Fatalf("%s stack %d is %d frames deep after %d", profile.label, stack.rank, stack.depth, profile.stacks[i-1].depth)
			}
			if stack.elided == 0 || stack.depth != len(stack.frames)+stack.elided {
				t.Fatalf("%s stack %d is %d frames deep, names %d and elides %d", profile.label, stack.rank, stack.depth, len(stack.frames), stack.elided)
			}
			// the root, outside the recursion, stays named
			if root := stack.frames[len(stack.frames)-1]; strings.Contains(root, ".parkExitProfileTestDeep (") {
				t.Fatalf("%s stack %d lost its root: ends at %s", profile.label, stack.rank, root)
			}
		}
	}
}

// A burst the cancel sets off is the top stack of the first sample, and is not
// in the baseline, which is taken after the drain and before the cancel.
func TestExitProfileSampleShowsTheBurstTheCancelSetsOff(t *testing.T) {
	const (
		survivorCount = 300
		burstCount    = 20_000
	)
	park := newExitProfileTestPark()
	defer park.Release()

	log := newExitProfileTestLog()
	profile := newExitProfile("1", &exitProfileSettings{
		TopStacks:     4,
		StackFrames:   6,
		SampleOffsets: []time.Duration{0},
		StopTimeout:   10 * time.Second,
	}, log.Logf)
	if profile == nil {
		t.Fatal("the exit profile is off")
	}
	defer profile.Stop()
	exitAfterDrain(
		// the drain leaves goroutines for the cancel to end
		func() {
			for range survivorCount {
				park.Add()
				go parkExitProfileTestSurvivor(park)
			}
			park.parked.Wait()
			waitForParkedExitProfileTestGoroutines(t, "parkExitProfileTestSurvivor", survivorCount)
		},
		// the cancel fans out, and every goroutine it starts parks
		func() {
			for range burstCount {
				park.Add()
				go parkExitProfileTestBurst(park)
			}
			park.parked.Wait()
			waitForParkedExitProfileTestGoroutines(t, "parkExitProfileTestBurst", burstCount)
		},
		profile,
	)
	waitForExitProfileSampler(t, profile)

	profiles := parseExitProfileTestLog(t, log.Lines())
	if labels := exitProfileTestLabels(profiles); !slices.Equal(labels, []string{"baseline", "sample 1/1"}) {
		t.Fatalf("profiles = %v", labels)
	}
	baseline, sample := profiles[0], profiles[1]
	if top := baseline.stacks[0]; !top.Names("parkExitProfileTestSurvivor") || top.goroutines != survivorCount {
		t.Fatalf("baseline top stack = %d goroutines at %v", top.goroutines, top.frames)
	}
	for _, stack := range baseline.stacks {
		if stack.Names("parkExitProfileTestBurst") {
			t.Fatalf("the baseline has the burst: %d goroutines at %v", stack.goroutines, stack.frames)
		}
	}
	if top := sample.stacks[0]; !top.Names("parkExitProfileTestBurst") || top.goroutines != burstCount {
		t.Fatalf("sample top stack = %d goroutines at %v", top.goroutines, top.frames)
	}
	if second := sample.stacks[1]; !second.Names("parkExitProfileTestSurvivor") || second.goroutines != survivorCount {
		t.Fatalf("sample second stack = %d goroutines at %v", second.goroutines, second.frames)
	}
	if sample.goroutines < survivorCount+burstCount {
		t.Fatalf("sample counts %d goroutines", sample.goroutines)
	}
}

// Stop ends the schedule: the sampler exits with the process instead of
// holding it for the samples still to come.
func TestExitProfileStopEndsTheSchedule(t *testing.T) {
	log := newExitProfileTestLog()
	profile := newExitProfile("1", &exitProfileSettings{
		TopStacks:     2,
		StackFrames:   2,
		SampleOffsets: []time.Duration{0, time.Hour},
		StopTimeout:   time.Hour,
	}, log.Logf)
	if profile == nil {
		t.Fatal("the exit profile is off")
	}
	exitAfterDrain(func() {}, func() {}, profile)
	log.WaitFor(t, "[alt][exitprofile]sample 1/2 at=")

	stopExitProfileWithin(t, profile, 10*time.Second)
	// the sampler has exited, so no later sample can be logged
	select {
	case <-profile.samplerDone:
	default:
		t.Fatal("the sampler outlived Stop")
	}
	profiles := parseExitProfileTestLog(t, log.Lines())
	if labels := exitProfileTestLabels(profiles); !slices.Equal(labels, []string{"baseline", "sample 1/2"}) {
		t.Fatalf("profiles = %v", labels)
	}
}

// A profile stalled in progress, here on a blocked log write, holds Stop for
// at most StopTimeout, and no sample starts after it.
func TestExitProfileStopWaitsForAStalledSampleAtMostTheStopTimeout(t *testing.T) {
	stalled := make(chan struct{})
	unstall := make(chan struct{})
	var unstallOnce sync.Once
	defer unstallOnce.Do(func() { close(unstall) })
	log := newExitProfileTestLog()
	logf := func(format string, args ...any) {
		log.Logf(format, args...)
		if strings.HasPrefix(fmt.Sprintf(format, args...), "[alt][exitprofile]sample 1/2 at=") {
			close(stalled)
			<-unstall
		}
	}
	profile := newExitProfile("1", &exitProfileSettings{
		TopStacks:     2,
		StackFrames:   2,
		SampleOffsets: []time.Duration{0, time.Hour},
		StopTimeout:   50 * time.Millisecond,
	}, logf)
	if profile == nil {
		t.Fatal("the exit profile is off")
	}
	exitAfterDrain(func() {}, func() {}, profile)
	select {
	case <-stalled:
	case <-time.After(10 * time.Second):
		t.Fatal("the first sample was not logged")
	}

	stopExitProfileWithin(t, profile, 10*time.Second)
	select {
	case <-profile.samplerDone:
		t.Fatal("the stalled sample finished before Stop returned")
	default:
	}
	unstallOnce.Do(func() { close(unstall) })
	select {
	case <-profile.samplerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the sampler did not exit after its stalled sample")
	}
	for _, line := range log.Lines() {
		if strings.HasPrefix(line, "[alt][exitprofile]sample 2/2") {
			t.Fatalf("a sample started after Stop: %q", line)
		}
	}
}

// Run can return before a drain ends, when a listener fails, and the drain
// then cancels after Stop: that takes no profile.
func TestExitProfileTakesNoProfileAfterStop(t *testing.T) {
	log := newExitProfileTestLog()
	profile := newExitProfile("1", &exitProfileSettings{
		TopStacks:     2,
		StackFrames:   2,
		SampleOffsets: []time.Duration{0},
		StopTimeout:   10 * time.Second,
	}, log.Logf)
	if profile == nil {
		t.Fatal("the exit profile is off")
	}
	profile.Stop()
	events := []string{}
	exitAfterDrain(
		func() { events = append(events, "drain") },
		func() { events = append(events, "cancel") },
		profile,
	)
	profile.Stop()
	if !slices.Equal(events, []string{"drain", "cancel"}) {
		t.Fatalf("exit = %v", events)
	}
	if profile.samplerDone != nil {
		t.Fatal("a sampler started after Stop")
	}
	for _, line := range log.Lines() {
		if !strings.HasPrefix(line, "[alt][exitprofile]enabled ") {
			t.Fatalf("logged after Stop: %q", line)
		}
	}
}

// A burst can outgrow the record buffer faster than the profile resizes it:
// the attempts are bounded, and the profile then logs the counts alone.
func TestExitProfileGivesUpOnAProfileThatOutgrowsEveryAttempt(t *testing.T) {
	attempts := 0
	_, ok := recordGoroutineStacks(100, func(records []runtime.StackRecord) (int, bool) {
		attempts += 1
		if exitProfileAttempts+2 < attempts {
			// an unbounded retry fails the check below rather than hanging
			return 0, true
		}
		// always one more goroutine than there is room for
		return len(records) + 1, false
	})
	if ok || attempts != exitProfileAttempts {
		t.Fatalf("ok = %t after %d attempts, want false after %d", ok, attempts, exitProfileAttempts)
	}

	// one that fits on its second attempt, sized from the count the first
	// attempt returned
	sizes := []int{}
	records, ok := recordGoroutineStacks(100, func(records []runtime.StackRecord) (int, bool) {
		sizes = append(sizes, len(records))
		if len(sizes) == 1 {
			return 10_000, false
		}
		return 10_000, true
	})
	if !ok || len(records) != 10_000 || len(sizes) != 2 || sizes[1] < 10_000 {
		t.Fatalf("ok = %t with %d records after sizes %v", ok, len(records), sizes)
	}

	log := newExitProfileTestLog()
	profile := newExitProfile("1", defaultExitProfileSettings(), log.Logf)
	if profile == nil {
		t.Fatal("the exit profile is off")
	}
	defer profile.Stop()
	profile.logSnapshot("sample 2/4", "at=300ms elapsed=300ms took=900ms", &exitProfileSnapshot{
		goroutines: 900_000,
		stackInuse: 23 << 30,
		heapAlloc:  11 << 30,
		sys:        40 << 30,
		mallocs:    293_000_000,
	})
	want := "[alt][exitprofile]sample 2/4 at=300ms elapsed=300ms took=900ms goroutines=900000 stack_inuse_mib=23552.0 heap_alloc_mib=11264.0 sys_mib=40960.0 mallocs=293000000 profile=outgrown\n"
	if lines := log.Lines(); len(lines) != 2 || lines[1] != want {
		t.Fatalf("logged %q, want %q", lines, want)
	}
}

// With the defaults, a stack as deep as the runtime records names at most
// StackFrames frames, marks the record's cut root, and fits a glog line; a
// name over its limit is cut.
func TestExitProfileStackLineIsBoundedAtTheDefaults(t *testing.T) {
	var pcs [exitProfileStackDepth]uintptr
	var record func(depth int)
	record = func(depth int) {
		if 0 < depth {
			record(depth - 1)
			return
		}
		deep := make([]uintptr, 4*exitProfileStackDepth)
		copy(pcs[:], deep[:runtime.Callers(1, deep)])
	}
	record(3 * exitProfileStackDepth)

	settings := defaultExitProfileSettings()
	frameCount, frames := exitProfileStackText(pcs, settings.StackFrames)
	if !strings.HasSuffix(frameCount, "+") {
		t.Fatalf("a full record counts %s frames", frameCount)
	}
	named := 0
	elided := false
	for _, frame := range strings.Split(frames, " < ") {
		if exitProfileTestElisionPattern.MatchString(frame) {
			elided = true
			continue
		}
		named += 1
	}
	if named != settings.StackFrames || !elided {
		t.Fatalf("names %d frames (elided %t), want %d: %s", named, elided, settings.StackFrames, frames)
	}
	line := fmt.Sprintf("[alt][exitprofile]sample 4/4 stack 40/40 goroutines=1000000 frames=%s: %s\n", frameCount, frames)
	if exitProfileTestGlogMessageLimit < len(line) {
		t.Fatalf("a %d byte line", len(line))
	}

	longFrame := exitProfileFrameText(runtime.Frame{
		Function: "example.com/" + strings.Repeat("f", 4*exitProfileFunctionLimit),
		File:     "/build/" + strings.Repeat("d", 4*exitProfileFileLimit) + "/" + strings.Repeat("é", 4*exitProfileFileLimit) + ".go",
		Line:     7,
	})
	if exitProfileFunctionLimit+exitProfileFileLimit+32 < len(longFrame) || !strings.HasSuffix(longFrame, "...:7)") {
		t.Fatalf("a %d byte frame: %s", len(longFrame), longFrame)
	}
	if strings.Contains(longFrame, "/build/") || !strings.Contains(longFrame, "é") {
		t.Fatalf("frame file = %s", longFrame)
	}
	if !utf8.ValidString(longFrame) {
		t.Fatalf("the cut split a rune: %q", longFrame)
	}
}

// Run takes the exit profile from the environment, stops it last so its
// samples cover every close, and brackets the drain's cancel with it.
func TestRunBracketsTheCancelWithTheExitProfile(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "run.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var run *ast.FuncDecl
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "runWithDependencies" {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("no runWithDependencies")
	}

	created := false
	stopIndex, cancelIndex := -1, -1
	for i, statement := range run.Body.List {
		switch v := statement.(type) {
		case *ast.AssignStmt:
			if len(v.Lhs) == 1 && len(v.Rhs) == 1 &&
				types.ExprString(v.Lhs[0]) == "exitProfile" &&
				types.ExprString(v.Rhs[0]) == "newExitProfileFromEnv()" {
				created = true
			}
		case *ast.DeferStmt:
			switch types.ExprString(v.Call) {
			case "exitProfile.Stop()":
				stopIndex = i
			case "cancel()":
				if cancelIndex < 0 {
					cancelIndex = i
				}
			}
		}
	}
	if !created {
		t.Fatal("Run does not take the exit profile from the environment")
	}
	if stopIndex < 0 || cancelIndex < 0 || cancelIndex < stopIndex {
		t.Fatalf("the exit profile stop is deferred at %d and the cancel at %d: it must run last", stopIndex, cancelIndex)
	}

	brackets := 0
	ast.Inspect(run.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && types.ExprString(call) == "exitAfterDrain(exchange.Drain, cancel, exitProfile)" {
			brackets += 1
		}
		return true
	})
	if brackets != 1 {
		t.Fatalf("Run brackets the drain's cancel %d times", brackets)
	}
}
