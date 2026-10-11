package startifact

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func ledgerObjectTestStore(t *testing.T) server.BlobStore {
	t.Helper()
	return server.NewLocalBlobStore(t.TempDir(), "test")
}

// Ledger objects are written once under their sha256 and read back verified.
// SV-3 (object immutability)
func TestLedgerObjectPutIfAbsentRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := ledgerObjectTestStore(t)
	object := []byte("account chunk")
	ref, err := PutLedgerObject(ctx, store, object)
	if err != nil {
		t.Fatal(err)
	}
	if ref != LedgerObjectRef(object) || !strings.HasPrefix(ref, "sha256:") || len(ref) != len("sha256:")+64 {
		t.Fatalf("reference %q", ref)
	}
	again, err := PutLedgerObject(ctx, store, object)
	if err != nil || again != ref {
		t.Fatalf("repeated put = %q, %v", again, err)
	}
	read, err := GetLedgerObject(ctx, store, ref)
	if err != nil || string(read) != string(object) {
		t.Fatalf("read %q, %v", read, err)
	}
	digest, err := ParseLedgerObjectRef(ref)
	if err != nil || LedgerObjectRef(object) != "sha256:"+strings.TrimPrefix(ref, "sha256:") || ledgerObjectKey(store, digest) != "test/st/v2/ledger/sha256/"+strings.TrimPrefix(ref, "sha256:") {
		t.Fatalf("parsed reference %x, %v", digest, err)
	}
}

// Every refusal of the object store, each triggered alone.
// SV-3 (object immutability)
func TestLedgerObjectRefusals(t *testing.T) {
	ctx := context.Background()
	store := ledgerObjectTestStore(t)
	object := []byte("provider chunk")
	ref := LedgerObjectRef(object)

	if _, err := PutLedgerObject(ctx, nil, object); err == nil || err.Error() != "server/blob store is unavailable" {
		t.Fatalf("nil store put: %v", err)
	}
	if _, err := GetLedgerObject(ctx, nil, ref); err == nil || err.Error() != "server/blob store is unavailable" {
		t.Fatalf("nil store get: %v", err)
	}
	if _, err := PutLedgerObject(ctx, store, make([]byte, MaximumLedgerObjectBytes+1)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize put: %v", err)
	}
	if _, err := GetLedgerObject(ctx, store, ref); !errors.Is(err, ErrLedgerObjectNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	for _, malformed := range []string{
		strings.TrimPrefix(ref, "sha256:"),
		"sha512:" + strings.TrimPrefix(ref, "sha256:"),
		strings.ToUpper(ref[:7]) + ref[7:],
		"sha256:" + strings.ToUpper(strings.TrimPrefix(ref, "sha256:")),
		ref[:len(ref)-1],
		ref + "0",
		"sha256:" + strings.Repeat("g", 64),
	} {
		if _, err := GetLedgerObject(ctx, store, malformed); err == nil || errors.Is(err, ErrLedgerObjectNotFound) || !strings.Contains(err.Error(), "64 lowercase hex digits") {
			t.Fatalf("malformed reference %q: %v", malformed, err)
		}
	}

	// A key that already holds other bytes is never accepted or replaced.
	digest, _ := ParseLedgerObjectRef(ref)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(other, []byte("different bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, ledgerObjectKey(store, digest), other, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if _, err := PutLedgerObject(ctx, store, object); err == nil || !strings.Contains(err.Error(), "already holds different bytes") {
		t.Fatalf("conflicting put: %v", err)
	}
	if _, err := GetLedgerObject(ctx, store, ref); err == nil || !strings.Contains(err.Error(), "does not match its reference") {
		t.Fatalf("corrupted object read: %v", err)
	}
}
