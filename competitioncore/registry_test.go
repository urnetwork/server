// Private registry files exercise atomic generation custody and proof ordering.
// No administrator command, service, sysctl or host registry is touched.
package competitioncore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testJoinVerifier struct {
	verify func(context.Context, Run, uint64) (joinProof, error)
}

func (self testJoinVerifier) Verify(ctx context.Context, run Run, generation uint64) (joinProof, error) {
	return self.verify(ctx, run, generation)
}

func newTestStore(t *testing.T) (*RegistryStore, string) {
	t.Helper()
	path := t.TempDir()
	root, err := openDirectory(path, false)
	if err != nil {
		t.Fatal(err)
	}
	store := &RegistryStore{root: root, settings: DefaultSettings(), boot: func(context.Context) (string, error) { return "11111111-2222-3333-4444-555555555555", nil }, join: nativeJoinVerifier{}}
	t.Cleanup(func() { store.Close() })
	if err := store.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store, path
}

// Exact components do not include a similarly prefixed sibling or parent.
func TestRegistryMatchingUsesCompleteComponentsNotStringPrefixes(t *testing.T) {
	registry := testRegistry(t)
	parent := registry.Runs[0].Cgroups[0]
	for _, check := range []struct {
		path   string
		scoped bool
	}{
		{path: parent, scoped: true}, {path: parent + "/synthetic-child.scope", scoped: true},
		{path: parent + "-foreign", scoped: false}, {path: filepath.Dir(parent), scoped: false},
		{path: "/system.slice/synthetic-ordinary.service", scoped: false}, {path: "/", scoped: false},
	} {
		got, err := scopedCgroup(registry, check.path, DefaultSettings())
		if err != nil || got != check.scoped {
			t.Fatal("core registry used a prefix/ancestor shortcut", check, got, err)
		}
	}
}

// Match the actual frozen evaluator ABI, not a plausible run-ID prefix:
// baseline/candidate/preflight use SHA256(job:attempt), live uses job UUID.
func TestRegistryDerivesActualJobAttemptAndLiveParentIdentities(t *testing.T) {
	registry := testRegistry(t)
	run := registry.Runs[0]
	token := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", run.JobId, run.Attempt))))[:32]
	baseline := sliceCgroup("urnetwork-evaluation-" + token + "-baseline-01.slice")
	live := sliceCgroup("urnetwork-evaluation-" + strings.ReplaceAll(run.JobId, "-", "") + "-live-01.slice")
	foundBaseline, foundLive := false, false
	for _, group := range run.Cgroups {
		foundBaseline = foundBaseline || group == baseline
		foundLive = foundLive || group == live
	}
	if !foundBaseline || !foundLive {
		t.Fatal("registry diverged from evaluator job/attempt cgroup names")
	}
	registry.Runs[0].Attempt++
	if ValidateRegistry(registry, DefaultSettings()) == nil {
		t.Fatal("old stage parents qualified a different job attempt")
	}
}

func TestRegistryExpiredDeadlineNeverDropsFrozenScope(t *testing.T) {
	registry := testRegistry(t)
	registry.Runs[0].StartedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	registry.Runs[0].DeadlineAt = registry.Runs[0].StartedAt.Add(3 * time.Hour)
	if scoped, err := scopedCgroup(registry, registry.Runs[0].Cgroups[0], DefaultSettings()); err != nil || !scoped {
		t.Fatal("elapsed deadline became scope-release authority", err)
	}
}

// Every run must have the complete fixed 31-parent inventory, even after its
// original job deadline; elapsed time is never permission to forget custody.
func TestRegistryRefusesPartialDuplicateOrAmbiguousRunInventories(t *testing.T) {
	settings := DefaultSettings()
	valid := testRegistry(t)
	if len(valid.Runs[0].Cgroups) != 31 || ValidateRegistry(valid, settings) != nil {
		t.Fatal("full frozen run inventory did not qualify")
	}
	for _, fault := range []string{"partial", "duplicate", "parent", "boot", "engine", "deadline", "generation"} {
		registry := testRegistry(t)
		switch fault {
		case "partial":
			registry.Runs[0].Cgroups = registry.Runs[0].Cgroups[:30]
		case "duplicate":
			registry.Runs = append(registry.Runs, registry.Runs[0])
		case "parent":
			registry.Runs[0].Cgroups[0] = "/urnetwork.slice"
		case "boot":
			registry.BootId = "invalid"
		case "engine":
			registry.EngineCgroups = registry.EngineCgroups[:3]
		case "deadline":
			registry.Runs[0].DeadlineAt = registry.Runs[0].DeadlineAt.Add(settings.MaximumJobLifetime)
		case "generation":
			registry.Generation = 0
		}
		if ValidateRegistry(registry, settings) == nil {
			t.Fatal("incomplete root authority became ordinary-process proof", fault)
		}
	}
}

// Registration commits exact original clocks, and stale/duplicate updates
// cannot replace them or grant a fresh dispatch attempt.
func TestRegistryAtomicRegistrationPreservesOriginalAttemptAndDeadline(t *testing.T) {
	store, _ := newTestStore(t)
	run := testRun(t)
	if err := store.Register(t.Context(), 1, run); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil || snapshot.Registry.Generation != 2 || len(snapshot.Registry.Runs) != 1 || !snapshot.Registry.Runs[0].DeadlineAt.Equal(run.DeadlineAt) {
		t.Fatal("durable registration changed original generation/clock", err)
	}
	run.DeadlineAt = run.DeadlineAt.Add(store.settings.MaximumJobLifetime)
	if store.Register(t.Context(), 1, run) == nil || store.Register(t.Context(), 2, run) == nil {
		t.Fatal("stale or duplicate registration replaced the original attempt")
	}
	after, err := store.Snapshot(t.Context())
	if err != nil || after.digest != snapshot.digest {
		t.Fatal("failed registration modified durable custody", err)
	}
	if err := store.Initialize(t.Context()); err == nil {
		t.Fatal("registry initialization overwrote active run custody")
	}
}

