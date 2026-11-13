package model

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Fixed predecessor query: this test compares the production statement to an
// independent retained relational shape rather than rewriting the new SQL.
const scoreSourceProductSQL = `
	            SELECT
	            	location_group_member_city.location_group_id AS city_location_group_id,
	            	location_group_member_region.location_group_id AS region_location_group_id,
	            	location_group_member_country.location_group_id AS country_location_group_id,
	                network_client_location_reliability.client_id,
	                network_client_location_reliability.network_id,
	                network_client_location_reliability.max_net_type_score,
	                network_client_location_reliability.max_net_type_score_speed,
	                network_client_location_reliability.min_relative_latency_ms,
		            network_client_location_reliability.max_bytes_per_second,
		            network_client_location_reliability.has_latency_test,
		            network_client_location_reliability.has_speed_test,
		            COALESCE(client_connection_reliability_score.lookback_index, 0),  -- fix(beta): see LEFT JOIN comment below
	                COALESCE(client_connection_reliability_score.reliability_weight, 1),
	                COALESCE(client_connection_reliability_score.independent_reliability_weight, 1),
	                -- publicly usable; see the per-location query above
	                EXISTS (
	                	SELECT 1 FROM provide_key
	                	WHERE
	                		provide_key.client_id = network_client_location_reliability.client_id AND
	                		provide_key.provide_mode = $1
	                ),
	                COALESCE(provider_egress_health.reputation_failed_names, ''),
	                network_client_location_reliability.ipv4_proven,
	                network_client_location_reliability.ipv6_proven,
	                -- the egress index and the published country; see the
	                -- per-location query above
	                network_client_location_reliability.egress_index,
	                network_client_location_reliability.egress_quality,
	                country_location.country_code

	            FROM network_client_location_reliability

	            INNER JOIN network_client ON
	                network_client.client_id = network_client_location_reliability.client_id

	            LEFT JOIN location AS country_location ON
	                country_location.location_id = network_client_location_reliability.country_location_id

	            -- fix(beta): same class of issue as UpdateClientLocations/the query
            -- above this one -- treats an unscored client as neutral rather
            -- than excluding it, since the reliability-scoring pipeline may
            -- never populate at this env's small/cold-start scale
	            LEFT JOIN client_connection_reliability_score ON
	        		client_connection_reliability_score.client_id = network_client_location_reliability.client_id
	            LEFT JOIN provider_egress_health ON
	                    provider_egress_health.client_id = network_client_location_reliability.client_id AND
	                    provider_egress_health.measured_at >= $3

	            LEFT JOIN location_group_member location_group_member_city ON
	                location_group_member_city.location_id = network_client_location_reliability.city_location_id

	            LEFT JOIN location_group_member location_group_member_region ON
	                location_group_member_region.location_id = network_client_location_reliability.region_location_id

	            LEFT JOIN location_group_member location_group_member_country ON
	                location_group_member_country.location_id = network_client_location_reliability.country_location_id

	            WHERE
	                network_client.active = true AND
	                network_client.source_client_id IS NULL AND
	            	network_client_location_reliability.connected = true AND
	            	network_client_location_reliability.valid = true AND
	            	-- same rule as the per-location query above: Public or
	            	-- Network. This one fills locationGroupClientScores -> the
	            	-- clientScoreLocationGroup* redis keys -> loadClientScores
	            	-- -> FindProviders2 whenever a spec carries a
	            	-- LocationGroupId, so a user who picks a promoted group
	            	-- (e.g. "Strong Privacy Laws") must be filtered by the same
	            	-- request-time network check as a plain location.
	            	EXISTS (
	            		SELECT 1 FROM provide_key
	            		WHERE
	            			provide_key.client_id = network_client_location_reliability.client_id AND
	            			provide_key.provide_mode IN ($1, $2)
	            	)
	        `

type scoreSourceKey struct {
	Client   server.Id
	Lookback int
}
type scoreSourceValue struct {
	Network                     server.Id
	NetType, SpeedType, Latency int
	Bytes                       ByteCount
	HasLatency, HasSpeed        bool
	Reliability, Independent    float64
	Public                      bool
	Reputation                  string
	IPv4, IPv6                  bool
	EgressIndex                 *int
	EgressQuality               *bool
	Country                     *string
}
type scoreSourceCapture struct {
	Values  map[scoreSourceKey]scoreSourceValue
	Groups  map[scoreSourceKey]map[server.Id]bool
	Rows    int
	Elapsed time.Duration
}

