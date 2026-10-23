package monitor

import (
	"context"
	"strings"
	"testing"
)

func runSubnetEvidence(t *testing.T, signal Signal, settings SignalSettings) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("evidence run failed: %v", err)
	}
	return alerts
}

// Identical complete history, verified committed artifacts and anchored
// payloads, and every required anchor: only the fixed semantic-replay gap.
func TestSubnetEvidenceHealthyHistoryHasOnlyTheSemanticGap(t *testing.T) {
	_, _, _, settings, source := newSubnetEvidenceFixture(t)
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	requireSubnetEvidenceOnlySemanticGap(t, alerts)
	gap := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-evidence/coverage", "semantic-replay")
	if gap.Severity != SeverityWarn || !strings.Contains(gap.Observed, "undecided_classes=subnet-audit-replay-gap") {
		t.Fatalf("semantic replay gap=%+v", gap)
	}
	if source.apiCalls == 0 {
		t.Fatal("history was not read")
	}
	requireSubnetReplayRedacted(t, alerts, "synthetic-prefix")
}

// A second run re-reads nothing already verified, keeping availability on
// the complete index.
func TestSubnetEvidenceVerifiedObjectsAreReadOnce(t *testing.T) {
	_, _, _, settings, source := newSubnetEvidenceFixture(t)
	signal := NewSubnetEvidenceSignal()
	runSubnetEvidence(t, signal, settings)
	first := source.apiCalls
	runSubnetEvidence(t, signal, settings)
	if second := source.apiCalls - first; second != 2 {
		t.Fatalf("second run made %d api reads, want the two index reads", second)
	}
}

// Origins that disagree warn; neither is authority.
func TestSubnetEvidenceOriginDivergenceWarns(t *testing.T) {
	_, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	journal.extraListed[1] = []string{"synthetic-prefix/st/v1/history/synthetic-replay-deployment/25/9/1/" + strings.Repeat("e7", 32) + ".json"}
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-history-availability", "subnet-evidence/history", "origin-divergence")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "origin-1-only=0 origin-2-only=1") {
		t.Fatalf("divergence=%+v", alert)
	}
}

// An out-of-order page is accepted index tampering.
func TestSubnetEvidenceOutOfOrderIndexPages(t *testing.T) {
	_, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	journal.disorderIndex[0] = true
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-artifact-integrity", "subnet-evidence/origin-1", "index/page-order-or-scope")
	if alert.Severity != SeverityPage {
		t.Fatalf("index tampering severity=%s", alert.Severity)
	}
}

// Bytes that fail the canonical rebuild at one origin page as tampering.
func TestSubnetEvidenceTamperedArtifactCopyPages(t *testing.T) {
	chain, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	hash := chain.roots[subnetSettlementKey{epoch: 2, noId: 2}].ArtifactHash
	journal.tamperedArtifacts[1][hash] = true
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-artifact-integrity", "subnet-evidence/origin-2", "epoch-2/no-2/artifact-bytes")
}

// Definitive absence at every origin pages for claimable evidence and only
// warns for an epoch whose claims cannot depend on it yet.
func TestSubnetEvidenceArtifactLostEverywhere(t *testing.T) {
	chain, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	finalized := chain.roots[subnetSettlementKey{epoch: 1, noId: 1}].ArtifactHash
	pending := chain.roots[subnetSettlementKey{epoch: 3, noId: 2}].ArtifactHash
	for origin := 0; origin < 2; origin++ {
		journal.missingArtifacts[origin][finalized] = true
		journal.missingArtifacts[origin][pending] = true
	}
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	page := requireSubnetReplayAlert(t, alerts, "subnet-history-availability", "subnet-evidence/no-1", "epoch-1/artifact-lost")
	warn := requireSubnetReplayAlert(t, alerts, "subnet-history-availability", "subnet-evidence/no-2", "epoch-3/artifact-lost")
	if page.Severity != SeverityPage || warn.Severity != SeverityWarn {
		t.Fatalf("loss severities finalized=%s pending=%s", page.Severity, warn.Severity)
	}
}

