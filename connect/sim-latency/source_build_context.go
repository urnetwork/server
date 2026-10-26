// Build contexts come from clean committed objects, never directory copies of
// operator checkouts. Ignored files, ambient credentials and mutable Git state
// must not cross the isolated compilation boundary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/urnetwork/server/model"
)

// Read-only input identities are separate from later build/test/host receipts.
type sourceBuildContext struct {
	Directory       string
	SourceDirectory string
	ManifestPath    string
	LockPath        string
}

// A public informational origin replaces local paths and credentials. This does
// not contact the remote or grant it authority over the approved commit map.
func sourceBuildPublicOrigin(repository string) string {
	if repository == "sn" {
		return "https://github.com/urfoundation/sn.git"
	}
	return "https://github.com/urnetwork/" + repository + ".git"
}

// Verify both ends of a local Git transport. No ignored files, alternate-object
// links, source configuration or hooks are copied into the clean checkout.
func cloneSourceBuildRepository(ctx context.Context, repository, source, destination, expectedCommit string) error {
	if !slices.Contains(sourceRepositoryNames(), repository) || !sourceGitShaPattern.MatchString(expectedCommit) {
		return errors.New("source build repository identity is invalid")
	}
	head, err := sourceGitText(ctx, source, "rev-parse", "HEAD")
	if err != nil || head != expectedCommit {
		return errors.New("source build input differs from approved commit")
	}
	status, err := sourceGitText(ctx, source, "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return errors.New("source build input is dirty or unreadable")
	}
	parent := filepath.Dir(destination)
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("source build clone destination already exists or is unreadable")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	// Fetch one committed tree through Git transport. Full clone would ask a
	// partial source for unrelated history and may trigger remote lazy fetches.
	if _, err := sourceGitText(ctx, parent, "init", "--quiet", "--template=", "--", destination); err != nil {
		return err
	}
	if _, err := sourceGitText(ctx, destination, "remote", "add", "origin", sourceBuildPublicOrigin(repository)); err != nil {
		return err
	}
	if _, err := sourceGitText(ctx, destination, "-c", "protocol.file.allow=always", "fetch", "--quiet", "--no-tags", "--depth=1", "--", source, expectedCommit); err != nil {
		return fmt.Errorf("exact approved source fetch: %w", err)
	}
	// FETCH_HEAD contains the private transport path, not public provenance.
	if err := os.Remove(filepath.Join(destination, ".git", "FETCH_HEAD")); err != nil {
		return err
	}
	if _, err := sourceGitText(ctx, destination, "checkout", "--quiet", "--detach", expectedCommit); err != nil {
		return err
	}
	head, err = sourceGitText(ctx, destination, "rev-parse", "HEAD")
	if err != nil || head != expectedCommit {
		return errors.New("source build clone lost approved identity")
	}
	status, err = sourceGitText(ctx, destination, "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return errors.New("source build clone is not clean")
	}
	if _, err := os.Lstat(filepath.Join(destination, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("source build clone retained external Git objects")
	}
	return nil
}

// Authenticate prospective generation evidence before touching staged roots.
// Genuine review authorization was checked by the parent against the main API;
// this boundary never substitutes a reward decision or a model assertion for it.
func validateSourceBuildRequest(request reviewedSourceBuildRequest) error {
	proposal := request.Proposal
	if request.Operation != "isolated_reviewed_source_build" || request.SandboxContract != "offline_evaluator_containment_v1" || proposal == nil || proposal.Schema != 2 ||
		proposal.ReviewId == ([16]byte{}) || proposal.JobId == ([16]byte{}) || request.SourceGeneration < 1 || maximumCompetitionEpoch < request.SourceGeneration ||
		!validSha256(proposal.ReviewDecisionDigest) || !validSha256(proposal.PatchSha256) || !validSha256(proposal.BaseSourceLockSha256) ||
		!strings.HasPrefix(proposal.EvaluatedImageDigest, "sha256:") || !validSha256(strings.TrimPrefix(proposal.EvaluatedImageDigest, "sha256:")) {
		return errors.New("source build requires the authenticated reviewed-source protocol")
	}
	if proposal.CandidatePathBase != "" && proposal.CandidatePathBase != "mono_root_v1" {
		return errors.New("source build candidate path base is unsupported")
	}
	manifest, err := decodeSourceManifest(request.SourceManifest)
	if err != nil {
		return err
	}
	if err := validateSourceManifest(manifest); err != nil {
		return err
	}
	if manifest.Schema != 2 || manifest.Staging != proposal.Staging || len(manifest.EvaluationSource.Generations) != request.SourceGeneration+1 {
		return errors.New("source build manifest mode or generation differs")
	}
	generation, err := manifest.generation(request.SourceGeneration)
	if err != nil {
		return err
	}
	lock, err := model.CompetitionSourceLockDigestForMode(proposal.Commits, proposal.Staging)
	if err != nil || lock != proposal.SourceLockSha256 || generation.SourceLockSha256 != lock || generation.SourceApprovalId != proposal.ReviewId.String() ||
		generation.SourceApprovalDigest != proposal.ReviewDecisionDigest || generation.PatchSha256 != proposal.PatchSha256 || generation.EvaluatedBaseSourceLockSha256 != proposal.BaseSourceLockSha256 {
		return errors.New("source build manifest does not bind the exact approved patch and commits")
	}
	if proposal.Significance == nil || generation.ScoreSha256 != proposal.Significance.ScoreSha256 || len(proposal.StagedRoots) != len(sourceRepositoryNames()) {
		return errors.New("source build evidence or private repository census is incomplete")
	}
	expectedStatistics, err := model.CompetitionAutomationDigest(generation.Significance)
	actualStatistics, actualErr := model.CompetitionAutomationDigest(proposal.Significance)
	if err != nil || actualErr != nil || actualStatistics != expectedStatistics {
		return errors.New("source build statistics differ from the authenticated manifest")
	}
	for _, repository := range sourceRepositoryNames() {
		path, ok := proposal.StagedRoots[repository]
		if !ok || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("source build repository staging paths are incomplete or redirected")
		}
	}
	for _, path := range []string{request.ConfigLocalDirectory, request.VaultLocalDirectory} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasSuffix(path, string(filepath.Separator)+"local") {
			return errors.New("source build runtime may name direct local leaves only")
		}
	}
	if filepath.Base(filepath.Dir(request.ConfigLocalDirectory)) != "config" || filepath.Base(filepath.Dir(request.VaultLocalDirectory)) != "vault" {
		return errors.New("source build runtime paths must be config/local and vault/local")
	}
	return nil
}