func scoreSourceRead(ctx context.Context, query string, aggregate bool) scoreSourceCapture {
	capture := scoreSourceCapture{Values: map[scoreSourceKey]scoreSourceValue{}, Groups: map[scoreSourceKey]map[server.Id]bool{}}
	started := time.Now()
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, query, ProvideModePublic, ProvideModeNetwork, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var k scoreSourceKey
				var v scoreSourceValue
				var groups [3][]server.Id
				var old [3]*server.Id
				var target [3]any
				for i := range target {
					if aggregate {
						target[i] = &groups[i]
					} else {
						target[i] = &old[i]
					}
				}
				server.Raise(rows.Scan(target[0], target[1], target[2], &k.Client, &v.Network, &v.NetType, &v.SpeedType, &v.Latency, &v.Bytes, &v.HasLatency, &v.HasSpeed, &k.Lookback, &v.Reliability, &v.Independent, &v.Public, &v.Reputation, &v.IPv4, &v.IPv6, &v.EgressIndex, &v.EgressQuality, &v.Country))
				if prior, ok := capture.Values[k]; ok && !reflect.DeepEqual(prior, v) {
					panic("fixture has conflicting provider/lookback score values")
				}
				capture.Values[k] = v
				if capture.Groups[k] == nil {
					capture.Groups[k] = map[server.Id]bool{}
				}
				var memberships []server.Id
				if aggregate {
					memberships = clientScoreDistinctGroupIds(groups[:]...)
				} else {
					memberships = distinctIds(old[:]...)
				}
				for _, id := range memberships {
					capture.Groups[k][id] = true
				}
				capture.Rows++
			}
		})
	})
	capture.Elapsed = time.Since(started)
	return capture
}

