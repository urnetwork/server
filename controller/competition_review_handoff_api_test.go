package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type competitionReviewHandoffTestStore struct {
	*fakeStore
	calls int
	actor string
}

func (self *competitionReviewHandoffTestStore) ImportReviewHandoff(_ context.Context, _ *Settings, actor string, _ model.CompetitionReviewHandoffImport) (*model.CompetitionIntegrityReviewResult, error) {
	self.calls++
	self.actor = actor
	return &model.CompetitionIntegrityReviewResult{}, nil
}

func TestCompetitionReviewHandoffImportRequiresSeparateScopedOperator(t *testing.T) {
	settings, _, _, _ := competitionIntakeFirstTestValues()
	store := &competitionReviewHandoffTestStore{fakeStore: &fakeStore{}}
	roles := []string{"operator", "submitter", competitionReviewerRole, competitionSchedulerRole, competitionSourcePromoterRole, competitionSourceHandoffRole, competitionReviewOperatorRole}
	for _, role := range roles {
		digest := sha256.Sum256([]byte("synthetic-" + role))
		settings.Tokens = append(settings.Tokens, Token{Name: "synthetic-" + role, Role: role, Sha256: hex.EncodeToString(digest[:]), Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId})
	}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	if _, err := service.Settings(); err != nil {
		t.Fatal(err)
	}
	request := model.CompetitionReviewHandoffImport{RoundId: server.NewId(), HandoffId: server.NewId(), DecisionSha256: strings.Repeat("a", 64), ProposalSha256: strings.Repeat("b", 64), EvidenceSha256: strings.Repeat("c", 64), Disposition: "publish_exact_model_proposal"}
	content, _ := json.Marshal(request)
	unauth := httptest.NewRecorder()
	competitionReviewHandoffImportHandler(service, unauth, httptest.NewRequest(http.MethodPost, "/competition/v2/review-handoff-imports", strings.NewReader("{")))
	if unauth.Code != http.StatusUnauthorized || store.calls != 0 {
		t.Fatal("decoded/imported before authentication")
	}
	for _, role := range roles {
		response := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/competition/v2/review-handoff-imports", bytes.NewReader(content))
		r.Header.Set("Authorization", "Bearer synthetic-"+role)
		competitionReviewHandoffImportHandler(service, response, r)
		if role != competitionReviewOperatorRole {
			if response.Code != http.StatusForbidden || store.calls != 0 {
				t.Fatal("nonoperator obtained import authority", role, response.Code)
			}
		} else if response.Code != http.StatusOK || store.calls != 1 || store.actor != "synthetic-"+role {
			t.Fatal("independent operator refused", response.Code)
		}
	}
	if validateCompetitionTokenRole(Token{Name: "synthetic", Role: competitionReviewOperatorRole}) == nil {
		t.Fatal("unscoped operator allowed")
	}
}
