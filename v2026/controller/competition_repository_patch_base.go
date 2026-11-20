// Pinned-tree checks complement canonical diff metadata. Callers retain their
// own trusted Git process, exact commit, literal pathspec and deadline ownership.
package controller

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// Declared mode is empty only for a modification whose optional index omits it.
// This metadata is produced by the canonical parser, never inferred from I/O.
type CompetitionPatchFileOperation struct {
	Operation string
	Mode      string
}

// Validates one repository's targets before application. Successful empty
// output proves absence only for a canonical creation; a Git error never does.
func ValidateCompetitionRepositoryPatchBase(patch *CanonicalPatch, policy PatchPolicy, repository string, readEntry func(string) ([]byte, error)) error {
	if patch == nil || len(patch.Paths) == 0 || len(patch.FileOperations) != len(patch.Paths) || readEntry == nil || policy.PathBase != "" && policy.PathBase != competitionMonoRootPathBase {
		return errors.New("patch base requires complete canonical operations and a trusted tree reader")
	}
	matched := false
	for _, filePath := range patch.Paths {
		owner, relative := "server", filePath
		if policy.PathBase == competitionMonoRootPathBase {
			var err error
			owner, relative, err = competitionRepositoryPath(filePath)
			if err != nil {
				return err
			}
		}
		if owner != repository {
			continue
		}
		matched = true
		operation, exists := patch.FileOperations[filePath]
		if !exists || operation.Operation != "modify" && operation.Operation != "create" && operation.Operation != "delete" ||
			operation.Mode != "" && operation.Mode != "100644" && (policy.PathBase != competitionMonoRootPathBase || operation.Mode != "100755") {
			return errors.New("patch base operation is incomplete or not a regular file mode")
		}
		content, err := readEntry(relative)
		if err != nil {
			return fmt.Errorf("read pinned patch target %q: %w", filePath, err)
		}
		if operation.Operation == "create" {
			if len(content) != 0 || operation.Mode == "" {
				return errors.New("patch creation target exists at its pinned base or lacks a regular mode")
			}
			continue
		}
		entries := bytes.Split(content, []byte{0})
		if len(entries) != 2 || len(entries[1]) != 0 {
			return errors.New("patch target is missing or its pinned Git entry is malformed")
		}
		header, target, found := strings.Cut(string(entries[0]), "\t")
		fields := strings.Fields(header)
		if !found || target != relative || len(fields) != 3 || fields[1] != "blob" || !gitShaPattern.MatchString(fields[2]) ||
			fields[0] != "100644" && (policy.PathBase != competitionMonoRootPathBase || fields[0] != "100755") {
			return errors.New("patch target is not an exact pinned regular source blob")
		}
		if operation.Mode != "" && operation.Mode != fields[0] {
			return errors.New("patch mode disagrees with the pinned source entry; mode changes are not accepted")
		}
	}
	if !matched {
		return errors.New("repository has no canonical patch targets")
	}
	return nil
}
