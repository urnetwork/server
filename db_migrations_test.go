package server

import (
	"context"
	"strings"
	"testing"
	"time"
	// "github.com/urnetwork/connect"
)

func sqlMigrationIndex(t testing.TB, marker string) int {
	t.Helper()
	for i, migration := range migrations {
		if sqlMigration, ok := migration.(*SqlMigration); ok &&
			strings.Contains(sqlMigration.sql, marker) {
			return i
		}
	}
	t.Fatalf("SQL migration containing %q not found", marker)
	return -1
}

func locationNameBackfillMigrationIndex(t testing.TB) int {
	return sqlMigrationIndex(t, "iso_country_name_backfill")
}

func canceledCircleRetryRestoreMigrationIndex(t testing.TB) int {
	return sqlMigrationIndex(t, "restore_canceled_circle_retries")
}

func accountPaymentContractRetentionMigrationIndex(t testing.TB) int {
	return sqlMigrationIndex(t, "account_payment_contract_retention_queue")
}

// Competition migrations were developed against an older main. They must
// remain a contiguous suffix so every migration integrated from origin runs
// first and retains its published version number.
func TestCompetitionMigrationsFollowOriginMigrations(t *testing.T) {
	markers := []string{
		"CREATE TABLE competition_round",
		"competition_append_only_guard",
		"competition_workload_backfill_guard",
		"competition_epoch_lifecycle_guard",
		"competition_image_identity_backfill_guard",
		"competition_candidate_review_gate",
	}
	firstCompetitionIndex := len(migrations) - len(markers)
	if firstCompetitionIndex <= accountPaymentContractRetentionMigrationIndex(t) {
		t.Fatalf("competition migration suffix starts at %d before latest origin migration", firstCompetitionIndex)
	}
	for i, marker := range markers {
		if index := sqlMigrationIndex(t, marker); index != firstCompetitionIndex+i {
			t.Fatalf("competition migration %q index = %d, want suffix index %d", marker, index, firstCompetitionIndex+i)
		}
	}
}

