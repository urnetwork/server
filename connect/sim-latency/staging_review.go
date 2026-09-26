package main

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// Staging reviews are explicit and retrospective. They make the public
// staging winner usable by Apex's approved-winner path, but never promote
// source or change the already-finalized ranking.
func runStagingReview(opts docopt.Opts) {
	epoch, err := strconv.Atoi(optString(opts, "--epoch", ""))
	if err != nil || epoch < 0 {
		fatalf("staging-review epoch must be a nonnegative integer")
	}
	settings, store, err := loadCompetitionControl()
	if err != nil {
		fatalf("load competition control plane: %s", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	action := "next"
	var state *controller.CandidateReviewState
	if optBool(opts, "approve") {
		action = "approve"
		jobId, parseErr := server.ParseId(optString(opts, "--job-id", ""))
		if parseErr != nil {
			fatalf("staging winner job id: %s", parseErr)
		}
		evidence, evidenceSha256, evidenceErr := readHonestyEvidence(optString(opts, "--evidence", ""))
		if evidenceErr != nil {
			fatalf("staging honesty evidence: %s", evidenceErr)
		}
		state, err = store.ApproveStagingWinner(ctx, settings, epoch, controller.CandidateReviewDecision{
			JobId: jobId, Decision: "approved",
			ReviewerId: optString(opts, "--reviewer", ""),
			Reason:     optString(opts, "--reason", ""),
			Evidence:   evidence, EvidenceSha256: evidenceSha256,
		})
	} else {
		state, err = store.PrepareStagingWinnerReview(ctx, settings, epoch)
	}
	if err != nil {
		fatalf("staging winner review: %s", err)
	}
	result := epochReviewResult{Schema: 1, Action: action, State: state}
	if reviewActionMaterializesCandidate(action, state) {
		result.CandidateDirectory, err = materializeReviewCandidate(state, optString(opts, "--out-dir", ""))
		if err != nil {
			fatalf("materialize staging winner: %s", err)
		}
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatalf("encode staging review: %s", err)
	}
	fmt.Printf("%s\n", encoded)
}
