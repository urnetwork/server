// Typed attempt objects retain exact wire bytes in separate immutable namespaces.
// Storage integrity is not validator authorization, policy replay or replication.
package startifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strings"

	"github.com/urnetwork/server/v2026"
)

// These independent limits come from trusted deployment configuration, never
// from a public query, manifest or claimed content size. No graph cap changes.
type AttemptObjectBounds struct {
	MetadataBytes uint64
	RecordBytes   uint64
	ProofBytes    uint64
}

// Every type has a finite byte bound, including the one-byte EOF probe.
func (self AttemptObjectBounds) Validate() error {
	for _, maximum := range []uint64{self.MetadataBytes, self.RecordBytes, self.ProofBytes} {
		if maximum == 0 || maximum >= math.MaxInt64 {
			return errors.New("attempt object bounds are missing or overflow")
		}
	}
	return nil
}

// Type names choose both media type and a distinct content namespace.
func (self AttemptObjectBounds) limit(kind string) (uint64, string, error) {
	if err := self.Validate(); err != nil {
		return 0, "", err
	}
	switch kind {
	case "metadata":
		return self.MetadataBytes, "application/json", nil
	case "records":
		return self.RecordBytes, "application/x-ndjson", nil
	case "proofs":
		return self.ProofBytes, "application/x-ndjson", nil
	default:
		return 0, "", errors.New("attempt object kind is unsupported")
	}
}

// Canonical identities cannot alias generic evidence, another type, or a path.
func AttemptObjectKey(store server.BlobStore, kind, contentHash string) (string, error) {
	if store == nil {
		return "", errors.New("attempt object store is unavailable")
	}
	if err := ValidateAttemptObject(kind, contentHash); err != nil {
		return "", err
	}
	prefix := store.Prefix()
	if prefix == "" || strings.Contains(prefix, "\\") {
		return "", errors.New("attempt object store prefix is invalid")
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("attempt object store prefix is not canonical")
		}
	}
	extension := ".jsonl"
	if kind == "metadata" {
		extension = ".json"
	}
	return path.Join(prefix, "st", "v2", "attempt", kind, "sha256", contentHash[2:]+extension), nil
}

// HTTP admission uses the same closed type/hash grammar before loading storage.
func ValidateAttemptObject(kind, contentHash string) error {
	if kind != "metadata" && kind != "records" && kind != "proofs" {
		return errors.New("attempt object kind is unsupported")
	}
	_, err := attemptObjectHash(contentHash)
	return err
}

// Public hashes have exactly one lowercase, nonzero 0x-prefixed representation.
func attemptObjectHash(contentHash string) ([32]byte, error) {
	var result [32]byte
	if len(contentHash) != 66 || !strings.HasPrefix(contentHash, "0x") {
		return result, errors.New("attempt object hash is not canonical")
	}
	decoded, err := hex.DecodeString(contentHash[2:])
	if err != nil || hex.EncodeToString(decoded) != contentHash[2:] {
		return result, errors.New("attempt object hash is not canonical")
	}
	copy(result[:], decoded)
	if result == ([32]byte{}) {
		return result, errors.New("attempt object hash is empty")
	}
	return result, nil
}

// Stages one already bounded producer-owned chunk. The caller must not mutate
// data during this call. Success requires atomic creation and complete fetch-back;
// it does not authorize publishing a signed cut before full independent replay.
func PublishAttemptObject(ctx context.Context, store server.BlobStore, bounds AttemptObjectBounds, kind, contentHash string, data []byte) (resultErr error) {
	if ctx == nil {
		return errors.New("attempt object context is missing")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	maximum, contentType, err := bounds.limit(kind)
	if err != nil {
		return err
	}
	key, err := AttemptObjectKey(store, kind, contentHash)
	if err != nil {
		return err
	}
	expected, _ := attemptObjectHash(contentHash)
	if len(data) == 0 || uint64(len(data)) > maximum || sha256.Sum256(data) != expected {
		return errors.New("attempt object bytes differ from the typed bound or hash")
	}
	file, err := os.CreateTemp("", "urnetwork-attempt-object-*")
	if err != nil {
		return err
	}
	filePath := file.Name()
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, file.Close())
		}
		resultErr = errors.Join(resultErr, os.Remove(filePath), ctx.Err())
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if count, err := file.Write(data); err != nil {
		return err
	} else if count != len(data) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	closed = true
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := store.PutIfAbsent(ctx, key, filePath, contentType); err != nil {
		return fmt.Errorf("stage immutable attempt object: %w", err)
	}
	count, err := ReadAttemptObjectTo(ctx, store, bounds, kind, contentHash, io.Discard)
	if err != nil {
		return err
	}
	if count != uint64(len(data)) {
		return errors.New("stored attempt object size differs from its producer")
	}
	return nil
}

// Streams with fixed memory. An error invalidates every emitted byte; an HTTP
// caller must abort an already-started response rather than finish a success.
// Bare EOF, the complete hash, the typed size bound, Close and context all pass
// before success. The destination is borrowed and is never closed here.
func ReadAttemptObjectTo(ctx context.Context, store server.BlobStore, bounds AttemptObjectBounds, kind, contentHash string, destination io.Writer) (written uint64, resultErr error) {
	if ctx == nil || destination == nil {
		return 0, errors.New("attempt object read ownership is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	maximum, _, err := bounds.limit(kind)
	if err != nil {
		return 0, err
	}
	key, err := AttemptObjectKey(store, kind, contentHash)
	if err != nil {
		return 0, err
	}
	expected, _ := attemptObjectHash(contentHash)
	reader, err := store.Get(ctx, key)
	if err != nil {
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
		return 0, errors.Join(err, ctx.Err())
	}
	if reader == nil {
		return 0, errors.New("attempt object store returned no reader")
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close(), ctx.Err()) }()
	digest := sha256.New()
	buffer := make([]byte, 32*1024)
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		remaining := maximum - written
		count, readErr := reader.Read(buffer[:min(uint64(len(buffer)), remaining+1)])
		if count < 0 || count > len(buffer) || uint64(count) > remaining {
			return written, errors.New("attempt object exceeds its typed byte bound")
		}
		if err := ctx.Err(); err != nil {
			return written, errors.Join(readErr, err)
		}
		if count != 0 {
			_, _ = digest.Write(buffer[:count])
			accepted, writeErr := destination.Write(buffer[:count])
			if accepted < 0 || accepted > count {
				return written, errors.New("attempt object destination returned an invalid count")
			}
			written += uint64(accepted)
			if writeErr != nil {
				return written, writeErr
			}
			if accepted != count {
				return written, io.ErrShortWrite
			}
			emptyReads = 0
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return written, io.ErrNoProgress
			}
		}
		if readErr == io.EOF {
			var actual [32]byte
			copy(actual[:], digest.Sum(nil))
			if written == 0 || actual != expected {
				return written, errors.New("attempt object bytes differ from their content identity")
			}
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
