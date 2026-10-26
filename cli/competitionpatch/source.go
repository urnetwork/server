// Trusted source application never executes candidate code. All repositories
// are checked before mutation; a failure retains the private checkout for the
// owning evaluator's bounded cleanup and cannot emit a successful identity.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Source operations consume one total bounded budget, not one reset per Git call.
type repositoryApplySettings struct {
	// Positive whole-operation deadline; default one minute, never per Git call.
	Timeout time.Duration
	// Positive post-cancellation pipe-drain bound; default five seconds.
	WaitDelay time.Duration
	// Positive per-command combined-output cap; default 64 KiB.
	MaximumOutputBytes int
}

// Keeps a typed boundary without classifying Git stderr or an empty diff. A
// future genuine patch-nonapplication classification may narrow this safely.
type repositoryApplicationError struct {
	cause error
}

// A typed process result permits only Git config's documented no-match exit
// without guessing from stderr or misreading an I/O failure as empty settings.
type repositoryGitExitError struct {
	exitCode int
}

// Command output is never exposed as a public error message.
func (self *repositoryGitExitError) Error() string {
	return "bounded trusted source Git operation failed"
}

// The message contains no command output, original identity bytes or secrets.
func (self *repositoryApplicationError) Error() string {
	return "trusted source application failed: " + self.cause.Error()
}

// Preserve ordinary errors.Is/As behavior for deterministic owner assertions.
func (self *repositoryApplicationError) Unwrap() error {
	return self.cause
}

// The whole Docker build remains bounded separately by its owning evaluator.
func defaultRepositoryApplySettings() repositoryApplySettings {
	return repositoryApplySettings{Timeout: time.Minute, WaitDelay: 5 * time.Second, MaximumOutputBytes: 64 * 1024}
}

// Exactly the trusted preparer's schema; no candidate-derived source paths.
type repositorySourceIdentity struct {
	Schema               int               `json:"schema"`
	Kind                 string            `json:"kind"`
	Temporary            bool              `json:"temporary"`
	BaseImageId          string            `json:"base_image_id"`
	BaseSha              string            `json:"base_sha"`
	SourceEpoch          int               `json:"source_epoch"`
	Staging              bool              `json:"staging"`
	Branch               string            `json:"branch"`
	SourceLockSha256     string            `json:"source_lock_sha256"`
	CandidatePathBase    string            `json:"candidate_path_base,omitempty"`
	Repositories         map[string]string `json:"repositories"`
	CandidatePatchSha256 *string           `json:"candidate_patch_sha256"`
}

// Keeps even unexpected Git output within the operation's memory bound.
type repositoryGitOutput struct {
	content bytes.Buffer
	limit   int
}

// Refuses overflow without retaining a partial over-limit write.
func (self *repositoryGitOutput) Write(content []byte) (int, error) {
	if len(content) > self.limit-self.content.Len() {
		return 0, errors.New("Git output exceeds source operation bound")
	}
	return self.content.Write(content)
}

