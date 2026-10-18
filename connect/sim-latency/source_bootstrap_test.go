// One-time baseline provisioning uses explicit policy approval, never an
// invented source review or a shortcut around runtime preparation.
package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Binds the approved policy to the same immutable baseline-only manifest.
func TestBootstrapRequiresExplicitExactBaselinePolicy(t *testing.T) {
	manifest := reviewedSourceTestManifest(t)
	generation := manifest.EvaluationSource.Generations[0]
	settings := &controller.Settings{BaseSha: generation.Repositories.Server.Commit,
		AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, PolicyApprovalSha256: strings.Repeat("e", 64), SourceLockSha256: generation.SourceLockSha256, SourceActivationGeneration: 1},
	}
	settings.EvaluationPolicy.TakeoverMargin = 0.161
	if err := validateBootstrapSource(manifest, settings); err != nil {
		t.Fatal(err)
	}
	settings.AutomationPolicy.PolicyApprovalSha256 = ""
	if err := validateBootstrapSource(manifest, settings); err == nil {
		t.Fatal("bootstrap invented policy approval")
	}
	settings.AutomationPolicy.PolicyApprovalSha256 = strings.Repeat("e", 64)
	settings.BaseSha = strings.Repeat("f", 40)
	if err := validateBootstrapSource(manifest, settings); err == nil {
		t.Fatal("bootstrap ignored the approved base")
	}
	settings.BaseSha = generation.Repositories.Server.Commit
	manifest.EvaluationSource.Rounds[0].Epoch = 2
	if err := validateBootstrapSource(manifest, settings); err == nil {
		t.Fatal("bootstrap bypassed epoch-one preparation")
	}
}

// A separate approved staging baseline is valid only under its explicit mode;
// zero margin does not convert an existing production policy into staging.
func TestBootstrapRequiresExactStagingModeAndZeroMargin(t *testing.T) {
	manifest := stagingSourceTestManifest(t)
	generation := manifest.EvaluationSource.Generations[0]
	settings := &controller.Settings{BaseSha: generation.Repositories.Server.Commit,
		AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Staging: true, StagingRoundWindowSeconds: 60,
			PolicyApprovalSha256: strings.Repeat("e", 64), SourceLockSha256: generation.SourceLockSha256, SourceActivationGeneration: 1},
	}
	if err := validateBootstrapSource(manifest, settings); err != nil {
		t.Fatal(err)
	}
	settings.AutomationPolicy.Staging = false
	if err := validateBootstrapSource(manifest, settings); err == nil {
		t.Fatal("staging bootstrap accepted production policy")
	}
	settings.AutomationPolicy.Staging = true
	settings.EvaluationPolicy.TakeoverMargin = 0.161
	if err := validateBootstrapSource(manifest, settings); err == nil {
		t.Fatal("staging bootstrap accepted a production threshold")
	}
}

// Bootstrap confirms already published immutable refs; it cannot use policy
// approval as permission to publish an unreviewed source change.
func TestBootstrapPublicationCannotChangeRefs(t *testing.T) {
	intent, backend := newSourcePublicationFixture()
	intent.Bootstrap = true
	intent.ApprovedPatchPath, intent.ApprovedPatchSha256 = "", ""
	for index := range intent.Refs {
		intent.Refs[index].Commit = intent.Refs[index].ExpectedCommit
	}
	if err := reconcileSourcePublication(context.Background(), intent, backend); err != nil {
		t.Fatal(err)
	}
	if len(backend.pushNames) != 0 {
		t.Fatal("baseline bootstrap pushed source or configuration")
	}
	intent.Refs[0].Commit = strings.Repeat("b", 40)
	if err := reconcileSourcePublication(context.Background(), intent, backend); err == nil {
		t.Fatal("bootstrap accepted an unpublished source change")
	}
}

// Starting supervision before bootstrap must not leave the approved baseline
// stuck behind a source-choice checkpoint that requires an already active pointer.
func TestBootstrapResumesEarlierSupervisorSourceChoice(t *testing.T) {
	preparedAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	intent := &model.CompetitionSourceActivation{Kind: "baseline", Generation: 1, TargetEpoch: 1, TargetRoundId: server.NewId(),
		PreparedAt: preparedAt, IntentSha256: strings.Repeat("a", 64), State: "prepared"}
	previous := &model.CompetitionSupervisorTransition{TargetEpoch: 1, Revision: 3, ExpectedRevision: 2, State: "source_choice_pending",
		SourceChoiceAt: preparedAt.Add(-time.Hour), UpdatedAt: preparedAt.Add(-time.Hour)}
	original := *previous
	next, record, err := bootstrapSourceTransition(intent, previous)
	if err != nil || !record || next.State != "source_publication_pending" || next.ExpectedRevision != previous.Revision ||
		next.SourceActivationGeneration != intent.Generation || !next.SourceChoiceAt.Equal(previous.SourceChoiceAt) ||
		!next.PreparationStartedAt.Equal(intent.PreparedAt) || next.RoundId != nil || !next.OpensAt.IsZero() {
		t.Fatalf("earlier supervisor checkpoint did not resume through ordinary publication/preparation: %+v, %v, %v", next, record, err)
	}
	if !reflect.DeepEqual(*previous, original) || intent.ActivatedAt != nil || intent.State != "prepared" {
		t.Fatal("checkpoint recovery mutated history or activated the baseline")
	}
	next.Revision = previous.Revision + 1
	replayed, record, err := bootstrapSourceTransition(intent, next)
	if err != nil || record || !reflect.DeepEqual(replayed, next) {
		t.Fatalf("checkpoint replay rewrote the selected baseline: %+v, %v, %v", replayed, record, err)
	}
	initial, record, err := bootstrapSourceTransition(intent, nil)
	if err != nil || !record || initial.ExpectedRevision != 0 || initial.State != "source_publication_pending" ||
		initial.SourceActivationGeneration != intent.Generation || !initial.PreparationStartedAt.Equal(intent.PreparedAt) {
		t.Fatalf("normal bootstrap no longer uses the same publication/preparation path: %+v, %v, %v", initial, record, err)
	}
}

