// Provider admission, accepted URL history, and quota progress share durable evidence.
package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// Every common-gate pass stays online even without client performance samples.
func TestFp2BucketMembershipAndCommonGates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		ctx := t.Context()
		city := egressTestCity(ctx, "Example City", "Example Region", "Example Country", "zz")
		passing := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		nonQuality := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, &ConnectionLocationScores{ArinNonQuality: true})
		risk := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{ArinRisk: true})
		unprobed := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		failing := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		dark := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		mislocated := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, nil)
		tls := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		unreliable := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		for _, provider := range []*egressTestProvider{passing, nonQuality, risk, mislocated, unreliable} {
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 1)
		}
		egressTestHealth(ctx, failing.clientId, server.NowUtc(), 5, 2)
		egressTestBlackhole(ctx, dark.clientId)
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{ClientId: mislocated.clientId, LocationId: city.LocationId, CountryCode: "yy", ObservedAt: server.NowUtc()})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{ClientId: tls.clientId, MeasuredAt: server.NowUtc(), OKCount: 3, Total: 3, TLSAuthenticationFailure: true})
		egressTestReliability(ctx, unprobed.clientId, 1, 1, 1)
		egressTestReliability(ctx, unreliable.clientId, 1, 0.1, 1)
		egressTestPasses(ctx, t)
		common := []*egressTestProvider{passing, nonQuality, unprobed, failing, dark, mislocated}
		cached := map[RankMode]map[server.Id]*ClientScore{}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			scores := egressTestCachedScores(ctx, t, city, mode, false)
			cached[mode] = scores
			if len(scores) != len(common) {
				t.Fatalf("mode=%s scores=%d want=%d", mode, len(scores), len(common))
			}
			for _, provider := range common {
				score := scores[provider.clientId]
				if score == nil || !score.Online {
					t.Fatalf("mode=%s online provider missing", mode)
				}
				// The deployed legacy publisher removed these providers after
				// logging them as online: either missing client measurement
				// contributes 40 points, which failed its speed minimum. That
				// performance penalty must survive as ranking data, never as
				// an admission gate for an otherwise eligible provider.
				if score.HasLatencyTest || score.HasSpeedTest || score.Scores[RankModeSpeed] < 2*ClientScorePerTier {
					t.Fatalf("mode=%s missing-measurement control lost its legacy >=40 penalty: latency=%t speed=%t score=%d", mode, score.HasLatencyTest, score.HasSpeedTest, score.Scores[RankModeSpeed])
				}
				wantNative := provider == passing || provider == mislocated || (provider == nonQuality && mode == RankModeSpeed)
				if score.PassesMinimums[mode] != wantNative {
					t.Errorf("mode=%s native=%t want=%t", mode, score.PassesMinimums[mode], wantNative)
				}
			}
		}
		for _, nativeReader := range []bool{false, true} {
			func() {
				pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(fmt.Sprintf("subscriber_quality_policy_version: 2\negress_index:\n  native_reader_enabled: %t\n", nativeReader)))
				defer pop()
				requestEgressIndexSettingsSnapshot.Store(nil)
				defer requestEgressIndexSettingsSnapshot.Store(nil)
				for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
					// ARIN subscriber evidence controls native Quality, not a
					// Quality request's independent Speed/Online fallback.
					// Assert identities and priority, not only a total count.
					wantTiers := map[server.Id]int{}
					for _, provider := range common {
						tier := 2 * egressTestBackfillOffset()
						if provider == passing || provider == mislocated || provider == nonQuality && mode == RankModeSpeed {
							tier = cached[mode][provider.clientId].Tiers[mode]
						} else if provider == nonQuality {
							tier = cached[RankModeSpeed][provider.clientId].Tiers[RankModeSpeed] + egressTestBackfillOffset()
						}
						wantTiers[provider.clientId] = tier
					}
					providers := egressTestFind(ctx, t, egressTestLocationSpec(city), mode, len(common), false, server.NewId())
					if len(providers) != len(wantTiers) {
						t.Fatalf("reader=%t mode=%s returned=%d want=%d", nativeReader, mode, len(providers), len(wantTiers))
					}
					assertEgressTestNoRepeats(t, providers)
					assertEgressTestTiersKeepOrder(t, providers)
					for index, provider := range providers {
						wantTier, exists := wantTiers[provider.ClientId]
						if !exists || provider.Tier != wantTier {
							t.Fatalf("reader=%t mode=%s index=%d: unexpected provider or bucket tier=%d want=%d", nativeReader, mode, index, provider.Tier, wantTier)
						}
						wantNativeCount := 2
						native := provider.ClientId == passing.clientId || provider.ClientId == mislocated.clientId
						if mode == RankModeSpeed {
							wantNativeCount = 3
							native = native || provider.ClientId == nonQuality.clientId
						}
						if native != (index < wantNativeCount) || mode == RankModeQuality && (provider.ClientId == nonQuality.clientId) != (index == 2) {
							t.Fatalf("reader=%t mode=%s: native, borrowed and Online priorities changed", nativeReader, mode)
						}
					}

					// Forced and explicitly named Quality retain the requested
					// bucket policy. They cannot promote the Speed borrower.
					forced := egressTestFind(ctx, t, egressTestLocationSpec(city), mode, len(common), true, server.NewId())
					wantForced := len(common)
					if mode == RankModeQuality {
						wantForced--
					}
					if len(forced) != wantForced {
						t.Fatalf("reader=%t mode=%s: forced membership=%d want=%d", nativeReader, mode, len(forced), wantForced)
					}
					assertEgressTestNoRepeats(t, forced)
					for _, provider := range forced {
						if _, exists := wantTiers[provider.ClientId]; !exists || mode == RankModeQuality && provider.ClientId == nonQuality.clientId {
							t.Fatalf("reader=%t mode=%s: forced selection bypassed a bucket or common gate", nativeReader, mode)
						}
					}
					for _, forceMinimum := range []bool{false, true} {
						named := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &nonQuality.clientId}}, mode, 1, forceMinimum, server.NewId())
						if mode == RankModeQuality && len(named) != 0 || mode == RankModeSpeed && (len(named) != 1 || named[0].ClientId != nonQuality.clientId) {
							t.Fatalf("reader=%t mode=%s forced=%t: named provider lost its requested bucket policy", nativeReader, mode, forceMinimum)
						}
						for _, excluded := range []*egressTestProvider{risk, tls, unreliable} {
							providers := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &excluded.clientId}}, mode, 1, forceMinimum, server.NewId())
							if len(providers) != 0 {
								t.Errorf("reader=%t mode=%s forced=%t: common gate bypassed", nativeReader, mode, forceMinimum)
							}
						}
					}
				}
			}()
		}
		counts := CountProviderEgress(ctx)
		onlineCount := int64(0)
		for _, count := range counts.BucketIndexCounts[ProviderEgressBucketOnline] {
			onlineCount += count
		}
		if onlineCount != int64(len(common)) {
			t.Fatalf("online count=%d want=%d", onlineCount, len(common))
		}
	})
}

