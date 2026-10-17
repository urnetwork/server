// Capability tests use synthetic identities and never contact external services.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/model"
)

// New roles have no implied authority over one another or legacy operator routes.
func TestCompetitionV2CapabilitiesAreSeparate(t *testing.T) {
	policy := model.CompetitionAutomationPolicy{Environment: "test-env", SeasonId: "test-season"}
	roles := []string{"submitter", "operator", competitionSchedulerRole, competitionReviewerRole, competitionSettlementRole, competitionSourcePromoterRole}
	for _, actualRole := range roles {
		principal := &Principal{Id: "test-actor", Role: actualRole, Environment: policy.Environment, CompetitionId: "test-competition", SeasonId: policy.SeasonId}
		for _, requiredRole := range roles[2:] {
			if got := competitionPrincipalAllows(principal, requiredRole, "test-competition", policy); got != (actualRole == requiredRole) {
				t.Fatalf("role %q allows %q = %v", actualRole, requiredRole, got)
			}
		}
	}
}

// Each scope component is an authority boundary, not only metadata.
func TestCompetitionV2CapabilitiesRejectOtherScopes(t *testing.T) {
	policy := model.CompetitionAutomationPolicy{Environment: "test-env", SeasonId: "test-season"}
	principals := []*Principal{
		nil,
		{Id: "test-actor", Role: competitionReviewerRole, Environment: "other-env", CompetitionId: "test-competition", SeasonId: policy.SeasonId},
		{Id: "test-actor", Role: competitionReviewerRole, Environment: policy.Environment, CompetitionId: "other-competition", SeasonId: policy.SeasonId},
		{Id: "test-actor", Role: competitionReviewerRole, Environment: policy.Environment, CompetitionId: "test-competition", SeasonId: "other-season"},
		{Id: "", Role: competitionReviewerRole, Environment: policy.Environment, CompetitionId: "test-competition", SeasonId: policy.SeasonId},
	}
	for _, principal := range principals {
		if competitionPrincipalAllows(principal, competitionReviewerRole, "test-competition", policy) {
			t.Fatalf("out-of-scope principal accepted: %+v", principal)
		}
	}
}

// Revocation takes effect on the next authentication and does not preserve a cached principal.
func TestCompetitionV2AuthenticationRejectsRevokedToken(t *testing.T) {
	secret := "synthetic-reviewer-bearer-for-tests"
	digest := sha256.Sum256([]byte(secret))
	settings := &Settings{Tokens: []Token{{Name: "test-reviewer", Role: competitionReviewerRole, Sha256: hex.EncodeToString(digest[:]), Environment: "test-env", CompetitionId: "test-competition", SeasonId: "test-season"}}}
	request := httptest.NewRequest(http.MethodPost, "/competition/v2/rounds/test/reviews", nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	principal, ok := Authenticate(request, settings)
	if !ok || principal.Id != "test-reviewer" || principal.SeasonId != "test-season" {
		t.Fatalf("scoped authentication = %+v, %v", principal, ok)
	}
	settings.Tokens[0].Revoked = true
	if _, ok := Authenticate(request, settings); ok {
		t.Fatal("revoked token authenticated")
	}
}

// Neither ordering nor an unrevoked alias can turn one bearer into a second capability.
func TestCompetitionV2AuthenticationRejectsCredentialAliases(t *testing.T) {
	secret := "synthetic-aliased-bearer"
	digest := sha256.Sum256([]byte(secret))
	first := Token{Name: "test-scheduler", Role: competitionSchedulerRole, Sha256: hex.EncodeToString(digest[:]), Environment: "test-env", CompetitionId: "test-competition", SeasonId: "test-season"}
	second := first
	second.Name, second.Role, second.SeasonId = "test-reviewer", competitionReviewerRole, "other-season"
	request := httptest.NewRequest(http.MethodPost, "/competition/v2/holds", nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	for _, revoked := range []bool{false, true} {
		first.Revoked = revoked
		for _, tokens := range [][]Token{{first, second}, {second, first}} {
			settings := validSettings()
			settings.Tokens = append(settings.Tokens, tokens...)
			if err := settings.Validate(); err == nil {
				t.Fatal("duplicate credential digests passed settings validation")
			}
			if principal, ok := Authenticate(request, settings); ok {
				t.Fatalf("aliased credential acquired authority: %+v", principal)
			}
		}
	}
}

// A newly introduced token role must not pass the legacy submitter/operator gate.
func TestCompetitionV2RolesCannotUseLegacyPrivateRoutes(t *testing.T) {
	settings := validSettings()
	secret := "synthetic-settlement-bearer-for-tests"
	digest := sha256.Sum256([]byte(secret))
	settings.Tokens = append(settings.Tokens, Token{Name: "test-settlement", Role: competitionSettlementRole, Sha256: hex.EncodeToString(digest[:]), Environment: "test-env", CompetitionId: "test-competition", SeasonId: "test-season"})
	service := newServiceWithImageDigest(settings, &fakeStore{}, testApiImageDigest(), nil)
	for _, operator := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodGet, "/competition/readyz", nil)
		request.Header.Set("Authorization", "Bearer "+secret)
		response := httptest.NewRecorder()
		if _, ok := requirePrincipal(response, request, service, operator); ok || response.Code != http.StatusUnauthorized {
			t.Fatalf("settlement token passed legacy boundary: operator=%v, status=%d", operator, response.Code)
		}
	}
}

// Explicit scoped roles may be provisioned before activation, but cannot be unscoped.
func TestCompetitionV2TokenConfigurationRequiresScope(t *testing.T) {
	for _, role := range []string{competitionSchedulerRole, competitionReviewerRole, competitionSettlementRole, competitionSourcePromoterRole} {
		token := Token{Name: "test-actor", Role: role}
		if validateCompetitionTokenRole(token) == nil {
			t.Fatalf("unscoped %s accepted", role)
		}
		token.Environment, token.CompetitionId, token.SeasonId = "test-env", "test-competition", "test-season"
		if err := validateCompetitionTokenRole(token); err != nil {
			t.Fatal(err)
		}
	}
}

// A capability hold cannot pause another subsystem by changing only its request kind.
func TestCompetitionV2HoldCapabilitiesDoNotOverlap(t *testing.T) {
	roles := map[string]string{"admission": competitionSchedulerRole, "evaluation": competitionSchedulerRole, "source_activation": competitionSourcePromoterRole, "reward_delivery": competitionSettlementRole}
	for kind, expected := range roles {
		if actual := competitionHoldRole(kind); actual != expected {
			t.Fatalf("hold %s requires %s, got %s", kind, expected, actual)
		}
	}
	if competitionHoldRole("all") != "" {
		t.Fatal("unbounded hold acquired authority")
	}
}

// One token never acquires both evaluation invalidation and source-compromise authority.
func TestCompetitionV2IncidentCapabilitiesAreIndependent(t *testing.T) {
	if competitionIncidentRole(model.CompetitionAutomationIncidentArgs{InvalidatesEvaluation: true}) != competitionSchedulerRole {
		t.Fatal("evaluation incident lost scheduler authority")
	}
	if competitionIncidentRole(model.CompetitionAutomationIncidentArgs{SourceCompromised: true}) != competitionSourcePromoterRole {
		t.Fatal("source incident lost source authority")
	}
	if competitionIncidentRole(model.CompetitionAutomationIncidentArgs{InvalidatesEvaluation: true, SourceCompromised: true}) != "" {
		t.Fatal("one token combined source and evaluation authority")
	}
}
