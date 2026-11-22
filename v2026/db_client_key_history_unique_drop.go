// The redundant unique registration_hash constraint of st_client_key_history
// (owner decision 2026-11-16, PARTITION-CANDIDATES.md §8.5): 20.6 GiB on Main
// that no statement had scanned in the 23 days of statistics up to its drop.
// The hash is SHA-256 of the registration bytes, and those bytes encode the
// client, the domain and the generation that form the primary key, so a
// repeated registration collides on the primary key first, and distinct bytes
// with one hash would be a SHA-256 collision. No foreign key, ON CONFLICT
// target, error check or monitor contract names the constraint.
package server

// A constraint's index cannot be dropped with DROP INDEX CONCURRENTLY, so the
// operator drops the constraint first, by hand, on a direct session with a
// short lock timeout in a vacuum gap: ALTER TABLE takes ACCESS EXCLUSIVE on
// the table for the catalog change and the unlink. This migration then
// records the drop, so the schema audit never recreates the constraint. It
// drops the constraint only while it exists, as on a new database, and is a
// catalog read otherwise: after the operator's drop, db migrate takes no lock
// on the table, where an unconditional ALTER TABLE ... DROP CONSTRAINT IF
// EXISTS would take ACCESS EXCLUSIVE even with nothing left to drop.
const clientKeyHistoryRegistrationHashUniqueDropSql = `
	SET LOCAL lock_timeout = '5s';
	DO $client_key_history_registration_hash_unique_drop$
	BEGIN
		IF EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE
				conrelid = 'public.st_client_key_history'::regclass AND
				conname = 'st_client_key_history_registration_hash_key'
		) THEN
			ALTER TABLE st_client_key_history DROP CONSTRAINT st_client_key_history_registration_hash_key;
		END IF;
	END
	$client_key_history_registration_hash_unique_drop$;
`

// The statement the operator runs on Main before this migration is merged.
const ClientKeyHistoryRegistrationHashUniqueDropOperatorSql = `ALTER TABLE public.st_client_key_history DROP CONSTRAINT IF EXISTS st_client_key_history_registration_hash_key`

// Versions are positions in the migration list, and branches merge in any
// order, so the monitor looks the contract up by migration content.
func ClientKeyHistoryRegistrationHashUniqueDropMigrationVersion() int {
	return migrationVersionOf(clientKeyHistoryRegistrationHashUniqueDropSql)
}
