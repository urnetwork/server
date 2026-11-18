// Synthetic stores prove archive selection without contacting storage or
// loading credentials, live settings, databases or a model provider.
package controller

import (
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Only identity is exercised; any accidental I/O panics through the nil embed.
type launchArtifactIdentityStore struct {
	server.RetainedBlobStore
	bucket string
}

// Deliberately distinct synthetic bucket names reproduce the old default mixup.
func (self *launchArtifactIdentityStore) Bucket() string { return self.bucket }

// A healthy default store cannot stand in for the archive actually configured.
func TestCompetitionLaunchStoreRejectsDefaultBucketSubstitution(t *testing.T) {
	store := &launchArtifactIdentityStore{bucket: "synthetic-competition"}
	settings := &Settings{ArtifactBucket: store.Bucket(), artifactArchive: &blobArtifactArchive{store: store}}
	actual, bucket, err := settings.CompetitionArtifactStore()
	if err != nil || actual != store || bucket != store.Bucket() {
		t.Fatalf("configured archive not returned: %v", err)
	}
	settings.artifactArchive = &blobArtifactArchive{store: &launchArtifactIdentityStore{bucket: "synthetic-default-blob"}}
	if _, _, err := settings.CompetitionArtifactStore(); err == nil {
		t.Fatal("unrelated default blob bucket qualified competition storage")
	}
}

// The same authoritative default as the archive loader applies only to an
// omitted bucket; unavailable or unrelated archive implementations refuse.
func TestCompetitionLaunchStoreUsesAuthoritativeDefault(t *testing.T) {
	store := &launchArtifactIdentityStore{bucket: defaultCompetitionArtifactBucket}
	settings := &Settings{artifactArchive: &blobArtifactArchive{store: store}}
	if _, bucket, err := settings.CompetitionArtifactStore(); err != nil || bucket != defaultCompetitionArtifactBucket {
		t.Fatalf("archive default changed: %v", err)
	}
	settings.artifactArchive = nil
	if _, _, err := settings.CompetitionArtifactStore(); err == nil {
		t.Fatal("missing archive qualified")
	}
}
