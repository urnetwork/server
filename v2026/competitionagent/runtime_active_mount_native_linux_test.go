//go:build linux

// Native counterfactual separates initial idle admission from an owned receipt
// recheck after a bounded temporary evidence mount appears beneath jobs.
package competitionagent

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// This creates only a fresh one-MiB temporary mount, never a job or Docker object.
func TestRuntimeActiveEvidenceMountNative(t *testing.T) {
	path := os.Getenv("COMPETITION_ACTIVE_MOUNT_NATIVE_CONFIG")
	if path == "" || os.Geteuid() != 0 {
		t.Skip("requires explicit root idle runtime fixture configuration")
	}
	config, err := LoadDockerProbeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := user.Lookup(config.WorkerUser)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := strconv.ParseUint(worker.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	gid, err := strconv.ParseUint(worker.Gid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	operator, err := trustedOperatorUid(config.TrustedOperatorUser)
	if err != nil {
		t.Fatal(err)
	}
	scope := RuntimeResourceScope{BaseDirectory: config.BaseDirectory, StateDirectory: config.StateDirectory, WorkDirectory: config.WorkDirectory,
		ScratchDirectory: config.DockerRole.ScratchDirectory, WorkerUid: uint32(uid), WorkerGid: uint32(gid), TrustedOperatorUid: operator}
	settings := DefaultRuntimeResourceBoundarySettings()
	receipt, err := VerifyRuntimeResourceBoundary(t.Context(), config.DockerRole.ResourceBoundary, scope, settings)
	if err != nil {
		t.Fatal("idle admission", err)
	}
	defer receipt.Close()
	root, err := os.MkdirTemp(config.WorkDirectory, "synthetic-active-evidence-")
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("synthetic-active-evidence", root, "tmpfs", syscall.MS_NODEV|syscall.MS_NOSUID|syscall.MS_NOEXEC, "size=1048576,mode=0700"); err != nil {
		_ = os.Remove(root)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Unmount(root, 0); err != nil {
			t.Error(err)
			return
		}
		if err := os.Remove(root); err != nil {
			t.Error(err)
		}
	})
	if err := receipt.Recheck(t.Context()); err != nil {
		t.Fatal("owned live receipt rejected ordinary active evidence", err)
	}
	other, err := VerifyRuntimeResourceBoundary(t.Context(), config.DockerRole.ResourceBoundary, scope, settings)
	if other != nil {
		_ = other.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "nested or aliased mount authority") {
		t.Fatalf("fresh idle admission did not reproduce active evidence refusal: %v", err)
	}
	if filepath.Dir(root) != config.WorkDirectory {
		t.Fatal("fixture escaped exact admitted job root")
	}
	t.Log("fresh admission refuses active evidence; retained generation recheck passes")
}
