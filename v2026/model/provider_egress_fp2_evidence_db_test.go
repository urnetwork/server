package model

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// A dual-stack outcome whose failed later legs carry verdict, as the ingest's
// judgement sets them, with e2e when not nil.
func fp2EvidenceTestOutcome(clientId server.Id, at time.Time, h3 string, tcp string, udp6 string, verdict string, e2e *egresshealth.UrlProbeE2eEvidence) *ProviderEgressHealth {
	health := testingIpv6UrlResult(clientId, testingTlsSite("fp2-site", "https://fp2.example/"), at, h3, tcp, udp6)
	evidence := health.UrlProbeEvidence
	if leg := evidence.H3; leg != nil && leg.Udp == egresshealth.UrlProbeH3UdpSilent {
		leg.Verdict = verdict
	}
	if leg := evidence.Ipv6; leg != nil {
		if leg.Tcp == egresshealth.UrlProbeIpv6TcpFailed {
			leg.Verdict = verdict
		}
		if leg.H3 != nil && leg.H3.Udp == egresshealth.UrlProbeH3UdpSilent {
			leg.H3.Verdict = verdict
		}
	}
	evidence.E2e = e2e
	return health
}

// The stored failed_later_legs and pq_outcome of one run.
func fp2EvidenceTestStored(t testing.TB, runId server.Id) (int16, int16) {
	t.Helper()
	ctx := t.Context()
	var failedLaterLegs, pqOutcome int16
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT failed_later_legs, pq_outcome FROM provider_egress_health_history WHERE run_id = $1`,
			runId).Scan(&failedLaterLegs, &pqOutcome))
	})
	return failedLaterLegs, pqOutcome
}

var fp2EvidenceTestPositive = egresshealth.UrlProbeE2eEvidence{
	Pq: egresshealth.UrlProbePqPositive, KeyExchange: "X25519MLKEM768", SealedBeforeRequest: true, Window: egresshealth.UrlProbeWindowResizable,
}

var fp2EvidenceTestNegative = egresshealth.UrlProbeE2eEvidence{
	Pq: egresshealth.UrlProbePqNegative, KeyExchange: "X25519", SealedBeforeRequest: true, Window: egresshealth.UrlProbeWindowFixed,
}

var fp2EvidenceTestUnmeasured = egresshealth.UrlProbeE2eEvidence{
	Pq: egresshealth.UrlProbePqUnmeasured, Window: egresshealth.UrlProbeWindowNotTested,
}

// The store writes the judged mask of each failed outcome and the
// post-quantum outcome of each measured one, in the columns
// FP2-PROTOCOL.md §4.1 adds, and returns both.
func TestProviderEgressStoreWritesTheFailedLaterLegsAndPostQuantumOutcome(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		passed, failed, silent := egresshealth.UrlProbeIpv6TcpPassed, egresshealth.UrlProbeIpv6TcpFailed, egresshealth.UrlProbeH3UdpSilent
		h3Passed := egresshealth.UrlProbeH3UdpPassed
		provider, place := egresshealth.UrlProbeH3VerdictProvider, egresshealth.UrlProbeH3VerdictPlace
		for i, c := range []struct {
			name            string
			h3, tcp, udp6   string
			verdict         string
			e2e             *egresshealth.UrlProbeE2eEvidence
			failedLaterLegs int16
			pqOutcome       int16
		}{
			{name: "passing run", h3: h3Passed, tcp: passed, udp6: h3Passed, e2e: &fp2EvidenceTestPositive,
				pqOutcome: egresshealth.UrlProbePqOutcomePositive},
			{name: "v4 h3 silent provider", h3: silent, tcp: passed, udp6: h3Passed, verdict: provider, e2e: &fp2EvidenceTestPositive,
				failedLaterLegs: 1, pqOutcome: egresshealth.UrlProbePqOutcomePositive},
			{name: "v6 https failed provider", h3: h3Passed, tcp: failed, verdict: provider, e2e: &fp2EvidenceTestNegative,
				failedLaterLegs: 2, pqOutcome: egresshealth.UrlProbePqOutcomeNegative},
			{name: "v6 h3 silent provider", h3: h3Passed, tcp: passed, udp6: silent, verdict: provider, e2e: &fp2EvidenceTestUnmeasured,
				failedLaterLegs: 4},
			{name: "v4 h3 silent and v6 https failed", h3: silent, tcp: failed, verdict: provider,
				failedLaterLegs: 1 | 2},
			{name: "v4 h3 silent and v6 h3 silent", h3: silent, tcp: passed, udp6: silent, verdict: provider,
				failedLaterLegs: 1 | 4},
			{name: "v4 h3 silent unjudged", h3: silent, tcp: passed, udp6: h3Passed},
		} {
			health := fp2EvidenceTestOutcome(server.NewId(), now.Add(time.Duration(i)*time.Second), c.h3, c.tcp, c.udp6, c.verdict, c.e2e)
			if err := health.UrlProbeEvidence.ValidateOutcome(health.OKCount, health.Total, false); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			stored := StoreProviderEgressHealth(ctx, health)
			if !stored.Stored || stored.FailedLaterLegs != c.failedLaterLegs || stored.PqOutcome != c.pqOutcome {
				t.Fatalf("%s: stored %+v, want mask %d and outcome %d", c.name, stored, c.failedLaterLegs, c.pqOutcome)
			}
			if failedLaterLegs, pqOutcome := fp2EvidenceTestStored(t, health.RunId); failedLaterLegs != c.failedLaterLegs || pqOutcome != c.pqOutcome {
				t.Fatalf("%s: history holds mask %d and outcome %d, want %d and %d", c.name, failedLaterLegs, pqOutcome, c.failedLaterLegs, c.pqOutcome)
			}
		}
		// a place-judged failure is stored as the pass the judge made of it
		placeJudged := fp2EvidenceTestOutcome(server.NewId(), now.Add(time.Minute), egresshealth.UrlProbeH3UdpPassed, failed, "", place, nil)
		placeJudged.OKCount = 1
		placeJudged.ClassResults = map[string]ProviderEgressHealthClassResult{"site": {OK: 1, Total: 1}}
		placeJudged.FailedNames = ""
		if err := placeJudged.UrlProbeEvidence.ValidateOutcome(1, 1, false); err != nil {
			t.Fatal(err)
		}
		StoreProviderEgressHealth(ctx, placeJudged)
		if failedLaterLegs, pqOutcome := fp2EvidenceTestStored(t, placeJudged.RunId); failedLaterLegs != 0 || pqOutcome != 0 {
			t.Fatalf("a place-judged pass holds mask %d outcome %d", failedLaterLegs, pqOutcome)
		}
	})
}

// A security recheck and an outcome on probation never enter the window, so
// both store 0 whatever their legs and session; a replay of a stored run id
// changes nothing.
func TestProviderEgressRecheckProbationAndReplayStoreNoFp2Evidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)

		// an open TLS finding for the URL makes the provider's next outcome
		// there a recheck
		clientId := server.NewId()
		finding := &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now.Add(-2 * time.Minute), Total: 1,
			TLSAuthenticationFailure: true, UrlProbeEvidence: fp2TestUrlEvidence(now.Add(-2*time.Minute), "https://fp2.example/", true)}
		if err := finding.UrlProbeEvidence.ValidateOutcome(0, 1, true); err != nil {
			t.Fatal(err)
		}
		if stored := StoreProviderEgressHealth(ctx, finding); !stored.Stored {
			t.Fatal("the TLS finding was not stored")
		}
		recheck := fp2EvidenceTestOutcome(clientId, now.Add(-time.Minute), egresshealth.UrlProbeH3UdpSilent, egresshealth.UrlProbeIpv6TcpFailed, "",
			egresshealth.UrlProbeH3VerdictProvider, &fp2EvidenceTestPositive)
		stored := StoreProviderEgressHealth(ctx, recheck)
		if !stored.Stored || !stored.Recheck || stored.FailedLaterLegs != 0 || stored.PqOutcome != 0 {
			t.Fatalf("recheck stored %+v", stored)
		}
		if failedLaterLegs, pqOutcome := fp2EvidenceTestStored(t, recheck.RunId); failedLaterLegs != 0 || pqOutcome != 0 {
			t.Fatalf("a recheck holds mask %d outcome %d", failedLaterLegs, pqOutcome)
		}

		probation := fp2EvidenceTestOutcome(server.NewId(), now, egresshealth.UrlProbeH3UdpSilent, egresshealth.UrlProbeIpv6TcpPassed, egresshealth.UrlProbeH3UdpPassed,
			egresshealth.UrlProbeH3VerdictProvider, &fp2EvidenceTestNegative)
		probation.UrlProbation = true
		stored = StoreProviderEgressHealth(ctx, probation)
		if !stored.Stored || !stored.Probation || stored.FailedLaterLegs != 0 || stored.PqOutcome != 0 {
			t.Fatalf("probation stored %+v", stored)
		}
		if failedLaterLegs, pqOutcome := fp2EvidenceTestStored(t, probation.RunId); failedLaterLegs != 0 || pqOutcome != 0 {
			t.Fatalf("an outcome on probation holds mask %d outcome %d", failedLaterLegs, pqOutcome)
		}

		first := fp2EvidenceTestOutcome(server.NewId(), now.Add(time.Second), egresshealth.UrlProbeH3UdpSilent, egresshealth.UrlProbeIpv6TcpPassed, egresshealth.UrlProbeH3UdpPassed,
			egresshealth.UrlProbeH3VerdictProvider, &fp2EvidenceTestNegative)
		StoreProviderEgressHealth(ctx, first)
		replay := fp2EvidenceTestOutcome(first.ClientId, now.Add(time.Second), egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpFailed, "",
			egresshealth.UrlProbeH3VerdictProvider, &fp2EvidenceTestPositive)
		replay.RunId = first.RunId
		if stored := StoreProviderEgressHealth(ctx, replay); stored.Stored {
			t.Fatalf("a replay stored %+v", stored)
		}
		if failedLaterLegs, pqOutcome := fp2EvidenceTestStored(t, first.RunId); failedLaterLegs != 1 || pqOutcome != egresshealth.UrlProbePqOutcomeNegative {
			t.Fatalf("the replay changed the stored run to mask %d outcome %d", failedLaterLegs, pqOutcome)
		}
	})
}

// During a rollout an older release keeps storing URL outcomes with its own
// insert, which names neither column. Its outcome reads 0 for both: a plain
// failure, never forgiven, and no post-quantum observation, never positive.
func TestProviderEgressOutcomeFromAnOlderReleaseHasNoFp2Evidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		// a current store first, which creates the day's partition
		StoreProviderEgressHealth(ctx, fp2EvidenceTestOutcome(server.NewId(), now.Add(-2*time.Minute), egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, "", "", nil))

		older := fp2EvidenceTestOutcome(server.NewId(), now.Add(-time.Minute), egresshealth.UrlProbeH3UdpSilent, egresshealth.UrlProbeIpv6TcpPassed, egresshealth.UrlProbeH3UdpPassed,
			egresshealth.UrlProbeH3VerdictProvider, &fp2EvidenceTestPositive)
		evidenceJson, err := json.Marshal(older.UrlProbeEvidence)
		if err != nil {
			t.Fatal(err)
		}
		// the release before the columns, its statement verbatim
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO provider_egress_health_history
				(run_id, client_id, measured_at, ok_count, total_count, class_results, tls_authentication_failure, cycle_started_at, url_probe, url_probe_evidence, url_probe_policy_version, url_source, url_probation)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10::jsonb,$11,$12,$13)
			ON CONFLICT DO NOTHING`,
				older.RunId, older.ClientId, older.MeasuredAt.UTC(), 0, 1,
				`{"site":{"ok":0,"total":1}}`, false, nil, false, string(evidenceJson), older.UrlProbeEvidence.PolicyVersion, int16(0), false))
		})
		if failedLaterLegs, pqOutcome := fp2EvidenceTestStored(t, older.RunId); failedLaterLegs != 0 || pqOutcome != 0 {
			t.Fatalf("an older release's outcome holds mask %d outcome %d", failedLaterLegs, pqOutcome)
		}
	})
}

