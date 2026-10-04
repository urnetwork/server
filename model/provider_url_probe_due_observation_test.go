package model

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestUrlDueObservationNilPanicAndRepeatedAttempts(t *testing.T) {
	var absent *ProviderUrlProbeDueObservation
	calls := 0
	absent.measure(ProviderUrlProbeDueClaimBody, func() { calls++ })
	if calls != 1 || absent.database(false) != nil || absent.database(true) != nil {
		t.Fatal("nil observation changed execution")
	}
	var observation ProviderUrlProbeDueObservation
	original := &struct{ marker int }{17}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		observation.measure(ProviderUrlProbeDueClaimBody, func() { panic(original) })
	}()
	observation.measure(ProviderUrlProbeDueClaimBody, func() { calls++ })
	if recovered != original || calls != 2 || observation.Phases[ProviderUrlProbeDueClaimBody].Count != 2 || observation.Phases[ProviderUrlProbeDueClaimBody].Duration <= 0 {
		t.Fatal("partial attempt or panic identity was lost")
	}
	// The exported request observation may carry only arrays, structs, and
	// numeric scalars; there is nowhere to retain an ID, SQL string, or error.
	var numeric func(reflect.Type)
	numeric = func(kind reflect.Type) {
		switch kind.Kind() {
		case reflect.Struct:
			for field := range kind.NumField() {
				numeric(kind.Field(field).Type)
			}
		case reflect.Array:
			numeric(kind.Elem())
		case reflect.Uint64, reflect.Int64:
		default:
			t.Fatalf("observation can carry nonnumeric data: %v", kind)
		}
	}
	numeric(reflect.TypeOf(observation))
}

func TestUrlDueObservationInvalidArgumentsHaveNoDatabasePhases(t *testing.T) {
	var observation ProviderUrlProbeDueObservation
	result := ClaimProviderUrlProbeDueWithObservation(t.Context(), time.Now(), 0, 0, 8, &observation)
	if len(result.Providers) != 0 || observation.Phases[ProviderUrlProbeDueModel].Count != 1 || observation.ClaimDatabase != (server.DbTiming{}) || observation.RetentionDatabase != (server.DbTiming{}) {
		t.Fatal("invalid arguments opened a transaction or lost model observation")
	}
	for _, sample := range observation.Phases[1:] {
		if sample != (server.DbTimingSample{}) {
			t.Fatal("invalid arguments fabricated model work")
		}
	}
}

