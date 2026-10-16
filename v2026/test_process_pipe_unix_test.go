//go:build unix

// Preflight is observational: accepted and rejected handles retain their owner.
package server

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Correct read and write endpoints remain usable with their original payload.
func TestTestProcessPipeAcceptsOwnedDirections(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if err := ValidateTestProcessPipe(reader.Fd(), TestProcessPipeRead); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTestProcessPipe(writer.Fd(), TestProcessPipeWrite); err != nil {
		t.Fatal(err)
	}
	checkTestProcessPipeStillOwned(t, reader, writer)
}

// Refusing an endpoint direction cannot close or consume somebody else's pipe.
func TestTestProcessPipeRejectsWrongDirections(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if err := ValidateTestProcessPipe(reader.Fd(), TestProcessPipeWrite); err == nil {
		t.Fatal("read endpoint accepted for writing")
	}
	if err := ValidateTestProcessPipe(writer.Fd(), TestProcessPipeRead); err == nil {
		t.Fatal("write endpoint accepted for reading")
	}
	checkTestProcessPipeStillOwned(t, reader, writer)
}

// Matching access mode does not turn a regular file into a barrier pipe.
func TestTestProcessPipeRejectsRegularFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "synthetic-file")
	payload := []byte("synthetic descriptor payload")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		access TestProcessPipeAccess
		flags  int
	}{
		{access: TestProcessPipeRead, flags: os.O_RDONLY},
		{access: TestProcessPipeWrite, flags: os.O_WRONLY},
	} {
		func() {
			file, err := os.OpenFile(path, testCase.flags, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := ValidateTestProcessPipe(file.Fd(), testCase.access); err == nil {
				t.Fatalf("regular file accepted for access %d", testCase.access)
			}
			if _, err := file.Stat(); err != nil {
				t.Fatalf("rejection closed regular file: %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}()
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(payload) {
		t.Fatalf("rejection changed regular file: bytes=%d err=%v", len(actual), err)
	}
}

// Invalid numbers are refused without manufacturing an os.File owner.
func TestTestProcessPipeRejectsMissingDescriptor(t *testing.T) {
	for _, access := range []TestProcessPipeAccess{TestProcessPipeRead, TestProcessPipeWrite} {
		if err := ValidateTestProcessPipe(^uintptr(0), access); err == nil {
			t.Fatalf("invalid descriptor accepted for access %d", access)
		}
	}
}

// An unspecified direction is not permission to accept any live descriptor.
func TestTestProcessPipeRejectsUnknownAccess(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if err := ValidateTestProcessPipe(writer.Fd(), TestProcessPipeAccess(0)); err == nil {
		t.Fatal("unspecified direction accepted")
	}
	checkTestProcessPipeStillOwned(t, reader, writer)
}

// A round trip proves preflight neither consumed bytes nor closed endpoints.
func checkTestProcessPipeStillOwned(t testing.TB, reader, writer *os.File) {
	t.Helper()
	payload := []byte("synthetic barrier")
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("preflight closed writer: %v", err)
	}
	actual := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, actual); err != nil || string(actual) != string(payload) {
		t.Fatalf("preflight changed pipe ownership or contents: bytes=%d err=%v", len(actual), err)
	}
}
