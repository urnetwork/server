// Host-file regressions use private files and injected ownership/mount facts.
// They perform no real chown, tmpfs mount, systemd, Docker or procfs lookup.
package competitionagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

type hostFilesTest struct {
	broker   *hostFileBroker
	output   string
	chowns   int
	mounts   int
	unmounts int
}

// Work uses a real temporary device identity; all privileged actions remain
// private injected callbacks rather than relying on the test process uid.
func newHostFilesTest(t *testing.T) *hostFilesTest {
	t.Helper()
	proxy := newDockerProxyTest(t)
	info, err := os.Stat(proxy.plan.ContextRoot)
	if err != nil {
		t.Fatal(err)
	}
	plan := proxy.plan
	plan.HostFiles = &HostFilesPlan{WorkerGid: 12345, ContainerRootUid: 200000, ContainerRootGid: 300000, ContainerIdMapLength: 65536,
		EvidenceBytes: 1024 * 1024, WorkDeviceId: uint64(info.Sys().(*syscall.Stat_t).Dev)}
	broker, err := newHostFileBroker(plan, DefaultHostFilesSettings())
	if err != nil {
		t.Fatal(err)
	}
	fixture := &hostFilesTest{broker: broker, output: filepath.Join(plan.ContextRoot, ".evidence-runtime", "runs", "candidate-01", "output")}
	if err := os.MkdirAll(fixture.output, 0700); err != nil {
		t.Fatal(err)
	}
	broker.chown = func(_ *os.File, _, _ uint32) error { fixture.chowns++; return nil }
	broker.mount = func(_ context.Context, scope cleanupPlan, uid, gid uint32, bytes int64) error {
		fixture.mounts++
		if scope.RunId != plan.RunId || scope.EvidenceDirectory != filepath.Join(plan.ContextRoot, ".evidence-runtime") || scope.WorkDeviceId != plan.HostFiles.WorkDeviceId ||
			uid != plan.WorkerUid || gid != plan.HostFiles.WorkerGid || bytes != plan.HostFiles.EvidenceBytes {
			t.Fatal("worker request changed protected evidence mount authority")
		}
		return nil
	}
	broker.unmount = func(_ context.Context, scope cleanupPlan) error {
		fixture.unmounts++
		if scope.EvidenceSource != broker.cleanupPlan().EvidenceSource {
			t.Fatal("worker request changed evidence unmount identity")
		}
		return nil
	}
	broker.beforePath = func(context.Context, string) error { return nil }
	broker.proveMap = func(context.Context, string) (HostNamespaceMap, error) {
		return HostNamespaceMap{ContainerRootUid: plan.HostFiles.ContainerRootUid, ContainerRootGid: plan.HostFiles.ContainerRootGid,
			ContainerIdMapLength: plan.HostFiles.ContainerIdMapLength, UidMapSha256: strings.Repeat("a", 64), GidMapSha256: strings.Repeat("b", 64)}, nil
	}
	return fixture
}

func (self *hostFilesTest) request(operation, path string) HostFilesRequest {
	return HostFilesRequest{Schema: Schema, RunId: self.broker.plan.RunId, Operation: operation, Path: path}
}

func (self *hostFilesTest) verify(t *testing.T) {
	t.Helper()
	request := self.request("verify-id-map", "")
	request.ContainerId = strings.Repeat("c", 64)
	if result := self.broker.Execute(t.Context(), request); result.ExitCode != 0 || !result.Acknowledged || result.Namespace == nil {
		t.Fatal("synthetic positive namespace proof was refused", result)
	}
}