// No native implementation means no retirement, not an optimistic cleanup ACK.
func TestRegistryRetirementRemainsHeldWithoutConcreteNativeJoin(t *testing.T) {
	store, path := newTestStore(t)
	run := testRun(t)
	if err := store.Register(t.Context(), 1, run); err != nil {
		t.Fatal(err)
	}
	before, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Retire(t.Context(), 2, run.RunId); err == nil {
		t.Fatal("unimplemented helper join removed protected run scope")
	}
	after, err := store.Snapshot(t.Context())
	if err != nil || before.digest != after.digest {
		t.Fatal("unknown cleanup changed scope custody", err)
	}
	if _, err := os.Stat(filepath.Join(path, "retired-"+run.RunId+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unknown cleanup manufactured a retirement receipt")
	}
}

// The positive test backend is private and explicitly not native evidence.
func TestRegistryPositivePrivateJoinRetainsReceiptAndRejectsReplay(t *testing.T) {
	store, path := newTestStore(t)
	run := testRun(t)
	if err := store.Register(t.Context(), 1, run); err != nil {
		t.Fatal(err)
	}
	store.join = testJoinVerifier{verify: func(_ context.Context, run Run, generation uint64) (joinProof, error) {
		return joinProof{runId: run.RunId, planSha256: run.PlanSha256, generation: generation, proofSha256: strings.Repeat("e", 64)}, nil
	}}
	if err := store.Retire(t.Context(), 2, run.RunId); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(path, "retired-"+run.RunId+".json")); err != nil {
		t.Fatal("scope disappeared without retained private join receipt", err)
	}
	after, err := store.Snapshot(t.Context())
	if err != nil || after.Registry.Generation != 3 || len(after.Registry.Runs) != 0 {
		t.Fatal("positive private joined removal did not commit atomically", err)
	}
	if store.Register(t.Context(), 3, run) == nil {
		t.Fatal("retired run identity became a new attempt")
	}
}

// A nested writer succeeds while join is pending, proving no publication lock
// surrounds it. Its generation change then invalidates the old positive proof.
func TestRegistryJoinNeverHoldsWriterLockAndStaleProofCannotRetire(t *testing.T) {
	store, _ := newTestStore(t)
	run := testRun(t)
	if err := store.Register(t.Context(), 1, run); err != nil {
		t.Fatal(err)
	}
	store.join = testJoinVerifier{verify: func(ctx context.Context, run Run, generation uint64) (joinProof, error) {
		if err := store.mutate(ctx, generation, func(*Registry) error { return nil }); err != nil {
			t.Fatal("join held a writer/publication lock across its native boundary", err)
		}
		return joinProof{runId: run.RunId, planSha256: run.PlanSha256, generation: generation, proofSha256: strings.Repeat("f", 64)}, nil
	}}
	if err := store.Retire(t.Context(), 2, run.RunId); err == nil {
		t.Fatal("stale join receipt retired a newer registry generation")
	}
	after, err := store.Snapshot(t.Context())
	if err != nil || after.Registry.Generation != 3 || len(after.Registry.Runs) != 1 {
		t.Fatal("stale join proof lost active scope", err)
	}
}

// Malformed, case-folded and reboot-stale root metadata is unknown authority.
func TestRegistryRejectsDuplicateCaseFoldedAndWrongBootMetadata(t *testing.T) {
	store, path := newTestStore(t)
	content, err := os.ReadFile(filepath.Join(path, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"generation", "Generation", "GENERATION"} {
		malformed := strings.TrimSuffix(string(content), "}") + ",\"" + key + "\":1}"
		if strictDocument([]byte(malformed), &Registry{}, store.settings) == nil {
			t.Fatal("duplicate/case-folded registry authority was accepted", key)
		}
	}
	store.boot = func(context.Context) (string, error) { return "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", nil }
	if _, err := store.Snapshot(t.Context()); err == nil {
		t.Fatal("previous-boot registry became ordinary authority")
	}
}

// Bounded token/byte work prevents a protected-but-invalid document from
// turning classification into unbounded parsing or accepting unrelated fields.
func TestRegistryControlDocumentsHaveStrictFiniteGrammar(t *testing.T) {
	settings := DefaultSettings()
	content, err := json.Marshal(testRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{append(append([]byte(nil), content...), content...), []byte(`{"schema":1,"arbitrary_exec":"/bin/sh"}`), []byte(strings.Repeat("[", settings.DocumentDepth+1) + "0" + strings.Repeat("]", settings.DocumentDepth+1))} {
		if strictDocument(invalid, &Registry{}, settings) == nil {
			t.Fatal("unbounded or unknown registry grammar was admitted")
		}
	}
	settings.DocumentBytes = int64(len(content) - 1)
	if strictDocument(content, &Registry{}, settings) == nil {
		t.Fatal("registry control bytes exceeded their declared bound")
	}
}