// Accepted reports aggregate independently of arrival order and retries do not add evidence.
func TestFp2UrlHistoryIsIdempotentAndWindowed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		now := server.NowUtc()
		latest := &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now.Add(-time.Hour), OKCount: 1, Total: 1}
		latest.UrlProbeEvidence = fp2TestUrlProbeEvidence(latest.MeasuredAt, true)
		SetProviderEgressHealth(ctx, latest)
		egressTestHealth(ctx, clientId, now.Add(-2*time.Hour), 2, 0)
		egressTestHealth(ctx, clientId, now.Add(-90*time.Minute), 2, 2)
		egressTestHealth(ctx, clientId, now.Add(-9*time.Hour), 100, 100)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now.Add(-30 * time.Minute), NotMeasuredCount: 20})
		replay := *latest
		replay.MeasuredAt = now
		SetProviderEgressHealth(ctx, &replay)
		counts := GetAllProviderEgressHealthCounts(ctx)[clientId]
		if counts.OKCount != 3 || counts.Total != 5 {
			t.Fatalf("history counts=%+v, want3/5", counts)
		}
		if ComputeEgressIndex(&EgressHealthRun{MeasuredAt: counts.MeasuredAt, OkCount: counts.OKCount, Total: counts.Total}, now, DefaultEgressIndexSettings()).Quality {
			t.Fatal("aggregate 3/5 must fail the 4/5 gate despite the most recent measured run 1/1 and unmeasured attempts")
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT SUM(total_count) FROM (`+providerEgressHealthWindowSql()+`) AS evidence WHERE client_id=$3`, now.Add(-2*time.Hour), now, clientId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing exact-boundary aggregate")
				}
				var total int
				server.Raise(rows.Scan(&total))
				if total != 3 {
					t.Fatalf("exact-cutoff event stayed in window: total=%d want3", total)
				}
			})
		})
	})
}

// Retries, grouped legacy reports, and stale tokens cannot advance URL progress.
func TestFp2UrlCycleProgressIsIdempotentAndTokenBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		now := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle (client_id,cycle_started_at,next_attempt_at) VALUES($1,$2,$2)`, clientId, now.Add(-time.Hour)))
		})
		for index := range 5 {
			ok := 0
			if index < 3 {
				ok = 1
			}
			health := &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, CycleStartedAt: now.Add(-time.Hour), MeasuredAt: now.Add(time.Duration(index) * time.Microsecond), OKCount: ok, Total: 1}
			health.UrlProbeEvidence = fp2TestUrlProbeEvidence(health.MeasuredAt, ok == 1)
			SetProviderEgressHealth(ctx, health)
			SetProviderEgressHealth(ctx, health)
		}
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, CycleStartedAt: now.Add(-time.Hour), MeasuredAt: now.Add(6 * time.Microsecond), OKCount: 7, Total: 7})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, CycleStartedAt: now.Add(-5 * time.Hour), MeasuredAt: now.Add(7 * time.Microsecond), OKCount: 1, Total: 1})
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT success_count,error_count,outcome_count FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing cycle")
				}
				var successCount, errorCount, outcomeCount int
				server.Raise(rows.Scan(&successCount, &errorCount, &outcomeCount))
				if successCount != 3 || errorCount != 2 || outcomeCount != 5 {
					t.Fatalf("URL successes=%d errors=%d ordinal=%d want3/2/5", successCount, errorCount, outcomeCount)
				}
			})
		})
	})
}