// One origin missing an object is a warning, never a loss.
func TestSubnetEvidenceOneOriginMissingWarns(t *testing.T) {
	chain, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	hash := chain.roots[subnetSettlementKey{epoch: 0, noId: 1}].ArtifactHash
	journal.missingArtifacts[0][hash] = true
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-history-availability", "subnet-evidence/origin-1", "committed-artifacts-unavailable")
	for _, alert := range alerts {
		if alert.Severity == SeverityPage {
			t.Fatalf("one missing copy paged: %+v", alert)
		}
	}
}

// A second verified artifact for one epoch and operator is equivocation.
func TestSubnetEvidenceEquivocatingArtifactPages(t *testing.T) {
	chain, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	committed := chain.roots[subnetSettlementKey{epoch: 2, noId: 1}]
	saved := *committed
	alternate := chain.commit(2, 1, chain.end(2)+8, [32]byte{0xa1})
	chain.roots[subnetSettlementKey{epoch: 2, noId: 1}] = &saved
	hash := strings.TrimPrefix(alternate.ContentHash, "sha256:")
	journal.extraListed[0] = []string{"synthetic-prefix/st/v1/history/synthetic-replay-deployment/25/2/1/" + hash + ".json"}
	journal.extraListed[1] = journal.extraListed[0]
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-artifact-integrity", "subnet-evidence/no-1", "epoch-2/equivocation")
	if alert.Severity != SeverityPage {
		t.Fatalf("equivocation severity=%s", alert.Severity)
	}
}

// A stored header that differs from its event is a rewritten anchor.
func TestSubnetEvidenceRewrittenAnchorPages(t *testing.T) {
	_, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	for _, anchor := range journal.anchors {
		if anchor.stored.Header.Epoch == 1 {
			anchor.stored.Header.PayloadHash = [32]byte{0xde, 0xad}
		}
	}
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-evidence-anchor", "subnet-evidence/no-1", "epoch-1/anchor-rewritten")
}

// A journal bound to another deployment makes every anchor foreign.
func TestSubnetEvidenceForeignJournalDomainPages(t *testing.T) {
	_, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	foreign := [32]byte{0xf0}
	journal.domainOverride = &foreign
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-evidence-anchor", "subnet-evidence/anchors", "journal-domain")
	if alert.Severity != SeverityPage {
		t.Fatalf("foreign journal severity=%s", alert.Severity)
	}
}

// An unfixed journal delays every required anchor: a warning, not unknown.
func TestSubnetEvidenceUnfixedJournalWarns(t *testing.T) {
	chain, journal, _, settings, source := newSubnetEvidenceFixture(t)
	node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, chain.finalizedBlock())
	chain.install(node)
	journal.install(node, false)
	source.node = node
	node.settings = settings.SubnetReplay
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-evidence-anchor", "subnet-evidence/anchors", "journal")
	if alert.Severity != SeverityWarn || alert.unobservable {
		t.Fatalf("unfixed journal=%+v", alert)
	}
}

// A missing census anchor is pending before its at-risk window, delayed
// inside it and paged after the enrolled deadline.
func TestSubnetEvidenceRequiredAnchorDeadlineBeforeAndAfter(t *testing.T) {
	for _, step := range []struct {
		deadline uint64
		severity Severity
		alerted  bool
	}{
		{deadline: 1000, alerted: false},
		{deadline: 50, severity: SeverityWarn, alerted: true},
		{deadline: 30, severity: SeverityPage, alerted: true},
	} {
		_, journal, _, settings, _ := newSubnetEvidenceFixture(t)
		for slot, anchor := range journal.anchors {
			if anchor.stored.Header.Epoch == 3 {
				delete(journal.anchors, slot)
			}
		}
		node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, journal.chain.finalizedBlock())
		journal.chain.install(node)
		logs := journal.logs[:0:0]
		for _, log := range journal.logs {
			if len(log.Topics) == 4 && log.Topics[0] == journal.chain.contracts.evidence.Events["EvidenceCommitted"].ID && log.Topics[3].Big().Uint64() == 3 {
				continue
			}
			logs = append(logs, log)
		}
		journal.logs = logs
		journal.install(node, true)
		settings.SubnetReplay.AnchorDeadlineBlocks = step.deadline
		settings.SubnetReplay.AnchorAtRiskBlocks = 20
		settings.Source.(*subnetReplayTestSource).node = node
		node.settings = settings.SubnetReplay
		alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
		found := []Alert{}
		for _, alert := range subnetReplayAlerts(alerts, "subnet-evidence-anchor") {
			if alert.Frame == "epoch-3/activation-1/census" {
				found = append(found, alert)
			}
		}
		if !step.alerted {
			if len(found) != 0 {
				t.Fatalf("deadline %d: a pending anchor alerted", step.deadline)
			}
			continue
		}
		if len(found) != 1 || found[0].Severity != step.severity {
			t.Fatalf("deadline %d: anchor alerts=%+v", step.deadline, found)
		}
	}
}

