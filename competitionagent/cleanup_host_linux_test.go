// Hard-exit cleanup tests replace host command, cgroup and mount boundaries.
// Every filesystem fixture is private; no host daemon, mount or cgroup is used.
package competitionagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Explicit barriers expose kill/join order without polling or scheduler luck.
type cleanupTestCgroups struct {
	record func(string)
	empty  func(context.Context, string) (bool, error)
}

// Only names already accepted by the real plan validator reach this seam.
func (self *cleanupTestCgroups) Kill(_ context.Context, name string) error {
	self.record("kill:" + name)
	return nil
}

// Tests may hold the exact population-read boundary to prove cleanup joins it.
func (self *cleanupTestCgroups) Empty(ctx context.Context, name string) (bool, error) {
	self.record("empty:" + name)
	if self.empty != nil {
		return self.empty(ctx, name)
	}
	return true, nil
}

// The private mount seam records only the already validated evidence identity.
type cleanupTestMounts struct {
	record func(string)
	calls  int
}

// No actual mount syscall is used by orchestration tests.
func (self *cleanupTestMounts) Unmount(_ context.Context, plan cleanupPlan) error {
	self.calls++
	self.record("unmount:" + plan.EvidenceDirectory)
	return nil
}

// The daemon fake models full immutable ids and deliberately separate labels.
type cleanupTestDocker struct {
	plan                               cleanupPlan
	stage                              string
	record                             func(string)
	containers, networks               []string
	removedContainers, removedNetworks []string
	inspectMutation                    func(bool, string, map[string]any)
	commandHook                        func(context.Context, []string) error
}

// All list queries must address both exact labels; inspect and remove retain
// full ids, explicit object kinds and a closed, non-pruning argument vocabulary.
func (self *cleanupTestDocker) command(ctx context.Context, args []string) ([]byte, error) {
	self.record("docker:" + strings.Join(args, " "))
	if self.commandHook != nil {
		if err := self.commandHook(ctx, args); err != nil {
			return nil, err
		}
	}
	if len(args) < 2 {
		return nil, errors.New("synthetic malformed command")
	}
	network := args[0] == "network"
	if args[0] != "container" && !network {
		return nil, errors.New("synthetic unscoped daemon command")
	}
	switch args[1] {
	case "ls":
		if !containsCleanupArgument(args, "label="+cleanupJobLabel+"="+self.plan.JobId) || !containsCleanupArgument(args, "label="+cleanupRoundLabel+"="+self.plan.RoundId) || !containsCleanupArgument(args, "--no-trunc") {
			return nil, errors.New("synthetic query omitted exact scope or full ids")
		}
		if network {
			return []byte(strings.Join(self.networks, "\n")), nil
		}
		return []byte(strings.Join(self.containers, "\n")), nil
	case "inspect":
		if len(args) != 4 || args[2] != "--" {
			return nil, errors.New("synthetic inspect expanded its target")
		}
		id := args[3]
		labels := map[string]string{cleanupJobLabel: self.plan.JobId, cleanupRoundLabel: self.plan.RoundId}
		report := map[string]any{"Id": id}
		if network {
			report["Labels"] = labels
		} else {
			report["Config"] = map[string]any{"Labels": labels}
			report["HostConfig"] = map[string]any{"CgroupParent": self.stage}
		}
		if self.inspectMutation != nil {
			self.inspectMutation(network, id, report)
		}
		return json.Marshal([]map[string]any{report})
	case "rm":
		id := args[len(args)-1]
		if network {
			if !reflect.DeepEqual(args, []string{"network", "rm", "--", id}) {
				return nil, errors.New("synthetic network removal expanded")
			}
			self.removedNetworks = append(self.removedNetworks, id)
			self.networks = removeCleanupTestId(self.networks, id)
		} else {
			if !reflect.DeepEqual(args, []string{"container", "rm", "--force", "--", id}) {
				return nil, errors.New("synthetic container removal expanded")
			}
			self.removedContainers = append(self.removedContainers, id)
			self.containers = removeCleanupTestId(self.containers, id)
		}
		return nil, nil
	default:
		return nil, errors.New("synthetic daemon mutation outside the cleanup vocabulary")
	}
}

