// Scopes host source Git trust to one canonical checkout per command. Only
// configured host paths and authenticated private staging paths reach this boundary.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Discovers ordinary and bare repository roots without asking Git to trust an
// owner first. Linked worktrees, symlink aliases and world-writable metadata fail closed.
func sourceGitRoots(directory string) (string, string, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) == string(filepath.Separator) {
		return "", "", errors.New("source Git requires an exact absolute directory")
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil || canonical != filepath.Clean(directory) {
		return "", "", errors.New("source Git directory is missing or uses a symlink alias")
	}
	info, err := os.Lstat(canonical)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o002 != 0 {
		return "", "", errors.New("source Git directory must be a non-world-writable directory")
	}
	for root := canonical; ; root = filepath.Dir(root) {
		metadata := filepath.Join(root, ".git")
		if info, err := os.Lstat(metadata); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o002 != 0 {
				return "", "", errors.New("source Git requires ordinary non-symlink repository metadata")
			}
			if !sourceGitMetadataSafe(metadata) {
				return "", "", errors.New("source Git repository metadata is missing or redirected")
			}
			rootInfo, err := os.Lstat(root)
			if err != nil || rootInfo.Mode().Perm()&0o002 != 0 {
				return "", "", errors.New("source Git repository root must not be world-writable")
			}
			return canonical, root, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("inspect source Git metadata: %w", err)
		}
		// Private bare fetches have no worktree or .git directory.
		head, headErr := os.Lstat(filepath.Join(root, "HEAD"))
		objects, objectsErr := os.Lstat(filepath.Join(root, "objects"))
		config, configErr := os.Lstat(filepath.Join(root, "config"))
		if headErr == nil && objectsErr == nil && configErr == nil {
			if !head.Mode().IsRegular() || !objects.IsDir() || objects.Mode()&os.ModeSymlink != 0 || !config.Mode().IsRegular() || !sourceGitMetadataSafe(root) ||
				head.Mode().Perm()&0o002 != 0 || objects.Mode().Perm()&0o002 != 0 || config.Mode().Perm()&0o002 != 0 {
				return "", "", errors.New("source Git bare repository metadata is unsafe")
			}
			rootInfo, err := os.Lstat(root)
			if err != nil || rootInfo.Mode().Perm()&0o002 != 0 {
				return "", "", errors.New("source Git bare repository root must not be world-writable")
			}
			return canonical, root, nil
		}
		if root == filepath.Dir(root) {
			// clone and init intentionally run from a new private directory.
			return canonical, "", nil
		}
	}
}

// Ordinary repositories have local regular head/config files and a local
// objects directory, never linked-worktree common-directory indirection.
func sourceGitMetadataSafe(root string) bool {
	for _, name := range []string{"HEAD", "config", "objects"} {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o002 != 0 ||
			(name == "objects" && !info.IsDir()) || (name != "objects" && !info.Mode().IsRegular()) {
			return false
		}
	}
	_, err := os.Lstat(filepath.Join(root, "commondir"))
	return errors.Is(err, os.ErrNotExist)
}

// Keeps credential/SSH configuration while rejecting inherited repository
// redirection. Hooks and filesystem-monitor programs cannot run in host staging.
func sourceGitCommand(ctx context.Context, directory string, args ...string) (*exec.Cmd, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, repositoryRoot, err := sourceGitRoots(directory)
	if err != nil {
		return nil, err
	}
	commandArgs := []string{"-c", "safe.directory="}
	if repositoryRoot != "" {
		if repositoryRoot == filepath.Dir(repositoryRoot) || filepath.Base(repositoryRoot) == "*" {
			return nil, errors.New("source Git repository trust cannot name a root or wildcard")
		}
		commandArgs = append(commandArgs, "-c", "safe.directory="+repositoryRoot)
	}
	commandArgs = append(commandArgs, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", directory)
	command := exec.CommandContext(ctx, "git", append(commandArgs, args...)...)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_NAMESPACE", "GIT_PREFIX":
			continue
		}
		command.Env = append(command.Env, entry)
	}
	return command, nil
}

// Resolves every configured origin before allocating a source attempt. Read
// failures retain no new clone tree; completed/partially staged evidence is untouched.
func sourceRepositoryOrigins(ctx context.Context, repositoriesRoot, sourceConfig string, staging bool) (map[string]string, error) {
	origins := map[string]string{}
	for _, repository := range sourceRepositoryNamesForMode(staging) {
		origin, err := sourceGitText(ctx, filepath.Join(repositoriesRoot, repository), "remote", "get-url", "origin")
		if err != nil || origin == "" {
			return nil, fmt.Errorf("read configured %s source origin: %w", repository, errors.Join(err, errors.New("source origin is unavailable")))
		}
		origins[repository] = origin
	}
	if sourceConfig != "" {
		configRoot, err := sourceGitText(ctx, filepath.Dir(sourceConfig), "rev-parse", "--show-toplevel")
		if err != nil {
			return nil, fmt.Errorf("discover configured source config repository: %w", err)
		}
		origin, err := sourceGitText(ctx, configRoot, "remote", "get-url", "origin")
		if err != nil || origin == "" {
			return nil, fmt.Errorf("read configured config source origin: %w", errors.Join(err, errors.New("source origin is unavailable")))
		}
		origins["config"] = origin
	}
	return origins, nil
}

// Allocates only after every local origin read succeeds. Later staging errors
// retain the newly owned attempt as evidence; historical attempts are never scanned or removed.
func newSourceStage(ctx context.Context, stateDirectory, prefix, repositoriesRoot, sourceConfig string, staging bool) (string, map[string]string, error) {
	origins, err := sourceRepositoryOrigins(ctx, repositoriesRoot, sourceConfig, staging)
	if err != nil {
		return "", nil, err
	}
	root, err := os.MkdirTemp(stateDirectory, prefix)
	if err != nil {
		return "", nil, err
	}
	return root, origins, nil
}
