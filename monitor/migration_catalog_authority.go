// SIGNALS.md §8.9: compiled migration identities are not live-schema knowledge.
package monitor

// Legacy predicates own through the version before the current suffix. Every
// additional version needs its explicit source contract; positional TRUE
// placeholders never establish authority for an unreviewed append or gap.
func migrationCatalogAuthorityHead(contracts []snMainnetMigrationContract) int {
	versions := map[int]bool{}
	for _, contract := range contracts {
		if contract.query != "" {
			versions[contract.artifact.requiredVersion] = true
		}
	}
	head := migrationCurrentSuffixFirstVersion - 1
	for versions[head+1] {
		head++
	}
	return head
}
