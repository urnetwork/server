// The real retained-envelope publisher and HTTPS client exercise API-only
// preparation; only database/fence facts are synthetic and host execution is
// an explicit tripwire. No genuine readiness or permission is minted here.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// A private TLS API observes the installed exact envelope and records only its
// own role. The test runner cannot return success for a host/worker invocation.
// As in the adapter fixture, the token lives under a link-free directory, since
// the reader refuses Darwin's /var alias.
func newCompetitionIntakeFirstPreparationTest(t testing.TB, ready bool) (*competitionPreparationAdapter, CompetitionPreparationRequest, *competitionPreparationTestState, *competitionRuntimeBlobTestStore) {
	t.Helper()
	settings, intent, _, now := competitionIntakeFirstTestValues()
	intent.Commits = map[string]string{}
	for _, repository := range []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "warp", "gvisor"} {
		intent.Commits[repository] = settings.BaseSha
	}
	var err error
	intent.SourceLockSha256, err = model.CompetitionSourceLockDigest(intent.Commits)
	if err != nil {
		t.Fatal(err)
	}
	intent.EvaluatorImageDigest, intent.HostQualificationSha256 = settings.EvaluatorImageDigest, settings.EvaluationPolicy.HostQualificationSha256
	intent.Manifest, intent.ManifestCommit = []byte("synthetic immutable source manifest\n"), strings.Repeat("a", 40)
	manifestHash := sha256.Sum256(intent.Manifest)
	intent.ManifestSha256 = hex.EncodeToString(manifestHash[:])
	intent.IntentSha256, err = competitionPreparationIntentDigest(*intent)
	if err != nil {
		t.Fatal(err)
	}
	settings.AutomationPolicy.SourceLockSha256 = intent.SourceLockSha256
	blobs := &competitionRuntimeBlobTestStore{objects: map[string][]byte{}, versions: map[string][]byte{}}
	archive := &blobArtifactArchive{store: blobs}
	settings.artifactArchive = archive
	state := &competitionPreparationTestState{}
	config := CompetitionPreparationConfig{Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId,
		ApiImageDigest: testApiImageDigest(), SourcePromoterTokenFile: filepath.Join(providerWorkPhysicalTempDir(t), "source-promoter.token"), TimeoutSeconds: 3600}
	if err := os.WriteFile(config.SourcePromoterTokenFile, []byte("synthetic-source-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.ContentLength != 0 || r.Header.Get("Authorization") != "Bearer synthetic-source-token" ||
			r.URL.Path != "/competition/v2/source/2/readiness" || LoadCompetitionRuntimeManifestContext(r.Context(), settings, intent) != nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		receipt := competitionPreparationTestReceipt(intent, "api", "synthetic-api", config.ApiImageDigest)
		receipt.Ready, receipt.RecordedAt = ready, now
		state.stateLock.Lock()
		state.events = append(state.events, "api")
		if ready {
			state.receipts = append(state.receipts, receipt)
		}
		state.stateLock.Unlock()
		w.Header().Set("X-Competition-Api-Capability", model.CompetitionRuntimeBlobCapability)
		_ = json.NewEncoder(w).Encode(receipt)
	}))
	t.Cleanup(api.Close)
	config.ApiBaseUrl = api.URL
	request := CompetitionPreparationRequest{Operation: "propagate_intake", Intent: *intent,
		Lease: model.CompetitionSupervisorLease{Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, OwnerId: "synthetic-supervisor", FencingToken: 1}}
	adapter := &competitionPreparationAdapter{settings: settings, config: config, archive: archive, client: api.Client()}
	adapter.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	adapter.checkFence = func(ctx context.Context, _ *Settings, value CompetitionPreparationRequest) (*model.CompetitionSourceActivation, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if value.Intent.IntentSha256 != intent.IntentSha256 || value.Lease.FencingToken != request.Lease.FencingToken {
			return nil, ErrLeaseLost
		}
		copy := *intent
		return &copy, nil
	}
	adapter.receipts = func(context.Context, *Settings, int64) ([]model.CompetitionSourceReadiness, error) {
		state.stateLock.Lock()
		defer state.stateLock.Unlock()
		return append([]model.CompetitionSourceReadiness(nil), state.receipts...), nil
	}
	adapter.runProgram = func(context.Context, string, string, any, []string) error {
		return errors.New("unexpected host/worker invocation from API-only purpose")
	}
	return adapter, request, state, blobs
}