// Exact equality prevents substring filters from satisfying a test accidentally.
func containsCleanupArgument(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}

// Fake removal cannot delete a different id merely sharing a prefix.
func removeCleanupTestId(ids []string, remove string) []string {
	result := []string{}
	for _, id := range ids {
		if id != remove {
			result = append(result, id)
		}
	}
	return result
}

// A synchronized event inventory exposes exactly which boundary was reached.
type cleanupTestEvents struct {
	stateLock sync.Mutex
	values    []string
}

// Events never contain production fixture values or inherited command output.
func (self *cleanupTestEvents) record(value string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.values = append(self.values, value)
}

// Barrier assertions use a snapshot, not concurrent slice reads.
func (self *cleanupTestEvents) snapshot() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.values...)
}

// No agent state/result is needed: abrupt-exit cleanup uses only its frozen plan.
type hostCleanupTest struct {
	cleaner *hostCleaner
	plan    cleanupPlan
	events  *cleanupTestEvents
	cgroups *cleanupTestCgroups
	docker  *cleanupTestDocker
	mounts  *cleanupTestMounts
}

// Production validation, ordering, budgets and inspect checks remain enabled.
func newHostCleanupTest(t *testing.T) *hostCleanupTest {
	t.Helper()
	root := t.TempDir()
	jobId := "11111111-2222-4333-8444-555555555555"
	roundId := "66666666-7777-4888-8999-aaaaaaaaaaaa"
	runId := RunId(jobId, 0, 1)
	tokenDigest := sha256.Sum256([]byte(jobId + ":1"))
	stage := "urnetwork-evaluation-" + hex.EncodeToString(tokenDigest[:])[:32] + "-candidate-01.slice"
	work := filepath.Join(root, "jobs")
	groups := []string{"urnetwork-submission-build-" + strings.Repeat("b", 32) + ".slice"}
	token := hex.EncodeToString(tokenDigest[:])[:32]
	for index := 1; index <= 9; index++ {
		for _, role := range []string{"baseline", "candidate"} {
			groups = append(groups, fmt.Sprintf("urnetwork-evaluation-%s-%s-%02d.slice", token, role, index))
		}
		groups = append(groups, fmt.Sprintf("urnetwork-evaluation-%s-live-%02d.slice", strings.ReplaceAll(jobId, "-", ""), index))
	}
	groups = append(groups, "urnetwork-evaluation-"+token+"-baseline-score.slice", "urnetwork-evaluation-"+token+"-candidate-score.slice")
	plan := cleanupPlan{Schema: Schema, RunId: runId, JobId: jobId, RoundId: roundId, Attempt: 1,
		CgroupNames:       groups,
		EvidenceDirectory: filepath.Join(work, runId, ".evidence-runtime"), EvidenceSource: "urnetwork-evidence-" + strings.ReplaceAll(jobId, "-", "") + "-1"}
	events := &cleanupTestEvents{}
	cgroups := &cleanupTestCgroups{record: events.record}
	docker := &cleanupTestDocker{plan: plan, stage: stage, record: events.record, containers: []string{strings.Repeat("a", 64)}, networks: []string{strings.Repeat("c", 64)}}
	mounts := &cleanupTestMounts{record: events.record}
	cleaner := &hostCleaner{config: Config{WorkDirectory: work, StateDirectory: filepath.Join(root, "state")}, settings: DefaultAgentSettings(), command: docker.command, cgroups: cgroups, mounts: mounts}
	return &hostCleanupTest{cleaner: cleaner, plan: plan, events: events, cgroups: cgroups, docker: docker, mounts: mounts}
}

