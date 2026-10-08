package monitor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func newSyntheticSshAdmissionStore(t *testing.T) (*sshAdmissionStore, sshAdmissionState) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	state := syntheticSshAdmissionState()
	store, err := newSshAdmissionStore(directory, state.BootId)
	if err != nil {
		t.Fatal(err)
	}
	store.status = func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus { return sshAdmissionOwnerLive }
	if err := store.update(context.Background(), func(current *sshAdmissionState, _ int64) error { *current = state; return nil }); err != nil {
		t.Fatal(err)
	}
	return store, state
}

// This polls a causal persisted state, never an elapsed-time success condition.
func awaitSyntheticSshAdmission(t *testing.T, store *sshAdmissionStore, predicate func(sshAdmissionState) bool) sshAdmissionState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var result sshAdmissionState
		err := store.update(ctx, func(state *sshAdmissionState, _ int64) error {
			result = *state
			result.Requests = append([]sshAdmissionRequest(nil), state.Requests...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if predicate(result) {
			return result
		}
		if err := waitSshAdmission(ctx); err != nil {
			t.Fatal("causal queue condition was not reached")
		}
	}
}

func TestSharedSshAdmissionActualTransportHonorsGlobalAndCancellation(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	owner := state.Watcher
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var releases []func() error
	defer func() {
		for _, release := range releases {
			if err := release(); err != nil {
				t.Error(err)
			}
		}
	}()
	for _, host := range []string{"a.example", "b.example", "c.example", "d.example"} {
		release, err := store.acquire(ctx, owner, owner.Identity, sshAdmissionWatch, []string{host})
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: &sharedSshAdmission{store: store, owner: owner}})
	invoked := make(chan struct{}, 1)
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		invoked <- struct{}{}
		return "", "", nil
	}
	callCtx, cancelCall := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		_, err := runner.ssh(callCtx, &host{name: "e.example", overlayIp: "192.0.2.10"}, "true", "")
		result <- err
	}()
	defer func() {
		cancelCall()
		select {
		case <-result:
		case <-time.After(5 * time.Second):
			t.Error("transport did not join")
		}
	}()
	// If the shared transport hook is removed (the original failure), the fifth
	// independent destination executes immediately instead of queueing.
	queued := make(chan struct{})
	go func() {
		defer close(queued)
		for {
			seen := false
			if err := store.update(callCtx, func(state *sshAdmissionState, _ int64) error { seen = len(state.Requests) == 5; return nil }); err != nil {
				return
			}
			if seen {
				return
			}
			if waitSshAdmission(callCtx) != nil {
				return
			}
		}
	}()
	defer func() { cancelCall(); <-queued }()
	select {
	case <-invoked:
		t.Fatal("watcher fanout bypassed the four-command global limit")
	case <-queued:
		if callCtx.Err() != nil {
			t.Fatal("transport never entered shared admission")
		}
	case <-ctx.Done():
		t.Fatal("transport did not reach the causal queue barrier")
	}
	cancelCall()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("queued cancellation lost ownership")
		}
		result <- err
	case <-ctx.Done():
		t.Fatal("queued transport cancellation did not join")
	}
	select {
	case <-invoked:
		t.Fatal("canceled queued command contacted target")
	default:
	}
	final := awaitSyntheticSshAdmission(t, store, func(state sshAdmissionState) bool { return len(state.Requests) == 4 })
	for _, request := range final.Requests {
		if !request.Active {
			t.Fatal("cancellation removed another active owner")
		}
	}
}

