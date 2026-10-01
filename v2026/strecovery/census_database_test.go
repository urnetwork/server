// The isolated PostgreSQL harness verifies the real status-independent query,
// snapshot isolation and bounds. No configured operator database is contacted.
package strecovery

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Fixture writes occur only in the harness-owned database, before or across the
// explicit snapshot barrier. Production collection has no equivalent write port.
func censusInsertDatabaseFixture(t testing.TB, ctx context.Context, image *DatabaseSnapshot) {
	t.Helper()
	server.Db(ctx, func(connection server.PgConn) {
		for _, intent := range image.Intents {
			_, err := connection.Exec(ctx, `INSERT INTO st_transaction_intent (intent_id,intent_key,logical_key,generation,profile,deployment_id,deployment_key,
				chain_id,genesis_hash,from_address,to_address,calldata_hash,calldata,nonce,status,current_tx_hash,attempt_count,error,create_time,update_time)
				VALUES ($1::text::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
				intent.Id, intent.IntentKey, intent.LogicalKey, intent.Generation, intent.Profile, intent.DeploymentId, intent.DeploymentKey,
				intent.ChainId, intent.Genesis, intent.From, intent.To, intent.CalldataHash, intent.Calldata, intent.Nonce, intent.Status, intent.CurrentHash, intent.AttemptCount, intent.Error, intent.CreateTime, intent.UpdateTime)
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, attempt := range image.Attempts {
			_, err := connection.Exec(ctx, `INSERT INTO st_transaction_attempt (intent_id,attempt,kind,tx_hash,raw_transaction,gas_limit,gas_price,gas_tip_cap,gas_fee_cap,status,
				inclusion_block,inclusion_hash,finalized_block,finalized_hash,error,create_time,update_time)
				VALUES ($1::text::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
				attempt.IntentId, attempt.Number, attempt.Kind, attempt.Hash, attempt.Raw, attempt.GasLimit, attempt.GasPrice, attempt.GasTipCap, attempt.GasFeeCap, attempt.Status,
				attempt.InclusionBlock, attempt.InclusionHash, attempt.FinalizedBlock, attempt.FinalizedHash, attempt.Error, attempt.CreateTime, attempt.UpdateTime)
			if err != nil {
				t.Fatal(err)
			}
		}
	})
}

// The existing live query is the negative causal control: terminal rows disappear
// there but remain byte-identical in the separate full snapshot exporter.
func TestCensusDatabaseReadsTerminalStatusesAndEveryGeneration(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config, reader := censusTestFixture(t)
		censusInsertDatabaseFixture(t, ctx, reader.images["database-a"])
		censusInsertDatabaseFixture(t, ctx, reader.images["database-b"])
		if intents := model.GetUnresolvedStTransactionIntents(ctx, config.ChainId, config.Genesis, config.Roles[0].Address); len(intents) != 0 {
			t.Fatalf("causal active-only control found %d terminal intents", len(intents))
		}
		var before *DatabaseSnapshot
		server.Db(ctx, func(connection server.PgConn) {
			var err error
			before, err = readDatabaseSnapshot(ctx, connection.Conn(), config.Limits, nil)
			if err != nil {
				t.Fatal(err)
			}
		})
		if len(before.Intents) != 4 || len(before.Attempts) != 5 {
			t.Fatalf("full snapshot omitted custody: %d intents %d attempts", len(before.Intents), len(before.Attempts))
		}
		for _, image := range reader.images {
			for _, expected := range image.Attempts {
				found := false
				for _, actual := range before.Attempts {
					if expected.Hash == actual.Hash {
						found = true
						if objectDigest(expected) != objectDigest(actual) {
							t.Fatalf("snapshot changed signed attempt %s", expected.Hash)
						}
					}
				}
				if !found {
					t.Fatalf("missing original %s", expected.Hash)
				}
			}
		}
		server.Db(ctx, func(connection server.PgConn) {
			after, err := readDatabaseSnapshot(ctx, connection.Conn(), config.Limits, nil)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("read-only repeat changed durable state: %v", err)
			}
		})
	})
}

// Insertion after counts but before row scans forces the isolation boundary.
// The first read excludes it; a fresh completed snapshot includes it.
func TestCensusDatabaseUsesReadOnlyRepeatableReadSnapshot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config, reader := censusTestFixture(t)
		censusInsertDatabaseFixture(t, ctx, reader.images["database-a"])
		server.Db(ctx, func(connection server.PgConn) {
			before, err := readDatabaseSnapshot(ctx, connection.Conn(), config.Limits, func(tx pgx.Tx) error {
				var readOnly, isolation string
				if err := tx.QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
					return err
				}
				if err := tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
					return err
				}
				if readOnly != "on" || isolation != "repeatable read" {
					t.Fatalf("snapshot admission differs: %q %q", readOnly, isolation)
				}
				censusInsertDatabaseFixture(t, ctx, reader.images["database-b"])
				return nil
			})
			if err != nil || len(before.Intents) != 2 || len(before.Attempts) != 4 {
				t.Fatalf("snapshot leaked concurrent source changes: %+v %v", before, err)
			}
			after, err := readDatabaseSnapshot(ctx, connection.Conn(), config.Limits, nil)
			if err != nil || len(after.Intents) != 4 || len(after.Attempts) != 5 {
				t.Fatalf("fresh snapshot lost committed source changes: %+v %v", after, err)
			}
		})
	})
}

// Preflight refuses oversized tables before reading raw rows; a hook failure
// after snapshot establishment also returns no partial successful image.
func TestCensusDatabaseBoundsAndInterruptedSnapshotRefusePartialImage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config, reader := censusTestFixture(t)
		censusInsertDatabaseFixture(t, ctx, reader.images["database-a"])
		server.Db(ctx, func(connection server.PgConn) {
			limits := config.Limits
			limits.MaximumAttempts = 3
			reachedRows := false
			image, err := readDatabaseSnapshot(ctx, connection.Conn(), limits, func(pgx.Tx) error { reachedRows = true; return nil })
			if err == nil || image != nil || reachedRows {
				t.Fatal("oversized snapshot reached raw row phase")
			}
			interrupted := errors.New("synthetic snapshot interruption")
			image, err = readDatabaseSnapshot(ctx, connection.Conn(), config.Limits, func(pgx.Tx) error { return interrupted })
			if !errors.Is(err, interrupted) || image != nil {
				t.Fatal("interrupted snapshot returned a partial image")
			}
			image, err = readDatabaseSnapshot(ctx, connection.Conn(), config.Limits, nil)
			if err != nil || len(image.Attempts) != 4 {
				t.Fatalf("failed snapshot leaked transaction ownership: %v", err)
			}
		})
	})
}