// Seal only tracked regular source. Git's immutable mode selects 0444/0555;
// incidental filesystem execute bits and executable metadata confer no powers.
func sealSourceBuildTree(ctx context.Context, root string, commits map[string]string, settings sourceBuildSandboxSettings) error {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root || !filepath.IsAbs(root) || root == string(filepath.Separator) || settings.OutputBytes <= 0 || settings.Timeout <= 0 {
		return errors.New("source sealing requires an exact bounded committed tree")
	}
	if _, err := model.CompetitionSourceLockDigest(commits); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	trackedModes := map[string]os.FileMode{}
	for _, repository := range sourceRepositoryNames() {
		repositoryRoot := filepath.Join(root, repository)
		head, err := sourceGitText(ctx, repositoryRoot, "rev-parse", "HEAD")
		if err != nil || head != commits[repository] {
			return errors.New("source sealing repository differs from its approved commit")
		}
		entries, err := sourceGitBytes(ctx, repositoryRoot, settings.OutputBytes, "ls-tree", "-r", "-z", "--full-tree", commits[repository])
		if err != nil {
			return err
		}
		if len(entries) == 0 || entries[len(entries)-1] != 0 {
			return errors.New("source sealing requires a complete tracked Git tree")
		}
		for _, entry := range strings.Split(string(entries[:len(entries)-1]), "\x00") {
			header, relative, found := strings.Cut(entry, "\t")
			fields := strings.Fields(header)
			if !found || relative == "" || filepath.ToSlash(filepath.Clean(relative)) != relative || strings.HasPrefix(relative, "/") || relative == ".." || strings.HasPrefix(relative, "../") || strings.ContainsAny(relative, "\\\r\n") ||
				len(fields) != 3 || fields[1] != "blob" || !sourceGitShaPattern.MatchString(fields[2]) || fields[0] != "100644" && fields[0] != "100755" {
				return errors.New("source build tree contains an unsafe path or non-regular committed entry")
			}
			mode := os.FileMode(0o444)
			if fields[0] == "100755" {
				mode = 0o555
			}
			trackedPath := filepath.Join(repositoryRoot, filepath.FromSlash(relative))
			if _, duplicate := trackedModes[trackedPath]; duplicate {
				return errors.New("source build tree repeats a tracked path")
			}
			trackedModes[trackedPath] = mode
		}
	}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o555)
		}
		if !info.Mode().IsRegular() {
			return errors.New("source build context contains a symlink or special file")
		}
		mode, tracked := trackedModes[path]
		if tracked {
			delete(trackedModes, path)
			return os.Chmod(path, mode)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) < 3 || !slices.Contains(sourceRepositoryNames(), parts[0]) || parts[1] != ".git" {
			return errors.New("source build context contains an untracked file")
		}
		return os.Chmod(path, 0o444)
	}); err != nil {
		return err
	}
	if len(trackedModes) != 0 {
		return errors.New("source build context is missing tracked files")
	}
	for _, repository := range sourceRepositoryNames() {
		status, err := sourceGitText(ctx, filepath.Join(root, repository), "--no-optional-locks", "status", "--porcelain=v1", "--untracked-files=all")
		if err != nil || status != "" {
			return errors.New("sealed source tree is not clean at its approved commit")
		}
	}
	return nil
}