// Diagnostic ordering cannot hide a delayed security finding or its clean recovery.
func TestFp2SecurityEventsOrderIndependentlyOfUnmeasuredReports(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		now := server.NowUtc().Truncate(time.Microsecond)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now, NotMeasuredCount: 1})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now.Add(-2 * time.Minute), Total: 1, TLSAuthenticationFailure: true, UrlProbeEvidence: fp2TestUrlEvidence(now.Add(-2*time.Minute), "https://affected.example/", true)})
		if !GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId] {
			t.Fatal("a newer unmeasured report suppressed a delayed TLS finding")
		}
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now.Add(time.Minute), Total: 1})
		if !GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId] {
			t.Fatal("an ordinary URL error cleared the security finding")
		}
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now.Add(-time.Minute), OKCount: 1, Total: 1, UrlProbeEvidence: fp2TestUrlEvidence(now.Add(-time.Minute), "https://affected.example/", false)})
		if GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId] {
			t.Fatal("a later clean success did not clear the older TLS finding")
		}
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now.Add(-3 * time.Minute), Total: 1, TLSAuthenticationFailure: true, UrlProbeEvidence: fp2TestUrlEvidence(now.Add(-3*time.Minute), "https://affected.example/", true)})
		if GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId] {
			t.Fatal("a delayed stale TLS finding overrode a later clean success")
		}
		if health := GetProviderEgressHealth(ctx, clientId); health == nil || !health.MeasuredAt.Equal(now.Add(time.Minute)) {
			t.Fatal("security ordering regressed the latest diagnostic timestamp")
		}
	})
}

