package startifact

// Settlement v2 ledger objects (VERSION2-FINAL.md §6.5): a pool ledger's
// header, provider chunks and account chunks. Each object is stored once,
// put-if-absent, under its sha256 and is never replaced. The root signer's
// on-chain commitment authenticates the header, which authenticates the
// chunks, so objects carry no signature.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"

	"github.com/urnetwork/server"
)

// MaximumLedgerObjectBytes bounds one object: a full provider chunk is
// 131,072 rows of 57 bytes, about 7.1 MiB (§6.6).
const MaximumLedgerObjectBytes = 8 << 20

// ErrLedgerObjectNotFound reports a well-formed reference with no stored object.
var ErrLedgerObjectNotFound = errors.New("ledger object not found")

// LedgerObjectRef returns the reference of b: "sha256:" and 64 lowercase hex digits.
func LedgerObjectRef(b []byte) string {
	digest := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// ParseLedgerObjectRef returns the digest named by a canonical reference.
func ParseLedgerObjectRef(ref string) ([32]byte, error) {
	var digest [32]byte
	encoded, ok := strings.CutPrefix(ref, "sha256:")
	if !ok || len(encoded) != 64 || strings.ToLower(encoded) != encoded {
		return digest, fmt.Errorf("ledger object reference %q is not sha256: and 64 lowercase hex digits", ref)
	}
	if _, err := hex.Decode(digest[:], []byte(encoded)); err != nil {
		return digest, fmt.Errorf("ledger object reference %q is not sha256: and 64 lowercase hex digits", ref)
	}
	return digest, nil
}

func ledgerObjectKey(store server.BlobStore, digest [32]byte) string {
	return filepath.ToSlash(filepath.Join(store.Prefix(), "st", "v2", "ledger", "sha256", hex.EncodeToString(digest[:])))
}

// PutLedgerObject stores b under its sha256 unless it is already present, then
// reads the stored object back and requires the same bytes.
func PutLedgerObject(ctx context.Context, store server.BlobStore, b []byte) (string, error) {
	if store == nil {
		return "", errors.New("server/blob store is unavailable")
	}
	if len(b) > MaximumLedgerObjectBytes {
		return "", fmt.Errorf("ledger object exceeds %d bytes", MaximumLedgerObjectBytes)
	}
	ref := LedgerObjectRef(b)
	digest, _ := ParseLedgerObjectRef(ref)
	key := ledgerObjectKey(store, digest)
	tmp, err := os.CreateTemp("", "urnetwork-st-ledger-*.bin")
	if err != nil {
		return "", err
	}
	path := tmp.Name()
	defer os.Remove(path)
	_, writeErr := tmp.Write(b)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return "", err
	}
	if _, err := store.PutIfAbsent(ctx, key, path, "application/octet-stream"); err != nil {
		return "", fmt.Errorf("create ledger object %s: %w", ref, err)
	}
	reader, err := store.Get(ctx, key)
	if err != nil {
		return "", err
	}
	equal, readErr := compareArtifactReader(ctx, reader, b)
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return "", err
	}
	if !equal {
		return "", fmt.Errorf("ledger object %s already holds different bytes", ref)
	}
	return ref, nil
}

// GetLedgerObject returns the object a reference names, after checking its
// sha256. A malformed reference is an error; a missing object is
// ErrLedgerObjectNotFound.
func GetLedgerObject(ctx context.Context, store server.BlobStore, ref string) ([]byte, error) {
	if store == nil {
		return nil, errors.New("server/blob store is unavailable")
	}
	digest, err := ParseLedgerObjectRef(ref)
	if err != nil {
		return nil, err
	}
	reader, err := store.Get(ctx, ledgerObjectKey(store, digest))
	if err != nil {
		return nil, ledgerObjectReadError(ref, err)
	}
	b, readErr := io.ReadAll(io.LimitReader(reader, MaximumLedgerObjectBytes+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, ledgerObjectReadError(ref, err)
	}
	if sha256.Sum256(b) != digest {
		return nil, fmt.Errorf("ledger object %s does not match its reference", ref)
	}
	return b, nil
}

// The local backends report a missing key from open; MinIO from the first read.
func ledgerObjectReadError(ref string, err error) error {
	if errors.Is(err, os.ErrNotExist) || minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return fmt.Errorf("%w: %s", ErrLedgerObjectNotFound, ref)
	}
	return err
}
