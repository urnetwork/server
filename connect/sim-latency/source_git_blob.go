// Reads exact source/config objects without fetching into product checkouts.
// Newly published pins are fetched only into disposable private bare repos.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// A source pin may exist only on origin after private promotion. Keep local
// refs and objects untouched; a missing remote pin fails instead of selecting
// a branch head. The bare fetch executes no candidate checkout or code.
func sourceGitBlobAtCommit(ctx context.Context, repositoryRoot, commit, blobPath string, limit int) ([]byte, error) {
	if !sourceGitShaPattern.MatchString(commit) {
		return nil, errors.New("pinned Git blob requires an exact source commit")
	}
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	resolved, resolveErr := sourceGitText(readCtx, repositoryRoot, "rev-parse", "--verify", commit+"^{commit}")
	if resolveErr != nil {
		if readCtx.Err() != nil {
			return nil, fmt.Errorf("resolve pinned Git commit: %w", readCtx.Err())
		}
		origin, err := sourceGitText(readCtx, repositoryRoot, "remote", "get-url", "origin")
		if err != nil {
			return nil, fmt.Errorf("read pinned Git origin: %w", err)
		}
		privateRoot, err := os.MkdirTemp("", "sim-latency-pinned-blob-")
		if err != nil {
			return nil, fmt.Errorf("create private pinned Git repository: %w", err)
		}
		defer os.RemoveAll(privateRoot)
		if _, err := sourceGitText(readCtx, privateRoot, "-c", "core.fsync=all", "init", "--quiet", "--bare"); err != nil {
			return nil, fmt.Errorf("initialize private pinned Git repository: %w", err)
		}
		if err := os.Chmod(privateRoot, 0o700); err != nil {
			return nil, fmt.Errorf("secure private pinned Git repository: %w", err)
		}
		if _, err := sourceGitText(readCtx, privateRoot, "-c", "core.fsync=all", "fetch", "--quiet", "--no-tags", "--depth=1", "--", origin, commit); err != nil {
			if readCtx.Err() != nil {
				return nil, fmt.Errorf("fetch exact pinned Git commit: %w", readCtx.Err())
			}
			return nil, fmt.Errorf("fetch exact pinned Git commit: %w", err)
		}
		repositoryRoot = privateRoot
		resolved, resolveErr = sourceGitText(readCtx, repositoryRoot, "rev-parse", "--verify", commit+"^{commit}")
	}
	if resolveErr != nil {
		return nil, fmt.Errorf("verify pinned Git commit: %w", resolveErr)
	}
	if resolved != commit {
		return nil, errors.New("resolved Git object is not the exact pinned commit")
	}
	content, err := sourceGitBytes(readCtx, repositoryRoot, limit, "show", commit+":"+blobPath)
	if err != nil {
		if readCtx.Err() != nil {
			return nil, fmt.Errorf("read exact pinned Git blob: %w", readCtx.Err())
		}
		return nil, fmt.Errorf("read exact pinned Git blob: %w", err)
	}
	return content, nil
}
