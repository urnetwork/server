// Evidence content is addressed by its sha256 hex h. New objects are written
// under two levels of h's own leading hex, st/v1/evidence/content/sha256/
// <h[0:2]>/<h[2:4]>/<h>.json, so each leaf directory holds about 1/65,536 of
// the corpus.
//
// Until 2026-11 every object was written flat, at content/sha256/<h>.json. On
// edge-5's XFS drives MinIO keeps each object as a directory, so that one
// directory held hundreds of millions of entries: every create walked its
// whole extent list under the directory's lock (about 73% of MinIO's sampled
// CPU), and it was weeks from XFS's 32 GiB directory limit, where creates
// fail (server CPUOPT6.md, part E §3.6 and #4). The flat objects stay where
// they are and nothing writes there again. Readers try the sharded key, then
// the legacy flat key, so every object published under either stays
// readable by its content hash. Objects stay immutable and content-addressed,
// and retention is unchanged.
package startifact

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/urnetwork/server"
)

// The canonical lowercase hex of a "sha256:<hex>" or bare hex content hash.
func evidenceContentHashHex(hash string) (string, error) {
	h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(hash)), "sha256:")
	if len(h) != 64 {
		return "", errors.New("content hash must be sha256 hex")
	}
	if _, err := hex.DecodeString(h); err != nil {
		return "", errors.New("content hash must be sha256 hex")
	}
	return h, nil
}

// The store-relative keys one content hash may be stored at, in read order:
// the sharded key new evidence is written at, then the legacy flat key. A
// caller without a store, such as one checking another operator's receipt,
// matches a key against these as a complete path suffix.
func EvidenceContentKeySuffixes(hash string) ([]string, error) {
	h, err := evidenceContentHashHex(hash)
	if err != nil {
		return nil, err
	}
	return []string{
		path.Join("st", "v1", "evidence", "content", "sha256", h[0:2], h[2:4], h+".json"),
		path.Join("st", "v1", "evidence", "content", "sha256", h+".json"),
	}, nil
}

// The keys one content hash may be stored at in store, in read order.
func evidenceContentKeys(store server.BlobStore, hash string) ([]string, error) {
	if store == nil {
		return nil, errors.New("server/blob store is unavailable")
	}
	suffixes, err := EvidenceContentKeySuffixes(hash)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(suffixes))
	for i, suffix := range suffixes {
		keys[i] = filepath.ToSlash(filepath.Join(store.Prefix(), filepath.FromSlash(suffix)))
	}
	return keys, nil
}

// The sharded key every new publication of this content is written at.
func EvidenceContentKey(store server.BlobStore, hash string) (string, error) {
	keys, err := evidenceContentKeys(store, hash)
	if err != nil {
		return "", err
	}
	return keys[0], nil
}

// The flat key content was written at before the sharded layout. It is read,
// after the sharded key, and never written.
func evidenceLegacyContentKey(store server.BlobStore, hash string) (string, error) {
	keys, err := evidenceContentKeys(store, hash)
	if err != nil {
		return "", err
	}
	return keys[1], nil
}

// Opens one content hash's stored evidence: its sharded key, or, when that
// holds no object, its legacy flat key. A remote store reports a missing
// object only at the first read, so the sharded object's first byte is read
// here to learn whether it exists, and is replayed to the caller. The legacy
// key is returned unread: its absence, or any failure reading it, surfaces
// exactly as a single Get of it would. A sharded object that fails other than
// by being absent is the answer, never a reason to read the legacy key. The
// caller closes the reader.
func OpenEvidenceContent(ctx context.Context, store server.BlobStore, hash string) (io.ReadCloser, error) {
	if ctx == nil {
		return nil, errors.New("evidence content read context is missing")
	}
	keys, err := evidenceContentKeys(store, hash)
	if err != nil {
		return nil, err
	}
	reader, err := store.Get(ctx, keys[0])
	if err == nil {
		first := make([]byte, 1)
		count, readErr := 0, error(nil)
		// a reader may answer a read with no bytes and no error; 100 such
		// answers in a row are no progress, as in compareArtifactReader
		for emptyReads := 0; count == 0 && readErr == nil; emptyReads++ {
			if 100 <= emptyReads {
				readErr = io.ErrNoProgress
				break
			}
			count, readErr = reader.Read(first)
		}
		if count < 0 || len(first) < count {
			count, readErr = 0, errors.Join(errors.New("evidence content reader returned an invalid count"), readErr)
		}
		if 0 < count {
			// a read error that came with the byte is the next read's answer
			return &evidenceContentReader{ReadCloser: reader, first: first[:count], err: readErr}, nil
		}
		if !server.IsBlobNotFound(readErr) {
			// an empty or failed sharded object is still the stored answer
			return &evidenceContentReader{ReadCloser: reader, err: readErr}, nil
		}
		if err := reader.Close(); err != nil {
			return nil, err
		}
	} else if !server.IsBlobNotFound(err) {
		return nil, err
	}
	return store.Get(ctx, keys[1])
}

// The sharded object's first byte, read to learn that it exists, then the
// rest of its body. Close closes the store's reader. Not safe for concurrent
// use, like the reader it wraps.
type evidenceContentReader struct {
	io.ReadCloser
	// the bytes already read from the store's reader, replayed first
	first []byte
	// the error that ended the first read, answered once first is replayed
	err error
}

func (self *evidenceContentReader) Read(value []byte) (int, error) {
	if 0 < len(self.first) {
		count := copy(value, self.first)
		self.first = self.first[count:]
		return count, nil
	}
	if self.err != nil {
		return 0, self.err
	}
	return self.ReadCloser.Read(value)
}