// A pending migration can race ahead of the runtime fix that stopped blank
// location names from being written. In that state a later blank region/city
// can coexist with an older canonical row, and normalizing both full names to
// the same value must not abort the migration.
func TestLocationNameBackfillHandlesCanonicalFullNameCollisions(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		migrationIndex := locationNameBackfillMigrationIndex(t)
		ApplyDbMigrationsUpTo(ctx, migrationIndex)

		countryId := NewId()
		canonicalRegionId := NewId()
		canonicalCityId := NewId()
		legacyRegionId := NewId()
		legacyDuplicateCityId := NewId()
		legacyUniqueCityId := NewId()

		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO location (
					location_id,
					location_type,
					location_name,
					city_location_id,
					region_location_id,
					country_location_id,
					country_code,
					location_full_name
				)
				VALUES
					($1, 'country', '', NULL, NULL, $1, 'sg', 'sg'),
					($2, 'region', 'Singapore', NULL, $2, $1, 'sg', 'Singapore, sg'),
					($3, 'city', 'Bedok', $3, $2, $1, 'sg', 'Bedok, Singapore, sg'),
					($4, 'region', '', NULL, $4, $1, 'sg', ', sg'),
					($5, 'city', 'Bedok', $5, $4, $1, 'sg', 'Bedok, , sg'),
					($6, 'city', 'Jurong', $6, $4, $1, 'sg', 'Jurong, , sg')
			`,
				countryId,
				canonicalRegionId,
				canonicalCityId,
				legacyRegionId,
				legacyDuplicateCityId,
				legacyUniqueCityId,
			))
		})

		ApplyDbMigrationsUpTo(ctx, migrationIndex+1)

		type locationState struct {
			name     string
			fullName string
		}
		states := map[Id]locationState{}
		blankNameCount := 0
		constraintExists := false
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT location_id, location_name, location_full_name
				FROM location
				WHERE location_id IN ($1, $2, $3, $4, $5, $6)
			`,
				countryId,
				canonicalRegionId,
				canonicalCityId,
				legacyRegionId,
				legacyDuplicateCityId,
				legacyUniqueCityId,
			)
			WithPgResult(result, err, func() {
				for result.Next() {
					var locationId Id
					var state locationState
					Raise(result.Scan(&locationId, &state.name, &state.fullName))
					states[locationId] = state
				}
			})

			result, err = conn.Query(ctx, `SELECT COUNT(*) FROM location WHERE location_name = ''`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&blankNameCount))
				}
			})

			result, err = conn.Query(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM pg_constraint
					WHERE
						conrelid = 'location'::regclass AND
						conname = 'location_name_not_blank'
				)
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&constraintExists))
				}
			})
		}, OptReadOnly())

		assertState := func(locationId Id, wantName string, wantFullName string) {
			t.Helper()
			got, ok := states[locationId]
			if !ok {
				t.Fatalf("location %s was removed", locationId)
			}
			if got.name != wantName || got.fullName != wantFullName {
				t.Fatalf(
					"location %s = (%q, %q), want (%q, %q)",
					locationId,
					got.name,
					got.fullName,
					wantName,
					wantFullName,
				)
			}
		}

		assertState(countryId, "Singapore", "sg")
		assertState(canonicalRegionId, "Singapore", "Singapore, sg")
		assertState(canonicalCityId, "Bedok", "Bedok, Singapore, sg")
		assertState(legacyRegionId, "Singapore", ", sg")
		assertState(legacyDuplicateCityId, "Bedok", "Bedok, , sg")
		assertState(legacyUniqueCityId, "Jurong", "Jurong, Singapore, sg")

		if blankNameCount != 0 {
			t.Fatalf("blank location names after migration = %d, want 0", blankNameCount)
		}
		if !constraintExists {
			t.Fatal("location_name_not_blank constraint was not added")
		}
		if version := DbVersion(ctx); version != migrationIndex+1 {
			t.Fatalf("DB version = %d, want %d", version, migrationIndex+1)
		}
	})
}

