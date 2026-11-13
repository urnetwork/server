// Incident exceptions require an explicit typed operator approval, preserved
// failure identity and exact retained diagnostic bytes, never just a fast score.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Synthetic diagnostics use reserved documentation addresses and no captured
// production identities. Returning a string simplifies homogeneous mutations.
func syntheticCompetitionBackingServiceRecovery(t testing.TB) string {
	t.Helper()
	content, err := json.Marshal(map[string]any{
		"error": "*fmt.wrapError=failed to deallocate cached statement(s): timeout: read tcp 192.0.2.10:12345->192.0.2.11:5432: i/o timeout",
		"stack": []string{"runtime/debug.Stack()", "github.com/urnetwork/server/v2026.dbWithPool({synthetic arguments})"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "synthetic logger Unexpected error: " + string(content) + "\n"
}

// A timeout-looking line cannot conceal another panic, wrong provenance,
// truncated record or duplicated JSON key inside the same retained log.
func TestCompetitionReevaluationBackingServiceLogRequiresOneExactRecovery(t *testing.T) {
	valid := syntheticCompetitionBackingServiceRecovery(t)
	limits := defaultCompetitionReevaluationEvidenceSettings()
	for _, c := range []struct {
		name, log string
		allowed   bool
	}{
		{name: "exact", log: valid, allowed: true},
		{name: "ordinary-surrounding-output", log: "synthetic prelude\n" + valid + "synthetic tail\n", allowed: true},
		{name: "empty", log: ""},
		{name: "no-recovery-owner", log: strings.Replace(valid, "Unexpected error:", "synthetic note:", 1)},
		{name: "not-database-stack", log: strings.Replace(valid, "server.dbWithPool(", "server.syntheticOther(", 1)},
		{name: "not-cleanup-timeout", log: strings.Replace(valid, "failed to deallocate cached statement(s): timeout:", "synthetic unrelated product failure:", 1)},
		{name: "two-recoveries", log: valid + valid},
		{name: "mixed-product-failure", log: valid + "Unexpected error: {\"error\":\"synthetic product panic\",\"stack\":[]}\n"},
		{name: "malformed", log: "Unexpected error: {synthetic malformed json}\n"},
		{name: "duplicate-json", log: strings.Replace(valid, `"error":`, `"error":"synthetic overwritten error","error":`, 1)},
	} {
		if actual := competitionReevaluationHasBackingServiceRecovery([]byte(c.log), limits); actual != c.allowed {
			t.Fatalf("%s allowed=%t want=%t", c.name, actual, c.allowed)
		}
	}
	limits.MaximumLogBytes = int64(len(valid) - 1)
	if competitionReevaluationHasBackingServiceRecovery([]byte(valid), limits) {
		t.Fatal("oversized log bypassed its owner bound")
	}
}

// Neither exception can borrow the other's proof or an arbitrary terminal
// code. All mutations recompute the outer digest, so rejection is semantic.
func TestCompetitionReevaluationIncidentRequestRequiresTypedBoundEvidence(t *testing.T) {
	settings, principal, request := newCompetitionLegacyArchiveRequestFixture(t)
	request.Authorization.IncidentClass = model.CompetitionJobReevaluationHeartbeatBudget
	var err error
	request.AuthorizationSha256, err = model.CompetitionAutomationDigest(request.Authorization)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCompetitionReevaluationRequest(settings, principal, request); err != nil {
		t.Fatalf("bound heartbeat approval refused: %v", err)
	}
	request.Authorization.IncidentClass = model.CompetitionJobReevaluationBackingService
	request.Authorization.IncidentAttempt = request.Authorization.ExpectedAttemptCount
	proof := model.CompetitionJobReevaluationBackingServiceEvidence{Schema: 1, Service: "postgresql",
		ArtifactPath:   "evidence/scorer-input/candidate-01-" + request.Authorization.JobId.String()[:8] + "/stderr.log",
		ArtifactSha256: strings.Repeat("a", 64), ArtifactBytes: 123}
	request.Authorization.BackingServiceEvidence = &proof
	for _, c := range []struct {
		name   string
		mutate func(*model.CompetitionJobReevaluationAuthorization)
	}{
		{name: "missing-proof", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.BackingServiceEvidence = nil }},
		{name: "wrong-service", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.BackingServiceEvidence.Service = "synthetic-other"
		}},
		{name: "wrong-schema", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.BackingServiceEvidence.Schema = 2 }},
		{name: "baseline-log", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.BackingServiceEvidence.ArtifactPath = strings.Replace(a.BackingServiceEvidence.ArtifactPath, "candidate-", "baseline-", 1)
		}},
		{name: "another-job", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.BackingServiceEvidence.ArtifactPath = "evidence/scorer-input/candidate-01-synthetic/stderr.log"
		}},
		{name: "outside-cohort", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.BackingServiceEvidence.ArtifactPath = "evidence/scorer-input/candidate-99-" + a.JobId.String()[:8] + "/stderr.log"
		}},
		{name: "missing-hash", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.BackingServiceEvidence.ArtifactSha256 = "" }},
		{name: "empty-log", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.BackingServiceEvidence.ArtifactBytes = 0 }},
		{name: "earlier-attempt", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.IncidentAttempt-- }},
		{name: "borrowed-heartbeat-proof", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.IncidentClass = model.CompetitionJobReevaluationHeartbeatBudget
			a.IncidentAttempt--
		}},
	} {
		changed := request
		copyProof := proof
		changed.Authorization.BackingServiceEvidence = &copyProof
		c.mutate(&changed.Authorization)
		changed.AuthorizationSha256, err = model.CompetitionAutomationDigest(changed.Authorization)
		if err != nil {
			t.Fatal(err)
		}
		if validateCompetitionReevaluationRequest(settings, principal, changed) == nil {
			t.Fatalf("%s granted unbound incident authority", c.name)
		}
	}
	request.AuthorizationSha256, err = model.CompetitionAutomationDigest(request.Authorization)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCompetitionReevaluationRequest(settings, principal, request); err != nil {
		t.Fatalf("bound backing-service approval refused: %v", err)
	}
}

