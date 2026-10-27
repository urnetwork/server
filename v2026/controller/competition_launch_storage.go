// Launch diagnostics inspect the exact archive selected by competition
// configuration, not the unrelated default blob bucket.
package controller

import (
	"errors"
	"strings"

	"github.com/urnetwork/server/v2026"
)

// Returns only the configured archive's backing identity for independent
// read-only proof. The existing loader remains the sole bucket/default policy.
func (self *Settings) CompetitionArtifactStore() (server.RetainedBlobStore, string, error) {
	if self == nil {
		return nil, "", errors.New("competition archive settings unavailable")
	}
	archive, ok := self.artifactArchive.(*blobArtifactArchive)
	if !ok || archive == nil || archive.store == nil {
		return nil, "", errors.New("competition artifact archive unavailable")
	}
	bucket := strings.TrimSpace(self.ArtifactBucket)
	if bucket == "" {
		bucket = defaultCompetitionArtifactBucket
	}
	if archive.store.Bucket() != bucket {
		return nil, "", errors.New("competition archive differs from its configured bucket")
	}
	return archive.store, bucket, nil
}