// The one-time repair must be conservative: an incorrectly canceled Circle
// retry is resumed only while its sweeps still prove ownership by pointing to
// that payment. A retry whose sweeps were already reassigned is left for manual
// reconciliation; recent and ordinary unsubmitted cancellations stay canceled.
func TestCanceledCircleRetryRestoreMigration(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		migrationIndex := canceledCircleRetryRestoreMigrationIndex(t)
		ApplyDbMigrationsUpTo(ctx, migrationIndex)

		attachedRetryId := NewId()
		reassignedRetryId := NewId()
		recentRetryId := NewId()
		replacementPaymentId := NewId()
		unsubmittedCanceledId := NewId()
		paymentPlanId := NewId()
		networkId := NewId()
		cancelTime := NowUtc()
		createTime := cancelTime.Add(-30*24*time.Hour - time.Hour)
		attachedRecord := "circle-attached"
		reassignedRecord := "circle-reassigned"
		recentRecord := "circle-recent"
		reassignedKey := NewId()

		MaintenanceTx(ctx, func(tx PgTx) {
			for _, payment := range []struct {
				id         Id
				record     *string
				key        *Id
				createTime time.Time
			}{
				{
					id:         attachedRetryId,
					record:     &attachedRecord,
					createTime: createTime,
				},
				{
					id:         reassignedRetryId,
					record:     &reassignedRecord,
					key:        &reassignedKey,
					createTime: createTime,
				},
				{
					id:         recentRetryId,
					record:     &recentRecord,
					createTime: cancelTime.Add(-time.Hour),
				},
				{id: unsubmittedCanceledId, createTime: createTime},
			} {
				RaisePgResult(tx.Exec(ctx, `
					INSERT INTO account_payment (
						payment_id,
						payment_plan_id,
						network_id,
						wallet_id,
						payout_byte_count,
						payout_nano_cents,
						min_sweep_time,
						create_time,
						payment_record,
						circle_idempotency_key,
						canceled,
						cancel_time
					) VALUES ($1, $2, $3, NULL, 100, 100, $4, $5, $6, $7, true, $8)
				`,
					payment.id,
					paymentPlanId,
					networkId,
					payment.createTime,
					payment.createTime,
					payment.record,
					payment.key,
					cancelTime,
				))
			}
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO account_payment (
					payment_id,
					payment_plan_id,
					network_id,
					wallet_id,
					payout_byte_count,
					payout_nano_cents,
					min_sweep_time
				) VALUES ($1, $2, $3, NULL, 100, 100, $4)
			`, replacementPaymentId, paymentPlanId, networkId, NowUtc()))

			// The reassigned retry's former sweep now points to a replacement
			// payment, exactly as the payout planner would leave it.
			for _, paymentId := range []Id{
				attachedRetryId,
				recentRetryId,
				replacementPaymentId,
				unsubmittedCanceledId,
			} {
				RaisePgResult(tx.Exec(ctx, `
					INSERT INTO transfer_escrow_sweep (
						contract_id,
						balance_id,
						network_id,
						payout_byte_count,
						payout_net_revenue_nano_cents,
						payment_id
					) VALUES ($1, $2, $3, 100, 100, $4)
				`, NewId(), NewId(), networkId, paymentId))
			}
		})

		ApplyDbMigrationsUpTo(ctx, migrationIndex+1)

		type paymentState struct {
			canceled   bool
			cancelTime *time.Time
		}
		states := map[Id]paymentState{}
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT payment_id, canceled, cancel_time
				FROM account_payment
				WHERE payment_id IN ($1, $2, $3, $4)
			`, attachedRetryId, reassignedRetryId, recentRetryId, unsubmittedCanceledId)
			WithPgResult(result, err, func() {
				for result.Next() {
					var paymentId Id
					var state paymentState
					Raise(result.Scan(&paymentId, &state.canceled, &state.cancelTime))
					states[paymentId] = state
				}
			})
		}, OptReadOnly())

		if len(states) != 4 {
			t.Fatalf("loaded %d payment states, want 4", len(states))
		}
		if state := states[attachedRetryId]; state.canceled || state.cancelTime != nil {
			t.Fatalf("attached retry was not restored: %+v", state)
		}
		for _, paymentId := range []Id{reassignedRetryId, recentRetryId, unsubmittedCanceledId} {
			state := states[paymentId]
			if !state.canceled || state.cancelTime == nil {
				t.Errorf("payment %s was unsafely restored: %+v", paymentId, state)
			}
		}
		if version := DbVersion(ctx); version != migrationIndex+1 {
			t.Fatalf("DB version = %d, want %d", version, migrationIndex+1)
		}
	})
}

