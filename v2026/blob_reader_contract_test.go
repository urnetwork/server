package server

// Public Get readers and the capacity copy wrapper must not admit complete
// buffers when their actual post-read custody or context checks fail.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect/v2026/durablevolume"
)

// Keep the actual public Get result and fd; only the post-I/O ordering varies.
func newBlobReaderContractFixture(t *testing.T, ctx context.Context, raw []byte) (*durableBlobFixture, *durableBlobReader) {
	t.Helper()
	fixture := newDurableBlobFixture(t)
	fixture.raw = append([]byte(nil), raw...)
	if err := os.WriteFile(fixture.source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Put(t.Context(), "object", fixture.source, ""); err != nil {
		t.Fatal(err)
	}
	opened, err := fixture.store.Get(ctx, "object")
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := opened.(*durableBlobReader)
	if !ok {
		_ = opened.Close()
		t.Fatal("public Get did not return the guarded reader")
	}
	file := reader.file
	t.Cleanup(func() {
		_ = reader.Close()
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) || reader.volume != nil || reader.parent != nil {
			t.Error("public reader did not release its fd and lease", err)
		}
	})
	return fixture, reader
}

// Cancellation and named replacement occur after a completed real file.Read.
func armBlobReaderContractFault(t *testing.T, fixture *durableBlobFixture, reader *durableBlobReader, cancel context.CancelFunc, replace bool, onRead int) *int {
	t.Helper()
	reads := new(int)
	reader.afterReadForTest = func(n int, err error) {
		*reads++
		if *reads != onRead {
			return
		}
		if onRead == 1 && n != len(fixture.raw) || onRead == 2 && (n != 0 || err != io.EOF) {
			t.Fatal("fault did not follow the intended real read", n, err)
		}
		if !replace {
			cancel()
			return
		}
		path := filepath.Join(fixture.root, "object")
		if err := os.Rename(path, path+".retained"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("unrelated replacement"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return reads
}

func checkBlobReaderContractFault(t *testing.T, fixture *durableBlobFixture, replace bool, err error) {
	t.Helper()
	name, want := "object", context.Canceled
	if replace {
		name, want = "object.retained", durablevolume.ErrIdentity
	}
	if !errors.Is(err, want) {
		t.Fatal("post-read refusal lost its original cause", err)
	}
	raw, readErr := os.ReadFile(filepath.Join(fixture.root, name))
	if readErr != nil || !bytes.Equal(raw, fixture.raw) {
		t.Fatal("refused public read changed original object", raw, readErr)
	}
}

func testBlobReaderFullBufferRefusal(t *testing.T, replace, decode bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture, reader := newBlobReaderContractFixture(t, ctx, []byte(`{"object":"original"}`))
	reads := armBlobReaderContractFault(t, fixture, reader, cancel, replace, 1)
	var err error
	if decode {
		var value map[string]string
		err = json.NewDecoder(reader).Decode(&value)
		if err == nil || value != nil {
			t.Fatalf("public Get JSON accepted bytes after actual post-read refusal: value=%v err=%v", value, err)
		}
	} else {
		n := 0
		n, err = io.ReadFull(reader, make([]byte, len(fixture.raw)))
		if err == nil || n != 0 {
			t.Fatalf("public Get ReadFull accepted bytes after actual post-read refusal: n=%d err=%v", n, err)
		}
	}
	if *reads != 1 {
		t.Fatal("full-buffer fault was not observed once", *reads)
	}
	checkBlobReaderContractFault(t, fixture, replace, err)
}

func TestBlobReaderDurableReadFullRejectsCancellation(t *testing.T) {
	testBlobReaderFullBufferRefusal(t, false, false)
}
func TestBlobReaderDurableReadFullRejectsReplacement(t *testing.T) {
	testBlobReaderFullBufferRefusal(t, true, false)
}
func TestBlobReaderDurableJsonRejectsCancellation(t *testing.T) {
	testBlobReaderFullBufferRefusal(t, false, true)
}
func TestBlobReaderDurableJsonRejectsReplacement(t *testing.T) {
	testBlobReaderFullBufferRefusal(t, true, true)
}

func testBlobReaderPartialEofRefusal(t *testing.T, replace bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture, reader := newBlobReaderContractFixture(t, ctx, []byte("original object"))
	reads := armBlobReaderContractFault(t, fixture, reader, cancel, replace, 2)
	buffer := make([]byte, len(fixture.raw)+1)
	n, err := io.ReadFull(reader, buffer)
	if *reads != 2 || n != len(fixture.raw) || !bytes.Equal(buffer[:n], fixture.raw) || !errors.Is(err, io.EOF) || err == io.EOF {
		t.Fatal("public Get partial EOF lost its prefix or refusal", *reads, n, err)
	}
	checkBlobReaderContractFault(t, fixture, replace, err)
}

func TestBlobReaderDurablePartialEofRetainsCancellation(t *testing.T) {
	testBlobReaderPartialEofRefusal(t, false)
}
func TestBlobReaderDurablePartialEofRetainsReplacement(t *testing.T) {
	testBlobReaderPartialEofRefusal(t, true)
}

func TestBlobReaderDurablePreservesOrdinaryEof(t *testing.T) {
	fixture, reader := newBlobReaderContractFixture(t, t.Context(), []byte("original object"))
	raw, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(raw, fixture.raw) {
		t.Fatal("public Get normal completion changed", raw, err)
	}
	if n, err := reader.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatal("public Get wrapped normal EOF", n, err)
	}
}

// io.Copy already returns a nonnil error for this pattern; no successful copy
// or durable-store publication is inferred from the full-buffer finding.
func TestBlobReaderDurableCopyAlreadyRefusesPostReadLoss(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture, reader := newBlobReaderContractFixture(t, ctx, []byte("original object"))
	armBlobReaderContractFault(t, fixture, reader, cancel, true, 1)
	var output bytes.Buffer
	_, err := io.Copy(&output, reader)
	checkBlobReaderContractFault(t, fixture, true, err)
}

// The capacity reader already accepts an io.Reader. Its test source performs
// an actual file read before canceling, without adding another product seam.
type blobCapacityFileReadFault struct {
	file          *os.File
	cancel        context.CancelFunc
	reads, onRead int
}

func (self *blobCapacityFileReadFault) Read(raw []byte) (int, error) {
	n, err := self.file.Read(raw)
	self.reads++
	if self.reads == self.onRead {
		self.cancel()
	}
	return n, err
}

func newBlobCapacityReaderContract(t *testing.T, onRead int) (*localBlobCapacityReader, *blobCapacityFileReadFault, []byte) {
	t.Helper()
	raw := []byte(`{"object":"original"}`)
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { cancel(); _ = file.Close() })
	source := &blobCapacityFileReadFault{file: file, cancel: cancel, onRead: onRead}
	return &localBlobCapacityReader{ctx: ctx, reader: source}, source, raw
}

func TestBlobReaderCapacityReadFullRejectsCancellation(t *testing.T) {
	reader, source, raw := newBlobCapacityReaderContract(t, 1)
	n, err := io.ReadFull(reader, make([]byte, len(raw)))
	if source.reads != 1 || n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("capacity ReadFull accepted canceled complete buffer", source.reads, n, err)
	}
}

