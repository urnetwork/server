// Historical receipts retain their exact optional-leaf omission semantics.
package controller

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestExecutorQualificationRecoveryLegacyReceiptRemainsValid(t *testing.T) {
	f := newCompetitionNoopScreenFixture(t)
	receipt, err := screenCompetitionExecutorNoop(f.settings, f.job, f.host, f.directory, f.response, f.outcome)
	if err != nil {
		t.Fatal(err)
	}
	sealCompetitionNoopScreenReceipt(t, f.settings, receipt)
	if err := validateCompetitionExecutorReceipt(f.settings, &f.job.Round, f.job.RoundBaseline, f.host, receipt); err != nil {
		t.Fatal("historical admission changed", err)
	}
	content, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("recovery_sha256")) {
		t.Fatal("historical receipt digest gained a new leaf")
	}
}
