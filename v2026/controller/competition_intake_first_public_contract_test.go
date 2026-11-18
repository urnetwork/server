// The actual companion OpenAPI contract covers the dedicated human handoff
// without changing submitter request or immutable round payloads.
package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Typed production requests are decoded as JSON before schema validation;
// the schema library deliberately does not treat Go structs as JSON objects.
func TestCompetitionIntakeFirstPublicHandoffContract(t *testing.T) {
	validateRequest := coveragePublicSchemaValidator(t, "CompetitionSourceOperatorHandoffArgs")
	validateReceipt := coveragePublicSchemaValidator(t, "CompetitionSourceOperatorHandoff")
	for _, disposition := range []string{"approve_promotion", "reject_promotion", "approve_no_winner_carry"} {
		request := model.CompetitionSourceOperatorHandoffArgs{TargetEpoch: 2, ExpectedRevision: 0,
			RewardDecisionSha256: strings.Repeat("a", 64), ProductSourceLockSha256: strings.Repeat("b", 64),
			Disposition: disposition, IdempotencyKey: "synthetic-handoff", EvidenceSha256: strings.Repeat("c", 64)}
		if disposition != "approve_no_winner_carry" {
			jobId, reviewId := server.NewId(), server.NewId()
			request.SelectedJobId, request.ReviewId = &jobId, &reviewId
			request.PatchSha256, request.ReviewDecisionSha256 = strings.Repeat("d", 64), strings.Repeat("e", 64)
		}
		if err := model.ValidateCompetitionSourceOperatorHandoffArgs(request); err != nil {
			t.Fatal("positive fixture is not a production request", err)
		}
		content, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var instance map[string]any
		if err := json.Unmarshal(content, &instance); err != nil {
			t.Fatal(err)
		}
		if err := validateRequest(instance); err != nil {
			t.Fatal("actual handoff request rejected", disposition, err)
		}
		for _, change := range []func(map[string]any){
			func(value map[string]any) { delete(value, "reward_decision_sha256") },
			func(value map[string]any) { value["expected_revision"] = -1 },
			func(value map[string]any) { value["target_epoch"] = 1 },
			func(value map[string]any) { value["disposition"] = "approve" },
			func(value map[string]any) { value["ready"] = true },
			func(value map[string]any) { value["idempotency_key"] = "invalid key" },
			func(value map[string]any) {
				if disposition == "approve_no_winner_carry" {
					value["selected_job_id"] = nil
				} else {
					delete(value, "review_id")
				}
			},
		} {
			var changed map[string]any
			if err := json.Unmarshal(content, &changed); err != nil {
				t.Fatal(err)
			}
			change(changed)
			if validateRequest(changed) == nil {
				t.Fatal("incomplete or caller-invented handoff accepted", disposition, changed)
			}
		}
		digest, err := model.CompetitionAutomationDigest(request)
		if err != nil {
			t.Fatal(err)
		}
		receipt := model.CompetitionSourceOperatorHandoff{Schema: 1, Kind: model.CompetitionSourceOperatorHandoffKind,
			Environment: "synthetic", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season", Revision: 1,
			ActorId: "synthetic-human", Request: request, RequestSha256: digest, CreatedAt: time.Unix(1000, 0).UTC()}
		content, err = json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		if err := json.Unmarshal(content, &response); err != nil {
			t.Fatal(err)
		}
		if err := validateReceipt(response); err != nil {
			t.Fatal("actual handoff receipt rejected", disposition, err)
		}
		response["worker_ready"] = true
		if validateReceipt(response) == nil {
			t.Fatal("handoff receipt manufactured runtime qualification")
		}
	}
}
