// Publishes one durable v2 source intent from private staging clones. Exact
// commits survive retries; Git publication never selects the runtime source.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/urnetwork/server/controller"
)

// Proves every original ref is still unpublished. One advanced or drifting ref
// forbids automatic fallback, including a successful push with a lost receipt.
func probeUnpublishedSource(ctx context.Context, intent *sourcePublicationIntent, backend sourcePublicationBackend) ([]byte, error) {
	if err := validateSourcePublicationIntent(intent); err != nil {
		return nil, err
	}
	type observedRef struct {
		Repository     string `json:"repository"`
		Branch         string `json:"branch"`
		ExpectedCommit string `json:"expected_commit"`
		ObservedCommit string `json:"observed_commit"`
	}
	refs := []observedRef{}
	for _, ref := range intent.Refs {
		head, err := backend.Head(ctx, ref)
		if err != nil {
			return nil, err
		}
		if head != ref.ExpectedCommit {
			return nil, errors.New("source fallback unavailable after any published or drifting ref")
		}
		refs = append(refs, observedRef{Repository: ref.Repository, Branch: ref.Branch, ExpectedCommit: ref.ExpectedCommit, ObservedCommit: head})
	}
	return json.Marshal(struct {
		Staging              bool          `json:"staging,omitempty"`
		Generation           int64         `json:"generation"`
		SourceDecisionDigest string        `json:"source_decision_digest"`
		Refs                 []observedRef `json:"refs"`
	}{Staging: intent.Staging, Generation: intent.ActivationGeneration, SourceDecisionDigest: intent.SourceDecisionDigest, Refs: refs})
}

// Stores one exact expected-ref transition. A changed ref must be precisely
// one commit above its predecessor, permitting ordinary fast-forward pushes
// to reject competing writes without any forced update or history rewrite.
type sourcePublicationRef struct {
	Repository     string `json:"repository"`
	Branch         string `json:"branch"`
	ExpectedCommit string `json:"expected_commit"`
	Commit         string `json:"commit"`
	StagedRoot     string `json:"staged_root"`
}

// Retains the immutable publication plan before the first push. The database
// intent digest must match; this file is recovery material, not authorization.
type sourcePublicationIntent struct {
	Schema               int                    `json:"schema"`
	Staging              bool                   `json:"staging,omitempty"`
	ActivationGeneration int64                  `json:"activation_generation"`
	SourceDecisionDigest string                 `json:"source_decision_digest"`
	ManifestSha256       string                 `json:"manifest_sha256"`
	ApprovedPatchSha256  string                 `json:"approved_patch_sha256,omitempty"`
	ApprovedPatchPath    string                 `json:"approved_patch_path,omitempty"`
	Bootstrap            bool                   `json:"bootstrap,omitempty"`
	Refs                 []sourcePublicationRef `json:"refs"`
}

// Abstracts publication for deterministic lost-reply and partial-push tests.
// Authorize must recheck the durable lease and separate source approval before
// every side effect, including a retry after an earlier successful push.
type sourcePublicationBackend interface {
	Authorize(context.Context, *sourcePublicationIntent) error
	Head(context.Context, sourcePublicationRef) (string, error)
	Verify(context.Context, sourcePublicationRef, *sourcePublicationIntent) error
	Push(context.Context, sourcePublicationRef) error
	Receipt(context.Context, sourcePublicationRef, *sourcePublicationIntent) error
}

