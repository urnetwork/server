package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// All owner names, times and output are synthetic. No SQL, SSH, network,
// settings load or process inspection is involved in these controls.
func admissionControlDiagnostic() *RunAdmissionDiagnostic {
	return &RunAdmissionDiagnostic{identity: settingsWitnessTestIdentity(), runId: strings.Repeat("a", 32), selection: strings.Repeat("b", 64), pending: make(chan *runAdmissionAttempt, runAdmissionAttemptLimit)}
}
func admissionControlContext(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, runSlotSignalContextKey{}, key)
}
func admissionControlSignal(t *testing.T, key string, run func(context.Context) (Alerts, error), budget time.Duration) Signal {
	t.Helper()
	for _, signal := range NewSignals() {
		if signal.Key() == key {
			return &admissionSyntheticSignal{signal, run, budget}
		}
	}
	t.Fatal("synthetic owner unavailable")
	return nil
}

type admissionSyntheticSignal struct {
	Signal
	run    func(context.Context) (Alerts, error)
	budget time.Duration
}

func (self *admissionSyntheticSignal) Run(ctx context.Context, _ SignalSettings) (Alerts, error) {
	return self.run(ctx)
}
func (self *admissionSyntheticSignal) runBudget() time.Duration { return self.budget }
func admissionControlPacket(t *testing.T, self *RunAdmissionDiagnostic) runAdmissionPacket {
	t.Helper()
	select {
	case attempt := <-self.pending:
		return self.packet(attempt)
	default:
		t.Fatal("sealed packet missing")
		return runAdmissionPacket{}
	}
}
func admissionControlHeld(t *testing.T, pool *runSlotPool, ctx context.Context) []func() {
	t.Helper()
	releases := []func(){}
	for range pool.limit {
		release, err := pool.acquire(admissionControlContext(ctx, "task-health"), false)
		if err != nil {
			t.Fatal("synthetic holder unavailable")
		}
		releases = append(releases, release)
	}
	return releases
}

func TestRunAdmissionDiagnosticDisabledConservesCaller(t *testing.T) {
	pool := newRunSlotPool(4)
	ran := 0
	signal := admissionControlSignal(t, "contract-degradation", func(context.Context) (Alerts, error) { ran++; return nil, nil }, time.Second)
	if _, err := runSignalInSlot(t.Context(), pool, signal, SignalSettings{}); err != nil || ran != 1 || pool.active != 0 || pool.diagnostic != nil {
		t.Fatal("disabled admission changed execution")
	}
}

func TestRunAdmissionDiagnosticAllFourHeldExpiryAndTypedJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		diagnostic := admissionControlDiagnostic()
		pool := newRunSlotPool(4)
		pool.diagnostic = diagnostic
		releases := admissionControlHeld(t, pool, t.Context())
		defer func() {
			for _, release := range releases {
				release()
			}
		}()
		signal := admissionControlSignal(t, "contract-degradation", func(context.Context) (Alerts, error) { t.Error("unadmitted Run called"); return nil, nil }, time.Second)
		_, err := runSignalInSlot(t.Context(), pool, signal, SignalSettings{})
		packet := admissionControlPacket(t, diagnostic)
		if !errors.Is(err, context.DeadlineExceeded) || pool.active != 4 || len(pool.waiting) != 0 || packet.Terminal != "expired_without_grant" || len(packet.Initial) != 4 || len(packet.Events) != 0 || !packet.EventsComplete {
			t.Fatal("queue expiration evidence changed")
		}
		before := visibilityAlert(syntheticSettings(nil), signal, &runSlotAdmissionError{cause: context.DeadlineExceeded})
		after := visibilityAlert(syntheticSettings(nil), signal, err)
		if before.Identity() != after.Identity() || before.Severity != after.Severity || before.Sustain != after.Sustain || !strings.Contains(after.Observed, "local_admission_attempt=1") {
			t.Fatal("attempt join changed public policy or identity")
		}
	})
}

