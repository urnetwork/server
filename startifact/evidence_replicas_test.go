package startifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/server"
)

// Observe real source inodes, conditional writes and opened winner readers.
// Hooks run inside the synchronous owner; no clock or substitute store decides
// whether a publication succeeded.
type stagedEvidenceReplicaTest struct {
	*preparedEvidenceStoreTest
	want       []byte
	paths      []string
	files      []os.FileInfo
	beforePut  func(string, string) error
	afterPut   func(string, string) error
	beforeGet  func()
	afterClose func()
}

func (self *stagedEvidenceReplicaTest) PutIfAbsent(ctx context.Context, key, path, contentType string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	self.paths = append(self.paths, path)
	self.files = append(self.files, info)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return false, errors.New("replica source lost its private regular-file ownership")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, self.want) {
		return false, errors.Join(errors.New("replica source changed its authenticated wire"), err)
	}
	if self.beforePut != nil {
		if err := self.beforePut(key, path); err != nil {
			return false, err
		}
	}
	created, err := self.preparedEvidenceStoreTest.PutIfAbsent(ctx, key, path, contentType)
	if err == nil && self.afterPut != nil {
		err = self.afterPut(key, path)
	}
	return created, err
}

func (self *stagedEvidenceReplicaTest) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if self.beforeGet != nil {
		self.beforeGet()
	}
	reader, err := self.preparedEvidenceStoreTest.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return &stagedEvidenceReplicaReaderTest{ReadCloser: reader, afterClose: self.afterClose}, nil
}

type stagedEvidenceReplicaReaderTest struct {
	io.ReadCloser
	afterClose func()
}

func (self *stagedEvidenceReplicaReaderTest) Close() error {
	err := self.ReadCloser.Close()
	if self.afterClose != nil {
		self.afterClose()
	}
	return err
}

func newStagedEvidenceReplicaTest(t *testing.T, root, prefix string, wire []byte) *stagedEvidenceReplicaTest {
	t.Helper()
	return &stagedEvidenceReplicaTest{preparedEvidenceStoreTest: &preparedEvidenceStoreTest{BlobStore: server.NewLocalBlobStore(root, prefix)}, want: bytes.Clone(wire)}
}

func TestPreparedEvidenceReplicasShareOneOwnedStageAndKeepEveryReadback(t *testing.T) {
	envelope := preparedEvidenceEnvelopeTest(t, "replica-stage")
	prepared, err := PrepareEvidence(envelope)
	if err != nil {
		t.Fatal(err)
	}
	first := newStagedEvidenceReplicaTest(t, t.TempDir(), "operator-1", prepared.encoded)
	second := newStagedEvidenceReplicaTest(t, t.TempDir(), "operator-2", prepared.encoded)
	stores := []server.BlobStore{first, second}
	first.afterPut = func(string, string) error {
		clear(envelope.Payload)
		*envelope = EvidenceEnvelope{}
		stores[1] = nil
		return nil
	}
	second.beforePut = func(string, string) error {
		if first.puts != 2 || first.gets != 4 || first.closes != 4 {
			return errors.New("later replica overtook an original route or direct readback")
		}
		return nil
	}
	if err := prepared.PublishAndVerifyReplicas(t.Context(), stores); err != nil {
		t.Fatal(err)
	}
	for _, store := range []*stagedEvidenceReplicaTest{first, second} {
		if len(store.paths) != 2 || store.puts != 2 || store.gets != 4 || store.closes != 4 {
			t.Fatal("replica work omitted a conditional write or an owned stored read")
		}
		for index, path := range store.paths {
			if path != first.paths[0] || !os.SameFile(store.files[index], first.files[0]) {
				t.Fatal("one envelope was staged repeatedly across immutable routes")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("replica operation retained its private stage", err)
			}
		}
	}
	if prepared, err := PrepareEvidence(envelope); prepared != nil || err == nil {
		t.Fatal("later invocation reused authentication for the mutated source")
	}
}

