// Exact reviewed generations are buildable before a future round exists.
// Staging diagnostic statistics must never silently become production gates.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
	"gopkg.in/yaml.v3"
)

// Exercise the measurement preflight with real private Git trees and an image
// lock. A prepared generation must work before any future round is mapped.
func TestReviewedImagePreflightSelectsGenerationWithoutInventingFutureRound(t *testing.T) {
	request := sourceBuildContextFixture(t)
	manifest, err := decodeSourceManifest(request.SourceManifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, round := range manifest.EvaluationSource.Rounds {
		if round.SourceGeneration == request.SourceGeneration {
			t.Fatal("fixture fabricated a future-round mapping instead of testing a prospective image")
		}
	}
	lock, err := json.Marshal(evaluatorSourceLock{Schema: 1, Repositories: request.Proposal.Commits})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source-lock.json")
	if err := os.WriteFile(path, lock, 0o400); err != nil {
		t.Fatal(err)
	}
	repositoriesRoot := filepath.Dir(request.Proposal.StagedRoots["server"])
	if err := verifySourceEpoch(manifest, request.SourceGeneration, repositoriesRoot, path); err != nil {
		t.Fatal("image preflight confused a source generation with a competition round", err)
	}
	if err := verifySourceEpoch(manifest, 0, repositoriesRoot, path); err == nil {
		t.Fatal("new reviewed image was accepted as the unchanged incumbent generation")
	}
}

// Supply the same immutable manifest stored by the actual active pointer.
func reviewedImageFixture(t *testing.T, staging bool) (*model.CompetitionSourceActivation, *reviewedSourceProposal) {
	t.Helper()
	current, proposal, _ := reviewedProposalFixture(t)
	manifest := reviewedSourceTestManifest(t)
	if staging {
		manifest = stagingSourceTestManifest(t)
		current.Staging, proposal.Staging, current.Threshold = true, true, 0
		proposal.Significance.TakeoverMarginPercent = 0
	}
	content, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	current.Manifest = content
	return current, &proposal
}

func TestReviewedImageManifestPreservesHistoryWithoutInventingRound(t *testing.T) {
	for _, staging := range []bool{false, true} {
		current, proposal := reviewedImageFixture(t, staging)
		original := append([]byte(nil), current.Manifest...)
		before, err := decodeSourceManifest(original)
		if err != nil {
			t.Fatal(err)
		}
		content, err := reviewedSourceImageManifest(current, proposal)
		if err != nil {
			t.Fatal(err)
		}
		after, err := decodeSourceManifest(content)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(current.Manifest, original) || !reflect.DeepEqual(before.EvaluationSource.Rounds, after.EvaluationSource.Rounds) ||
			!reflect.DeepEqual(before.EvaluationSource.Epochs, after.EvaluationSource.Epochs) || len(after.EvaluationSource.Generations) != 2 ||
			!reflect.DeepEqual(before.EvaluationSource.Generations[0], after.EvaluationSource.Generations[0]) {
			t.Fatal("prospective image changed immutable history or invented a round")
		}
		if !reflect.DeepEqual(after.EvaluationSource.Generations[1], reviewedGenerationForProposal(current, proposal)) {
			t.Fatal("image generation differs from the later publication generation")
		}
	}
}

func TestReviewedImageManifestRejectsChangedAuthorityAndDependency(t *testing.T) {
	for _, mutate := range []func(*model.CompetitionSourceActivation, *reviewedSourceProposal){
		func(c *model.CompetitionSourceActivation, _ *reviewedSourceProposal) {
			c.Commits["sdk"] = strings.Repeat("c", 40)
		},
		func(_ *model.CompetitionSourceActivation, p *reviewedSourceProposal) {
			p.Commits["sdk"] = strings.Repeat("c", 40)
		},
		func(_ *model.CompetitionSourceActivation, p *reviewedSourceProposal) { p.ReviewDecisionDigest = "" },
		func(_ *model.CompetitionSourceActivation, p *reviewedSourceProposal) {
			p.BaseSourceLockSha256 = strings.Repeat("f", 64)
		},
		func(_ *model.CompetitionSourceActivation, p *reviewedSourceProposal) { p.Significance = nil },
		func(_ *model.CompetitionSourceActivation, p *reviewedSourceProposal) { p.Staging = true },
	} {
		current, proposal := reviewedImageFixture(t, false)
		mutate(current, proposal)
		if _, err := reviewedSourceImageManifest(current, proposal); err == nil {
			t.Fatal("changed source authority accepted")
		}
	}
}

// Force the exact no-op/regression case that production-style promotion
// validation previously rejected after staging had legitimately elected it.
func TestStagingReviewedImageAcceptsNonsignificantBestSafeCandidate(t *testing.T) {
	for _, improvement := range []float64{0, -10} {
		current, proposal := reviewedImageFixture(t, true)
		proposal.Significance.ObservedImprovementPercent = improvement
		proposal.Significance.OneSidedPValue = 0.9
		proposal.Significance.CandidateMeanRawScore = 110
		if _, err := reviewedSourceImageManifest(current, proposal); err != nil {
			t.Fatal("staging source promotion reintroduced statistical eligibility", err)
		}
		production := *proposal.Significance
		production.TakeoverMarginPercent = 16.1
		if validateSourceSignificanceForMode(&production, false) == nil {
			t.Fatal("production accepted staging-only eligibility")
		}
	}
}

func TestStagingSourceDiagnosticsStillRejectMalformedStatistics(t *testing.T) {
	for _, mutate := range []func(*sourceSignificance){
		func(s *sourceSignificance) { s.OneSidedPValue = 1.1 },
		func(s *sourceSignificance) { s.OneSidedPValue = -0.1 },
		func(s *sourceSignificance) { s.TakeoverMarginPercent = 16.1 },
		func(s *sourceSignificance) { s.CandidateSampleVariance = -1 },
	} {
		significance := validSourceSignificance()
		significance.TakeoverMarginPercent = 0
		mutate(significance)
		if validateSourceSignificanceForMode(significance, true) == nil {
			t.Fatal("malformed staging statistics accepted")
		}
	}
}