func TestRunAdmissionDiagnosticTimelyReleaseDispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		diagnostic := admissionControlDiagnostic()
		pool := newRunSlotPool(4)
		pool.diagnostic = diagnostic
		releases := admissionControlHeld(t, pool, t.Context())
		done := make(chan error, 1)
		ran := 0
		signal := admissionControlSignal(t, "contract-degradation", func(context.Context) (Alerts, error) { ran++; return nil, nil }, time.Minute)
		go func() { _, err := runSignalInSlot(t.Context(), pool, signal, SignalSettings{}); done <- err }()
		synctest.Wait()
		releases[0]()
		if <-done != nil {
			t.Fatal("timely dispatch refused")
		}
		for _, release := range releases[1:] {
			release()
		}
		packet := admissionControlPacket(t, diagnostic)
		if ran != 1 || pool.active != 0 || packet.Terminal != "granted_for_dispatch" || !packet.EventsComplete || len(packet.Events) != 2 || packet.Events[0].Kind != "release" || packet.Events[1].Kind != "grant" {
			t.Fatal("admission window did not end at grant")
		}
	})
}

// This context delivers cancellation on the caller's third Err check: initial
// acquire, dispatch eligibility, then actual pre-Run guard. Done closes with Err.
type admissionPreRunCancelContext struct {
	context.Context
	done     chan struct{}
	checks   int
	deadline time.Time
}

func (self *admissionPreRunCancelContext) Deadline() (time.Time, bool) { return self.deadline, true }
func (self *admissionPreRunCancelContext) Done() <-chan struct{}       { return self.done }
func (self *admissionPreRunCancelContext) Err() error {
	self.checks++
	if self.checks == 3 {
		close(self.done)
	}
	if self.checks >= 3 {
		return context.Canceled
	}
	return nil
}
func TestRunAdmissionDiagnosticTerminalFollowsCallerPreRunCheck(t *testing.T) {
	for _, withWitness := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "witness"}[withWitness], func(t *testing.T) {
			diagnostic := admissionControlDiagnostic()
			pool := newRunSlotPool(4)
			pool.diagnostic = diagnostic
			ran := false
			ctx := &admissionPreRunCancelContext{Context: context.Background(), done: make(chan struct{}), deadline: time.Now().Add(time.Minute)}
			signal := admissionControlSignal(t, "contract-degradation", func(context.Context) (Alerts, error) { ran = true; return nil, nil }, 0)
			var err error
			if withWitness {
				_, err = runSignalInSlotWithWitness(ctx, pool, signal, SignalSettings{}, invocationControlWitness(t, signal, io.Discard, io.Discard), 15*time.Minute)
			} else {
				_, err = runSignalInSlot(ctx, pool, signal, SignalSettings{})
			}
			packet := admissionControlPacket(t, diagnostic)
			if !errors.Is(err, context.Canceled) || ran || pool.active != 0 || packet.Terminal != "granted_then_context_ended_before_Run" || len(packet.Events) != 1 || packet.Events[0].Kind != "grant" {
				t.Fatal("grant incorrectly proved Run")
			}
		})
	}
}

// Cancel at select's Done operand, after dispatch. Both select outcomes are
// legal; either must retain its grant/caller terminal and return the own permit.
type admissionGrantCancelContext struct {
	context.Context
	done     chan struct{}
	deadline time.Time
	canceled bool
}

