package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

func locationProbeCycleSnapshot(ctx context.Context) map[server.Id]string {
	values := map[server.Id]string{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT client_id,to_jsonb(cycle)::text FROM provider_egress_probe_cycle AS cycle`)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				var value string
				server.Raise(rows.Scan(&id, &value))
				values[id] = value
			}
		})
	})
	return values
}

// A normal public-mode writer can commit while this location transaction is
// between preparation and its first permanent row write. Preserve both its
// admission and rejection rather than overwriting them with the older census.
func TestLocationProbeEligibilityPrecomputePreservesConcurrentProvide(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		_, admitted, _ := locationProbeConnectedFixture(t, ctx, 85)
		_, rejected, _ := locationProbeConnectedFixture(t, ctx, 86)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		SetProvide(ctx, admitted, map[ProvideMode][]byte{})
		pause := &locationProbeCensusPause{reached: make(chan struct{}), resume: make(chan struct{}),
			needles: []string{"INSERT INTO network_client_location_reliability"}}
		scope, err := server.NewTestPgQueryScope(ctx, pause)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		done := make(chan error, 1)
		go func() {
			done <- locationProbeCatch(func() {
				UpdateClientLocationReliabilities(context.WithValue(ctx, pause, true), now.Add(-time.Hour), now.Add(time.Minute))
			})
		}()
		select {
		case <-pause.reached:
		case err := <-done:
			t.Fatalf("location row-write boundary was not reached: %v", err)
		case <-ctx.Done():
			close(pause.resume)
			<-done
			t.Fatal("location row-write pause timed out")
		}
		changeErr := locationProbeCatch(func() {
			SetProvide(ctx, admitted, map[ProvideMode][]byte{ProvideModePublic: []byte("reentered-public")})
			SetProvide(ctx, rejected, map[ProvideMode][]byte{})
		})
		close(pause.resume)
		publisherErr := <-done
		if changeErr != nil || publisherErr != nil {
			t.Fatalf("intervening writer or publisher failed: change=%v publisher=%v", changeErr, publisherErr)
		}
		if pause.publishedRows != 0 {
			t.Fatal("prepared publication overwrote a newer direct eligibility change")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var admittedEligible, rejectedEligible bool
			server.Raise(conn.QueryRow(ctx, `SELECT eligible FROM provider_egress_probe_cycle WHERE client_id=$1`, admitted).Scan(&admittedEligible))
			server.Raise(conn.QueryRow(ctx, `SELECT eligible FROM provider_egress_probe_cycle WHERE client_id=$1`, rejected).Scan(&rejectedEligible))
			if !admittedEligible || rejectedEligible {
				t.Fatal("location publication replaced a committed public-mode admission or rejection")
			}
		})
	})
}

// Use actual connection aggregation, the generated validity column and the
// unchanged fleet reconciler as the result oracle. Existing cycle identity,
// future pacing, progress and claims must survive reactivation/rejection.
func TestLocationProbeEligibilityPrecomputeMatchesAuthoritativeRollup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		type fixture struct {
			network server.Id
			client  server.Id
			city    *Location
		}
		clients := map[string]fixture{}
		for index, name := range []string{"good", "new", "inactive", "child", "no_public", "risk", "failed", "grace", "ipv6", "duplicate", "disconnected", "missing_country"} {
			network, client, city := locationProbeConnectedFixture(t, ctx, 60+index)
			clients[name] = fixture{network: network, client: client, city: city}
		}
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, clients["inactive"].client))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id=$2 WHERE client_id=$1`, clients["child"].client, clients["good"].client))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provide_key WHERE client_id=$1`, clients["no_public"].client))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET arin_risk=true
				WHERE connection_id IN(SELECT connection_id FROM network_client_connection WHERE client_id=$1)`, clients["risk"].client))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE client_id=ANY($1::uuid[])`, []server.Id{clients["disconnected"].client, clients["ipv6"].client}, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_intent_probe_priority(client_id,priority_since,update_time) VALUES($1,$2,$2)`, clients["grace"].client, now.Add(-time.Hour)))
		})
		for _, name := range []string{"failed", "grace"} {
			x := clients[name]
			testingInsertProviderConnectionScore(ctx, x.client, x.city)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE client_connection_reliability_score SET lookback_index=1,independent_reliability_weight=0.1 WHERE client_id=$1`, x.client))
			})
		}
		for _, extra := range []struct {
			name, address string
			family        int
		}{{"ipv6", "[2001:db8::44]:20001", 6}, {"duplicate", "198.51.100.44:20001", 4}, {"missing_country", "203.0.113.44:20001", 4}} {
			x := clients[extra.name]
			city := x.city
			if extra.name == "missing_country" {
				// Conflicting real locations produce a NULL rollup country;
				// the connection-location source column itself is NOT NULL.
				city = &Location{LocationType: LocationTypeCity, City: "Other City", Region: "Other Region", Country: "Other Country", CountryCode: "zy"}
				CreateLocation(ctx, city)
			}
			connection, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, x.client, extra.address, CreateNetworkClientHandler(ctx), extra.family)
			if err != nil {
				t.Fatal(err)
			}
			if err := SetConnectionLocation(ctx, connection, city.LocationId, &ConnectionLocationScores{}); err != nil {
				t.Fatal(err)
			}
		}
		states := map[server.Id]string{}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`, clients["new"].client))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET eligible=false,
				cycle_started_at=$1,next_attempt_at=$2,success_count=2,error_count=3,outcome_count=7,claim_ordinal=9`, now.Add(-time.Hour), now.Add(time.Hour)))
			rows, err := tx.Query(ctx, `SELECT client_id,(to_jsonb(cycle)-'eligible'-'completed_priority_ready')::text FROM provider_egress_probe_cycle AS cycle`)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					var value string
					server.Raise(rows.Scan(&id, &value))
					states[id] = value
				}
			})
		})
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now.Add(time.Minute))
		for name, x := range clients {
			server.Db(ctx, func(conn server.PgConn) {
				var eligible bool
				var state string
				server.Raise(conn.QueryRow(ctx, `SELECT eligible,(to_jsonb(cycle)-'eligible'-'completed_priority_ready')::text
					FROM provider_egress_probe_cycle AS cycle WHERE client_id=$1`, x.client).Scan(&eligible, &state))
				if want := name == "good" || name == "new" || name == "grace"; eligible != want {
					t.Errorf("%s prospective admission differs from its authoritative inputs: eligible=%t", name, eligible)
				}
				if prior, present := states[x.client]; present && state != prior {
					t.Errorf("%s changed existing cycle identity, progress, claim or pacing", name)
				}
			})
		}
		before := locationProbeCycleSnapshot(ctx)
		server.Tx(ctx, func(tx server.PgTx) { updateProviderUrlProbeEligibility(ctx, tx) })
		after := locationProbeCycleSnapshot(ctx)
		if len(before) != len(after) {
			t.Fatal("the unchanged authoritative reconciler added or removed a missed cycle")
		}
		for id, state := range before {
			if after[id] != state {
				t.Fatal("prospective publication differs from actual generated-column/fleet reconciliation")
			}
		}
	})
}

// A late publication failure must not expose the newly rolled-up location or
// a new cycle. The ordinary retry then publishes both, with no lost seed.
func TestLocationProbeEligibilityPrecomputeKeepsAtomicRollback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		_, client, _ := locationProbeConnectedFixture(t, ctx, 81)
		now := server.NowUtc()
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		_, newClient, _ := locationProbeConnectedFixture(t, ctx, 82)
		before := locationProbeCycleSnapshot(ctx)
		var originalLocation string
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `SELECT to_jsonb(location)::text FROM network_client_location_reliability AS location WHERE client_id=$1`, client).Scan(&originalLocation))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET arin_risk=true
				WHERE connection_id IN(SELECT connection_id FROM network_client_connection WHERE client_id=$1)`, client))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION testing_location_probe_publish_failure() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN RAISE EXCEPTION 'synthetic location eligibility failure' USING ERRCODE='ZX117'; END $$`))
			server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER testing_location_probe_publish_failure
				BEFORE UPDATE ON provider_egress_probe_cycle FOR EACH ROW WHEN(NOT NEW.eligible)
				EXECUTE FUNCTION testing_location_probe_publish_failure()`))
		})
		failure := locationProbeCatch(func() { UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now.Add(time.Minute)) })
		var pg *pgconn.PgError
		if !errors.As(failure, &pg) || pg.Code != "ZX117" {
			t.Fatalf("the exact late publication failure did not occur: %v", failure)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP FUNCTION testing_location_probe_publish_failure() CASCADE`))
			var location string
			var newRows int
			server.Raise(tx.QueryRow(ctx, `SELECT to_jsonb(location)::text FROM network_client_location_reliability AS location WHERE client_id=$1`, client).Scan(&location))
			server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM network_client_location_reliability WHERE client_id=$1`, newClient).Scan(&newRows))
			if location != originalLocation || newRows != 0 {
				t.Fatal("failed hint publication exposed an independently committed location")
			}
		})
		after := locationProbeCycleSnapshot(ctx)
		if len(before) != len(after) {
			t.Fatal("failed publication exposed a new cycle")
		}
		for id, state := range before {
			if after[id] != state {
				t.Fatal("failed publication changed a previously committed cycle")
			}
		}
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now.Add(time.Minute))
		server.Db(ctx, func(conn server.PgConn) {
			var risk, eligible, newEligible bool
			server.Raise(conn.QueryRow(ctx, `SELECT location.arin_risk,cycle.eligible FROM network_client_location_reliability AS location
				JOIN provider_egress_probe_cycle AS cycle USING(client_id) WHERE location.client_id=$1`, client).Scan(&risk, &eligible))
			server.Raise(conn.QueryRow(ctx, `SELECT eligible FROM provider_egress_probe_cycle WHERE client_id=$1`, newClient).Scan(&newEligible))
			if !risk || eligible || !newEligible {
				t.Fatal("ordinary retry did not publish the exact risk rejection and new eligible cycle")
			}
		})
	})
}
