// Strict worker launch identity is checked before any database, archive, or
// candidate execution; transactional claim/fence guards are tested separately.
package controller

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// No omitted mode, extra field, oversized body or second JSON value can cause
// the supervised worker to fall through to the legacy latest-round behavior.
func TestCompetitionSupervisorWorkerRequestRequiresExactBoundedIdentity(t *testing.T) {
	request := CompetitionSupervisorWorkerRequest{Operation: "run_one_epoch", Round: model.CompetitionRoundResult{RoundId: server.NewId(), Epoch: 1, DecisionMode: model.CompetitionDecisionModeV2}, Lease: model.CompetitionSupervisorLease{Environment: "synthetic", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season", OwnerId: "owner", FencingToken: 1}}
	content, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCompetitionSupervisorWorkerRequest(bytes.NewReader(content))
	if err != nil || decoded.Round.RoundId != request.Round.RoundId {
		t.Fatalf("valid exact worker identity rejected: %v", err)
	}
	invalid := [][]byte{append(append([]byte{}, content...), []byte(" {}")...), append([]byte(`{"unexpected":true,`), content[1:]...), []byte(strings.Repeat(" ", 1024*1024+1))}
	for _, mutate := range []func(*CompetitionSupervisorWorkerRequest){
		func(value *CompetitionSupervisorWorkerRequest) { value.Operation = "run_latest" },
		func(value *CompetitionSupervisorWorkerRequest) { value.Round.RoundId = server.Id{} },
		func(value *CompetitionSupervisorWorkerRequest) { value.Round.Epoch = 7 },
		func(value *CompetitionSupervisorWorkerRequest) { value.Round.DecisionMode = "" },
		func(value *CompetitionSupervisorWorkerRequest) { value.Lease.OwnerId = "" },
		func(value *CompetitionSupervisorWorkerRequest) { value.Lease.FencingToken = 0 },
		func(value *CompetitionSupervisorWorkerRequest) { value.Lease.SeasonId = "" },
	} {
		value := request
		mutate(&value)
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		invalid = append(invalid, encoded)
	}
	for index, content := range invalid {
		if _, err := DecodeCompetitionSupervisorWorkerRequest(bytes.NewReader(content)); err == nil {
			t.Fatalf("invalid worker launch %d accepted", index)
		}
	}
}
