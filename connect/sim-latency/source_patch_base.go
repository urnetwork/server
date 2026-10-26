// Staging and publication replay share the canonical operation validator while
// keeping exact-commit, literal-path Git reads inside their trusted owner.
package main

import (
	"context"
	"errors"

	"github.com/urnetwork/server/controller"
)

// This checks immutable objects, never the mutable index or a filesystem path.
// Failed reads are errors and cannot authorize creation through false absence.
func validateSourcePatchBase(ctx context.Context, root, commit, repository string, patch *controller.CanonicalPatch, policy controller.PatchPolicy) error {
	if !sourceGitShaPattern.MatchString(commit) {
		return errors.New("source patch base requires an exact pinned commit")
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSourceGitReadSettings().Timeout)
	defer cancel()
	return controller.ValidateCompetitionRepositoryPatchBase(patch, policy, repository, func(relative string) ([]byte, error) {
		return sourceGitBytes(ctx, root, maximumSourceConfigBytes, "--literal-pathspecs", "ls-tree", "-z", "--full-tree", commit, "--", relative)
	})
}