// Complete source/image bytes and actual API evidence suffice without a host
// command, worker image, qualified evaluator, baseline or synthetic worker ACK.
func TestCompetitionIntakeFirstPreparationUsesRealApiWithoutWorker(t *testing.T) {
	adapter, request, state, blobs := newCompetitionIntakeFirstPreparationTest(t, true)
	if err := validateCompetitionPreparationPurposeConfig(adapter.settings, adapter.config, true); err != nil {
		t.Fatal(err)
	}
	if validateCompetitionPreparationConfig(adapter.settings, adapter.config) == nil {
		t.Fatal("API-only deployment inventory became a grading profile")
	}
	result, err := adapter.run(context.Background(), request)
	if err != nil || result == nil || result.EnvelopeVersion == "" || result.TargetRoundId != request.Intent.TargetRoundId {
		t.Fatal("genuine API-only propagation failed", result, err)
	}
	state.stateLock.Lock()
	defer state.stateLock.Unlock()
	if !reflect.DeepEqual(state.events, []string{"api"}) || len(state.receipts) != 1 || state.receipts[0].Role != "api" || blobs.retainedWrites != 1 {
		t.Fatal("API-only propagation attempted worker readiness or skipped publication", state.events, state.receipts)
	}
}

// A retry joins the original API identity and retained version without a new
// network request, host process, source grant or timeline.
func TestCompetitionIntakeFirstPreparationReplaysSameApiAndEnvelope(t *testing.T) {
	adapter, request, state, blobs := newCompetitionIntakeFirstPreparationTest(t, true)
	first, err := adapter.run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	tripwire := &competitionPreparationHttpTripwire{}
	adapter.client = &http.Client{Transport: tripwire}
	second, err := adapter.run(context.Background(), request)
	if err != nil || first == nil || second == nil || *first != *second || tripwire.calls != 0 || blobs.retainedWrites != 1 {
		t.Fatal("retry replaced API or retained source evidence", first, second, err)
	}
	state.stateLock.Lock()
	defer state.stateLock.Unlock()
	if len(state.receipts) != 1 {
		t.Fatal("retry manufactured another runtime role")
	}
}

// A real endpoint refusing evidence and a canceled owner both stop before any
// possible qualifier/worker invocation. A response flag is not an ACK by fiat.
func TestCompetitionIntakeFirstPreparationRefusesMissingApiAndFence(t *testing.T) {
	adapter, request, state, _ := newCompetitionIntakeFirstPreparationTest(t, false)
	if _, err := adapter.run(context.Background(), request); err == nil {
		t.Fatal("negative API response became readiness")
	}
	state.stateLock.Lock()
	if len(state.receipts) != 0 {
		t.Fatal("negative API response recorded readiness")
	}
	state.stateLock.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.run(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled preparation owner continued publication", err)
	}
}

// A prospective API-only operation is never a compatibility alias for a
// historical production/staging source or a different control capability.
func TestCompetitionIntakeFirstPreparationRequiresExplicitProspectiveScope(t *testing.T) {
	adapter, request, _, _ := newCompetitionIntakeFirstPreparationTest(t, true)
	adapter.settings.IntakeFirstPolicy = nil
	if _, err := adapter.run(context.Background(), request); err == nil {
		t.Fatal("unapproved optional lifecycle operation accepted")
	}
}
