// The Bitcoin block scanner's kept chain (server db_crypto_scan_chain.go,
// mmm/BTCPAY.md §16.7): the recent blocks the scanner read, kept beside the
// coin's cursor so a reorg is found again after a restart.
package monitor

import "github.com/urnetwork/server"

// One jsonb column on crypto_scan_cursor, nullable, with no default. The
// migration publishes no index or check.
var cryptoScanChainMigrationContract = migrationCurrentSuffix(server.CryptoScanChainVersion(), "bitcoin scanner kept chain column",
	snMainnetMigrationColumns("crypto_scan_cursor",
		snMainnetMigrationColumn{name: "recent_blocks", kind: "jsonb"}))