// Restore the identical durable fixture before each path. The result and both
// tables must match, including retained old receipts and the new claim identity.
func TestUrlDueObservationPreservesDatabaseResults(t *testing.T) {
	for _, priority := range []bool{false, true} {
		name := "legacy_order"
		if priority {
			name = "completed_order"
		}
		t.Run(name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := t.Context()
				now := server.NowUtc().Truncate(time.Microsecond)
				since := now
				if priority {
					since = now.Add(-8 * time.Hour)
				}
				testingUrlCompletionPriority(t, since)
				clientId := testingUrlCompletionClients(t, now, 1)[0]
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET claim_ordinal=1 WHERE client_id=$1`, clientId))
				})
				var baseline, restoreColumns string
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT to_jsonb(cycle)::text FROM provider_egress_probe_cycle AS cycle WHERE client_id=$1`, clientId).Scan(&baseline))
					// Generated slots are recomputed from the identical client ID;
					// all writable columns restore their exact pre-claim values.
					server.Raise(conn.QueryRow(ctx, `SELECT string_agg(quote_ident(attname),',' ORDER BY attnum)
						FROM pg_attribute WHERE attrelid='provider_egress_probe_cycle'::regclass
						AND attnum>0 AND NOT attisdropped AND attgenerated=''`).Scan(&restoreColumns))
				})
				reset := func() {
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_url_probe_run WHERE client_id=$1`, clientId))
						server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId))
						server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`INSERT INTO provider_egress_probe_cycle(%[1]s)
							SELECT %[1]s FROM jsonb_populate_record(NULL::provider_egress_probe_cycle,$1::jsonb)`, restoreColumns), baseline))
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_url_probe_run(client_id,claim_ordinal,claimed_at) VALUES($1,1,$2)`, clientId, now.Add(-providerUrlProbeRunRetention-time.Hour)))
					})
				}
				snapshot := func() (cycle, runs string) {
					server.Db(ctx, func(conn server.PgConn) {
						server.Raise(conn.QueryRow(ctx, `SELECT to_jsonb(cycle)::text FROM provider_egress_probe_cycle AS cycle WHERE client_id=$1`, clientId).Scan(&cycle))
						server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(run) ORDER BY claim_ordinal),'[]'::jsonb)::text FROM provider_url_probe_run AS run WHERE client_id=$1`, clientId).Scan(&runs))
					})
					return
				}
				// Use the actual owner slot; one shard of eight must find this row.
				var shard int
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT slot_id%8 FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId).Scan(&shard))
				})
				reset()
				plain := ClaimProviderUrlProbeDueWithStatus(ctx, now, 1, shard, 8)
				plainCycle, plainRuns := snapshot()
				reset()
				var observation ProviderUrlProbeDueObservation
				observed := ClaimProviderUrlProbeDueWithObservation(ctx, now, 1, shard, 8, &observation)
				observedCycle, observedRuns := snapshot()
				if !reflect.DeepEqual(observed, plain) || observedCycle != plainCycle || observedRuns != plainRuns || len(observed.Providers) != 1 || observed.Providers[0].ClaimOrdinal != 2 {
					t.Fatal("observation changed durable selection, identity, or cleanup")
				}
				for phase, sample := range observation.Phases {
					want := uint64(1)
					if phase == int(ProviderUrlProbeDueRetentionTransaction) || phase == int(ProviderUrlProbeDueRetentionQuery) {
						want = 0
					}
					if !priority && (phase == int(ProviderUrlProbeDuePromote) || phase == int(ProviderUrlProbeDuePromotePending)) {
						want = 0
					}
					if sample.Count != want || (want > 0 && sample.Duration <= 0) || (want == 0 && sample.Duration != 0) {
						t.Fatalf("phase%d=%+v want count%d", phase, sample, want)
					}
				}
				if observation.RetentionDatabase != (server.DbTiming{}) {
					t.Fatal("admission acquired a retention transaction")
				}
				for _, database := range []server.DbTiming{observation.ClaimDatabase} {
					for phase, sample := range database.Phases {
						want := uint64(1)
						if phase == int(server.DbTimingRollback) || phase == int(server.DbTimingRetryWait) {
							want = 0
						}
						if sample.Count != want {
							t.Fatalf("database phase%d=%+v want count%d", phase, sample, want)
						}
					}
				}
				if observation.Phases[ProviderUrlProbeDueModel].Duration < observation.Phases[ProviderUrlProbeDueClaimTransaction].Duration+observation.Phases[ProviderUrlProbeDueRetentionTransaction].Duration || observation.Phases[ProviderUrlProbeDueClaimTransaction].Duration < observation.Phases[ProviderUrlProbeDueClaimBody].Duration {
					t.Fatal("inclusive phase timing lost its parent boundary")
				}
			})
		})
	}
}

func TestUrlDueObservationPendingMaintenanceSkipsClaimAndStorageCleanup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		testingUrlCompletionPriority(t, now.Add(-8*time.Hour))
		clientId := testingUrlCompletionClients(t, now.Add(-5*time.Hour), 1)[0]
		first := ClaimProviderUrlProbeDue(ctx, now.Add(-5*time.Hour), 1, 0, 1)[0]
		testingCompleteUrlClaim(t, ctx, first, now.Add(-5*time.Hour+time.Second), "tunnel_failed")
		var observation ProviderUrlProbeDueObservation
		server.Tx(ctx, func(tx server.PgTx) {
			var locked server.Id
			server.Raise(tx.QueryRow(ctx, `SELECT client_id FROM provider_egress_probe_cycle WHERE client_id=$1 FOR UPDATE`, clientId).Scan(&locked))
			result := ClaimProviderUrlProbeDueWithObservation(ctx, now, 1, 0, 1, &observation)
			if !result.PriorityMaintenancePending || len(result.Providers) != 0 {
				t.Fatal("locked maintenance no longer prevents admission")
			}
		})
		for _, phase := range []ProviderUrlProbeDuePhase{ProviderUrlProbeDueExpiry, ProviderUrlProbeDueExpiryPending} {
			if observation.Phases[phase].Count != 1 {
				t.Fatalf("pending path lost phase%d", phase)
			}
		}
		for _, phase := range []ProviderUrlProbeDuePhase{ProviderUrlProbeDuePromote, ProviderUrlProbeDuePromotePending, ProviderUrlProbeDueClaimQueryRows, ProviderUrlProbeDueRetentionTransaction, ProviderUrlProbeDueRetentionQuery} {
			if observation.Phases[phase].Count != 0 {
				t.Fatalf("pending path fabricated phase%d", phase)
			}
		}
	})
}
