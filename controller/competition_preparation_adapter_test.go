// Deterministic adapter tests use a private TLS API, synthetic retained store
// and controlled execution barriers. No fixture accesses real fleet state.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Role receipts and event order share one lock across the TLS server and
// adapter caller. Tests inject failures at exact transitions, never by sleeping.
type competitionPreparationTestState struct {
	stateLock sync.Mutex
	events    []string
	receipts  []model.CompetitionSourceReadiness
}

// A single adapter caller must never reach the network when a durable receipt
// already settles the API step, including when that receipt is invalid.
type competitionPreparationHttpTripwire struct {
	calls int
}

// Records attempted transport without sending credentials or contacting a host.
func (self *competitionPreparationHttpTripwire) RoundTrip(*http.Request) (*http.Response, error) {
	self.calls++
	return nil, errors.New("synthetic unexpected API request")
}

// Builds role-shaped synthetic evidence only inside private tests.
func competitionPreparationTestReceipt(intent *model.CompetitionSourceActivation, role, actor, image string) model.CompetitionSourceReadiness {
	return model.CompetitionSourceReadiness{Staging: intent.Staging, Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId,
		Generation: intent.Generation, TargetRoundId: intent.TargetRoundId, Role: role, ActorId: actor, RuntimeImageDigest: image,
		ManifestCommit: intent.ManifestCommit, ManifestSha256: intent.ManifestSha256, SourceLockSha256: intent.SourceLockSha256, QualificationSha256: intent.HostQualificationSha256, Ready: true}
}

// The normal concrete HTTPS, publication and receipt validators remain real;
// only database authority and trusted host execution use deterministic seams.
func newCompetitionPreparationAdapterFixture(t testing.TB) (*competitionPreparationAdapter, CompetitionPreparationRequest, *competitionPreparationTestState, *competitionRuntimeBlobTestStore) {
	t.Helper()
	settings, sourceStore, archive, blobs := newCompetitionRuntimeBlobFixture(t)
	intent := *sourceStore.intent
	intent.ActivatedAt, intent.State = nil, "prepared"
	state := &competitionPreparationTestState{}
	config := CompetitionPreparationConfig{Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Staging: intent.Staging,
		ApiImageDigest: testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest(), WorkerId: "synthetic-worker", TimeoutSeconds: 3600,
		QualificationCommand: "/synthetic/qualification", QualificationSha256: strings.Repeat("c", 64), WorkerCommand: "/synthetic/worker", WorkerSha256: strings.Repeat("d", 64),
		SourcePromoterTokenFile: filepath.Join(t.TempDir(), "source-promoter.token")}
	if err := os.WriteFile(config.SourcePromoterTokenFile, []byte("synthetic-promoter-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.ContentLength != 0 || r.URL.Path != fmt.Sprintf("/competition/v2/source/%d/readiness", intent.Generation) || r.Header.Get("Authorization") != "Bearer synthetic-promoter-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if err := LoadCompetitionRuntimeManifestContext(r.Context(), settings, &intent); err != nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		receipt := competitionPreparationTestReceipt(&intent, "api", "synthetic-promoter", config.ApiImageDigest)
		state.stateLock.Lock()
		state.events = append(state.events, "api")
		if len(state.receipts) == 0 {
			state.receipts = append(state.receipts, receipt)
		}
		state.stateLock.Unlock()
		w.Header().Set("X-Competition-Api-Capability", model.CompetitionRuntimeBlobCapability)
		_ = json.NewEncoder(w).Encode(receipt)
	}))
	t.Cleanup(api.Close)
	config.ApiBaseUrl = api.URL
	request := CompetitionPreparationRequest{Operation: "propagate_and_qualify", Intent: intent,
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
		copy := intent
		return &copy, nil
	}
	adapter.receipts = func(context.Context, *Settings, int64) ([]model.CompetitionSourceReadiness, error) {
		state.stateLock.Lock()
		defer state.stateLock.Unlock()
		return append([]model.CompetitionSourceReadiness(nil), state.receipts...), nil
	}
	adapter.runProgram = func(_ context.Context, commandPath, _ string, value any, arguments []string) error {
		state.stateLock.Lock()
		defer state.stateLock.Unlock()
		if len(state.receipts) < 1 || state.receipts[0].Role != "api" {
			return errors.New("host qualification preceded independent API evidence")
		}
		if commandPath == config.QualificationCommand {
			bound, ok := value.(CompetitionPreparationRequest)
			if !ok || bound.Intent.IntentSha256 != intent.IntentSha256 || len(arguments) != 0 {
				return errors.New("qualifier did not receive exact fenced request")
			}
			state.events = append(state.events, "qualify")
			return nil
		}
		if commandPath != config.WorkerCommand || value != nil || !reflect.DeepEqual(arguments, []string{fmt.Sprintf("--source-readiness=%d", intent.Generation), "--worker_id=" + config.WorkerId}) {
			return errors.New("worker did not receive fixed source-readiness arguments")
		}
		state.events = append(state.events, "worker")
		if len(state.receipts) == 1 {
			state.receipts = append(state.receipts, competitionPreparationTestReceipt(&intent, "worker", config.WorkerId, config.WorkerImageDigest))
		}
		return nil
	}
	return adapter, request, state, blobs
}

