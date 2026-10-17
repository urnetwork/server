// Separates v2 control-plane capabilities from legacy operator and submitter tokens.
package controller

import (
	"fmt"
	"net/http"

	"github.com/urnetwork/server/model"
)

const (
	competitionSchedulerRole      = "scheduler"
	competitionReviewerRole       = "reviewer"
	competitionSettlementRole     = "settlement"
	competitionSourcePromoterRole = "source_promoter"
)

// Legacy token shapes remain valid. New capabilities require an explicit scope
// and never inherit the legacy operator's evaluator or hidden-evidence access.
func validateCompetitionTokenRole(token Token) error {
	switch token.Role {
	case "submitter", "operator":
		return nil
	case competitionSchedulerRole, competitionReviewerRole, competitionSettlementRole, competitionSourcePromoterRole:
		if !tokenNamePattern.MatchString(token.Environment) ||
			!tokenNamePattern.MatchString(token.CompetitionId) || !tokenNamePattern.MatchString(token.SeasonId) {
			return fmt.Errorf("competition token %q requires an explicit environment, competition, and season scope", token.Name)
		}
		return nil
	default:
		return fmt.Errorf("competition token %q has invalid role", token.Name)
	}
}

// Checks exact scope and role; an operator token is deliberately not a wildcard.
func competitionPrincipalAllows(principal *Principal, role string, competitionId string, policy model.CompetitionAutomationPolicy) bool {
	return principal != nil && principal.Id != "" && principal.Role == role &&
		principal.Environment == policy.Environment && principal.CompetitionId == competitionId &&
		principal.SeasonId == policy.SeasonId && policy.Environment != "" && policy.SeasonId != ""
}

// Authentication is checked before any round lookup or private request decoding.
func requireCompetitionV2Principal(w http.ResponseWriter, r *http.Request, service *Service, role string) (*Principal, bool) {
	settings, err := service.Settings()
	if err != nil {
		writeCompetitionJson(w, http.StatusServiceUnavailable, infrastructureError("configuration_unavailable", "competition configuration is not ready"))
		return nil, false
	}
	principal, ok := Authenticate(r, settings)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="competition"`)
		writeCompetitionJson(w, http.StatusUnauthorized, &CompetitionError{
			Kind: "auth", Code: "unauthorized", Message: "missing or invalid competition bearer token", Retriable: false,
		})
		return nil, false
	}
	if !competitionPrincipalAllows(principal, role, settings.CompetitionId, settings.AutomationPolicy) {
		writeCompetitionJson(w, http.StatusForbidden, &CompetitionError{
			Kind: "auth", Code: "scope_denied", Message: "this token does not authorize the requested competition capability", Retriable: false,
		})
		return nil, false
	}
	return principal, true
}
