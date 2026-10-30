// Protected file access and exact tree census never interpret private contents.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Tests inject a synthetic protected anchor; production is always root-owned /.
type pathPolicy struct {
	OwnerUid   uint32
	AnchorPath string
}

// Every component must be canonical, owned, non-symlink, and not writable by
// others. A missing target is permitted only for the final creation component.
func checkProtectedPath(path string, policy pathPolicy, missingLeaf bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !filepath.IsAbs(policy.AnchorPath) || filepath.Clean(policy.AnchorPath) != policy.AnchorPath {
		return errors.New("absolute canonical protected paths required")
	}
	relative, err := filepath.Rel(policy.AnchorPath, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return errors.New("path escapes protected anchor")
	}
	paths := []string{policy.AnchorPath}
	if relative != "." {
		for _, part := range strings.Split(relative, string(os.PathSeparator)) {
			paths = append(paths, filepath.Join(paths[len(paths)-1], part))
		}
	}
	for index, current := range paths {
		info, err := os.Lstat(current)
		if missingLeaf && index == len(paths)-1 && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("protected path unavailable: %s", current)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != policy.OwnerUid || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("unprotected path refused: %s", current)
		}
		if index != len(paths)-1 && !info.IsDir() {
			return errors.New("protected ancestor is not a directory")
		}
		if info.Mode().IsRegular() && stat.Nlink != 1 {
			return errors.New("multiply linked protected file refused")
		}
	}
	return nil
}

// The descriptor is checked against the lstat inode after a no-follow open.
func openProtected(path string, policy pathPolicy) (*os.File, error) {
	if err := checkProtectedPath(path, policy, false); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, errors.New("protected regular file required")
	}
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("protected file open refused")
	}
	file := os.NewFile(uintptr(descriptor), path)
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		file.Close()
		return nil, errors.New("protected file changed during open")
	}
	return file, nil
}

// A bounded opaque read cannot include an error's untrusted content in output.
func readPinned(pin filePin, policy pathPolicy, maximumBytes int64) ([]byte, error) {
	if !validDigest(pin.Sha256) || maximumBytes <= 0 {
		return nil, errors.New("valid protected file digest and positive read bound required")
	}
	file, err := openProtected(pin.Path, policy)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	if err != nil || int64(len(content)) > maximumBytes || digestBytes(content) != pin.Sha256 {
		return nil, fmt.Errorf("protected file digest or size mismatch: %s", pin.Path)
	}
	return content, nil
}

// Streaming hashes keep copied input and forensic memory independent of size.
func hashProtected(path string, policy pathPolicy, maximumBytes int64) (string, int64, error) {
	file, err := openProtected(path, policy)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || stat.Size() > maximumBytes || maximumBytes <= 0 {
		return "", 0, errors.New("protected file exceeds bounded forensic census")
	}
	digest := sha256.New()
	size, err := io.Copy(digest, io.LimitReader(file, maximumBytes+1))
	if err != nil || size != stat.Size() || size > maximumBytes {
		return "", 0, errors.New("protected file changed while hashing")
	}
	return hex.EncodeToString(digest.Sum(nil)), size, nil
}

// Directory sizes are normalized to zero; file order is lexical WalkDir order.
type treeEntry struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256,omitempty"`
}