// Namespace proof precedes mapped output ownership; a metadata document alone
// never establishes the live daemon's actual host uid/gid mapping.
func TestHostFilesRequireLiveNamespaceProofBeforeOutputHandoff(t *testing.T) {
	fixture := newHostFilesTest(t)
	request := fixture.request("prepare-output", fixture.output)
	if result := fixture.broker.Execute(t.Context(), request); result.ExitCode == 0 || result.ErrorCode != "host_namespace_unverified" || fixture.chowns != 0 {
		t.Fatal("declared namespace metadata became live ownership authority", result)
	}
	fixture.verify(t)
	if result := fixture.broker.Execute(t.Context(), request); result.ExitCode != 0 || !result.Acknowledged || fixture.chowns != 1 {
		t.Fatal("actual matched namespace proof did not admit empty output handoff", result)
	}
	if err := os.WriteFile(filepath.Join(fixture.output, "existing.json"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if result := fixture.broker.Execute(t.Context(), request); result.ExitCode == 0 || fixture.chowns != 1 {
		t.Fatal("nonempty output directory was handed off as fresh", result)
	}
}

// An incorrect probe cannot authorize even one output mutation, and a mapped
// range cannot overlap host root, the management uid/gid or the uint32 bound.
func TestHostFilesRejectWrongNamespaceAndOverlappingHostIds(t *testing.T) {
	fixture := newHostFilesTest(t)
	fixture.broker.proveMap = func(context.Context, string) (HostNamespaceMap, error) {
		return HostNamespaceMap{ContainerRootUid: 1, ContainerRootGid: 1, ContainerIdMapLength: 65536, UidMapSha256: strings.Repeat("a", 64), GidMapSha256: strings.Repeat("b", 64)}, nil
	}
	request := fixture.request("verify-id-map", "")
	request.ContainerId = strings.Repeat("c", 64)
	if result := fixture.broker.Execute(t.Context(), request); result.ExitCode == 0 {
		t.Fatal("wrong actual namespace was accepted")
	}
	if result := fixture.broker.Execute(t.Context(), fixture.request("prepare-output", fixture.output)); result.ExitCode == 0 || fixture.chowns != 0 {
		t.Fatal("failed namespace probe was retained as verified")
	}
	for _, change := range []func(*HostFilesPlan){func(plan *HostFilesPlan) { plan.ContainerRootUid = 0 },
		func(plan *HostFilesPlan) { plan.ContainerRootUid = fixture.broker.plan.WorkerUid },
		func(plan *HostFilesPlan) { plan.ContainerRootGid = plan.WorkerGid },
		func(plan *HostFilesPlan) { plan.ContainerRootUid = ^uint32(0) - 10 },
		func(plan *HostFilesPlan) { plan.ContainerIdMapLength = hostFilesContainerUid }} {
		plan := fixture.broker.plan
		copy := *plan.HostFiles
		change(&copy)
		plan.HostFiles = &copy
		if _, err := newHostFileBroker(plan, fixture.broker.settings); err == nil {
			t.Error("overlapping or insufficient namespace range was admitted")
		}
	}
}

// No request can select another run, an arbitrary path/owner/command, a mount
// source, a short container identity or a directory outside the stage grammar.
func TestHostFilesRejectExpandedCapabilitiesBeforeNativeBoundary(t *testing.T) {
	fixture := newHostFilesTest(t)
	fixture.verify(t)
	requests := []HostFilesRequest{fixture.request("shell", fixture.output), fixture.request("mount-evidence", fixture.output),
		fixture.request("reclaim-output", fixture.broker.plan.ContextRoot), fixture.request("prepare-output", filepath.Join(fixture.broker.plan.ContextRoot, "worker-request.json")),
		fixture.request("reclaim-output", filepath.Join(fixture.broker.plan.ContextRoot, ".evidence-runtime", "runs", "candidate-10", "output")),
		fixture.request("reclaim-output", fixture.output+"/../output"), fixture.request("reclaim-output", "/synthetic-outside/output")}
	otherRun := fixture.request("prepare-output", fixture.output)
	otherRun.RunId = strings.Repeat("e", 64)
	requests = append(requests, otherRun)
	shortContainer := fixture.request("verify-id-map", "")
	shortContainer.ContainerId = strings.Repeat("c", 12)
	requests = append(requests, shortContainer)
	for _, request := range requests {
		if result := fixture.broker.Execute(t.Context(), request); result.ExitCode == 0 || fixture.chowns != 0 || fixture.mounts != 0 || fixture.unmounts != 0 {
			t.Fatal("expanded host capability reached a native mutation", request.Operation, result)
		}
	}
}

// A swap at the precise post-enumeration/open boundary must not redirect root
// ownership changes through a candidate-controlled ancestor symlink.
func TestHostFilesDirectorySwapCannotModifyOutsideTree(t *testing.T) {
	fixture := newHostFilesTest(t)
	fixture.verify(t)
	outside := t.TempDir()
	if err := os.Chmod(outside, 0750); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(fixture.output, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	fixture.broker.beforeOpen = func(_ *os.File, name string) {
		if name == "child" {
			if err := os.Rename(child, child+"-captured"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, child); err != nil {
				t.Fatal(err)
			}
		}
	}
	if result := fixture.broker.Execute(t.Context(), fixture.request("reclaim-output", fixture.output)); result.ExitCode == 0 || fixture.chowns != 0 {
		t.Fatal("swapped directory symlink acquired root metadata authority", result)
	}
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0750 {
		t.Fatal("root metadata operation escaped through a swapped directory", err)
	}
}

// Once captured, an fd names the original inode even after its path becomes
// an external symlink. No path-based chmod/chown can follow the replacement.
func TestHostFilesFileSwapUsesCapturedInodeOnly(t *testing.T) {
	fixture := newHostFilesTest(t)
	fixture.verify(t)
	outside := filepath.Join(t.TempDir(), "outside.json")
	inside := filepath.Join(fixture.output, "inside.json")
	for _, path := range []string{outside, inside} {
		if err := os.WriteFile(path, []byte("synthetic preserved data"), 0444); err != nil {
			t.Fatal(err)
		}
	}
	insideInfo, err := os.Stat(inside)
	if err != nil {
		t.Fatal(err)
	}
	captured := false
	fixture.broker.chown = func(file *os.File, _, _ uint32) error {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		if os.SameFile(info, insideInfo) && !captured {
			captured = true
			if err := os.Rename(inside, inside+"-captured"); err != nil {
				return err
			}
			return os.Symlink(outside, inside)
		}
		return nil
	}
	_ = fixture.broker.Execute(t.Context(), fixture.request("reclaim-output", fixture.output))
	outsideInfo, err := os.Stat(outside)
	if err != nil || !captured || outsideInfo.Mode().Perm() != 0444 {
		t.Fatal("captured file operation followed a later path replacement", err)
	}
	retained, err := os.Stat(inside + "-captured")
	if err != nil || retained.Mode().Perm() != 0600 {
		t.Fatal("metadata operation did not affect its captured original inode", err)
	}
}

// A regular inode shared outside the output tree must not acquire ownership
// or writable mode merely because one of its hard links is below the root.
func TestHostFilesRejectHardLinkedOutputBeforeMetadataWrite(t *testing.T) {
	fixture := newHostFilesTest(t)
	fixture.verify(t)
	outside := filepath.Join(fixture.broker.plan.BaseDirectory, "outside.json")
	if err := os.WriteFile(outside, []byte("synthetic unchanged"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(fixture.output, "shared.json")); err != nil {
		t.Fatal(err)
	}
	if result := fixture.broker.Execute(t.Context(), fixture.request("reclaim-output", fixture.output)); result.ExitCode == 0 || fixture.chowns != 0 {
		t.Fatal("shared external inode acquired metadata write authority", result)
	}
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0444 {
		t.Fatal("hard-linked outside inode changed", err)
	}
}

// The filesystem walk stops at its declared examination/depth bound and can
// run while native Docker visibility is explicitly refused without root writes.
func TestHostFilesBoundTreeWorkAndRequireDockerVisibility(t *testing.T) {
	fixture := newHostFilesTest(t)
	fixture.verify(t)
	if err := os.WriteFile(filepath.Join(fixture.output, "one.json"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.broker.settings.MaximumTreeEntries = 1
	if result := fixture.broker.Execute(t.Context(), fixture.request("reclaim-output", fixture.output)); result.ExitCode == 0 || fixture.chowns != 0 {
		t.Fatal("walk examined beyond its declared entry budget", result)
	}
	fixture.broker.settings.MaximumTreeEntries = 100
	fixture.broker.beforePath = func(context.Context, string) error { return errors.New("synthetic daemon visibility loss") }
	if result := fixture.broker.Execute(t.Context(), fixture.request("reclaim-output", fixture.output)); result.ExitCode == 0 || fixture.chowns != 0 {
		t.Fatal("unknown daemon state admitted a host output mutation", result)
	}
}

// Evidence mutation always uses derived scope; an unconfirmed native mount
// result remains unknown rather than a successful cleanup receipt.
func TestHostFilesEvidenceScopeAndUnknownAcknowledgmentStayDistinct(t *testing.T) {
	fixture := newHostFilesTest(t)
	if result := fixture.broker.Execute(t.Context(), fixture.request("mount-evidence", "")); result.ExitCode != 0 || fixture.mounts != 1 {
		t.Fatal("derived bounded evidence mount was refused", result)
	}
	if result := fixture.broker.Execute(t.Context(), fixture.request("unmount-evidence", "")); result.ExitCode != 0 || fixture.unmounts != 1 {
		t.Fatal("derived exact evidence unmount was refused", result)
	}
	fixture.broker.mount = func(context.Context, cleanupPlan, uint32, uint32, int64) error { return errHostFilesUnknown }
	result := fixture.broker.Execute(t.Context(), fixture.request("mount-evidence", ""))
	if result.Acknowledged || result.ExitCode == 0 || result.ErrorCode != "host_operation_outcome_unknown" {
		t.Fatal("unknown native evidence operation was acknowledged as complete", result)
	}
}

// A canceled native-operation waiter cannot wake later and stack another
// mount. Its owned lifetime ends while the first explicit barrier is held.
func TestHostFilesSerializesNativeMutationAndWithdrawsCanceledWaiter(t *testing.T) {
	fixture := newHostFilesTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	calls := 0
	fixture.broker.mount = func(ctx context.Context, _ cleanupPlan, _, _ uint32, _ int64) error {
		calls++
		if calls == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	done, joined := make(chan HostFilesResult, 1), make(chan struct{})
	go func() {
		defer close(joined)
		done <- fixture.broker.Execute(t.Context(), fixture.request("mount-evidence", ""))
	}()
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); <-joined })
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result := fixture.broker.Execute(ctx, fixture.request("mount-evidence", "")); result.ExitCode == 0 || result.ErrorCode != "host_operation_canceled" || calls != 1 {
		t.Fatal("canceled capacity waiter entered a native mutation", result)
	}
	releaseOnce.Do(func() { close(release) })
	if result := <-done; result.ExitCode != 0 {
		t.Fatal("retiring another waiter canceled the admitted native owner", result)
	}
	<-joined
	if result := fixture.broker.Execute(t.Context(), fixture.request("mount-evidence", "")); result.ExitCode != 0 || calls != 2 {
		t.Fatal("canceled waiter retained native capacity or replayed after its lifetime", result)
	}
}
