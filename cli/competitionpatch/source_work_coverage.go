// The immutable host validator authenticates coverage from the actual raw
// image lock, then fences dedicated helpers independently of patch parsing.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// This is the raw image format, not its distinct canonical repository digest.
type repositoryWorkCoverageSourceLock struct {
	Schema              int                                  `json:"schema"`
	DevelopmentSnapshot bool                                 `json:"development_snapshot"`
	CandidatePathBase   string                               `json:"candidate_path_base,omitempty"`
	Repositories        map[string]string                    `json:"repositories"`
	WorkCoveragePolicy  *model.CompetitionWorkCoveragePolicy `json:"work_coverage_policy,omitempty"`
}

// Host preparation retains its exact raw lock outside the repository trees.
// Docker replays against the built-in lock, with no unchecked alternate path.
func repositoryWorkCoverageSourceLockPath(root string) string {
	if root == "/workspace" {
		return "/opt/urnetwork/source-lock.json"
	}
	return filepath.Join(root, ".evaluation-source-lock.json")
}

// Absence of a caller's marker is never sufficient to choose the legacy path.
// The original identity digest is checked by the caller before this boundary.
func authenticateRepositoryWorkCoverage(ctx context.Context, root string, identityBytes []byte, identity repositorySourceIdentity, policy controller.PatchPolicy) (*model.CompetitionWorkCoveragePolicy, error) {
	return authenticateRepositoryWorkCoverageWithReader(ctx, root, identityBytes, identity, policy, readRegularFile)
}

// Production always supplies the bounded regular-file reader above. The
// private seam tests built-in Docker paths without touching host-global paths.
func authenticateRepositoryWorkCoverageWithReader(ctx context.Context, root string, identityBytes []byte, identity repositorySourceIdentity, policy controller.PatchPolicy, readLock func(string, int64) ([]byte, error)) (result *model.CompetitionWorkCoveragePolicy, authErr error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result = nil
			authErr = errors.Join(err, authErr)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if readLock == nil {
		return nil, errors.New("authenticated raw image source lock reader is unavailable")
	}
	content, err := readLock(repositoryWorkCoverageSourceLockPath(root), maxPolicyBytes)
	if err != nil {
		return nil, errors.New("authenticated raw image source lock is unavailable")
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != identity.SourceLockSha256 {
		return nil, errors.New("authenticated raw image source lock digest differs")
	}
	var lock repositoryWorkCoverageSourceLock
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil || decoder.Decode(new(any)) != io.EOF || lock.Schema != 1 || lock.DevelopmentSnapshot {
		return nil, errors.New("authenticated raw image source lock is malformed")
	}
	coverage, err := model.CompetitionWorkCoveragePolicyFromSourceJson(content)
	if err != nil {
		return nil, errors.New("raw image source lock coverage is ambiguous")
	}
	preparedCoverage, err := model.CompetitionWorkCoveragePolicyFromSourceJson(identityBytes)
	if err != nil {
		return nil, errors.New("prepared source identity coverage is ambiguous")
	}
	equal := func(left, right *model.CompetitionWorkCoveragePolicy) bool {
		return left == nil && right == nil || left != nil && right != nil && *left == *right
	}
	if !maps.Equal(lock.Repositories, identity.Repositories) || lock.CandidatePathBase != identity.CandidatePathBase ||
		!equal(coverage, lock.WorkCoveragePolicy) || !equal(coverage, preparedCoverage) || !equal(coverage, identity.WorkCoveragePolicy) {
		return nil, errors.New("raw image source lock and prepared identity coverage or repositories differ")
	}
	if err := model.ValidateCompetitionWorkCoveragePatchPolicy(policy, coverage); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return coverage.Clone(), nil
}

// The values contain no mutable pointers, and each observation owns its map.
type repositoryWorkCoverageFile struct {
	mode string
	blob string
}

// Git is the caller's bounded, context-owned, filter-disabled native adapter.
// Every entry must agree across HEAD, the index, and no-filter physical bytes.
func repositoryWorkCoverageSnapshot(root string, coverage *model.CompetitionWorkCoveragePolicy, runGit func(string, ...string) (string, error)) (map[string]repositoryWorkCoverageFile, error) {
	paths, err := model.CompetitionWorkCoverageProtectedHelperPaths(coverage)
	if err != nil || coverage == nil {
		return nil, err
	}
	headArgs := append([]string{"--literal-pathspecs", "ls-tree", "-z", "--full-tree", "HEAD", "--"}, paths...)
	head, err := runGit("server", headArgs...)
	if err != nil {
		return nil, errors.New("coverage helper HEAD entries are unavailable")
	}
	indexArgs := append([]string{"--literal-pathspecs", "ls-files", "--stage", "-z", "--"}, paths...)
	index, err := runGit("server", indexArgs...)
	if err != nil {
		return nil, errors.New("coverage helper index entries are unavailable")
	}
	parse := func(content string, staged bool) (map[string]repositoryWorkCoverageFile, error) {
		entries := map[string]repositoryWorkCoverageFile{}
		for _, record := range strings.Split(content, "\x00") {
			if record == "" {
				continue
			}
			metadata, path, found := strings.Cut(record, "\t")
			fields := strings.Fields(metadata)
			if !found || len(fields) != 3 || fields[0] != "100644" && fields[0] != "100755" {
				return nil, errors.New("coverage helper is not a regular committed file")
			}
			blob := fields[2]
			if staged {
				blob = fields[1]
				if fields[2] != "0" {
					return nil, errors.New("coverage helper index has unresolved stages")
				}
			} else if fields[1] != "blob" {
				return nil, errors.New("coverage helper is not a Git blob")
			}
			if !gitShaPattern.MatchString(blob) {
				return nil, errors.New("coverage helper blob identity is invalid")
			}
			if _, duplicate := entries[path]; duplicate {
				return nil, errors.New("coverage helper entry is duplicated")
			}
			entries[path] = repositoryWorkCoverageFile{mode: fields[0], blob: blob}
		}
		return entries, nil
	}
	headEntries, headErr := parse(head, false)
	indexEntries, indexErr := parse(index, true)
	if headErr != nil || indexErr != nil || len(headEntries) != len(paths) || !maps.Equal(headEntries, indexEntries) {
		return nil, errors.New("coverage helper HEAD and index differ or are incomplete")
	}
	for _, path := range paths {
		entry, present := headEntries[path]
		if !present {
			return nil, errors.New("coverage helper HEAD path is missing")
		}
		current := filepath.Join(root, "server")
		parts := strings.Split(path, "/")
		for index, part := range parts {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("coverage helper path is absent or redirected")
			}
			if index != len(parts)-1 {
				if !info.IsDir() {
					return nil, errors.New("coverage helper parent is not a directory")
				}
			} else if !info.Mode().IsRegular() || (info.Mode().Perm()&0111 != 0) != (entry.mode == "100755") {
				return nil, errors.New("coverage helper working file mode differs")
			}
		}
	}
	hashArgs := append([]string{"hash-object", "--no-filters", "--"}, paths...)
	hashed, err := runGit("server", hashArgs...)
	worktreeBlobs := strings.Fields(hashed)
	if err != nil || len(worktreeBlobs) != len(paths) {
		return nil, errors.New("coverage helper working bytes are unavailable")
	}
	for index, path := range paths {
		if worktreeBlobs[index] != headEntries[path].blob {
			return nil, errors.New("coverage helper working bytes differ from HEAD")
		}
	}
	return headEntries, nil
}
