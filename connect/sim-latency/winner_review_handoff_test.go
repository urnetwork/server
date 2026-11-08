// Synthetic hooks exercise custody and authority boundaries without a model,
// production packet, API, database credential or candidate execution.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

type winnerHandoffFixture struct {
	owner                                             model.CompetitionReviewHandoff
	plan                                              *winnerMultipassPlan
	bundle                                            winnerMultipassBundle
	evidence                                          winnerMultipassEvidence
	now                                               time.Time
	claims, invokes, retains, completions, admissions int
	claimed                                           map[string]bool
	failInvoke                                        bool
	tooManyTokens                                     bool
	lastCompletion                                    *model.CompetitionReviewHandoffCompletion
}

func newWinnerHandoffFixture(t *testing.T, disqualify bool) *winnerHandoffFixture {
	t.Helper()
	bundle, draft, judgment := winnerMultipassFixture(t)
	if disqualify {
		judgment.Decision = "disqualify"
		judgment.ReasonCodes = []string{"SCORE_TAMPERING"}
		judgment.Findings[0].Disposition = "fail"
		judgment.Findings[0].ReasonCode = "SCORE_TAMPERING"
	}
	request, template := bundle.Request.Request, bundle.Request.Template
	provider, _ := json.Marshal(draft.Provider)
	owner := model.CompetitionReviewHandoff{HandoffId: server.NewId(), ActorId: "synthetic-capture", StartedAt: draft.Owner.StartedAt, Deadline: draft.Owner.Deadline,
		Args: model.CompetitionReviewHandoffArgs{RoundId: request.RoundId, JobId: request.JobId, RewardRevision: request.RewardRevision, DecisionSha256: request.RewardDecisionSha256, SealSha256: request.SealSha256,
			PatchSha256: template.PatchSha256, BaseSourceLockSha256: template.BaseSourceLockSha256, ScoreSha256: template.ScoreSha256, ArtifactManifestSha256: template.ArtifactManifestSha256, BaselineSha256: template.BaselineSha256,
			AuthorizationSha256: winnerMultipassSha256([]byte("synthetic-run-authority")), ProviderSha256: winnerMultipassSha256(provider), Protocol: model.CompetitionReviewHandoffProtocol, TimeoutSeconds: 3600}}
	plan, bundle, err := winnerHandoffPlan(owner, bundle, draft.Settings, draft.Provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	planBytes, _ := json.Marshal(plan)
	owner.Plan = model.CompetitionReviewHandoffPlan{PlanSha256: winnerMultipassSha256(planBytes), BundleSha256: plan.BundleSha256, CallIds: []string{}}
	for _, call := range plan.Calls {
		owner.Plan.CallIds = append(owner.Plan.CallIds, call.Id)
	}
	evidence := winnerMultipassEvidenceFixture(t, bundle, plan, judgment)
	return &winnerHandoffFixture{owner: owner, plan: plan, bundle: bundle, evidence: evidence, now: owner.StartedAt, claimed: map[string]bool{}}
}

func (self *winnerHandoffFixture) hooks(t *testing.T) winnerHandoffCaptureHooks {
	t.Helper()
	return winnerHandoffCaptureHooks{Now: func() time.Time { return self.now },
		Reserve: func(_ context.Context, capacity *winnerMultipassCapacity) (winnerHandoffCapacityReservation, error) {
			content, _ := json.Marshal(capacity)
			return winnerHandoffCapacityReservation{CapacitySha256: winnerMultipassSha256(content), ReservationPath: "/synthetic/review/reservation", ReservationInode: 1,
				ReservedBytes: capacity.RequiredDurableBytes, MemoryLimitBytes: capacity.Settings.MaximumMemoryBytes, HostAvailableBytes: capacity.Settings.MaximumMemoryBytes,
				MemoryCgroup: "/synthetic/cgroup", StoragePaths: []string{"/synthetic/review"}}, nil
		},
		Admit: func(_ context.Context, call winnerMultipassCall, payload []byte) (winnerHandoffTokenization, error) {
			self.admissions++
			count := 100
			if self.tooManyTokens {
				count = 1000000
			}
			admission := winnerMultipassAdmissionFixture(self.plan, call, payload, count)
			return winnerHandoffTokenization{Admission: admission, Custody: self.custody("tokenize", call, payload, admission, "")}, nil
		},
		Claim: func(_ context.Context, value model.CompetitionReviewHandoffCall) (*model.CompetitionReviewHandoffCall, error) {
			if self.claimed[value.CallId] {
				return nil, controller.ErrReviewCallAlreadyClaimed
			}
			self.claimed[value.CallId] = true
			value.RecordedAt = self.now
			self.claims++
			return &value, nil
		},
		Invoke: func(_ context.Context, call winnerMultipassCall, payload []byte, admission winnerMultipassAdmission, claimSha string) (winnerHandoffTransport, error) {
			if !self.claimed[call.Id] || !validSha256(claimSha) {
				t.Fatal("provider called before its durable claim")
			}
			original := self.evidence.Calls[self.invokes]
			self.invokes++
			self.now = self.now.Add(time.Second)
			if original.CallId != call.Id || !bytes.Equal(original.Request, payload) {
				t.Fatal("provider input differs from fixed complete plan")
			}
			if self.failInvoke {
				return winnerHandoffTransport{VisibleResponse: []byte("synthetic incomplete terminal"), Error: "synthetic_transport_failure"}, errors.New("synthetic failure")
			}
			return winnerHandoffTransport{VisibleResponse: original.VisibleResponse, TransportResponseSha256: original.TransportResponseSha256, ContainmentReceiptSha256: original.TransportReceiptSha256,
				Custody: self.custody("generate", call, payload, admission, original.TransportResponseSha256)}, nil
		},
		Receipt: func(_ context.Context, value model.CompetitionReviewHandoffCall) (*model.CompetitionReviewHandoffCall, error) {
			if !self.claimed[value.CallId] || !validSha256(value.EvidenceSha256) {
				t.Fatal("receipt lacks claim or original evidence")
			}
			value.RecordedAt = self.now
			self.now = self.now.Add(time.Second)
			return &value, nil
		},
		Retain: func(_ context.Context, content []byte, digest string) (*controller.CompetitionWinnerReviewEvidence, error) {
			self.retains++
			if winnerMultipassSha256(content) != digest {
				t.Fatal("retention digest changed")
			}
			return &controller.CompetitionWinnerReviewEvidence{Key: "synthetic/evidence", VersionId: fmt.Sprintf("synthetic-version-%d", self.retains), Sha256: digest, RetainUntil: self.owner.Deadline.Add(time.Hour)}, nil
		},
		Complete: func(_ context.Context, value model.CompetitionReviewHandoffCompletion) (*model.CompetitionReviewHandoffCompletion, error) {
			self.completions++
			if self.lastCompletion != nil {
				return nil, controller.ErrConflict
			}
			value.CompletedAt = self.now
			self.lastCompletion = &value
			return &value, nil
		},
	}
}

// This is a synthetic archive-receipt seam, not a containment qualification.
// Separate wrapper tests must prove the actual originals and protected readback.
func (self *winnerHandoffFixture) custody(operation string, call winnerMultipassCall, payload []byte, admission winnerMultipassAdmission, transportSha string) winnerHandoffInvocationCustody {
	manifestSha := winnerMultipassSha256([]byte("synthetic-wrapper-manifest"))
	receiptSha := winnerMultipassSha256([]byte("synthetic-transport-proof"))
	value := winnerHandoffInvocationCustody{Operation: operation, CallId: call.Id, RequestSha256: winnerMultipassSha256(payload), ManifestSha256: manifestSha, WrapperReceiptSha256: receiptSha, Originals: []winnerHandoffCustodyOriginal{}}
	leaves := map[string]string{"call.started.json": winnerMultipassSha256([]byte("synthetic-started")), "call.complete.json": winnerMultipassSha256([]byte("synthetic-completed")), "evidence-manifest.json": manifestSha, "wrapper-receipt.json": receiptSha}
	if operation == "tokenize" {
		leaves["tokenize.request.json"], leaves["tokenize.response.json"] = admission.TokenizerRequestSha256, admission.TokenizerResponseSha256
	} else {
		leaves["transport.response"] = transportSha
	}
	for name, digest := range leaves {
		value.Originals = append(value.Originals, winnerHandoffCustodyOriginal{Name: name, Bytes: 1,
			Retention: model.CompetitionReviewHandoffRetention{Key: "synthetic/original/" + name, VersionId: "synthetic-version", Sha256: digest, RetainUntil: self.owner.Deadline.Add(time.Hour)}})
	}
	return value
}

func TestWinnerHandoffCompleteCaptureLeavesBothPossibleProposalsPending(t *testing.T) {
	for _, disqualify := range []bool{false, true} {
		fixture := newWinnerHandoffFixture(t, disqualify)
		result, err := captureWinnerHandoff(t.Context(), filepath.Join(t.TempDir(), "capture"), &fixture.owner, fixture.plan, fixture.bundle, defaultWinnerHandoffCaptureSettings(), fixture.hooks(t))
		if err != nil || result.Status != "MODEL_FINDINGS_PENDING_OPERATOR" || result.PromotionAuthorized || fixture.invokes != len(fixture.plan.Calls) || fixture.completions != 1 || result.Completion.Proposal == nil {
			t.Fatal("complete original evidence lost pending-only authority", err)
		}
		if disqualify && result.Completion.Proposal.RewardDisposition != "disqualified" {
			t.Fatal("genuine adverse model proposal was erased")
		}
		if err := validateWinnerHandoffCompletion(&fixture.owner, result.Completion, fixture.plan, fixture.bundle, result.Evidence, result.Custody); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWinnerHandoffFailedOriginalIsRetainedWithoutRetry(t *testing.T) {
	fixture := newWinnerHandoffFixture(t, false)
	fixture.failInvoke = true
	result, err := captureWinnerHandoff(t.Context(), filepath.Join(t.TempDir(), "capture"), &fixture.owner, fixture.plan, fixture.bundle, defaultWinnerHandoffCaptureSettings(), fixture.hooks(t))
	if err == nil || fixture.invokes != 1 || fixture.claims != 1 || fixture.retains < 2 || fixture.lastCompletion == nil || fixture.lastCompletion.Proposal != nil || fixture.lastCompletion.Failure == "" || len(result.Evidence.Calls) != 1 || string(result.Evidence.Calls[0].VisibleResponse) != "synthetic incomplete terminal" {
		t.Fatal("failed provider original was lost or rerolled", err)
	}
	_, err = captureWinnerHandoff(t.Context(), filepath.Join(t.TempDir(), "second-path"), &fixture.owner, fixture.plan, fixture.bundle, defaultWinnerHandoffCaptureSettings(), fixture.hooks(t))
	if err == nil || fixture.invokes != 1 {
		t.Fatal("new local directory resampled the same durable owner")
	}
}

func TestWinnerHandoffOverLimitPreflightMakesNoClaimOrDraw(t *testing.T) {
	fixture := newWinnerHandoffFixture(t, false)
	fixture.tooManyTokens = true
	_, err := captureWinnerHandoff(t.Context(), filepath.Join(t.TempDir(), "capture"), &fixture.owner, fixture.plan, fixture.bundle, defaultWinnerHandoffCaptureSettings(), fixture.hooks(t))
	if err == nil || fixture.claims != 0 || fixture.invokes != 0 || fixture.lastCompletion == nil || fixture.lastCompletion.Proposal != nil {
		t.Fatal("unadmitted complete input consumed a draw or conferred authority", err)
	}
}

func TestWinnerHandoffMissingProtectedTokenizerStopsBeforeAnyClaim(t *testing.T) {
	fixture := newWinnerHandoffFixture(t, false)
	hooks := fixture.hooks(t)
	admit := hooks.Admit
	hooks.Admit = func(ctx context.Context, call winnerMultipassCall, payload []byte) (winnerHandoffTokenization, error) {
		value, err := admit(ctx, call, payload)
		for index := range value.Custody.Originals {
			if value.Custody.Originals[index].Name == "tokenize.response.json" {
				value.Custody.Originals[index].Retention.VersionId = ""
			}
		}
		return value, err
	}
	result, err := captureWinnerHandoff(t.Context(), filepath.Join(t.TempDir(), "capture"), &fixture.owner, fixture.plan, fixture.bundle, defaultWinnerHandoffCaptureSettings(), hooks)
	if err == nil || fixture.claims != 0 || fixture.invokes != 0 || result.Completion == nil || result.Completion.Proposal != nil {
		t.Fatal("digest-only tokenizer evidence reached inference or proposal", err)
	}
}

func TestWinnerHandoffCapacityRefusesBeforeTokenizerOrDraw(t *testing.T) {
	for _, memory := range []bool{false, true} {
		fixture := newWinnerHandoffFixture(t, false)
		settings := defaultWinnerHandoffCaptureSettings()
		if memory {
			settings.Capacity.MaximumMemoryBytes = 1
		} else {
			settings.Capacity.MaximumDurableBytes = 1
		}
		_, err := captureWinnerHandoff(t.Context(), filepath.Join(t.TempDir(), "capture"), &fixture.owner, fixture.plan, fixture.bundle, settings, fixture.hooks(t))
		if err == nil || fixture.admissions != 0 || fixture.claims != 0 || fixture.invokes != 0 {
			t.Fatal("impossible fixed review budget consumed provider work", memory, err)
		}
	}
}

func TestWinnerHandoffCompletionRejectsDisappearingTransportOriginal(t *testing.T) {
	fixture := newWinnerHandoffFixture(t, false)
	result, err := captureWinnerHandoff(t.Context(), filepath.Join(t.TempDir(), "capture"), &fixture.owner, fixture.plan, fixture.bundle, defaultWinnerHandoffCaptureSettings(), fixture.hooks(t))
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	for index := range result.Custody.Invocations {
		invocation := &result.Custody.Invocations[index]
		if invocation.Operation == "generate" && !changed {
			for originalIndex := range invocation.Originals {
				if invocation.Originals[originalIndex].Name == "transport.response" {
					invocation.Originals[originalIndex].Retention.VersionId = ""
					changed = true
				}
			}
		}
	}
	if !changed {
		t.Fatal("synthetic transport original was absent")
	}
	if err := validateWinnerHandoffCompletion(&fixture.owner, result.Completion, fixture.plan, fixture.bundle, result.Evidence, result.Custody); err == nil {
		t.Fatal("a local hash without independently retrievable transport version remained importable")
	}
}
