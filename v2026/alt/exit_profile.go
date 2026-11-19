package alt

// The opt-in exit profile, an operator-requested diagnostic for the exit
// burst: within seconds of the cancel that follows the exchange drain, the old
// alt has had at least 700k goroutines alive at once and taken 8 to 30 GiB of
// host memory (10-24 to 10-26), and a cgroup cap can kill it before anything
// else records what those goroutines were.
//
// With URNETWORK_ALT_EXIT_PROFILE true, alt logs an aggregated goroutine
// profile, counted as pprof's goroutine profile at debug=1 counts it (the
// goroutines on each distinct stack), with the goroutine count and memory
// statistics: once after the drain and before the cancel, then at each sample
// offset after the cancel. Every line is bounded: at most TopStacks stacks,
// each of at most StackFrames frames named by function and file:line. A
// goroutine profile carries no request data and no identifiers.

import (
	"cmp"
	"fmt"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// Any value strconv.ParseBool reads as true turns the exit profile on. It is
// off when unset.
const exitProfileEnvName = "URNETWORK_ALT_EXIT_PROFILE"

// The program counters the runtime records per goroutine. A deeper stack is
// recorded from its innermost frame, so its root is cut.
const exitProfileStackDepth = len(runtime.StackRecord{}.Stack0)

// A burst can outgrow a record buffer between the goroutine count it is sized
// from and the runtime's own count, so a profile tries this many times, each
// with a quarter more room than the last count. pprof's goroutine profile
// retries with 10 records of room until one fits, which a burst can outrun
// for as long as it grows.
const exitProfileAttempts = 3

// The longest function and file names one frame logs.
const (
	exitProfileFunctionLimit = 256
	exitProfileFileLimit     = 64
)

type exitProfileSettings struct {
	// the distinct stacks one profile logs, most goroutines first
	TopStacks int
	// the frames one stack logs. A deeper stack keeps its innermost frames and
	// its outermost, which name the function its goroutines started in.
	StackFrames int
	// when the samples are taken, measured from the cancel
	SampleOffsets []time.Duration
	// the longest Stop waits for a profile in progress
	StopTimeout time.Duration
}

func defaultExitProfileSettings() *exitProfileSettings {
	return &exitProfileSettings{
		TopStacks:   40,
		StackFrames: 24,
		SampleOffsets: []time.Duration{
			100 * time.Millisecond,
			300 * time.Millisecond,
			1 * time.Second,
			3 * time.Second,
		},
		StopTimeout: 5 * time.Second,
	}
}

// The exit profile of one run. A nil *exitProfile is the disabled profile, and
// every method of it is a no-op. Safe for concurrent use.
type exitProfile struct {
	settings *exitProfileSettings
	logf     func(string, ...any)
	// closed by the first Stop
	stop chan struct{}

	stateLock sync.Mutex
	stopped   bool
	baseline  *exitProfileSnapshot
	// when the baseline was recorded, so the cancel's own duration is known
	baselineEndTime time.Time
	// closed when the sampler exits; nil until it starts
	samplerDone chan struct{}
}

func newExitProfileFromEnv() *exitProfile {
	return newExitProfile(os.Getenv(exitProfileEnvName), defaultExitProfileSettings(), glog.Infof)
}

// Returns nil, the disabled profile, unless value reads as true.
func newExitProfile(value string, settings *exitProfileSettings, logf func(string, ...any)) *exitProfile {
	if value == "" {
		return nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		logf("[alt][exitprofile]disabled: %s is not a boolean\n", exitProfileEnvName)
		return nil
	}
	if !enabled {
		return nil
	}
	sampleOffsets := []string{}
	for _, sampleOffset := range settings.SampleOffsets {
		sampleOffsets = append(sampleOffsets, sampleOffset.String())
	}
	logf(
		"[alt][exitprofile]enabled top_stacks=%d stack_frames=%d samples=%s\n",
		settings.TopStacks,
		settings.StackFrames,
		strings.Join(sampleOffsets, ","),
	)
	return &exitProfile{
		settings: settings,
		logf:     logf,
		stop:     make(chan struct{}),
	}
}

// Records the profile of what survived the drain. It only collects: the
// sampler writes the lines after the cancel, so no log write holds the cancel
// back.
func (self *exitProfile) TakeBaseline() {
	if self == nil || self.isStopped() {
		return
	}
	var baseline *exitProfileSnapshot
	// the drain runs this on its way to the cancel, which a fault here must
	// never stop
	server.HandleError(func() {
		baseline = takeExitProfileSnapshot(self.settings.TopStacks)
	})
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.baseline = baseline
	self.baselineEndTime = time.Now()
}

// Starts the sampler, which logs the baseline and then one profile at each
// sample offset after the cancel, until the schedule ends or Stop. Only the
// first call after the cancel starts it.
func (self *exitProfile) StartAfterCancel() {
	if self == nil {
		return
	}
	cancelTime := time.Now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.stopped || self.samplerDone != nil {
		return
	}
	baseline := self.baseline
	cancelDuration := cancelTime.Sub(self.baselineEndTime)
	samplerDone := make(chan struct{})
	self.samplerDone = samplerDone
	go server.HandleError(func() {
		defer close(samplerDone)
		self.sample(baseline, cancelDuration, cancelTime)
	})
}

// Ends the schedule: no profile starts after Stop, and the samples still to
// come are not waited for. A profile already in progress, its log writes
// included, holds Stop for at most StopTimeout.
func (self *exitProfile) Stop() {
	if self == nil {
		return
	}
	samplerDone, stopped := self.markStopped()
	if !stopped {
		return
	}
	close(self.stop)
	if samplerDone == nil {
		return
	}
	timer := time.NewTimer(self.settings.StopTimeout)
	defer timer.Stop()
	select {
	case <-samplerDone:
	case <-timer.C:
	}
}

func (self *exitProfile) isStopped() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.stopped
}

