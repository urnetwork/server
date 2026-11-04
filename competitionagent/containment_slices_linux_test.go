// Synthetic systemd/kernel boundaries exercise fixed-slice containment without
// changing any host unit, Docker object, cgroup, mount or account permission.
package competitionagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type containmentTestCgroups struct {
	events   *cleanupTestEvents
	freeze   func(context.Context, string) error
	frozen   func(context.Context, string) (bool, error)
	empty    func(context.Context, string) (bool, error)
	prepared bool
}

func (self *containmentTestCgroups) Freeze(ctx context.Context, name string) error {
	self.events.record("freeze:" + name)
	if self.freeze != nil {
		return self.freeze(ctx, name)
	}
	return nil
}

func (self *containmentTestCgroups) Kill(_ context.Context, name string) error {
	self.events.record("kill:" + name)
	return nil
}

func (self *containmentTestCgroups) Frozen(ctx context.Context, name string) (bool, error) {
	self.events.record("frozen:" + name)
	if self.frozen != nil {
		return self.frozen(ctx, name)
	}
	return !self.prepared, nil
}

func (self *containmentTestCgroups) Empty(ctx context.Context, name string) (bool, error) {
	self.events.record("empty:" + name)
	if self.empty != nil {
		return self.empty(ctx, name)
	}
	return true, nil
}

// Every prepared unit is the exact manifest-derived full-R9 parent inventory.
func TestContainmentSlicesPrepareRequiresEmptyUnfrozenCompleteParents(t *testing.T) {
	for _, refusal := range []string{"", "populated", "frozen", "missing"} {
		fixture := newHostCleanupTest(t)
		groups := &containmentTestCgroups{events: fixture.events, prepared: true}
		if refusal == "populated" {
			groups.empty = func(context.Context, string) (bool, error) { return false, nil }
		}
		if refusal == "frozen" {
			groups.frozen = func(context.Context, string) (bool, error) { return true, nil }
		}
		if refusal == "missing" {
			groups.frozen = func(context.Context, string) (bool, error) { return false, errCleanupCgroupAbsent }
		}
		calls := 0
		boundary := &hostRunSlices{config: fixture.cleaner.config, settings: fixture.cleaner.settings, cgroups: groups,
			command: func(ctx context.Context, operation string, names []string) error {
				calls++
				if ctx.Err() != nil || operation != "start" || !reflect.DeepEqual(names, fixture.plan.CgroupNames) || len(names) != 31 {
					t.Fatal("preparation changed its fixed complete identity")
				}
				return nil
			}}
		err := boundary.Prepare(context.Background(), fixture.plan)
		if calls != 1 || (err != nil) != (refusal != "") {
			t.Fatal("preparation accepted unknown/populated/frozen parents", refusal, err)
		}
	}
}

