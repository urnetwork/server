// Protected spooling retires only an authenticated attempt's local payload
// copies. One-shot claims and compact custody journals are never cleanup targets.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/server/controller"
	"golang.org/x/sys/unix"
)

// Limits are owned by the native spool, independently of provider success.
// Compact journals stay local; full capture documents are protected and transient.
type winnerHandoffSpoolLimits struct {
	MaximumEntries      int   `json:"maximum_entries"`       // Positive exact local census cap; default 48.
	MaximumCommitBytes  int64 `json:"maximum_commit_bytes"`  // Positive compact journal cap; default and approved maximum 1 MiB.
	MaximumCaptureBytes int64 `json:"maximum_capture_bytes"` // Positive full invocation capture cap; default and approved maximum 32 MiB.
	MaximumLeafBytes    int64 `json:"maximum_leaf_bytes"`    // Positive original object cap; default 128 MiB.
}

func defaultWinnerHandoffSpoolLimits() winnerHandoffSpoolLimits {
	return winnerHandoffSpoolLimits{MaximumEntries: 48, MaximumCommitBytes: 1024 * 1024, MaximumCaptureBytes: 32 * 1024 * 1024, MaximumLeafBytes: 128 * 1024 * 1024}
}

// The approved release, complete plan, resource census and exact invocation all
// enter the protected commit. No field grants generation or winner authority.
type winnerHandoffSpoolBinding struct {
	RunId              string `json:"run_id"`
	InvocationPosition int    `json:"invocation_position"`
	PlanSha256         string `json:"plan_sha256"`
	BundleSha256       string `json:"bundle_sha256"`
	ReleaseSha256      string `json:"release_sha256"`
	NativeDriverSha256 string `json:"native_driver_sha256"`
	CapacitySha256     string `json:"capacity_sha256"`
}

// These are original filesystem observations, not rehydrated ownership claims.
// Each regular entry maps to one exact-version original; FIFOs carry no payload.
type winnerHandoffSpoolEntry struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Original string `json:"original,omitempty"`
	Bytes    int64  `json:"bytes"`
	Sha256   string `json:"sha256,omitempty"`
	Device   uint64 `json:"device"`
	Inode    uint64 `json:"inode"`
	Uid      uint32 `json:"uid"`
	Gid      uint32 `json:"gid"`
	Mode     uint32 `json:"mode"`
	Links    uint64 `json:"links"`
	Modified int64  `json:"modified_unix_nanos"`
	Changed  int64  `json:"changed_unix_nanos"`
}

// The descriptor itself is archived with compliance retention and read back
// before cleanup. Original claims remain outside this attempt and are not listed.
type winnerHandoffSpoolCommit struct {
	Schema       int                            `json:"schema"`
	Kind         string                         `json:"kind"`
	Lifecycle    string                         `json:"lifecycle"`
	Binding      winnerHandoffSpoolBinding      `json:"binding"`
	AttemptRoot  string                         `json:"attempt_root"`
	Attempt      string                         `json:"attempt"`
	Invocation   winnerHandoffInvocationCustody `json:"invocation"`
	Physical     winnerHandoffCustodyOriginal   `json:"physical_authentication"`
	Extra        []winnerHandoffCustodyOriginal `json:"extra_originals"`
	Entries      []winnerHandoffSpoolEntry      `json:"entries"`
	MinimumUntil time.Time                      `json:"minimum_retention_until"`
	Limits       winnerHandoffSpoolLimits       `json:"limits"`
}

// A local immutable reference is fsynced after its protected commit readback.
// Losing it is an ambiguous custody boundary, never permission for another draw.
type winnerHandoffSpoolReference struct {
	Schema  int                          `json:"schema"`
	Kind    string                       `json:"kind"`
	Binding winnerHandoffSpoolBinding    `json:"binding"`
	Commit  winnerHandoffCustodyOriginal `json:"commit"`
}

