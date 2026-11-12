//go:build linux

package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This owner composes the real serve lifecycle, sealed registry and typed PID1
// parser. Only process/manager/kernel I/O is synthetic; no native host is read.
type managementSshRetirementFixture struct {
	t             *testing.T
	config        Config
	policy        ManagementResourcePolicy
	settings      ManagementSshSettings
	host          *managementSshHost
	plan          managementSshPlan
	values        map[string]string
	waitRelease   chan struct{}
	waitJoined    atomic.Bool
	starts        int
	stops         int
	resets        int
	empties       int
	nextId        int
	joinCancel    context.CancelFunc
	afterStop     func()
	beforeObserve func()
	onReset       func() error
	onEmpty       func() (bool, error)
}

// A failed transient service remains loaded until the exact reset is admitted.
func newManagementSshRetirementFixture(t *testing.T) *managementSshRetirementFixture {
	t.Helper()
	config, policy, settings := managementSshTestConfig(t)
	self := &managementSshRetirementFixture{t: t, config: config, policy: policy, settings: settings, nextId: 1}
	self.host = managementSshTestHost(config, policy, settings)
	self.host.newId = func() (string, error) { id := fmt.Sprintf("%064x", self.nextId); self.nextId++; return id, nil }
	self.host.joinContext = func(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithTimeout(parent, timeout)
		self.joinCancel = cancel
		return ctx, cancel
	}
	self.host.start = func(_ context.Context, _ *Config, plan managementSshPlan, _ string, _ []*os.File) (managementSshProcess, error) {
		self.starts++
		self.plan = plan
		self.values = managementSshTestProjection(config, policy, plan, settings)
		self.values["SubState"], self.values["MainPID"] = "exited", "0"
		self.waitRelease = make(chan struct{})
		self.waitJoined.Store(false)
		return &managementSshTestProcess{wait: func() error { <-self.waitRelease; self.waitJoined.Store(true); return nil },
			kill: func() error { self.releaseWait(); return nil }}, nil
	}
	self.host.observe = func(_ context.Context, _ *Config, _ *ManagementResourcePolicy, _ Pin, plan managementSshPlan) (managementSshUnit, error) {
		if self.beforeObserve != nil {
			self.beforeObserve()
		}
		values := self.values
		if values == nil || values["Id"] != plan.UnitName {
			values = managementSshTestProjection(config, policy, plan, settings)
			managementSshRetirementMissing(values)
		}
		return validateManagementSshUnit(managementSshTestProjectionBytes(values), config, &policy, plan, settings)
	}
	self.host.stop = func(_ context.Context, manager Pin, unit string) error {
		if unit != self.plan.UnitName || manager.Path != "/usr/bin/systemctl" {
			t.Fatal("stop escaped the exact protected control scope")
		}
		self.stops++
		self.values["ActiveState"], self.values["SubState"], self.values["MainPID"], self.values["ControlGroup"] = "failed", "failed", "0", ""
		self.releaseWait()
		if self.afterStop != nil {
			self.afterStop()
		}
		return nil
	}
	self.host.empty = func(_ context.Context, unit string) (bool, error) {
		if unit != self.plan.UnitName {
			t.Fatal("native proof escaped the exact control nonce")
		}
		self.empties++
		if self.onEmpty != nil {
			return self.onEmpty()
		}
		return true, nil
	}
	self.host.resetFailed = func(_ context.Context, manager Pin, unit string) error {
		if unit != self.plan.UnitName || manager.Path != "/usr/bin/systemctl" || self.stops == 0 || !self.waitJoined.Load() || self.empties < 2 {
			t.Fatal("reset preceded exact stop, native proof or physical monitor join")
		}
		if _, err := os.Lstat(self.leaf("admit")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("reset retained late-child admission")
		}
		if _, err := os.Lstat(self.leaf("json")); err != nil {
			t.Fatal("loaded failed unit became unrecorded before manager retirement", err)
		}
		self.resets++
		if self.onReset != nil {
			return self.onReset()
		}
		managementSshRetirementMissing(self.values)
		return nil
	}
	return self
}