// Validates complete source coverage and enforces config publication last.
func validateSourcePublicationIntent(intent *sourcePublicationIntent) error {
	if intent == nil || intent.Schema != 2 || intent.ActivationGeneration < 1 ||
		!validSha256(intent.SourceDecisionDigest) || !validSha256(intent.ManifestSha256) ||
		len(intent.Refs) != len(sourceRepositoryNames())+1 {
		return errors.New("source publication intent identity or complete repository set is invalid")
	}
	for index, repository := range append(sourceRepositoryNames(), "config") {
		ref := intent.Refs[index]
		branch := sourceBranch(intent.Staging)
		if repository == "config" {
			branch = "main"
		}
		if ref.Repository != repository || ref.Branch != branch ||
			!sourceGitShaPattern.MatchString(ref.ExpectedCommit) || !sourceGitShaPattern.MatchString(ref.Commit) ||
			!filepath.IsAbs(ref.StagedRoot) {
			return fmt.Errorf("source publication ref %d is invalid", index)
		}
		if repository != "server" && repository != "config" && ref.Commit != ref.ExpectedCommit {
			return fmt.Errorf("source publication cannot change protected repository %s", repository)
		}
		if intent.Bootstrap && ref.Commit != ref.ExpectedCommit {
			return errors.New("baseline bootstrap cannot change a published ref")
		}
		if repository == "server" && ref.Commit != ref.ExpectedCommit && (!validSha256(intent.ApprovedPatchSha256) || !filepath.IsAbs(intent.ApprovedPatchPath)) {
			return errors.New("changed server source requires its separately approved canonical patch digest")
		}
	}
	return nil
}

// Converges one immutable intent. An absent acknowledgement after a successful
// push re-verifies that exact commit and records a receipt, never creates code.
func reconcileSourcePublication(ctx context.Context, intent *sourcePublicationIntent, backend sourcePublicationBackend) error {
	if err := validateSourcePublicationIntent(intent); err != nil {
		return err
	}
	for _, ref := range intent.Refs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := backend.Authorize(ctx, intent); err != nil {
			return err
		}
		if err := backend.Verify(ctx, ref, intent); err != nil {
			return fmt.Errorf("verify staged %s: %w", ref.Repository, err)
		}
		head, err := backend.Head(ctx, ref)
		if err != nil {
			return err
		}
		if head != ref.ExpectedCommit && head != ref.Commit {
			return fmt.Errorf("source/ref drift in %s: expected %s or staged %s, found %s", ref.Repository, ref.ExpectedCommit, ref.Commit, head)
		}
		if head != ref.Commit {
			if err := backend.Push(ctx, ref); err != nil {
				return fmt.Errorf("publish %s; activation remains pending: %w", ref.Repository, err)
			}
		}
		head, err = backend.Head(ctx, ref)
		if err != nil || head != ref.Commit {
			return fmt.Errorf("published %s does not resolve to its immutable intended commit", ref.Repository)
		}
		if err := backend.Receipt(ctx, ref, intent); err != nil {
			return err
		}
	}
	return nil
}

// Uses the private durable clone as the sole object source. Callbacks are the
// authenticated database boundary; local files never grant promotion powers.
type gitSourcePublicationBackend struct {
	authorize   func(context.Context, *sourcePublicationIntent) error
	receipt     func(context.Context, sourcePublicationRef, *sourcePublicationIntent) error
	patchPolicy controller.PatchPolicy
}

// Rechecks the current fenced source decision before a remote write.
func (self *gitSourcePublicationBackend) Authorize(ctx context.Context, intent *sourcePublicationIntent) error {
	if self.authorize == nil || self.receipt == nil {
		return errors.New("source publication requires durable authorization and receipt callbacks")
	}
	return self.authorize(ctx, intent)
}

// Reads a remote head without using or changing an operator checkout.
func (self *gitSourcePublicationBackend) Head(ctx context.Context, ref sourcePublicationRef) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Lstat(ref.StagedRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("private staging clone is missing or unsafe")
	}
	output, err := sourceGitBytes(ctx, ref.StagedRoot, 4096, "ls-remote", "--exit-code", "origin", "refs/heads/"+ref.Branch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || fields[1] != "refs/heads/"+ref.Branch || !sourceGitShaPattern.MatchString(fields[0]) {
		return "", errors.New("source remote returned an invalid exact branch identity")
	}
	return fields[0], nil
}

