// Full positive protected-spool execution is exercised with intercepted HTTP,
// explicit synthetic kernel observations and an exact-version in-memory archive.
// No Docker, real provider, production identity, DB or actual retention is used.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

type winnerMultipassSpoolActorCommit struct {
	Schema           int                                          `json:"schema"`
	Kind             string                                       `json:"kind"`
	Capture          winnerMultipassProtectedReference            `json:"capture"`
	Physical         winnerMultipassProtectedReference            `json:"physical"`
	Originals        map[string]winnerMultipassProtectedReference `json:"originals"`
	IndependentClaim *winnerMultipassProtectedReference           `json:"independent_claim"`
}

type winnerMultipassSpoolActorPending struct {
	Receipt          *winnerMultipassContainmentReceipt
	Originals        map[string][]byte
	IndependentClaim []byte
}

type winnerMultipassSpoolActor struct {
	T                *testing.T
	Plan             *winnerMultipassQualificationPlan
	Directory        string
	Template         map[string][]byte
	TemplateAttempt  string
	TemplateProvider currentQgQualificationProviderProfile
	Profile          winnerMultipassContainmentProfile
	Archive          map[string][]byte
	References       map[string]winnerMultipassProtectedReference
	Commits          map[int]currentQgQualificationPin
	Claims           map[int]currentQgQualificationPin
	Pending          map[string]winnerMultipassSpoolActorPending
	Invocations      int
	Generations      int
	PhysicalChecks   int
	PastDeadline     bool
}