func TestSharedSshAdmissionDiagnosticAtomicLeaseAndAuthority(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	diagnostic := syntheticSshAdmissionOwner(202)
	release, err := store.acquire(ctx, diagnostic, state.Watcher.Identity, sshAdmissionDiagnostic, []string{"a.example", "b.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	current := awaitSyntheticSshAdmission(t, store, func(state sshAdmissionState) bool { return len(state.Requests) == 1 })
	if !current.Requests[0].Active || len(current.Requests[0].Hosts) != 2 {
		t.Fatal("hop lease was split")
	}
	for _, testCase := range []struct {
		owner    sshAdmissionOwner
		expected SshAdmissionWatcher
		hosts    []string
	}{
		{syntheticSshAdmissionOwner(303), state.Watcher.Identity, []string{"c.example"}},
		{diagnostic, syntheticSshAdmissionOwner(404).Identity, []string{"c.example"}},
		{diagnostic, state.Watcher.Identity, []string{"outside.example"}},
	} {
		if unexpectedRelease, err := store.acquire(ctx, testCase.owner, testCase.expected, sshAdmissionDiagnostic, testCase.hosts); err == nil {
			unexpectedRelease()
			t.Fatal("invalid or second diagnostic acquired capacity")
		}
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.acquire(ctx, state.Watcher, state.Watcher.Identity, sshAdmissionDiagnostic, []string{"a.example"}); err == nil {
		t.Fatal("diagnostic reused watcher service")
	}
}

func TestSharedSshAdmissionReleaseRequiresJoinedMessage(t *testing.T) {
	for _, input := range []string{"", "invalid\n", "releas"} {
		released := false
		err := awaitSshAdmissionRelease(context.Background(), strings.NewReader(input), func() error { released = true; return nil })
		if err == nil || released {
			t.Fatal("EOF or malformed message freed an active reservation")
		}
	}
	released := 0
	if err := awaitSshAdmissionRelease(context.Background(), strings.NewReader("release\n"), func() error { released++; return nil }); err != nil || released != 1 {
		t.Fatal("joined release was not accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	cancel()
	if err := awaitSshAdmissionRelease(ctx, reader, func() error { t.Fatal("cancellation released live work"); return nil }); err == nil {
		t.Fatal("cancellation accepted")
	}
}

func TestSharedSshAdmissionCorruptionAndNoopPublication(t *testing.T) {
	store, _ := newSyntheticSshAdmissionStore(t)
	path := filepath.Join(store.directory, "queue.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.update(context.Background(), func(*sshAdmissionState, int64) error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("queue observation rewrote unchanged state")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, malformed := range [][]byte{
		[]byte(`{"version":1,"version":1}`),
		append(append([]byte(nil), raw...), []byte(` {}`)...),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"extra":true,"version":1`), 1),
		[]byte(`{`),
	} {
		if err := os.WriteFile(path, malformed, 0600); err != nil {
			t.Fatal(err)
		}
		if err := store.update(context.Background(), func(*sshAdmissionState, int64) error { t.Fatal("malformed state reached mutation"); return nil }); err == nil {
			t.Fatal("corrupt state became empty capacity")
		}
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.update(context.Background(), func(*sshAdmissionState, int64) error { return nil }); err == nil {
		t.Fatal("public coordination state accepted")
	}
}

// Six separate test processes contend for four real file-backed reservations.
// Each successful child holds its claim until the parent permits joined release.
func TestSharedSshAdmissionCrossProcessGlobalLimit(t *testing.T) {
	store, _ := newSyntheticSshAdmissionStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	type child struct {
		command *exec.Cmd
		input   io.WriteCloser
		done    chan error
	}
	children := []child{}
	defer func() {
		cancel()
		for _, child := range children {
			child.input.Close()
		}
		for _, child := range children {
			select {
			case <-child.done:
			case <-time.After(5 * time.Second):
				t.Error("local admission child did not join")
			}
		}
	}()
	for i := range 6 {
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSharedSshAdmissionProcessHelper$")
		command.Env = append(os.Environ(), "SYNTHETIC_SSH_ADMISSION_DIRECTORY="+store.directory, "SYNTHETIC_SSH_ADMISSION_HOST="+[]string{"a.example", "b.example", "c.example", "d.example", "a.example", "b.example"}[i])
		input, err := command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Start(); err != nil {
			input.Close()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		children = append(children, child{command: command, input: input, done: done})
	}
	current := awaitSyntheticSshAdmission(t, store, func(state sshAdmissionState) bool { return len(state.Requests) == 6 })
	active := 0
	for _, request := range current.Requests {
		if request.Active {
			active++
		}
	}
	if active != 4 {
		t.Fatal("cross-process queue did not conserve the four-slot cap")
	}
	for _, child := range children {
		if _, err := io.WriteString(child.input, "release\n"); err != nil {
			t.Fatal(err)
		}
	}
	for _, child := range children {
		select {
		case err := <-child.done:
			child.done <- err
			if err != nil {
				t.Fatal("local admission process failed")
			}
		case <-ctx.Done():
			t.Fatal("cross-process claims failed to finish")
		}
	}
	awaitSyntheticSshAdmission(t, store, func(state sshAdmissionState) bool { return len(state.Requests) == 0 })
}

func TestSharedSshAdmissionProcessHelper(t *testing.T) {
	directory := os.Getenv("SYNTHETIC_SSH_ADMISSION_DIRECTORY")
	if directory == "" {
		return
	}
	state := syntheticSshAdmissionState()
	store, err := newSshAdmissionStore(directory, state.BootId)
	if err != nil {
		t.Fatal(err)
	}
	store.status = func(context.Context, sshAdmissionOwner) sshAdmissionOwnerStatus { return sshAdmissionOwnerLive }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	release, err := store.acquire(ctx, state.Watcher, state.Watcher.Identity, sshAdmissionWatch, []string{os.Getenv("SYNTHETIC_SSH_ADMISSION_HOST")})
	if err != nil {
		t.Fatal(err)
	}
	if err := awaitSshAdmissionRelease(ctx, os.Stdin, release); err != nil {
		t.Fatal(err)
	}
}

func TestSharedSshAdmissionStaleQueuedAndRetainedActive(t *testing.T) {
	store, state := newSyntheticSshAdmissionStore(t)
	now, err := store.now()
	if err != nil {
		t.Fatal(err)
	}
	err = store.update(context.Background(), func(current *sshAdmissionState, _ int64) error {
		active := appendSyntheticSshAdmission(current, sshAdmissionDiagnostic, "a.example", "b.example")
		if !current.grant(active) {
			return errors.New("setup")
		}
		current.Requests[0].ExpiresNanos = now - 1
		appendSyntheticSshAdmission(current, sshAdmissionWatch, "c.example")
		current.Requests[1].ExpiresNanos = now - 1
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	store.status = func(_ context.Context, owner sshAdmissionOwner) sshAdmissionOwnerStatus {
		if owner == state.Watcher {
			return sshAdmissionOwnerLive
		}
		return sshAdmissionOwnerDead
	}
	err = store.update(context.Background(), func(current *sshAdmissionState, now int64) error {
		store.prune(context.Background(), current, now)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	current := awaitSyntheticSshAdmission(t, store, func(state sshAdmissionState) bool { return len(state.Requests) == 1 })
	if !current.Requests[0].Active || current.Requests[0].Class != sshAdmissionDiagnostic {
		t.Fatal("dead helper released possible hop children")
	}
}

func TestSharedSshAdmissionClassificationOmitsPrivateState(t *testing.T) {
	err := &sshAdmissionUnavailableError{err: errors.New("private synthetic state /private/queue.json")}
	if classifyObservationError(err) != observationErrorClassAdmissionUnavailable || strings.Contains(err.Error(), "private") {
		t.Fatal("local admission lost fixed privacy classification")
	}
}

func TestSharedSshAdmissionNativeOwnerCleanupProof(t *testing.T) {
	owner := syntheticSshAdmissionOwner(202)
	for _, testCase := range []struct {
		name              string
		live              bool
		processUnknown    bool
		missing           bool
		reused            bool
		invocationChanged bool
		populated         string
		boot              string
		want              sshAdmissionOwnerStatus
	}{
		{name: "live", live: true, want: sshAdmissionOwnerLive},
		{name: "process-unknown", processUnknown: true, want: sshAdmissionOwnerUnknown},
		{name: "dead-live-children", populated: "populated 1\n", want: sshAdmissionOwnerDead},
		{name: "dead-empty-original", populated: "populated 0\nfrozen 0\n", want: sshAdmissionOwnerGone},
		{name: "dead-missing-original", missing: true, want: sshAdmissionOwnerGone},
		{name: "reused-path", reused: true, populated: "populated 0\n", want: sshAdmissionOwnerDead},
		{name: "reused-unit", invocationChanged: true, populated: "populated 0\n", want: sshAdmissionOwnerDead},
		{name: "population-unknown", populated: "frozen 0\n", want: sshAdmissionOwnerDead},
		{name: "new-boot", boot: "00000000-0000-0000-0000-000000000002", want: sshAdmissionOwnerGone},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			probe := sshAdmissionOwnerProbe{
				read: func(path string, _ int64) ([]byte, error) {
					if strings.HasSuffix(path, "boot_id") {
						if testCase.boot != "" {
							return []byte(testCase.boot), nil
						}
						return []byte(owner.Identity.BootId), nil
					}
					return []byte(testCase.populated), nil
				},
				process: func(int, string) (SshAdmissionWatcher, string, error) {
					if testCase.processUnknown {
						return SshAdmissionWatcher{}, "", errors.New("synthetic unavailable")
					}
					if testCase.live {
						return owner.Identity, owner.Cgroup, nil
					}
					return SshAdmissionWatcher{}, "", os.ErrNotExist
				},
				stat: func(path string, stat *unix.Stat_t) error {
					if testCase.missing && path != "/sys/fs/cgroup" {
						return unix.ENOENT
					}
					stat.Dev = owner.Device
					stat.Ino = owner.Inode
					if testCase.reused {
						stat.Ino++
					}
					return nil
				},
				unit: func(context.Context, string) (map[string]string, error) {
					invocation := owner.InvocationId
					if testCase.invocationChanged {
						invocation = strings.Repeat("f", 32)
					}
					return map[string]string{"InvocationID": invocation, "ControlGroup": owner.Cgroup}, nil
				},
			}
			if got := sshAdmissionProbeStatus(context.Background(), owner, probe); got != testCase.want {
				t.Fatalf("owner disposition=%d, want %d", got, testCase.want)
			}
		})
	}
}