// Checks immutable objects and allowed change surfaces on every retry. A
// process cannot resume with a different commit merely because its ref moved.
func (self *gitSourcePublicationBackend) Verify(ctx context.Context, ref sourcePublicationRef, intent *sourcePublicationIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if intent == nil || (ref.Repository != "config" && ref.Branch != sourceBranch(intent.Staging)) ||
		(ref.Repository == "config" && ref.Branch != "main") {
		return errors.New("publication ref does not match its explicit source mode")
	}
	info, err := os.Lstat(ref.StagedRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("private staging clone is missing or unsafe")
	}
	status, err := sourceGitText(ctx, ref.StagedRoot, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil || status != "" {
		return errors.New("private staging clone is not clean")
	}
	for _, commit := range []string{ref.ExpectedCommit, ref.Commit} {
		resolved, err := sourceGitText(ctx, ref.StagedRoot, "rev-parse", "--verify", commit+"^{commit}")
		if err != nil || resolved != commit {
			return errors.New("private staging clone lost an immutable commit")
		}
	}
	if ref.Commit != ref.ExpectedCommit {
		parents, err := sourceGitText(ctx, ref.StagedRoot, "rev-list", "--parents", "-n", "1", ref.Commit)
		if err != nil || parents != ref.Commit+" "+ref.ExpectedCommit {
			return errors.New("publication must be one exact fast-forward commit above its expected ref")
		}
		if ref.Repository == "server" {
			previousTree, previousErr := sourceGitText(ctx, ref.StagedRoot, "rev-parse", ref.ExpectedCommit+":connect/sim-latency")
			candidateTree, candidateErr := sourceGitText(ctx, ref.StagedRoot, "rev-parse", ref.Commit+":connect/sim-latency")
			if previousErr != nil || candidateErr != nil || previousTree != candidateTree {
				return errors.New("source publication modifies the protected evaluator tree")
			}
			patchInfo, err := os.Lstat(intent.ApprovedPatchPath)
			if err != nil || !patchInfo.Mode().IsRegular() || patchInfo.Mode().Perm()&0o077 != 0 {
				return errors.New("approved patch must remain a private regular file")
			}
			patchBytes, err := readSourceFile(intent.ApprovedPatchPath)
			if err != nil {
				return err
			}
			patch, patchErr := controller.ValidateAndCanonicalizePatch(string(patchBytes), self.patchPolicy)
			if patchErr != nil || patch.Sha256 != intent.ApprovedPatchSha256 || string(patch.Bytes) != string(patchBytes) {
				return errors.New("approved canonical patch digest or allowed surface does not match")
			}
			indexRoot, err := os.MkdirTemp("", "sim-latency-source-verify-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(indexRoot)
			indexPath := filepath.Join(indexRoot, "index")
			runIndex := func(args ...string) ([]byte, error) {
				command := exec.CommandContext(ctx, "git", append([]string{"-C", ref.StagedRoot}, args...)...)
				command.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
				writer := &sourceBoundedWriter{limit: 4096}
				command.Stdout = writer
				command.Stderr = &sourceBoundedWriter{limit: 4096}
				if err := command.Run(); err != nil {
					return nil, fmt.Errorf("verify approved source tree: %w", err)
				}
				return writer.content, nil
			}
			if _, err := runIndex("read-tree", ref.ExpectedCommit); err != nil {
				return err
			}
			if _, err := runIndex("apply", "--cached", "--whitespace=error-all", intent.ApprovedPatchPath); err != nil {
				return err
			}
			approvedTree, err := runIndex("write-tree")
			stagedTree, treeErr := sourceGitText(ctx, ref.StagedRoot, "rev-parse", ref.Commit+"^{tree}")
			if err != nil || treeErr != nil || strings.TrimSpace(string(approvedTree)) != stagedTree {
				return errors.New("staged source tree differs from the exact separately approved patch on its evaluated base")
			}
		}
	}
	if ref.Repository == "config" {
		manifestPath := sourceManifestRepositoryPath(intent.Staging)
		changed, err := sourceGitText(ctx, ref.StagedRoot, "diff", "--name-only", ref.ExpectedCommit, ref.Commit)
		if err != nil || (changed != manifestPath && !(intent.Bootstrap && changed == "" && ref.Commit == ref.ExpectedCommit)) {
			return errors.New("source config publication must change only the source manifest")
		}
		manifestBytes, err := sourceGitBytes(ctx, ref.StagedRoot, maximumSourceConfigBytes, "show", ref.Commit+":"+manifestPath)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(manifestBytes)
		if hex.EncodeToString(digest[:]) != intent.ManifestSha256 {
			return errors.New("published source manifest does not match the durable activation digest")
		}
		manifest, err := decodeSourceManifest(manifestBytes)
		if err != nil || manifest.Schema != 2 || manifest.Staging != intent.Staging {
			return errors.New("published source manifest does not match the explicit v2 mode")
		}
	}
	return nil
}

// Captures exact bounded Git bytes with cancellation; no whitespace
// normalization is allowed at an authenticated-content boundary.
func sourceGitBytes(ctx context.Context, repositoryRoot string, limit int, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repositoryRoot}, args...)...)
	writer := &sourceBoundedWriter{limit: limit}
	command.Stdout = writer
	command.Stderr = &sourceBoundedWriter{limit: 4096}
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("bounded Git content read failed: %w", err)
	}
	return writer.content, nil
}