// A first blocked population read cannot delay stop requests to other exact
// parents; a late daemon start under any one of them is already frozen.
func TestContainmentSlicesFreezeAndKillEveryParentBeforeAnyWait(t *testing.T) {
	fixture := newHostCleanupTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	groups := &containmentTestCgroups{events: fixture.events}
	first := true
	groups.frozen = func(ctx context.Context, _ string) (bool, error) {
		if first {
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
	boundary := &hostRunSlices{config: fixture.cleaner.config, settings: fixture.cleaner.settings, cgroups: groups,
		command: func(context.Context, string, []string) error {
			t.Error("reaping attempted unit creation/removal")
			return nil
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	joined := make(chan error, 1)
	finished := make(chan struct{})
	var releaseOnce sync.Once
	go func() { defer close(finished); joined <- boundary.FreezeAndReap(ctx, fixture.plan) }()
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("synthetic freezer owner did not join")
		}
	})
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("freezer did not reach the explicit acknowledgment barrier")
	}
	wanted := []string{}
	for _, name := range fixture.plan.CgroupNames {
		wanted = append(wanted, "freeze:"+name)
	}
	for _, name := range fixture.plan.CgroupNames {
		wanted = append(wanted, "kill:"+name)
	}
	wanted = append(wanted, "frozen:"+fixture.plan.CgroupNames[0])
	if !reflect.DeepEqual(fixture.events.snapshot(), wanted) {
		t.Fatal("a blocked acknowledgment prevented another exact parent stop request")
	}
	select {
	case err := <-joined:
		t.Fatal("a freeze write was mistaken for acknowledged suspension", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
}

// One failed target still attempts every kill, but never certifies custody.
func TestContainmentSlicesFailureCannotSkipOtherOwnedKills(t *testing.T) {
	fixture := newHostCleanupTest(t)
	cause := errors.New("synthetic freeze refusal")
	groups := &containmentTestCgroups{events: fixture.events, freeze: func(_ context.Context, name string) error {
		if name == fixture.plan.CgroupNames[0] {
			return cause
		}
		return nil
	}}
	boundary := &hostRunSlices{config: fixture.cleaner.config, settings: fixture.cleaner.settings, cgroups: groups,
		command: func(context.Context, string, []string) error { return nil }}
	if err := boundary.FreezeAndReap(context.Background(), fixture.plan); !errors.Is(err, cause) {
		t.Fatal("a failed stop target was certified as quiescent", err)
	}
	stops := 0
	for _, event := range fixture.events.snapshot() {
		if strings.HasPrefix(event, "kill:") {
			stops++
		}
	}
	if stops != len(fixture.plan.CgroupNames) {
		t.Fatal("first failed freeze abandoned another owned process tree")
	}
}

// No foreign or incomplete plan may create or write any kernel/systemd target.
func TestContainmentSlicesRejectUnauthorizedPlanBeforeAnyBoundary(t *testing.T) {
	fixture := newHostCleanupTest(t)
	groups := &containmentTestCgroups{events: fixture.events}
	boundary := &hostRunSlices{config: fixture.cleaner.config, settings: fixture.cleaner.settings, cgroups: groups,
		command: func(context.Context, string, []string) error { t.Error("invalid plan reached systemd"); return nil }}
	fixture.plan.CgroupNames[0] = "user.slice"
	for _, run := range []func(context.Context, cleanupPlan) error{boundary.Prepare, boundary.FreezeAndReap, boundary.VerifyRetained} {
		if err := run(context.Background(), fixture.plan); err == nil {
			t.Fatal("invalid finite inventory reached containment")
		}
	}
	if len(fixture.events.snapshot()) != 0 {
		t.Fatal("invalid plan touched a kernel boundary")
	}
}

// An empty or missing group alone does not acknowledge a retained freezer.
func TestContainmentSlicesRetainedProofRequiresFrozenAndEmpty(t *testing.T) {
	for _, refusal := range []string{"", "not-frozen", "populated", "absent"} {
		fixture := newHostCleanupTest(t)
		groups := &containmentTestCgroups{events: fixture.events}
		switch refusal {
		case "not-frozen":
			groups.frozen = func(context.Context, string) (bool, error) { return false, nil }
		case "populated":
			groups.empty = func(context.Context, string) (bool, error) { return false, nil }
		case "absent":
			groups.frozen = func(context.Context, string) (bool, error) { return false, errCleanupCgroupAbsent }
		}
		boundary := &hostRunSlices{config: fixture.cleaner.config, settings: fixture.cleaner.settings, cgroups: groups,
			command: func(context.Context, string, []string) error { t.Error("verification changed a unit"); return nil }}
		if err := boundary.VerifyRetained(context.Background(), fixture.plan); (err != nil) != (refusal != "") {
			t.Fatal("missing freeze or population was certified as retained containment", refusal, err)
		}
	}
}

// Native FD access is exercised only under a synthetic private hierarchy.
func newContainmentKernelFixture(t *testing.T) (*hostCleanupCgroups, string, string) {
	t.Helper()
	root := t.TempDir()
	name := "urnetwork-submission-build-" + strings.Repeat("a", 32) + ".slice"
	relative, err := cleanupSlicePath(name)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, relative)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cgroup.freeze"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	return &hostCleanupCgroups{rootPath: root, maximumBytes: 128}, name, directory
}

func TestContainmentKernelFreezeRequiresExactFiniteAcknowledgment(t *testing.T) {
	groups, name, directory := newContainmentKernelFixture(t)
	if err := groups.Freeze(context.Background(), name); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "cgroup.freeze"))
	if err != nil || string(content) != "1\n" {
		t.Fatal("freeze did not address exactly the intended private property")
	}
	for _, check := range []struct {
		content string
		frozen  bool
		valid   bool
	}{
		{content: "populated 0\nfrozen 1\n", frozen: true, valid: true},
		{content: "populated 0\nfrozen 0\n", valid: true},
		{content: "populated 0\n"}, {content: "frozen 2\n"}, {content: "frozen 1\nfrozen 1\n"},
		{content: "frozen true\n"}, {content: "frozen -1\n"}, {content: "frozen 18446744073709551616\n"},
		{content: "frozen 1 extra\n"}, {content: strings.Repeat("x", 129)},
	} {
		if err := os.WriteFile(filepath.Join(directory, "cgroup.events"), []byte(check.content), 0600); err != nil {
			t.Fatal(err)
		}
		frozen, err := groups.Frozen(context.Background(), name)
		if (err == nil) != check.valid || frozen != check.frozen {
			t.Fatal("unknown kernel reply manufactured a freeze acknowledgment", check.content, frozen, err)
		}
	}
	absent := "urnetwork-submission-build-" + strings.Repeat("b", 32) + ".slice"
	if frozen, err := groups.Frozen(context.Background(), absent); frozen || err == nil {
		t.Fatal("missing parent was mistaken for a precommitted frozen target")
	}
}

func TestContainmentKernelFreezeRejectsSymlinkWithoutForeignWrites(t *testing.T) {
	groups, name, directory := newContainmentKernelFixture(t)
	foreign := filepath.Join(t.TempDir(), "foreign")
	if err := os.WriteFile(foreign, []byte("synthetic-untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	property := filepath.Join(directory, "cgroup.freeze")
	if err := os.Remove(property); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(foreign, property); err != nil {
		t.Fatal(err)
	}
	if err := groups.Freeze(context.Background(), name); err == nil {
		t.Fatal("freeze followed a replaced property")
	}
	if content, err := os.ReadFile(foreign); err != nil || string(content) != "synthetic-untouched" {
		t.Fatal("failed freeze changed a foreign file")
	}
}