// Payload bytes that differ from the anchored digest page; bytes absent at
// every origin page as loss.
func TestSubnetEvidencePayloadTamperedAndLost(t *testing.T) {
	_, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	for _, anchor := range journal.anchors {
		switch anchor.stored.Header.Epoch {
		case 0:
			journal.tamperedPayloads[0][anchor.stored.Header.PayloadHash] = true
		case 2:
			journal.missingPayloads[0][anchor.stored.Header.PayloadHash] = true
			journal.missingPayloads[1][anchor.stored.Header.PayloadHash] = true
		}
	}
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-artifact-integrity", "subnet-evidence/origin-1", "epoch-0/no-1/payload-bytes")
	lost := requireSubnetReplayAlert(t, alerts, "subnet-history-availability", "subnet-evidence/no-1", "epoch-2/payload-lost")
	if lost.Severity != SeverityPage {
		t.Fatalf("payload loss severity=%s", lost.Severity)
	}
}

// An enrolled validator without an activation delays its anchors.
func TestSubnetEvidenceEnrolledValidatorWithoutActivationWarns(t *testing.T) {
	_, _, _, settings, _ := newSubnetEvidenceFixture(t)
	settings.SubnetReplay.Validators = []string{subnetReplayTestSs58(t, [32]byte{0x55})}
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-evidence-anchor", "subnet-evidence/anchors", "validator-1/activation")
	if alert.Severity != SeverityWarn {
		t.Fatalf("missing activation severity=%s", alert.Severity)
	}
}

// Past the audit service level, a definitively absent artifact is an audit gap.
func TestSubnetEvidenceAuditReplayGapNamesMissingRequirements(t *testing.T) {
	chain, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	settings.SubnetReplay.AuditReplaySloBlocks = 10
	hash := chain.roots[subnetSettlementKey{epoch: 1, noId: 1}].ArtifactHash
	journal.missingArtifacts[0][hash] = true
	journal.missingArtifacts[1][hash] = true
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-audit-replay-gap", "subnet-evidence/no-1", "epoch-1/audit")
	if !strings.Contains(alert.Observed, "missing=artifact") || alert.Severity != SeverityWarn {
		t.Fatalf("audit gap=%+v", alert)
	}
}

// Missing enrollment and sources are unknown, never green.
func TestSubnetEvidenceUnknownSourcesNeverGreen(t *testing.T) {
	_, _, _, settings, _ := newSubnetEvidenceFixture(t)
	settings.SubnetReplay.EvidenceFromBlock = 0
	alerts := runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-evidence/coverage", "evidence-journal")

	_, _, _, settings, _ = newSubnetEvidenceFixture(t)
	settings.SubnetReplay.ApiOrigins = nil
	alerts = runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	history := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-evidence/coverage", "history")
	if !strings.Contains(history.Observed, "reason=api-origins-not-enrolled") {
		t.Fatalf("history gap=%q", history.Observed)
	}

	_, _, _, settings, _ = newSubnetEvidenceFixture(t)
	settings.SubnetReplay.HistoryMaximumPages = 1
	settings.SubnetReplay.HistoryPageObjects = 2
	alerts = runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	bound := requireSubnetReplayAlert(t, alerts, "subnet-history-availability", "subnet-evidence/origin-1", "index")
	if !bound.unobservable || !strings.Contains(bound.Observed, "reason=index-page-bound") {
		t.Fatalf("page bound=%+v", bound)
	}

	_, _, node, settings, _ := newSubnetEvidenceFixture(t)
	node.failMethods["eth_getLogs"] = true
	alerts = runSubnetEvidence(t, NewSubnetEvidenceSignal(), settings)
	for _, alert := range alerts {
		if alert.Severity == SeverityPage {
			t.Fatalf("an unknown journal paged: %+v", alert)
		}
	}
	requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-evidence/coverage", "evidence-journal")
}

