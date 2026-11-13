// Synthetic local object graphs test dedicated preimages and postimages only.
// No real operator approval, source selection or instrument child is created.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The fixture uses private global Git identity without changing shared Git
// helpers or inheriting the operator/runner's actor or configuration.
func sourceCoverageInstrumentFixture(t *testing.T, newSymlink bool) (string, string, *model.CompetitionTrustedInstrumentApproval) {
	t.Helper()
	sourceTestPrivateGlobalIdentity(t, "Synthetic Coverage", "coverage@fixture.example")
	root := t.TempDir()
	sourceTestRepository(t, root, "server")
	repository := filepath.Join(root, "server")
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	helpers, err := model.CompetitionWorkCoverageProtectedHelperPaths(&coverage)
	if err != nil {
		t.Fatal(err)
	}
	if !newSymlink {
		for _, path := range helpers {
			absolute := filepath.Join(repository, path)
			if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(absolute, []byte("// synthetic original helper\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		sourceTestGit(t, repository, "add", "--all")
		sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic helper preimages")
	}
	base := sourceTestGit(t, repository, "rev-parse", "HEAD")
	for index, path := range helpers {
		absolute := filepath.Join(repository, path)
		if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
			t.Fatal(err)
		}
		if newSymlink && index == 0 {
			if err := os.Symlink("synthetic-target", absolute); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(absolute, []byte("// synthetic coverage helper\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sourceTestGit(t, repository, "add", "--all")
	patch, err := sourceGitBytes(t.Context(), repository, maximumSourceConfigBytes, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-renames", base, "--")
	if err != nil {
		t.Fatal(err)
	}
	// The trusted overlay owns an independent temporary index. Resetting only
	// this synthetic index also permits a later distinct selected-product edit.
	sourceTestGit(t, repository, "read-tree", base)
	commits := sourceTestCommits(strings.Repeat("a", 40))
	commits["server"] = base
	lock, err := model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	approval := &model.CompetitionTrustedInstrumentApproval{Schema: 1, ApprovalId: "synthetic-coverage-instrument", TargetEpoch: 2,
		G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, WorkCoveragePolicy: coverage.Clone(),
		InstrumentBaseCommits: commits, InstrumentBaseSourceLockSha256: lock, InstrumentBaseImageDigest: "sha256:" + strings.Repeat("b", 64),
		Patch: patch, ProtectedPaths: helpers}
	sourceCoverageInstrumentReseal(t, approval)
	return repository, base, approval
}

// Rehashing synthetic mutations does not create real operator authority; it
// makes each test reach its intended native structural/custody boundary.
func sourceCoverageInstrumentReseal(t *testing.T, approval *model.CompetitionTrustedInstrumentApproval) {
	t.Helper()
	patchDigest := sha256.Sum256(approval.Patch)
	approval.PatchSha256, approval.ApprovalSha256 = hex.EncodeToString(patchDigest[:]), ""
	var err error
	approval.ApprovalSha256, err = model.CompetitionAutomationDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.ValidateCompetitionTrustedInstrumentApproval(approval); err != nil {
		t.Fatal("synthetic instrument did not reach its intended native boundary", err)
	}
}

// Every helper must be named in the exact approval census, not merely present
// by coincidence in the selected product or the resulting temporary tree.
func TestSourceCoverageInstrumentRequiresEveryHelperPreimage(t *testing.T) {
	for missing := 0; missing < 6; missing++ {
		repository, base, approval := sourceCoverageInstrumentFixture(t, false)
		path := approval.ProtectedPaths[missing]
		approval.ProtectedPaths = slices.Delete(slices.Clone(approval.ProtectedPaths), missing, missing+1)
		sections := strings.Split(string(approval.Patch), "diff --git ")
		kept := sections[:1]
		for _, section := range sections[1:] {
			if !strings.HasPrefix(section, "a/"+path+" b/"+path+"\n") {
				kept = append(kept, section)
			}
		}
		approval.Patch = []byte(strings.Join(kept, "diff --git "))
		sourceCoverageInstrumentReseal(t, approval)
		if _, err := trustedInstrumentTree(t.Context(), repository, base, approval); err == nil || !strings.Contains(err.Error(), "omits a fixed dedicated helper preimage") {
			t.Fatal("incomplete helper approval reached native overlay application", path, err)
		}
	}
}

// A complete exact patch can create a technical tree, but a selected-product
// helper conflict is never merged away or silently restored from current main.
func TestSourceCoverageInstrumentRetainsCompleteHelpersAndRejectsDrift(t *testing.T) {
	repository, base, approval := sourceCoverageInstrumentFixture(t, false)
	tree, err := trustedInstrumentTree(t.Context(), repository, base, approval)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range approval.ProtectedPaths {
		entry, err := sourceGitText(t.Context(), repository, "--literal-pathspecs", "ls-tree", "--full-tree", tree, "--", path)
		if err != nil || !strings.HasPrefix(entry, "100644 blob ") || !strings.HasSuffix(entry, "\t"+path) {
			t.Fatal("complete synthetic instrument omitted a regular helper", path, err)
		}
	}
	path := approval.ProtectedPaths[0]
	sourceTestGit(t, repository, "add", "--", path)
	sourceTestGit(t, repository, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic selected helper conflict")
	selected := sourceTestGit(t, repository, "rev-parse", "HEAD")
	if _, err := trustedInstrumentTree(t.Context(), repository, selected, approval); err == nil || !strings.Contains(err.Error(), "protected preimage conflict") {
		t.Fatal("selected-product helper drift was repaired or ignored", err)
	}
	if sourceTestGit(t, repository, "rev-parse", "HEAD") != selected {
		t.Fatal("refused instrumentation changed the selected product")
	}
}

// A newly added symlink is not a regular Go helper even when its exact raw
// patch, path list and synthetic digests pass approval syntax validation.
func TestSourceCoverageInstrumentRejectsSymlinkPostimage(t *testing.T) {
	repository, base, approval := sourceCoverageInstrumentFixture(t, true)
	if _, err := trustedInstrumentTree(t.Context(), repository, base, approval); err == nil || !strings.Contains(err.Error(), "postimage is not a regular committed file") {
		t.Fatal("technical overlay produced a redirected trusted helper", err)
	}
}
