// Exact legacy-lock exception tests use synthetic temporary inode identities.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Synthetic exact identity passes without relaxing any recovery path checks.
func TestExactLegacyLockIdentityPassesAndRechecksWhileHeld(t *testing.T) {
	fixture := newRecoveryFixture(t)
	lock, err := openLegacyLock(fixture.Request.Config, fixture.Policy.ScoringLock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := recheckHeldLegacyLock(lock, fixture.Request.Config, fixture.Policy.ScoringLock); err != nil {
		t.Fatal(err)
	}
}

// Root qualification exercises a real synthetic uid-1000 inode, not a mocked
// ownership value. Only that temporary file is chowned; no live lock is touched.
func TestActualUid1000LegacyLockExceptionLeavesRootProtectionStrict(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root for the actual uid-1000 synthetic lock; mandatory root qualification must have zero skips")
	}
	fixture := newRecoveryFixture(t)
	config, policy := fixture.Request.Config, fixture.Policy.ScoringLock
	if err := os.Chown(config.ScoringLock.Path, 1000, -1); err != nil {
		t.Fatal(err)
	}
	config.ScoringLockIdentity.OwnerUid, policy.Identity.OwnerUid = 1000, 1000
	lock, err := openLegacyLock(config, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := recheckHeldLegacyLock(lock, config, policy); err != nil {
		t.Fatal(err)
	}
	if err := checkProtectedPath(config.ScoringLock.Path, fixture.Policy.Paths, false); err == nil {
		t.Fatal("legacy exception relaxed the ordinary root-owned path policy")
	}
	if err := checkProtectedPath(config.PublicPlan.Path, fixture.Policy.Paths, false); err != nil {
		t.Fatal("root-owned authority protection changed")
	}
}

// Matching config/policy still cannot substitute the wrong actual inode facts.
func TestLegacyLockRejectsWrongInodeDeviceUidModeAndPath(t *testing.T) {
	fixture := newRecoveryFixture(t)
	mutations := []func(*lockIdentity){
		func(identity *lockIdentity) { identity.Inode++ },
		func(identity *lockIdentity) { identity.Device++ },
		func(identity *lockIdentity) { identity.OwnerUid++ },
		func(identity *lockIdentity) { identity.Mode = 0640 },
	}
	for index, mutate := range mutations {
		config, policy := fixture.Request.Config, fixture.Policy.ScoringLock
		mutate(&config.ScoringLockIdentity)
		policy.Identity = config.ScoringLockIdentity
		if _, err := openLegacyLock(config, policy); err == nil {
			t.Errorf("wrong synthetic inode fact admitted: %d", index)
		}
	}
	config := fixture.Request.Config
	config.ScoringLock.Path = filepath.Join(fixture.Policy.Paths.AnchorPath, "another-synthetic.lock")
	if _, err := openLegacyLock(config, fixture.Policy.ScoringLock); err == nil {
		t.Fatal("wrong legacy lock path admitted")
	}
}

// A replaced pathname or symlink cannot move recovery onto a different lock.
func TestLegacyLockRejectsSymlinkAndReplacedPathWhileHeld(t *testing.T) {
	fixture := newRecoveryFixture(t)
	lock, err := openLegacyLock(fixture.Request.Config, fixture.Policy.ScoringLock)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	path := fixture.Request.Config.ScoringLock.Path
	retainedPath := path + ".synthetic-retained"
	if err := os.Rename(path, retainedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(retainedPath, path); err != nil {
		t.Fatal(err)
	}
	if _, err := openLegacyLock(fixture.Request.Config, fixture.Policy.ScoringLock); err == nil {
		t.Fatal("legacy symlink admitted")
	}
	if err := recheckHeldLegacyLock(lock, fixture.Request.Config, fixture.Policy.ScoringLock); err == nil {
		t.Fatal("changed legacy path admitted after open")
	}
}

// An otherwise idle synthetic process holding only a campaign fd is evidence.
// Disappeared descriptors are benign, whereas inaccessible descriptors refuse.
func TestRuntimeRejectsFdOnlyEvidenceAndUncertainDescriptor(t *testing.T) {
	fixture := newRecoveryFixture(t)
	procRoot := filepath.Join(fixture.Policy.Paths.AnchorPath, "synthetic-proc")
	writeSyntheticFile(t, filepath.Join(procRoot, "42", "cmdline"), []byte("synthetic-idle-process"))
	writeSyntheticFile(t, filepath.Join(procRoot, "42", "mountinfo"), nil)
	writeSyntheticFile(t, filepath.Join(procRoot, "self", "mountinfo"), nil)
	fdPath := filepath.Join(procRoot, "42", "fd", "10")
	if err := os.MkdirAll(filepath.Dir(fdPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fixture.Policy.CampaignRoot, "synthetic-open-file"), fdPath); err != nil {
		t.Fatal(err)
	}
	files := runtimeFiles{ReadFile: os.ReadFile, ReadDir: os.ReadDir, Readlink: os.Readlink}
	if err := checkRuntimeWithFiles(fixture.Request.Config, fixture.Settings, procRoot, 99, files); err == nil {
		t.Fatal("fd-only campaign process evidence admitted")
	}
	files.Readlink = func(path string) (string, error) {
		if path == fdPath {
			return "", os.ErrNotExist
		}
		return os.Readlink(path)
	}
	if err := checkRuntimeWithFiles(fixture.Request.Config, fixture.Settings, procRoot, 99, files); err != nil {
		t.Fatal("vanished descriptor was not treated as a completed race")
	}
	files.Readlink = func(path string) (string, error) {
		if path == fdPath {
			return "", os.ErrPermission
		}
		return os.Readlink(path)
	}
	if err := checkRuntimeWithFiles(fixture.Request.Config, fixture.Settings, procRoot, 99, files); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("inaccessible descriptor incorrectly asserted quiescent")
	}
}