// Parent cancellation and the one total operation deadline retire the entire
// Git process group; a descendant cannot keep inherited pipes alive forever.
func applyRepositoryPatchesContext(ctx context.Context, root string, identityBytes []byte, expectedIdentitySha256 string, baseSha string, patchSha256 string, policy controller.PatchPolicy, canonical *controller.CanonicalPatch, patches map[string][]byte, settings repositoryApplySettings) (candidateCommits map[string]string, operationErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if settings.Timeout <= 0 || settings.WaitDelay <= 0 || settings.MaximumOutputBytes <= 0 || !sha256Pattern.MatchString(expectedIdentitySha256) || !gitShaPattern.MatchString(baseSha) || !sha256Pattern.MatchString(patchSha256) || canonical == nil || canonical.Sha256 != patchSha256 || len(patches) == 0 || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return nil, errors.New("bounded authenticated source application required")
	}
	digest := sha256.Sum256(identityBytes)
	if hex.EncodeToString(digest[:]) != expectedIdentitySha256 {
		return nil, errors.New("original source identity bytes changed")
	}
	var identity repositorySourceIdentity
	pathBase := policy.PathBase
	decoder := json.NewDecoder(bytes.NewReader(identityBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		return nil, errors.New("invalid original source identity")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("original source identity has trailing data")
	}
	branch := "sim-latency"
	if identity.Staging {
		branch = "sim-latency-staging"
	}
	if identity.Schema != 1 || identity.Kind != "sim-latency-evaluation-source" || !identity.Temporary || identity.BaseSha != baseSha || identity.Repositories["server"] != baseSha || identity.CandidatePatchSha256 != nil || identity.Branch != branch || !sha256Pattern.MatchString(identity.SourceLockSha256) {
		return nil, errors.New("original source identity does not name pristine competition checkouts")
	}
	if identity.CandidatePathBase != "" && identity.CandidatePathBase != "mono_root_v1" || pathBase != "" && pathBase != "mono_root_v1" || pathBase != "" && identity.CandidatePathBase != pathBase {
		return nil, errors.New("source image does not support the frozen candidate path base")
	}
	if _, err := model.CompetitionSourceLockDigest(identity.Repositories); err != nil {
		return nil, err
	}
	for repository, patch := range patches {
		if _, exists := identity.Repositories[repository]; !exists || len(patch) == 0 {
			return nil, errors.New("routed patch names an absent repository")
		}
		if pathBase == "" && repository != "server" {
			return nil, errors.New("legacy source application cannot change a dependency repository")
		}
	}
	resolved, err := filepath.EvalSymlinks(root)
	info, statErr := os.Lstat(root)
	if err != nil || statErr != nil || resolved != root || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("source root is not a private canonical directory")
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	defer func() {
		if err := ctx.Err(); err != nil {
			operationErr = errors.Join(err, operationErr)
		}
	}()
	runGitRaw := func(repository string, args ...string) (string, error) {
		repositoryRoot := filepath.Join(root, repository)
		commandArgs := []string{"-C", repositoryRoot, "-c", "safe.directory=" + repositoryRoot, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "commit.gpgSign=false", "-c", "protocol.allow=never", "-c", "gc.auto=0", "-c", "maintenance.auto=false"}
		command := exec.CommandContext(ctx, "git", append(commandArgs, args...)...)
		command.WaitDelay = settings.WaitDelay
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Cancel = func() error {
			if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			} else {
				return err
			}
		}
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "GIT_") {
				command.Env = append(command.Env, value)
			}
		}
		command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=URnetwork Competition", "GIT_AUTHOR_EMAIL=competition@invalid", "GIT_COMMITTER_NAME=URnetwork Competition", "GIT_COMMITTER_EMAIL=competition@invalid", "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z", "LANG=C", "LC_ALL=C")
		output := &repositoryGitOutput{limit: settings.MaximumOutputBytes}
		command.Stdout, command.Stderr = output, output
		if err := command.Run(); err != nil {
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				return output.content.String(), errors.Join(ctx.Err(), &repositoryGitExitError{exitCode: exitError.ExitCode()})
			}
			return "", errors.Join(ctx.Err(), errors.New("bounded trusted source Git operation failed"))
		}
		return strings.TrimSpace(output.content.String()), nil
	}
	runGit := func(repository string, args ...string) (string, error) {
		configured, err := runGitRaw(repository, "config", "--local", "--includes", "--name-only", "--get-regexp", `^filter\.`)
		var exitError *repositoryGitExitError
		if !errors.As(err, &exitError) || exitError.exitCode != 1 || configured != "" {
			return "", errors.Join(ctx.Err(), errors.New("trusted source Git filter configuration is present or unreadable"))
		}
		return runGitRaw(repository, args...)
	}
	repositories := model.CompetitionSourceRepositoryNames()
	sort.Strings(repositories)
	for _, repository := range repositories {
		path := filepath.Join(root, repository)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || resolved != path {
			return nil, errors.New("source repository is redirected")
		}
		metadata := filepath.Join(path, ".git")
		info, err := os.Lstat(metadata)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("source repository metadata is not an independent clone")
		}
		head, headErr := runGit(repository, "rev-parse", "HEAD")
		actualBranch, branchErr := runGit(repository, "symbolic-ref", "--quiet", "--short", "HEAD")
		status, statusErr := runGit(repository, "status", "--porcelain=v1", "--untracked-files=all")
		if headErr != nil || branchErr != nil || statusErr != nil || head != identity.Repositories[repository] || actualBranch != branch || status != "" {
			return nil, errors.New("source repository differs from its pristine frozen identity")
		}
	}
	protectedTree, err := runGit("server", "rev-parse", "HEAD:connect/sim-latency")
	if err != nil || !gitShaPattern.MatchString(protectedTree) {
		return nil, errors.New("protected evaluator tree is unavailable")
	}
	patchDirectory, err := os.MkdirTemp("", "competition-routed-patches-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(patchDirectory)
	if _, err := writeRepositoryPatches(filepath.Join(patchDirectory, "patches"), patches); err != nil {
		return nil, err
	}
	for _, repository := range repositories {
		if _, exists := patches[repository]; exists {
			if err := controller.ValidateCompetitionRepositoryPatchBase(canonical, policy, repository, func(relative string) ([]byte, error) {
				entry, err := runGit(repository, "--literal-pathspecs", "ls-tree", "-z", "--full-tree", identity.Repositories[repository], "--", relative)
				return []byte(entry), err
			}); err != nil {
				return nil, err
			}
			if _, err := runGit(repository, "apply", "--check", "--whitespace=error-all", filepath.Join(patchDirectory, "patches", repository+".patch")); err != nil {
				return nil, err
			}
		}
	}
	commits := map[string]string{}
	for _, repository := range repositories {
		commits[repository] = identity.Repositories[repository]
		if _, exists := patches[repository]; !exists {
			continue
		}
		if _, err := runGit(repository, "apply", "--index", "--whitespace=error-all", filepath.Join(patchDirectory, "patches", repository+".patch")); err != nil {
			return nil, err
		}
		if _, err := runGit(repository, "diff", "--cached", "--check"); err != nil {
			return nil, err
		}
		if _, err := runGit(repository, "commit", "--quiet", "--no-gpg-sign", "-m", "competition submission "+patchSha256); err != nil {
			return nil, err
		}
		commits[repository], err = runGit(repository, "rev-parse", "HEAD")
		if err != nil || !gitShaPattern.MatchString(commits[repository]) {
			return nil, errors.New("candidate commit identity is unavailable")
		}
		status, err := runGit(repository, "status", "--porcelain=v1", "--untracked-files=all")
		if err != nil || status != "" {
			return nil, errors.New("candidate checkout is not clean after source application")
		}
	}
	actualTree, err := runGit("server", "rev-parse", "HEAD:connect/sim-latency")
	if err != nil || actualTree != protectedTree {
		return nil, errors.New("candidate changed protected evaluator source")
	}
	return commits, nil
}
