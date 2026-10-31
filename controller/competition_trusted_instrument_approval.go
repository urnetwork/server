// Pure operator-approval validation is usable by the frozen-source instrument
// image without importing newer Settings, source authorization or build code.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"slices"
	"strings"

	"github.com/urnetwork/server/model"
)

// Approval content is independent of the unknown winner and admits only
// literal competition-instrument files, never a product implementation file.
func ValidateCompetitionTrustedInstrumentApproval(approval *model.CompetitionTrustedInstrumentApproval) error {
	if approval == nil || approval.Schema != 1 || !tokenNamePattern.MatchString(approval.ApprovalId) || approval.TargetEpoch != 2 ||
		approval.G2VolumePolicy != model.CompetitionG2VolumePolicyMinimumWork || !imageDigestPattern.MatchString(approval.InstrumentBaseImageDigest) ||
		!sha256Pattern.MatchString(approval.ApprovalSha256) || !sha256Pattern.MatchString(approval.PatchSha256) || len(approval.Patch) == 0 || len(approval.ProtectedPaths) == 0 {
		return errors.New("trusted instrument requires a separately approved exact patch and original instrument provenance")
	}
	lock, err := model.CompetitionSourceLockDigestForMode(approval.InstrumentBaseCommits, false)
	if err != nil || lock != approval.InstrumentBaseSourceLockSha256 {
		return errors.New("trusted instrument original source graph is incomplete or changed")
	}
	digest := sha256.Sum256(approval.Patch)
	if hex.EncodeToString(digest[:]) != approval.PatchSha256 {
		return errors.New("trusted instrument patch bytes differ from operator approval")
	}
	approved := *approval
	approved.ApprovalSha256 = ""
	approvalDigest, err := model.CompetitionAutomationDigest(approved)
	if err != nil || approvalDigest != approval.ApprovalSha256 {
		return errors.New("trusted instrument approval digest does not bind its complete content")
	}
	for index, file := range approval.ProtectedPaths {
		if file == "" || path.Clean(file) != file || strings.HasPrefix(file, "/") || strings.HasPrefix(file, "../") ||
			strings.ContainsAny(file, "\\\r\n\x00\t *?[]:\"") || (index > 0 && approval.ProtectedPaths[index-1] >= file) ||
			!(strings.HasPrefix(file, "connect/sim-latency/") || strings.HasPrefix(file, "controller/competition_") ||
				strings.HasPrefix(file, "model/competition_") || strings.HasPrefix(file, "competition_")) ||
			!(strings.HasSuffix(file, ".go") || strings.HasSuffix(file, ".sh")) {
			return errors.New("trusted instrument protected_paths must be sorted unique literal competition-instrument files")
		}
		for _, segment := range strings.Split(file, "/") {
			if strings.EqualFold(segment, ".git") || strings.EqualFold(segment, ".hg") || strings.EqualFold(segment, ".svn") {
				return errors.New("trusted instrument cannot name repository metadata")
			}
		}
	}
	// Git's own parser applies hunks later, in a credentialless isolated index.
	// This census forbids quoted paths, renames, binary payloads and deletions.
	paths := []string{}
	for _, line := range strings.Split(string(approval.Patch), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			fields := strings.Split(line, " ")
			if len(fields) != 4 || !strings.HasPrefix(fields[2], "a/") || !strings.HasPrefix(fields[3], "b/") || fields[2][2:] != fields[3][2:] || !slices.Contains(approval.ProtectedPaths, fields[2][2:]) {
				return errors.New("trusted instrument patch has an unapproved or ambiguous path")
			}
			paths = append(paths, fields[2][2:])
		}
		if strings.HasPrefix(line, "deleted file mode ") || strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode ") ||
			strings.HasPrefix(line, "rename ") || strings.HasPrefix(line, "copy ") || strings.HasPrefix(line, "GIT binary patch") ||
			strings.HasPrefix(line, "Binary files ") || strings.ContainsRune(line, '\x00') || strings.ContainsRune(line, '\r') {
			return errors.New("trusted instrument patch cannot delete, rename, change modes or contain binary files")
		}
	}
	if !slices.Equal(paths, approval.ProtectedPaths) {
		return errors.New("trusted instrument patch differs from the exact approved protected path census")
	}
	return nil
}