// Returns the sampler's completion, and whether this call is the one that
// stopped the profile.
func (self *exitProfile) markStopped() (chan struct{}, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.stopped {
		return nil, false
	}
	self.stopped = true
	return self.samplerDone, true
}

func (self *exitProfile) sample(baseline *exitProfileSnapshot, cancelDuration time.Duration, cancelTime time.Time) {
	if baseline != nil {
		self.logSnapshot(
			"baseline",
			fmt.Sprintf(
				"took=%s cancel_took=%s",
				formatExitProfileDuration(baseline.took),
				formatExitProfileDuration(cancelDuration),
			),
			baseline,
		)
	}
	for i, sampleOffset := range self.settings.SampleOffsets {
		if !self.waitUntil(cancelTime.Add(sampleOffset)) {
			return
		}
		snapshot := takeExitProfileSnapshot(self.settings.TopStacks)
		self.logSnapshot(
			fmt.Sprintf("sample %d/%d", i+1, len(self.settings.SampleOffsets)),
			fmt.Sprintf(
				"at=%s elapsed=%s took=%s",
				sampleOffset,
				formatExitProfileDuration(snapshot.time.Sub(cancelTime)),
				formatExitProfileDuration(snapshot.took),
			),
			snapshot,
		)
	}
}

// Waits until sampleTime, and reports false when Stop comes first.
func (self *exitProfile) waitUntil(sampleTime time.Time) bool {
	timer := time.NewTimer(time.Until(sampleTime))
	defer timer.Stop()
	select {
	case <-self.stop:
		return false
	case <-timer.C:
	}
	// both can be ready at once, and Stop wins
	select {
	case <-self.stop:
		return false
	default:
		return true
	}
}

