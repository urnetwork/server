// Owned temporary paths are normalized only by fixture provisioning; the
// configured production constructor keeps strict physical-ancestry checks.
package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/durablevolume"
)

// A forced alias reproduces Darwin's temporary-directory ancestry on any host.
func TestDurableBlobFixtureResolvesTemporaryDirectoryAlias(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(parent, "physical")
	if err := os.Mkdir(physical, 0700); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(physical, "volume")
	if err := os.Mkdir(mount, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatal(err)
	}
	fixture := newDurableBlobFixtureAt(t, filepath.Join(alias, "volume"))
	var store BlobStore = fixture.store
	if created, err := store.PutIfAbsent(t.Context(), "retained", fixture.source, ""); err != nil || !created {
		t.Fatal("cold retained write through normalized fixture", created, err)
	}
	reader, err := store.Get(t.Context(), "retained")
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(reader)
	if err := errors.Join(readErr, reader.Close()); err != nil || !bytes.Equal(raw, fixture.raw) {
		t.Fatal("normalized fixture did not preserve retained bytes", err)
	}
}

// Production admission must still refuse an aliased declaration ancestor.
func TestDurableLocalBlobConstructorRejectsDeclarationAlias(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(filepath.Dir(fixture.reference.Path), alias); err != nil {
		t.Fatal(err)
	}
	reference := fixture.reference
	reference.Path = filepath.Join(alias, filepath.Base(reference.Path))
	store, err := newDurableLocalBlobStore(t.Context(), fixture.root, fixture.store.prefix, fixture.store.maxBytes, reference, fixture.host)
	if store != nil || !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("configured constructor accepted an aliased declaration", store, err)
	}
}

// Valid physical paths and declaration hashes cannot authorize another type.
func TestDurableLocalBlobConstructorRejectsFilesystemFactMismatch(t *testing.T) {
	fixture := newDurableBlobFixture(t)
	func() {
		fixture.host.stateLock.Lock()
		defer fixture.host.stateLock.Unlock()
		fixture.host.filesystem.Type ^= 1
	}()
	store, err := newDurableLocalBlobStore(t.Context(), fixture.root, fixture.store.prefix, fixture.store.maxBytes, fixture.reference, fixture.host)
	if store != nil || !errors.Is(err, durablevolume.ErrIdentity) {
		t.Fatal("configured constructor accepted incompatible filesystem facts", store, err)
	}
}
