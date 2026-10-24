// Moving main releases bind genuine identities per preparation, never by
// weakening the exact-source receipt, quorum or explicit deployment restriction.
package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

func TestCompetitionPreparationRuntimeIdentityUsesDurableApiWithoutSeasonPin(t *testing.T) {
	adapter, request, state, _ := newCompetitionPreparationAdapterFixture(t)
	adapter.config.ApiImageDigest = ""
	if err := validateCompetitionPreparationConfig(adapter.settings, adapter.config); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.run(context.Background(), request); err != nil {
		t.Fatal("unpinned main API could not prepare from genuine role evidence", err)
	}
	state.stateLock.Lock()
	defer state.stateLock.Unlock()
	if len(state.receipts) != 2 || state.receipts[0].RuntimeImageDigest != testApiImageDigest() || state.receipts[1].RuntimeImageDigest != testWorkerImageDigest() {
		t.Fatal("actual release identities were not retained independently", state.receipts)
	}
	if adapter.config.ApiImageDigest != "" {
		t.Fatal("one intent froze the deployment-wide API setting")
	}
}

func TestCompetitionPreparationRuntimeIdentityRejectsUnpersistedHttpAcknowledgment(t *testing.T) {
	adapter, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	adapter.config.ApiImageDigest = ""
	adapter.receipts = func(context.Context, *Settings, int64) ([]model.CompetitionSourceReadiness, error) {
		return nil, nil
	}
	commands := 0
	adapter.runProgram = func(context.Context, string, string, any, []string) error {
		commands++
		return nil
	}
	if _, err := adapter.run(context.Background(), request); err == nil || commands != 0 {
		t.Fatalf("HTTP-only acknowledgment reached qualification: err=%v commands=%d", err, commands)
	}
}

func TestCompetitionPreparationRuntimeIdentityReplaysOriginalAckWithoutContactingMovingMain(t *testing.T) {
	adapter, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	adapter.config.ApiImageDigest = ""
	first, err := adapter.run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	transport := &competitionPreparationHttpTripwire{}
	adapter.client = &http.Client{Transport: transport}
	replayed, err := adapter.run(context.Background(), request)
	if err != nil || *replayed != *first || transport.calls != 0 {
		t.Fatalf("immutable per-intent acknowledgment was replaced: err=%v calls=%d", err, transport.calls)
	}
}

func TestCompetitionPreparationRuntimeIdentityRetainsExplicitConfigRestrictions(t *testing.T) {
	adapter, _, _, _ := newCompetitionPreparationAdapterFixture(t)
	for _, invalid := range []string{"main", "sha256:abc", "sha256:" + strings.Repeat("G", 64)} {
		config := adapter.config
		config.ApiImageDigest = invalid
		if err := validateCompetitionPreparationConfig(adapter.settings, config); err == nil {
			t.Fatal("malformed API release restriction was accepted", invalid)
		}
	}
	config := adapter.config
	config.ApiImageDigest, config.WorkerImageDigest = "", ""
	if err := validateCompetitionPreparationConfig(adapter.settings, config); err == nil {
		t.Fatal("optional API restriction also disabled independent worker identity")
	}
}

func TestCompetitionPreparationRuntimeIdentityRejectsMidPreparationSubstitution(t *testing.T) {
	adapter, request, state, _ := newCompetitionPreparationAdapterFixture(t)
	adapter.config.ApiImageDigest = ""
	run := adapter.runProgram
	adapter.runProgram = func(ctx context.Context, path, digest string, value any, arguments []string) error {
		if err := run(ctx, path, digest, value, arguments); err != nil {
			return err
		}
		if path == adapter.config.WorkerCommand {
			state.stateLock.Lock()
			state.receipts[0].RuntimeImageDigest = "sha256:" + strings.Repeat("e", 64)
			state.stateLock.Unlock()
		}
		return nil
	}
	if _, err := adapter.run(context.Background(), request); err == nil {
		t.Fatal("unpinned deployment permitted an API identity change within one preparation")
	}
}

func TestCompetitionPreparationRuntimeIdentityRejectsHttpAndDurableDisagreement(t *testing.T) {
	adapter, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	adapter.config.ApiImageDigest = ""
	read := adapter.receipts
	adapter.receipts = func(ctx context.Context, settings *Settings, generation int64) ([]model.CompetitionSourceReadiness, error) {
		rows, err := read(ctx, settings, generation)
		for index := range rows {
			if rows[index].Role == "api" {
				rows[index].RuntimeImageDigest = "sha256:" + strings.Repeat("e", 64)
			}
		}
		return rows, err
	}
	commands := 0
	adapter.runProgram = func(context.Context, string, string, any, []string) error {
		commands++
		return nil
	}
	if _, err := adapter.run(context.Background(), request); err == nil || commands != 0 {
		t.Fatalf("contradictory API identities reached qualification: err=%v commands=%d", err, commands)
	}
}