func newWinnerMultipassSpoolActor(t *testing.T) *winnerMultipassSpoolActor {
	t.Helper()
	_, _, _, image, _, attempt, provider, template := winnerMultipassCanceledContainmentFixture(t)
	requests, fixture := winnerMultipassQualificationPackingRequests(t, false)
	release := fixture.Release
	release.ImageId, release.ProviderSettings = image, provider.Settings
	now := time.Now().UTC()
	owner := winnerMultipassOwner{Id: "synthetic-protected-spool-group", StartedAt: now.Add(-time.Minute), Deadline: now.Add(4 * time.Hour)}
	plan, err := prepareWinnerMultipassQualification(requests, owner, fixture.Settings, release, nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return &winnerMultipassSpoolActor{T: t, Plan: plan, Directory: directory, Template: template, TemplateAttempt: attempt, TemplateProvider: provider,
		Profile: winnerMultipassContainmentProfile{ImageId: image, ProviderSettings: provider.Settings, StateRoot: "/var/lib/urnetwork/competition-review-multipass/attempts"},
		Archive: map[string][]byte{}, References: map[string]winnerMultipassProtectedReference{}, Commits: map[int]currentQgQualificationPin{}, Claims: map[int]currentQgQualificationPin{}, Pending: map[string]winnerMultipassSpoolActorPending{}}
}

// The archive is a strict test double: same-key/different-version readback is
// never accepted, and disappearance is terminal rather than a local fallback.
func (self *winnerMultipassSpoolActor) retain(content []byte) winnerMultipassProtectedReference {
	index := len(self.References) + 1
	ref := winnerMultipassProtectedReference{Key: fmt.Sprintf("synthetic/protected/%08d", index), VersionId: fmt.Sprintf("synthetic-version-%08d", index), Sha256: winnerMultipassSha256(content), Bytes: int64(len(content)), RetainUntil: self.Plan.Owner.Deadline.Add(24 * time.Hour)}
	self.Archive[ref.Key+"/"+ref.VersionId] = append([]byte{}, content...)
	self.References[ref.Key] = ref
	return ref
}

func (self *winnerMultipassSpoolActor) read(_ context.Context, ref winnerMultipassProtectedReference) ([]byte, error) {
	content, found := self.Archive[ref.Key+"/"+ref.VersionId]
	if !found || self.References[ref.Key] != ref || int64(len(content)) != ref.Bytes || winnerMultipassSha256(content) != ref.Sha256 {
		return nil, errors.New("synthetic protected exact version disappeared or changed")
	}
	return append([]byte{}, content...), nil
}

func (self *winnerMultipassSpoolActor) resolve(ctx context.Context, pin currentQgQualificationPin, content []byte) (*winnerMultipassQualificationSpoolResolvedCapture, error) {
	if len(content) > winnerMultipassSpoolManifestBytes || winnerMultipassSha256(content) != pin.Sha256 {
		return nil, errors.New("synthetic compact commit changed")
	}
	var commit winnerMultipassSpoolActorCommit
	if decodeStrictJSONBytes(content, &commit, "synthetic protected-spool commit") != nil || commit.Schema != 1 || commit.Kind != "synthetic-protected-spool-commit/v1" {
		return nil, errors.New("synthetic commit malformed")
	}
	captureBytes, err := self.read(ctx, commit.Capture)
	if err != nil {
		return nil, err
	}
	if len(captureBytes) > winnerMultipassSpoolCaptureBytes {
		return nil, errors.New("synthetic full capture exceeds its independent record bound")
	}
	var capture winnerMultipassQualificationCapture
	if err := decodeStrictJSONBytes(captureBytes, &capture, "synthetic original protected capture"); err != nil {
		return nil, err
	}
	proof, err := loadWinnerMultipassPhysicalAuthentication(ctx, commit.Physical, self.Plan.Release, self.Plan.Owner.Deadline, self.read)
	if err != nil {
		return nil, err
	}
	value := &winnerMultipassQualificationSpoolResolvedCapture{Commit: pin, Capture: &capture, CaptureReference: commit.Capture, Originals: map[string][]byte{}, PhysicalAuthentication: proof}
	for name, reference := range commit.Originals {
		content, err := self.read(ctx, reference)
		if err != nil {
			return nil, err
		}
		value.Originals[name] = content
	}
	if commit.IndependentClaim != nil {
		value.IndependentGenerationClaim, err = self.read(ctx, *commit.IndependentClaim)
		if err != nil {
			return nil, err
		}
	}
	return value, nil
}

func (self *winnerMultipassSpoolActor) invoke(ctx context.Context, input []byte) ([]byte, []byte, error) {
	self.Invocations++
	var envelope winnerMultipassBrokerEnvelope
	if err := decodeStrictJSONBytes(input, &envelope, "synthetic intercepted wrapper input"); err != nil {
		return nil, nil, err
	}
	var item winnerMultipassQualificationCase
	for _, candidate := range self.Plan.Cases {
		if candidate.Plan.Owner == envelope.Owner {
			item = candidate
			break
		}
	}
	if item.Plan == nil {
		return nil, nil, errors.New("synthetic invocation is not a planned control")
	}
	canceled := envelope.Operation == "qualification_cancel_before_provider"
	var result *winnerMultipassBrokerResult
	output := []byte{}
	journal := map[string][]byte{}
	if !canceled {
		observation := winnerMultipassObservation{Summary: "Synthetic complete scoped observation, not provider evidence.", Issues: []winnerMultipassIssue{}, RetainedIssueIds: []string{}, DeferredArtifactIds: []string{}}
		if envelope.Call.Stage == "final" {
			mock := currentQgExecutionMockResponse(self.T, item.Bundle.Request, item.Expectation, nil)
			var broker winnerReviewBrokerResult
			var snow winnerSnowEvidence
			if json.Unmarshal(mock, &broker) != nil || json.Unmarshal([]byte(broker.PrivateEvidence), &snow) != nil {
				return nil, nil, errors.New("synthetic original final missing")
			}
			observation.FinalJudgment = &snow.Members[0].Judgment
		}
		encoded, _ := json.Marshal(observation)
		base := winnerMultipassProviderClient(self.T, item.Plan, envelope.Call, string(encoded), &self.Generations)
		client := &http.Client{Transport: winnerReviewTransport(func(request *http.Request) (*http.Response, error) {
			response, err := base.Transport.RoundTrip(request)
			if err != nil || request.URL.Path != "/v1/responses" {
				return response, err
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				return nil, err
			}
			body = bytes.Replace(body, []byte("synthetic-"+envelope.Call.Id), []byte(fmt.Sprintf("synthetic-control-%02d-%s", item.Position, envelope.Call.Id)), 1)
			response.Body = io.NopCloser(bytes.NewReader(body))
			return response, nil
		})}
		directory := winnerMultipassProviderDirectory(self.T)
		var err error
		result, err = executeWinnerMultipassBroker(ctx, client, envelope, item.Plan.Provider, directory, defaultWinnerMultipassProviderLimits())
		if err != nil {
			return nil, nil, err
		}
		output, _ = json.Marshal(result)
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, nil, err
		}
		for _, entry := range entries {
			content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
			if err != nil {
				return nil, nil, err
			}
			journal[entry.Name()] = content
		}
	}
	attempt := filepath.Join(self.Profile.StateRoot, fmt.Sprintf("invocation.synthetic-%04d", self.Invocations))
	provider := currentQgSnowQualificationProfile(self.Profile.ProviderSettings)
	originals := map[string][]byte{}
	for name, content := range self.Template {
		value := strings.ReplaceAll(string(content), self.TemplateAttempt, attempt)
		value = strings.ReplaceAll(value, self.TemplateProvider.ProviderHost, provider.ProviderHost)
		value = strings.ReplaceAll(value, self.TemplateProvider.ProviderIp, provider.ProviderIp)
		value = strings.ReplaceAll(value, strconv.Itoa(self.TemplateProvider.ProviderPort), strconv.Itoa(provider.ProviderPort))
		originals[name] = []byte(value)
	}
	originals["input.json"], originals["broker.stdout"] = input, output
	for name, content := range journal {
		originals[name] = content
	}
	owner := strings.Repeat("a", 64)
	var terminal map[string]any
	if err := json.Unmarshal(originals["terminal.json"], &terminal); err != nil {
		return nil, nil, err
	}
	status := 143
	if !canceled {
		status = 0
		terminal["wrapper_exit"], terminal["broker_exit"], terminal["broker_exit_observed"], terminal["provider_input_released"] = 0, 0, true, true
		originals["container-state.json"] = []byte(`{"Status":"exited","Running":false,"ExitCode":0,"OOMKilled":false}`)
	}
	originals["terminal.json"], _ = json.Marshal(terminal)
	var independent []byte
	if envelope.Operation == "generate" {
		members := []map[string]string{}
		for _, member := range item.Plan.Provider.Members {
			members = append(members, map[string]string{"name": member.Name, "model": member.Model, "revision": member.Revision, "reasoning": member.Reasoning})
		}
		memberBytes, _ := json.Marshal(members)
		independent, _ = json.Marshal(map[string]any{"schema": 1, "kind": "snow-multipass-single-call-claim", "idempotency_key_sha256": winnerMultipassSha256([]byte(envelope.Owner.Id + "/" + envelope.Call.Id)), "input_sha256": winnerMultipassSha256(input), "provider_settings_sha256": self.Profile.ProviderSettings.Sha256, "model_set_sha256": winnerMultipassSha256(memberBytes), "owner": owner, "attempt_directory": attempt, "automatic_retries": false})
		originals["attempt-claim.json"] = independent
	}
	names := []string{}
	for name := range originals {
		names = append(names, name)
	}
	sort.Strings(names)
	bindings := []currentQgQualificationBinding{}
	for _, name := range names {
		bindings = append(bindings, currentQgQualificationBinding{Path: name, Sha256: winnerMultipassSha256(originals[name]), Bytes: int64(len(originals[name]))})
	}
	manifest, _ := json.Marshal(map[string]any{"schema": 1, "kind": "winner-review-container-evidence", "artifacts": bindings})
	wrapper := currentQgQualificationWrapperReceipt{Schema: 1, Kind: "winner-review-container-receipt", Owner: owner, AttemptDirectory: attempt, ManifestSha256: winnerMultipassSha256(manifest), WrapperExit: status, CleanupVerified: true}
	diagnostics, _ := json.Marshal(wrapper)
	originals["evidence-manifest.json"], originals["wrapper-receipt.json"] = manifest, diagnostics
	receipt := &winnerMultipassContainmentReceipt{AttemptDirectory: attempt, ManifestSha256: wrapper.ManifestSha256, WrapperReceiptSha256: winnerMultipassSha256(diagnostics), WrapperReceipt: diagnostics, Artifacts: bindings, Result: result, Canceled: canceled}
	receipt.Artifacts = append(receipt.Artifacts, currentQgQualificationBinding{Path: "evidence-manifest.json", Sha256: wrapper.ManifestSha256, Bytes: int64(len(manifest))})
	self.Pending[winnerMultipassSha256(input)] = winnerMultipassSpoolActorPending{Receipt: receipt, Originals: originals, IndependentClaim: independent}
	return output, append(append([]byte{}, diagnostics...), '\n'), nil
}

