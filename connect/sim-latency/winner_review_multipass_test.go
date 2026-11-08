// Synthetic, offline coverage/provenance tests never call a provider or use a
// production packet. Passing them is not a genuine-model qualification receipt.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Reuse the authenticated synthetic native-review fixture, not production data.
func winnerMultipassFixture(t *testing.T) (winnerMultipassBundle, *winnerMultipassPlan, winnerModelJudgment) {
	t.Helper()
	request, judgment := winnerSnowJudgmentFixture()
	bundle := winnerMultipassBundle{Request: request, SupplementalFiles: []winnerMultipassSourceFile{}, RequiredSupplementalPaths: []string{}, UnresolvedContext: []string{}}
	settings := defaultWinnerMultipassSettings()
	for _, member := range defaultWinnerSnowProviderSettings().Members {
		settings.TokenizationProfiles[member.Name] = winnerMultipassTokenizationProfile{Member: member, MaximumContextTokens: 100000,
			TokenizerContractSha256: winnerMultipassSha256([]byte("synthetic-render-contract")), RuntimeEvidenceSha256: winnerMultipassSha256([]byte("synthetic-runtime-proof"))}
	}
	started := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	plan, err := prepareWinnerMultipass(bundle, winnerMultipassOwner{Id: "synthetic-owner", StartedAt: started, Deadline: started.Add(time.Hour)}, settings, defaultWinnerSnowProviderSettings())
	if err != nil {
		t.Fatal(err)
	}
	return bundle, plan, judgment
}

// A test-only count is explicit synthetic input; production adapters must retain
// an actual rendered-token request/reply and independently qualified runtime pins.
func winnerMultipassAdmissionFixture(plan *winnerMultipassPlan, call winnerMultipassCall, payload []byte, count int) winnerMultipassAdmission {
	profile := plan.Settings.TokenizationProfiles[call.Member.Name]
	return winnerMultipassAdmission{CallId: call.Id, RequestSha256: winnerMultipassSha256(payload), Model: call.Member.Model, Revision: call.Member.Revision,
		TokenizerContractSha256: profile.TokenizerContractSha256, RenderedPromptSha256: winnerMultipassSha256([]byte("synthetic-render")),
		TokenizerRequestSha256: winnerMultipassSha256([]byte("synthetic-count-request")), TokenizerResponseSha256: winnerMultipassSha256([]byte("synthetic-count-response")),
		MaximumContextTokens: profile.MaximumContextTokens, InputTokens: count, OutputTokens: plan.Settings.MaximumOutputTokens, NoTruncation: true}
}

// Build real-shaped visible terminals solely for offline deterministic validators.
func winnerMultipassEvidenceFixture(t *testing.T, bundle winnerMultipassBundle, plan *winnerMultipassPlan, judgment winnerModelJudgment) winnerMultipassEvidence {
	t.Helper()
	planBytes, _ := json.Marshal(plan)
	evidence := winnerMultipassEvidence{Version: winnerMultipassVersion, PlanSha256: winnerMultipassSha256(planBytes), BundleSha256: plan.BundleSha256, Calls: []winnerMultipassCallEvidence{}}
	completed := map[string]winnerMultipassCallEvidence{}
	for index, call := range plan.Calls {
		payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, completed)
		if err != nil {
			t.Fatal(err)
		}
		observation := winnerMultipassObservation{Summary: "Synthetic scoped observation; no provider was called.", Issues: []winnerMultipassIssue{}, RetainedIssueIds: []string{}, DeferredArtifactIds: []string{}}
		if call.Stage == "final" {
			copied := judgment
			observation.FinalJudgment = &copied
		}
		output, _ := json.Marshal(observation)
		response, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("synthetic-response-%04d", index), "status": "completed", "model": call.Member.Model,
			"usage": map[string]int{"input_tokens": 100, "output_tokens": 100}, "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": string(output)}}}}})
		item := winnerMultipassCallEvidence{CallId: call.Id, StartedAt: plan.Owner.StartedAt.Add(time.Duration(index*2) * time.Second), CompletedAt: plan.Owner.StartedAt.Add(time.Duration(index*2+1) * time.Second),
			Request: payload, Admission: winnerMultipassAdmissionFixture(plan, call, payload, 200), VisibleResponse: response, VisibleResponseSha256: winnerMultipassSha256(response),
			TransportResponseSha256: winnerMultipassSha256(response), TransportReceiptSha256: winnerMultipassSha256([]byte("synthetic-transport-proof")), ProviderOutput: string(output), Observation: observation}
		evidence.Calls = append(evidence.Calls, item)
		completed[call.Id] = item
	}
	return evidence
}

