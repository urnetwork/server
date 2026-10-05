//go:build linux

// Real descriptors and explicit barriers cover policy size/type/cancellation
// before a parser or approved-key consumer can observe resource bytes.
package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Ordinary policy input is private and small; the override owns no filesystem.
func TestSimpleResourceBoundedReadPreservesDetachedOverride(t *testing.T) {
	original := []byte("public policy")
	resource := &SimpleResource{path: "/absent/resource", override: original}
	raw, err := resource.BytesBoundedE(t.Context(), 64)
	if err != nil || !bytes.Equal(raw, original) {
		t.Fatal("pushed resource override was lost", err)
	}
	raw[0] ^= 1
	if bytes.Equal(raw, original) {
		t.Fatal("bounded read exposed mutable pushed bytes")
	}
	if raw, err := resource.BytesBoundedE(t.Context(), 1); err == nil || raw != nil {
		t.Fatal("oversized override passed its caller's bound")
	}
}

// The opened descriptor's size is checked before the first read or allocation.
func TestSimpleResourceBoundedReadRejectsOversizeBeforeReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yml")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, 65), 0600); err != nil {
		t.Fatal(err)
	}
	read := false
	raw, err := (&SimpleResource{path: path}).bytesBounded(t.Context(), 64, boundedResourceHooks{afterRead: func() { read = true }})
	if err == nil || raw != nil || read {
		t.Fatal("oversized policy was read before admission", err, read)
	}
}

// A replaced resolver path cannot block on a FIFO between stat and open.
func TestSimpleResourceBoundedReadRejectsRacedFifo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority.yml")
	if err := os.WriteFile(path, []byte("old policy"), 0600); err != nil {
		t.Fatal(err)
	}
	resource := &SimpleResource{path: path}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := resource.BytesBoundedE(t.Context(), 64)
	if err == nil || raw != nil {
		t.Fatal("raced FIFO became policy bytes")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if raw, err := resource.BytesBoundedE(t.Context(), 64); err == nil || raw != nil {
		t.Fatal("raced directory became policy bytes")
	}
}

// Projected config links retain their existing resolver behavior.
func TestSimpleResourceBoundedReadAllowsProjectedRegularSymlink(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "original.yml")
	if err := os.WriteFile(path, []byte("approved public resource"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "policy.yml")
	if err := os.Symlink("original.yml", link); err != nil {
		t.Fatal(err)
	}
	raw, err := (&SimpleResource{path: link}).BytesBoundedE(t.Context(), 64)
	if err != nil || string(raw) != "approved public resource" {
		t.Fatal("projected regular policy was refused", err)
	}
}

// Cancellation after a real read prevents even already-read bytes from escaping.
func TestSimpleResourceBoundedReadCancellationDiscardsOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yml")
	if err := os.WriteFile(path, []byte("approved public resource"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	raw, err := (&SimpleResource{path: path}).bytesBounded(ctx, 64, boundedResourceHooks{afterRead: cancel})
	if !errors.Is(err, context.Canceled) || raw != nil {
		t.Fatal("canceled resource read published bytes", err)
	}
}

// An admitted file cannot grow into an unbounded parser input during the read.
func TestSimpleResourceBoundedReadRejectsGrowthAfterAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yml")
	if err := os.WriteFile(path, []byte("small"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := (&SimpleResource{path: path}).bytesBounded(t.Context(), 64, boundedResourceHooks{afterOpen: func(*os.File) {
		if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, 65), 0600); err != nil {
			t.Fatal(err)
		}
	}})
	if err == nil || raw != nil {
		t.Fatal("growing resource bypassed the byte limit")
	}
}
