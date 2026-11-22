// Synthetic credentials exercise actual authentication, capability gates and
// local copy ownership without creating a production credential or grant.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The bearer is visibly synthetic; its registration binds one generated job
// and round and never copies an operational identity into a test fixture.
func competitionPatchRetryCredentialForTest(t testing.TB, settings *Settings, auth model.CompetitionJobReevaluationAuthorization) (CompetitionOperatorPatchRetryCredential, *http.Request, time.Time) {
	t.Helper()
	now := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	raw := "synthetic-exact-patch-retry-bearer"
	digest := sha256.Sum256([]byte(raw))
	registration := CompetitionOperatorPatchRetryCredential{Schema: 1, Kind: CompetitionOperatorPatchRetryCredentialKind,
		Capability: model.CompetitionOperatorRequestedPatchRetryCapability, Name: "synthetic-exact-patch-retry", Role: competitionOperatorPatchRetryRole,
		Sha256: hex.EncodeToString(digest[:]), Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId,
		SeasonId: settings.AutomationPolicy.SeasonId, RoundId: auth.RoundId, JobId: auth.JobId, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	request := httptest.NewRequest(http.MethodPost, "/competition/v2/synthetic", nil)
	request.Header.Set("Authorization", "Bearer "+raw)
	return registration, request, now
}

// Local registration cannot alter or alias the shared token list. The real
// authentication path preserves exact claims instead of fabricating a role.
func TestCompetitionPatchRetryCredentialAuthenticatesExactClaimsWithoutSharedMutation(t *testing.T) {
	settings, _, request := competitionOperatorPatchRetryRequestForTest(t)
	settings.AutomationPolicy.Environment = "main"
	registration, bearer, now := competitionPatchRetryCredentialForTest(t, settings, request.Authorization)
	original := append([]Token(nil), settings.Tokens...)
	local, err := WithCompetitionOperatorPatchRetryCredential(settings, registration, now)
	if err != nil || local == settings || !reflect.DeepEqual(settings.Tokens, original) || len(local.Tokens) != len(original)+1 {
		t.Fatal("registration altered shared settings or lost local ownership", err)
	}
	principal, ok := authenticateCompetitionAt(bearer, local, now)
	if !ok || principal.Id != registration.Name || principal.Role != registration.Role || principal.RoundId != registration.RoundId ||
		principal.JobId != registration.JobId || !principal.ExpiresAt.Equal(registration.ExpiresAt) || principal.Environment != registration.Environment ||
		principal.CompetitionId != registration.CompetitionId || principal.SeasonId != registration.SeasonId {
		t.Fatal("actual authentication lost exact retry claims")
	}
	local.Tokens[0].Name = "synthetic-local-change"
	if !reflect.DeepEqual(settings.Tokens, original) {
		t.Fatal("local registration shared token backing storage")
	}
}

// Expiry is exclusive. Revocation, wrong bearers and duplicate registrations
// cannot authenticate by changing configuration order or aliasing another role.
func TestCompetitionPatchRetryCredentialRejectsExpiryRevocationAndAliases(t *testing.T) {
	settings, _, request := competitionOperatorPatchRetryRequestForTest(t)
	settings.AutomationPolicy.Environment = "main"
	registration, bearer, now := competitionPatchRetryCredentialForTest(t, settings, request.Authorization)
	local, err := WithCompetitionOperatorPatchRetryCredential(settings, registration, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []time.Time{registration.ExpiresAt, registration.ExpiresAt.Add(time.Nanosecond)} {
		if _, ok := authenticateCompetitionAt(bearer, local, cut); ok {
			t.Fatal("expired patch-retry bearer authenticated")
		}
	}
	local.Tokens[len(local.Tokens)-1].Revoked = true
	if _, ok := authenticateCompetitionAt(bearer, local, now); ok {
		t.Fatal("revoked patch-retry bearer authenticated")
	}
	local.Tokens[len(local.Tokens)-1].Revoked = false
	alias := local.Tokens[len(local.Tokens)-1]
	alias.Name = "synthetic-alias"
	alias.Revoked = true
	local.Tokens = append(local.Tokens, alias)
	if _, ok := authenticateCompetitionAt(bearer, local, now); ok {
		t.Fatal("duplicate bearer selected the unrevoked capability")
	}
	if _, err := WithCompetitionOperatorPatchRetryCredential(local, registration, now); err == nil {
		t.Fatal("local registration replaced an existing bearer")
	}
	bearer.Header.Set("Authorization", "Bearer synthetic-other-bearer")
	if _, ok := authenticateCompetitionAt(bearer, local, now); ok {
		t.Fatal("wrong bearer acquired retry scope")
	}
}

// Every registration field is authority. Expired/future-issued or incomplete
// records fail without changing the shared configuration or legacy role rules.
func TestCompetitionPatchRetryCredentialRegistrationRejectsIncompleteOrChangedAuthority(t *testing.T) {
	settings, _, request := competitionOperatorPatchRetryRequestForTest(t)
	settings.AutomationPolicy.Environment = "main"
	registration, _, now := competitionPatchRetryCredentialForTest(t, settings, request.Authorization)
	for _, mutate := range []func(*CompetitionOperatorPatchRetryCredential){
		func(r *CompetitionOperatorPatchRetryCredential) { r.Schema++ },
		func(r *CompetitionOperatorPatchRetryCredential) { r.Kind = "synthetic-other-kind" },
		func(r *CompetitionOperatorPatchRetryCredential) {
			r.Capability = model.CompetitionDistributedJobReevaluationCapability
		},
		func(r *CompetitionOperatorPatchRetryCredential) { r.Role = "operator" },
		func(r *CompetitionOperatorPatchRetryCredential) { r.Name = "" },
		func(r *CompetitionOperatorPatchRetryCredential) { r.Sha256 = "" },
		func(r *CompetitionOperatorPatchRetryCredential) { r.Environment = "synthetic-other-env" },
		func(r *CompetitionOperatorPatchRetryCredential) { r.CompetitionId = "synthetic-other-competition" },
		func(r *CompetitionOperatorPatchRetryCredential) { r.SeasonId = "synthetic-other-season" },
		func(r *CompetitionOperatorPatchRetryCredential) { r.RoundId = server.Id{} },
		func(r *CompetitionOperatorPatchRetryCredential) { r.JobId = server.Id{} },
		func(r *CompetitionOperatorPatchRetryCredential) { r.IssuedAt = time.Time{} },
		func(r *CompetitionOperatorPatchRetryCredential) { r.IssuedAt = now.Add(time.Nanosecond) },
		func(r *CompetitionOperatorPatchRetryCredential) { r.ExpiresAt = now },
		func(r *CompetitionOperatorPatchRetryCredential) { r.ExpiresAt = time.Time{} },
	} {
		changed := registration
		mutate(&changed)
		if local, err := WithCompetitionOperatorPatchRetryCredential(settings, changed, now); err == nil || local != nil {
			t.Fatal("changed registration acquired authority")
		}
	}
	for _, role := range []string{"submitter", "operator", competitionSchedulerRole, competitionReviewerRole, competitionSettlementRole,
		competitionSourcePromoterRole, competitionSourceHandoffRole, competitionReviewOperatorRole} {
		if validateCompetitionTokenRole(Token{Name: "synthetic-misleading-scope", Role: role, RoundId: registration.RoundId, JobId: registration.JobId, ExpiresAt: registration.ExpiresAt}) == nil {
			t.Fatal("another role advertised unenforced job-level scope", role)
		}
	}
}

// The dedicated bearer accepts exactly the typed patch class and original job
// scope. It cannot authorize either distributed infrastructure or singleton work.
func TestCompetitionPatchRetryCredentialRequestRequiresExactJobRoundAndClass(t *testing.T) {
	settings, _, request := competitionOperatorPatchRetryRequestForTest(t)
	settings.AutomationPolicy.Environment, request.Authorization.Environment = "main", "main"
	request.AuthorizationSha256 = competitionRecoveryTestDigest(t, request.Authorization)
	registration, bearer, now := competitionPatchRetryCredentialForTest(t, settings, request.Authorization)
	registration.ExpiresAt = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)
	local, err := WithCompetitionOperatorPatchRetryCredential(settings, registration, now)
	if err != nil {
		t.Fatal(err)
	}
	principal, ok := authenticateCompetitionAt(bearer, local, now)
	if !ok || validateCompetitionDistributedRecoveryRequest(local, principal, request) != nil {
		t.Fatal("dedicated bearer could not authorize its exact typed request")
	}
	if competitionOperatorPatchRetryPrincipalAllowsAt(local, principal, request.Authorization, registration.ExpiresAt) {
		t.Fatal("retained principal outlived its credential")
	}
	local.Tokens[len(local.Tokens)-1].Revoked = true
	if validateCompetitionDistributedRecoveryRequest(local, principal, request) == nil {
		t.Fatal("revoked-after-authentication credential acquired grant authority")
	}
	local.Tokens[len(local.Tokens)-1].Revoked = false
	for _, mutate := range []func(*Principal){
		func(p *Principal) { p.Environment = "synthetic-other-env" },
		func(p *Principal) { p.CompetitionId = "synthetic-other-competition" },
		func(p *Principal) { p.SeasonId = "synthetic-other-season" },
		func(p *Principal) { p.RoundId = server.NewId() },
		func(p *Principal) { p.JobId = server.NewId() },
		func(p *Principal) { p.RoundId = server.Id{} },
		func(p *Principal) { p.JobId = server.Id{} },
		func(p *Principal) { p.ExpiresAt = time.Time{} },
		func(p *Principal) { p.tokenSha256 = "" },
	} {
		changed := *principal
		mutate(&changed)
		if validateCompetitionDistributedRecoveryRequest(local, &changed, request) == nil {
			t.Fatal("wrong principal scope acquired patch retry")
		}
	}
	for _, incident := range []string{model.CompetitionJobReevaluationDistributedInfrastructure, model.CompetitionJobReevaluationArchiveInventory,
		model.CompetitionJobReevaluationLegacyArchiveBudget, model.CompetitionJobReevaluationHeartbeatBudget, model.CompetitionJobReevaluationBackingService,
		model.CompetitionJobReevaluationOperatorInterruption} {
		changed := competitionRecoveryTestCopy(t, request)
		changed.Authorization.IncidentClass = incident
		changed.Authorization.Kind = model.CompetitionDistributedJobReevaluationAuthorizationKind
		changed.AuthorizationSha256 = competitionRecoveryTestDigest(t, changed.Authorization)
		if validateCompetitionDistributedRecoveryRequest(local, principal, changed) == nil || validateCompetitionReevaluationRequest(local, principal, changed) == nil {
			t.Fatal("patch-only bearer acquired another recovery class", incident)
		}
	}
}

// An authenticated retry bearer has no legacy private API or unrelated v2
// control capability, even if a caller supplies an otherwise matching scope.
func TestCompetitionPatchRetryCredentialCannotUseLegacyOrV2ApiCapabilities(t *testing.T) {
	settings := automationApiSettings()
	settings.AutomationPolicy.Environment = "main"
	registration, bearer, now := competitionPatchRetryCredentialForTest(t, settings, model.CompetitionJobReevaluationAuthorization{RoundId: server.NewId(), JobId: server.NewId()})
	local, err := WithCompetitionOperatorPatchRetryCredential(settings, registration, now)
	if err != nil {
		t.Fatal(err)
	}
	// API authentication uses the real wall-clock entry point; this fixed far
	// future credential avoids timing margins while exact expiry is tested above.
	local.Tokens[len(local.Tokens)-1].ExpiresAt = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)
	service := newServiceWithImageDigest(local, &fakeStore{}, testApiImageDigest(), nil)
	for _, operator := range []bool{false, true} {
		response := httptest.NewRecorder()
		if _, ok := requirePrincipal(response, bearer, service, operator); ok || response.Code != http.StatusUnauthorized {
			t.Fatal("patch retry obtained legacy API access", operator, response.Code)
		}
	}
	for _, role := range []string{competitionSchedulerRole, competitionReviewerRole, competitionSettlementRole, competitionSourcePromoterRole,
		competitionSourceHandoffRole, competitionReviewOperatorRole, "submitter", "operator"} {
		response := httptest.NewRecorder()
		if _, ok := requireCompetitionV2Principal(response, bearer, service, role); ok || response.Code != http.StatusForbidden {
			t.Fatal("patch retry obtained another API capability", role, response.Code)
		}
	}
}

