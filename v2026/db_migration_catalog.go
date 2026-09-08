package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"strconv"
	"strings"
)

const (
	// publishedMigrationPrefixCount is the database history executed by the
	// release-1.0 testnet before later migrations were authored. This prefix is
	// append-only: changing its order changes what a numeric DB version means.
	publishedMigrationPrefixCount = 593

	// migrationCatalogTableIndex creates the catalog in ordinary SQL so schema
	// audit reconstruction sees it. migrationCatalogBaselineIndex is the next
	// code migration, which records every prior identity; its normal success
	// record then adds the baseline migration itself.
	migrationCatalogTableIndex    = 598
	migrationCatalogBaselineIndex = 599
)

const migrationCatalogSchemaSQL = `
	CREATE TABLE IF NOT EXISTS migration_catalog (
		migration_index integer PRIMARY KEY CHECK (migration_index >= 0),
		identity_sha256 char(64) NOT NULL CHECK (identity_sha256 ~ '^[0-9a-f]{64}$'),
		accepted_at timestamp NOT NULL DEFAULT now()
	)
`

// migrationCatalogMigrations is assigned after the migration slice finishes
// initialization. Keeping this read-only alias out of the slice initializer's
// dependency graph lets the catalog installer itself remain a code migration.
var migrationCatalogMigrations []any

func init() {
	migrationCatalogMigrations = migrations
}

type migrationCatalogEntry struct {
	Index    int
	Identity string
}

type migrationIndexShape struct {
	Valid     bool
	Ready     bool
	Unique    bool
	Primary   bool
	Columns   []string
	Predicate string
}

func writeMigrationIdentityPart(digest hash.Hash, value string) {
	_, _ = digest.Write([]byte(strconv.Itoa(len(value))))
	_, _ = digest.Write([]byte{':'})
	_, _ = digest.Write([]byte(value))
}

