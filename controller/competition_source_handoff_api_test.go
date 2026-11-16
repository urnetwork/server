// The additional human barrier has its own exact scoped capability. It does
// not silently widen automated publishers or historical operator credentials.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// The storage witness proves authentication order only, not source approval.
type competitionSourceHandoffTestStore struct {
	*fakeStore
	calls int
	actor string
}

func (self *competitionSourceHandoffTestStore) RecordSourceOperatorHandoff(_ context.Context, _ *Settings, actor string, request model.CompetitionSourceOperatorHandoffArgs) (*model.CompetitionSourceOperatorHandoff, error) {
	self.calls++
	self.actor = actor
	return &model.CompetitionSourceOperatorHandoff{Request: request}, nil
}

// A malformed body never reaches parsing/storage before native authentication;
// only the independently scoped human role may append a handoff request.
func TestCompetitionIntakeFirstHandoffApiRequiresIndependentScopedHuman(t *testing.T) {
	settings, _, _, _ := competitionIntakeFirstTestValues()
	store := &competitionSourceHandoffTestStore{fakeStore: &fakeStore{}}
	roles := []string{"operator", "submitter", competitionSourcePromoterRole, competitionReviewerRole, competitionSchedulerRole, competitionSourceHandoffRole}
	for _, role := range roles {
		credential := "synthetic-" + role + "-credential"
		digest := sha256.Sum256([]byte(credential))
		settings.Tokens = append(settings.Tokens, Token{Name: "synthetic-" + role, Role: role, Sha256: hex.EncodeToString(digest[:]),
			Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId})
	}
	service := newServiceWithImageDigest(settings, store, testApiImageDigest(), nil)
	if _, err := service.Settings(); err != nil {
		t.Fatal(err)
	}
	unauthenticated := httptest.NewRecorder()
	competitionSourceHandoffHandler(service, unauthenticated, httptest.NewRequest(http.MethodPost, "/competition/v2/source-handoffs", strings.NewReader("{")))
	if unauthenticated.Code != http.StatusUnauthorized || store.calls != 0 {
		t.Fatal("handoff request was parsed or stored before authentication", unauthenticated.Code, store.calls)
	}
	request := model.CompetitionSourceOperatorHandoffArgs{TargetEpoch: 2, RewardDecisionSha256: strings.Repeat("a", 64), ProductSourceLockSha256: strings.Repeat("b", 64),
		Disposition: "approve_no_winner_carry", IdempotencyKey: "synthetic-empty-ranking", EvidenceSha256: strings.Repeat("c", 64)}
	content, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		response := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/competition/v2/source-handoffs", bytes.NewReader(content))
		r.Header.Set("Authorization", "Bearer synthetic-"+role+"-credential")
		competitionSourceHandoffHandler(service, response, r)
		if role != competitionSourceHandoffRole {
			if response.Code != http.StatusForbidden || store.calls != 0 {
				t.Fatal("nonhuman capability acquired handoff authority", role, response.Code, store.calls)
			}
		} else if response.Code != http.StatusOK || store.calls != 1 || store.actor != "synthetic-"+role {
			t.Fatal("exact independent handoff capability was rejected", response.Code, store.calls, store.actor)
		}
	}
}

// The new capability requires its own complete scope and cannot be copied to
// a second environment/competition/season by editing only the route body.
func TestCompetitionIntakeFirstHandoffRoleHasNoUnscopedFallback(t *testing.T) {
	for _, token := range []Token{
		{Name: "synthetic-human", Role: competitionSourceHandoffRole},
		{Name: "synthetic-human", Role: competitionSourceHandoffRole, Environment: "synthetic", CompetitionId: "synthetic-competition"},
	} {
		if validateCompetitionTokenRole(token) == nil {
			t.Fatal("unscoped human source capability accepted")
		}
	}
	settings, _, _, _ := competitionIntakeFirstTestValues()
	principal := &Principal{Id: "synthetic-human", Role: competitionSourceHandoffRole, Environment: settings.AutomationPolicy.Environment,
		CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId}
	if !competitionPrincipalAllows(principal, competitionSourceHandoffRole, settings.CompetitionId, settings.AutomationPolicy) {
		t.Fatal("exact scoped human capability rejected")
	}
	principal.SeasonId = "other-synthetic-season"
	if competitionPrincipalAllows(principal, competitionSourceHandoffRole, settings.CompetitionId, settings.AutomationPolicy) {
		t.Fatal("another season reused the handoff capability")
	}
}