func fp2TestUrlEvidence(at time.Time, target string, tlsFailure bool) *egresshealth.UrlProbeEvidence {
	destination := egresshealth.Destination{Name: "synthetic-target", Class: egresshealth.ClassSite, Url: target}
	evidence := fp2TestUrlProbeEvidence(at, !tlsFailure)
	evidence.Destination = destination
	evidence.Security = []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsFailure: tlsFailure, TlsAuthenticated: !tlsFailure}}
	return evidence
}

// Synthetic policy-one evidence shared with due/rolling-quota tests. It is a
// complete tiny document, not a fabricated high-bandwidth measurement.
func fp2TestUrlProbeEvidence(at time.Time, ok bool) *egresshealth.UrlProbeEvidence {
	destination := egresshealth.Destination{Name: "synthetic-document", Class: egresshealth.ClassSite, Url: "https://document.example/"}
	evidence := &egresshealth.UrlProbeEvidence{
		PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
		Destination: destination, MeasuredAt: at, ContentMatcherVersion: 1,
		Security:   []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: at, TlsAuthenticated: true}},
		StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
		RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000,
		ContentClassification: "content", PerformanceClassification: "insufficient_sample",
	}
	if !ok {
		evidence.ContentClassification = "captcha"
		evidence.PerformanceClassification = ""
		evidence.FailureStage = "response_content"
	}
	return evidence
}

// Every failed URL needs its own later authenticated response. Content errors
// can clear that URL's TLS finding, but unrelated successes and replays cannot.
func TestFp2UrlSecurityRecoveryIsExactOrderedAndIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		now := server.NowUtc().Truncate(time.Microsecond)
		submit := func(at time.Time, target string, tls bool, ok int) *ProviderEgressHealth {
			health := &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: at, Total: 1, OKCount: ok, TLSAuthenticationFailure: tls, UrlProbeEvidence: fp2TestUrlEvidence(at, target, tls)}
			if ok == 0 && !tls {
				health.UrlProbeEvidence.ContentClassification = "captcha"
				health.UrlProbeEvidence.PerformanceClassification = ""
				health.UrlProbeEvidence.FailureStage = "response_content"
			}
			SetProviderEgressHealth(ctx, health)
			return health
		}
		assertQuarantine := func(want bool) {
			if got := GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId]; got != want {
				t.Fatalf("security quarantine=%t want=%t", got, want)
			}
		}
		submit(now, "https://a.example/", true, 0)
		submit(now, "https://b.example/", true, 0)
		submit(now.Add(time.Second), "https://unrelated.example/", false, 1)
		assertQuarantine(true)
		submit(now.Add(2*time.Second), "https://a.example/", false, 0)
		assertQuarantine(true)
		cleanB := submit(now.Add(3*time.Second), "https://b.example/", false, 0)
		assertQuarantine(false)
		submit(now.Add(time.Second), "https://b.example/", true, 0)
		assertQuarantine(false)
		replay := *cleanB
		replay.TLSAuthenticationFailure = true
		replay.UrlProbeEvidence = fp2TestUrlEvidence(now.Add(4*time.Second), "https://b.example/", true)
		SetProviderEgressHealth(ctx, &replay)
		assertQuarantine(false)
		submit(now.Add(5*time.Second), "https://b.example/", true, 0)
		submit(now.Add(5*time.Second), "https://b.example/", false, 1)
		assertQuarantine(true)
		submit(now.Add(6*time.Second), "https://b.example/", false, 0)
		assertQuarantine(false)
	})
}