// Lost owner processes need no final worker result to kill exact BuildKit work.
// Explicit barriers prove no Docker removal or mount cleanup precedes its join.
func TestHostCleanupHardExitPlanKillsAndJoinsBuildBeforeDocker(t *testing.T) {
	fixture := newHostCleanupTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	build := fixture.plan.CgroupNames[0]
	first := true
	fixture.cgroups.empty = func(ctx context.Context, name string) (bool, error) {
		if name == build && first {
			first = false
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		return true, nil
	}
	done := make(chan error, 1)
	go func() { done <- fixture.cleaner.Cleanup(context.Background(), fixture.plan) }()
	<-entered
	expected := []string{"kill:" + build, "empty:" + build}
	if events := fixture.events.snapshot(); !reflect.DeepEqual(events, expected) {
		t.Fatalf("cleanup escaped unjoined build boundary: %q", events)
	}
	select {
	case err := <-done:
		t.Fatalf("cleanup acknowledged unjoined build: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(fixture.docker.removedContainers) != 1 || len(fixture.docker.removedNetworks) != 1 || fixture.mounts.calls != 1 {
		t.Fatal("exact run resources were not removed")
	}
	events := fixture.events.snapshot()
	if events[len(events)-1] != "unmount:"+fixture.plan.EvidenceDirectory {
		t.Fatalf("mount cleanup preceded resource joins: %q", events)
	}
}

// Cancellation joins the held population-read owner and cannot certify cleanup.
func TestHostCleanupCancellationCannotAcknowledgePopulatedBuild(t *testing.T) {
	fixture := newHostCleanupTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	fixture.cgroups.empty = func(ctx context.Context, _ string) (bool, error) {
		close(entered)
		<-ctx.Done()
		return false, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- fixture.cleaner.Cleanup(ctx, fixture.plan) }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was certified as cleanup: %v", err)
	}
	if len(fixture.events.snapshot()) != 2 || fixture.mounts.calls != 0 {
		t.Fatal("canceled cleanup reached unrelated boundaries")
	}
}

// Both labels, inspect identity and cgroup parent are independently required.
func TestHostCleanupRejectsForeignContainerProofsBeforeRemoval(t *testing.T) {
	mutations := []func(bool, string, map[string]any){
		func(network bool, _ string, report map[string]any) {
			if !network {
				report["Config"] = map[string]any{"Labels": map[string]string{cleanupJobLabel: "synthetic-foreign-job", cleanupRoundLabel: "synthetic-foreign-round"}}
			}
		},
		func(network bool, _ string, report map[string]any) {
			if !network {
				report["HostConfig"] = map[string]any{"CgroupParent": "system.slice"}
			}
		},
		func(network bool, _ string, report map[string]any) {
			if !network {
				report["Id"] = strings.Repeat("d", 64)
			}
		},
		func(network bool, _ string, report map[string]any) {
			if !network {
				delete(report, "Id")
				report["ID"] = strings.Repeat("a", 64)
			}
		},
	}
	for _, mutation := range mutations {
		fixture := newHostCleanupTest(t)
		fixture.docker.inspectMutation = mutation
		if err := fixture.cleaner.Cleanup(context.Background(), fixture.plan); err == nil {
			t.Fatal("foreign container ownership was accepted")
		}
		if len(fixture.docker.removedContainers) != 0 || len(fixture.docker.removedNetworks) != 0 || fixture.mounts.calls != 0 {
			t.Fatal("failed ownership proof reached a destructive boundary")
		}
	}
}

// A foreign network does not become removable from a matching job label alone.
func TestHostCleanupRejectsForeignNetworkBeforeRemovingContainers(t *testing.T) {
	fixture := newHostCleanupTest(t)
	fixture.docker.inspectMutation = func(network bool, _ string, report map[string]any) {
		if network {
			report["Labels"] = map[string]string{cleanupJobLabel: fixture.plan.JobId, cleanupRoundLabel: "synthetic-other-round"}
		}
	}
	if err := fixture.cleaner.Cleanup(context.Background(), fixture.plan); err == nil {
		t.Fatal("foreign network round was accepted")
	}
	if len(fixture.docker.removedContainers) != 0 || len(fixture.docker.removedNetworks) != 0 {
		t.Fatal("ownership preflight did not precede removal")
	}
}

// Invalid lists cannot be expanded into targets or silently truncated to fit.
func TestHostCleanupRejectsShortDuplicateAndExcessDockerIds(t *testing.T) {
	excess := []string{}
	for index := 1; index <= 31; index++ {
		excess = append(excess, fmt.Sprintf("%064x", index))
	}
	cases := [][]string{{"abcdef"}, {strings.Repeat("a", 64), strings.Repeat("a", 64)}, excess}
	for _, ids := range cases {
		fixture := newHostCleanupTest(t)
		fixture.cleaner.settings.CleanupObjectCount = len(fixture.plan.CgroupNames)
		fixture.docker.containers = ids
		if err := fixture.cleaner.Cleanup(context.Background(), fixture.plan); err == nil {
			t.Fatal("invalid daemon inventory was accepted")
		}
		if len(fixture.docker.removedContainers) != 0 || len(fixture.docker.removedNetworks) != 0 {
			t.Fatal("invalid inventory reached removal")
		}
	}
}

// No path, stage or wildcard outside this one attempt reaches host operations.
func TestHostCleanupRejectsExpandedPlansBeforeHostAccess(t *testing.T) {
	mutations := []func(*cleanupPlan){
		func(plan *cleanupPlan) { plan.CgroupNames = append(plan.CgroupNames, "system.slice") },
		func(plan *cleanupPlan) { plan.CgroupNames = append(plan.CgroupNames, plan.CgroupNames[0]) },
		func(plan *cleanupPlan) { plan.CgroupNames[0] = "urnetwork-submission-build-*.slice" },
		func(plan *cleanupPlan) { plan.CgroupNames = plan.CgroupNames[:len(plan.CgroupNames)-1] },
		func(plan *cleanupPlan) { plan.CgroupNames = plan.CgroupNames[:1] },
		func(plan *cleanupPlan) {
			plan.CgroupNames[len(plan.CgroupNames)-1] = "urnetwork-submission-build-" + strings.Repeat("e", 32) + ".slice"
		},
		func(plan *cleanupPlan) { plan.EvidenceDirectory = "/" },
		func(plan *cleanupPlan) { plan.EvidenceSource = "synthetic-foreign-mount" },
		func(plan *cleanupPlan) { plan.RunId = "../other" },
		func(plan *cleanupPlan) { plan.JobId = "synthetic-invalid-uuid" },
		func(plan *cleanupPlan) { plan.Attempt = 0 },
	}
	for _, mutation := range mutations {
		fixture := newHostCleanupTest(t)
		mutation(&fixture.plan)
		if err := fixture.cleaner.Cleanup(context.Background(), fixture.plan); err == nil {
			t.Fatal("expanded cleanup scope was accepted")
		}
		if len(fixture.events.snapshot()) != 0 {
			t.Fatal("invalid cleanup plan touched a host boundary")
		}
	}
}

// A blocked daemon call owns a child deadline and joins cancellation before
// any subsequent mutation or successful cleanup acknowledgement.
func TestHostCleanupDaemonCancellationRetainsOneBoundedOwner(t *testing.T) {
	fixture := newHostCleanupTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan bool, 1)
	fixture.docker.commandHook = func(ctx context.Context, _ []string) error {
		deadline, present := ctx.Deadline()
		entered <- present && time.Until(deadline) <= fixture.cleaner.settings.CleanupCommandTimeout
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- fixture.cleaner.Cleanup(ctx, fixture.plan) }()
	if !<-entered {
		t.Fatal("daemon call had no configured child deadline")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked daemon cancellation: %v", err)
	}
	if fixture.mounts.calls != 0 || len(fixture.docker.removedContainers) != 0 {
		t.Fatal("canceled daemon call released cleanup custody")
	}
}