// Only the authenticated loader constructs this in-memory cleanup capability.
// Every use rechecks protected bytes and actual local identities before unlink.
type winnerHandoffProtectedSpool struct {
	commit        winnerHandoffSpoolCommit
	reference     winnerHandoffSpoolReference
	referencePath string
}

// The archive reader must enforce scope, exact version, length, digest and
// protected retention. A path-only local reader is not a valid native adapter.
type winnerHandoffSpoolReader func(context.Context, winnerHandoffCustodyOriginal) ([]byte, error)

// Portable semantic verification consumes original protected bytes and the
// separately protected physical authentication, never recreated filesystem state.
type winnerHandoffSpoolVerifier func(winnerHandoffSpoolCommit, map[string][]byte) error

func validateWinnerHandoffSpoolBinding(value winnerHandoffSpoolBinding) error {
	if value.RunId == "" || len(value.RunId) > 128 || value.InvocationPosition <= 0 {
		return errors.New("spool has no fixed run or invocation identity")
	}
	for _, digest := range []string{value.PlanSha256, value.BundleSha256, value.ReleaseSha256, value.NativeDriverSha256, value.CapacitySha256} {
		if !validSha256(digest) {
			return errors.New("spool lacks its exact release, plan or capacity binding")
		}
	}
	return nil
}

