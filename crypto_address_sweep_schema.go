// The sweep of a per-address coin's issued addresses
// (controller/crypto_pay_sweep.go, model/crypto_address_sweep_model.go). The
// ETH and native SOL scanners read an invoice's address often while the
// invoice is in play; the sweep reads every address an invoice was ever given
// after that, from its own position on the chain, so a payment to an old or
// expired invoice's address is recorded too:
//
//   - crypto_address_sweep: per coin and address, how far its incoming
//     transfers have been read (ETH: a block; SOL: a transaction signature
//     and its slot), when a read last completed, and why the last one failed;
//   - crypto_address_sweep_position: per coin, the pool index the rotation
//     over the issued addresses last passed.
//
// The migration only creates the two tables, empty: no existing table is
// locked or rewritten. The five-second lock bound keeps a catalog wait from
// stalling behind a long reader, and a table that exists already is left as
// it is, so a failed lock wait is safe to retry. Old binaries never read
// them. Apply before deploying the taskworker that runs the scans.
package server

// The migration's statement.
const CryptoAddressSweepSql = `
	DO $address_sweep$
	BEGIN
		PERFORM set_config('lock_timeout', '5s', true);
		IF to_regclass('public.crypto_address_sweep') IS NULL THEN
			CREATE TABLE crypto_address_sweep (
				asset varchar(16) NOT NULL,
				address text NOT NULL,
				height bigint NULL,
				signature varchar(128) NULL,
				read_at timestamp NULL,
				last_error text NULL,
				update_time timestamp NOT NULL,
				PRIMARY KEY (asset, address)
			);
		END IF;
		IF to_regclass('public.crypto_address_sweep_position') IS NULL THEN
			CREATE TABLE crypto_address_sweep_position (
				asset varchar(16) NOT NULL,
				after_address_index bigint NOT NULL,
				update_time timestamp NOT NULL,
				PRIMARY KEY (asset)
			);
		END IF;
	END
	$address_sweep$
`

var cryptoAddressSweepMigration = newSqlMigration(CryptoAddressSweepSql)

// The migration's version: its position in the list, from 1.
func CryptoAddressSweepVersion() int {
	for i, migration := range migrations {
		if migration == cryptoAddressSweepMigration {
			return i + 1
		}
	}
	return 0
}
