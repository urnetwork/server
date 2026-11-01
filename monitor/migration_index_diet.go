// The index diet's drops (server/db_index_diet_migrations.go). Each version
// requires its index absent, as the earlier unread-index drops do (816-819),
// so a copy rebuilt under the published name is schema drift. Each version
// follows its migration's position in the server's list.
package monitor

import (
	"github.com/urnetwork/server"
)

var indexDietMigrationContracts = []snMainnetMigrationContract{
	migrationCurrentSuffix(server.IndexDietMigrationVersion(server.IndexDietReapPendingIndexDrop), "unread reap-pending contract index removed",
		migrationIndexRemoved("transfer_contract", "transfer_contract_reap_pending_create_time")),
}
