//go:build linux || darwin || freebsd

package controller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Exercise a symlinked TMPDIR on every supported host, not only Darwin /var.
// Positive fixture setup resolves the alias before minting custody; the actual
// validator must still reject the same root presented through an alias.
func TestProviderWorkFixtureCreationStoreOwnsPhysicalRoot(t *testing.T) {
	parent, err := os.MkdirTemp("", "provider-work-physical-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(parent); err != nil {
			t.Error(err)
		}
	})
	physicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	realDirectory, alias := filepath.Join(physicalParent, "real"), filepath.Join(physicalParent, "alias")
	if err := os.Mkdir(realDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDirectory, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	root := filepath.Join(providerWorkPhysicalTempDir(t), "requests")
	scope := connect.OriginalContractStoreScope{DomainHash: [32]byte{1}, ClientId: [16]byte{2}, PublicKey: [32]byte{3}, SourceGeneration: [16]byte{4}}
	providerWorkPrepareCreationStore(t, root, scope)
	if physical, err := filepath.EvalSymlinks(root); err != nil || physical != root {
		t.Fatal("fresh original custody root is not physical", err)
	}
	relative, err := filepath.Rel(realDirectory, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := connect.ValidateOriginalContractStore(t.Context(), filepath.Join(alias, relative), scope); err == nil {
		t.Fatal("runtime original custody accepted a symlink ancestor")
	}
	if err := connect.ValidateOriginalContractStore(t.Context(), root, scope); err != nil {
		t.Fatal("alias rejection changed the original physical custody", err)
	}
}
