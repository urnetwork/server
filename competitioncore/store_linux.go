// Root-only atomic registry updates precede launch. A run cannot disappear
// until a private native join verifier has returned a positive bound receipt.
package competitioncore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"syscall"
)

// Native retirement remains deliberately impossible in this draft. It needs
// joined per-run parents, daemon custody and all core helpers, not Boolean input.
var ErrCleanupJoinUnimplemented = errors.New("native core-helper/daemon/run join implementation is missing")

// Only the package's verified native backend can construct this non-wire token.
type joinProof struct {
	runId, planSha256, proofSha256 string
	generation                     uint64
}

// Tests replace this private backend, never expose a caller-supplied proof flag.
type joinVerifier interface {
	Verify(context.Context, Run, uint64) (joinProof, error)
}

type nativeJoinVerifier struct{}

// No cleanup publication is safe until helper lifetime accounting is concrete.
func (nativeJoinVerifier) Verify(context.Context, Run, uint64) (joinProof, error) {
	return joinProof{}, ErrCleanupJoinUnimplemented
}

// A fixed root directory and root descriptor confine every journal operation.
// The process/file lock is held only during a bounded mutation, never joining.
type RegistryStore struct {
	root      *directory
	settings  Settings
	protected bool
	boot      func(context.Context) (string, error)
	join      joinVerifier
}

// Production opens only the reviewed fixed directory; it creates no directory
// and cannot choose a deployment path from a crash argument or environment.
func OpenRegistryStore(settings Settings) (*RegistryStore, error) {
	if os.Geteuid() != 0 || validateSettings(settings) != nil {
		return nil, errors.New("core registry updates require finite root ownership")
	}
	if err := protectedPath(RegistryDirectory, true); err != nil {
		return nil, err
	}
	root, err := openDirectory(RegistryDirectory, true)
	if err != nil {
		return nil, err
	}
	host := nativeHost{settings: settings}
	return &RegistryStore{root: root, settings: settings, protected: true, boot: host.BootId, join: nativeJoinVerifier{}}, nil
}

// Closing the root lease grants no permission to remove a run or thaw custody.
func (self *RegistryStore) Close() error {
	if self == nil || self.root == nil {
		return nil
	}
	return self.root.Close()
}

// Initialization uses the actual boot, complete fixed engine inventory and no
// candidate runs. Existing state is never overwritten or silently reinitialized.
func (self *RegistryStore) Initialize(ctx context.Context) error {
	return self.withLock(ctx, func() error {
		if _, err := self.root.Lstat("registry.json"); !errors.Is(err, os.ErrNotExist) {
			return errors.New("core registry already exists or is unavailable")
		}
		boot, err := self.boot(ctx)
		if err != nil {
			return err
		}
		registry := Registry{Schema: Schema, Generation: 1, BootId: boot, HandlerCgroup: HandlerCgroup, EngineCgroups: EngineCgroups(), Runs: []Run{}}
		return self.write(ctx, registry)
	})
}

// The frozen root plan, original clock and complete parents are durable before
// launch. Conflicts fail closed rather than refresh or replace an old attempt.
func (self *RegistryStore) Register(ctx context.Context, generation uint64, run Run) error {
	return self.mutate(ctx, generation, func(registry *Registry) error {
		if !validHex(run.RunId, 64) {
			return errors.New("core registry cannot register an invalid run identity")
		}
		if _, err := self.root.Lstat("retired-" + run.RunId + ".json"); !errors.Is(err, os.ErrNotExist) {
			return errors.New("core registry cannot replay a retired or uncertain attempt")
		}
		for _, existing := range registry.Runs {
			if existing.RunId == run.RunId {
				return errors.New("core run already has precommitted custody")
			}
		}
		registry.Runs = append(registry.Runs, run)
		return nil
	})
}