// An old aggregate bit names no trustworthy URL. Never guess that the next
// unrelated success authenticated that missing target.
func TestFp2LegacyTlsQuarantineCannotClearOnUnrelatedUrl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		now := server.NowUtc().Truncate(time.Microsecond)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{ClientId: clientId, MeasuredAt: now.Add(-24 * time.Hour), Total: 1, TLSAuthenticationFailure: true})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{ClientId: clientId, MeasuredAt: now, OKCount: 1, Total: 1, UrlProbeEvidence: fp2TestUrlEvidence(now, "https://unrelated.example/", false)})
		// During schema/API cutover a legacy writer can still replace the old
		// projection. The retained quarantine column remains authoritative.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_health SET tls_authentication_failure=false WHERE client_id=$1`, clientId))
		})
		if !GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId] {
			t.Fatal("unidentified legacy security finding was silently cleared")
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT (`+providerHasUrlSecurityExceptionSql("$1")+`)`, clientId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing common-gate verdict")
				}
				var blocked bool
				server.Raise(rows.Scan(&blocked))
				if !blocked {
					t.Fatal("SQL common gate lost retained legacy quarantine")
				}
			})
		})
	})
}

// The published sampling weights increase with ratio at equal reliability and performance.
func TestFp2PublishedUrlRatioWeights(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Example City", "Example Region", "Example Country", "zz")
		lower := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		higher := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		// Compare two native-eligible ratios: 4/5 and 9/10.
		egressTestHealth(ctx, lower.clientId, server.NowUtc(), 5, 1)
		egressTestHealth(ctx, higher.clientId, server.NowUtc(), 10, 1)
		egressTestPasses(ctx, t)
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			scores := egressTestCachedScores(ctx, t, city, mode, false)
			lowScore, highScore := scores[lower.clientId], scores[higher.clientId]
			if lowScore == nil || highScore == nil || lowScore.ScaledWeights[mode] <= 0 || highScore.ScaledWeights[mode] <= lowScore.ScaledWeights[mode] || lowScore.Tiers[mode] < highScore.Tiers[mode] {
				t.Fatalf("mode=%s higher ratio did not improve published ranking", mode)
			}
		}
	})
}

// Poor performance lowers ranking weight without removing native membership.
func TestFp2PerformancePenaltyDoesNotBecomePerfectWeight(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Example City", "Example Region", "Example Country", "zz")
		fast := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		slow := egressTestConnect(ctx, t, city, egressTestPerformance{latencyMs: 60000, bytesPerSecond: 1}, nil, nil)
		for _, provider := range []*egressTestProvider{fast, slow} {
			egressTestHealth(ctx, provider.clientId, server.NowUtc(), 5, 0)
		}
		egressTestPasses(ctx, t)
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			scores := egressTestCachedScores(ctx, t, city, mode, false)
			fastScore, slowScore := scores[fast.clientId], scores[slow.clientId]
			if slowScore == nil || !slowScore.PassesMinimums[mode] || !slowScore.Online {
				t.Fatalf("mode=%s performance became an admission gate", mode)
			}
			if slowScore.ScaledWeights[mode] <= 0 || fastScore.ScaledWeights[mode] <= slowScore.ScaledWeights[mode] {
				t.Fatalf("mode=%s fast weight=%f slow weight=%f", mode, fastScore.ScaledWeights[mode], slowScore.ScaledWeights[mode])
			}
		}
	})
}

// ARIN facts follow active connection generations instead of disconnected history.
func TestFp2ArinRollupForgetsDisconnectedRisk(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Example City", "Example Region", "Example Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestFast, nil, &ConnectionLocationScores{ArinRisk: true, ArinNonQuality: true})
		handlerId := CreateNetworkClientHandler(ctx)
		cleanConnectionId, _, _, _, err := ConnectNetworkClient(ctx, provider.clientId, "192.0.2.250:0", handlerId)
		if err != nil {
			t.Fatal(err)
		}
		if err := SetConnectionLocation(ctx, cleanConnectionId, city.LocationId, &ConnectionLocationScores{ArinQualityVerified: true}); err != nil {
			t.Fatal(err)
		}
		assertFlags := func(want bool) {
			t.Helper()
			testing_rollUpEgress(ctx)
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT arin_risk, arin_non_quality FROM network_client_location_reliability WHERE client_id=$1`, provider.clientId)
				server.WithPgResult(rows, err, func() {
					if !rows.Next() {
						t.Fatal("missing rollup")
					}
					var risk, nonQuality bool
					server.Raise(rows.Scan(&risk, &nonQuality))
					if risk != want || nonQuality != want {
						t.Fatalf("risk=%t non_quality=%t want=%t", risk, nonQuality, want)
					}
				})
			})
		}
		assertFlags(true)
		if err := DisconnectNetworkClient(ctx, provider.connectionId); err != nil {
			t.Fatal(err)
		}
		assertFlags(false)
	})
}