// All before/after/patch/score/instrument bytes have exactly one core owner.
func TestWinnerMultipassCoverageIncludesCompleteBeforeAndAfter(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	artifacts, err := winnerMultipassArtifacts(bundle, plan.Settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		position := 0
		seen := false
		for _, unit := range plan.Units {
			for _, span := range unit.Spans {
				if span.ArtifactId != artifact.Id {
					continue
				}
				if span.Start != position || span.End < span.Start || span.ContextStart > span.Start || span.ContextEnd < span.End || span.ContextEnd > len(artifact.Content) {
					t.Fatal("coverage gap or invalid context")
				}
				if span.Sha256 != winnerMultipassSha256([]byte(artifact.Content[span.Start:span.End])) {
					t.Fatal("span digest does not bind original bytes")
				}
				position, seen = span.End, true
			}
		}
		if !seen || position != len(artifact.Content) {
			t.Fatalf("incomplete artifact %s", artifact.Id)
		}
	}
	index := slices.IndexFunc(artifacts, func(artifact winnerMultipassArtifact) bool { return artifact.Kind == "candidate" })
	if index < 0 || !strings.Contains(artifacts[index].Content, "return len(packet)") || artifacts[index].DerivationSha256 != bundle.Request.Template.PatchSha256 {
		t.Fatal("complete exact post-patch source is missing")
	}
}

// A digest-echoed but altered frozen base cannot produce plausible candidate code.
func TestWinnerMultipassPatchApplicationRejectsWrongFrozenContext(t *testing.T) {
	_, err := applyWinnerMultipassPatchSection("package example\nold\n", "diff --git a/example.go b/example.go\n--- a/example.go\n+++ b/example.go\n@@ -1,2 +1,2 @@\n package example\n-different\n+new\n")
	if err == nil {
		t.Fatal("wrong frozen context was fuzzily accepted")
	}
}

// No-newline markers bind actual terminal bytes rather than a normalized copy.
func TestWinnerMultipassPatchApplicationPreservesMissingFinalNewline(t *testing.T) {
	after, err := applyWinnerMultipassPatchSection("old", "diff --git a/example.go b/example.go\n--- a/example.go\n+++ b/example.go\n@@ -1 +1 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n")
	if err != nil || after != "new" {
		t.Fatalf("missing newline changed: %q %v", after, err)
	}
}

// Multiple hunks require both old and new coordinates without offset heuristics.
func TestWinnerMultipassPatchApplicationChecksMultipleHunkCoordinates(t *testing.T) {
	section := "diff --git a/example.go b/example.go\n--- a/example.go\n+++ b/example.go\n@@ -1 +1,2 @@\n a\n+inserted\n@@ -3 +4 @@\n-c\n+changed\n"
	after, err := applyWinnerMultipassPatchSection("a\nb\nc\n", section)
	if err != nil || after != "a\ninserted\nb\nchanged\n" {
		t.Fatalf("multiple hunk derivation failed: %q %v", after, err)
	}
	if _, err := applyWinnerMultipassPatchSection("a\nb\nc\n", strings.Replace(section, "+4 @@", "+3 @@", 1)); err == nil {
		t.Fatal("wrong candidate coordinates accepted")
	}
}

// Empty/absent evidence remains an explicit covered ledger entry.
func TestWinnerMultipassEmptyArtifactHasExplicitCoverage(t *testing.T) {
	artifact := winnerMultipassArtifact{Id: "synthetic-empty", Kind: "instrument", Path: "server/empty.go", Commit: strings.Repeat("a", 40), Sha256: winnerMultipassSha256(nil), Absent: true}
	spans, err := splitWinnerMultipassArtifact(artifact, defaultWinnerMultipassSettings())
	if err != nil || len(spans) != 1 || spans[0].Start != 0 || spans[0].End != 0 {
		t.Fatal("absence vanished from coverage")
	}
}