func (self *admissionGrantCancelContext) Deadline() (time.Time, bool) { return self.deadline, true }
func (self *admissionGrantCancelContext) Done() <-chan struct{} {
	if !self.canceled {
		self.canceled = true
		close(self.done)
	}
	return self.done
}
func (self *admissionGrantCancelContext) Err() error {
	if self.canceled {
		return context.Canceled
	}
	return nil
}
func TestRunAdmissionDiagnosticCancelAfterGrantKeepsDistinctTerminal(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		attempt := &runAdmissionAttempt{granted: true}
		if runAdmissionFailure(attempt, err) != runAdmissionCancelAfterGrant {
			t.Fatal("cancel-after-grant collapsed into never granted")
		}
	}
	if runAdmissionFailure(nil, context.DeadlineExceeded) != runAdmissionExpired || runAdmissionFailure(nil, context.Canceled) != runAdmissionCanceled {
		t.Fatal("queue terminal class changed")
	}
	diagnostic := admissionControlDiagnostic()
	pool := newRunSlotPool(4)
	pool.diagnostic = diagnostic
	ran := false
	ctx := &admissionGrantCancelContext{Context: context.Background(), done: make(chan struct{}), deadline: time.Now().Add(time.Minute)}
	signal := admissionControlSignal(t, "contract-degradation", func(context.Context) (Alerts, error) { ran = true; return nil, nil }, 0)
	_, err := runSignalInSlot(ctx, pool, signal, SignalSettings{})
	packet := admissionControlPacket(t, diagnostic)
	if !errors.Is(err, context.Canceled) || ran || pool.active != 0 || (packet.Terminal != "cancel_after_grant" && packet.Terminal != "granted_then_context_ended_before_Run") {
		t.Fatal("grant/cancel race changed capacity or Run boundary")
	}
}

func TestRunAdmissionDiagnosticSameKeyGrantsRemainDistinct(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		diagnostic := admissionControlDiagnostic()
		pool := newRunSlotPool(4)
		pool.diagnostic = diagnostic
		releases := admissionControlHeld(t, pool, t.Context())
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		attempt := diagnostic.beginWithLock(ctx, "contract-degradation", true, pool.holders, pool.active)
		releases[0]()
		release, err := pool.acquire(admissionControlContext(t.Context(), "task-health"), false)
		if err != nil {
			t.Fatal("replacement holder refused")
		}
		release()
		for _, held := range releases[1:] {
			held()
		}
		attempt.closeWithLock(false)
		diagnostic.finish(attempt, runAdmissionCanceled)
		packet := admissionControlPacket(t, diagnostic)
		if packet.Initial[0].Grant == packet.Events[1].Grant || packet.Initial[0].Key != packet.Events[1].Key || packet.Events[1].Kind != "grant" {
			t.Fatal("same-key ownership merged")
		}
	})
}

func TestRunAdmissionDiagnosticFairnessConserved(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newRunSlotPool(1)
		pool.diagnostic = admissionControlDiagnostic()
		release, err := pool.acquire(t.Context(), false)
		if err != nil {
			t.Fatal("initial admission refused")
		}
		admitted := make(chan string, 5)
		finish := make(chan struct{})
		for _, item := range []struct {
			name     string
			priority bool
		}{{"ordinary1", false}, {"ordinary2", false}, {"bounded1", true}, {"bounded2", true}, {"bounded3", true}} {
			go func() {
				release, err := pool.acquire(t.Context(), item.priority)
				if err != nil {
					t.Error("queued admission refused")
					return
				}
				admitted <- item.name
				<-finish
				release()
			}()
			synctest.Wait()
		}
		release()
		for _, want := range []string{"bounded1", "bounded2", "ordinary1", "bounded3", "ordinary2"} {
			if <-admitted != want {
				t.Fatal("diagnostic changed fairness")
			}
			finish <- struct{}{}
		}
		synctest.Wait()
		if pool.active != 0 || len(pool.waiting) != 0 {
			t.Fatal("diagnostic retained grants")
		}
	})
}

func TestRunAdmissionDiagnosticOnlyFiveFirstAttempts(t *testing.T) {
	diagnostic := admissionControlDiagnostic()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for i, key := range []string{"contract-degradation", "pg-freeze-age", "transaction-horizon", "contract-partition-retention-blocked", "partition-retention-blocked"} {
		attempt := diagnostic.beginWithLock(ctx, key, true, [4]*runSlotWaiter{}, 0)
		if attempt == nil || attempt.slot != i+1 || diagnostic.beginWithLock(ctx, key, true, [4]*runSlotWaiter{}, 0) != nil {
			t.Fatal("finite enrollment changed")
		}
		attempt.closeWithLock(true)
		diagnostic.finish(attempt, runAdmissionForDispatch)
	}
	if len(diagnostic.pending) != 5 || diagnostic.beginWithLock(ctx, "task-health", false, [4]*runSlotWaiter{}, 0) != nil {
		t.Fatal("extra attempt admitted")
	}
}