// Logs one header line, one line per stack, and one line for the goroutines
// on the stacks not shown.
func (self *exitProfile) logSnapshot(label string, timing string, snapshot *exitProfileSnapshot) {
	if !snapshot.profiled {
		self.logf(
			"[alt][exitprofile]%s %s goroutines=%d stack_inuse_mib=%.1f heap_alloc_mib=%.1f sys_mib=%.1f mallocs=%d profile=outgrown\n",
			label,
			timing,
			snapshot.goroutines,
			exitProfileMib(snapshot.stackInuse),
			exitProfileMib(snapshot.heapAlloc),
			exitProfileMib(snapshot.sys),
			snapshot.mallocs,
		)
		return
	}
	self.logf(
		"[alt][exitprofile]%s %s goroutines=%d stack_inuse_mib=%.1f heap_alloc_mib=%.1f sys_mib=%.1f mallocs=%d profiled=%d stacks=%d\n",
		label,
		timing,
		snapshot.goroutines,
		exitProfileMib(snapshot.stackInuse),
		exitProfileMib(snapshot.heapAlloc),
		exitProfileMib(snapshot.sys),
		snapshot.mallocs,
		snapshot.profiledGoroutines,
		snapshot.distinctStacks,
	)
	shownGoroutines := 0
	for i, stack := range snapshot.stacks {
		frameCount, frames := exitProfileStackText(stack.pcs, self.settings.StackFrames)
		self.logf(
			"[alt][exitprofile]%s stack %d/%d goroutines=%d frames=%s: %s\n",
			label,
			i+1,
			len(snapshot.stacks),
			stack.count,
			frameCount,
			frames,
		)
		shownGoroutines += stack.count
	}
	if otherStacks := snapshot.distinctStacks - len(snapshot.stacks); 0 < otherStacks {
		self.logf(
			"[alt][exitprofile]%s other stacks=%d goroutines=%d\n",
			label,
			otherStacks,
			snapshot.profiledGoroutines-shownGoroutines,
		)
	}
}

// One profile: the counts and memory statistics, then the most common stacks
// as program counters, which are named only when logged.
type exitProfileSnapshot struct {
	time       time.Time
	took       time.Duration
	goroutines int
	stackInuse uint64
	heapAlloc  uint64
	sys        uint64
	mallocs    uint64
	// false when the goroutines outgrew every attempt to record them
	profiled           bool
	profiledGoroutines int
	distinctStacks     int
	// the most common distinct stacks, most goroutines first
	stacks []exitProfileStack
}

// The goroutines on one distinct stack.
type exitProfileStack struct {
	count int
	pcs   [exitProfileStackDepth]uintptr
}

func takeExitProfileSnapshot(topStacks int) *exitProfileSnapshot {
	startTime := time.Now()
	snapshot := &exitProfileSnapshot{
		time:       startTime,
		goroutines: runtime.NumGoroutine(),
	}
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	snapshot.stackInuse = memStats.StackInuse
	snapshot.heapAlloc = memStats.HeapAlloc
	snapshot.sys = memStats.Sys
	snapshot.mallocs = memStats.Mallocs
	if records, ok := recordGoroutineStacks(snapshot.goroutines, runtime.GoroutineProfile); ok {
		snapshot.profiled = true
		snapshot.profiledGoroutines = len(records)
		snapshot.stacks, snapshot.distinctStacks = topExitProfileStacks(records, topStacks)
	}
	snapshot.took = time.Since(startTime)
	return snapshot
}

// Records every goroutine's stack, sizing the buffer from the last count.
// Returns false when the goroutines outgrew every attempt.
func recordGoroutineStacks(
	goroutines int,
	goroutineProfile func([]runtime.StackRecord) (int, bool),
) ([]runtime.StackRecord, bool) {
	n := goroutines
	for range exitProfileAttempts {
		records := make([]runtime.StackRecord, n+n/4+1024)
		var ok bool
		n, ok = goroutineProfile(records)
		if ok {
			return records[:n], true
		}
	}
	return nil, false
}