// Join happens outside the publication/file lock. The generation is compared
// again afterward; stale positive observations cannot remove current custody.
func (self *RegistryStore) Retire(ctx context.Context, generation uint64, runId string) error {
	before, err := self.Snapshot(ctx)
	if err != nil || before.Registry.Generation != generation || self.join == nil {
		return errors.New("core retirement lacks its exact current root generation")
	}
	var run *Run
	for index := range before.Registry.Runs {
		if before.Registry.Runs[index].RunId == runId {
			run = &before.Registry.Runs[index]
		}
	}
	if run == nil {
		return errors.New("core retirement cannot discover or remove an unrelated run")
	}
	proof, err := self.join.Verify(ctx, *run, generation)
	if err != nil || proof.runId != runId || proof.planSha256 != run.PlanSha256 || proof.generation != generation || !validHex(proof.proofSha256, 64) {
		return errors.New("core retirement has no positive helper/daemon/kernel join")
	}
	// Persist a bounded receipt before releasing scope. Its exact proof cannot
	// come from registration JSON; interrupted removal keeps the run covered.
	return self.withLock(ctx, func() error {
		current, err := self.Snapshot(ctx)
		if err != nil || current.Registry.Generation != generation || current.digest != before.digest {
			return errors.New("core registry changed during native join")
		}
		receipt := struct {
			Schema     int    `json:"schema"`
			Generation uint64 `json:"generation"`
			RunId      string `json:"run_id"`
			PlanSha256 string `json:"plan_sha256"`
			Proof      string `json:"join_proof_sha256"`
		}{Schema: Schema, Generation: generation, RunId: runId, PlanSha256: run.PlanSha256, Proof: proof.proofSha256}
		content, err := json.Marshal(receipt)
		if err != nil || self.atomic(ctx, "retired-"+runId+".json", content, false) != nil {
			return errors.New("core retirement proof could not be durably retained")
		}
		for index, existing := range current.Registry.Runs {
			if existing.RunId == runId {
				current.Registry.Runs = append(current.Registry.Runs[:index], current.Registry.Runs[index+1:]...)
				break
			}
		}
		if generation == math.MaxUint64 {
			return errors.New("core registry generation exhausted")
		}
		current.Registry.Generation++
		return self.write(ctx, current.Registry)
	})
}

// A snapshot revalidates protected bytes, the exact inventory and actual boot.
func (self *RegistryStore) Snapshot(ctx context.Context) (Snapshot, error) {
	if self == nil || self.root == nil || self.boot == nil || ctx.Err() != nil {
		return Snapshot{}, errors.New("core registry reader has no live root owner")
	}
	content, err := readRelative(self.root, "registry.json", self.settings.DocumentBytes, self.protected)
	if err != nil {
		return Snapshot{}, err
	}
	registry := Registry{}
	if strictDocument(content, &registry, self.settings) != nil || ValidateRegistry(registry, self.settings) != nil {
		return Snapshot{}, errors.New("core registry is corrupt, ambiguous or incomplete")
	}
	boot, err := self.boot(ctx)
	if err != nil || boot != registry.BootId || ctx.Err() != nil {
		return Snapshot{}, errors.New("core registry does not belong to the current kernel boot")
	}
	return Snapshot{Registry: registry, digest: digest(content)}, nil
}

// Compare-and-swap happens under the writer lease, without changing old clocks.
func (self *RegistryStore) mutate(ctx context.Context, generation uint64, change func(*Registry) error) error {
	return self.withLock(ctx, func() error {
		current, err := self.Snapshot(ctx)
		if err != nil || current.Registry.Generation != generation || generation == math.MaxUint64 {
			return errors.New("core registry generation is missing, stale or exhausted")
		}
		if err := change(&current.Registry); err != nil {
			return err
		}
		current.Registry.Generation++
		return self.write(ctx, current.Registry)
	})
}

// This nonblocking lock never makes a core helper wait on an administrator.
func (self *RegistryStore) withLock(ctx context.Context, change func() error) error {
	if self == nil || self.root == nil || validateSettings(self.settings) != nil || ctx.Err() != nil {
		return errors.New("core registry mutation has no bounded owner")
	}
	file, err := self.root.OpenFile("registry.lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("core registry writer lease is not private")
	}
	if self.protected {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return errors.New("core registry writer lease is not root-owned")
		}
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("core registry writer is already active")
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return change()
}

// Metadata must fit its bound and pass the same classifier admission grammar.
func (self *RegistryStore) write(ctx context.Context, registry Registry) error {
	if err := ValidateRegistry(registry, self.settings); err != nil {
		return err
	}
	content, err := json.Marshal(registry)
	if err != nil {
		return err
	}
	return self.atomic(ctx, "registry.json", content, true)
}

// Exclusive temporary bytes, file fsync, rename and directory fsync precede
// publication. The bounded journal inventory cannot grow without limit.
func (self *RegistryStore) atomic(ctx context.Context, name string, content []byte, replace bool) error {
	if int64(len(content)) > self.settings.DocumentBytes || ctx.Err() != nil {
		return errors.New("core registry publication is oversized or canceled")
	}
	directory, err := self.root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(self.settings.MaximumRuns*4 + 1)
	if err != nil || len(entries) > self.settings.MaximumRuns*4 {
		return errors.New("core registry journal inventory needs explicit bounded maintenance")
	}
	if !replace {
		if _, err := self.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			return errors.New("core custody receipt already exists or is unavailable")
		}
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporary := ".pending-" + hex.EncodeToString(random)
	file, err := self.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer self.root.Remove(temporary)
	written, writeErr := file.Write(content)
	syncErr, closeErr := file.Sync(), file.Close()
	if written != len(content) || writeErr != nil || syncErr != nil || closeErr != nil || ctx.Err() != nil {
		return errors.New("core registry bytes did not reach durable custody")
	}
	if err := self.root.Rename(temporary, name); err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("core registry directory custody is uncertain: %w", err)
	}
	return ctx.Err()
}
