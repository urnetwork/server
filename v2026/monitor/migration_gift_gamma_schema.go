// Gift orders paid with Gamma (server gift_gamma_schema.go,
// controller/gift_gamma_controller.go) belong to the migration catalog.
package monitor

import "github.com/urnetwork/server/v2026"

// Follow the migration's position after merges instead of pinning a number.
// Without the column no order can be paid with Gamma, and the biller's credit
// of a gift intent finds no order; without its uniqueness one spend could
// fulfil two orders. The column stays nullable: every order paid another way,
// and every order the 30-day unlink passed, has none.
var giftGammaSchemaMigrationContract = migrationCurrentSuffix(
	server.GiftGammaSchemaVersion(),
	"gift orders paid with Gamma",
	snMainnetMigrationColumns("gift_order",
		snMainnetMigrationColumn{name: "gamma_intent_id", kind: "uuid"}),
	snMainnetMigrationConstraints("gift_order", "UNIQUE (gamma_intent_id)"))
