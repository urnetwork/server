package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// One provider's URL outcome at `at` whose IPv4 HTTPS leg failed before it
// wrote a request, at its DNS lookup through the provider, carrying e2e.
func sealedTurnTestOutcome(clientId server.Id, at time.Time, e2e *egresshealth.UrlProbeE2eEvidence) *ProviderEgressHealth {
	evidence := fp2TestUrlProbeEvidence(at, true)
	evidence.Security = nil
	evidence.ContentClassification, evidence.PerformanceClassification, evidence.FailureStage = "", "", "dial_dns"
	evidence.StatusCode, evidence.ByteCount, evidence.WireByteCount, evidence.WireSampleByteCount = 0, 0, 0, 0
	evidence.BodyComplete, evidence.RequestWritten, evidence.FirstByteReceived = false, false, false
	evidence.TtfbMillis, evidence.BodyMillis, evidence.BodyBitsPerSecond = 0, 0, 0
	evidence.E2e = e2e
	return &ProviderEgressHealth{
		RunId: server.NewId(), ClientId: clientId, MeasuredAt: at, OKCount: 0, Total: 1, UrlProbeEvidence: evidence,
		ClassResults: map[string]ProviderEgressHealthClassResult{string(evidence.Destination.Class): {OK: 0, Total: 1}},
		FailedNames:  evidence.Destination.Name,
	}
}

// The ingest records a sealed turn without a request as post-quantum evidence
// for a provider whose published country the guard the score export last
// published exempts, and nothing for the same turn in a country it measured
// and does not exempt. In the exempt country an unsealed turn, and a sealed
// turn whose leg wrote its request, record nothing either. Every turn stores
// pq_outcome 0, so strict selection's eight-hour window is unchanged (owner,
// 2026-11-24).
func TestProviderEgressSealedTurnRecordsPostQuantumOnlyInAnExemptCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		exemptCity := egressTestCity(ctx, "Novosibirsk", "Novosibirsk Oblast", "Russia", "ru")
		measuredCity := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")
		exempt := egressTestConnect(ctx, t, exemptCity, egressTestFast, nil, nil)
		notExempt := egressTestConnect(ctx, t, measuredCity, egressTestFast, nil, nil)
		unsealed := egressTestConnect(ctx, t, exemptCity, egressTestFast, nil, nil)
		written := egressTestConnect(ctx, t, exemptCity, egressTestFast, nil, nil)
		// the rollup publishes each provider's country
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		connect.AssertEqual(t, publishProviderOnlineSuccessBaseline(ctx, &providerOnlineSuccessBaseline{
			computeTime: now,
			enabled:     true,
			floor:       0.36,
			countryCodeCounts: map[string]providerOnlineSuccessCount{
				"ru": {Measured: 5, Succeeded: 1, NoSuccess: 4, Exempt: true},
				"us": {Measured: 10, Succeeded: 9, NoSuccess: 1},
			},
		}, time.Hour), nil)

		at := server.NowUtc().Truncate(time.Microsecond).Add(-time.Minute)
		sealed := egresshealth.UrlProbeE2eEvidence{
			Pq: egresshealth.UrlProbePqUnmeasured, KeyExchange: "X25519MLKEM768", Window: egresshealth.UrlProbeWindowResizable, Sealed: true,
		}
		notSealed := sealed
		notSealed.Sealed = false
		exemptTurn := sealedTurnTestOutcome(exempt.clientId, at, &sealed)
		notExemptTurn := sealedTurnTestOutcome(notExempt.clientId, at, &sealed)
		unsealedTurn := sealedTurnTestOutcome(unsealed.clientId, at, &notSealed)
		// the same seal on an outcome whose leg wrote its request and failed
		// on the response
		writtenTurn := &ProviderEgressHealth{
			RunId: server.NewId(), ClientId: written.clientId, MeasuredAt: at, OKCount: 0, Total: 1,
			UrlProbeEvidence: fp2TestUrlProbeEvidence(at, false),
			ClassResults:     map[string]ProviderEgressHealthClassResult{string(egresshealth.ClassSite): {OK: 0, Total: 1}},
			FailedNames:      "synthetic-document",
		}
		writtenTurn.UrlProbeEvidence.E2e = &sealed
		turns := []*ProviderEgressHealth{exemptTurn, notExemptTurn, unsealedTurn, writtenTurn}
		for _, turn := range turns {
			if stored := StoreProviderEgressHealth(ctx, turn); !stored.Stored {
				t.Fatalf("the turn of %s was not stored", turn.ClientId)
			}
		}

		records := getProviderModernEvidenceRecords(ctx, []server.Id{exempt.clientId, notExempt.clientId, unsealed.clientId, written.clientId})
		record := records[exempt.clientId]
		if record == nil || !record.PostQuantum || !record.WindowResizable || record.KeyExchange != "X25519MLKEM768" || !record.MeasuredAt.Equal(at) {
			t.Fatalf("a sealed turn in an exempt country recorded %+v", record)
		}
		connect.AssertEqual(t, classifyProviderModernEvidence(record, server.NowUtc()), ProviderModernOutcomeModern)
		for name, clientId := range map[string]server.Id{
			"a sealed turn in a country the guard does not exempt": notExempt.clientId,
			"an unsealed turn in an exempt country":                unsealed.clientId,
			"a sealed turn that wrote its request":                 written.clientId,
		} {
			if record, ok := records[clientId]; ok {
				t.Fatalf("%s recorded %+v", name, record)
			}
		}
		// strict selection's window reads pq_outcome, which none of these
		// moves
		for _, turn := range turns {
			_, pqOutcome := fp2EvidenceTestStored(t, turn.RunId)
			connect.AssertEqual(t, pqOutcome, egresshealth.UrlProbePqOutcomeUnmeasured)
		}
	})
}