// Nonpositive budgets and oversized daemon documents fail before useful work.
func TestHostCleanupRejectsDisabledBoundsAndOversizedOutput(t *testing.T) {
	fixture := newHostCleanupTest(t)
	fixture.cleaner.settings.CleanupTimeout = 0
	if err := fixture.cleaner.Cleanup(context.Background(), fixture.plan); err == nil || len(fixture.events.snapshot()) != 0 {
		t.Fatal("disabled cleanup budget admitted host work")
	}
	fixture = newHostCleanupTest(t)
	fixture.cleaner.settings.DiagnosticBytes = 8
	fixture.cleaner.command = func(context.Context, []string) ([]byte, error) { return []byte(strings.Repeat("x", 9)), nil }
	if err := fixture.cleaner.Cleanup(context.Background(), fixture.plan); err == nil {
		t.Fatal("oversized daemon document was accepted")
	}
}

// Synthetic cgroup properties exercise the real descriptor-confined backend.
func TestHostCleanupCgroupFilesAreExactAndForeignGroupsUntouched(t *testing.T) {
	fixture := newHostCleanupTest(t)
	root := t.TempDir()
	name := fixture.plan.CgroupNames[1]
	relative, err := cleanupSlicePath(name)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, relative)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cgroup.kill"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cgroup.events"), []byte("populated 0\nfrozen 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, "system.slice")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "cgroup.kill"), []byte("synthetic-foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	groups := &hostCleanupCgroups{rootPath: root, maximumBytes: 128}
	if err := groups.Kill(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	if empty, err := groups.Empty(context.Background(), name); err != nil || !empty {
		t.Fatalf("exact cgroup join: empty=%t error=%v", empty, err)
	}
	if err := groups.Kill(context.Background(), "system.slice"); err == nil {
		t.Fatal("foreign system slice was accepted")
	}
	content, err := os.ReadFile(filepath.Join(foreign, "cgroup.kill"))
	if err != nil || string(content) != "synthetic-foreign" {
		t.Fatal("foreign cgroup was modified")
	}
	content, err = os.ReadFile(filepath.Join(directory, "cgroup.kill"))
	if err != nil || string(content) != "1\n" {
		t.Fatal("exact cgroup did not receive kill")
	}
}

// Missing kernel support/hierarchy differs from an absent exact owned group.
func TestHostCleanupCgroupUnavailableOrMissingPropertyCannotProveCleanup(t *testing.T) {
	fixture := newHostCleanupTest(t)
	root := t.TempDir()
	name := fixture.plan.CgroupNames[1]
	groups := &hostCleanupCgroups{rootPath: filepath.Join(root, "missing-root"), maximumBytes: 128}
	if err := groups.Kill(context.Background(), name); err == nil {
		t.Fatal("missing cgroup hierarchy was treated as empty")
	}
	groups.rootPath = root
	if empty, err := groups.Empty(context.Background(), name); err != nil || !empty {
		t.Fatal("absent exact group was not already empty")
	}
	relative, err := cleanupSlicePath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, relative), 0700); err != nil {
		t.Fatal(err)
	}
	if err := groups.Kill(context.Background(), name); err == nil {
		t.Fatal("missing required kill support was treated as successful cleanup")
	}
	if _, err := groups.Empty(context.Background(), name); err == nil {
		t.Fatal("missing population property was treated as empty")
	}
}