// A stored post-quantum turn records its conclusive observation as modern
// evidence, as the capability probe recorded its own: a positive session as
// post-quantum with resizable windows, a classical one as neither, and a
// post-quantum group that never sealed with its windows but not as
// post-quantum. An unmeasured session, a recheck and an outcome on probation
// record nothing.
func TestProviderEgressE2eRecordsConclusiveModernEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		modern := server.NewId()
		StoreProviderEgressHealth(ctx, fp2EvidenceTestOutcome(modern, now.Add(-3*time.Minute), egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, "", "", &fp2EvidenceTestPositive))
		classical := server.NewId()
		StoreProviderEgressHealth(ctx, fp2EvidenceTestOutcome(classical, now.Add(-2*time.Minute), egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, "", "", &fp2EvidenceTestNegative))
		unmeasured := server.NewId()
		StoreProviderEgressHealth(ctx, fp2EvidenceTestOutcome(unmeasured, now.Add(-time.Minute), egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, "", "", &fp2EvidenceTestUnmeasured))
		probation := fp2EvidenceTestOutcome(server.NewId(), now, egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, "", "", &fp2EvidenceTestPositive)
		probation.UrlProbation = true
		StoreProviderEgressHealth(ctx, probation)
		// a post-quantum group negotiated that never sealed: a negative
		// outcome, never post-quantum evidence (FP2-PROTOCOL.md §12.8)
		unsealed := server.NewId()
		StoreProviderEgressHealth(ctx, fp2EvidenceTestOutcome(unsealed, now.Add(-90*time.Second), egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, "", "", &egresshealth.UrlProbeE2eEvidence{
			Pq: egresshealth.UrlProbePqNegative, KeyExchange: "X25519MLKEM768", SealedBeforeRequest: false, Window: egresshealth.UrlProbeWindowResizable,
		}))

		records := getProviderModernEvidenceRecords(ctx, []server.Id{modern, classical, unmeasured, probation.ClientId, unsealed})
		if record := records[modern]; record == nil || !record.PostQuantum || !record.WindowResizable || record.KeyExchange != "X25519MLKEM768" ||
			!record.MeasuredAt.Equal(now.Add(-3*time.Minute)) {
			t.Fatalf("a positive turn recorded %+v", record)
		}
		if record := records[classical]; record == nil || record.PostQuantum || record.WindowResizable || record.KeyExchange != "X25519" {
			t.Fatalf("a classical turn recorded %+v", record)
		}
		if record := records[unsealed]; record == nil || record.PostQuantum || !record.WindowResizable || record.KeyExchange != "X25519MLKEM768" ||
			providerModernEvidencePostQuantum(record, now) || classifyProviderModernEvidence(record, now) != ProviderModernOutcomeNotPostQuantum {
			t.Fatalf("an unsealed post-quantum turn recorded %+v", record)
		}
		if record, ok := records[unmeasured]; ok {
			t.Fatalf("an unmeasured turn recorded %+v", record)
		}
		if record, ok := records[probation.ClientId]; ok {
			t.Fatalf("an outcome on probation recorded %+v", record)
		}
	})
}