func migrationIdentity(migration any) (string, error) {
	digest := sha256.New()
	switch value := migration.(type) {
	case *SqlMigration:
		if value == nil {
			return "", fmt.Errorf("nil SQL migration")
		}
		writeMigrationIdentityPart(digest, "sql")
		writeMigrationIdentityPart(digest, value.sql)
	case *OnlineSqlMigration:
		if value == nil {
			return "", fmt.Errorf("nil online SQL migration")
		}
		writeMigrationIdentityPart(digest, "online-sql")
		writeMigrationIdentityPart(digest, value.sql)
		writeMigrationIdentityPart(digest, value.auditSql)
	case *CodeMigration:
		if value == nil || value.callback == nil || strings.TrimSpace(value.id) == "" {
			return "", fmt.Errorf("code migration has no stable identity or callback")
		}
		writeMigrationIdentityPart(digest, "code")
		writeMigrationIdentityPart(digest, value.id)
	default:
		return "", fmt.Errorf("unknown migration type %T", migration)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func migrationPrefixIdentity(count int) (string, error) {
	if count < 0 || count > len(migrations) {
		return "", fmt.Errorf("migration prefix count %d outside [0,%d]", count, len(migrations))
	}
	digest := sha256.New()
	for index := 0; index < count; index += 1 {
		identity, err := migrationIdentity(migrations[index])
		if err != nil {
			return "", fmt.Errorf("migration %d identity: %w", index, err)
		}
		writeMigrationIdentityPart(digest, strconv.Itoa(index))
		writeMigrationIdentityPart(digest, identity)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func recordMigrationIdentity(ctx context.Context, tx PgTx, index int, identity string) error {
	commandTag, err := tx.Exec(ctx, `
		INSERT INTO migration_catalog (migration_index, identity_sha256)
		VALUES ($1, $2)
		ON CONFLICT (migration_index) DO UPDATE
		SET identity_sha256 = EXCLUDED.identity_sha256
		WHERE migration_catalog.identity_sha256 = EXCLUDED.identity_sha256
	`, index, identity)
	if err != nil {
		return err
	}
	if commandTag.RowsAffected() != 1 {
		return fmt.Errorf("migration %d identity conflicts with durable catalog", index)
	}
	return nil
}

func recordMigrationIdentityIfCatalogExists(ctx context.Context, tx PgTx, index int) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('public.migration_catalog') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	identity, err := migrationIdentity(migrations[index])
	if err != nil {
		return err
	}
	return recordMigrationIdentity(ctx, tx, index, identity)
}

func loadMigrationColumnShape(ctx context.Context, table string, column string) (string, bool, string, error) {
	var dataType string
	var notNull bool
	var defaultExpression string
	var queryErr error
	MaintenanceDb(ctx, func(conn PgConn) {
		queryErr = conn.QueryRow(ctx, `
			SELECT
				format_type(attribute.atttypid, attribute.atttypmod),
				attribute.attnotnull,
				COALESCE(pg_get_expr(attribute_default.adbin, attribute_default.adrelid), '')
			FROM pg_class AS relation
			JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			JOIN pg_attribute AS attribute ON attribute.attrelid = relation.oid
			LEFT JOIN pg_attrdef AS attribute_default
				ON attribute_default.adrelid = relation.oid
				AND attribute_default.adnum = attribute.attnum
			WHERE namespace.nspname = 'public'
			  AND relation.relname = $1
			  AND attribute.attname = $2
			  AND attribute.attnum > 0
			  AND NOT attribute.attisdropped
		`, table, column).Scan(&dataType, &notNull, &defaultExpression)
	}, OptReadOnly(), OptNoRetry())
	return dataType, notNull, defaultExpression, queryErr
}

func loadMigrationIndexShape(ctx context.Context, table string, index string) (migrationIndexShape, error) {
	var shape migrationIndexShape
	var queryErr error
	MaintenanceDb(ctx, func(conn PgConn) {
		queryErr = conn.QueryRow(ctx, `
			SELECT
				index_catalog.indisvalid,
				index_catalog.indisready,
				index_catalog.indisunique,
				index_catalog.indisprimary,
				ARRAY(
					SELECT attribute.attname
					FROM unnest(index_catalog.indkey) WITH ORDINALITY AS key_column(attnum, position)
					JOIN pg_attribute AS attribute
					  ON attribute.attrelid = table_catalog.oid
					 AND attribute.attnum = key_column.attnum
					WHERE key_column.position <= index_catalog.indnkeyatts
					ORDER BY key_column.position
				),
				COALESCE(pg_get_expr(index_catalog.indpred, index_catalog.indrelid), '')
			FROM pg_class AS index_relation
			JOIN pg_namespace AS namespace ON namespace.oid = index_relation.relnamespace
			JOIN pg_index AS index_catalog ON index_catalog.indexrelid = index_relation.oid
			JOIN pg_class AS table_catalog ON table_catalog.oid = index_catalog.indrelid
			WHERE namespace.nspname = 'public'
			  AND table_catalog.relname = $1
			  AND index_relation.relname = $2
		`, table, index).Scan(
			&shape.Valid,
			&shape.Ready,
			&shape.Unique,
			&shape.Primary,
			&shape.Columns,
			&shape.Predicate,
		)
	}, OptReadOnly(), OptNoRetry())
	return shape, queryErr
}

func verifyMigrationIndexShape(ctx context.Context, table string, index string, columns []string, predicate string) error {
	shape, err := loadMigrationIndexShape(ctx, table, index)
	if err != nil {
		return fmt.Errorf("load migration index %s: %w", index, err)
	}
	if !shape.Valid || !shape.Ready || shape.Unique || shape.Primary ||
		strings.Join(shape.Columns, ",") != strings.Join(columns, ",") ||
		shape.Predicate != predicate {
		return fmt.Errorf("migration index %s on %s has unexpected shape: %+v", index, table, shape)
	}
	return nil
}

func verifyAppendedMigrationArtifacts(ctx context.Context) error {
	cursorType, cursorNotNull, cursorDefault, err := loadMigrationColumnShape(
		ctx,
		"account_payment",
		"contract_retention_cursor",
	)
	if err != nil {
		return fmt.Errorf("load contract retention cursor: %w", err)
	}
	if cursorType != "uuid" || cursorNotNull || cursorDefault != "" {
		return fmt.Errorf(
			"contract retention cursor has type=%s not_null=%t default=%q",
			cursorType,
			cursorNotNull,
			cursorDefault,
		)
	}
	pendingType, pendingNotNull, pendingDefault, err := loadMigrationColumnShape(
		ctx,
		"account_payment",
		"contract_retention_pending",
	)
	if err != nil {
		return fmt.Errorf("load contract retention pending: %w", err)
	}
	if pendingType != "boolean" || !pendingNotNull ||
		(pendingDefault != "false" && pendingDefault != "'false'::boolean") {
		return fmt.Errorf(
			"contract retention pending has type=%s not_null=%t default=%q",
			pendingType,
			pendingNotNull,
			pendingDefault,
		)
	}
	checks := []struct {
		table     string
		index     string
		columns   []string
		predicate string
	}{
		{
			table:   "transfer_escrow",
			index:   "transfer_escrow_balance_contract",
			columns: []string{"balance_id", "contract_id"},
		},
		{
			table:     "account_payment",
			index:     "account_payment_contract_retention_pending",
			columns:   []string{"complete_time", "payment_id"},
			predicate: "contract_retention_pending",
		},
		{
			table:   "transfer_escrow_sweep",
			index:   "transfer_escrow_sweep_payment_contract",
			columns: []string{"payment_id", "contract_id"},
		},
	}
	for _, check := range checks {
		if err := verifyMigrationIndexShape(
			ctx,
			check.table,
			check.index,
			check.columns,
			check.predicate,
		); err != nil {
			return err
		}
	}
	return nil
}

func migrationInstallCatalog(ctx context.Context) {
	if len(migrationCatalogMigrations) <= migrationCatalogBaselineIndex {
		panic(fmt.Errorf(
			"migration catalog baseline index %d outside migration count %d",
			migrationCatalogBaselineIndex,
			len(migrationCatalogMigrations),
		))
	}
	baseline, ok := migrationCatalogMigrations[migrationCatalogBaselineIndex].(*CodeMigration)
	if !ok || baseline.id != "20260830_install_migration_catalog" {
		panic(fmt.Errorf("migration catalog baseline is not at immutable index %d", migrationCatalogBaselineIndex))
	}
	Raise(verifyAppendedMigrationArtifacts(ctx))
	MaintenanceTx(ctx, func(tx PgTx) {
		for index := 0; index < migrationCatalogBaselineIndex; index += 1 {
			identity, err := migrationIdentity(migrationCatalogMigrations[index])
			Raise(err)
			Raise(recordMigrationIdentity(ctx, tx, index, identity))
		}
	})
}

func validateMigrationCatalog(version int, entries []migrationCatalogEntry) error {
	if version < 0 || version > len(migrations) {
		return fmt.Errorf("database migration version %d outside local catalog [0,%d]", version, len(migrations))
	}
	if len(entries) != version {
		return fmt.Errorf("migration catalog has %d entries for database version %d", len(entries), version)
	}
	for index, entry := range entries {
		if entry.Index != index {
			return fmt.Errorf("migration catalog position %d records index %d", index, entry.Index)
		}
		identity, err := migrationIdentity(migrations[index])
		if err != nil {
			return fmt.Errorf("migration %d identity: %w", index, err)
		}
		if entry.Identity != identity {
			return fmt.Errorf("migration %d identity differs from durable catalog", index)
		}
	}
	return nil
}

func verifyMigrationCatalog(ctx context.Context, version int) {
	var exists bool
	entries := []migrationCatalogEntry{}
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.migration_catalog') IS NOT NULL`).Scan(&exists))
		if !exists {
			return
		}
		result, err := conn.Query(ctx, `
			SELECT migration_index, identity_sha256
			FROM migration_catalog
			ORDER BY migration_index
		`)
		WithPgResult(result, err, func() {
			for result.Next() {
				var entry migrationCatalogEntry
				Raise(result.Scan(&entry.Index, &entry.Identity))
				entry.Identity = strings.TrimSpace(entry.Identity)
				entries = append(entries, entry)
			}
		})
	}, OptReadOnly(), OptNoRetry())
	if !exists {
		if version > migrationCatalogTableIndex {
			panic(fmt.Errorf(
				"database version %d has no migration identity catalog created at version %d",
				version,
				migrationCatalogTableIndex+1,
			))
		}
		return
	}
	// The catalog table and its success record are separate transactions. These
	// two exact bootstrap states are safe and idempotent: either the table DDL
	// committed before its numeric audit record, or that record committed and the
	// immediately following baseline population has not run yet.
	if version == migrationCatalogTableIndex && len(entries) == 0 {
		return
	}
	if version == migrationCatalogBaselineIndex && len(entries) == 1 &&
		entries[0].Index == migrationCatalogTableIndex {
		identity, err := migrationIdentity(migrations[migrationCatalogTableIndex])
		Raise(err)
		if entries[0].Identity == identity {
			return
		}
	}
	Raise(validateMigrationCatalog(version, entries))
}