// Original winner collisions, writes, reads and final Close still fail the
// whole object. Every path owns the same cleanup even after a late refusal.
func TestPreparedEvidenceReplicasPreserveFailuresCancellationAndStageCleanup(t *testing.T) {
	for _, fault := range []string{"write", "collision", "winner-read", "winner-close", "direct-read", "direct-close", "stage-change", "cancel", "late-cancel"} {
		t.Run(fault, func(t *testing.T) {
			prepared, err := PrepareEvidence(preparedEvidenceEnvelopeTest(t, "replica-stage"))
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			first := newStagedEvidenceReplicaTest(t, root, "operator-1", prepared.encoded)
			second := newStagedEvidenceReplicaTest(t, t.TempDir(), "operator-2", prepared.encoded)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sentinel := errors.New("actual replica I/O refusal")
			switch fault {
			case "write":
				first.beforePut = func(string, string) error { return sentinel }
			case "collision":
				publication, err := prepared.publication(first)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, filepath.FromSlash(publication.ContentKey))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("different immutable winner"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "winner-read":
				first.readErr = sentinel
			case "winner-close":
				first.closeErr = sentinel
			case "direct-read", "direct-close":
				first.beforeGet = func() {
					if first.gets == 2 {
						if fault == "direct-read" {
							first.readErr = sentinel
						} else {
							first.closeErr = sentinel
						}
					}
				}
			case "stage-change":
				first.beforePut = func(_ string, path string) error {
					if first.puts == 1 {
						return os.WriteFile(path, []byte("changed staging source"), 0o600)
					}
					return nil
				}
			case "cancel":
				first.afterPut = func(string, string) error { cancel(); return nil }
			case "late-cancel":
				second.afterClose = func() {
					if second.closes == 4 {
						cancel()
					}
				}
			}
			err = prepared.PublishAndVerifyReplicas(ctx, []server.BlobStore{first, second})
			if err == nil {
				t.Fatal("replica refusal became successful publication")
			}
			if (fault == "cancel" || fault == "late-cancel") && !errors.Is(err, context.Canceled) {
				t.Fatal("replica owner lost cancellation", err)
			}
			if fault != "collision" && fault != "stage-change" && fault != "cancel" && fault != "late-cancel" && !errors.Is(err, sentinel) {
				t.Fatal("replica owner lost original I/O refusal", err)
			}
			for _, store := range []*stagedEvidenceReplicaTest{first, second} {
				if store.gets != store.closes {
					t.Fatal("replica refusal abandoned an acquired reader")
				}
				for _, path := range store.paths {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("failed publication retained its staging file", err)
					}
				}
			}
			if len(first.paths) == 0 || fault != "late-cancel" && second.puts != 0 {
				t.Fatal("failure missed the actual staging boundary or admitted a later replica")
			}
			if fault == "collision" {
				publication, err := prepared.publication(first)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(publication.ContentKey)))
				if err != nil || string(raw) != "different immutable winner" {
					t.Fatal("replica publication overwrote its conflicting winner", err)
				}
			}
		})
	}
}

func TestPreparedEvidenceReplicasRejectInvalidOwnershipBeforeStorage(t *testing.T) {
	prepared, err := PrepareEvidence(preparedEvidenceEnvelopeTest(t, "replica-stage"))
	if err != nil {
		t.Fatal(err)
	}
	store := newStagedEvidenceReplicaTest(t, t.TempDir(), "operator-1", prepared.encoded)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, input := range []struct {
		owner *PreparedEvidence
		ctx context.Context
		stores []server.BlobStore
	}{
		{owner: prepared, ctx: nil, stores: []server.BlobStore{store}},
		{owner: prepared, ctx: canceled, stores: []server.BlobStore{store}},
		{owner: prepared, ctx: t.Context()},
		{owner: prepared, ctx: t.Context(), stores: make([]server.BlobStore, 129)},
		{owner: prepared, ctx: t.Context(), stores: []server.BlobStore{store, nil}},
		{owner: nil, ctx: t.Context(), stores: []server.BlobStore{store}},
		{owner: &PreparedEvidence{}, ctx: t.Context(), stores: []server.BlobStore{store}},
	} {
		if err := input.owner.PublishAndVerifyReplicas(input.ctx, input.stores); err == nil || len(store.paths) != 0 || store.gets != 0 {
			t.Fatal("incomplete replica owner acquired storage")
		}
	}
}
