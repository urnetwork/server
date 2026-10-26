// Explicit mono-root patches route text changes to the exact locked repository.
// Legacy server-relative policies retain their historical canonical bytes.
package controller

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/urnetwork/server/model"
)

const competitionMonoRootPathBase = "mono_root_v1"

// Uses the evaluated round's immutable path mode, not the moving live policy.
// A promotion must change at least one locked repository; legacy authority
// cannot be reused to approve an unreviewed dependency change.
func validateCompetitionRepositoryPromotion(previous map[string]string, next map[string]string, policy PatchPolicy) error {
	if _, err := model.CompetitionSourceLockDigest(previous); err != nil {
		return err
	}
	if _, err := model.CompetitionSourceLockDigest(next); err != nil {
		return err
	}
	if policy.PathBase != "" && policy.PathBase != competitionMonoRootPathBase {
		return errors.New("unsupported source patch path base")
	}
	changed := false
	for repository, commit := range previous {
		if next[repository] != commit {
			if policy.PathBase == "" && repository != "server" {
				return errors.New("legacy promotion cannot change dependency commits")
			}
			changed = true
		}
	}
	if !changed {
		return errors.New("reviewed promotion must change an evaluated repository")
	}
	return nil
}

// Refuses unknown roots, metadata and the evaluator independently of allowlists.
func competitionRepositoryPath(value string) (string, string, error) {
	if err := validateRepositoryPath(value); err != nil {
		return "", "", err
	}
	repository, relative, found := strings.Cut(value, "/")
	if !found || relative == "" {
		return "", "", errors.New("patch path must name a locked repository and file")
	}
	known := false
	for _, name := range model.CompetitionSourceRepositoryNames() {
		known = known || name == repository
	}
	if !known {
		return "", "", errors.New("patch repository is not in the competition source graph")
	}
	for _, segment := range strings.Split(relative, "/") {
		if strings.EqualFold(segment, ".git") || strings.EqualFold(segment, ".hg") || strings.EqualFold(segment, ".svn") {
			return "", "", errors.New("repository metadata is not an editable product file")
		}
	}
	if repository == "server" && (relative == "connect/sim-latency" || strings.HasPrefix(relative, "connect/sim-latency/")) {
		return "", "", errors.New("sim-latency evaluator source is protected")
	}
	return repository, relative, nil
}

// Broad mode allows explicit locked repository subtrees, not ambiguous globs.
func validateCompetitionAllowedPaths(policy PatchPolicy) error {
	if policy.PathBase == "" {
		return validateLiteralAllowedPaths(policy.AllowedPaths)
	}
	if policy.PathBase != competitionMonoRootPathBase {
		return errors.New("unsupported patch_policy.path_base")
	}
	for _, value := range policy.AllowedPaths {
		probe := value
		if strings.HasSuffix(value, "/**") {
			probe = strings.TrimSuffix(value, "**") + "product.go"
		} else if strings.ContainsAny(value, `*?[\\`) {
			return fmt.Errorf("mono-root allowlist requires literal files or repository subtrees: %q", value)
		}
		if _, _, err := competitionRepositoryPath(probe); err != nil {
			return fmt.Errorf("invalid mono-root allowlist %q: %w", value, err)
		}
	}
	return nil
}

// Revalidates the original canonical patch, then strips only repository prefixes
// on structural headers. Hunk contents are copied byte-for-byte, never replaced.
// The returned per-repository patches are used only in private fresh checkouts.
func SplitCompetitionRepositoryPatch(raw string, policy PatchPolicy) (map[string][]byte, *CompetitionError) {
	canonical, evalError := ValidateAndCanonicalizePatch(raw, policy)
	if evalError != nil {
		return nil, evalError
	}
	if policy.PathBase == "" {
		return map[string][]byte{"server": append([]byte{}, canonical.Bytes...)}, nil
	}
	patches := map[string][]byte{}
	repository, relative := "", ""
	inHunk := false
	for _, line := range bytes.SplitAfter(canonical.Bytes, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		text := string(line)
		switch {
		case strings.HasPrefix(text, "diff --git "):
			value, err := parseDiffHeader(strings.TrimSuffix(text, "\n"))
			if err != nil {
				return nil, submissionError("invalid_patch_structure", "validated patch header could not be routed")
			}
			repository, relative, err = competitionRepositoryPath(value)
			if err != nil {
				return nil, submissionError("path_not_allowed", err.Error())
			}
			text = "diff --git a/" + relative + " b/" + relative + "\n"
			inHunk = false
		case !inHunk && strings.HasPrefix(text, "--- a/"):
			text = "--- a/" + relative + "\n"
		case !inHunk && strings.HasPrefix(text, "+++ b/"):
			text = "+++ b/" + relative + "\n"
		case strings.HasPrefix(text, "@@ "):
			inHunk = true
		}
		patches[repository] = append(patches[repository], []byte(text)...)
	}
	return patches, nil
}