// A retained Principal must not bypass the ordinary authentication rule when
// any same-bearer alias appears later, regardless of name, role or revocation.
func TestCompetitionPatchRetryCredentialRetainedPrincipalRejectsBearerAliasesInEveryOrder(t *testing.T) {
	settings, _, request := competitionOperatorPatchRetryRequestForTest(t)
	settings.AutomationPolicy.Environment, request.Authorization.Environment = "main", "main"
	request.AuthorizationSha256 = competitionRecoveryTestDigest(t, request.Authorization)
	registration, bearer, now := competitionPatchRetryCredentialForTest(t, settings, request.Authorization)
	registration.ExpiresAt = time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)
	local, err := WithCompetitionOperatorPatchRetryCredential(settings, registration, now)
	if err != nil {
		t.Fatal(err)
	}
	principal, ok := authenticateCompetitionAt(bearer, local, now)
	if !ok || validateCompetitionDistributedRecoveryRequest(local, principal, request) != nil {
		t.Fatal("healthy dedicated credential could not enter its exact request")
	}
	primary := local.Tokens[len(local.Tokens)-1]
	for _, role := range []string{competitionOperatorPatchRetryRole, "operator", competitionReviewerRole} {
		for _, revoked := range []bool{false, true} {
			for _, upper := range []bool{false, true} {
				alias := primary
				alias.Name, alias.Role, alias.Revoked = "synthetic-other-bearer-alias", role, revoked
				if upper {
					alias.Sha256 = strings.ToUpper(alias.Sha256)
				}
				for _, first := range []bool{false, true} {
					changed := *local
					if first {
						changed.Tokens = append([]Token{alias}, local.Tokens...)
					} else {
						changed.Tokens = append(append([]Token(nil), local.Tokens...), alias)
					}
					if _, ok := authenticateCompetitionAt(bearer, &changed, now); ok {
						t.Fatal("ordinary authentication accepted an aliased bearer")
					}
					if validateCompetitionDistributedRecoveryRequest(&changed, principal, request) == nil {
						t.Fatal("retained principal bypassed an added bearer alias", role, revoked, upper, first)
					}
				}
			}
		}
	}
	otherDigest := sha256.Sum256([]byte("synthetic-unrelated-bearer"))
	unrelated := Token{Name: "synthetic-unrelated-operator", Role: "operator", Sha256: hex.EncodeToString(otherDigest[:])}
	for _, revoked := range []bool{false, true} {
		unrelated.Revoked = revoked
		changed := *local
		changed.Tokens = append(append([]Token(nil), local.Tokens...), unrelated)
		if _, ok := authenticateCompetitionAt(bearer, &changed, now); !ok || validateCompetitionDistributedRecoveryRequest(&changed, principal, request) != nil {
			t.Fatal("healthy unrelated token revoked the exact retry capability", revoked)
		}
	}
}