func TestRunAdmissionDiagnosticOverflowAndClockRemainUnknown(t *testing.T) {
	for _, kind := range []string{"overflow", "clock", "occupancy"} {
		t.Run(kind, func(t *testing.T) {
			diagnostic := admissionControlDiagnostic()
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			attempt := diagnostic.beginWithLock(ctx, "contract-degradation", true, [4]*runSlotWaiter{}, 0)
			holder := &runSlotWaiter{signalKey: "task-health", grantOrdinal: 1}
			for range runAdmissionEventLimit + 1 {
				diagnostic.eventWithLock(runAdmissionGrant, holder, 1)
			}
			attempt.closeWithLock(false)
			attempt.terminal = runAdmissionCanceled
			if kind != "overflow" {
				attempt.overflow = false
			}
			if kind == "clock" {
				attempt.windowEnd = attempt.enqueued.Add(-time.Second)
			}
			if kind == "occupancy" {
				attempt.initialActive = 4
			}
			if diagnostic.packet(attempt).EventsComplete {
				t.Fatal("incomplete events became complete")
			}
		})
	}
}

func TestRunAdmissionDiagnosticPrivacyProjection(t *testing.T) {
	diagnostic := admissionControlDiagnostic()
	pool := newRunSlotPool(4)
	pool.diagnostic = diagnostic
	canary := "synthetic-private-endpoint-token-not-public"
	release, err := pool.acquire(admissionControlContext(t.Context(), canary), false)
	if err != nil {
		t.Fatal("synthetic holder refused")
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	attempt := diagnostic.beginWithLock(ctx, "contract-degradation", true, pool.holders, pool.active)
	attempt.closeWithLock(false)
	attempt.terminal = runAdmissionCanceled
	raw, err := json.Marshal(diagnostic.packet(attempt))
	if err != nil || strings.Contains(string(raw), canary) || diagnostic.packet(attempt).EventsComplete {
		t.Fatal("private label projected or unknown became complete")
	}
	observed := observationFailureDetails(&runSlotAdmissionError{cause: errors.New(canary), admissionAttempt: 1}, "observation-timeout")
	if strings.Contains(observed, canary) || !strings.Contains(observed, "local_admission_attempt=1") {
		t.Fatal("raw error projected")
	}
}

func TestRunAdmissionDiagnosticBlockedEmitterDoesNotHoldPermitOrShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		diagnostic := admissionControlDiagnostic()
		blocked := make(chan struct{})
		entered := make(chan struct{})
		joined := make(chan struct{})
		diagnostic.emit = func(*runAdmissionAttempt) { close(entered); <-blocked }
		go func() { diagnostic.drain(); close(joined) }()
		pool := newRunSlotPool(4)
		pool.diagnostic = diagnostic
		ran := 0
		signal := admissionControlSignal(t, "contract-degradation", func(context.Context) (Alerts, error) { ran++; return nil, nil }, time.Second)
		if _, err := runSignalInSlot(t.Context(), pool, signal, SignalSettings{}); err != nil {
			t.Fatal("blocked emitter blocked Run")
		}
		<-entered
		if pool.active != 0 || ran != 1 {
			t.Fatal("emitter retained permit")
		}
		diagnostic.Close()
		select {
		case <-joined:
			t.Fatal("blocked sink unexpectedly joined")
		default:
		}
		release, err := pool.acquire(t.Context(), false)
		if err != nil {
			t.Fatal("sink changed admission")
		}
		release()
		close(blocked)
		<-joined
	})
}

type admissionFailureOutput struct {
	n                           int
	writeErr, syncErr, closeErr error
	closed                      bool
}