// A successful qualifier exit alone is never the completion condition; both
// real role receipts are read after the worker program completes.
func TestCompetitionPreparationAdapterOrdersIndependentEvidenceAndReplays(t *testing.T) {
	adapter, request, state, blobs := newCompetitionPreparationAdapterFixture(t)
	if err := validateCompetitionPreparationConfig(adapter.settings, adapter.config); err != nil {
		t.Fatal(err)
	}
	first, err := adapter.run(context.Background(), request)
	if err != nil || first.Generation != request.Intent.Generation || first.TargetRoundId != request.Intent.TargetRoundId || first.EnvelopeVersion == "" {
		t.Fatalf("preparation failed: %+v %v", first, err)
	}
	state.stateLock.Lock()
	events := append([]string(nil), state.events...)
	state.stateLock.Unlock()
	if !reflect.DeepEqual(events, []string{"api", "qualify", "worker"}) {
		t.Fatalf("unsafe preparation order: %v", events)
	}
	replayed, err := adapter.run(context.Background(), request)
	if err != nil || *replayed != *first || blobs.retainedWrites != 1 {
		t.Fatalf("replay changed retained identity: %+v %v", replayed, err)
	}
}

// The original authenticated API acknowledgment survives a failed host probe
// and later API rollout; retry uses it without rewriting its pinned identity.
func TestCompetitionPreparationAdapterReusesPersistedApiAfterQualificationFailure(t *testing.T) {
	adapter, request, state, blobs := newCompetitionPreparationAdapterFixture(t)
	runProgram := adapter.runProgram
	failure := errors.New("synthetic initial host qualification failure")
	adapter.runProgram = func(context.Context, string, string, any, []string) error { return failure }
	if _, err := adapter.run(context.Background(), request); !errors.Is(err, failure) {
		t.Fatalf("initial qualification did not stop at the controlled failure: %v", err)
	}
	state.stateLock.Lock()
	original := append([]model.CompetitionSourceReadiness(nil), state.receipts...)
	state.stateLock.Unlock()
	if len(original) != 1 || original[0].Role != "api" || !sourcePreparationReceiptMatches(&request.Intent, original[0]) {
		t.Fatal("first attempt did not persist its authenticated API acknowledgment")
	}
	transport := &competitionPreparationHttpTripwire{}
	adapter.client = &http.Client{Transport: transport}
	adapter.runProgram = runProgram
	if result, err := adapter.run(context.Background(), request); err != nil || result == nil {
		t.Fatalf("exact receipt retry failed: %v", err)
	}
	state.stateLock.Lock()
	defer state.stateLock.Unlock()
	if transport.calls != 0 || blobs.retainedWrites != 1 || !reflect.DeepEqual(state.events, []string{"api", "qualify", "worker"}) {
		t.Fatalf("retry repeated API publication or skipped host evidence: calls=%d events=%v", transport.calls, state.events)
	}
	if len(state.receipts) != 2 || !reflect.DeepEqual(state.receipts[0], original[0]) || state.receipts[1].Role != "worker" {
		t.Fatal("retry replaced immutable API evidence or fabricated worker completion")
	}
}

