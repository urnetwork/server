// The Bitcoin block scanner's kept chain (controller/crypto_pay_bitcoin.go,
// mmm/BTCPAY.md §16.7): the recent blocks it read, with their hashes and what
// each paid the invoices in play, stored beside the coin's scan cursor in
// crypto_scan_cursor.recent_blocks, so a reorg is found again after a
// restart, on whichever taskworker the next scan runs, and the scan re-reads
// from the fork point.
//
// One nullable column, a catalog change only: no row is rewritten, the
// five-second lock bound keeps the catalog wait from stalling behind a long
// reader, and a column that exists already is left as it is, so a failed
// lock wait is safe to retry. Only the Bitcoin reader reads or writes it: the
// other coins' cursors are read and written without it, so their scanners
// work whether or not it is applied. Apply before deploying the taskworker
// that scans Bitcoin (Bitcoin stays off until its crypto_pay.yml entry
// exists).
package server

// The migration's statement.
const CryptoScanChainSql = `
	SET LOCAL lock_timeout = '5s';

	ALTER TABLE crypto_scan_cursor ADD COLUMN IF NOT EXISTS recent_blocks jsonb NULL;
`

var cryptoScanChainMigration = newSqlMigration(CryptoScanChainSql)

// The migration's version: its position in the list, from 1.
func CryptoScanChainVersion() int {
	for i, migration := range migrations {
		if migration == cryptoScanChainMigration {
			return i + 1
		}
	}
	return 0
}