// Counts the goroutines on each distinct stack, as pprof's goroutine profile
// at debug=1 does, and returns the topStacks most common, most goroutines
// first, with the number of distinct stacks.
func topExitProfileStacks(records []runtime.StackRecord, topStacks int) ([]exitProfileStack, int) {
	counts := map[[exitProfileStackDepth]uintptr]int{}
	for _, record := range records {
		counts[record.Stack0]++
	}
	stacks := make([]exitProfileStack, 0, len(counts))
	for pcs, count := range counts {
		stacks = append(stacks, exitProfileStack{
			count: count,
			pcs:   pcs,
		})
	}
	slices.SortFunc(stacks, func(a exitProfileStack, b exitProfileStack) int {
		if c := cmp.Compare(b.count, a.count); c != 0 {
			return c
		}
		return slices.Compare(a.pcs[:], b.pcs[:])
	})
	return slices.Clone(stacks[:min(topStacks, len(stacks))]), len(stacks)
}

// Names one stack's frames, innermost first, for one bounded line, and counts
// them. A stack deeper than maxFrames keeps its innermost frames and its
// outermost, with the number left out between them. The count ends in + when
// the runtime's record was full, which cuts the root.
func exitProfileStackText(pcs [exitProfileStackDepth]uintptr, maxFrames int) (string, string) {
	depth := 0
	for depth < len(pcs) && pcs[depth] != 0 {
		depth += 1
	}
	frames := exitProfileFrames(pcs[:depth])
	frameCount := strconv.Itoa(len(frames))
	if depth == len(pcs) {
		frameCount += "+"
	}
	texts := []string{}
	if len(frames) <= maxFrames {
		for _, frame := range frames {
			texts = append(texts, exitProfileFrameText(frame))
		}
	} else {
		outer := maxFrames / 3
		inner := maxFrames - outer
		for _, frame := range frames[:inner] {
			texts = append(texts, exitProfileFrameText(frame))
		}
		texts = append(texts, fmt.Sprintf("[%d frames elided]", len(frames)-maxFrames))
		for _, frame := range frames[len(frames)-outer:] {
			texts = append(texts, exitProfileFrameText(frame))
		}
	}
	return frameCount, strings.Join(texts, " < ")
}

// The frames of one stack, innermost first. As pprof's debug=1 text does, it
// leaves out runtime.goexit and the runtime frames a parked goroutine starts
// with, unless the stack has nothing else.
func exitProfileFrames(pcs []uintptr) []runtime.Frame {
	if len(pcs) == 0 {
		return nil
	}
	frames := []runtime.Frame{}
	callersFrames := runtime.CallersFrames(pcs)
	for {
		frame, more := callersFrames.Next()
		if frame.Function != "runtime.goexit" {
			frames = append(frames, frame)
		}
		if !more {
			break
		}
	}
	for i, frame := range frames {
		if !strings.HasPrefix(frame.Function, "runtime.") && !strings.HasPrefix(frame.Function, "internal/runtime/") {
			return frames[i:]
		}
	}
	return frames
}

// A frame by its function and its file's base name, each of bounded length.
// A frame the runtime cannot name is its program counter.
func exitProfileFrameText(frame runtime.Frame) string {
	if frame.Function == "" {
		return fmt.Sprintf("%#x", frame.PC)
	}
	file := "?"
	if frame.File != "" {
		file = path.Base(frame.File)
	}
	return fmt.Sprintf(
		"%s (%s:%d)",
		truncateExitProfileName(frame.Function, exitProfileFunctionLimit),
		truncateExitProfileName(file, exitProfileFileLimit),
		frame.Line,
	)
}

// Cuts name to at most limit bytes on a rune boundary, and marks the cut.
func truncateExitProfileName(name string, limit int) string {
	if len(name) <= limit {
		return name
	}
	cut := limit
	for 0 < cut && !utf8.RuneStart(name[cut]) {
		cut -= 1
	}
	return name[:cut] + "..."
}

func formatExitProfileDuration(duration time.Duration) string {
	return duration.Round(100 * time.Microsecond).String()
}

func exitProfileMib(bytes uint64) float64 {
	return float64(bytes) / (1 << 20)
}