// Foreign, incomplete, duplicate or differently pinned API rows are refused
// before a network call or host command; retry cannot replace bad evidence.
func TestCompetitionPreparationAdapterRejectsMismatchedPersistedApiReceipts(t *testing.T) {
	adapter, request, state, _ := newCompetitionPreparationAdapterFixture(t)
	transport := &competitionPreparationHttpTripwire{}
	adapter.client = &http.Client{Transport: transport}
	commands := 0
	adapter.runProgram = func(context.Context, string, string, any, []string) error { commands++; return nil }
	original := competitionPreparationTestReceipt(&request.Intent, "api", "synthetic-promoter", adapter.config.ApiImageDigest)
	differentDigest := func(value string) string {
		if value == strings.Repeat("a", len(value)) {
			return strings.Repeat("b", len(value))
		}
		return strings.Repeat("a", len(value))
	}
	for _, sample := range []struct {
		name   string
		mutate func(*model.CompetitionSourceReadiness)
	}{
		{name: "environment", mutate: func(value *model.CompetitionSourceReadiness) { value.Environment = "synthetic-other-environment" }},
		{name: "competition", mutate: func(value *model.CompetitionSourceReadiness) { value.CompetitionId = "synthetic-other-competition" }},
		{name: "season", mutate: func(value *model.CompetitionSourceReadiness) { value.SeasonId = "synthetic-other-season" }},
		{name: "mode", mutate: func(value *model.CompetitionSourceReadiness) { value.Staging = !value.Staging }},
		{name: "generation", mutate: func(value *model.CompetitionSourceReadiness) { value.Generation++ }},
		{name: "round", mutate: func(value *model.CompetitionSourceReadiness) { value.TargetRoundId = server.NewId() }},
		{name: "manifest commit", mutate: func(value *model.CompetitionSourceReadiness) {
			value.ManifestCommit = differentDigest(value.ManifestCommit)
		}},
		{name: "manifest digest", mutate: func(value *model.CompetitionSourceReadiness) {
			value.ManifestSha256 = differentDigest(value.ManifestSha256)
		}},
		{name: "source lock", mutate: func(value *model.CompetitionSourceReadiness) {
			value.SourceLockSha256 = differentDigest(value.SourceLockSha256)
		}},
		{name: "qualification", mutate: func(value *model.CompetitionSourceReadiness) {
			value.QualificationSha256 = differentDigest(value.QualificationSha256)
		}},
		{name: "image", mutate: func(value *model.CompetitionSourceReadiness) { value.RuntimeImageDigest = testWorkerImageDigest() }},
		{name: "actor", mutate: func(value *model.CompetitionSourceReadiness) { value.ActorId = "" }},
		{name: "ready", mutate: func(value *model.CompetitionSourceReadiness) { value.Ready = false }},
		{name: "duplicate", mutate: nil},
	} {
		changed := original
		if sample.mutate != nil {
			sample.mutate(&changed)
			if reflect.DeepEqual(changed, original) {
				t.Fatalf("%s mutation did not change the receipt", sample.name)
			}
		}
		expected := []model.CompetitionSourceReadiness{changed}
		if sample.mutate == nil {
			expected = append(expected, original)
		}
		state.stateLock.Lock()
		state.receipts = append([]model.CompetitionSourceReadiness(nil), expected...)
		state.stateLock.Unlock()
		if _, err := adapter.run(context.Background(), request); err == nil {
			t.Fatalf("%s persisted API evidence was accepted", sample.name)
		}
		state.stateLock.Lock()
		unchanged := reflect.DeepEqual(state.receipts, expected)
		state.stateLock.Unlock()
		if !unchanged || transport.calls != 0 || commands != 0 {
			t.Fatalf("%s receipt was replaced or bypassed: calls=%d commands=%d", sample.name, transport.calls, commands)
		}
	}
}

// Store unavailability cannot be mistaken for absence and trigger replacement
// of an immutable acknowledgment whose contents were never authenticated.
func TestCompetitionPreparationAdapterReceiptReadFailureStopsBeforeApi(t *testing.T) {
	adapter, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	transport := &competitionPreparationHttpTripwire{}
	adapter.client = &http.Client{Transport: transport}
	failure := errors.New("synthetic durable receipt read failure")
	adapter.receipts = func(context.Context, *Settings, int64) ([]model.CompetitionSourceReadiness, error) {
		return nil, failure
	}
	commands := 0
	adapter.runProgram = func(context.Context, string, string, any, []string) error { commands++; return nil }
	if _, err := adapter.run(context.Background(), request); !errors.Is(err, failure) || transport.calls != 0 || commands != 0 {
		t.Fatalf("failed receipt read was bypassed: %v calls=%d commands=%d", err, transport.calls, commands)
	}
}

