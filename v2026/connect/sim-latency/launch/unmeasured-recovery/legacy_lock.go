// The immutable native executor uses one legacy operator-owned scoring inode.
// This exact incident-reviewed exception never relaxes other protected paths.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Device/inode and ownership are approval-bound, not discovered substitutes.
type lockIdentity struct {
	OwnerUid uint32 `json:"owner_uid"`
	Device   uint64 `json:"device"`
	Inode    uint64 `json:"inode"`
	Mode     uint32 `json:"mode"`
}

// Only the compiled legacy lock's exact known ancestor classes are exempted.
type lockAncestor struct {
	Path     string
	OwnerUid uint32
	Mode     uint32
}

// Neither the config nor command-line flags can select a different inode/path.
type legacyLockPolicy struct {
	Path      string
	Identity  lockIdentity
	Ancestors []lockAncestor
}

// A zero-size, single-link regular inode is the historical empty scoring lock.
func matchesLockIdentity(info os.FileInfo, identity lockIdentity) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && info.Mode().Perm() == os.FileMode(identity.Mode) && info.Size() == 0 &&
		stat.Uid == identity.OwnerUid && uint64(stat.Dev) == identity.Device && stat.Ino == identity.Inode && stat.Nlink == 1
}

// Canonical no-symlink ancestry and the final inode are rechecked while flocked.
func validateLegacyLockPath(config recoveryConfig, policy legacyLockPolicy) (os.FileInfo, error) {
	if config.ScoringLock.Path != policy.Path || !filepath.IsAbs(policy.Path) || filepath.Clean(policy.Path) != policy.Path ||
		config.ScoringLockIdentity != policy.Identity || config.ScoringLock.Sha256 != digestBytes(nil) || policy.Identity.Mode != 0600 || len(policy.Ancestors) == 0 {
		return nil, errors.New("legacy scoring lock differs from its exact compiled path/identity")
	}
	for index, ancestor := range policy.Ancestors {
		if index > 0 && filepath.Dir(ancestor.Path) != policy.Ancestors[index-1].Path {
			return nil, errors.New("legacy lock ancestor census is incomplete")
		}
		info, err := os.Lstat(ancestor.Path)
		if err != nil {
			return nil, errors.New("legacy lock ancestor unavailable")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || stat.Uid != ancestor.OwnerUid || uint32(info.Mode().Perm()) != ancestor.Mode {
			return nil, errors.New("legacy lock ancestor class changed")
		}
	}
	if filepath.Dir(policy.Path) != policy.Ancestors[len(policy.Ancestors)-1].Path {
		return nil, errors.New("legacy lock parent is not the compiled ancestor")
	}
	info, err := os.Lstat(policy.Path)
	if err != nil || !matchesLockIdentity(info, policy.Identity) {
		return nil, errors.New("legacy scoring lock inode, owner, mode, or link count changed")
	}
	return info, nil
}

// The lock is never created, moved, chowned, chmodded, truncated, or replaced.
func openLegacyLock(config recoveryConfig, policy legacyLockPolicy) (*os.File, error) {
	before, err := validateLegacyLockPath(config, policy)
	if err != nil {
		return nil, err
	}
	descriptor, err := syscall.Open(policy.Path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("legacy scoring lock no-follow open refused")
	}
	file := os.NewFile(uintptr(descriptor), policy.Path)
	after, err := file.Stat()
	if err != nil || !matchesLockIdentity(after, policy.Identity) || !os.SameFile(before, after) {
		file.Close()
		return nil, errors.New("legacy scoring lock changed during open")
	}
	return file, nil
}

// Both descriptor and path must still name the exact historical flock inode.
func recheckHeldLegacyLock(file *os.File, config recoveryConfig, policy legacyLockPolicy) error {
	pathInfo, err := validateLegacyLockPath(config, policy)
	if err != nil {
		return err
	}
	descriptorInfo, err := file.Stat()
	if err != nil || !matchesLockIdentity(descriptorInfo, policy.Identity) || !os.SameFile(pathInfo, descriptorInfo) {
		return errors.New("flocked scoring descriptor no longer matches the canonical inode")
	}
	return nil
}
