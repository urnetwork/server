package controller

import (
	"strings"
	"testing"
)

func TestEpochThreeGoofeAdmissionRequestRejectsMalformedInput(t *testing.T) {
	for _, input := range []string{
		`{"unexpected":true}`,
		`{}{}`,
		`{"lease":{},"approval":{},"approval_sha256":"","receipt":{}}`,
	} {
		if err := RecordCompetitionExecutorAdmissionRequest(t.Context(), nil, strings.NewReader(input)); err == nil {
			t.Fatalf("accepted malformed input %s", input)
		}
	}
}

func TestEpochThreeGoofeAdmissionReceiptBindsOriginalAttempt(t *testing.T) {
	receipt := competitionExecutorEquivalenceReceipt{
		RoundId:               "01a1f4d0-47c9-ac2d-ce45-55ef159a9b41",
		HostId:                "goofe",
		QualificationJobId:    "0196802b-b5d9-3dfe-a52a-b9eb97dd118b",
		BaselineSha256:        "3b6c73d5cb2fd3c370ed5e79990184500fc9bef9358c36ffb2062d746212bc02",
		ArchiveManifestSha256: "d8925103a48981c2a6d3e34c18963fc6886393042270e65b945ecabfb21b5486",
		Retention: &retainedArtifact{
			Sha256:    "f48570db8860e5fcfac81eae5fdf04128e15fba4f94b24c051693c0ba56e3d3f",
			VersionId: "7b18931c-57c4-4cc3-989d-1256c0b8e04d",
		},
	}
	if !isEpochThreeGoofeAdmissionReceipt(&receipt) {
		t.Fatal("original qualification did not match")
	}
	for name, mutate := range map[string]func(*competitionExecutorEquivalenceReceipt){
		"round":    func(r *competitionExecutorEquivalenceReceipt) { r.RoundId = "" },
		"host":     func(r *competitionExecutorEquivalenceReceipt) { r.HostId = "sille" },
		"job":      func(r *competitionExecutorEquivalenceReceipt) { r.QualificationJobId = "" },
		"baseline": func(r *competitionExecutorEquivalenceReceipt) { r.BaselineSha256 = "" },
		"archive":  func(r *competitionExecutorEquivalenceReceipt) { r.ArchiveManifestSha256 = "" },
		"receipt":  func(r *competitionExecutorEquivalenceReceipt) { r.Retention = nil },
		"version": func(r *competitionExecutorEquivalenceReceipt) {
			retention := *r.Retention
			retention.VersionId = "other-version"
			r.Retention = &retention
		},
		"digest": func(r *competitionExecutorEquivalenceReceipt) {
			retention := *r.Retention
			retention.Sha256 = strings.Repeat("0", 64)
			r.Retention = &retention
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := receipt
			mutate(&changed)
			if isEpochThreeGoofeAdmissionReceipt(&changed) {
				t.Fatal("different qualification matched")
			}
		})
	}
}
