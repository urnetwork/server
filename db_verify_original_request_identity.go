// Exact historical JSON-reader identities remain auditable while migration 779
// replaces their functions. No durable catalog row is rewritten or generalized.
package server

// Both sides are pinned: a later accidental edit cannot inherit permission to
// accept either historical identity merely by occupying the same numeric slot.
func matchesVerifyOriginalMigrationIdentity(index int, current, recorded string) bool {
	switch index {
	case 772:
		return current == "c5c0b909314aa096b835306b77e6febca1bcdcbb0a7ab009896ae9c4dda0e4c1" &&
			recorded == "6166efbed5e36f5b416b2771cc96ebbc0a3aef9767a37e7e700201face01b212"
	case 774:
		return current == "456185b799bc780ca9e7a186ac66d3a62750fef0729c33f871ebbfc86d07ef20" &&
			recorded == "2780bc1901e2fafb9d1d53955ea2810597d68faddbab5db6d41141e8afe2a509"
	default:
		return false
	}
}

// Startup and read-only monitors admit the same exact recorded history. The
// repair does not claim the current function exists before migration 779 runs.
func MigrationIdentityMatches(index int, recorded string) (bool, error) {
	current, err := MigrationIdentity(index)
	if err != nil {
		return false, err
	}
	return recorded == current || matchesVerifyOriginalMigrationIdentity(index, current, recorded), nil
}