// Request-time freshness demotes an expired cached passing verdict to online.
func TestFp2CachedEvidenceExpiresBeforeCacheTtl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, testCase := range []struct {
			name         string
			online       bool
			native       bool
			expired      bool
			missingClock bool
			wantCount    int
			wantTier     int
		}{
			{name: "fresh_native", native: true, wantCount: 1},
			{name: "expired_native", native: true, expired: true, wantCount: 1, wantTier: 2 * egressTestBackfillOffset()},
			{name: "native_without_clock", native: true, missingClock: true, wantCount: 1, wantTier: 2 * egressTestBackfillOffset()},
			{name: "expired_online", online: true, expired: true, wantCount: 1, wantTier: 2 * egressTestBackfillOffset()},
			{name: "expired_never_admitted", expired: true},
		} {
			locationId := server.NewId()
			score := onlineBackfillScore(testCase.online, 1)
			score.PassesMinimums = map[string]bool{RankModeQuality: testCase.native, RankModeSpeed: testCase.native}
			if testCase.expired {
				expired := server.NowUtc().Add(-time.Second)
				score.EgressValidUntil = &expired
			} else if testCase.missingClock {
				score.EgressValidUntil = nil
			}
			for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
				writeOnlineBackfillSample(ctx, t, locationId, mode, false, []*ClientScore{score})
			}
			for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
				providers := egressTestFind(ctx, t, []*ProviderSpec{{LocationId: &locationId}}, mode, 1, false, server.NewId())
				if len(providers) != testCase.wantCount {
					t.Errorf("case=%s mode=%s provider count=%d want=%d", testCase.name, mode, len(providers), testCase.wantCount)
				} else if len(providers) != 0 && providers[0].Tier != testCase.wantTier {
					t.Errorf("case=%s mode=%s tier=%d want=%d", testCase.name, mode, providers[0].Tier, testCase.wantTier)
				}
			}
		}
	})
}

// Both execution paths apply identical observed reliability floors.
func TestFp2ReliabilitySqlMatchesGo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Example City", "Example Region", "Example Country", "zz")
		minimums := providerReliabilityMinimums()
		for _, weights := range []map[int]float64{{}, {1: minimums[1]}, {1: minimums[1] - 0.001}, {2: minimums[2]}, {2: minimums[2] - 0.001}, {3: minimums[3] - 0.001}, {0: 0.1}} {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			for lookback, weight := range weights {
				egressTestReliability(ctx, provider.clientId, lookback, weight, 1)
			}
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT `+providerReliabilityEligibilitySql("$1::uuid"), provider.clientId)
				server.WithPgResult(rows, err, func() {
					if !rows.Next() {
						t.Fatal("missing reliability decision")
					}
					var sqlPasses bool
					server.Raise(rows.Scan(&sqlPasses))
					if sqlPasses != providerReliabilityPasses(weights, minimums) {
						t.Fatalf("SQL/Go reliability disagreement for%v", weights)
					}
				})
			})
		}
	})
}