func (self *admissionFailureOutput) Write(p []byte) (int, error) {
	if self.n < 0 {
		return len(p), self.writeErr
	}
	return self.n, self.writeErr
}
func (self *admissionFailureOutput) Sync() error  { return self.syncErr }
func (self *admissionFailureOutput) Close() error { self.closed = true; return self.closeErr }
func TestRunAdmissionDiagnosticCommitRequiresFinishedWrites(t *testing.T) {
	for _, failure := range []string{"short", "write", "sync", "close"} {
		t.Run(failure, func(t *testing.T) {
			output := &admissionFailureOutput{n: -1}
			switch failure {
			case "short":
				output.n = 0
			case "write":
				output.writeErr = io.ErrClosedPipe
			case "sync":
				output.syncErr = io.ErrClosedPipe
			case "close":
				output.closeErr = io.ErrClosedPipe
			}
			if finishRunAdmissionFile(output, []byte("synthetic")) || !output.closed {
				t.Fatal("failed sink accepted or not closed")
			}
		})
	}
	for _, failAt := range []int{1, 2, 3} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			calls := 0
			committed := false
			commitRunAdmissionPacket("attempt-1", []byte("data"), []byte("receipt"), func(string, []byte) bool { calls++; return calls != failAt }, func(string, string) error {
				calls++
				if failAt == 3 {
					return io.ErrClosedPipe
				}
				committed = true
				return nil
			})
			if committed {
				t.Fatal("sink failure created accepted commit")
			}
		})
	}
}

func admissionControlArtifact(t *testing.T) ([]byte, []byte) {
	t.Helper()
	diagnostic := admissionControlDiagnostic()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	attempt := diagnostic.beginWithLock(ctx, "contract-degradation", true, [4]*runSlotWaiter{}, 0)
	diagnostic.eventWithLock(runAdmissionGrant, &runSlotWaiter{signalKey: "contract-degradation", grantOrdinal: 1}, 1)
	attempt.closeWithLock(true)
	attempt.terminal = runAdmissionForDispatch
	data, err := json.Marshal(diagnostic.packet(attempt))
	if err != nil {
		t.Fatal("synthetic serialization failed")
	}
	digest := sha256.Sum256(data)
	receipt, err := json.Marshal(runAdmissionCommit{1, len(data), hex.EncodeToString(digest[:]), diagnostic.runId, 1})
	if err != nil {
		t.Fatal("synthetic commit failed")
	}
	return data, receipt
}
func TestRunAdmissionDiagnosticStrictCommittedArtifact(t *testing.T) {
	data, receipt := admissionControlArtifact(t)
	if _, err := readRunAdmissionArtifact(data, receipt); err != nil {
		t.Fatal("valid synthetic artifact refused")
	}
	for _, item := range []struct {
		name          string
		data, receipt []byte
	}{{"missing", data, nil}, {"partial-data", data[:len(data)-1], receipt}, {"partial-receipt", data, receipt[:len(receipt)-1]}, {"extra-frame", append(append([]byte{}, data...), []byte("{}")...), receipt}, {"wrong-data", append(append([]byte{}, data...), byte(' ')), receipt}, {"extra-field", data, []byte(`{"schema":1,"unexpected":true}`)}} {
		t.Run(item.name, func(t *testing.T) {
			if _, err := readRunAdmissionArtifact(item.data, item.receipt); err == nil {
				t.Fatal("uncommitted or malformed artifact accepted")
			}
		})
	}
}

func TestRunAdmissionDiagnosticFixedMemoryAndPacketBounds(t *testing.T) {
	if runAdmissionAttemptLimit != 5 || runAdmissionEventLimit != 512 || runAdmissionPacketLimit != 128*1024 {
		t.Fatal("finite diagnostic bounds changed")
	}
	if reflect.TypeOf(RunAdmissionDiagnostic{}).Size() > 256*1024 {
		t.Fatal("fixed observation memory exceeds bound")
	}
	diagnostic := admissionControlDiagnostic()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	attempt := diagnostic.beginWithLock(ctx, "contract-degradation", true, [4]*runSlotWaiter{}, 0)
	for range runAdmissionEventLimit {
		diagnostic.eventWithLock(runAdmissionGrant, &runSlotWaiter{signalKey: "contract-partition-retention-blocked", grantOrdinal: 1}, 4)
	}
	attempt.closeWithLock(true)
	attempt.terminal = runAdmissionForDispatch
	raw, err := json.Marshal(diagnostic.packet(attempt))
	if err != nil || len(raw) > runAdmissionPacketLimit {
		t.Fatal("bounded event packet exceeds cap")
	}
}

