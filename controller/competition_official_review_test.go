package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"net/http"
	"net/http/httptest"
	"testing"
)

type competitionOfficialReviewTestStore struct {
	*fakeStore
	calls int
}

func (self *competitionOfficialReviewTestStore) ImportOfficialReview(context.Context, *Settings, string, model.CompetitionOfficialReviewImport) (*model.CompetitionIntegrityReviewResult, error) {
	self.calls++
	return &model.CompetitionIntegrityReviewResult{}, nil
}

func TestCompetitionOfficialReviewRequiresExactGrantAndScopedOperator(t *testing.T) {
	settings, _, _, _ := competitionIntakeFirstTestValues()
	store := &competitionOfficialReviewTestStore{fakeStore: &fakeStore{}}
	roles := []string{competitionReviewerRole, competitionReviewOperatorRole, competitionSchedulerRole}
	for _, role := range roles {
		digest := sha256.Sum256([]byte("synthetic-" + role))
		settings.Tokens = append(settings.Tokens, Token{Name: "synthetic-" + role, Role: role, Sha256: hex.EncodeToString(digest[:]), Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId})
	}
	request := model.CompetitionOfficialReviewImport{RoundId: server.NewId()}
	content, _ := json.Marshal(request)
	digest, _ := model.CompetitionAutomationDigest(request)
	for _, role := range roles {
		for _, pin := range []string{"", digest} {
			settings.OfficialReviewImportSha256 = pin
			service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
			response := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/competition/v2/official-review-imports", bytes.NewReader(content))
			r.Header.Set("Authorization", "Bearer synthetic-"+role)
			before := store.calls
			competitionOfficialReviewImportHandler(service, response, r)
			allowed := role == competitionReviewOperatorRole && pin == digest
			if allowed {
				if response.Code != http.StatusOK || store.calls != before+1 {
					t.Fatal("pinned scoped operator refused", response.Code, response.Body.String())
				}
			} else if response.Code != http.StatusForbidden || store.calls != before {
				t.Fatal("unscoped or unpinned import reached store", role, response.Code)
			}
		}
	}
}

func TestCompetitionOfficialReviewCannotUseOrdinaryReviewerStore(t *testing.T) {
	_, err := (PostgresStore{}).RecordIntegrityReview(context.Background(), nil, server.NewId(), "synthetic-reviewer", model.CompetitionIntegrityReviewRequest{OfficialReviewOverride: &model.CompetitionOfficialReviewOverride{}})
	if !errors.Is(err, ErrConflict) {
		t.Fatal("ordinary reviewer reached override storage", err)
	}
}
