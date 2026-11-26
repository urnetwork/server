package server

// The api's mirror of the local model files (DESIGN-COMPUTE §14.3, §14.5, §6.5; CO-49; CSRV-M11-01), one object family
// of the blob store (compute-plan-model-mirror-store, QD-1409; metadata row compute-12):
//
//	<prefix>/compute-models/<sha256>/index       the file's chunk index, its bytes as the release tooling computed them
//	<prefix>/compute-models/<sha256>/chunks/<n>  chunk n's bytes, n decimal from 0
//	<prefix>/compute-models/<sha256>/state       the file's state, {"index_sha256":"<hex>","state":"served"}, written last
//
// sidecar/handler's model routes read it through ComputeModelBlobStore, which is their handler.ComputeModelStore;
// compute_model_mirror and the release tooling (CSRV-M11-02) write it through the Put methods. A file answers only
// once its state is served, which Serve writes after it checked every chunk against the index and the index against
// the signed manifest's pin; from then on the file's objects are never written again, so nothing changes under the
// routes or the caches before them. A key is built from a lowercase hex SHA-256 and a chunk number alone, never from
// a path a caller sent. The objects are public files: nothing here names a member, a network, a token or a download.
//
// The blob store has no Delete (blob.go): a file a release stops naming leaves the routes when its objects are taken
// away, which nothing here does (compute-12's ttl, the release tooling's).
//
// Safe for concurrent use.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/urnetwork/sidecar/handler"
)

// The family's names under the store's prefix.
const (
	computeModelKeyDirectory   = "compute-models"
	computeModelIndexName      = "index"
	computeModelChunkDirectory = "chunks"
	computeModelStateName      = "state"
)

// The objects' content types.
const (
	computeModelIndexContentType = "application/json"
	computeModelChunkContentType = "application/octet-stream"
	computeModelStateContentType = "application/json"
)

// The store has nothing to read or write into: no blob store is configured (LoadBlobStore).
var ErrComputeModelStoreUnavailable = errors.New("compute model mirror: no blob store")

// A write the mirror refuses: the file is served, and a served file's objects never change.
var ErrComputeModelFileServed = errors.New("compute model mirror: the file is served, and its objects are final")

// Tunables of the mirror's store.
type ComputeModelBlobStoreSettings struct {
	// The largest state object read, in bytes: 4 KiB, forty times the record's own hundred bytes. A larger one is
	// refused unread, as a store fault.
	StateMaxByteCount int64
	// The buffer an object's first bytes are read into before its reader is handed on, so a missing object answers
	// absent before any of it is answered (MinIO's reads are lazy): 64 KiB, two of io.Copy's buffers. Below bufio's
	// smallest, 16, bufio's is used.
	ReadBufferByteCount int
}

func DefaultComputeModelBlobStoreSettings() *ComputeModelBlobStoreSettings {
	return &ComputeModelBlobStoreSettings{
		StateMaxByteCount:   4 * 1024,
		ReadBufferByteCount: 64 * 1024,
	}
}

// The mirror over the blob store LoadBlobStore chooses, at each call.
type ComputeModelBlobStore struct {
	settings *ComputeModelBlobStoreSettings
}

// A nil settings takes the defaults.
func NewComputeModelBlobStore(settings *ComputeModelBlobStoreSettings) *ComputeModelBlobStore {
	if settings == nil {
		settings = DefaultComputeModelBlobStoreSettings()
	}
	return &ComputeModelBlobStore{settings: settings}
}

// The state object's bytes.
type computeModelStateObject struct {
	IndexSha256 string `json:"index_sha256,omitempty"`
	State       string `json:"state"`
}

// The blob store, or ErrComputeModelStoreUnavailable.
func (self *ComputeModelBlobStore) blobStore() (BlobStore, error) {
	store, ok := LoadBlobStore()
	if !ok || store == nil {
		return nil, ErrComputeModelStoreUnavailable
	}
	return store, nil
}

// The file's directory, from a SHA-256 in lowercase hex alone.
func computeModelFileKey(store BlobStore, sha256Text string) (string, error) {
	if !computeModelSha256Valid(sha256Text) {
		return "", fmt.Errorf("compute model mirror: %q is no lowercase hex SHA-256", sha256Text)
	}
	return strings.Join([]string{store.Prefix(), computeModelKeyDirectory, sha256Text}, "/"), nil
}