func TestRunAdmissionDiagnosticPrivateExclusiveCommit(t *testing.T) {
	parent := t.TempDir()
	if os.Chmod(parent, 0700) != nil {
		t.Fatal("private synthetic parent unavailable")
	}
	witness := &RunInvocationWitness{identity: settingsWitnessTestIdentity(), runId: strings.Repeat("a", 32), selectedKeysSha256: strings.Repeat("b", 64)}
	diagnostic, err := NewRunAdmissionDiagnostic(filepath.Join(parent, "diagnostic"), witness)
	if err != nil {
		t.Fatal("private synthetic constructor refused")
	}
	diagnostic.Close()
	if _, err := NewRunAdmissionDiagnostic(filepath.Join(parent, "diagnostic"), witness); err == nil {
		t.Fatal("existing directory reused")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal("synthetic root unavailable")
	}
	defer root.Close()
	data, receipt := admissionControlArtifact(t)
	commitRunAdmissionPacket("attempt-1", data, receipt, func(name string, value []byte) bool { return writeRunAdmissionFile(root, name, value) }, root.Link)
	stored, err := os.ReadFile(filepath.Join(parent, "attempt-1.commit.json"))
	if err != nil {
		t.Fatal("committed receipt missing")
	}
	if _, err := readRunAdmissionArtifact(data, stored); err != nil {
		t.Fatal("committed receipt refused")
	}
	info, err := os.Lstat(filepath.Join(parent, "attempt-1.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private file mode changed")
	}
	if writeRunAdmissionFile(root, "attempt-1.json", []byte("replacement")) {
		t.Fatal("accepted artifact overwritten")
	}
	if !writeRunAdmissionFile(root, "attempt-2.commit.json", []byte("synthetic-existing")) {
		t.Fatal("synthetic existing receipt unavailable")
	}
	commitRunAdmissionPacket("attempt-2", data, receipt, func(name string, value []byte) bool { return writeRunAdmissionFile(root, name, value) }, root.Link)
	existing, err := os.ReadFile(filepath.Join(parent, "attempt-2.commit.json"))
	if err != nil || string(existing) != "synthetic-existing" {
		t.Fatal("existing committed name overwritten")
	}
}

// The actual startup fencing helper is exercised against only fresh synthetic
// local paths. A replaced parent cannot redirect an emitter into the target.
func TestRunAdmissionDiagnosticDirectoryReplacementRefused(t *testing.T) {
	for _, scenario := range []string{"parent-symlink", "child-symlink", "child-replacement", "child-mode"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			parent := filepath.Join(base, "private")
			victim := filepath.Join(base, "synthetic-private-target")
			directory := filepath.Join(parent, "diagnostic")
			if os.Mkdir(parent, 0700) != nil || os.Mkdir(victim, 0700) != nil {
				t.Fatal("synthetic private directories unavailable")
			}
			root, err := newRunAdmissionDirectory(directory, func() {
				switch scenario {
				case "parent-symlink":
					if os.Rename(parent, parent+"-moved") != nil || os.Symlink(victim, parent) != nil {
						t.Fatal("synthetic parent replacement unavailable")
					}
				case "child-symlink":
					if os.Rename(directory, directory+"-moved") != nil || os.Symlink(victim, directory) != nil {
						t.Fatal("synthetic child replacement unavailable")
					}
				case "child-replacement":
					if os.Rename(directory, directory+"-moved") != nil || os.Mkdir(directory, 0700) != nil {
						t.Fatal("synthetic child identity replacement unavailable")
					}
				case "child-mode":
					if os.Chmod(directory, 0777) != nil {
						t.Fatal("synthetic mode replacement unavailable")
					}
				}
			})
			if root != nil {
				root.Close()
			}
			if err == nil || err.Error() != "admission diagnostic directory unavailable" {
				t.Fatal("replaced directory authority accepted or private path projected")
			}
			entries, readErr := os.ReadDir(victim)
			if readErr != nil || len(entries) != 0 {
				t.Fatal("replaced parent redirected output")
			}
		})
	}
	stat := unix.Stat_t{Mode: unix.S_IFDIR | 0700, Uid: uint32(os.Geteuid()), Dev: 1, Ino: 2}
	if !runAdmissionPrivateDirectory(&stat) {
		t.Fatal("valid synthetic ownership refused")
	}
	stat.Uid++
	if runAdmissionPrivateDirectory(&stat) {
		t.Fatal("foreign owner accepted")
	}
}