// Reads bounded textual Git output only where whitespace is not authenticated
// content. Process cancellation covers network stalls and lease loss.
func sourceGitText(ctx context.Context, repositoryRoot string, args ...string) (string, error) {
	content, err := sourceGitBytes(ctx, repositoryRoot, maximumSourceConfigBytes, args...)
	return strings.TrimSpace(string(content)), err
}

// Refuses oversized process output without retaining it in memory.
type sourceBoundedWriter struct {
	limit   int
	content []byte
}

// Accepts bytes only within the independently bounded output budget.
func (self *sourceBoundedWriter) Write(content []byte) (int, error) {
	if len(content) > self.limit-len(self.content) {
		return 0, errors.New("source command output exceeds its bound")
	}
	self.content = append(self.content, content...)
	return len(content), nil
}

// Publishes a plain fast-forward only; no force flag is ever permitted.
func (self *gitSourcePublicationBackend) Push(ctx context.Context, ref sourcePublicationRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := sourceGitText(ctx, ref.StagedRoot, "push", "--porcelain", "origin", ref.Commit+":refs/heads/"+ref.Branch)
	return err
}

// Persists acknowledgement after independently verifying the remote head.
func (self *gitSourcePublicationBackend) Receipt(ctx context.Context, ref sourcePublicationRef, intent *sourcePublicationIntent) error {
	return self.receipt(ctx, ref, intent)
}

// Writes recovery material once, fsyncing both file and parent. Same-body
// replays succeed; a conflicting intent cannot overwrite a staged transition.
func saveSourcePublicationIntent(path string, intent *sourcePublicationIntent) error {
	if err := validateSourcePublicationIntent(intent); err != nil {
		return err
	}
	content, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	if existing, err := readSourceFile(path); err == nil {
		if string(existing) != string(content) {
			return errors.New("publication intent already exists with a different body")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writePrivateReviewFile(path, content); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Reads the one frozen journal without trusting caller-edited recovery data.
func loadSourcePublicationIntent(path string, expectedDigest string) (*sourcePublicationIntent, error) {
	content, err := readSourceFile(path)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	if !validSha256(expectedDigest) || hex.EncodeToString(digest[:]) != expectedDigest {
		return nil, errors.New("publication journal differs from its durable authorization")
	}
	intent := &sourcePublicationIntent{}
	decoder := json.NewDecoder(strings.NewReader(string(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(intent); err != nil {
		return nil, err
	}
	if err := validateSourcePublicationIntent(intent); err != nil {
		return nil, err
	}
	return intent, nil
}