// Only the observed setup failure leaves are eligible. A result or even one
// child in runs, score-output, or the unmounted runtime directory refuses recovery.
func failedTreeInventory(root string, policy pathPolicy, settings recoverySettings) ([]treeEntry, string, error) {
	allowedDirs := map[string]bool{
		".": true, "artifacts": true, "artifacts/.evidence-runtime": true,
		"artifacts/failed-evidence": true, "artifacts/failed-evidence/runs": true, "artifacts/failed-evidence/score-output": true,
	}
	allowedFiles := map[string]bool{
		"evaluator.stderr": true, "evaluator.stdout": true, "finalization.json": true, "started.json": true,
		"artifacts/canonical.patch": true, "artifacts/providers.yml": true, "artifacts/precommitted-request.json": true,
		"artifacts/worker-request.json": true, "artifacts/failed-evidence-manifest.json": true,
		"artifacts/failed-evidence/local-mounts.json": true, "artifacts/failed-evidence/evidence-quota.json": true,
		"artifacts/failed-evidence/docker-id-map.json": true, "artifacts/failed-evidence/policy.json": true,
		"artifacts/failed-evidence/builder-identity.json": true, "artifacts/failed-evidence/resource-boundary.json": true,
		"artifacts/failed-evidence/failure.json": true,
	}
	entries := []treeEntry{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("failed job tree unavailable")
		}
		if len(entries) >= settings.MaximumTreeEntries {
			return errors.New("failed job tree exceeds census bound")
		}
		if err := checkProtectedPath(path, policy, false); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := entry.Info()
		if err != nil {
			return errors.New("failed job census stat unavailable")
		}
		record := treeEntry{Path: relative, Mode: uint32(info.Mode().Perm())}
		if entry.IsDir() {
			if !allowedDirs[relative] {
				return fmt.Errorf("non-setup directory refused: %s", path)
			}
		} else {
			if !allowedFiles[relative] {
				return fmt.Errorf("measurement or unrecognized failed artifact refused: %s", path)
			}
			record.Sha256, record.Size, err = hashProtected(path, policy, settings.MaximumFileBytes)
			if err != nil {
				return err
			}
			if (relative == "evaluator.stderr" || relative == "evaluator.stdout") && record.Size != 0 {
				return errors.New("nonempty evaluator log needs separate review")
			}
		}
		entries = append(entries, record)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if len(entries) != len(allowedDirs)+len(allowedFiles) {
		return nil, "", errors.New("failed setup evidence census is incomplete")
	}
	content, err := json.Marshal(entries)
	if err != nil {
		return nil, "", err
	}
	return entries, digestBytes(content), nil
}

// Exact sorted leaf names prevent unjournaled runtime state in any other job.
func requireDirectoryEntries(path string, names []string, policy pathPolicy) error {
	if err := checkProtectedPath(path, policy, false); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != len(names) {
		return fmt.Errorf("unexpected directory census: %s", path)
	}
	for index, entry := range entries {
		if entry.Name() != names[index] {
			return fmt.Errorf("unexpected directory entry: %s", path)
		}
	}
	return nil
}

// O_EXCL and fsync retain partial files on every failure; no cleanup is attempted.
func writeExclusive(path string, content []byte, mode os.FileMode, policy pathPolicy) error {
	if err := checkProtectedPath(path, policy, true); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("exclusive file creation refused: %s", path)
	}
	_, writeErr := file.Write(content)
	err = errors.Join(writeErr, file.Chmod(mode), file.Sync(), file.Close())
	if err != nil {
		return errors.New("exclusive durable record incomplete; retain claim and inspect audit")
	}
	return syncDirectory(filepath.Dir(path))
}

// Directory fsync makes preceding entries and renames durable before advancing.
func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.New("cannot open directory for durable audit")
	}
	return errors.Join(file.Sync(), file.Close())
}

// Destinations are in a newly created protected directory. No overwrite path or
// rollback exists; if a step fails, the one-use claim remains permanently spent.
func renameRetained(source string, destination string, policy pathPolicy) error {
	if err := checkProtectedPath(source, policy, false); err != nil {
		return err
	}
	if err := checkProtectedPath(destination, policy, true); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("forensic rename destination already exists")
	}
	if err := os.Rename(source, destination); err != nil {
		return errors.New("forensic rename failed; retain claim and inspect audit")
	}
	return errors.Join(syncDirectory(filepath.Dir(source)), syncDirectory(filepath.Dir(destination)))
}

// Input copying is opaque and digest-checked before and after writing. Partial
// copies are intentionally retained and can never trigger an automatic retry.
func copyInputExclusive(source string, destination filePin, policy pathPolicy, maximumBytes int64) error {
	if err := checkProtectedPath(destination.Path, policy, true); err != nil {
		return err
	}
	file, err := openProtected(source, policy)
	if err != nil {
		return err
	}
	defer file.Close()
	output, err := os.OpenFile(destination.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("exclusive restoration input already exists or cannot be created")
	}
	digest := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(output, digest), io.LimitReader(file, maximumBytes+1))
	writeErr := errors.Join(copyErr, output.Sync(), output.Close())
	if writeErr != nil || size > maximumBytes || hex.EncodeToString(digest.Sum(nil)) != destination.Sha256 {
		return errors.New("restored opaque input mismatch; retain partial state")
	}
	return syncDirectory(filepath.Dir(destination.Path))
}