func TestRunAdmissionDiagnosticMalformedCanonicalRehashRefused(t *testing.T) {
	data, _ := admissionControlArtifact(t)
	var valid runAdmissionPacket
	if json.Unmarshal(data, &valid) != nil || !valid.EventsComplete {
		t.Fatal("valid semantic fixture unavailable")
	}
	cases := []struct {
		name   string
		mutate func(*runAdmissionPacket)
	}{
		{"terminal", func(p *runAdmissionPacket) { p.Terminal = "invented-terminal" }},
		{"kind", func(p *runAdmissionPacket) { p.Events[0].Kind = "invented-event" }},
		{"nil-window", func(p *runAdmissionPacket) { p.WindowEnd = nil }},
		{"zero-enqueue", func(p *runAdmissionPacket) { p.Enqueued = time.Time{} }},
		{"zero-deadline", func(p *runAdmissionPacket) { p.Deadline = time.Time{} }},
		{"deadline-before-enqueue", func(p *runAdmissionPacket) { p.Deadline = p.Enqueued.Add(-time.Second) }},
		{"negative-window", func(p *runAdmissionPacket) { v := int64(-1); p.WindowEnd = &v }},
		{"nil-offset", func(p *runAdmissionPacket) { p.Events[0].Offset = nil }},
		{"negative-offset", func(p *runAdmissionPacket) { v := int64(-1); p.Events[0].Offset = &v }},
		{"after-window", func(p *runAdmissionPacket) { v := *p.WindowEnd + 1; p.Events[0].Offset = &v }},
		{"regressing-offset", func(p *runAdmissionPacket) {
			first := p.Events[0]
			first.Key = "task-health"
			later := first
			later.Key = p.Key
			later.Grant = 2
			later.Active = 2
			later.Sequence = 2
			v := int64(0)
			later.Offset = &v
			w := int64(1)
			first.Offset = &w
			p.WindowEnd = &w
			p.Events = []runAdmissionPublicEvent{first, later}
		}},
		{"occupancy", func(p *runAdmissionPacket) { p.InitialActive = 1 }},
		{"ordinal", func(p *runAdmissionPacket) { p.Events[0].Grant = 0 }},
		{"key", func(p *runAdmissionPacket) { p.Events[0].Key = "synthetic-private-value" }},
		{"active", func(p *runAdmissionPacket) { p.Events[0].Active = 4 }},
		{"sequence", func(p *runAdmissionPacket) { p.Events[0].Sequence = 2 }},
		{"overflow", func(p *runAdmissionPacket) { p.EventOverflow = true }},
		{"nil-remaining", func(p *runAdmissionPacket) { p.Remaining = nil }},
		{"negative-remaining", func(p *runAdmissionPacket) { v := int64(-1); p.Remaining = &v }},
		{"unknown-grant", func(p *runAdmissionPacket) { p.Events[0].Key = "unknown" }},
		{"claim-no-grant", func(p *runAdmissionPacket) { p.Terminal = "canceled_without_grant" }},
		{"false-reason", func(p *runAdmissionPacket) { p.EventsComplete = false; p.IncompleteReason = "clock_unavailable" }},
		{"reason-with-complete", func(p *runAdmissionPacket) { p.IncompleteReason = "event_overflow" }},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			var packet runAdmissionPacket
			if json.Unmarshal(data, &packet) != nil {
				t.Fatal("synthetic decode failed")
			}
			item.mutate(&packet)
			malformed, err := json.Marshal(packet)
			if err != nil {
				t.Fatal("synthetic canonical encode failed")
			}
			digest := sha256.Sum256(malformed)
			receipt, err := json.Marshal(runAdmissionCommit{1, len(malformed), hex.EncodeToString(digest[:]), packet.RunId, packet.Slot})
			if err != nil {
				t.Fatal("synthetic canonical receipt failed")
			}
			if _, err := readRunAdmissionArtifact(malformed, receipt); err == nil {
				t.Fatal("impossible canonical rehashed packet accepted")
			}
		})
	}
	// A declared unknown holder is retained only as UNKNOWN, never complete.
	valid.EventsComplete = false
	valid.IncompleteReason = "holder_unavailable"
	valid.InitialActive = 1
	valid.Initial = []runAdmissionPublicHolder{{Key: "unknown", Grant: 7, Age: nil}}
	incomplete, err := json.Marshal(valid)
	if err != nil {
		t.Fatal("synthetic unknown encode failed")
	}
	digest := sha256.Sum256(incomplete)
	receipt, err := json.Marshal(runAdmissionCommit{1, len(incomplete), hex.EncodeToString(digest[:]), valid.RunId, valid.Slot})
	if err != nil {
		t.Fatal("synthetic unknown receipt failed")
	}
	packet, err := readRunAdmissionArtifact(incomplete, receipt)
	if err != nil || packet.EventsComplete {
		t.Fatal("qualified incomplete artifact became complete or was discarded")
	}
}

