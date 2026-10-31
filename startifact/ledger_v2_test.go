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

// Recovery usage objects are written once under their sha256 like ledger
// objects, but under their own prefix: the public ledger object route never
// serves one, and the recovery reader never serves a ledger object.
func TestRecoveryObjectPrefix(t *testing.T) {
	ctx := context.Background()
	store := ledgerObjectTestStore(t)
	usage, ledgerChunk := []byte("usage chunk"), []byte("ledger chunk")
	ref, err := PutRecoveryObject(ctx, store, usage)
	if err != nil || ref != LedgerObjectRef(usage) {
		t.Fatalf("put %q, %v", ref, err)
	}
	if again, err := PutRecoveryObject(ctx, store, usage); err != nil || again != ref {
		t.Fatalf("repeated put = %q, %v", again, err)
	}
	if read, err := GetRecoveryObject(ctx, store, ref); err != nil || string(read) != string(usage) {
		t.Fatalf("read %q, %v", read, err)
	}
	if _, err := GetLedgerObject(ctx, store, ref); !errors.Is(err, ErrLedgerObjectNotFound) {
		t.Fatalf("the ledger route reads a recovery object: %v", err)
	}
	ledgerRef, err := PutLedgerObject(ctx, store, ledgerChunk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GetRecoveryObject(ctx, store, ledgerRef); !errors.Is(err, ErrLedgerObjectNotFound) {
		t.Fatalf("the recovery reader reads a ledger object: %v", err)
	}
	digest, _ := ParseLedgerObjectRef(ref)
	if key := recoveryObjectKey(store, digest); key != "test/st/v2/recovery/sha256/"+strings.TrimPrefix(ref, "sha256:") {
		t.Fatalf("key %q", key)
	}
	if _, err := GetRecoveryObject(ctx, store, "sha256:"+strings.Repeat("0", 63)); err == nil {
		t.Fatal("a malformed reference read")
	}
	if _, err := PutRecoveryObject(ctx, nil, usage); err == nil {
		t.Fatal("a put without a store")
	}
}