// Recovery cannot attach a different epoch, source generation, reserved round,
// or an unpersisted baseline, nor move an already-started preparation backwards.
func TestBootstrapSourceChoiceRejectsConflictingIdentity(t *testing.T) {
	preparedAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	intent := model.CompetitionSourceActivation{Kind: "baseline", Generation: 1, TargetEpoch: 1, TargetRoundId: server.NewId(),
		PreparedAt: preparedAt, IntentSha256: strings.Repeat("a", 64), State: "prepared"}
	previous := model.CompetitionSupervisorTransition{TargetEpoch: 1, Revision: 1, State: "source_choice_pending", SourceChoiceAt: preparedAt.Add(-time.Hour)}
	for index, mutate := range []func(*model.CompetitionSupervisorTransition){
		func(value *model.CompetitionSupervisorTransition) { value.TargetEpoch = 2 },
		func(value *model.CompetitionSupervisorTransition) { value.SourceActivationGeneration = 2 },
		func(value *model.CompetitionSupervisorTransition) { id := server.NewId(); value.RoundId = &id },
		func(value *model.CompetitionSupervisorTransition) { value.State = "preparation_pending" },
		func(value *model.CompetitionSupervisorTransition) { value.PreparationStartedAt = preparedAt },
		func(value *model.CompetitionSupervisorTransition) { value.OpensAt = preparedAt.Add(16 * time.Hour) },
	} {
		changed := previous
		mutate(&changed)
		if _, _, err := bootstrapSourceTransition(&intent, &changed); err == nil {
			t.Fatalf("conflicting checkpoint %d was rewritten", index)
		}
	}
	for index, mutate := range []func(*model.CompetitionSourceActivation){
		func(value *model.CompetitionSourceActivation) { value.Kind = "reviewed_source_promotion" },
		func(value *model.CompetitionSourceActivation) { value.Generation = 2 },
		func(value *model.CompetitionSourceActivation) { value.ExpectedGeneration = 1 },
		func(value *model.CompetitionSourceActivation) { value.SourceGeneration = 1 },
		func(value *model.CompetitionSourceActivation) { value.TargetEpoch = 2 },
		func(value *model.CompetitionSourceActivation) { value.TargetRoundId = server.Id{} },
		func(value *model.CompetitionSourceActivation) { value.IntentSha256 = "" },
		func(value *model.CompetitionSourceActivation) { value.AbandonedAt = &preparedAt },
	} {
		changed := intent
		mutate(&changed)
		if _, _, err := bootstrapSourceTransition(&changed, &previous); err == nil {
			t.Fatalf("conflicting baseline %d was selected", index)
		}
	}
}

// Repeating bootstrap must not reset a selected, qualified, or completed
// checkpoint, including its immutable committed opening time and round identity.
func TestBootstrapPreservesAlreadySelectedCheckpoint(t *testing.T) {
	preparedAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	intent := &model.CompetitionSourceActivation{Kind: "baseline", Generation: 1, TargetEpoch: 1, TargetRoundId: server.NewId(),
		PreparedAt: preparedAt, IntentSha256: strings.Repeat("a", 64), State: "prepared"}
	for _, state := range []string{"source_publication_pending", "publication_blocked", "preparation_pending", "preparation_blocked", "preparation_ready", "source_active", "round_created"} {
		previous := &model.CompetitionSupervisorTransition{TargetEpoch: 1, Revision: 5, ExpectedRevision: 4, State: state,
			SourceActivationGeneration: 1, SourceChoiceAt: preparedAt, PreparationStartedAt: preparedAt}
		if state == "round_created" {
			previous.RoundId, previous.OpensAt = &intent.TargetRoundId, preparedAt.Add(16*time.Hour)
		}
		original := *previous
		next, record, err := bootstrapSourceTransition(intent, previous)
		if err != nil || record || !reflect.DeepEqual(*next, original) || !reflect.DeepEqual(*previous, original) {
			t.Fatalf("already selected %s checkpoint changed: %+v, %v, %v", state, next, record, err)
		}
	}
}