// The retention targets must own finite first-attempt packets without enrolling
// sibling registry probes or accepting another target's slot.
func TestRunAdmissionDiagnosticRetentionEnrollmentAndCommittedSlots(t *testing.T) {
	for i, key := range []string{"contract-partition-retention-blocked", "partition-retention-blocked"} {
		t.Run(key, func(t *testing.T) {
			diagnostic := admissionControlDiagnostic()
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			attempt := diagnostic.beginWithLock(ctx, key, true, [4]*runSlotWaiter{}, 0)
			if attempt == nil || attempt.slot != i+4 {
				t.Fatal("retention first attempt unavailable")
			}
			diagnostic.eventWithLock(runAdmissionGrant, &runSlotWaiter{signalKey: key, grantOrdinal: 1}, 1)
			attempt.closeWithLock(true)
			attempt.terminal = runAdmissionForDispatch
			packet := diagnostic.packet(attempt)
			data, err := json.Marshal(packet)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			receipt, err := json.Marshal(runAdmissionCommit{1, len(data), hex.EncodeToString(digest[:]), diagnostic.runId, i + 4})
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := readRunAdmissionArtifact(data, receipt)
			if err != nil || decoded.Key != key || decoded.Slot != i+4 || !decoded.EventsComplete {
				t.Fatal("retention committed artifact unavailable")
			}
			packet.Slot = 1
			data, err = json.Marshal(packet)
			if err != nil {
				t.Fatal(err)
			}
			digest = sha256.Sum256(data)
			receipt, err = json.Marshal(runAdmissionCommit{1, len(data), hex.EncodeToString(digest[:]), diagnostic.runId, 1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readRunAdmissionArtifact(data, receipt); err == nil {
				t.Fatal("retention target accepted another target slot")
			}
		})
	}
	for _, key := range []string{"", "task-health", "contract-partition-runway", "partition-runway", "contract-partition-retention-blocked-extra"} {
		diagnostic := admissionControlDiagnostic()
		if runAdmissionSlot(key) != 0 || diagnostic.beginWithLock(t.Context(), key, true, [4]*runSlotWaiter{}, 0) != nil {
			t.Fatal("unsupported target enrolled")
		}
	}
}