// Only the synthetic manager can collect its own unit object.
func managementSshRetirementMissing(values map[string]string) {
	values["LoadState"], values["ActiveState"], values["SubState"] = "not-found", "inactive", "dead"
	values["MainPID"], values["ControlPID"], values["Job"] = "0", "0", ""
	values["ControlGroup"], values["FragmentPath"], values["DropInPaths"] = "", "", ""
}

func (self *managementSshRetirementFixture) releaseWait() {
	select {
	case <-self.waitRelease:
	default:
		close(self.waitRelease)
	}
}

func (self *managementSshRetirementFixture) leaf(suffix string) string {
	return filepath.Join(self.config.StateDirectory, "managed-controls", self.plan.Id+"."+suffix)
}

func (self *managementSshRetirementFixture) serve(ctx context.Context) error {
	self.t.Helper()
	inRead, _, _, outWrite, _, diagnostic := managementSshTestPipes(self.t)
	return self.host.serve(ctx, self.config.configPath, inRead, outWrite, diagnostic)
}

// This is the registry input consumed by legacy closure: a failed loaded unit
// must still have its genuine original plan, not a synthesized registry entry.
func (self *managementSshRetirementFixture) assertRetained(err error) {
	self.t.Helper()
	if !errors.Is(err, ErrManagedSshUnknown) {
		self.t.Fatal("uncertain manager retirement became a positive join", err)
	}
	registry, openErr := openManagementSshRegistry(self.config, self.settings, false)
	if openErr != nil {
		self.t.Fatal(openErr)
	}
	defer registry.Close()
	content, readErr := registry.read(self.plan.Id + ".json")
	want, marshalErr := json.Marshal(self.plan)
	if readErr != nil || marshalErr != nil || string(content) != string(want) {
		self.t.Fatal("uncertain unit lost its exact original plan or session clock", readErr)
	}
	content, readErr = registry.read(self.plan.Id + ".closed")
	closed := managementSshClosed{}
	if readErr != nil || decodeStrict(content, &closed) != nil || closed.Schema != Schema || closed.Id != self.plan.Id || closed.Outcome != "unknown" ||
		closed.PlanSha256 != digestDockerBytes(want) || closed.InvocationId != strings.Repeat("e", 32) {
		self.t.Fatal("uncertain retirement lost its original closed authority", readErr)
	}
	if _, err := os.Lstat(self.leaf("admit")); !errors.Is(err, os.ErrNotExist) {
		self.t.Fatal("uncertain retirement kept admission for a late launcher")
	}
}

