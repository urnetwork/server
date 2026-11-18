// Five finite controls use synthetic stack producers, never runtime stacks,
// SDK diagnostics, service fixtures, goroutine-count assumptions or timing waits.
package perfvar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Embedding preserves testing.TB's private interface method. Only harmless
// diagnostic logs and Helper calls are observed; failures still reach t.
type sdkBrowsingJoinStackLog struct {
	testing.TB
	messages    []string
	helperCalls int
}

// Successful join must not even enter the capture's diagnostic helper path.
func (self *sdkBrowsingJoinStackLog) Helper() { self.helperCalls++ }

// Raw stack/error contents must never become ordinary test output.
func (self *sdkBrowsingJoinStackLog) Logf(format string, arguments ...any) {
	self.messages = append(self.messages, fmt.Sprintf(format, arguments...))
}

// Decode the bounded header separately from its byte-exact raw payload.
func sdkBrowsingReadJoinStack(t *testing.T, path string) (sdkBrowsingDeviceJoinStackHeader, []byte) {
	t.Helper()
	wire, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	metadata, payload, ok := bytes.Cut(wire, []byte{'\n'})
	if !ok || len(metadata) > 1024 || len(wire) > 2*1024*1024+1025 {
		t.Fatal("device join artifact exceeded its fixed envelope")
	}
	var header sdkBrowsingDeviceJoinStackHeader
	if err := json.Unmarshal(metadata, &header); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if header.Version != 1 || header.Phase != "device_close_and_wait" || header.Scope != "diagnostic_only" || header.SdkJoinProven ||
		header.CapacityBytes != 2*1024*1024 || header.StackBytes != len(payload) || header.StackSha256 != hex.EncodeToString(digest[:]) ||
		header.StartedUnixNs <= 0 || header.FinishedUnixNs <= 0 || header.DurationNs < 0 {
		t.Fatalf("device join header or payload integrity changed: %+v", header)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("device join artifact permission changed: %v", err)
	}
	return header, payload
}

// A configured target still costs no helper/log/stack/file work on success.
func TestSdkBrowsingJoinStackSuccessfulJoinIsInert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unused.stack")
	t.Setenv("CONNECT_PERFVAR_DEVICE_JOIN_STACK_PATH", path)
	log := &sdkBrowsingJoinStackLog{TB: t}
	calls := 0
	sdkBrowsingCaptureFailedDeviceJoin(log, nil, func([]byte, bool) int { calls++; return 0 })
	if calls != 0 || log.helperCalls != 0 || len(log.messages) != 0 {
		t.Fatal("successful join entered its diagnostic path")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("successful join touched its artifact path: ", err)
	}
}

// Exact synthetic bytes, error classes, private permissions and safe logs are
// independent of the runtime's current goroutine population or scheduler.
func TestSdkBrowsingJoinStackPreservesExactPrivatePayload(t *testing.T) {
	for _, test := range []struct {
		err   error
		class string
	}{
		{err: context.DeadlineExceeded, class: "deadline"},
		{err: context.Canceled, class: "canceled"},
		{err: errors.New("synthetic private error detail"), class: "other"},
	} {
		path := filepath.Join(t.TempDir(), "failure.stack")
		t.Setenv("CONNECT_PERFVAR_DEVICE_JOIN_STACK_PATH", path)
		log := &sdkBrowsingJoinStackLog{TB: t}
		payload := []byte("synthetic private goroutine receipt\n")
		calls := 0
		sdkBrowsingCaptureFailedDeviceJoin(log, test.err, func(buffer []byte, all bool) int {
			calls++
			if len(buffer) != 2*1024*1024 || !all {
				t.Fatal("stack scope or fixed buffer changed")
			}
			return copy(buffer, payload)
		})
		header, actual := sdkBrowsingReadJoinStack(t, path)
		if calls != 1 || header.ErrorClass != test.class || header.Truncated || !bytes.Equal(actual, payload) ||
			len(log.messages) != 1 || !strings.Contains(log.messages[0], "status=captured") ||
			strings.Contains(log.messages[0], "synthetic private") {
			t.Fatalf("private payload/class/call/log contract changed: calls=%d header=%+v", calls, header)
		}
	}
}

