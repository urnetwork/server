// Root control output keeps the historical public boundary even when its
// immutable database audit additionally retains private native inventories.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// The actual output writer must redact, not merely rely on a caller omitting
// its private field or a schema that cannot encode the new stored proof.
func TestDistributedRecoveryReceiptStdoutOmitsPrivateNativeProof(t *testing.T) {
	privateProof := json.RawMessage(`{"files":[{"path":"synthetic-private-input/hidden-request.json","signature":"synthetic-private-signature"}]}`)
	receipt := &model.CompetitionJobReevaluationReceipt{CompetitionJobReevaluationCycle: model.CompetitionJobReevaluationCycle{Schema: 2,
		Distributed: &model.CompetitionDistributedJobReevaluationCycle{NativeProofSha256: strings.Repeat("a", 64)}}, DistributedProof: privateProof,
		RequestSha256: strings.Repeat("b", 64), AttemptOffset: 1}
	var output bytes.Buffer
	if err := writeRecoveryReceipt(&output, receipt); err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"distributed_proof", "synthetic-private-input", "synthetic-private-signature"} {
		if strings.Contains(output.String(), hidden) {
			t.Fatal("private native inventory reached standard output")
		}
	}
	if !bytes.Equal(receipt.DistributedProof, privateProof) || !strings.Contains(output.String(), strings.Repeat("a", 64)) || !strings.Contains(output.String(), strings.Repeat("b", 64)) {
		t.Fatal("stdout redaction changed the private audit or omitted public receipt provenance")
	}
}
