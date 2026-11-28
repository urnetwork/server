// Protected custody uses synthetic archive versions and no production identity.
package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// An empty stderr leaf is still an exact original, not a missing report. The
// historical single-call report API retains its nonempty-document requirement.
func TestCompetitionReviewHandoffRetainsEmptyOriginalWithoutWideningLegacy(t *testing.T) {
	store := &winnerReviewEvidenceStore{}
	settings := &Settings{CompetitionId: "synthetic-review", AutomationPolicy: model.CompetitionAutomationPolicy{Environment: "test", SeasonId: "synthetic-season", WinnerReviewPolicy: model.CompetitionSnowWinnerReviewPolicy},
		RetainUntil: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), artifactArchive: &blobArtifactArchive{store: store}}
	round, job := server.NewId(), server.NewId()
	digest := sha256.Sum256(nil)
	sha := hex.EncodeToString(digest[:])
	if _, err := RetainCompetitionWinnerReviewEvidence(t.Context(), settings, round, job, nil, sha); err == nil {
		t.Fatal("legacy report admitted an empty document")
	}
	retained, err := RetainCompetitionReviewHandoffEvidence(t.Context(), settings, round, job, nil, sha)
	if err != nil || retained.Sha256 != sha || retained.VersionId == "" {
		t.Fatal("empty original lost its exact protected identity", err)
	}
	value, err := LoadCompetitionReviewHandoffEvidence(t.Context(), settings, round, job, model.CompetitionReviewHandoffRetention{Key: retained.Key, VersionId: retained.VersionId, Sha256: retained.Sha256, RetainUntil: retained.RetainUntil}, 0)
	if err != nil || len(value) != 0 {
		t.Fatal("empty protected original was not independently replayable", err)
	}
	store.corrupt = true
	if _, err := RetainCompetitionReviewHandoffEvidence(t.Context(), settings, round, job, nil, sha); err == nil {
		t.Fatal("empty original bypassed exact-version readback")
	}
}

// Neither a mutable latest object, another candidate namespace nor a claimed
// digest substitutes for the protected original version being presented.
func TestCompetitionReviewHandoffReloadBindsScopeVersionLengthAndDigest(t *testing.T) {
	store := &winnerReviewEvidenceStore{}
	settings := &Settings{CompetitionId: "synthetic-review", AutomationPolicy: model.CompetitionAutomationPolicy{Environment: "test", SeasonId: "synthetic-season", WinnerReviewPolicy: model.CompetitionSnowWinnerReviewPolicy},
		RetainUntil: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), artifactArchive: &blobArtifactArchive{store: store}}
	round, job := server.NewId(), server.NewId()
	content := []byte("synthetic exact visible transport")
	digest := sha256.Sum256(content)
	retained, err := RetainCompetitionReviewHandoffEvidence(t.Context(), settings, round, job, content, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	receipt := model.CompetitionReviewHandoffRetention{Key: retained.Key, VersionId: retained.VersionId, Sha256: retained.Sha256, RetainUntil: retained.RetainUntil}
	actual, err := LoadCompetitionReviewHandoffEvidence(t.Context(), settings, round, job, receipt, int64(len(content)))
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatal("protected original reload failed", err)
	}
	for _, mutate := range []func(*model.CompetitionReviewHandoffRetention){
		func(value *model.CompetitionReviewHandoffRetention) { value.Key += "/other" },
		func(value *model.CompetitionReviewHandoffRetention) { value.VersionId = "other-synthetic-version" },
		func(value *model.CompetitionReviewHandoffRetention) { value.VersionId = "" },
	} {
		changed := receipt
		mutate(&changed)
		if _, err := LoadCompetitionReviewHandoffEvidence(t.Context(), settings, round, job, changed, int64(len(content))); err == nil {
			t.Fatal("changed protected original identity was accepted")
		}
	}
	if _, err := LoadCompetitionReviewHandoffEvidence(t.Context(), settings, round, server.NewId(), receipt, int64(len(content))); err == nil {
		t.Fatal("another candidate read this private namespace")
	}
	if _, err := LoadCompetitionReviewHandoffEvidence(t.Context(), settings, round, job, receipt, int64(len(content)-1)); err == nil {
		t.Fatal("changed original length was accepted")
	}
	store.corrupt = true
	if _, err := LoadCompetitionReviewHandoffEvidence(t.Context(), settings, round, job, receipt, int64(len(content))); err == nil {
		t.Fatal("changed original bytes were accepted")
	}
}
