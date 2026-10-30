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

	"github.com/urnetwork/server/controller"
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
func cloneSourceBuildRepository(ctx context.Context, repository, source, destination, expectedCommit string) (resultErr error) {
	if !slices.Contains(sourceRepositoryNames(), repository) || !sourceGitShaPattern.MatchString(expectedCommit) {
		return errors.New("source build repository identity is invalid")
	}
	canonicalSource, sourceRoot, err := sourceGitRoots(source)
	if err != nil || canonicalSource != source || sourceRoot != source {
		return errors.New("source build transport requires an exact ordinary source checkout")
	}
	sourceGitDirectory := filepath.Join(sourceRoot, ".git")
	canonicalGit, gitErr := filepath.EvalSymlinks(sourceGitDirectory)
	gitInfo, statErr := os.Lstat(sourceGitDirectory)
	if gitErr != nil || statErr != nil || canonicalGit != sourceGitDirectory || !gitInfo.IsDir() || gitInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("source build transport Git directory is missing or redirected")
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
	// Git clears command config before local upload-pack. A private global file
	// survives that boundary without changing host config or widening other calls.
	configurationRoot, err := os.MkdirTemp(parent, ".source-build-git-")
	if err != nil {
		return err
	}
	configurationPath := filepath.Join(configurationRoot, "gitconfig")
	defer func() {
		if err := os.Remove(configurationPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
		resultErr = errors.Join(resultErr, os.Remove(configurationRoot))
	}()
	quote := func(value string) string {
		return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\t", "\\t", "\b", "\\b").Replace(value) + "\""
	}
	configuration := "[safe]\n\tdirectory =\n\tdirectory = " + quote(destination) + "\n\tdirectory = " + quote(sourceRoot) + "\n\tdirectory = " + quote(sourceGitDirectory) + "\n"
	if err := writePrivateReviewFile(configurationPath, []byte(configuration)); err != nil {
		return err
	}
	if err := os.Chmod(configurationPath, 0o400); err != nil {
		return err
	}
	// All command text is fixed. Git quotes the separately supplied source path;
	// child-local guards cannot be replaced by the source's repository config.
	fetch, err := sourceGitLocalCommand(ctx, destination, "-c", "safe.directory="+sourceRoot, "-c", "safe.directory="+sourceGitDirectory,
		"-c", "protocol.file.allow=always", "fetch",
		"--upload-pack=/usr/bin/git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c core.attributesFile=/dev/null -c log.showSignature=false -c gc.auto=0 -c maintenance.auto=false -c protocol.allow=never -c credential.helper= -c credential.interactive=false upload-pack",
		"--quiet", "--no-tags", "--depth=1", "--", source, expectedCommit)
	if err != nil {
		return err
	}
	for index, entry := range fetch.Env {
		if strings.HasPrefix(entry, "GIT_CONFIG_GLOBAL=") {
			fetch.Env[index] = "GIT_CONFIG_GLOBAL=" + configurationPath
		}
	}
	fetch.Env = append(fetch.Env, "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0")
	fetch.Stdout, fetch.Stderr = &sourceBoundedWriter{limit: maximumSourceConfigBytes}, &sourceBoundedWriter{limit: 4096}
	if err := fetch.Run(); err != nil {
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
	instrument := request.Operation == "isolated_trusted_instrument_build"
	if (!instrument && request.Operation != "isolated_reviewed_source_build") || request.SandboxContract != "offline_evaluator_containment_v1" || proposal == nil || proposal.Schema != 2 ||
		(!instrument && (proposal.ReviewId == ([16]byte{}) || proposal.JobId == ([16]byte{}) || !validSha256(proposal.ReviewDecisionDigest))) || request.SourceGeneration < 1 || maximumCompetitionEpoch < request.SourceGeneration ||
		!validSha256(proposal.PatchSha256) || !validSha256(proposal.BaseSourceLockSha256) ||
		!strings.HasPrefix(proposal.EvaluatedImageDigest, "sha256:") || !validSha256(strings.TrimPrefix(proposal.EvaluatedImageDigest, "sha256:")) {
		return errors.New("source build requires the authenticated reviewed-source protocol")
	}
	if !instrument && request.TrustedInstrument != nil {
		return errors.New("reviewed product build cannot reuse trusted instrument authority")
	}
	if proposal.CandidatePathBase != "" && proposal.CandidatePathBase != "mono_root_v1" {
		return errors.New("source build candidate path base is unsupported")
	}
	if proposal.WinnerReviewPolicy != "" && !model.IsCompetitionWinnerReviewPolicy(proposal.WinnerReviewPolicy) {
		return errors.New("source build review policy is unsupported")
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
	if err != nil || lock != proposal.SourceLockSha256 || generation.SourceLockSha256 != lock {
		return errors.New("source build manifest does not bind the exact approved patch and commits")
	}
	if instrument {
		proof := request.TrustedInstrument
		if proof == nil || proof.Schema != 1 || proposal.Staging || proposal.ReviewId != ([16]byte{}) || proposal.JobId != ([16]byte{}) || proposal.ReviewDecisionDigest != "" || proposal.Significance != nil ||
			proof.BuildReceipt != nil || generation.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork || generation.TrustedInstrument == nil ||
			proof.InstrumentedServerCommit != proposal.Commits["server"] || proof.SelectedProductSourceLockSha256 != proposal.BaseSourceLockSha256 || proof.Approval.PatchSha256 != proposal.PatchSha256 {
			return errors.New("instrument build requires explicit separate approval without fabricated product review metadata")
		}
		if err := controller.ValidateCompetitionTrustedInstrumentApproval(&proof.Approval); err != nil {
			return err
		}
		manifestProof, err := model.CompetitionAutomationDigest(generation.TrustedInstrument)
		requestProof, proofErr := model.CompetitionAutomationDigest(proof)
		if err != nil || proofErr != nil || manifestProof != requestProof {
			return errors.New("instrument build proof differs from the prospective generation")
		}
	} else {
		if generation.SourceApprovalId != proposal.ReviewId.String() || generation.SourceApprovalDigest != proposal.ReviewDecisionDigest || generation.PatchSha256 != proposal.PatchSha256 || generation.EvaluatedBaseSourceLockSha256 != proposal.BaseSourceLockSha256 ||
			proposal.Significance == nil || generation.ScoreSha256 != proposal.Significance.ScoreSha256 {
			return errors.New("source build approval or statistics do not match the authenticated manifest")
		}
		expectedStatistics, err := model.CompetitionAutomationDigest(generation.Significance)
		actualStatistics, actualErr := model.CompetitionAutomationDigest(proposal.Significance)
		if err != nil || actualErr != nil || actualStatistics != expectedStatistics {
			return errors.New("source build statistics differ from the authenticated manifest")
		}
	}
	if len(proposal.StagedRoots) != len(sourceRepositoryNames()) {
		return errors.New("source build evidence or private repository census is incomplete")
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
	if request.TrustedInstrument != nil {
		if err := verifyTrustedInstrumentChild(ctx, request.Proposal.StagedRoots["server"], request.TrustedInstrument); err != nil {
			return nil, err
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