// The ready marker sets only once the post-quantum stream has run for a whole
// window and is still running, with the export's ttl; clears, and forgets the
// stream, once it goes silent; and a new stream starts a window of its own.
func TestClientScorePostQuantumReadyMarker(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultClientScorePostQuantumReadySettings()
		ttl := 5 * time.Hour
		start := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
		mark := func(at time.Time) bool {
			ready, err := markClientScorePostQuantumReady(ctx, ttl, at, settings)
			if err != nil {
				t.Fatal(err)
			}
			return ready
		}
		readyValue := func() (string, time.Duration, bool) {
			var value string
			var expire time.Duration
			found := false
			server.Redis(ctx, func(r server.RedisClient) {
				v, err := r.Get(ctx, ClientScorePostQuantumReadyKey).Result()
				if err == nil {
					value, found = v, true
					expire = r.TTL(ctx, ClientScorePostQuantumReadyKey).Val()
				}
			})
			return value, expire, found
		}

		if mark(start) {
			t.Fatal("ready with no stream")
		}
		// a stream that runs, noted every ten minutes
		at := start
		for ; at.Before(start.Add(settings.ReadyAfter)); at = at.Add(10 * time.Minute) {
			noteClientScorePostQuantumStream(ctx, at, settings)
			if mark(at) {
				t.Fatalf("ready at %s, before a whole window", at.Sub(start))
			}
		}
		noteClientScorePostQuantumStream(ctx, at, settings)
		if !mark(at) {
			t.Fatalf("not ready at %s, a whole window", at.Sub(start))
		}
		value, expire, found := readyValue()
		if !found || value != strconv.FormatInt(start.UnixMilli(), 10) || expire <= 0 || ttl < expire {
			t.Fatalf("marker %q expire %s found %t", value, expire, found)
		}
		// the export's ttl outlives a short silence
		if !mark(at.Add(settings.SeenMaxAge - time.Second)) {
			t.Fatal("a stream silent less than its bound stopped being ready")
		}
		// silence past the bound ends the stream
		silentAt := at.Add(settings.SeenMaxAge)
		if mark(silentAt) {
			t.Fatal("a silent stream stayed ready")
		}
		if _, _, found := readyValue(); found {
			t.Fatal("the marker of a silent stream remains")
		}
		// the next stream starts a window of its own
		restart := silentAt.Add(time.Hour)
		noteClientScorePostQuantumStream(ctx, restart, settings)
		if mark(restart.Add(time.Minute)) {
			t.Fatal("a restarted stream was ready at once")
		}
		at = restart
		for ; at.Before(restart.Add(settings.ReadyAfter)); at = at.Add(10 * time.Minute) {
			noteClientScorePostQuantumStream(ctx, at, settings)
		}
		noteClientScorePostQuantumStream(ctx, at, settings)
		if !mark(at) {
			t.Fatal("a restarted stream was not ready after its own window")
		}
		// a stream noted with a gap longer than the bound restarted at the
		// note after the gap, so a window counts only from there
		gapAt := at.Add(settings.SeenMaxAge)
		noteClientScorePostQuantumStream(ctx, gapAt, settings)
		if mark(gapAt) {
			t.Fatal("a stream that went silent past the bound kept its old window")
		}
		if _, err := markClientScorePostQuantumReady(ctx, 0, restart, settings); err == nil {
			t.Fatal("a marker without a ttl was accepted")
		}
	})
}

// A stored post-quantum turn notes the stream through the ingest's own
// store, and a turn without a session does not.
func TestProviderEgressE2eNotesThePostQuantumStream(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		streamNoted := func() bool {
			noted := false
			server.Redis(ctx, func(r server.RedisClient) {
				noted = 0 < r.Exists(ctx, clientScorePostQuantumSinceKey, clientScorePostQuantumSeenKey).Val()
			})
			return noted
		}
		StoreProviderEgressHealth(ctx, fp2EvidenceTestOutcome(server.NewId(), now.Add(-time.Minute), egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, "", "", nil))
		if streamNoted() {
			t.Fatal("a turn without a session noted the post-quantum stream")
		}
		StoreProviderEgressHealth(ctx, fp2EvidenceTestOutcome(server.NewId(), now, egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, "", "", &fp2EvidenceTestUnmeasured))
		if !streamNoted() {
			t.Fatal("a post-quantum turn did not note the stream")
		}
	})
}
