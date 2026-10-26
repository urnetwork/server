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
	"time"

	"github.com/urnetwork/server/model"
)

// Source operations consume one total bounded budget, not one reset per Git call.
type repositoryApplySettings struct {
	Timeout            time.Duration
	MaximumOutputBytes int
}

// The whole Docker build remains bounded separately by its owning evaluator.
func defaultRepositoryApplySettings() repositoryApplySettings {
	return repositoryApplySettings{Timeout: time.Minute, MaximumOutputBytes: 64 * 1024}
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

// Pins all clean source heads and branches before any patch is applied. The
// canonical input digest authenticates the preparer's original identity bytes.
func applyRepositoryPatches(root string, identityBytes []byte, expectedIdentitySha256 string, baseSha string, patchSha256 string, patches map[string][]byte, settings repositoryApplySettings) (map[string]string, error) {
	if settings.Timeout <= 0 || settings.MaximumOutputBytes <= 0 || !sha256Pattern.MatchString(expectedIdentitySha256) || !gitShaPattern.MatchString(baseSha) || !sha256Pattern.MatchString(patchSha256) || len(patches) == 0 || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return nil, errors.New("bounded authenticated source application required")
	}
	digest := sha256.Sum256(identityBytes)
	if hex.EncodeToString(digest[:]) != expectedIdentitySha256 {
		return nil, errors.New("original source identity bytes changed")
	}
	var identity repositorySourceIdentity
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
	if _, err := model.CompetitionSourceLockDigest(identity.Repositories); err != nil {
		return nil, err
	}
	for repository, patch := range patches {
		if _, exists := identity.Repositories[repository]; !exists || len(patch) == 0 {
			return nil, errors.New("routed patch names an absent repository")
		}
	}
	resolved, err := filepath.EvalSymlinks(root)
	info, statErr := os.Lstat(root)
	if err != nil || statErr != nil || resolved != root || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("source root is not a private canonical directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), settings.Timeout)
	defer cancel()
	runGit := func(repository string, args ...string) (string, error) {
		repositoryRoot := filepath.Join(root, repository)
		commandArgs := []string{"-C", repositoryRoot, "-c", "safe.directory=" + repositoryRoot, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "commit.gpgSign=false", "-c", "protocol.allow=never"}
		command := exec.CommandContext(ctx, "git", append(commandArgs, args...)...)
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "GIT_") {
				command.Env = append(command.Env, value)
			}
		}
		command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=URnetwork Competition", "GIT_AUTHOR_EMAIL=competition@invalid", "GIT_COMMITTER_NAME=URnetwork Competition", "GIT_COMMITTER_EMAIL=competition@invalid", "GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z", "LANG=C", "LC_ALL=C")
		output := &repositoryGitOutput{limit: settings.MaximumOutputBytes}
		command.Stdout, command.Stderr = output, output
		if err := command.Run(); err != nil {
			return "", errors.New("bounded trusted source Git operation failed")
		}
		return strings.TrimSpace(output.content.String()), nil
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
