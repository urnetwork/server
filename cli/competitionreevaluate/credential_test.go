// Synthetic registrations traverse the actual local binding/authentication
// path; no root credential, configuration or database is used by these tests.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The fixed future expiry keeps the public Authenticate entry deterministic;
// exact expiry cuts are exercised in the controller and context regressions.
func recoveryCredentialForTest(t testing.TB) (*controller.Settings, model.CompetitionJobReevaluationAuthorization, controller.CompetitionOperatorPatchRetryCredential, string) {
	t.Helper()
	raw := "synthetic-local-patch-retry-bearer"
	digest := sha256.Sum256([]byte(raw))
	settings := &controller.Settings{CompetitionId: "synthetic-competition", AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Environment: "main", SeasonId: "synthetic-season"}}
	auth := model.CompetitionJobReevaluationAuthorization{Schema: 2, Kind: model.CompetitionOperatorRequestedPatchRetryAuthorizationKind,
		IncidentClass: model.CompetitionJobReevaluationOperatorRequestedPatchRetry, Environment: "main", CompetitionId: settings.CompetitionId,
		SeasonId: settings.AutomationPolicy.SeasonId, RoundId: server.NewId(), JobId: server.NewId()}
	registration := controller.CompetitionOperatorPatchRetryCredential{Schema: 1, Kind: controller.CompetitionOperatorPatchRetryCredentialKind,
		Capability: model.CompetitionOperatorRequestedPatchRetryCapability, Name: "synthetic-local-patch-retry", Role: "operator_patch_retry",
		Sha256: hex.EncodeToString(digest[:]), Environment: auth.Environment, CompetitionId: auth.CompetitionId, SeasonId: auth.SeasonId,
		RoundId: auth.RoundId, JobId: auth.JobId, IssuedAt: time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)}
	return settings, auth, registration, raw
}

// Decoding a registration produces only a local token record. Its actual raw
// bearer must still authenticate, and shared settings cannot learn that bearer.
func TestRecoveryCredentialBindsExactLocalRegistrationThroughRealAuthentication(t *testing.T) {
	settings, auth, registration, raw := recoveryCredentialForTest(t)
	content, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	local, err := bindRecoveryCredential(settings, content, auth, defaultRecoverySettings(), registration.IssuedAt)
	if err != nil || len(settings.Tokens) != 0 {
		t.Fatal("local credential changed shared settings", err)
	}
	bearer := httptest.NewRequest(http.MethodPost, "/synthetic-control", nil)
	bearer.Header.Set("Authorization", "Bearer "+raw)
	principal, ok := controller.Authenticate(bearer, local)
	if !ok || principal.RoundId != auth.RoundId || principal.JobId != auth.JobId || principal.Role != "operator_patch_retry" {
		t.Fatal("grant tool bypassed or lost actual authentication claims")
	}
	if _, ok := controller.Authenticate(bearer, settings); ok {
		t.Fatal("registration installed a shared credential")
	}
	local.Tokens[0].Revoked = true
	if _, ok := controller.Authenticate(bearer, local); ok {
		t.Fatal("grant tool ignored actual revocation")
	}
}

// A different typed class or exact job scope cannot borrow the registration,
// even if its JSON and outer file digest are otherwise internally consistent.
func TestRecoveryCredentialRejectsOtherClassesScopesAndAmbiguousDocuments(t *testing.T) {
	settings, auth, registration, _ := recoveryCredentialForTest(t)
	content, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*model.CompetitionJobReevaluationAuthorization){
		func(a *model.CompetitionJobReevaluationAuthorization) { a.Schema = 1 },
		func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Kind = model.CompetitionDistributedJobReevaluationAuthorizationKind
		},
		func(a *model.CompetitionJobReevaluationAuthorization) {
			a.IncidentClass = model.CompetitionJobReevaluationDistributedInfrastructure
		},
		func(a *model.CompetitionJobReevaluationAuthorization) { a.Environment = "synthetic-other-env" },
		func(a *model.CompetitionJobReevaluationAuthorization) {
			a.CompetitionId = "synthetic-other-competition"
		},
		func(a *model.CompetitionJobReevaluationAuthorization) { a.SeasonId = "synthetic-other-season" },
		func(a *model.CompetitionJobReevaluationAuthorization) { a.RoundId = server.NewId() },
		func(a *model.CompetitionJobReevaluationAuthorization) { a.JobId = server.NewId() },
	} {
		changed := auth
		mutate(&changed)
		if local, err := bindRecoveryCredential(settings, content, changed, defaultRecoverySettings(), registration.IssuedAt); err == nil || local != nil {
			t.Fatal("registration authorized a different grant")
		}
	}
	for _, malformed := range [][]byte{
		[]byte(strings.Replace(string(content), `"schema":1`, `"schema":1,"schema":1`, 1)),
		[]byte(strings.Replace(string(content), `"schema":1`, `"Schema":1`, 1)),
		[]byte(strings.Replace(string(content), `"schema":1`, `"schema":1,"unknown":true`, 1)),
		append(append([]byte(nil), content...), []byte(` {}`)...),
	} {
		if local, err := bindRecoveryCredential(settings, malformed, auth, defaultRecoverySettings(), registration.IssuedAt); err == nil || local != nil || strings.Contains(err.Error(), registration.Sha256) {
			t.Fatal("ambiguous registration passed or exposed private input")
		}
	}
	limits := defaultRecoverySettings()
	limits.DocumentBytes = int64(len(content) - 1)
	if _, err := bindRecoveryCredential(settings, content, auth, limits, registration.IssuedAt); err == nil {
		t.Fatal("oversized registration passed")
	}
	if _, err := bindRecoveryCredential(settings, content, auth, defaultRecoverySettings(), registration.ExpiresAt); err == nil {
		t.Fatal("expired registration passed")
	}
}

// The existing caller-owned operation budget cannot survive credential expiry
// or parent cancellation, and legacy callers retain their original deadline.
func TestRecoveryCredentialContextExpiresWithoutExtendingControlOrEvaluationBudget(t *testing.T) {
	started := time.Date(2090, time.January, 1, 0, 0, 0, 0, time.UTC)
	limits := defaultRecoverySettings()
	for _, expiry := range []time.Time{time.Time{}, started.Add(time.Second), started.Add(2 * limits.Timeout)} {
		ctx, cancel, err := newRecoveryCredentialContext(context.Background(), limits, started, &controller.Principal{ExpiresAt: expiry})
		if err != nil {
			t.Fatal(err)
		}
		deadline, ok := ctx.Deadline()
		expected := started.Add(limits.Timeout)
		if !expiry.IsZero() && expiry.Before(expected) {
			expected = expiry
		}
		cancel()
		if !ok || !deadline.Equal(expected) {
			t.Fatal("credential changed or escaped the original control budget")
		}
	}
	if ctx, cancel, err := newRecoveryCredentialContext(context.Background(), limits, started, &controller.Principal{ExpiresAt: started}); err == nil || ctx != nil || cancel != nil {
		t.Fatal("already expired credential acquired a control owner")
	}
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	if _, _, err := newRecoveryCredentialContext(parent, limits, started, &controller.Principal{ExpiresAt: started.Add(time.Hour)}); err == nil {
		t.Fatal("credential revived parent cancellation")
	}
}
