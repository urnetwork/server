// Failure-only stack evidence stays in an exclusive stage-owned artifact.
// It never changes SDK ownership, join deadlines or the original test error.
package perfvar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The first line describes the following raw stack bytes; no raw error,
// identity, credential or stack is copied into ordinary test output.
type sdkBrowsingDeviceJoinStackHeader struct {
	Version        int    `json:"version"`
	Phase          string `json:"phase"`
	ErrorClass     string `json:"error_class"`
	StartedUnixNs  int64  `json:"started_unix_ns"`
	FinishedUnixNs int64  `json:"finished_unix_ns"`
	DurationNs     int64  `json:"duration_ns"`
	CapacityBytes  int    `json:"capacity_bytes"`
	StackBytes     int    `json:"stack_bytes"`
	Truncated      bool   `json:"truncated"`
	StackSha256    string `json:"stack_sha256"`
	Scope          string `json:"scope"`
	SdkJoinProven  bool   `json:"sdk_join_proven"`
}

// The actual fixture invokes this only after its failed device join. Keeping
// the nil guard first also makes a successful direct call entirely inert.
func sdkBrowsingCaptureFailedDeviceJoin(t testing.TB, joinErr error, stack func([]byte, bool) int) {
	if joinErr == nil {
		return
	}
	t.Helper()
	path := os.Getenv("CONNECT_PERFVAR_DEVICE_JOIN_STACK_PATH")
	if path == "" {
		t.Logf("[perfvar] sdk-device-join-stack status=disabled sdk_join_proven=false")
		return
	}
	header, err := sdkBrowsingWriteDeviceJoinStack(path, joinErr, stack)
	if err != nil {
		t.Logf("[perfvar] sdk-device-join-stack status=unavailable error_class=%T sdk_join_proven=false", err)
		return
	}
	t.Logf("[perfvar] sdk-device-join-stack status=captured bytes=%d truncated=%t sha256=%s sdk_join_proven=false", header.StackBytes, header.Truncated, header.StackSha256)
}

// Only the caller's new absolute file is written. The single fixed stack
// buffer is never grown or retried; Sync and Close errors remain unavailable
// capture evidence. No SDK operation, goroutine, wait or timeout is introduced.
func sdkBrowsingWriteDeviceJoinStack(path string, joinErr error, stack func([]byte, bool) int) (header sdkBrowsingDeviceJoinStackHeader, resultErr error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || stack == nil {
		return header, errors.New("invalid device join stack target or producer")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return header, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if err := file.Chmod(0600); err != nil {
		return header, err
	}
	buffer := make([]byte, 2*1024*1024)
	started := time.Now()
	count := stack(buffer, true)
	finished := time.Now()
	if count <= 0 || len(buffer) < count {
		return header, errors.New("invalid device join stack byte count")
	}
	errorClass := "other"
	if errors.Is(joinErr, context.DeadlineExceeded) {
		errorClass = "deadline"
	} else if errors.Is(joinErr, context.Canceled) {
		errorClass = "canceled"
	}
	digest := sha256.Sum256(buffer[:count])
	header = sdkBrowsingDeviceJoinStackHeader{
		Version:        1,
		Phase:          "device_close_and_wait",
		ErrorClass:     errorClass,
		StartedUnixNs:  started.UnixNano(),
		FinishedUnixNs: finished.UnixNano(),
		DurationNs:     finished.Sub(started).Nanoseconds(),
		CapacityBytes:  len(buffer),
		StackBytes:     count,
		Truncated:      count == len(buffer),
		StackSha256:    hex.EncodeToString(digest[:]),
		Scope:          "diagnostic_only",
		SdkJoinProven:  false,
	}
	wire, err := json.Marshal(header)
	if err != nil || 1024 < len(wire) {
		return header, errors.New("invalid device join stack header")
	}
	wire = append(wire, '\n')
	if count, err := file.Write(wire); err != nil {
		return header, err
	} else if count != len(wire) {
		return header, io.ErrShortWrite
	}
	if written, err := file.Write(buffer[:count]); err != nil {
		return header, err
	} else if written != count {
		return header, io.ErrShortWrite
	}
	return header, file.Sync()
}
