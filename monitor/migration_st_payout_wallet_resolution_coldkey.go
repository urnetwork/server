package monitor

import (
	"github.com/urnetwork/server"
)

// The settled earning wallets by coldkey within an epoch, smallest client
// first, with the client's network: the v2 earnings readers' attribution index
// (server db_network_wallet_mapping_consent.go), built online. The covering
// column is pinned with the keys, since each probe reads the network from the
// index. Versioned by its position.
var stPayoutWalletResolutionColdkeyIndexMigrationContract = migrationCurrentSuffix(
	server.StPayoutWalletResolutionColdkeyIndexMigrationVersion(),
	"settled earning wallets by coldkey within an epoch, smallest client first",
	stPayoutWalletResolutionColdkeyIndexArtifactQuery,
)

const stPayoutWalletResolutionColdkeyIndexArtifactQuery = `EXISTS (SELECT 1 FROM index_artifact
		WHERE table_name = 'st_payout_wallet_resolution' AND index_name = 'st_payout_wallet_resolution_coldkey'
		AND definition = 'CREATE INDEX st_payout_wallet_resolution_coldkey ON public.st_payout_wallet_resolution USING btree (deployment_key, no_id, epoch, coldkey, client_id) INCLUDE (network_id)'
		AND predicate_definition IS NULL AND indisvalid AND indisready)`
