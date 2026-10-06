// Cohort decisions retain the fleet policy while limiting evidence and cache
// authority to the identities captured by each consumer.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

func TestProviderCountCohortEmptyNeedsNoBackend(t *testing.T) {
	for _, ids := range [][]server.Id{nil, {}} {
		f := newProviderCountFilterForClients(t.Context(), ids)
		if len(f.arinRisk)+len(f.arinNonQuality)+len(f.reliabilityFailed)+len(f.tlsAuthenticationFailed)+len(f.countryCodes)+len(f.healthCounts) != 0 || f.healthWindowEnd.IsZero() {
			t.Fatal("empty consumer population loaded evidence or lost its window")
		}
	}
}

func TestProviderCountCohortExportPopulationAndLoadedPlans(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		cohortTestTables(ctx)
		country, group := cohortTestId(1000000), cohortTestId(2000000)
		cohortTestExec(ctx, `INSERT INTO location VALUES($1,'country','Synthetic',NULL,NULL,NULL,'us');`, country)
		cohortTestExec(ctx, `INSERT INTO location_group VALUES($1,'Synthetic',true);`, group)
		cohortTestExec(ctx, `INSERT INTO location_group_member VALUES($1,$2)`, group, country)
		ids := []server.Id{}
		for n := 1; n <= 13; n++ {
			id := cohortTestId(n)
			ids = append(ids, id)
			cohortTestExec(ctx, `INSERT INTO network_client(client_id,active,source_client_id) VALUES($1,$2,$3)`, id, n != 8, func() *server.Id {
				if n == 9 {
					v := cohortTestId(99)
					return &v
				}
				return nil
			}())
			mode := ProvideModePublic
			if n == 6 {
				mode = ProvideModeNetwork
			}
			cohortTestExec(ctx, `INSERT INTO provide_key VALUES($1,$2)`, id, mode)
			cohortTestExec(ctx, `INSERT INTO network_client_location_reliability(client_id,connected,valid,city_location_id,region_location_id,country_location_id,egress_index,egress_quality,arin_risk,arin_non_quality) VALUES($1,$2,$3,$4,$4,$4,0,true,$5,$6)`, id, n != 7, n != 13, country, n == 2 || n == 7 || n == 12, n == 5)
			if n != 10 {
				weight := 1.0
				if n == 3 {
					weight = 0.94
				}
				cohortTestExec(ctx, `INSERT INTO client_connection_reliability_score(client_id,lookback_index,independent_reliability_weight) VALUES($1,0,1),($1,1,$2)`, id, weight)
			}
			if n == 4 {
				cohortTestExec(ctx, `INSERT INTO provider_egress_health(client_id,legacy_tls_authentication_failure,measured_at) VALUES($1,true,now()-interval '30 days')`, id)
			}
			cohortTestExec(ctx, `INSERT INTO provider_egress_location VALUES($1,'US',now())`, id)
			if n != 10 {
				cohortTestExec(ctx, `INSERT INTO provider_egress_health_history VALUES($1,now()-interval '1 hour',1,1,'{}',$2),($1,now()-interval '9 hours',0,1,'{}',$2),($1,now()-interval '2 hours',0,1,'{}',$3)`, id, SelectedProviderUrlProbePolicyVersion(), SelectedProviderUrlProbePolicyVersion()+100)
			}
		}
		// The source-map census must retain health evidence outside the cohort.
		cohortTestExec(ctx, `INSERT INTO provider_egress_health_history VALUES($1,now()-interval '1 hour',1,1,'{}',$2)`, cohortTestId(10000), SelectedProviderUrlProbePolicyVersion())
		fleet := newProviderCountFilter(ctx, true)
		scoped := newProviderCountFilterForClients(ctx, append(append([]server.Id{}, ids...), ids...))
		for _, id := range ids {
			if fleet.arinRisk[id] != scoped.arinRisk[id] || fleet.arinNonQuality[id] != scoped.arinNonQuality[id] || fleet.reliabilityFailed[id] != scoped.reliabilityFailed[id] || fleet.tlsAuthenticationFailed[id] != scoped.tlsAuthenticationFailed[id] || fleet.countryCodes[id] != scoped.countryCodes[id] || fleet.healthCounts[id] != scoped.healthCounts[id] {
				t.Fatal("scoped facts differ from the existing decision evidence")
			}
		}
		if len(fleet.arinRisk) < 50000 || len(scoped.arinRisk) != 3 || len(scoped.healthCounts) != 12 {
			t.Fatal("fixture did not retain dense unrelated history or precise health scope")
		}
		largeCohort := make([]server.Id, 5000)
		for i := range largeCohort {
			largeCohort[i] = cohortTestId(10000 + i)
		}
		large := newProviderCountFilterForClients(ctx, append(largeCohort, largeCohort...))
		for _, id := range largeCohort {
			if large.arinRisk[id] != fleet.arinRisk[id] || large.arinNonQuality[id] != fleet.arinNonQuality[id] || large.reliabilityFailed[id] != fleet.reliabilityFailed[id] {
				t.Fatal("multi-batch scoped filter lost or changed evidence")
			}
		}
		if len(large.arinRisk) != 1000 || len(large.reliabilityFailed) != 1600 {
			t.Fatal("multi-batch population escaped scope")
		}
		t.Log("cohort control: 5000-client cohort with duplicate inputs retains every batch and tail")
		cohortTestPlanWork(ctx, t)
		t.Log("cohort control: exact scoped facts; old TLS retained; missing history neutral; accepted policy/window preserved")

		// pg_stat_statements is supplemental when the local test service enables it.
		pgss := false
		server.Db(ctx, func(c server.PgConn) {
			server.Raise(c.QueryRow(ctx, `SELECT current_setting('shared_preload_libraries') LIKE '%pg_stat_statements%'`).Scan(&pgss))
		})
		if pgss {
			cohortTestExec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements; SELECT pg_stat_statements_reset()`)
		}
		settings := egressIndexSettings()
		wantCounts := &ProviderEgressCounts{BucketIndexCounts: map[string]map[string]int64{}, ReasonCounts: map[string]int64{}, MaxIndex: settings.MaxIndex()}
		for _, bucket := range ProviderEgressBuckets {
			wantCounts.BucketIndexCounts[bucket] = map[string]int64{}
		}
		for _, reason := range ProviderExcludedReasons {
			wantCounts.ReasonCounts[reason] = 0
		}
		wantLocation := 0
		for _, n := range []int{1, 2, 3, 4, 5, 10, 11, 12} {
			id := cohortTestId(n)
			code := "us"
			index := 0
			quality := true
			d := decideProviderEgress(fleet.egressFacts(id, &code, &index, &quality, settings), providerEgressTestEnabled())
			if d.quality {
				wantCounts.BucketIndexCounts[RankModeQuality]["0"]++
			}
			if d.speed {
				wantCounts.BucketIndexCounts[RankModeSpeed]["0"]++
			}
			if d.online {
				wantCounts.BucketIndexCounts[ProviderEgressBucketOnline]["0"]++
			}
			if d.reason != "" {
				wantCounts.ReasonCounts[d.reason]++
			}
			if !fleet.hasHardEgressFailure(id) && d.counted {
				wantLocation++
			}
		}
		if got := CountProviderEgress(ctx); !reflect.DeepEqual(got, wantCounts) {
			t.Fatal("dashboard changed its public cohort or decision counts")
		}
		if err := UpdateClientLocations(ctx, time.Hour); err != nil {
			t.Fatal(err)
		}
		locations, err := loadClientLocations(ctx, map[server.Id]bool{country: true})
		if err != nil || locations[country] == nil || locations[country].ClientCount != wantLocation {
			t.Fatalf("location count mismatch: expected=%d err=%v", wantLocation, err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			m, err := r.SMIsMember(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember, providerHardExclusionsCohortReadyMember, providerHardExclusionsCheckedMember(cohortTestId(1)), providerHardExclusionsCheckedMember(cohortTestId(6)), providerHardExclusionsCheckedMember(cohortTestId(7))).Result()
			if err != nil || !reflect.DeepEqual(m, []bool{false, true, true, false, false}) {
				t.Fatal("publication falsely claimed complete or unrelated coverage")
			}
			ttl, err := r.TTL(ctx, providerHardExclusionsKey).Result()
			if err != nil || ttl <= 0 || ttl > time.Hour {
				t.Fatal("publication TTL changed")
			}
		})
		t.Log("cohort control: dashboard/location parity; public-only publication; atomic version/coverage/TTL")

		// Simulate clients first visible in the second query without timing a
		// race. This local view hides only those two rows from the first exact
		// production statement; both remain visible to its group and fact reads.
		cohortTestExec(ctx, `ALTER TABLE network_client_location_reliability RENAME TO cohort_fixture_rollup;
CREATE VIEW network_client_location_reliability AS SELECT * FROM cohort_fixture_rollup WHERE client_id NOT IN ('00000000-0000-0000-0000-00000000000b','00000000-0000-0000-0000-00000000000c') OR current_query() NOT LIKE '%-- fix(beta): same class of issue as UpdateClientLocations above%';`)
		if err := UpdateClientScores(ctx, time.Hour, 2); err != nil {
			t.Fatal(err)
		}
		for _, isGroup := range []bool{false, true} {
			locationIds, groupIds := map[server.Id]bool{}, map[server.Id]bool{}
			if isGroup {
				groupIds[group] = true
			} else {
				locationIds[country] = true
			}
			got, err := loadClientScores(true, RankModeQuality, ctx, locationIds, groupIds, server.Id{}, 1000, []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only})
			if err != nil {
				t.Fatal(err)
			}
			wanted := map[server.Id]bool{cohortTestId(1): true, cohortTestId(5): true, cohortTestId(6): true, cohortTestId(10): true}
			if isGroup {
				wanted[cohortTestId(11)] = true
			}
			if len(got) != len(wanted) {
				t.Fatalf("score cohort size: group=%t got=%d want=%d", isGroup, len(got), len(wanted))
			}
			for id, score := range got {
				if !wanted[id] || score.NetworkOnly != (id == cohortTestId(6)) {
					t.Fatal("score lost network-only or admitted a common failure")
				}
				if id == cohortTestId(10) && (score.LookbackIndex != 0 || score.ReliabilityWeight != 1) {
					t.Fatal("missing reliability history lost neutral score")
				}
			}
		}
		census, err := GetClientScoreNativeCensus(ctx)
		if err != nil || census == nil || census.EgressRatio == nil || census.EgressRatio.SourceMap == nil || census.EgressRatio.SourceMap.Providers != len(fleet.healthCounts) {
			t.Fatal("native SourceMap diagnostic population changed")
		}
		t.Log("cohort control: both score populations; late group client checked; failure omitted; neutral/network-only/fanout/source-map preserved")
		if pgss {
			var calls, rows, unscoped int64
			server.Db(ctx, func(c server.PgConn) {
				server.Raise(c.QueryRow(ctx, `SELECT coalesce(sum(calls),0)::bigint,coalesce(sum(rows),0)::bigint,coalesce(sum(calls) FILTER(WHERE query NOT LIKE '%ANY($1)%'),0)::bigint FROM pg_stat_statements WHERE query LIKE 'WITH failed_reliability%'`).Scan(&calls, &rows, &unscoped))
			})
			if calls != 3 || rows > 36 || unscoped != 0 {
				t.Fatalf("bulk filter escaped cohort: calls=%d rows=%d unscoped=%d", calls, rows, unscoped)
			}
			t.Logf("cohort work: background_rollups=250000 dense_exceptions_over=50000 calls=%d returned_rows=%d unscoped_calls=%d", calls, rows, unscoped)
		}
		cohortTestExec(ctx, `ALTER TABLE provider_egress_url_security RENAME TO unavailable_url_security`)
		refused := func() (refused bool) {
			defer func() { refused = recover() != nil }()
			CountProviderEgress(ctx)
			return false
		}()
		if !refused {
			t.Fatal("evidence query failure became an empty allowed map")
		}
		t.Log("cohort control: evidence query errors propagate")
	})
}

func cohortTestPlanWork(ctx context.Context, t testing.TB) {
	t.Helper()
	server.Db(ctx, func(c server.PgConn) {
		server.RaisePgResult(c.Exec(ctx, `PREPARE cohort_filter(uuid[]) AS `+providerCountFilterClientSql()))
		defer func() { server.RaisePgResult(c.Exec(ctx, `DEALLOCATE cohort_filter`)) }()
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			server.RaisePgResult(c.Exec(ctx, "SET plan_cache_mode="+mode))
			defer func() { server.RaisePgResult(c.Exec(ctx, "RESET plan_cache_mode")) }()
			for _, count := range []int{1, 256, 1024} {
				ids := make([]string, count)
				for i := range ids {
					ids[i] = fmt.Sprintf("'%s'::uuid", cohortTestId(10000+i))
				}
				var raw []byte
				server.Raise(c.QueryRow(ctx, "EXPLAIN(ANALYZE,BUFFERS,FORMAT JSON) EXECUTE cohort_filter(ARRAY["+strings.Join(ids, ",")+"])").Scan(&raw))
				var plans []struct{ Plan hardExclusionPlanNode }
				server.Raise(json.Unmarshal(raw, &plans))
				var visited, rollupVisited float64
				var visit func(hardExclusionPlanNode)
				visit = func(p hardExclusionPlanNode) {
					if p.Relation != "" {
						visited += (p.Rows + p.Removed) * p.Loops
						if p.Relation == "network_client_location_reliability" {
							rollupVisited += (p.Rows + p.Removed) * p.Loops
						}
					}
					for _, child := range p.Plans {
						visit(child)
					}
				}
				visit(plans[0].Plan)
				if plans[0].Plan.Rows > float64(2*count) || rollupVisited > float64(2*count) || visited > float64(6*count) {
					t.Fatalf("scoped query visited unrelated history: mode=%s clients=%d rows=%.0f visited=%.0f", mode, count, plans[0].Plan.Rows, visited)
				}
				t.Logf("cohort plan: mode=%s clients=%d returned=%.0f visited=%.0f rollup_visited=%.0f", mode, count, plans[0].Plan.Rows, visited, rollupVisited)
			}
		}
	})
}

// Synchronous driver barriers inspect both sides of one real Redis transaction.
// Contradictory generations expose mixed coverage/exclusion reads without sleeps.
type cohortPublicationHook struct {
	beforeCommit func()
	afterCommit  func()
	transactions int
}

func (self *cohortPublicationHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *cohortPublicationHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (self *cohortPublicationHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		names := make([]string, len(commands))
		for i, command := range commands {
			names[i] = command.Name()
		}
		if !reflect.DeepEqual(names, []string{"multi", "del", "sadd", "expire", "exec"}) {
			return fmt.Errorf("cohort publication lost its single ordered transaction: %v", names)
		}
		self.beforeCommit()
		if err := next(ctx, commands); err != nil {
			return err
		}
		self.transactions++
		self.afterCommit()
		return nil
	}
}

func TestProviderCountCohortCacheCoverageAndFallback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		known, allowed, outside := server.NewId(), server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) { testingHardExclusionQueryTables(t, tx, false) }, server.OptNoRetry())
		cohortTestExec(ctx, `INSERT INTO network_client_location_reliability VALUES($1,true)`, outside)
		f := providerCountFilter{arinRisk: map[server.Id]bool{known: true, outside: true}}
		if err := publishProviderHardExclusions(ctx, []server.Id{known, allowed, known}, f, time.Hour); err != nil {
			t.Fatal(err)
		}
		got, err := getProviderHardExclusions(ctx, []server.Id{known, allowed, outside})
		if err != nil || len(got) != 2 || !got[known] || !got[outside] || got[allowed] {
			t.Fatal("uncovered historical failure was treated as absent/allowed")
		}
		// Exercise the v2 reader's exact decision boundary against the new
		// publication: no completeness marker means read all requested ids.
		var oldComplete bool
		server.Redis(ctx, func(r server.RedisClient) {
			members, err := r.SMIsMember(ctx, providerHardExclusionsKey, "ready:v2", known.String(), outside.String()).Result()
			server.Raise(err)
			oldComplete = members[0]
		})
		if oldComplete {
			t.Fatal("new cohort claimed complete coverage to a rolling old reader")
		}
		got, err = readProviderHardExclusions(ctx, []server.Id{known, outside})
		if err != nil || len(got) != 1 || !got[outside] {
			t.Fatal("old reader fallback lost the failure outside the new cohort")
		}
		// Checked answers need no SQL; an unknown candidate must still fail on
		// missing storage, even alongside successfully cached answers.
		cohortTestExec(ctx, `ALTER TABLE provider_egress_url_security RENAME TO unavailable_url_security`)
		got, err = getProviderHardExclusions(ctx, []server.Id{known, allowed})
		if err != nil || len(got) != 1 || !got[known] {
			t.Fatal("checked cache answers unnecessarily read SQL")
		}
		got, err = getProviderHardExclusions(ctx, []server.Id{known, outside})
		if err == nil || got != nil {
			t.Fatal("unknown coverage SQL failure returned a partial allowed result")
		}
		cohortTestExec(ctx, `ALTER TABLE unavailable_url_security RENAME TO provider_egress_url_security`)
		t.Log("cohort control: checked positive/negative; disconnected/history fallback; fallback error stays error")
		for _, state := range []string{"v2", "missing", "expired", "mixed", "unchecked-exclusion"} {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Del(ctx, providerHardExclusionsKey).Err())
				switch state {
				case "v2":
					server.Raise(r.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember, outside.String()).Err())
				case "expired":
					server.Raise(r.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsCohortReadyMember).Err())
					server.Raise(r.Expire(ctx, providerHardExclusionsKey, -time.Second).Err())
				case "mixed":
					server.Raise(r.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsReadyMember, providerHardExclusionsCohortReadyMember, providerHardExclusionsCheckedMember(outside)).Err())
				case "unchecked-exclusion":
					server.Raise(r.SAdd(ctx, providerHardExclusionsKey, providerHardExclusionsCohortReadyMember, allowed.String()).Err())
				}
			})
			got, err = getProviderHardExclusions(ctx, []server.Id{outside, allowed, outside})
			if err != nil || len(got) != 1 || !got[outside] || got[allowed] {
				t.Fatalf("cache state %s changed safety: err=%v count=%d", state, err, len(got))
			}
		}
		t.Log("cohort control: rolling v2; missing/expired/mixed markers; unchecked entry cannot decide")
		if err := publishProviderHardExclusions(ctx, nil, providerCountFilter{}, time.Hour); err != nil {
			t.Fatal(err)
		}
		ids := make([]server.Id, 530)
		for i := range ids {
			ids[i] = server.NewId()
		}
		cohortTestExec(ctx, `INSERT INTO network_client_location_reliability VALUES($1,true),($2,true)`, ids[0], ids[529])
		got, err = getProviderHardExclusions(ctx, append(ids, ids[0]))
		if err != nil || len(got) != 2 || !got[ids[0]] || !got[ids[529]] {
			t.Fatal("empty cohort or multi-chunk fallback lost tail candidates")
		}
		if err := publishProviderHardExclusions(ctx, []server.Id{known}, f, time.Hour); err != nil {
			t.Fatal(err)
		}
		hook := &cohortPublicationHook{
			beforeCommit: func() {
				got, err := getProviderHardExclusions(ctx, []server.Id{known, allowed})
				if err != nil || len(got) != 1 || !got[known] {
					t.Fatal("pending publication replaced part of the old generation")
				}
			},
			afterCommit: func() {
				got, err := getProviderHardExclusions(ctx, []server.Id{known, allowed})
				if err != nil || len(got) != 1 || !got[allowed] {
					t.Fatal("committed publication retained part of the old generation")
				}
			},
		}
		server.Redis(ctx, func(r server.RedisClient) { r.AddHook(hook) })
		if err := publishProviderHardExclusions(ctx, []server.Id{allowed}, providerCountFilter{arinRisk: map[server.Id]bool{allowed: true}}, time.Hour); err != nil {
			t.Fatal(err)
		}
		if hook.transactions != 1 {
			t.Fatal("publication did not cross the real transaction boundary")
		}
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, providerHardExclusionsKey).Err())
			server.Raise(r.Set(ctx, providerHardExclusionsKey, "wrong-type", time.Hour).Err())
		})
		if got, err := getProviderHardExclusions(ctx, []server.Id{known}); err == nil || got != nil {
			t.Fatal("Redis error became empty evidence")
		}
		t.Log("cohort control: empty publication; 530 candidates/three SQL chunks; atomic contradictory generations; Redis errors")
	})
}