func TestCompetitionPreparationRuntimeIdentityRejectsSubstitutionImmediatelyAfterCachedAck(t *testing.T) {
	adapter, request, state, _ := newCompetitionPreparationAdapterFixture(t)
	adapter.config.ApiImageDigest = ""
	state.receipts = []model.CompetitionSourceReadiness{
		competitionPreparationTestReceipt(&request.Intent, "api", "synthetic-api", testApiImageDigest()),
	}
	read := adapter.receipts
	reads := 0
	adapter.receipts = func(ctx context.Context, settings *Settings, generation int64) ([]model.CompetitionSourceReadiness, error) {
		rows, err := read(ctx, settings, generation)
		reads++
		if reads > 1 {
			for index := range rows {
				if rows[index].Role == "api" {
					rows[index].RuntimeImageDigest = "sha256:" + strings.Repeat("e", 64)
				}
			}
		}
		return rows, err
	}
	if _, err := adapter.run(context.Background(), request); err == nil {
		t.Fatal("post-ack read changed the release bound to this preparation")
	}
}

func TestCompetitionPreparationRuntimeIdentityAcceptsNewReleaseOnlyForItsOwnIntent(t *testing.T) {
	_, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	first := request.Intent
	second := first
	second.Generation++
	old := competitionPreparationTestReceipt(&first, "api", "synthetic-api", testApiImageDigest())
	nextImage := "sha256:" + strings.Repeat("f", 64)
	next := competitionPreparationTestReceipt(&second, "api", "synthetic-api", nextImage)
	for _, sample := range []struct {
		intent *model.CompetitionSourceActivation
		row    model.CompetitionSourceReadiness
		image  string
	}{{intent: &first, row: old, image: old.RuntimeImageDigest}, {intent: &second, row: next, image: nextImage}} {
		actual, err := competitionSourceApiRuntimeIdentity(sample.intent, "", []model.CompetitionSourceReadiness{sample.row})
		if err != nil || actual != sample.image {
			t.Fatal("distinct main release could not bind its own intent", actual, err)
		}
	}
	if _, err := competitionSourceApiRuntimeIdentity(&second, "", []model.CompetitionSourceReadiness{old}); err == nil {
		t.Fatal("new intent reused an old release's source acknowledgment")
	}
	if _, err := competitionSourceApiRuntimeIdentity(&second, testApiImageDigest(), []model.CompetitionSourceReadiness{next}); err == nil {
		t.Fatal("explicit deployment restriction was ignored")
	}
}

func TestCompetitionPreparationRuntimeIdentityRequiresExactlyOneAuthenticApiRow(t *testing.T) {
	_, request, _, _ := newCompetitionPreparationAdapterFixture(t)
	valid := competitionPreparationTestReceipt(&request.Intent, "api", "synthetic-api", testApiImageDigest())
	different := func(digest string) string {
		if strings.HasPrefix(digest, "0") {
			return "1" + digest[1:]
		}
		return "0" + digest[1:]
	}
	for _, mutate := range []func(*model.CompetitionSourceReadiness){
		func(row *model.CompetitionSourceReadiness) { row.Ready = false },
		func(row *model.CompetitionSourceReadiness) { row.Staging = !row.Staging },
		func(row *model.CompetitionSourceReadiness) { row.RuntimeImageDigest = "" },
		func(row *model.CompetitionSourceReadiness) { row.SourceLockSha256 = different(row.SourceLockSha256) },
		func(row *model.CompetitionSourceReadiness) {
			row.QualificationSha256 = different(row.QualificationSha256)
		},
		func(row *model.CompetitionSourceReadiness) { row.ActorId = "" },
	} {
		row := valid
		mutate(&row)
		if _, err := competitionSourceApiRuntimeIdentity(&request.Intent, "", []model.CompetitionSourceReadiness{row}); err == nil {
			t.Fatal("invalid source identity became acceptable without a season API pin", row)
		}
	}
	for _, rows := range [][]model.CompetitionSourceReadiness{nil, {valid, valid}} {
		if _, err := competitionSourceApiRuntimeIdentity(&request.Intent, "", rows); err == nil {
			t.Fatal("missing or duplicate API evidence was accepted")
		}
	}
}