// The same message-free receipt retains the original submission failure;
// removing its typed operator exception does not make that failure recoverable.
func TestCompetitionReevaluationIncidentCycleRetainsOriginalFailure(t *testing.T) {
	cycle := newCompetitionReevaluationCycleTestFixture(t)
	cycle.IncidentClass = model.CompetitionJobReevaluationBackingService
	cycle.PreviousFailure = model.CompetitionEvaluationFailure{Kind: "submission", Code: "run_unstable", Retriable: false}
	var err error
	cycle.ReceiptSha256, err = model.CompetitionJobReevaluationCycleDigest(cycle)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.ValidateCompetitionJobReevaluationCycle(&cycle); err != nil {
		t.Fatalf("typed retained failure refused: %v", err)
	}
	for _, class := range []string{"", model.CompetitionJobReevaluationHeartbeatBudget, model.CompetitionJobReevaluationArchiveInventory, "synthetic_unknown"} {
		changed := cycle
		changed.IncidentClass = class
		changed.ReceiptSha256, err = model.CompetitionJobReevaluationCycleDigest(changed)
		if err != nil {
			t.Fatal(err)
		}
		if model.ValidateCompetitionJobReevaluationCycle(&changed) == nil {
			t.Fatalf("%q borrowed backing-service recovery authority", class)
		}
	}
}

// Authenticate exact bytes before interpreting diagnostics. Changing content,
// size or a link cannot preserve an otherwise valid operator authorization.
func TestCompetitionReevaluationBackingServiceArtifactAuthenticatesRetainedBytes(t *testing.T) {
	settings, _, request := newCompetitionLegacyArchiveRequestFixture(t)
	settings.ArtifactRoot = t.TempDir()
	a := request.Authorization
	a.IncidentClass = model.CompetitionJobReevaluationBackingService
	a.IncidentAttempt = a.ExpectedAttemptCount
	content := []byte(syntheticCompetitionBackingServiceRecovery(t))
	digest := sha256.Sum256(content)
	a.BackingServiceEvidence = &model.CompetitionJobReevaluationBackingServiceEvidence{Schema: 1, Service: "postgresql",
		ArtifactPath:   "evidence/scorer-input/candidate-01-" + a.JobId.String()[:8] + "/stderr.log",
		ArtifactSha256: hex.EncodeToString(digest[:]), ArtifactBytes: int64(len(content))}
	root := filepath.Join(settings.ArtifactRoot, a.JobId.String(), "attempt-02")
	path := filepath.Join(root, a.BackingServiceEvidence.ArtifactPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o400); err != nil {
		t.Fatal(err)
	}
	limits := defaultCompetitionReevaluationEvidenceSettings()
	if err := validateCompetitionReevaluationBackingService(settings, a, limits); err != nil {
		t.Fatalf("exact retained bytes refused: %v", err)
	}
	for _, c := range []struct {
		name   string
		mutate func(*model.CompetitionJobReevaluationBackingServiceEvidence)
	}{
		{name: "hash", mutate: func(p *model.CompetitionJobReevaluationBackingServiceEvidence) {
			p.ArtifactSha256 = strings.Repeat("f", 64)
		}},
		{name: "size", mutate: func(p *model.CompetitionJobReevaluationBackingServiceEvidence) { p.ArtifactBytes++ }},
	} {
		changed := a
		proof := *a.BackingServiceEvidence
		changed.BackingServiceEvidence = &proof
		c.mutate(changed.BackingServiceEvidence)
		if validateCompetitionReevaluationBackingService(settings, changed, limits) == nil {
			t.Fatalf("%s mismatch accepted", c.name)
		}
	}
	limits.MaximumLogBytes = int64(len(content) - 1)
	if validateCompetitionReevaluationBackingService(settings, a, limits) == nil {
		t.Fatal("artifact bypassed maximum read bytes")
	}
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".retained", path); err != nil {
		t.Fatal(err)
	}
	if validateCompetitionReevaluationBackingService(settings, a, defaultCompetitionReevaluationEvidenceSettings()) == nil {
		t.Fatal("symlink substituted for retained artifact")
	}
}
