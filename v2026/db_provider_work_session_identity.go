// The exact old v776 identity remains auditable while v782 repairs its fences.
package server

// Pin both SQL identities and the slot; an unrelated edit cannot inherit this
// historical exception. The repair never changes the recorded catalog row.
func matchesProviderWorkSessionMigrationIdentity(index int, current, recorded string) bool {
	return index == 775 &&
		current == "e4a902dbb1fabfa96a6b41908148906596d550c5f7f72bba0e42a52498e7328b" &&
		recorded == "646873dd5a17d8bd2ecf6e68ff094532fef6bd94c459f624a244d081ef0c39b7"
}