func (self *winnerMultipassSpoolActor) hooks() winnerMultipassQualificationSpoolHooks {
	return winnerMultipassQualificationSpoolHooks{Mode: "offline", Now: func() time.Time {
		if self.PastDeadline {
			return self.Plan.Owner.Deadline.Add(time.Minute)
		}
		return time.Now().UTC()
	}, ValidateRelease: validateWinnerMultipassQualificationReleaseShape,
		Reserve: func(spool *winnerMultipassSpoolCapacity, aggregate *winnerMultipassQualificationCapacity) error {
			if spool.RequiredMemoryBytes != aggregate.RequiredMemoryBytes {
				return errors.New("synthetic memory census changed")
			}
			return nil
		},
		Load: func(invocation winnerMultipassQualificationInvocation) (*winnerMultipassQualificationSpoolResolvedCapture, error) {
			pin, found := self.Commits[invocation.Position]
			if !found {
				if _, started := self.Claims[invocation.Position]; started {
					return nil, errors.New("synthetic started intent has no complete custody")
				}
				return nil, nil
			}
			content, err := readCurrentQgQualificationPin(pin, winnerMultipassSpoolManifestBytes, true)
			if err != nil {
				return nil, err
			}
			return self.resolve(context.Background(), pin, content)
		},
		Claim: func(invocation winnerMultipassQualificationInvocation, content []byte) (currentQgQualificationPin, error) {
			if _, exists := self.Claims[invocation.Position]; exists {
				return currentQgQualificationPin{}, errors.New("synthetic one-shot intent already claimed")
			}
			path := filepath.Join(self.Directory, fmt.Sprintf("claim-%04d.json", invocation.Position))
			if err := writePrivateReviewFile(path, content); err != nil {
				return currentQgQualificationPin{}, err
			}
			pin := currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(content)}
			self.Claims[invocation.Position] = pin
			return pin, nil
		}, Invoke: self.invoke,
		Authenticate: func(input, output, diagnostics []byte, canceled bool) (*winnerMultipassContainmentReceipt, error) {
			self.PhysicalChecks++
			pending, found := self.Pending[winnerMultipassSha256(input)]
			if !found {
				return nil, errors.New("synthetic local capture already evicted; no reconstructed physical replay")
			}
			bound := map[string][]byte{}
			for name, content := range pending.Originals {
				if name != "wrapper-receipt.json" {
					bound[name] = content
				}
			}
			settings := defaultCurrentQgQualificationSettings()
			settings.StateRoot = self.Profile.StateRoot
			if _, err := validateWinnerMultipassContainmentBytes(input, output, diagnostics, canceled, self.Profile.ImageId, settings, currentQgSnowQualificationProfile(self.Profile.ProviderSettings), bound, pending.IndependentClaim); err != nil {
				return nil, err
			}
			return pending.Receipt, nil
		},
		Retain: func(ctx context.Context, capture winnerMultipassQualificationCapture) (currentQgQualificationPin, error) {
			key := winnerMultipassSha256(capture.Input)
			pending, found := self.Pending[key]
			if !found {
				return currentQgQualificationPin{}, errors.New("synthetic physical capture missing before custody")
			}
			captureBytes, _ := json.Marshal(capture)
			releaseBytes, _ := json.Marshal(self.Plan.Release)
			receiptBytes, _ := json.Marshal(capture.Containment)
			proof := winnerMultipassPhysicalAuthentication{Schema: 1, Kind: "winner-review-physical-containment-authentication/v1", Protocol: winnerMultipassVersion, ReleaseSha256: winnerMultipassSha256(releaseBytes), NativeDriver: self.Plan.Release.NativeDriver, Profile: self.Profile, ObservedAt: time.Now().UTC(), InputSha256: key, OutputSha256: winnerMultipassSha256(capture.Output), ManifestSha256: pending.Receipt.ManifestSha256, WrapperReceiptSha256: pending.Receipt.WrapperReceiptSha256, AuthenticatedReceiptSha256: winnerMultipassSha256(receiptBytes), Canceled: pending.Receipt.Canceled}
			if len(pending.IndependentClaim) != 0 {
				proof.IndependentClaimSha256 = winnerMultipassSha256(pending.IndependentClaim)
			}
			proofBytes, _ := json.Marshal(proof)
			commit := winnerMultipassSpoolActorCommit{Schema: 1, Kind: "synthetic-protected-spool-commit/v1", Capture: self.retain(captureBytes), Physical: self.retain(proofBytes), Originals: map[string]winnerMultipassProtectedReference{}}
			for name, content := range pending.Originals {
				commit.Originals[name] = self.retain(content)
			}
			if len(pending.IndependentClaim) != 0 {
				reference := self.retain(pending.IndependentClaim)
				commit.IndependentClaim = &reference
			}
			content, _ := json.Marshal(commit)
			if len(content) > winnerMultipassSpoolManifestBytes {
				return currentQgQualificationPin{}, errors.New("synthetic compact manifest oversized")
			}
			path := filepath.Join(self.Directory, fmt.Sprintf("commit-%04d.json", capture.Invocation.Position))
			if err := writePrivateReviewFile(path, content); err != nil {
				return currentQgQualificationPin{}, err
			}
			pin := currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(content)}
			value, err := self.resolve(ctx, pin, content)
			if err != nil {
				return currentQgQualificationPin{}, err
			}
			if _, err := replayWinnerMultipassPortableContainment(value.Originals, value.IndependentGenerationClaim, value.PhysicalAuthentication, self.Profile); err != nil {
				return currentQgQualificationPin{}, err
			}
			self.Commits[capture.Invocation.Position] = pin
			delete(self.Pending, key)
			return pin, nil
		},
		Resolve: func(ctx context.Context, pin currentQgQualificationPin) (*winnerMultipassQualificationSpoolResolvedCapture, error) {
			content, err := readCurrentQgQualificationPin(pin, winnerMultipassSpoolManifestBytes, true)
			if err != nil {
				return nil, err
			}
			return self.resolve(ctx, pin, content)
		},
		RetainCase: func(position int, evidence winnerMultipassEvidence) (currentQgQualificationPin, error) {
			content, _ := json.Marshal(evidence)
			path := filepath.Join(self.Directory, fmt.Sprintf("case-%02d.json", position))
			if existing, err := os.ReadFile(path); err == nil {
				if !bytes.Equal(existing, content) {
					return currentQgQualificationPin{}, errors.New("synthetic completed evidence changed")
				}
				return currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(content)}, nil
			}
			self.retain(content)
			if err := writePrivateReviewFile(path, content); err != nil {
				return currentQgQualificationPin{}, err
			}
			return currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(content)}, nil
		},
	}
}