// Existing payments completed by the synchronous pre-deploy path normally
// already have reap_time. Queue only the recent edge defensively so a deadline
// inherited from the former straggler rule cannot shorten the seven-day
// post-completion window; older history remains a one-time backfill concern.
func TestAccountPaymentContractRetentionMigration(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		migrationIndex := accountPaymentContractRetentionMigrationIndex(t)
		ApplyDbMigrationsUpTo(ctx, migrationIndex)

		recentPaymentId := NewId()
		oldPaymentId := NewId()
		networkId := NewId()
		paymentPlanId := NewId()
		now := NowUtc()
		MaintenanceTx(ctx, func(tx PgTx) {
			for _, payment := range []struct {
				id           Id
				completeTime time.Time
			}{
				{id: recentPaymentId, completeTime: now.Add(-24 * time.Hour)},
				{id: oldPaymentId, completeTime: now.Add(-8 * 24 * time.Hour)},
			} {
				RaisePgResult(tx.Exec(ctx, `
					INSERT INTO account_payment (
						payment_id,
						payment_plan_id,
						network_id,
						payout_byte_count,
						payout_nano_cents,
						min_sweep_time,
						create_time,
						completed,
						complete_time
					)
					VALUES ($1, $2, $3, 1, 1, $4, $4, true, $4)
				`,
					payment.id,
					paymentPlanId,
					networkId,
					payment.completeTime,
				))
			}
		})

		ApplyDbMigrationsUpTo(ctx, migrationIndex+1)

		states := map[Id]struct {
			pending bool
			cursor  *Id
		}{}
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT payment_id, contract_retention_pending, contract_retention_cursor
				FROM account_payment
				WHERE payment_id IN ($1, $2)
			`, recentPaymentId, oldPaymentId)
			WithPgResult(result, err, func() {
				for result.Next() {
					var paymentId Id
					var state struct {
						pending bool
						cursor  *Id
					}
					Raise(result.Scan(&paymentId, &state.pending, &state.cursor))
					states[paymentId] = state
				}
			})
		}, OptReadOnly())

		if len(states) != 2 {
			t.Fatalf("loaded %d payment retention states, want 2", len(states))
		}
		if state := states[recentPaymentId]; !state.pending || state.cursor != nil {
			t.Fatalf("recent completed payment retention state = %+v, want pending with nil cursor", state)
		}
		if state := states[oldPaymentId]; state.pending || state.cursor != nil {
			t.Fatalf("old completed payment retention state = %+v, want converged", state)
		}
		if version := DbVersion(ctx); version != migrationIndex+1 {
			t.Fatalf("DB version = %d, want %d", version, migrationIndex+1)
		}
	})
}

// Net-escrow reconciliation now reads fresh bounded balance pages instead of
// one fleet-wide snapshot. This index is what keeps each page from rescanning
// the complete transfer_escrow history.
func TestNetEscrowReconcileBalanceIndex(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var indexDefinition string
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT indexdef
				FROM pg_indexes
				WHERE schemaname = current_schema()
				  AND indexname = 'transfer_escrow_balance_contract'
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&indexDefinition))
				}
			})
		}, OptReadOnly())
		if !strings.Contains(indexDefinition, "(balance_id, contract_id)") {
			t.Fatalf("transfer_escrow reconciliation index = %q", indexDefinition)
		}
	})
}

func TestApplyDbMigrations(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()

		ApplyDbMigrations(ctx)

		var reloptions []string
		var retiredIndexExists bool
		var idleTimeoutConfigured bool
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT coalesce(reloptions, ARRAY[]::text[])
				FROM pg_class
				WHERE oid = 'pending_task'::regclass
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&reloptions))
				}
			})

			result, err = conn.Query(ctx, `
				SELECT to_regclass('transfer_contract_open_source_id_companion_contract_id') IS NOT NULL
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&retiredIndexExists))
				}
			})

			result, err = conn.Query(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM pg_db_role_setting settings
					CROSS JOIN LATERAL unnest(settings.setconfig) config(value)
					WHERE settings.setdatabase = (SELECT oid FROM pg_database WHERE datname = current_database())
					  AND settings.setrole = 0
					  AND config.value = 'idle_in_transaction_session_timeout=5min'
				)
			`)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&idleTimeoutConfigured))
				}
			})
		}, OptReadWrite())

		options := strings.Join(reloptions, ",")
		for _, want := range []string{
			"autovacuum_vacuum_scale_factor=0",
			"autovacuum_vacuum_threshold=50",
			"autovacuum_vacuum_cost_delay=0",
			"autovacuum_analyze_scale_factor=0",
			"autovacuum_analyze_threshold=50",
		} {
			if !strings.Contains(options, want) {
				t.Fatalf("pending_task reloptions %q missing %q", options, want)
			}
		}
		if retiredIndexExists {
			t.Fatal("retired 99GB companion index still exists after head migrations")
		}
		if !idleTimeoutConfigured {
			t.Fatal("database idle-in-transaction timeout was not configured")
		}
	})
}