// Parent cancellation withholds every evidence finding.
func TestSubnetEvidenceCanceledRunWithholdsFindings(t *testing.T) {
	_, journal, node, settings, _ := newSubnetEvidenceFixture(t)
	journal.disorderIndex[0] = true
	ctx, cancel := context.WithCancel(context.Background())
	node.beforeExchange = func(exchange int) {
		if exchange == 4 {
			cancel()
		}
	}
	alerts, err := NewSubnetEvidenceSignal().Run(ctx, settings)
	if err == nil || len(alerts) != 0 {
		t.Fatalf("canceled evidence run returned alerts=%d err=%v", len(alerts), err)
	}
}

// A proved tampered copy stays open after the copy is repaired.
func TestSubnetEvidenceRetainsIntegrityCounterexample(t *testing.T) {
	chain, journal, _, settings, _ := newSubnetEvidenceFixture(t)
	signal := NewSubnetEvidenceSignal()
	hash := chain.roots[subnetSettlementKey{epoch: 2, noId: 2}].ArtifactHash
	journal.tamperedArtifacts[1][hash] = true
	runSubnetEvidence(t, signal, settings)
	journal.tamperedArtifacts[1][hash] = false
	alerts := runSubnetEvidence(t, signal, settings)
	retained := requireSubnetReplayAlert(t, alerts, "subnet-artifact-integrity", "subnet-evidence/origin-2", "epoch-2/no-2/artifact-bytes")
	if !strings.Contains(retained.Observed, "retained=true") {
		t.Fatalf("repaired copy erased the counterexample: %+v", retained)
	}
}

func TestSubnetEvidenceRegistrationMetadata(t *testing.T) {
	signal := NewSubnetEvidenceSignal()
	if signal.Number() != "22.8" || signal.Key() != "subnet-evidence" || signal.ID() != "subnet/evidence" || signal.Cadence() <= 0 {
		t.Fatalf("unexpected metadata: %s %s %s", signal.Number(), signal.Key(), signal.ID())
	}
}

// §22.10 acceptance fixtures for this family.
func init() {
	subnetFamilyAcceptanceCases["22.8"] = subnetFamilyAcceptanceCase{
		healthy: func(t *testing.T) (Signal, SignalSettings) {
			_, _, _, settings, _ := newSubnetEvidenceFixture(t)
			return NewSubnetEvidenceSignal(), settings
		},
		violated: func(t *testing.T) (Signal, SignalSettings, func()) {
			chain, journal, _, settings, _ := newSubnetEvidenceFixture(t)
			hash := chain.roots[subnetSettlementKey{epoch: 2, noId: 2}].ArtifactHash
			journal.tamperedArtifacts[1][hash] = true
			return NewSubnetEvidenceSignal(), settings, func() { journal.tamperedArtifacts[1][hash] = false }
		},
		violatedClass: "subnet-artifact-integrity",
		unknown: func(t *testing.T) (Signal, SignalSettings) {
			_, _, node, settings, _ := newSubnetEvidenceFixture(t)
			node.failMethods["eth_call"] = true
			return NewSubnetEvidenceSignal(), settings
		},
		canceled: func(t *testing.T) (Signal, SignalSettings, context.Context) {
			_, journal, node, settings, _ := newSubnetEvidenceFixture(t)
			journal.disorderIndex[0] = true
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			node.beforeExchange = func(exchange int) {
				if exchange == 4 {
					cancel()
				}
			}
			return NewSubnetEvidenceSignal(), settings, ctx
		},
		fixedGaps: []string{"semantic-replay"},
	}
}