// Failed qualification preserves the published envelope and API role evidence
// for exact retry, but never invokes worker acknowledgment or fabricates quorum.
func TestCompetitionPreparationAdapterQualificationFailureStopsBeforeWorker(t *testing.T) {
	adapter, request, state, _ := newCompetitionPreparationAdapterFixture(t)
	failure := errors.New("synthetic qualification failure")
	called := 0
	adapter.runProgram = func(context.Context, string, string, any, []string) error { called++; return failure }
	if _, err := adapter.run(context.Background(), request); !errors.Is(err, failure) || called != 1 {
		t.Fatalf("qualification failure did not stop execution: %v calls=%d", err, called)
	}
	state.stateLock.Lock()
	defer state.stateLock.Unlock()
	if len(state.receipts) != 1 || state.receipts[0].Role != "api" {
		t.Fatal("qualification failure fabricated worker readiness")
	}
}

// A no-op command cannot make preparation ready; the durable worker receipt
// is independent and must match the exact configured worker release and actor.
func TestCompetitionPreparationAdapterRequiresDurableWorkerEvidence(t *testing.T) {
	adapter, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	adapter.runProgram = func(context.Context, string, string, any, []string) error { return nil }
	if _, err := adapter.run(context.Background(), request); !errors.Is(err, ErrReviewNotReady) {
		t.Fatalf("successful no-op became ready: %v", err)
	}
}

// Losing the fence after publication prevents qualification even when the API
// has already independently recorded its exact generation acknowledgment.
func TestCompetitionPreparationAdapterRechecksFenceBeforeQualification(t *testing.T) {
	adapter, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	check := adapter.checkFence
	calls, commands := 0, 0
	adapter.checkFence = func(ctx context.Context, settings *Settings, value CompetitionPreparationRequest) (*model.CompetitionSourceActivation, error) {
		calls++
		if calls == 3 {
			return nil, ErrLeaseLost
		}
		return check(ctx, settings, value)
	}
	adapter.runProgram = func(context.Context, string, string, any, []string) error { commands++; return nil }
	if _, err := adapter.run(context.Background(), request); !errors.Is(err, ErrLeaseLost) || commands != 0 {
		t.Fatalf("stale preparer ran qualification: %v commands=%d", err, commands)
	}
}

// Explicit barriers prove cancellation joins the in-flight qualifier before
// returning, without relying on a short timeout or scheduler luck.
func TestCompetitionPreparationAdapterCancellationJoinsQualifier(t *testing.T) {
	adapter, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	entered, joined := make(chan struct{}), make(chan struct{})
	adapter.runProgram = func(ctx context.Context, _ string, _ string, _ any, _ []string) error {
		close(entered)
		<-ctx.Done()
		close(joined)
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := adapter.run(ctx, request); result <- err }()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("adapter returned before qualifier cleanup")
	}
}

// TLS redirects are not followed with the bearer; an old-capability API cannot
// stand in for a verified runtime even if its receipt shape looks plausible.
func TestCompetitionPreparationAdapterRejectsOldApiAndRedirects(t *testing.T) {
	adapter, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	for _, status := range []int{http.StatusOK, http.StatusTemporaryRedirect} {
		api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://synthetic-elsewhere.example/credential-sink")
			w.Header().Set("X-Competition-Api-Capability", model.CompetitionAutomationCapability)
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(competitionPreparationTestReceipt(&request.Intent, "api", "synthetic-promoter", adapter.config.ApiImageDigest))
		}))
		adapter.config.ApiBaseUrl, adapter.client = api.URL, api.Client()
		adapter.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		_, err := adapter.acknowledgeApi(context.Background(), &request.Intent)
		api.Close()
		if err == nil {
			t.Fatalf("status %d bypassed exact capability or redirect refusal", status)
		}
	}
}

