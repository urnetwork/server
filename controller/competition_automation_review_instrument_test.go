// Pure synthetic provenance checks require no database, provider or runtime.
package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// The selected product parent and activated instrument child are intentionally
// different. The private packet preserves both without changing public policy.
func reviewInstrumentFixture(t *testing.T) *CompetitionSourceReviewPacket {
	t.Helper()
	_, _, intent := trustedInstrumentAuthorizationFixture(t)
	return &CompetitionSourceReviewPacket{BaseSha: intent.Commits["server"], SourceCommits: intent.Commits,
		EvaluatedImageDigest: intent.EvaluatorImageDigest, Entry: model.CompetitionSealedEntry{BaseSourceLockSha256: intent.SourceLockSha256},
		Instrument: &CompetitionReviewInstrument{Schema: 1, ServerCommit: intent.Commits["server"], ImageDigest: intent.EvaluatorImageDigest,
			SimulatorSha256: intent.SimulatorSha256, ScorerSha256: intent.ScorerSha256, TrustedInstrument: intent.TrustedInstrument,
			EvaluationPolicy: model.CompetitionEvaluationPolicy{G2VolumePolicy: intent.G2VolumePolicy, SimulatorSha256: intent.SimulatorSha256, ScorerSha256: intent.ScorerSha256}}}
}

// Image, compiled binaries and policy must describe the instrument child, not
// the unchanged product graph selected before its separately approved overlay.
func TestCompetitionReviewInstrumentBindsActivatedOverlay(t *testing.T) {
	packet := reviewInstrumentFixture(t)
	if err := ValidateCompetitionReviewInstrument(packet); err != nil {
		t.Fatal("complete synthetic instrument provenance refused", err)
	}
	if packet.Instrument.ServerCommit == packet.Instrument.TrustedInstrument.SelectedProductCommits["server"] {
		t.Fatal("fixture failed to distinguish product parent from instrument child")
	}
	for _, mutate := range []func(*CompetitionSourceReviewPacket){
		func(p *CompetitionSourceReviewPacket) { p.Instrument = nil },
		func(p *CompetitionSourceReviewPacket) {
			p.Instrument.ServerCommit = p.Instrument.TrustedInstrument.SelectedProductCommits["server"]
		},
		func(p *CompetitionSourceReviewPacket) { p.Instrument.ImageDigest = "sha256:" + strings.Repeat("9", 64) },
		func(p *CompetitionSourceReviewPacket) { p.Instrument.SimulatorSha256 = strings.Repeat("9", 64) },
		func(p *CompetitionSourceReviewPacket) {
			p.Instrument.EvaluationPolicy.ScorerSha256 = strings.Repeat("9", 64)
		},
		func(p *CompetitionSourceReviewPacket) {
			p.Instrument.TrustedInstrument.BuildReceipt.EvaluatorImageDigest = "sha256:" + strings.Repeat("9", 64)
		},
		func(p *CompetitionSourceReviewPacket) {
			p.Instrument.TrustedInstrument.InstrumentedServerCommit = strings.Repeat("9", 40)
		},
		func(p *CompetitionSourceReviewPacket) {
			p.Instrument.TrustedInstrument.SelectedProductSourceLockSha256 = strings.Repeat("9", 64)
		},
		func(p *CompetitionSourceReviewPacket) {
			p.Instrument.TrustedInstrument.Approval.ApprovalSha256 = strings.Repeat("9", 64)
		},
		func(p *CompetitionSourceReviewPacket) { p.SourceCommits["connect"] = strings.Repeat("9", 40) },
	} {
		changed := reviewInstrumentFixture(t)
		mutate(changed)
		if ValidateCompetitionReviewInstrument(changed) == nil {
			t.Fatal("substituted instrument provenance accepted")
		}
	}
}

// Old serialized packets keep their original bytes/schema. They can be read,
// but an absent new provenance envelope is not silently upgraded into evidence.
func TestCompetitionReviewInstrumentLegacyEncodingDoesNotGrantAuthority(t *testing.T) {
	legacy := CompetitionSourceReviewPacket{BaseSha: strings.Repeat("a", 40)}
	content, err := json.Marshal(legacy)
	if err != nil || strings.Contains(string(content), `"instrument"`) {
		t.Fatal("optional private context rewrote historical packet shape", err)
	}
	if ValidateCompetitionReviewInstrument(&legacy) == nil {
		t.Fatal("historical packet lacking instrument identity authorized a new review")
	}
}

// A legacy scoring policy does not exempt a newly requested review from binary
// provenance. Matching empty duplicates must not bypass the actual format check.
func TestCompetitionReviewInstrumentRequiresLegacyBinaryPins(t *testing.T) {
	for _, staging := range []bool{false, true} {
		for _, fixture := range []struct {
			simulator string
			scorer    string
			valid     bool
		}{
			{simulator: strings.Repeat("7", 64), scorer: strings.Repeat("8", 64), valid: true},
			{},
			{simulator: strings.Repeat("7", 64)},
			{scorer: strings.Repeat("8", 64)},
			{simulator: strings.Repeat("7", 63), scorer: strings.Repeat("8", 64)},
			{simulator: strings.Repeat("7", 64), scorer: strings.Repeat("G", 64)},
		} {
			packet := &CompetitionSourceReviewPacket{BaseSha: strings.Repeat("a", 40), EvaluatedImageDigest: "sha256:" + strings.Repeat("9", 64),
				Seal: model.CompetitionEvaluationSeal{Staging: staging}}
			packet.Instrument = &CompetitionReviewInstrument{Schema: 1, ServerCommit: packet.BaseSha, ImageDigest: packet.EvaluatedImageDigest,
				SimulatorSha256: fixture.simulator, ScorerSha256: fixture.scorer,
				EvaluationPolicy: model.CompetitionEvaluationPolicy{G2VolumePolicy: model.CompetitionG2VolumePolicyLegacy, SimulatorSha256: fixture.simulator, ScorerSha256: fixture.scorer}}
			err := ValidateCompetitionReviewInstrument(packet)
			if (err == nil) != fixture.valid || err != nil && !strings.Contains(err.Error(), "compiled binary identities") {
				t.Fatalf("staging=%t simulator=%q scorer=%q valid=%t: %v", staging, fixture.simulator, fixture.scorer, fixture.valid, err)
			}
		}
	}
}