// On the old joined predicate this test deletes the plan with a failed PID1
// unit still loaded. The composed owner must instead retire PID1 first.
func TestManagementSshRetirementLoadedFailedRequiresPositiveManagerCollection(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	if err := fixture.serve(t.Context()); err != nil {
		t.Fatal("failed transient control was not positively retired", err)
	}
	if fixture.resets != 1 || fixture.values["LoadState"] != "not-found" || !fixture.waitJoined.Load() {
		t.Fatal("old inactive/empty/joined predicate forgot a loaded failed PID1 unit")
	}
	entries, err := os.ReadDir(filepath.Join(fixture.config.StateDirectory, "managed-controls"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "registry.lock" {
		t.Fatal("positively retired control metadata was not reaped", err)
	}
}

// A manager object already positively absent after stop needs no reset of a
// missing unit. Missing from the outset is still not a stop/join proof.
func TestManagementSshRetirementAlreadyCollectedSkipsReset(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.afterStop = func() { managementSshRetirementMissing(fixture.values) }
	if err := fixture.serve(t.Context()); err != nil || fixture.resets != 0 || !fixture.waitJoined.Load() {
		t.Fatal("already collected exact generation required resetting missing authority", err)
	}
}

// A returned reset failure retains the original plan and counts against the
// same finite registry capacity. Reconnection never reaps or replays it.
func TestManagementSshRetirementFailedResetRetainsBoundedUnknownRegistry(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.host.settings.MaximumControls = 1
	refusal := errors.New("synthetic exact reset acknowledgement lost")
	fixture.onReset = func() error { return refusal }
	err := fixture.serve(t.Context())
	fixture.assertRetained(err)
	if !errors.Is(err, refusal) || fixture.resets != 1 || fixture.values["LoadState"] != "loaded" {
		t.Fatal("failed reset did not retain the original manager failure", err)
	}
	if err := fixture.serve(t.Context()); err == nil || fixture.starts != 1 || fixture.resets != 1 {
		t.Fatal("retained uncertainty bypassed capacity or reissued a manager operation", err)
	}
}

// Even a nil late manager acknowledgement cannot publish positive retirement
// after the finite join owner has canceled. No fresh owner budget is created.
func TestManagementSshRetirementLateResetAckAfterCancelRetainsAuthority(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.onReset = func() error {
		managementSshRetirementMissing(fixture.values)
		fixture.joinCancel()
		return nil
	}
	err := fixture.serve(t.Context())
	fixture.assertRetained(err)
	if !errors.Is(err, context.Canceled) || fixture.resets != 1 {
		t.Fatal("late reset acknowledgement acquired a new cleanup lifetime", err)
	}
}

// A canceled join before reset cannot issue a manager operation even when the
// monitor already joined and every previously observed resource was empty.
func TestManagementSshRetirementCanceledBeforeResetRetainsAuthority(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.host.beforeJoinedReturn = func() { fixture.joinCancel() }
	err := fixture.serve(t.Context())
	fixture.assertRetained(err)
	if !errors.Is(err, context.Canceled) || fixture.resets != 0 {
		t.Fatal("canceled join issued a new manager operation", err)
	}
}

// A positive reset acknowledgement is not positive collection. The original
// invocation can remain loaded until the join owner expires; keep its plan.
func TestManagementSshRetirementResetAckWithoutCollectionRetainsAuthority(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.onReset = func() error { return nil }
	fixture.beforeObserve = func() {
		if fixture.resets != 0 {
			fixture.joinCancel()
		}
	}
	err := fixture.serve(t.Context())
	fixture.assertRetained(err)
	if fixture.resets != 1 || !errors.Is(err, context.Canceled) || fixture.values["LoadState"] != "loaded" {
		t.Fatal("reset acknowledgement alone erased a loaded failed unit", err)
	}
}

// No manager failure reset may be issued if a replacement generation appears
// between the first empty proof and the exact retirement authority readback.
func TestManagementSshRetirementGenerationChangeBeforeResetRetainsPlan(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.onEmpty = func() (bool, error) {
		if fixture.empties == 2 {
			fixture.values["InvocationID"] = strings.Repeat("f", 32)
		}
		return true, nil
	}
	err := fixture.serve(t.Context())
	fixture.assertRetained(err)
	if fixture.resets != 0 {
		t.Fatal("replacement manager generation inherited the old reset permission")
	}
}

// A replacement or late start after the reset cannot be mistaken for manager
// collection of the known stopped invocation.
func TestManagementSshRetirementGenerationChangeAfterResetRetainsPlan(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.onReset = func() error { fixture.values["InvocationID"] = strings.Repeat("f", 32); return nil }
	err := fixture.serve(t.Context())
	fixture.assertRetained(err)
	if fixture.resets != 1 {
		t.Fatal("exact original failure reset was not observed")
	}
}

// PID1's queued start/stop work has independent authority even with zero
// MainPID and an empty cgroup. It must not be forgotten or failure-reset.
func TestManagementSshRetirementPendingManagerJobRetainsPlan(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.afterStop = func() { fixture.values["Job"] = "17" }
	err := fixture.serve(t.Context())
	fixture.assertRetained(err)
	if fixture.resets != 0 {
		t.Fatal("pending PID1 work was cleared as a stale failure")
	}
}

// Empty cgroup evidence must stay true after PID1 collection too; omission or
// a new native task leaves the original authority available for recovery.
func TestManagementSshRetirementNativeWorkAfterResetRetainsPlan(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.onEmpty = func() (bool, error) { return fixture.resets == 0, nil }
	err := fixture.serve(t.Context())
	fixture.assertRetained(err)
	if fixture.resets != 1 {
		t.Fatal("native post-retirement proof was not exercised")
	}
}

// The final absence read brackets native emptiness. A late queued job cannot
// disappear from accounting merely because the preceding read said missing.
func TestManagementSshRetirementLateManagerWorkAfterNativeProofRetainsPlan(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.onEmpty = func() (bool, error) {
		if fixture.resets != 0 {
			fixture.values["LoadState"], fixture.values["Job"] = "loaded", "17"
			fixture.values["FragmentPath"] = "/run/systemd/transient/" + fixture.plan.UnitName
		}
		return true, nil
	}
	fixture.assertRetained(fixture.serve(t.Context()))
}

// Healthy sequential owners reuse only registry capacity, never a nonce,
// original session clock or cached permission from a prior closed receipt.
func TestManagementSshRetirementSequentialCollectionReapsWithinRegistryBound(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.host.settings.MaximumControls = 2
	for range 8 {
		if err := fixture.serve(t.Context()); err != nil {
			t.Fatal("positive exact retirement leaked finite registry capacity", err)
		}
		entries, err := os.ReadDir(filepath.Join(fixture.config.StateDirectory, "managed-controls"))
		if err != nil || len(entries) != 1 || entries[0].Name() != "registry.lock" {
			t.Fatal("sequential joined controls accumulated forgotten or orphan authority", err)
		}
	}
	if fixture.starts != 8 || fixture.stops != 8 || fixture.resets != 8 {
		t.Fatal("bounded collection replayed or skipped an exact owner")
	}
}

// No executable has been issued, but an uncertain initial manager observation
// still cannot erase the sole nonce record for a possibly loaded PID1 unit.
func TestManagementSshRetirementUncertainInitialObservationKeepsBoundedPlan(t *testing.T) {
	fixture := newManagementSshRetirementFixture(t)
	fixture.host.settings.MaximumControls = 1
	refusal := errors.New("synthetic initial manager readback unavailable")
	fixture.host.observe = func(context.Context, *Config, *ManagementResourcePolicy, Pin, managementSshPlan) (managementSshUnit, error) {
		return managementSshUnit{}, refusal
	}
	err := fixture.serve(t.Context())
	if !errors.Is(err, ErrManagedSshUnknown) || !errors.Is(err, refusal) || fixture.starts != 0 || fixture.stops != 0 || fixture.resets != 0 {
		t.Fatal("uncertain initial manager ownership became unrecorded or caused a mutation", err)
	}
	registry, err := openManagementSshRegistry(fixture.config, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	id := fmt.Sprintf("%064x", 1)
	content, err := registry.read(id + ".json")
	plan := managementSshPlan{}
	if err != nil || decodeStrict(content, &plan) != nil || plan.Id != id {
		t.Fatal("uncertain initial authority lost its original nonce plan", err)
	}
	content, err = registry.read(id + ".closed")
	closed := managementSshClosed{}
	if err != nil || decodeStrict(content, &closed) != nil || closed.Outcome != "unknown" || closed.InvocationId != "" {
		t.Fatal("unseen manager generation acquired a false joined receipt", err)
	}
	if err := fixture.serve(t.Context()); err == nil || fixture.starts != 0 {
		t.Fatal("initial manager uncertainty bypassed its retained registry bound", err)
	}
}