// Root and mapped-member file observations are captured only after independent
// containment authentication. Unknown files, aliases and hardlinks refuse early.
func snapshotWinnerHandoffSpool(attemptRoot string, receipt *winnerMultipassContainmentReceipt, limits winnerHandoffSpoolLimits) ([]winnerHandoffSpoolEntry, error) {
	if receipt == nil || currentQgQualificationPrivateRoot(attemptRoot) != nil || filepath.Dir(receipt.AttemptDirectory) != attemptRoot || !strings.HasPrefix(filepath.Base(receipt.AttemptDirectory), "invocation.") ||
		currentQgQualificationPrivateRoot(receipt.AttemptDirectory) != nil || limits.MaximumEntries <= 0 || limits.MaximumEntries > defaultWinnerHandoffSpoolLimits().MaximumEntries {
		return nil, errors.New("spool can snapshot only one owned authenticated attempt")
	}
	originals := map[string]currentQgQualificationBinding{}
	for _, binding := range receipt.Artifacts {
		if filepath.Base(binding.Path) != binding.Path || binding.Path == "." || binding.Path == "" || originals[binding.Path].Path != "" || !validSha256(binding.Sha256) || binding.Bytes < 0 || binding.Bytes > limits.MaximumLeafBytes {
			return nil, errors.New("spool received an ambiguous authenticated original census")
		}
		originals[binding.Path] = binding
	}
	entries := []winnerHandoffSpoolEntry{}
	var visit func(string) error
	visit = func(relative string) error {
		path := receipt.AttemptDirectory
		if relative != "." {
			path = filepath.Join(path, relative)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSymlink != 0 || len(entries) >= limits.MaximumEntries {
			return errors.New("spool local census has an alias or exceeds its bound")
		}
		entry := winnerHandoffSpoolEntry{Path: relative, Bytes: info.Size(), Device: uint64(stat.Dev), Inode: stat.Ino, Uid: stat.Uid, Gid: stat.Gid, Mode: uint32(info.Mode()), Links: uint64(stat.Nlink), Modified: stat.Mtim.Nano(), Changed: stat.Ctim.Nano()}
		if info.IsDir() {
			if relative != "." && relative != "member-state" || info.Mode().Perm() != 0700 || relative == "." && stat.Uid != uint32(os.Geteuid()) {
				return errors.New("spool refuses an unknown directory or changed owner")
			}
			entry.Kind, entry.Bytes = "directory", 0
			entries = append(entries, entry)
			children, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			for _, child := range children {
				if err := visit(filepath.Join(relative, child.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		if stat.Nlink != 1 || info.Mode().Perm() != 0600 {
			return errors.New("spool local payload changed its private mode or link count")
		}
		if info.Mode()&os.ModeNamedPipe != 0 {
			if !slices.Contains([]string{"stdin.pipe", "stdout.pipe", "stderr.pipe"}, relative) || stat.Uid != uint32(os.Geteuid()) || info.Size() != 0 {
				return errors.New("spool refuses an unknown non-payload FIFO")
			}
			entry.Kind = "fifo"
		} else if info.Mode().IsRegular() {
			name := relative
			if strings.HasPrefix(relative, "member-state/") {
				name = strings.TrimPrefix(relative, "member-state/")
				if !slices.Contains([]string{"call.started.json", "call.complete.json", "tokenize.request.json", "tokenize.response.json", "transport.response"}, name) {
					return errors.New("spool refuses an unknown member-state leaf")
				}
			} else if stat.Uid != uint32(os.Geteuid()) {
				return errors.New("spool captured original is not owned by the native process")
			}
			original, found := originals[name]
			if !found || info.Size() != original.Bytes {
				return errors.New("spool local file is not an authenticated original")
			}
			content, err := readWinnerHandoffSpoolFile(path, original.Bytes)
			if err != nil || winnerMultipassSha256(content) != original.Sha256 {
				return errors.New("spool original changed after physical authentication")
			}
			entry.Kind, entry.Original, entry.Sha256 = "file", name, original.Sha256
		} else {
			return errors.New("spool refuses an unknown local file type")
		}
		entries = append(entries, entry)
		return nil
	}
	if err := visit("."); err != nil {
		return nil, err
	}
	for name := range originals {
		if !slices.ContainsFunc(entries, func(entry winnerHandoffSpoolEntry) bool { return entry.Path == name && entry.Kind == "file" }) {
			return nil, errors.New("spool original local census is incomplete")
		}
	}
	slices.SortFunc(entries, func(a, b winnerHandoffSpoolEntry) int { return strings.Compare(a.Path, b.Path) })
	return entries, nil
}

// No-follow plus an exact length cap avoids opening a FIFO or following a leaf
// replacement while checking a payload. Ancestors are separately pinned private.
func readWinnerHandoffSpoolFile(path string, length int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != length || length < 0 {
		return nil, errors.New("spool payload is not the exact regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, length+1))
	if err != nil || int64(len(content)) != length {
		return nil, errors.New("spool original length changed while reading")
	}
	return content, nil
}

func validateWinnerHandoffSpoolCommit(commit winnerHandoffSpoolCommit, binding winnerHandoffSpoolBinding, attemptRoot string) error {
	defaults := defaultWinnerHandoffSpoolLimits()
	if validateWinnerHandoffSpoolBinding(binding) != nil || commit.Binding != binding || commit.Schema != 1 || commit.Kind != "winner-review-protected-spool-commit/v1" || commit.Lifecycle != "protected-exact-version-spool/v1" ||
		commit.AttemptRoot != attemptRoot || !filepath.IsAbs(attemptRoot) || filepath.Clean(attemptRoot) != attemptRoot || filepath.Dir(commit.Attempt) != attemptRoot || !strings.HasPrefix(filepath.Base(commit.Attempt), "invocation.") ||
		commit.MinimumUntil.IsZero() || commit.Limits.MaximumEntries <= 0 || commit.Limits.MaximumEntries > defaults.MaximumEntries || commit.Limits.MaximumCommitBytes <= 0 || commit.Limits.MaximumCommitBytes > defaults.MaximumCommitBytes ||
		commit.Limits.MaximumCaptureBytes <= 0 || commit.Limits.MaximumCaptureBytes > defaults.MaximumCaptureBytes || commit.Limits.MaximumLeafBytes <= 0 || commit.Limits.MaximumLeafBytes > defaults.MaximumLeafBytes ||
		len(commit.Entries) == 0 || len(commit.Entries) > commit.Limits.MaximumEntries || len(commit.Extra) > 2 || len(commit.Invocation.Originals) == 0 || len(commit.Invocation.Originals) > defaults.MaximumEntries ||
		commit.Physical.Name != "physical-authentication.json" || !validSha256(commit.Invocation.ManifestSha256) || !validSha256(commit.Invocation.WrapperReceiptSha256) || !validSha256(commit.Invocation.RequestSha256) {
		return errors.New("protected spool commit is not the approved bounded invocation")
	}
	originals := map[string]winnerHandoffCustodyOriginal{}
	for _, original := range append(append(slices.Clone(commit.Invocation.Originals), commit.Physical), commit.Extra...) {
		if validateWinnerHandoffOriginal(original, commit.MinimumUntil, commit.Limits.MaximumLeafBytes) != nil || originals[original.Name].Name != "" {
			return errors.New("protected spool commit has an invalid or repeated original")
		}
		if original.Name == "invocation-capture.json" && original.Bytes > commit.Limits.MaximumCaptureBytes || original.Name == "physical-authentication.json" && original.Bytes > commit.Limits.MaximumCommitBytes {
			return errors.New("protected invocation capture or physical proof exceeds its separate cap")
		}
		originals[original.Name] = original
	}
	if originals["evidence-manifest.json"].Retention.Sha256 != commit.Invocation.ManifestSha256 || originals["wrapper-receipt.json"].Retention.Sha256 != commit.Invocation.WrapperReceiptSha256 {
		return errors.New("protected spool commit lost its exact original manifest or receipt")
	}
	seen := map[string]bool{}
	for _, entry := range commit.Entries {
		if seen[entry.Path] || filepath.IsAbs(entry.Path) || filepath.Clean(entry.Path) != entry.Path || strings.HasPrefix(entry.Path, "../") || entry.Inode == 0 {
			return errors.New("protected spool census has an alias or duplicate")
		}
		seen[entry.Path] = true
		switch entry.Kind {
		case "directory":
			if entry.Path != "." && entry.Path != "member-state" || os.FileMode(entry.Mode) != os.ModeDir|0700 || entry.Bytes != 0 || entry.Original != "" || entry.Sha256 != "" {
				return errors.New("protected spool directory census is malformed")
			}
		case "fifo":
			if !slices.Contains([]string{"stdin.pipe", "stdout.pipe", "stderr.pipe"}, entry.Path) || os.FileMode(entry.Mode) != os.ModeNamedPipe|0600 || entry.Bytes != 0 || entry.Links != 1 || entry.Original != "" || entry.Sha256 != "" {
				return errors.New("protected spool FIFO census is malformed")
			}
		case "file":
			original, found := originals[entry.Original]
			if !found || entry.Original == "physical-authentication.json" || entry.Original == "wrapper-receipt.json" || entry.Original == "invocation-capture.json" ||
				entry.Path != entry.Original && entry.Path != filepath.Join("member-state", entry.Original) || entry.Bytes != original.Bytes || entry.Sha256 != original.Retention.Sha256 || entry.Links != 1 || os.FileMode(entry.Mode) != 0600 {
				return errors.New("protected spool payload is not its exact archived original")
			}
		default:
			return errors.New("protected spool census has an unknown type")
		}
	}
	if !seen["."] || !seen["member-state"] {
		return errors.New("protected spool lacks its exact two directory identities")
	}
	content, err := json.Marshal(commit)
	if err != nil || int64(len(content)) > commit.Limits.MaximumCommitBytes {
		return errors.New("protected spool compact commit exceeds its predeclared bound")
	}
	return nil
}

// Every original is fetched again by exact protected version, not just believed
// because an upload returned a digest. The verifier independently replays it.
func replayWinnerHandoffSpool(ctx context.Context, commit winnerHandoffSpoolCommit, read winnerHandoffSpoolReader, verify winnerHandoffSpoolVerifier) (map[string][]byte, error) {
	if read == nil || verify == nil {
		return nil, errors.New("protected spool has no exact-version reader or semantic verifier")
	}
	originals := map[string][]byte{}
	for _, original := range append(append(slices.Clone(commit.Invocation.Originals), commit.Physical), commit.Extra...) {
		content, err := read(ctx, original)
		if err != nil || int64(len(content)) != original.Bytes || winnerMultipassSha256(content) != original.Retention.Sha256 {
			return nil, errors.New("protected spool original exact-version readback failed")
		}
		originals[original.Name] = content
	}
	if err := verify(commit, originals); err != nil {
		return nil, err
	}
	return originals, nil
}

// This durable two-file journal has no success overwrite. A crash before the
// reference is fsynced leaves local originals intact and cannot authorize reuse.
func commitWinnerHandoffSpool(ctx context.Context, directory string, commit winnerHandoffSpoolCommit, retain func(context.Context, []byte, string) (*controller.CompetitionWinnerReviewEvidence, error), read winnerHandoffSpoolReader, verify winnerHandoffSpoolVerifier) (*winnerHandoffProtectedSpool, error) {
	if currentQgQualificationPrivateRoot(directory) != nil || validateWinnerHandoffSpoolCommit(commit, commit.Binding, commit.AttemptRoot) != nil || retain == nil {
		return nil, errors.New("protected spool commit lacks a private journal or exact binding")
	}
	if _, err := replayWinnerHandoffSpool(ctx, commit, read, verify); err != nil {
		return nil, err
	}
	if err := checkWinnerHandoffSpoolLocal(commit, false); err != nil {
		return nil, err
	}
	content, _ := json.Marshal(commit)
	name := fmt.Sprintf("invocation-%06d", commit.Binding.InvocationPosition)
	if err := writePrivateReviewFile(filepath.Join(directory, name+".commit.json"), content); err != nil {
		return nil, err
	}
	if err := syncWinnerHandoffSpoolDirectory(directory); err != nil {
		return nil, err
	}
	original, err := retainWinnerHandoffDocument(ctx, "spool-commit.json", content, int(commit.Limits.MaximumCommitBytes), commit.MinimumUntil, retain)
	if err != nil {
		return nil, err
	}
	stored, err := read(ctx, original)
	if err != nil || !bytes.Equal(stored, content) {
		return nil, errors.New("spool commit exact-version readback failed; local originals retained")
	}
	reference := winnerHandoffSpoolReference{Schema: 1, Kind: "winner-review-protected-spool-reference/v1", Binding: commit.Binding, Commit: original}
	referenceBytes, _ := json.Marshal(reference)
	if int64(len(content)+len(referenceBytes)) > commit.Limits.MaximumCommitBytes {
		return nil, errors.New("combined local compact commit and reference exceed their single invocation allowance")
	}
	path := filepath.Join(directory, name+".retention.json")
	if err := writePrivateReviewFile(path, referenceBytes); err != nil {
		return nil, err
	}
	if err := syncWinnerHandoffSpoolDirectory(directory); err != nil {
		return nil, err
	}
	return &winnerHandoffProtectedSpool{commit: commit, reference: reference, referencePath: path}, nil
}

// Reopening a compact reference cannot manufacture a physical observation: its
// exact protected proof and all original bytes must pass the portable verifier.
func loadWinnerHandoffSpool(ctx context.Context, referencePath string, binding winnerHandoffSpoolBinding, attemptRoot string, read winnerHandoffSpoolReader, verify winnerHandoffSpoolVerifier) (*winnerHandoffProtectedSpool, error) {
	if currentQgQualificationPrivateRoot(filepath.Dir(referencePath)) != nil || filepath.Base(referencePath) != fmt.Sprintf("invocation-%06d.retention.json", binding.InvocationPosition) {
		return nil, errors.New("spool reference is not in its immutable private journal")
	}
	info, err := os.Lstat(referencePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() > defaultWinnerHandoffSpoolLimits().MaximumCommitBytes || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, errors.New("spool reference is absent, changed or ambiguous")
	}
	content, err := readWinnerHandoffSpoolFile(referencePath, info.Size())
	if err != nil {
		return nil, err
	}
	var reference winnerHandoffSpoolReference
	if rejectWinnerReviewCaseAliases(content) != nil || decodeStrictJSONBytes(content, &reference, "protected spool reference") != nil || reference.Schema != 1 || reference.Kind != "winner-review-protected-spool-reference/v1" || reference.Binding != binding ||
		reference.Commit.Name != "spool-commit.json" || reference.Commit.Bytes > defaultWinnerHandoffSpoolLimits().MaximumCommitBytes || read == nil {
		return nil, errors.New("spool reference changed its exact invocation binding")
	}
	protected, err := read(ctx, reference.Commit)
	if err != nil || int64(len(protected)) != reference.Commit.Bytes || winnerMultipassSha256(protected) != reference.Commit.Retention.Sha256 {
		return nil, errors.New("spool protected commit unavailable by exact original version")
	}
	var commit winnerHandoffSpoolCommit
	if rejectWinnerReviewCaseAliases(protected) != nil || decodeStrictJSONBytes(protected, &commit, "protected spool commit") != nil || validateWinnerHandoffSpoolCommit(commit, binding, attemptRoot) != nil ||
		validateWinnerHandoffOriginal(reference.Commit, commit.MinimumUntil, commit.Limits.MaximumCommitBytes) != nil {
		return nil, errors.New("spool protected commit no longer validates")
	}
	localPath := strings.TrimSuffix(referencePath, ".retention.json") + ".commit.json"
	local, err := readWinnerHandoffSpoolFile(localPath, int64(len(protected)))
	if err != nil || !bytes.Equal(local, protected) {
		return nil, errors.New("spool original local commit differs from its protected version")
	}
	if _, err := replayWinnerHandoffSpool(ctx, commit, read, verify); err != nil {
		return nil, err
	}
	return &winnerHandoffProtectedSpool{commit: commit, reference: reference, referencePath: referencePath}, nil
}

// Missing expected entries are allowed only after a protected commit, for
// idempotent recovery from interrupted cleanup. Unknown or changed entries refuse
// the entire pass before the first unlink; this never executes a model call.
func checkWinnerHandoffSpoolLocal(commit winnerHandoffSpoolCommit, allowMissing bool) error {
	if currentQgQualificationPrivateRoot(commit.AttemptRoot) != nil {
		return errors.New("spool attempt parent changed its private identity")
	}
	expected := map[string]winnerHandoffSpoolEntry{}
	for _, entry := range commit.Entries {
		expected[entry.Path] = entry
	}
	seen := map[string]bool{}
	var visit func(string) error
	visit = func(relative string) error {
		path := commit.Attempt
		if relative != "." {
			path = filepath.Join(path, relative)
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && allowMissing {
			return nil
		}
		if err != nil {
			return err
		}
		entry, found := expected[relative]
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !found || !ok || stat.Ino != entry.Inode || uint64(stat.Dev) != entry.Device || stat.Uid != entry.Uid || stat.Gid != entry.Gid || uint32(info.Mode()) != entry.Mode ||
			!info.IsDir() && (info.Size() != entry.Bytes || uint64(stat.Nlink) != entry.Links || stat.Mtim.Nano() != entry.Modified || stat.Ctim.Nano() != entry.Changed) {
			return errors.New("spool refuses cleanup of an unknown or changed local entry")
		}
		seen[relative] = true
		if info.IsDir() {
			children, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			for _, child := range children {
				if err := visit(filepath.Join(relative, child.Name())); err != nil {
					return err
				}
			}
		} else if info.Mode().IsRegular() {
			content, err := readWinnerHandoffSpoolFile(path, entry.Bytes)
			if err != nil || winnerMultipassSha256(content) != entry.Sha256 {
				return errors.New("spool refuses cleanup of changed original bytes")
			}
		}
		return nil
	}
	if err := visit("."); err != nil {
		return err
	}
	if !allowMissing && len(seen) != len(expected) {
		return errors.New("spool original local census is incomplete before protected commit")
	}
	return nil
}

// Leaf removal uses pinned directory descriptors and a second no-follow inode
// check. The caller may inject an unlink failure in deterministic lifecycle tests.
func evictWinnerHandoffSpool(ctx context.Context, spool *winnerHandoffProtectedSpool, read winnerHandoffSpoolReader, verify winnerHandoffSpoolVerifier, unlink func(int, string, int) error) error {
	if spool == nil || unlink == nil {
		return errors.New("spool cleanup lacks its protected capability")
	}
	loaded, err := loadWinnerHandoffSpool(ctx, spool.referencePath, spool.commit.Binding, spool.commit.AttemptRoot, read, verify)
	if err != nil || !reflect.DeepEqual(loaded.commit, spool.commit) {
		return errors.New("spool protected capability changed before cleanup")
	}
	commit := loaded.commit
	if err := checkWinnerHandoffSpoolLocal(commit, true); err != nil {
		return err
	}
	if _, err := os.Lstat(commit.Attempt); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	rootFd, err := unix.Open(commit.AttemptRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(rootFd)
	attemptFd, err := unix.Openat(rootFd, filepath.Base(commit.Attempt), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(attemptFd)
	checkDirectory := func(fd int, path string) error {
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			return err
		}
		for _, entry := range commit.Entries {
			if entry.Path == path && stat.Ino == entry.Inode && uint64(stat.Dev) == entry.Device && stat.Uid == entry.Uid && stat.Gid == entry.Gid && stat.Mode == unix.S_IFDIR|0700 {
				return nil
			}
		}
		return errors.New("spool directory identity changed before descriptor-relative cleanup")
	}
	if err := checkDirectory(attemptFd, "."); err != nil {
		return err
	}
	memberFd, memberErr := unix.Openat(attemptFd, "member-state", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if memberErr == nil {
		defer unix.Close(memberFd)
		if err := checkDirectory(memberFd, "member-state"); err != nil {
			return err
		}
	} else if !errors.Is(memberErr, unix.ENOENT) {
		return memberErr
	}
	entries := slices.Clone(commit.Entries)
	slices.SortFunc(entries, func(a, b winnerHandoffSpoolEntry) int {
		if a.Kind == "directory" && b.Kind != "directory" {
			return 1
		}
		if a.Kind != "directory" && b.Kind == "directory" {
			return -1
		}
		return strings.Compare(b.Path, a.Path)
	})
	for _, entry := range entries {
		fd, name, flags := attemptFd, entry.Path, 0
		if entry.Path == "." {
			fd, name, flags = rootFd, filepath.Base(commit.Attempt), unix.AT_REMOVEDIR
		} else if entry.Path == "member-state" {
			flags = unix.AT_REMOVEDIR
		} else if strings.HasPrefix(entry.Path, "member-state/") {
			if memberErr != nil {
				continue
			}
			fd, name = memberFd, filepath.Base(entry.Path)
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
			continue
		} else if err != nil {
			return err
		}
		kindMode := map[string]uint32{"directory": unix.S_IFDIR | 0700, "file": unix.S_IFREG | 0600, "fifo": unix.S_IFIFO | 0600}[entry.Kind]
		if stat.Ino != entry.Inode || uint64(stat.Dev) != entry.Device || stat.Uid != entry.Uid || stat.Gid != entry.Gid || stat.Mode != kindMode ||
			entry.Kind != "directory" && (uint64(stat.Nlink) != entry.Links || stat.Size != entry.Bytes || stat.Mtim.Nano() != entry.Modified || stat.Ctim.Nano() != entry.Changed) {
			return errors.New("spool entry changed at the exact cleanup boundary")
		}
		if err := unlink(fd, name, flags); err != nil {
			return err
		}
		if err := unix.Fsync(fd); err != nil {
			return err
		}
	}
	return nil
}

func syncWinnerHandoffSpoolDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