func TestBlobReaderCapacityJsonRejectsCancellation(t *testing.T) {
	reader, source, _ := newBlobCapacityReaderContract(t, 1)
	var value map[string]string
	err := json.NewDecoder(reader).Decode(&value)
	if source.reads != 1 || value != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("capacity JSON accepted canceled complete value", source.reads, value, err)
	}
}

func TestBlobReaderCapacityPartialEofRetainsBothCauses(t *testing.T) {
	reader, source, raw := newBlobCapacityReaderContract(t, 2)
	buffer := make([]byte, len(raw)+1)
	n, err := io.ReadFull(reader, buffer)
	if source.reads != 2 || n != len(raw) || !bytes.Equal(buffer[:n], raw) || !errors.Is(err, io.EOF) || !errors.Is(err, context.Canceled) {
		t.Fatal("capacity partial EOF lost original completion cause", source.reads, n, err)
	}
}

func TestBlobReaderCapacityPreservesOrdinaryEof(t *testing.T) {
	reader, _, raw := newBlobCapacityReaderContract(t, 0)
	actual, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(actual, raw) {
		t.Fatal("capacity copy wrapped normal completion", actual, err)
	}
	if n, err := reader.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatal("capacity reader lost bare EOF", n, err)
	}
}

func TestBlobReaderCapacityCopyAlreadyRefusesCancellation(t *testing.T) {
	reader, _, _ := newBlobCapacityReaderContract(t, 1)
	var output bytes.Buffer
	if _, err := io.Copy(&output, reader); !errors.Is(err, context.Canceled) {
		t.Fatal("capacity Copy acknowledged a canceled source", err)
	}
}
