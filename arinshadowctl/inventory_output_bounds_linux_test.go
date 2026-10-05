// Copy and actual child-pipe tests preserve host inventory output admission.
package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strconv"
	"testing"
	"time"
)

// A reader-only source exposes an accidentally promoted destination ReadFrom.
func TestCaptureInventoryCopyRejectsExcessOutput(t *testing.T) {
	output := &captureBoundedOutput{limit: 8}
	input := struct{ io.Reader }{Reader: bytes.NewReader([]byte("123456789"))}
	n, err := io.Copy(output, input)
	if err == nil || n > 8 || len(output.Bytes()) > 8 {
		t.Fatal("copy bypassed the inventory output bound", n, len(output.Bytes()), err)
	}
}

// Exact-limit metadata remains valid through the same copy dispatch.
func TestCaptureInventoryCopyPreservesExactLimit(t *testing.T) {
	output := &captureBoundedOutput{limit: 8}
	input := struct{ io.Reader }{Reader: bytes.NewReader([]byte("12345678"))}
	if n, err := io.Copy(output, input); err != nil || n != 8 || string(output.Bytes()) != "12345678" {
		t.Fatal("copy changed exact bounded inventory", n, string(output.Bytes()), err)
	}
}

// Only explicitly selected child invocations emit a finite synthetic stream.
func TestCaptureInventoryOutputChild(t *testing.T) {
	raw := os.Getenv("URNETWORK_TEST_CAPTURE_OUTPUT_BYTES")
	if raw == "" {
		return
	}
	remaining, err := strconv.Atoi(raw)
	if err != nil || remaining < 1 || remaining > 256*1024+1 {
		os.Exit(2)
	}
	chunk := bytes.Repeat([]byte{0x63}, 16*1024)
	for remaining > 0 {
		n, err := os.Stdout.Write(chunk[:min(remaining, len(chunk))])
		if err != nil {
			os.Exit(0)
		}
		remaining -= n
	}
	os.Exit(0)
}

// The real capture command path must refuse excess before publishing bytes.
func TestCaptureDockerRejectsOversizedChildOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_TEST_CAPTURE_OUTPUT_BYTES", "262145")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	output, err := captureDocker(ctx, executable, "-test.run=^TestCaptureInventoryOutputChild$")
	if err == nil || output != nil {
		t.Fatal("actual child pipe bypassed the inventory output bound", len(output), err)
	}
}

// Small metadata remains readable through the same actual pipe owner.
func TestCaptureDockerPreservesBoundedChildOutput(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("URNETWORK_TEST_CAPTURE_OUTPUT_BYTES", "8")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	output, err := captureDocker(ctx, executable, "-test.run=^TestCaptureInventoryOutputChild$")
	if err != nil || !bytes.Equal(output, bytes.Repeat([]byte{0x63}, 8)) {
		t.Fatal("actual child pipe changed bounded inventory", string(output), err)
	}
}