func computeModelIndexKey(store BlobStore, sha256Text string) (string, error) {
	fileKey, err := computeModelFileKey(store, sha256Text)
	if err != nil {
		return "", err
	}
	return fileKey + "/" + computeModelIndexName, nil
}

func computeModelChunkKey(store BlobStore, sha256Text string, n int) (string, error) {
	fileKey, err := computeModelFileKey(store, sha256Text)
	if err != nil {
		return "", err
	}
	if n < 0 {
		return "", fmt.Errorf("compute model mirror: chunk %d", n)
	}
	return fileKey + "/" + computeModelChunkDirectory + "/" + strconv.Itoa(n), nil
}

func computeModelStateKey(store BlobStore, sha256Text string) (string, error) {
	fileKey, err := computeModelFileKey(store, sha256Text)
	if err != nil {
		return "", err
	}
	return fileKey + "/" + computeModelStateName, nil
}

// Whether text is a SHA-256 in lowercase hex.
func computeModelSha256Valid(text string) bool {
	if len(text) != 2*sha256.Size {
		return false
	}
	for i := 0; i < len(text); i += 1 {
		c := text[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// The object at key, opened and its first bytes read, so a missing one is handler.ErrComputeModelObjectAbsent here.
func (self *ComputeModelBlobStore) open(ctx context.Context, store BlobStore, key string) (io.ReadCloser, error) {
	reader, err := store.Get(ctx, key)
	if err != nil {
		if IsBlobNotFound(err) {
			return nil, handler.ErrComputeModelObjectAbsent
		}
		return nil, err
	}
	buffered := bufio.NewReaderSize(reader, self.settings.ReadBufferByteCount)
	if _, err := buffered.Peek(1); err != nil && !errors.Is(err, io.EOF) {
		reader.Close()
		if IsBlobNotFound(err) {
			return nil, handler.ErrComputeModelObjectAbsent
		}
		return nil, err
	}
	return &computeModelObjectReader{Reader: buffered, closer: reader}, nil
}

// An opened object: its buffered bytes, closing the store's reader.
type computeModelObjectReader struct {
	*bufio.Reader
	closer io.Closer
}

func (self *computeModelObjectReader) Close() error {
	return self.closer.Close()
}

// The file's state, or handler.ErrComputeModelObjectAbsent for a file with none.
func (self *ComputeModelBlobStore) FileRecord(ctx context.Context, sha256Text string) (*handler.ComputeModelFileRecord, error) {
	store, err := self.blobStore()
	if err != nil {
		return nil, err
	}
	return self.fileRecord(ctx, store, sha256Text)
}

func (self *ComputeModelBlobStore) fileRecord(ctx context.Context, store BlobStore, sha256Text string) (*handler.ComputeModelFileRecord, error) {
	key, err := computeModelStateKey(store, sha256Text)
	if err != nil {
		return nil, err
	}
	reader, err := self.open(ctx, store, key)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, self.settings.StateMaxByteCount+1))
	if err != nil {
		if IsBlobNotFound(err) {
			return nil, handler.ErrComputeModelObjectAbsent
		}
		return nil, err
	}
	if self.settings.StateMaxByteCount < int64(len(data)) {
		return nil, fmt.Errorf("compute model mirror: %s's state is past %d bytes", sha256Text, self.settings.StateMaxByteCount)
	}
	return parseComputeModelState(data)
}

// A state object's record, refused unless it is exactly one record of a known state.
func parseComputeModelState(data []byte) (*handler.ComputeModelFileRecord, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	object := &computeModelStateObject{}
	if err := decoder.Decode(object); err != nil {
		return nil, fmt.Errorf("compute model mirror: a state that does not read: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("compute model mirror: more follows a state")
	}
	record := &handler.ComputeModelFileRecord{
		State:       handler.ComputeModelFileState(object.State),
		IndexSha256: object.IndexSha256,
	}
	if err := computeModelStateValid(record); err != nil {
		return nil, err
	}
	return record, nil
}

// Whether a record is one a state object may hold: copied with no index pin, indexed or served with one.
func computeModelStateValid(record *handler.ComputeModelFileRecord) error {
	switch record.State {
	case handler.ComputeModelFileStateCopied:
		if record.IndexSha256 != "" {
			return errors.New("compute model mirror: a copied file pins no index")
		}
	case handler.ComputeModelFileStateIndexed, handler.ComputeModelFileStateServed:
		if !computeModelSha256Valid(record.IndexSha256) {
			return fmt.Errorf("compute model mirror: a file %s pins no index", record.State)
		}
	default:
		return fmt.Errorf("compute model mirror: the state %q", record.State)
	}
	return nil
}

// The index's bytes, or handler.ErrComputeModelObjectAbsent.
func (self *ComputeModelBlobStore) OpenIndex(ctx context.Context, sha256Text string) (io.ReadCloser, error) {
	store, err := self.blobStore()
	if err != nil {
		return nil, err
	}
	key, err := computeModelIndexKey(store, sha256Text)
	if err != nil {
		return nil, err
	}
	return self.open(ctx, store, key)
}

// Chunk n's bytes, or handler.ErrComputeModelObjectAbsent.
func (self *ComputeModelBlobStore) OpenChunk(ctx context.Context, sha256Text string, n int) (io.ReadCloser, error) {
	store, err := self.blobStore()
	if err != nil {
		return nil, err
	}
	key, err := computeModelChunkKey(store, sha256Text, n)
	if err != nil {
		return nil, err
	}
	return self.open(ctx, store, key)
}

// Whether the file is served, which refuses every write but its own state again.
func (self *ComputeModelBlobStore) served(ctx context.Context, store BlobStore, sha256Text string) (*handler.ComputeModelFileRecord, error) {
	record, err := self.fileRecord(ctx, store, sha256Text)
	if errors.Is(err, handler.ErrComputeModelObjectAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record.State != handler.ComputeModelFileStateServed {
		return nil, nil
	}
	return record, nil
}

// Stores the file at localPath as the file's index, the release tooling's step; refused once the file is served.
func (self *ComputeModelBlobStore) PutIndex(ctx context.Context, sha256Text string, localPath string) error {
	store, err := self.blobStore()
	if err != nil {
		return err
	}
	key, err := computeModelIndexKey(store, sha256Text)
	if err != nil {
		return err
	}
	if record, err := self.served(ctx, store, sha256Text); err != nil {
		return err
	} else if record != nil {
		return ErrComputeModelFileServed
	}
	return store.Put(ctx, key, localPath, computeModelIndexContentType)
}

// Stores the file at localPath as the file's chunk n, compute_model_mirror's copy; refused once the file is served.
func (self *ComputeModelBlobStore) PutChunk(ctx context.Context, sha256Text string, n int, localPath string) error {
	store, err := self.blobStore()
	if err != nil {
		return err
	}
	key, err := computeModelChunkKey(store, sha256Text, n)
	if err != nil {
		return err
	}
	if record, err := self.served(ctx, store, sha256Text); err != nil {
		return err
	} else if record != nil {
		return ErrComputeModelFileServed
	}
	return store.Put(ctx, key, localPath, computeModelChunkContentType)
}

// Writes the file's state, the last of its objects: copied, indexed, or served once Serve checked every chunk.
// Refused for a record no state object may hold and, once the file is served, for any record but the same one.
func (self *ComputeModelBlobStore) PutFileRecord(ctx context.Context, sha256Text string, record *handler.ComputeModelFileRecord) error {
	if record == nil {
		return errors.New("compute model mirror: no state")
	}
	if err := computeModelStateValid(record); err != nil {
		return err
	}
	store, err := self.blobStore()
	if err != nil {
		return err
	}
	key, err := computeModelStateKey(store, sha256Text)
	if err != nil {
		return err
	}
	if served, err := self.served(ctx, store, sha256Text); err != nil {
		return err
	} else if served != nil {
		if *served == *record {
			return nil
		}
		return ErrComputeModelFileServed
	}
	data, err := json.Marshal(&computeModelStateObject{IndexSha256: record.IndexSha256, State: string(record.State)})
	if err != nil {
		return err
	}
	staged, err := os.CreateTemp("", "compute-model-state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	if _, err := staged.Write(data); err != nil {
		staged.Close()
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	return store.Put(ctx, key, staged.Name(), computeModelStateContentType)
}
