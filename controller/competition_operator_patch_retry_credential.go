// A local root-pinned credential adds one exact patch-retry capability without
// modifying the shared API vault or inheriting legacy operator permissions.
package controller

import (
	"crypto/subtle"
	"encoding/hex"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const CompetitionOperatorPatchRetryCredentialKind = "competition-operator-patch-retry-credential/v1"

// This contains only a bearer hash. The raw bearer remains in a separate private
// file, and the local caller authenticates it through the ordinary token path.
type CompetitionOperatorPatchRetryCredential struct {
	Schema        int       `json:"schema"`
	Kind          string    `json:"kind"`
	Capability    string    `json:"capability"`
	Name          string    `json:"name"`
	Role          string    `json:"role"`
	Sha256        string    `json:"sha256"`
	Environment   string    `json:"environment"`
	CompetitionId string    `json:"competition_id"`
	SeasonId      string    `json:"season_id"`
	RoundId       server.Id `json:"round_id"`
	JobId         server.Id `json:"job_id"`
	IssuedAt      time.Time `json:"issued_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Revoked       bool      `json:"revoked"`
}

// Registration is local-only and copy-on-write. Duplicate names or bearers,
// including revoked entries, cannot replace an existing configured capability.
func WithCompetitionOperatorPatchRetryCredential(settings *Settings, registration CompetitionOperatorPatchRetryCredential, now time.Time) (*Settings, error) {
	if settings == nil || !settings.AutomationPolicy.Enabled || registration.Schema != 1 ||
		registration.Kind != CompetitionOperatorPatchRetryCredentialKind || registration.Capability != model.CompetitionOperatorRequestedPatchRetryCapability ||
		registration.Role != competitionOperatorPatchRetryRole || !tokenNamePattern.MatchString(registration.Name) || !sha256Pattern.MatchString(registration.Sha256) ||
		registration.IssuedAt.IsZero() || now.IsZero() || now.Before(registration.IssuedAt) || !now.Before(registration.ExpiresAt) ||
		registration.Environment != settings.AutomationPolicy.Environment || registration.CompetitionId != settings.CompetitionId || registration.SeasonId != settings.AutomationPolicy.SeasonId {
		return nil, ErrAutomationDisabled
	}
	token := Token{Name: registration.Name, Role: registration.Role, Sha256: registration.Sha256, Environment: registration.Environment,
		CompetitionId: registration.CompetitionId, SeasonId: registration.SeasonId, RoundId: registration.RoundId, JobId: registration.JobId,
		ExpiresAt: registration.ExpiresAt, Revoked: registration.Revoked}
	if validateCompetitionTokenRole(token) != nil {
		return nil, ErrAutomationDisabled
	}
	for _, configured := range settings.Tokens {
		if configured.Name == token.Name || configured.Sha256 == token.Sha256 {
			return nil, ErrAutomationDisabled
		}
	}
	local := *settings
	local.Tokens = append(append([]Token(nil), settings.Tokens...), token)
	return &local, nil
}

// This role can enter only the separately typed unchanged-patch request. The
// authenticated round/job claims never become a wildcard for other recoveries.
func competitionOperatorPatchRetryPrincipalAllows(settings *Settings, principal *Principal, auth model.CompetitionJobReevaluationAuthorization) bool {
	return competitionOperatorPatchRetryPrincipalAllowsAt(settings, principal, auth, server.NowUtc())
}

// Request admission rechecks expiry after authentication; a retained Principal
// cannot outlive its credential. The explicit cut makes boundary tests exact.
func competitionOperatorPatchRetryPrincipalAllowsAt(settings *Settings, principal *Principal, auth model.CompetitionJobReevaluationAuthorization, now time.Time) bool {
	if !(settings != nil && competitionPrincipalAllows(principal, competitionOperatorPatchRetryRole, settings.CompetitionId, settings.AutomationPolicy) &&
		principal.Environment == "main" && !principal.ExpiresAt.IsZero() && !now.IsZero() && now.Before(principal.ExpiresAt) && auth.Schema == 2 &&
		auth.Kind == model.CompetitionOperatorRequestedPatchRetryAuthorizationKind && auth.IncidentClass == model.CompetitionJobReevaluationOperatorRequestedPatchRetry &&
		auth.RoundId != (server.Id{}) && auth.JobId != (server.Id{}) && principal.RoundId == auth.RoundId && principal.JobId == auth.JobId) {
		return false
	}
	principalDigest, err := hex.DecodeString(principal.tokenSha256)
	if err != nil || !sha256Pattern.MatchString(principal.tokenSha256) {
		return false
	}
	matched, matchedBearers := 0, 0
	for _, token := range settings.Tokens {
		digest, err := hex.DecodeString(token.Sha256)
		if err == nil && len(digest) == len(principalDigest) && subtle.ConstantTimeCompare(digest, principalDigest) == 1 {
			matchedBearers++
		}
		if token.Name == principal.Id {
			if token.Revoked || token.Role != competitionOperatorPatchRetryRole || validateCompetitionTokenRole(token) != nil ||
				token.Sha256 != principal.tokenSha256 || !sha256Pattern.MatchString(principal.tokenSha256) ||
				token.Environment != principal.Environment || token.CompetitionId != principal.CompetitionId || token.SeasonId != principal.SeasonId ||
				token.RoundId != principal.RoundId || token.JobId != principal.JobId || !token.ExpiresAt.Equal(principal.ExpiresAt) {
				return false
			}
			matched++
		}
	}
	return matched == 1 && matchedBearers == 1
}