// Allocate once and retain incomplete attempts for diagnosis. The caller uses a
// fresh unpredictable owner for each attempt; no existing directory is replaced.
func prepareSourceBuildContext(ctx context.Context, request reviewedSourceBuildRequest, owner string, settings sourceBuildSandboxSettings) (*sourceBuildContext, error) {
	if err := validateSourceBuildRequest(request); err != nil {
		return nil, err
	}
	if !validSha256(owner) || !filepath.IsAbs(settings.StateDirectory) || filepath.Clean(settings.StateDirectory) != settings.StateDirectory {
		return nil, errors.New("source build context requires an exact private state directory")
	}
	canonical, err := filepath.EvalSymlinks(settings.StateDirectory)
	info, statErr := os.Lstat(settings.StateDirectory)
	if err != nil || statErr != nil || canonical != settings.StateDirectory || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("source build state is not a private canonical directory")
	}
	root := filepath.Join(settings.StateDirectory, owner)
	if err := os.Mkdir(root, 0o700); err != nil {
		return nil, err
	}
	result := &sourceBuildContext{Directory: root, SourceDirectory: filepath.Join(root, "source"), ManifestPath: filepath.Join(root, "sim-latency.yml"), LockPath: filepath.Join(root, "source-lock.json")}
	if err := os.Mkdir(result.SourceDirectory, 0o700); err != nil {
		return nil, err
	}
	for _, repository := range sourceRepositoryNames() {
		source, ok := request.Proposal.StagedRoots[repository]
		if !ok {
			return nil, fmt.Errorf("source build repository %s is missing", repository)
		}
		if err := cloneSourceBuildRepository(ctx, repository, source, filepath.Join(result.SourceDirectory, repository), request.Proposal.Commits[repository]); err != nil {
			return nil, fmt.Errorf("source build repository %s: %w", repository, err)
		}
	}
	if err := verifyFrozenSourceGraphForMode(ctx, filepath.Join(result.SourceDirectory, "server"), request.Proposal.Commits["server"], request.Proposal.Staging); err != nil {
		return nil, err
	}
	lockBytes, err := json.MarshalIndent(evaluatorSourceLock{Schema: 1, CandidatePathBase: request.Proposal.CandidatePathBase, Repositories: request.Proposal.Commits}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writePrivateReviewFile(result.ManifestPath, request.SourceManifest); err != nil {
		return nil, err
	}
	if err := writePrivateReviewFile(result.LockPath, append(lockBytes, '\n')); err != nil {
		return nil, err
	}
	if err := sealSourceBuildTree(ctx, result.SourceDirectory, request.Proposal.Commits, settings); err != nil {
		return nil, err
	}
	return result, nil
}