func TestClientScoreGroupSourceLoadedParity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		cohortTestTables(ctx)
		cohortTestExec(ctx, `TRUNCATE network_client_location_reliability,client_connection_reliability_score;
INSERT INTO location SELECT lpad(to_hex(1000000+n),32,'0')::uuid,CASE n WHEN 1 THEN 'region' WHEN 2 THEN 'city' ELSE 'country' END,'Synthetic',NULL,NULL,NULL,'us' FROM generate_series(0,3)n;
INSERT INTO location_group SELECT lpad(to_hex(2000000+n),32,'0')::uuid,'Synthetic',true FROM generate_series(0,3)n;
INSERT INTO location_group_member SELECT lpad(to_hex(2000000),32,'0')::uuid,lpad(to_hex(1000000+n),32,'0')::uuid FROM generate_series(0,2)n;
INSERT INTO location_group_member SELECT lpad(to_hex(2000001+n),32,'0')::uuid,lpad(to_hex(1000000+n),32,'0')::uuid FROM generate_series(0,2)n;
INSERT INTO network_client SELECT lpad(to_hex(n),32,'0')::uuid,true,NULL FROM generate_series(1,86499)n;
INSERT INTO network_client SELECT lpad(to_hex(900000+n),32,'0')::uuid,n<>1,CASE WHEN n=2 THEN lpad(to_hex(999999),32,'0')::uuid END FROM generate_series(1,5)n;

INSERT INTO network_client_location_reliability(client_id,connected,valid,city_location_id,region_location_id,country_location_id,egress_index,egress_quality,arin_risk,arin_non_quality)
SELECT lpad(to_hex(n),32,'0')::uuid,true,true,
 lpad(to_hex(CASE WHEN n%11=0 THEN 1000003 WHEN n%13=0 THEN 1000000 ELSE 1000002 END),32,'0')::uuid,
 CASE WHEN n%17<>0 THEN lpad(to_hex(CASE WHEN n%11=0 THEN 1000003 WHEN n%13=0 THEN 1000000 ELSE 1000001 END),32,'0')::uuid END,
 lpad(to_hex(CASE WHEN n%11=0 THEN 1000003 ELSE 1000000 END),32,'0')::uuid,
 CASE WHEN n%2=0 THEN n%4 END,n%3<>0,n%41=0,n%43=0 FROM generate_series(1,86499)n;
INSERT INTO network_client_location_reliability(client_id,connected,valid,country_location_id) SELECT lpad(to_hex(900000+n),32,'0')::uuid,n<>3,n<>4,lpad(to_hex(1000000),32,'0')::uuid FROM generate_series(1,5)n;
INSERT INTO client_connection_reliability_score SELECT lpad(to_hex(n),32,'0')::uuid,l,1,CASE WHEN n%47=0 THEN .2 ELSE 1 END FROM generate_series(1,86499)n CROSS JOIN generate_series(1,3)l WHERE n%5<>0;
ANALYZE;`)
		cohortTestExec(ctx, `INSERT INTO provide_key SELECT lpad(to_hex(n),32,'0')::uuid,CASE WHEN n%7=0 THEN $2::int ELSE $1::int END FROM generate_series(1,86499)n;`, ProvideModePublic, ProvideModeNetwork)
		cohortTestExec(ctx, `INSERT INTO provide_key SELECT lpad(to_hex(900000+n),32,'0')::uuid,CASE WHEN n=5 THEN $2::int ELSE $1::int END FROM generate_series(1,5)n;`, ProvideModePublic, ProvideModeStream)
		cohortTestExec(ctx, `ANALYZE provide_key;`)
		before := scoreSourceRead(ctx, scoreSourceProductSQL, false)
		after := scoreSourceRead(ctx, clientScoreLocationGroupSourceSQL, true)
		if !reflect.DeepEqual(before.Values, after.Values) || !reflect.DeepEqual(before.Groups, after.Groups) {
			t.Fatal("source aggregation changed cohort, fields, lookbacks, or group memberships")
		}
		if after.Rows != len(after.Values) || before.Rows <= 4*after.Rows {
			t.Fatal("fixture failed to discriminate the Cartesian source-row amplification")
		}
		noGroup, networkOnly, neutral := 0, 0, 0
		clients := map[server.Id]bool{}
		for k, v := range after.Values {
			clients[k.Client] = true
			if len(after.Groups[k]) == 0 {
				noGroup++
			}
			if !v.Public {
				networkOnly++
			}
			if k.Lookback == 0 {
				neutral++
			}
		}
		if len(clients) != 86499 {
			t.Fatal("source population changed")
		}
		for n := 1; n <= 5; n++ {
			if clients[cohortTestId(900000+n)] {
				t.Fatal("inactive, derived, disconnected, invalid, or stream-only client admitted")
			}
		}
		if noGroup == 0 || networkOnly == 0 || neutral == 0 {
			t.Fatal("fixture lost LEFT-join, network-only or unscored controls")
		}
		t.Logf("loaded source parity: synthetic_clients=86499 old_rows=%d new_rows=%d unique_provider_lookbacks=%d no_group=%d network_only=%d neutral=%d old_scan_ms=%d new_scan_ms=%d", before.Rows, after.Rows, len(after.Values), noGroup, networkOnly, neutral, before.Elapsed.Milliseconds(), after.Elapsed.Milliseconds())
		// Observe database work separately from client-side scan/allocation.
		for name, query := range map[string]string{"product": scoreSourceProductSQL, "aggregated": clientScoreLocationGroupSourceSQL} {
			server.Db(ctx, func(conn server.PgConn) {
				var raw []byte
				server.Raise(conn.QueryRow(ctx, "EXPLAIN(ANALYZE,BUFFERS,FORMAT JSON) "+query, ProvideModePublic, ProvideModeNetwork, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)).Scan(&raw))
				var plans []struct {
					Plan struct {
						Rows       float64 `json:"Actual Rows"`
						Loops      float64 `json:"Actual Loops"`
						SharedHit  float64 `json:"Shared Hit Blocks"`
						SharedRead float64 `json:"Shared Read Blocks"`
						TempRead   float64 `json:"Temp Read Blocks"`
						TempWrite  float64 `json:"Temp Written Blocks"`
					}
					Execution float64 `json:"Execution Time"`
				}
				server.Raise(json.Unmarshal(raw, &plans))
				if len(plans) != 1 {
					t.Fatal("missing plan")
				}
				p := plans[0]
				t.Logf("loaded source plan: shape=%s rows=%.0f execution_ms=%.3f shared_hit=%.0f shared_read=%.0f temp_read=%.0f temp_write=%.0f", name, p.Plan.Rows*p.Plan.Loops, p.Execution, p.Plan.SharedHit, p.Plan.SharedRead, p.Plan.TempRead, p.Plan.TempWrite)
			})
		}
	})
}