// Request field smuggling, duplicate keys and accidental concatenation are
// rejected before database or command access.
func TestCompetitionPreparationRequestIsStrictAndBounded(t *testing.T) {
	adapter, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	content, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCompetitionPreparationRequest(bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	for _, malformed := range [][]byte{
		append(bytes.Clone(content), []byte("{}")...),
		[]byte(strings.Replace(string(content), `"operation":`, `"unknown":true,"operation":`, 1)),
		[]byte(strings.Replace(string(content), `"operation":`, `"operation":"other","operation":`, 1)),
		bytes.Repeat([]byte(" "), competitionRuntimeManifestLimit+1),
	} {
		if _, err := DecodeCompetitionPreparationRequest(bytes.NewReader(malformed)); err == nil {
			t.Fatal("ambiguous or oversized preparation request accepted")
		}
	}
	request.Intent.Manifest = []byte("changed immutable source")
	if _, err := CheckCompetitionPreparationFence(context.Background(), adapter.settings, request); !errors.Is(err, ErrConflict) {
		t.Fatal("changed request payload was accepted")
	}
}

// A deployment inventory cannot silently select another mode or send scoped
// credentials to plaintext, a URL userinfo field or an attacker-selected query.
func TestCompetitionPreparationConfigRejectsUnsafeAuthority(t *testing.T) {
	adapter, _, _, _ := newCompetitionPreparationAdapterFixture(t)
	for _, endpoint := range []string{"http://synthetic-api.example", "https://synthetic:secret@synthetic-api.example", "https://synthetic-api.example?next=other", "https://synthetic-api.example/#other", "https://synthetic-api.example/%2fother"} {
		config := adapter.config
		config.ApiBaseUrl = endpoint
		if validateCompetitionPreparationConfig(adapter.settings, config) == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	config := adapter.config
	config.Staging = !config.Staging
	if validateCompetitionPreparationConfig(adapter.settings, config) == nil {
		t.Fatal("cross-mode preparation inventory accepted")
	}
}

// The concrete program path executes the pinned inode with fixed arguments;
// wrong digests and writable credential files are refused before execution.
func TestCompetitionPreparationProgramPinsExecutableAndPrivateFiles(t *testing.T) {
	directory := t.TempDir()
	commandPath, markerPath := filepath.Join(directory, "synthetic-program"), filepath.Join(directory, "synthetic-marker")
	program := []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$1\" > %q\n", markerPath))
	if err := os.WriteFile(commandPath, program, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(program)
	if err := runCompetitionPreparationProgram(context.Background(), commandPath, hex.EncodeToString(digest[:]), map[string]string{"synthetic": "request"}, []string{"--synthetic-fixed"}); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil || string(marker) != "--synthetic-fixed" {
		t.Fatalf("pinned command did not execute fixed protocol: %q %v", marker, err)
	}
	if runCompetitionPreparationProgram(context.Background(), commandPath, strings.Repeat("f", 64), nil, nil) == nil {
		t.Fatal("wrong executable digest accepted")
	}
	if err := os.Chmod(markerPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCompetitionPreparationFile(markerPath, 4096, true); err == nil {
		t.Fatal("publicly readable token file accepted")
	}
}

// Private database execution proves the new SQL capability and the shared
// qualifier fence, without opening an epoch or mutating any real source refs.
func TestCompetitionPreparationDatabaseBindsIntentFenceAndBlobCapability(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithMode(t, true)
		fixture.now = fixture.round.RevealAt
		sealed, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		fixture.settings.AutomationPolicy.RequiredApiCapability = model.CompetitionRuntimeBlobCapability
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-runtime-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		intent := competitionAutomationSourceIntent(t, fixture, sealed, 2)
		prepared, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, intent)
		if err != nil {
			t.Fatal(err)
		}
		request := CompetitionPreparationRequest{Operation: "propagate_and_qualify", Intent: *prepared, Lease: *lease}
		if stored, err := CheckCompetitionPreparationFence(ctx, fixture.settings, request); err != nil || stored.IntentSha256 != prepared.IntentSha256 {
			t.Fatalf("valid durable preparation rejected: %+v %v", stored, err)
		}
		changed := request
		changed.Intent.TargetRoundId = server.NewId()
		if _, err := CheckCompetitionPreparationFence(ctx, fixture.settings, changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("copied digest blessed another target: %v", err)
		}
		changed = request
		changed.Lease.FencingToken++
		if _, err := CheckCompetitionPreparationFence(ctx, fixture.settings, changed); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("stale preparation lease accepted: %v", err)
		}
		acknowledgeCompetitionAutomationSource(t, fixture, prepared, "api", "worker")
		if _, err := fixture.store.ActivateSource(ctx, fixture.settings, *lease, prepared.Generation, prepared.ExpectedGeneration); err != nil {
			t.Fatal(err)
		}
		if _, err := CheckCompetitionPreparationFence(ctx, fixture.settings, request); !errors.Is(err, ErrConflict) {
			t.Fatalf("active epoch permitted a qualification replay: %v", err)
		}
		fixture.now = prepared.PreparedAt.Add(time.Minute)
		next, err := fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, prepared.Generation, fixture.now)
		if err != nil || next == nil {
			t.Fatalf("migration 851 refused exact blob-required successor: %+v %v", next, err)
		}
		storedRound, err := fixture.store.GetRound(ctx, fixture.settings, next.RoundId)
		if err != nil || competitionRoundApiCapability(storedRound) != model.CompetitionRuntimeBlobCapability {
			t.Fatalf("successor lost frozen blob capability: %+v %v", storedRound, err)
		}
	})
}