// Cgroup counters must be finite boolean values, unique and byte-bounded.
func TestHostCleanupCgroupRejectsMalformedPopulationAndLinks(t *testing.T) {
	fixture := newHostCleanupTest(t)
	root := t.TempDir()
	name := fixture.plan.CgroupNames[1]
	relative, err := cleanupSlicePath(name)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, relative)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "cgroup.events")
	groups := &hostCleanupCgroups{rootPath: root, maximumBytes: 128}
	for _, content := range []string{"populated NaN\n", "populated 2\n", "populated 0\npopulated 1\n", "frozen 0\n", "populated 0\nfrozen NaN\n", strings.Repeat("x", 129)} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := groups.Empty(context.Background(), name); err == nil {
			t.Fatalf("malformed population was accepted: %q", content)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "events")
	if err := os.WriteFile(outside, []byte("populated 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := groups.Empty(context.Background(), name); err == nil {
		t.Fatal("cgroup property link escaped its confined root")
	}
}

// A synthetic filesystem never satisfies the production kernel-type check;
// symlinked ancestors cannot redirect writes into a different synthetic group.
func TestHostCleanupCgroupRejectsWrongFilesystemAndSymlinkAncestors(t *testing.T) {
	fixture := newHostCleanupTest(t)
	root := t.TempDir()
	name := fixture.plan.CgroupNames[1]
	groups := &hostCleanupCgroups{rootPath: root, maximumBytes: 128, requireCgroupFs: true}
	if err := groups.Kill(context.Background(), name); err == nil {
		t.Fatal("non-cgroup filesystem accepted as kernel containment")
	}
	groups.requireCgroupFs = false
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "cgroup.kill"), []byte("synthetic-foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, filepath.Join(root, "urnetwork.slice")); err != nil {
		t.Fatal(err)
	}
	if err := groups.Kill(context.Background(), name); err == nil {
		t.Fatal("symlinked hierarchy was accepted")
	}
	content, err := os.ReadFile(filepath.Join(foreign, "cgroup.kill"))
	if err != nil || string(content) != "synthetic-foreign" {
		t.Fatal("symlinked foreign cgroup was modified")
	}
}

