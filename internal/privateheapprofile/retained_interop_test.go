package privateheapprofile

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestRetainedGoPythonInterop(t *testing.T) {
	reader := os.Getenv("PRIVATE_REPLAY_INTEROP_ROOT")
	if reader == "" {
		t.Skip("external protocol fixture not selected")
	}
	var calls atomic.Int32
	s := testServer(t, func(*net.UnixConn) bool { return true }, func(context.Context, func() Companion) (Capture, error) { calls.Add(1); return replayFixture(), nil })
	identity, err := json.Marshal(testIdentity())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), CaptureBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", "-B", filepath.Join(reader, "interop_client.py"), s.path, string(identity))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("local Go/Python protocol fixture failed: %v: %s", err, output)
	}
	if calls.Load() != 1 {
		t.Fatal("interop reran sampler")
	}
	t.Log(string(output))
}
