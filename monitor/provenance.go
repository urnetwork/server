package monitor

import "regexp"

// Go currently records Git revisions as full SHA-1 or SHA-256 object IDs.
// Keep artifact-identity validation shared so a service-specific diagnostic
// cannot silently accept weaker provenance than the fleet deployment signal.
var goSourceRevisionPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// Warp supplies the inspected OCI image content identity, never a mutable tag.
var ociImageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func validGoSourceRevision(revision string) bool {
	return goSourceRevisionPattern.MatchString(revision)
}

func validOCIImageDigest(digest string) bool {
	return ociImageDigestPattern.MatchString(digest)
}