// Filling the fixed buffer is conservatively incomplete, with one producer
// call and no growth/retry or claim that SDK ownership joined.
func TestSdkBrowsingJoinStackFullBufferDeclaresTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.stack")
	t.Setenv("CONNECT_PERFVAR_DEVICE_JOIN_STACK_PATH", path)
	log := &sdkBrowsingJoinStackLog{TB: t}
	calls := 0
	sdkBrowsingCaptureFailedDeviceJoin(log, context.DeadlineExceeded, func(buffer []byte, all bool) int {
		calls++
		if len(buffer) != 2*1024*1024 || !all {
			t.Fatal("capture grew its buffer or narrowed its stack scope")
		}
		buffer[0], buffer[len(buffer)-1] = 'a', 'z'
		return len(buffer)
	})
	header, payload := sdkBrowsingReadJoinStack(t, path)
	if calls != 1 || !header.Truncated || len(payload) != 2*1024*1024 || payload[0] != 'a' || payload[len(payload)-1] != 'z' ||
		len(log.messages) != 1 || !strings.Contains(log.messages[0], "truncated=true") {
		t.Fatal("full capture lost its explicit fixed-size truncation")
	}
}

// Exclusive creation refuses both an existing file and a symlink before the
// producer is invoked; neither target may be overwritten or removed.
func TestSdkBrowsingJoinStackNeverOverwritesExistingTarget(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		directory := t.TempDir()
		target := filepath.Join(directory, "existing")
		original := []byte("synthetic existing owner data")
		if err := os.WriteFile(target, original, 0600); err != nil {
			t.Fatal(err)
		}
		path := target
		if symlink {
			path = filepath.Join(directory, "capture-link")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("CONNECT_PERFVAR_DEVICE_JOIN_STACK_PATH", path)
		log := &sdkBrowsingJoinStackLog{TB: t}
		calls := 0
		sdkBrowsingCaptureFailedDeviceJoin(log, context.DeadlineExceeded, func([]byte, bool) int { calls++; return 0 })
		actual, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(actual, original) || calls != 0 || len(log.messages) != 1 || !strings.Contains(log.messages[0], "status=unavailable") {
			t.Fatal("exclusive refusal changed existing custody: ", err)
		}
		if info, err := os.Lstat(path); err != nil || symlink && info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("exclusive refusal removed or followed its target: ", err)
		}
	}
}

// Missing configuration and invalid/unavailable targets stay diagnostic only.
// Returning from capture leaves the caller's original deadline error intact.
func TestSdkBrowsingJoinStackUnavailableCannotMaskOriginalFailure(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	for _, path := range []string{"", "relative.stack", filepath.Join(directory, "missing", "failure.stack"), directory} {
		t.Setenv("CONNECT_PERFVAR_DEVICE_JOIN_STACK_PATH", path)
		log := &sdkBrowsingJoinStackLog{TB: t}
		calls := 0
		original := context.DeadlineExceeded
		sdkBrowsingCaptureFailedDeviceJoin(log, original, func([]byte, bool) int { calls++; return 0 })
		status := "status=unavailable"
		if path == "" {
			status = "status=disabled"
		}
		if calls != 0 || len(log.messages) != 1 || !strings.Contains(log.messages[0], status) || !strings.Contains(log.messages[0], "sdk_join_proven=false") {
			t.Fatal("capture availability replaced or masked the failed join")
		}
	}
	if _, err := os.Stat(filepath.Join(directory, "relative.stack")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("capture created a fallback relative artifact: ", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("capture created an unowned parent directory: ", err)
	}
}
