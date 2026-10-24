// A shell can read its program lazily after an external command returns.
// Explicit FIFOs reproduce mutation at that boundary, without scheduler sleeps.
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The normal trusted-program launcher executes either the mutable original
// (negative control) or its separately published immutable snapshot.
func exerciseSourceSupervisorShellSnapshot(t *testing.T, snapshot bool) string {
	t.Helper()
	root := t.TempDir()
	readyPath, gatePath := filepath.Join(root, "ready.fifo"), filepath.Join(root, "gate.fifo")
	for _, path := range []string{readyPath, gatePath} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	gate, err := os.OpenFile(gatePath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	program := []byte("#!/bin/bash\nset -eu\nprintf x > \"$1\"\nIFS= read -r proceed < \"$2\"\n#" + strings.Repeat("padding", 1024*1024) + "\nprintf '{\"stable\":true}\\n'\n")
	original, executable := filepath.Join(root, "original.sh"), filepath.Join(root, "original.sh")
	if err := writePrivateReviewFile(original, program); err != nil {
		t.Fatal(err)
	}
	if snapshot {
		executable = filepath.Join(root, "installed-snapshot.sh")
		if err := writePrivateReviewFile(executable, program); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(executable, 0o500); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(program))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() {
		ready.Close()
		gate.Close()
	})
	defer stopClose()
	type outcome struct {
		output []byte
		err    error
	}
	result, finished := make(chan outcome, 1), make(chan struct{})
	go func() {
		defer close(finished)
		output, err := runSourceSupervisorProgramArguments(ctx, executable, digest, nil, []string{"PATH=/usr/bin:/bin", "LANG=C"}, []string{readyPath, gatePath})
		result <- outcome{output: output, err: err}
	}()
	t.Cleanup(func() {
		cancel()
		<-finished
	})
	var marker [1]byte
	if _, err := io.ReadFull(ready, marker[:]); err != nil || marker[0] != 'x' {
		t.Fatal("program did not reach the explicit unread-tail boundary", err)
	}
	// Emptying the live source is the deterministic negative case: a lazy
	// interpreter loses its unread tail, while an installed copy is unaffected.
	if err := os.Chmod(original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Write([]byte("continue\n")); err != nil {
		t.Fatal(err)
	}
	observed := <-result
	if observed.err != nil {
		t.Fatal(observed.err)
	}
	return strings.TrimSpace(string(observed.output))
}

// A pre-launch digest check cannot make a subsequently edited script immutable.
func TestSourceSupervisorLiveScriptMutationLosesUnreadTail(t *testing.T) {
	if output := exerciseSourceSupervisorShellSnapshot(t, false); output != "" {
		t.Fatal("mutable-program negative control did not reproduce the unread-tail failure", output)
	}
}

// Publishing a separate immutable program preserves the exact verified bytes.
func TestSourceSupervisorImmutableScriptSnapshotPreservesUnreadTail(t *testing.T) {
	if output := exerciseSourceSupervisorShellSnapshot(t, true); output != `{"stable":true}` {
		t.Fatal("installed snapshot changed when the editable source changed", output)
	}
}