// Every renderer, full visible transport, usage count, protected original,
// portable containment replay and all19case outcomes is composed here. Completed
// replay after deadline makes no calls; losing one protected version cannot reroll.
func TestWinnerMultipassSpoolQualificationCompletePipelineAndProtectedReplay(t *testing.T) {
	actor := newWinnerMultipassSpoolActor(t)
	planBytes, _ := json.Marshal(actor.Plan)
	planPin := currentQgQualificationPin{Path: filepath.Join(actor.Directory, "plan.json"), Sha256: winnerMultipassSha256(planBytes)}
	if err := writePrivateReviewFile(planPin.Path, planBytes); err != nil {
		t.Fatal(err)
	}
	settings := defaultWinnerMultipassCapacitySettings()
	settings.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	spool := winnerMultipassSpoolSettings{LifecycleVersion: winnerMultipassSpoolLifecycle, MaximumLocalPeakBytes: 8 * 1024 * 1024 * 1024, MaximumProtectedBytes: 128 * 1024 * 1024 * 1024}
	report, err := executeWinnerMultipassSpoolQualification(context.Background(), actor.Plan, planPin, settings, spool, actor.hooks())
	if err != nil || report == nil || !report.Complete || report.Qualified || len(report.CaptureCommits) != 181 || len(report.CaseEvidence) != 18 || actor.Invocations != 181 || actor.Generations != 72 || actor.PhysicalChecks != 181 || len(actor.Pending) != 0 {
		t.Fatalf("synthetic full spool pipeline incomplete or falsely qualified: %v invocations=%d generations=%d physical=%d", err, actor.Invocations, actor.Generations, actor.PhysicalChecks)
	}
	actor.PastDeadline = true
	reportPin, err := writeWinnerMultipassQualificationCanonical(filepath.Join(actor.Directory, "offline-spool-report.json"), report)
	if err != nil {
		t.Fatal(err)
	}
	replayHooks := actor.hooks()
	replayHooks.CompletedReport = &reportPin
	resumed, err := executeWinnerMultipassSpoolQualification(context.Background(), actor.Plan, planPin, settings, spool, replayHooks)
	if err != nil || !reflect.DeepEqual(report, resumed) || actor.Invocations != 181 || actor.PhysicalChecks != 181 || actor.Generations != 72 {
		t.Fatal("completed protected replay changed originals, reacquired physical facts or rerolled", err)
	}
	if _, err := authenticateWinnerMultipassSpoolQualification(reportPin, actor.Plan.Release, actor.resolve); err == nil {
		t.Fatal("complete synthetic/offline spool pipeline qualified a real release")
	}
	firstBytes, err := readCurrentQgQualificationPin(actor.Commits[1], winnerMultipassSpoolManifestBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	var first winnerMultipassSpoolActorCommit
	if err := json.Unmarshal(firstBytes, &first); err != nil {
		t.Fatal(err)
	}
	delete(actor.Archive, first.Physical.Key+"/"+first.Physical.VersionId)
	if _, err := executeWinnerMultipassSpoolQualification(context.Background(), actor.Plan, planPin, settings, spool, replayHooks); err == nil || actor.Invocations != 181 || actor.Generations != 72 {
		t.Fatal("missing protected original fell back to nonexistent local evidence or rerolled")
	}
}

// Failed physical authentication retains bounded original host bytes without a
// success proof, eviction, subsequent call or automatic retry.
func TestWinnerMultipassSpoolFailedAuthenticationRetainsTerminalAuditPin(t *testing.T) {
	actor := newWinnerMultipassSpoolActor(t)
	planPin, err := writeWinnerMultipassQualificationCanonical(filepath.Join(actor.Directory, "failed-plan.json"), actor.Plan)
	if err != nil {
		t.Fatal(err)
	}
	hooks := actor.hooks()
	hooks.Authenticate = func([]byte, []byte, []byte, bool) (*winnerMultipassContainmentReceipt, error) {
		return nil, errors.New("synthetic physical authentication failed")
	}
	var failureCapture winnerMultipassProtectedReference
	hooks.Retain = func(_ context.Context, capture winnerMultipassQualificationCapture) (currentQgQualificationPin, error) {
		if capture.Containment != nil || capture.AuthenticationError == "" {
			t.Fatal("failed physical authentication acquired a success proof")
		}
		content, _ := json.Marshal(capture)
		failureCapture = actor.retain(content)
		pin, err := writeWinnerMultipassQualificationCanonical(filepath.Join(actor.Directory, "failed-capture-audit.json"), map[string]any{"kind": "synthetic-failed-capture-audit/v1", "capture": failureCapture})
		if err != nil {
			return currentQgQualificationPin{}, err
		}
		return currentQgQualificationPin{}, &winnerMultipassQualificationSpoolRetentionError{Failure: pin, Cause: errors.New(capture.AuthenticationError)}
	}
	settings := defaultWinnerMultipassCapacitySettings()
	settings.MaximumDurableBytes = 128 * 1024 * 1024 * 1024
	spool := winnerMultipassSpoolSettings{LifecycleVersion: winnerMultipassSpoolLifecycle, MaximumLocalPeakBytes: 8 * 1024 * 1024 * 1024, MaximumProtectedBytes: 128 * 1024 * 1024 * 1024}
	report, err := executeWinnerMultipassSpoolQualification(context.Background(), actor.Plan, planPin, settings, spool, hooks)
	if err == nil || report == nil || report.Complete || report.Qualified || report.FailureCaptureCommit == nil || report.FailureInvocation == nil || report.FailureInvocation.Position != 1 || len(report.CaptureCommits) != 0 || actor.Invocations != 1 || actor.Generations != 0 || len(actor.Pending) != 1 {
		t.Fatal("failed authentication lost audit custody, evicted originals or continued", err)
	}
	content, err := actor.read(context.Background(), failureCapture)
	if err != nil {
		t.Fatal("failed original capture disappeared", err)
	}
	var capture winnerMultipassQualificationCapture
	if json.Unmarshal(content, &capture) != nil || capture.AuthenticationError == "" || capture.Containment != nil || len(capture.Input) == 0 || len(capture.ClaimContent) == 0 || len(capture.Diagnostics) == 0 {
		t.Fatal("failure custody omitted original bytes or invented physical evidence")
	}
	if _, err := executeWinnerMultipassSpoolQualification(context.Background(), actor.Plan, planPin, settings, spool, hooks); err == nil || actor.Invocations != 1 {
		t.Fatal("failed original invocation rerolled")
	}
	report.Mode, report.Complete, report.Qualified, report.Error = "genuine_provider", true, true, ""
	reportPin, err := writeWinnerMultipassQualificationCanonical(filepath.Join(actor.Directory, "tampered-failed-report.json"), report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateWinnerMultipassSpoolQualification(reportPin, actor.Plan.Release, actor.resolve); err == nil {
		t.Fatal("failure audit pin was converted into a qualified report")
	}
}
