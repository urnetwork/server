// Mainnet custody upgrades extend the published production catalog exactly.
package server

import "testing"

// The integration base af17d1d2 publishes head 721. Its existing identities
// must survive the usage, policy namespace, guard, and archive additions.
func TestMainnetUsageMigrationsPreservePublishedCatalog(t *testing.T) {
	const publishedHead = 721
	const publishedPrefix = "8c563020311a2df64905e48646e46e0641cccf8067fa110d60fdc725b377e7c2"
	prefix, err := migrationPrefixIdentity(publishedHead)
	if err != nil || prefix != publishedPrefix {
		t.Fatalf("published mainnet head changed: identity=%s error=%v", prefix, err)
	}
	if index := sqlMigrationIndex(t, "ADD COLUMN usage_origin_is_source boolean NULL"); index != publishedHead {
		t.Fatalf("provider usage migration index=%d, want append at %d", index, publishedHead)
	}
	for index, expected := range map[int]string{
		723: clientKeyPolicyHistorySchemaSql,
		724: contractUsageGuardSchemaSql,
		725: providerUsageArchiveSchemaSql,
	} {
		migration, ok := migrations[index].(*SqlMigration)
		if !ok || migration.sql != expected {
			t.Fatalf("mainnet custody migration %d changed identity or order", index)
		}
	}
}