// An exact tmpfs inventory plus a stable descriptor is the only unmount proof.
type cleanupMountTest struct {
	mounts     *hostCleanupMounts
	plan       cleanupPlan
	content    string
	unmounts   int
	reads      int
	beforeRead func(int)
	child      *os.File
}

// The real FD-confined open is used; only procfs mount metadata and unmount are
// synthetic. The temporary directory is never an actual mountpoint.
func newCleanupMountTest(t *testing.T) *cleanupMountTest {
	t.Helper()
	fixture := newHostCleanupTest(t)
	work := filepath.Join(t.TempDir(), "runtime space", "jobs")
	fixture.cleaner.config.WorkDirectory = work
	fixture.plan.EvidenceDirectory = filepath.Join(work, fixture.plan.RunId, ".evidence-runtime")
	if err := os.MkdirAll(fixture.plan.EvidenceDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(work)
	if err != nil {
		t.Fatal(err)
	}
	fixture.cleaner.config.ExpectedDataDeviceId = uint64(info.Sys().(*syscall.Stat_t).Dev)
	test := &cleanupMountTest{plan: fixture.plan}
	test.content = cleanupMountTestLine(42, test.plan.EvidenceDirectory, "tmpfs", test.plan.EvidenceSource)
	test.mounts = newHostCleanupMounts(fixture.cleaner.config, fixture.cleaner.settings)
	test.mounts.readMounts = func(context.Context) ([]byte, error) {
		test.reads++
		if test.beforeRead != nil {
			test.beforeRead(test.reads)
		}
		return []byte(test.content), nil
	}
	test.mounts.mountId = func(file *os.File) (uint64, error) { test.child = file; return 42, nil }
	test.mounts.unmount = func(ctx context.Context, target string, flags int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasPrefix(target, "/proc/self/fd/") || !strings.HasSuffix(target, "/.evidence-runtime") || flags != 8 {
			return errors.New("synthetic unmount was not non-lazy and descriptor-pinned")
		}
		actual, err := os.Readlink(strings.TrimSuffix(target, "/.evidence-runtime"))
		if err != nil || actual != filepath.Dir(test.plan.EvidenceDirectory) {
			return errors.New("synthetic unmount descriptor changed")
		}
		if test.child == nil {
			return errors.New("synthetic cleanup never captured the child mount identity")
		}
		if _, err := test.child.Stat(); !errors.Is(err, os.ErrClosed) {
			return errors.New("synthetic child mount descriptor would keep tmpfs busy")
		}
		test.unmounts++
		test.content = ""
		return nil
	}
	return test
}

// Test mountinfo uses only encoded synthetic private paths and sources.
func cleanupMountTestLine(id int, target, filesystem, source string) string {
	encoded := strings.NewReplacer("\\", `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(target)
	return fmt.Sprintf("%d 1 0:77 / %s rw,nosuid,nodev - %s %s rw\n", id, encoded, filesystem, source)
}

// Normal cleanup never passes a raceable pathname or a lazy/recursive flag.
func TestHostCleanupEvidenceUnmountUsesStableDescriptorAndExactSource(t *testing.T) {
	fixture := newCleanupMountTest(t)
	if err := fixture.mounts.Unmount(context.Background(), fixture.plan); err != nil {
		t.Fatal(err)
	}
	if fixture.unmounts != 1 || fixture.reads != 3 {
		t.Fatalf("mount identity was not revalidated around unmount: calls=%d reads=%d", fixture.unmounts, fixture.reads)
	}
}

// A source mismatch, foreign filesystem, stacked mount or nested mount never
// reaches the kernel mutation, including a nested mount with no matching parent.
func TestHostCleanupEvidenceRefusesForeignAndNestedMounts(t *testing.T) {
	mutations := []func(*cleanupMountTest){
		func(f *cleanupMountTest) {
			f.content = cleanupMountTestLine(42, f.plan.EvidenceDirectory, "tmpfs", "synthetic-foreign-source")
		},
		func(f *cleanupMountTest) {
			f.content = cleanupMountTestLine(42, f.plan.EvidenceDirectory, "ext4", f.plan.EvidenceSource)
		},
		func(f *cleanupMountTest) {
			f.content += cleanupMountTestLine(43, f.plan.EvidenceDirectory, "tmpfs", f.plan.EvidenceSource)
		},
		func(f *cleanupMountTest) {
			f.content += cleanupMountTestLine(43, filepath.Join(f.plan.EvidenceDirectory, "foreign"), "tmpfs", "synthetic-child")
		},
		func(f *cleanupMountTest) {
			f.content = cleanupMountTestLine(43, filepath.Join(f.plan.EvidenceDirectory, "foreign"), "tmpfs", "synthetic-child")
		},
	}
	for _, mutate := range mutations {
		fixture := newCleanupMountTest(t)
		mutate(fixture)
		if err := fixture.mounts.Unmount(context.Background(), fixture.plan); err == nil {
			t.Fatal("foreign or nested mount was accepted")
		}
		if fixture.unmounts != 0 {
			t.Fatal("foreign mount reached unmount")
		}
	}
}

// Both a descriptor mismatch and a later path replacement refuse unmount.
func TestHostCleanupEvidenceReplacementCannotRetargetUnmount(t *testing.T) {
	fixture := newCleanupMountTest(t)
	fixture.mounts.mountId = func(*os.File) (uint64, error) { return 43, nil }
	if err := fixture.mounts.Unmount(context.Background(), fixture.plan); err == nil || fixture.unmounts != 0 {
		t.Fatal("replaced descriptor mount was unmounted")
	}
	fixture = newCleanupMountTest(t)
	fixture.beforeRead = func(index int) {
		if index == 2 {
			fixture.content = cleanupMountTestLine(43, fixture.plan.EvidenceDirectory, "tmpfs", fixture.plan.EvidenceSource)
		}
	}
	if err := fixture.mounts.Unmount(context.Background(), fixture.plan); err == nil || fixture.unmounts != 0 {
		t.Fatal("replaced mount path retargeted unmount")
	}
}

// Overflowed ids, oversized mount inventories and bad path escapes fail closed.
func TestHostCleanupEvidenceRejectsMalformedAndUnboundedInventory(t *testing.T) {
	cases := []string{"18446744073709551616 1 0:77 / /synthetic rw - tmpfs synthetic rw\n", "42 1 0:77 / /synthetic\\777 rw - tmpfs synthetic rw\n"}
	for _, content := range cases {
		fixture := newCleanupMountTest(t)
		fixture.content = content
		if err := fixture.mounts.Unmount(context.Background(), fixture.plan); err == nil || fixture.unmounts != 0 {
			t.Fatal("malformed inventory reached unmount")
		}
	}
	fixture := newCleanupMountTest(t)
	fixture.mounts.settings.DiagnosticBytes = 8
	if err := fixture.mounts.Unmount(context.Background(), fixture.plan); err == nil || fixture.unmounts != 0 {
		t.Fatal("oversized inventory reached unmount")
	}
}
