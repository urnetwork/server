// Reads exact source/config objects without fetching into product checkouts.
// Newly published pins are fetched only into disposable private bare repos.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

// Bounds one exact-commit resolution/fetch and object read together.
type sourceGitReadSettings struct {
	// Positive total read/fetch deadline; default two minutes, never per command.
	Timeout time.Duration
}

// A private fetch shares the same budget as its following object read.
func defaultSourceGitReadSettings() sourceGitReadSettings {
	return sourceGitReadSettings{Timeout: 2 * time.Minute}
}

// A source pin may exist only on origin after private promotion. Keep local
// refs and objects untouched; a missing remote pin fails instead of selecting
// a branch head. The bare fetch executes no candidate checkout or code.
func sourceGitBlobAtCommit(ctx context.Context, repositoryRoot, commit, blobPath string, limit int) ([]byte, error) {
	return sourceGitReadAtCommit(ctx, repositoryRoot, commit, defaultSourceGitReadSettings(), func(readCtx context.Context, root string) ([]byte, error) {
		return sourceGitBytes(readCtx, root, limit, "show", commit+":"+blobPath)
	})
}

// An empty successful literal ls-tree result proves absence. Existing empty
// blobs, redirected/missing repositories and every Git failure remain distinct.
func sourceGitPathAbsentAtCommit(ctx context.Context, repositoryRoot, commit, blobPath string, limit int) (bool, error) {
	if limit <= 0 || blobPath == "" || blobPath == "." || blobPath == ".." || path.Clean(blobPath) != blobPath || strings.HasPrefix(blobPath, "/") || strings.HasPrefix(blobPath, "../") || strings.ContainsAny(blobPath, "\\\r\n\x00") {
		return false, errors.New("pinned Git absence requires one literal repository path")
	}
	content, err := sourceGitReadAtCommit(ctx, repositoryRoot, commit, defaultSourceGitReadSettings(), func(readCtx context.Context, root string) ([]byte, error) {
		return sourceGitBytes(readCtx, root, limit, "--literal-pathspecs", "ls-tree", "-z", "--full-tree", commit, "--", blobPath)
	})
	if err != nil {
		return false, err
	}
	return len(content) == 0, nil
}

// Both content and absence queries resolve the same exact commit, fetching
// only into a disposable private bare repository when the local object lacks it.
func sourceGitReadAtCommit(ctx context.Context, repositoryRoot, commit string, settings sourceGitReadSettings, read func(context.Context, string) ([]byte, error)) ([]byte, error) {
	if !sourceGitShaPattern.MatchString(commit) || settings.Timeout <= 0 || read == nil {
		return nil, errors.New("pinned Git read requires an exact source commit and deadline")
	}
	readCtx, cancel := context.WithTimeout(ctx, settings.Timeout)
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
	content, err := read(readCtx, repositoryRoot)
	if err != nil {
		if readCtx.Err() != nil {
			return nil, fmt.Errorf("read exact pinned Git blob: %w", readCtx.Err())
		}
		return nil, fmt.Errorf("read exact pinned Git blob: %w", err)
	}
	return content, nil
}