// Small line overlap bounds cannot walk away from the core or loop forever.
func TestWinnerMultipassOverlapIsBoundedAtLongLineEdges(t *testing.T) {
	content := strings.Repeat("// "+strings.Repeat("x", 60)+"\n", 8)
	settings := defaultWinnerMultipassSettings()
	settings.SpanBytes = 128
	settings.MaximumOverlapBytes = 3
	spans, err := splitWinnerMultipassArtifact(winnerMultipassArtifact{Id: "synthetic", Path: "server/example.go", Content: content}, settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, span := range spans {
		if span.Start-span.ContextStart > settings.MaximumOverlapBytes || span.ContextEnd-span.End > settings.MaximumOverlapBytes {
			t.Fatal("overlap escaped its byte bound")
		}
	}
}

// A declaration too large for one atom is honestly labeled as split, not whole.
func TestWinnerMultipassSplitDeclarationIsExplicit(t *testing.T) {
	content := "package example\nfunc Work() {\n" + strings.Repeat(" println(1)\n", 30) + "}\n"
	settings := defaultWinnerMultipassSettings()
	settings.SpanBytes = 100
	spans, err := splitWinnerMultipassArtifact(winnerMultipassArtifact{Id: "synthetic", Path: "server/example.go", Content: content}, settings)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(spans, func(span winnerMultipassSpan) bool { return span.SplitDeclaration }) {
		t.Fatal("partial function was presented as whole")
	}
}

// The native validator recomputes coverage; changing hashes cannot hide a gap.
func TestWinnerMultipassPlanRejectsRemovedCoverageSpan(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	plan.Units[0].Spans = plan.Units[0].Spans[1:]
	if validateWinnerMultipassPlan(plan, bundle) == nil {
		t.Fatal("coverage omission accepted")
	}
}

// Both models must cover exactly the same complete artifacts, even with unequal packing.
func TestWinnerMultipassGroupingRejectsDuplicateOrMissingUnits(t *testing.T) {
	_, plan, _ := winnerMultipassFixture(t)
	groups := append([]winnerMultipassScanGroup{}, plan.ScanGroups...)
	groups[0].UnitIds = append(groups[0].UnitIds, groups[0].UnitIds[0])
	if setWinnerMultipassScanGroups(plan, groups) == nil {
		t.Fatal("duplicate coverage atom accepted")
	}
}

// Supplemental source must match an explicit census and the authenticated map.
func TestWinnerMultipassRejectsMovingSupplementalCommit(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	bundle.Request.Packet.SourceCommits = map[string]string{"server": strings.Repeat("a", 40)}
	bundle.RequiredSupplementalPaths = []string{"server/dependency.go"}
	bundle.SupplementalFiles = []winnerMultipassSourceFile{{Path: "server/dependency.go", Commit: strings.Repeat("b", 40), Content: "package example\n", Sha256: winnerMultipassSha256([]byte("package example\n"))}}
	if _, err := winnerMultipassArtifacts(bundle, plan.Settings); err == nil {
		t.Fatal("moving dependency commit accepted")
	}
}

// Passing offline evidence retains original final outputs and all twelve findings.
func TestWinnerMultipassCompleteEvidenceRetainsTwelveAttributedFindings(t *testing.T) {
	bundle, plan, judgment := winnerMultipassFixture(t)
	evidence := winnerMultipassEvidenceFixture(t, bundle, plan, judgment)
	combined, err := validateWinnerMultipassEvidence(plan, bundle, evidence)
	if err != nil || combined.Decision != "approve" || len(combined.Findings) != 12 {
		t.Fatalf("complete synthetic evidence failed: %s %v", combined.Decision, err)
	}
	for _, finding := range combined.Findings {
		if finding.Model == "" {
			t.Fatal("final finding lost reviewer attribution")
		}
	}
}

// Partial success never substitutes for the missing member or final synthesis.
func TestWinnerMultipassCompletedPrefixCannotAuthorizeFinal(t *testing.T) {
	bundle, plan, judgment := winnerMultipassFixture(t)
	evidence := winnerMultipassEvidenceFixture(t, bundle, plan, judgment)
	evidence.Calls = evidence.Calls[:1]
	if _, err := validateWinnerMultipassCompletedPrefix(plan, bundle, evidence); err != nil {
		t.Fatal(err)
	}
	if combined, err := validateWinnerMultipassEvidence(plan, bundle, evidence); err == nil || combined.Decision != "uncertain" {
		t.Fatal("partial coverage authorized a result")
	}
}

// Decoded fields cannot overwrite the original provider judgment on replay.
func TestWinnerMultipassRejectsAlteredDecodedObservation(t *testing.T) {
	bundle, plan, judgment := winnerMultipassFixture(t)
	evidence := winnerMultipassEvidenceFixture(t, bundle, plan, judgment)
	evidence.Calls[0].Observation.Summary = "replacement"
	if _, err := validateWinnerMultipassEvidence(plan, bundle, evidence); err == nil {
		t.Fatal("altered decoded output accepted")
	}
}

// New deadlines and correctly rehashed terminal bytes cannot conceal late work.
func TestWinnerMultipassRejectsLateOrRenewedOwner(t *testing.T) {
	bundle, plan, judgment := winnerMultipassFixture(t)
	evidence := winnerMultipassEvidenceFixture(t, bundle, plan, judgment)
	evidence.Calls[0].CompletedAt = plan.Owner.Deadline.Add(time.Second)
	if _, err := validateWinnerMultipassEvidence(plan, bundle, evidence); err == nil {
		t.Fatal("late provider completion accepted")
	}
	evidence = winnerMultipassEvidenceFixture(t, bundle, plan, judgment)
	plan.Owner.Deadline = plan.Owner.Deadline.Add(time.Minute)
	if _, err := validateWinnerMultipassEvidence(plan, bundle, evidence); err == nil {
		t.Fatal("renewed owner accepted old evidence")
	}
}

// A failed/ambiguous call is retained but cannot be reused as a completed prefix.
func TestWinnerMultipassFailedCallCannotResumeAsSuccess(t *testing.T) {
	bundle, plan, judgment := winnerMultipassFixture(t)
	evidence := winnerMultipassEvidenceFixture(t, bundle, plan, judgment)
	evidence.Calls[0].Error = "synthetic transport failure"
	if _, err := validateWinnerMultipassCompletedPrefix(plan, bundle, evidence); err == nil {
		t.Fatal("failed call granted resume authority")
	}
}

// Token admission binds exact runtime identity, full payload and output reserve.
func TestWinnerMultipassTokenAdmissionChecksActualCapacity(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	call := plan.Calls[0]
	payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	profile := plan.Settings.TokenizationProfiles[call.Member.Name]
	admission := winnerMultipassAdmissionFixture(plan, call, payload, profile.MaximumContextTokens-plan.Settings.MaximumOutputTokens)
	if err := validateWinnerMultipassAdmission(plan, call, payload, admission); err != nil {
		t.Fatal(err)
	}
	admission.InputTokens++
	if validateWinnerMultipassAdmission(plan, call, payload, admission) == nil {
		t.Fatal("over-context call admitted")
	}
	admission.InputTokens--
	admission.MaximumContextTokens++
	if validateWinnerMultipassAdmission(plan, call, payload, admission) == nil {
		t.Fatal("invented larger runtime admitted")
	}
}

// Offline layout is possible without runtime metadata, but no inference is admitted.
func TestWinnerMultipassMissingRuntimeProofRefusesAdmission(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	call := plan.Calls[0]
	payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
	if err != nil {
		t.Fatal(err)
	}
	admission := winnerMultipassAdmissionFixture(plan, call, payload, 100)
	plan.Settings.TokenizationProfiles = map[string]winnerMultipassTokenizationProfile{}
	if validateWinnerMultipassAdmission(plan, call, payload, admission) == nil {
		t.Fatal("unverified runtime admitted")
	}
}

// Scoped scans cannot emit a native full-candidate verdict.
func TestWinnerMultipassScopedScanCannotApprove(t *testing.T) {
	bundle, plan, judgment := winnerMultipassFixture(t)
	observation := winnerMultipassObservation{Summary: "synthetic", Issues: []winnerMultipassIssue{}, RetainedIssueIds: []string{}, DeferredArtifactIds: []string{}, FinalJudgment: &judgment}
	if validateWinnerMultipassObservation(plan, bundle, plan.Calls[0], observation, nil) == nil {
		t.Fatal("scoped scan issued global approval")
	}
}

// Every inherited issue survives even a unanimous-looking final response.
func TestWinnerMultipassFinalCannotEraseAnUncertainIssue(t *testing.T) {
	bundle, plan, judgment := winnerMultipassFixture(t)
	call := plan.Calls[len(plan.Calls)-1]
	issue := winnerMultipassAttributedIssue{Id: "synthetic/issue-0001", CallId: "synthetic", Issue: winnerMultipassIssue{Criterion: "source_safety", Disposition: "uncertain", ReasonCode: "SOURCE_NOT_READY", Explanation: "Synthetic missing helper", ArtifactIds: []string{plan.Artifacts[0].Id}}}
	observation := winnerMultipassObservation{Summary: "synthetic", Issues: []winnerMultipassIssue{}, RetainedIssueIds: []string{issue.Id}, DeferredArtifactIds: []string{}, FinalJudgment: &judgment}
	if validateWinnerMultipassObservation(plan, bundle, call, observation, []winnerMultipassAttributedIssue{issue}) == nil {
		t.Fatal("uncertainty erased by final approval")
	}
	observation.RetainedIssueIds = []string{}
	if validateWinnerMultipassObservation(plan, bundle, call, observation, []winnerMultipassAttributedIssue{issue}) == nil {
		t.Fatal("inherited issue omitted")
	}
}

// An absent helper cannot be mislabeled as planned deferred context.
func TestWinnerMultipassRejectsUnplannedDeferredArtifact(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	observation := winnerMultipassObservation{Summary: "synthetic", Issues: []winnerMultipassIssue{}, RetainedIssueIds: []string{}, DeferredArtifactIds: []string{"dependency:server/not-supplied.go"}}
	if validateWinnerMultipassObservation(plan, bundle, plan.Calls[0], observation, nil) == nil {
		t.Fatal("missing context was disguised as future planned source")
	}
}

// Per-model grouping can differ, but exact final payloads are always recounted.
func TestWinnerMultipassProspectivePackingUsesTokenizerAndFinalDigest(t *testing.T) {
	bundle, initial, _ := winnerMultipassFixture(t)
	initial.Settings.SpanBytes = 128
	plan, err := prepareWinnerMultipass(bundle, initial.Owner, initial.Settings, initial.Provider)
	if err != nil {
		t.Fatal(err)
	}
	queries := 0
	packing, err := packWinnerMultipass(context.Background(), plan, bundle, func(_ context.Context, call winnerMultipassCall, payload []byte) (winnerMultipassAdmission, error) {
		queries++
		count := len(call.UnitIds) * 20000
		if call.Member.Name == plan.Provider.Members[1].Name {
			count /= 2
		}
		return winnerMultipassAdmissionFixture(plan, call, payload, count), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if queries <= len(packing.FinalAdmissions) || len(packing.Plan.Calls) >= len(plan.Calls) {
		t.Fatal("token packing did not reduce calls or recount final payloads")
	}
	if err := validateWinnerMultipassLeafAdmissions(packing.Plan, bundle, packing.FinalAdmissions); err != nil {
		t.Fatal(err)
	}
	packing.FinalAdmissions[0].RequestSha256 = winnerMultipassSha256([]byte("old-draft-prompt"))
	if validateWinnerMultipassLeafAdmissions(packing.Plan, bundle, packing.FinalAdmissions) == nil {
		t.Fatal("draft token count admitted final prompt")
	}
}

// Cancellation is checked before a single tokenizer request; there is no retry loop.
func TestWinnerMultipassPackingCancellationMakesNoTokenizerCall(t *testing.T) {
	bundle, plan, _ := winnerMultipassFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err := packWinnerMultipass(ctx, plan, bundle, func(context.Context, winnerMultipassCall, []byte) (winnerMultipassAdmission, error) {
		called = true
		return winnerMultipassAdmission{}, nil
	})
	if err == nil || called {
		t.Fatal("canceled planning invoked tokenizer")
	}
}
