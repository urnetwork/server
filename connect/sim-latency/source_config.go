// Fresh source intents may preserve unrelated config/main advances. Persisted
// intents never enter this path: their exact expected/new commits remain fixed.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"github.com/urnetwork/server/model"
)

// Stages one manifest-only successor on the fetched main head after verifying
// the active manifest's ancestry and exact bytes. No source pins or remote refs
// are changed, and an existing staging path is never reused or rebased.
func stageFreshSourceConfig(ctx context.Context, origin, stagedRoot string, current *model.CompetitionSourceActivation, manifestBytes []byte, message string) (string, string, error) {
	if current == nil || !sourceGitShaPattern.MatchString(current.ManifestCommit) || !validSha256(current.ManifestSha256) || len(current.Manifest) == 0 || len(current.Manifest) > maximumSourceConfigBytes {
		return "", "", errors.New("active source lacks its authenticated config predecessor")
	}
	currentDigest := sha256.Sum256(current.Manifest)
	if hex.EncodeToString(currentDigest[:]) != current.ManifestSha256 {
		return "", "", errors.New("active source manifest bytes do not match its immutable digest")
	}
	if len(manifestBytes) == 0 || len(manifestBytes) > maximumSourceConfigBytes {
		return "", "", errors.New("source successor manifest is empty or oversized")
	}
	for _, content := range [][]byte{current.Manifest, manifestBytes} {
		manifest, err := decodeSourceManifest(content)
		if err != nil || manifest.Schema != 2 || manifest.Staging != current.Staging {
			return "", "", errors.New("source config predecessor and successor must retain the explicit v2 mode")
		}
	}
	manifestRepositoryPath := sourceManifestRepositoryPath(current.Staging)
	if _, err := os.Lstat(stagedRoot); !errors.Is(err, os.ErrNotExist) {
		return "", "", errors.New("fresh config staging requires a new private path")
	}
	if _, err := sourceGitText(ctx, filepath.Dir(stagedRoot), "-c", "core.fsync=all", "clone", "--quiet", "--no-checkout", "--no-hardlinks", origin, stagedRoot); err != nil {
		return "", "", err
	}
	if err := os.Chmod(stagedRoot, 0o700); err != nil {
		return "", "", err
	}
	// Fetch the named branch rather than relying on origin's default branch or
	// the clone's earlier snapshot. Later drift is rejected by publication.
	if _, err := sourceGitText(ctx, stagedRoot, "-c", "core.fsync=all", "fetch", "--quiet", "--no-tags", "origin", "refs/heads/main"); err != nil {
		return "", "", err
	}
	parent, err := sourceGitText(ctx, stagedRoot, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil || !sourceGitShaPattern.MatchString(parent) {
		return "", "", errors.New("config main lacks an immutable fetched head")
	}
	if _, err := sourceGitText(ctx, stagedRoot, "merge-base", "--is-ancestor", current.ManifestCommit, parent); err != nil {
		return "", "", errors.New("config main no longer descends from the active manifest commit")
	}
	for _, commit := range []string{current.ManifestCommit, parent} {
		content, err := sourceGitBytes(ctx, stagedRoot, maximumSourceConfigBytes, "show", commit+":"+manifestRepositoryPath)
		if err != nil {
			return "", "", err
		}
		digest := sha256.Sum256(content)
		if !bytes.Equal(content, current.Manifest) || hex.EncodeToString(digest[:]) != current.ManifestSha256 {
			return "", "", errors.New("config source manifest differs from the active immutable manifest")
		}
	}
	if _, err := sourceGitText(ctx, stagedRoot, "checkout", "--quiet", "--detach", parent); err != nil {
		return "", "", err
	}
	manifestPath := filepath.Join(stagedRoot, filepath.FromSlash(manifestRepositoryPath))
	info, err := os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", errors.New("config source manifest must remain a regular file")
	}
	if err := os.WriteFile(manifestPath, manifestBytes, 0o644); err != nil {
		return "", "", err
	}
	if _, err := sourceGitText(ctx, stagedRoot, "add", "--", manifestRepositoryPath); err != nil {
		return "", "", err
	}
	if _, err := sourceGitText(ctx, stagedRoot, "-c", "core.fsync=all", "-c", "user.name=URnetwork Competition", "-c", "user.email=competition@invalid", "commit", "--quiet", "--no-gpg-sign", "-m", message); err != nil {
		return "", "", err
	}
	commit, err := sourceGitText(ctx, stagedRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || !sourceGitShaPattern.MatchString(commit) {
		return "", "", errors.New("staged config successor lacks its immutable commit")
	}
	return parent, commit, nil
}
