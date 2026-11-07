//go:build linux

// Exercise the real scrubber and exec inheritance without changing stdout/stderr.
package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The retained destination must survive for forwarding/restoration, but not exec.
func TestScrubDescriptorOriginalDestinationNotInherited(t *testing.T) {
	sink, err := os.CreateTemp(t.TempDir(), "synthetic-scrub-destination")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	var identity syscall.Stat_t
	if err := syscall.Fstat(int(sink.Fd()), &identity); err != nil {
		t.Fatal(err)
	}
	restore, err := scrubDescriptor(int(sink.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	restore = sync.OnceFunc(restore)
	defer restore()
	if _, err := io.WriteString(sink, "before 203.0.113.9\n"); err != nil {
		t.Fatal(err)
	}
	runScrubDescriptorExecChild(t, identity, false, nil)
	if _, err := io.WriteString(sink, "after 203.0.113.9\n"); err != nil {
		t.Fatal(err)
	}
	restore()
	if _, err := io.WriteString(sink, "restored\n"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(sink.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "before [scrubbed]\nafter [scrubbed]\nrestored\n" {
		t.Fatal("forwarding or restoration lost the original destination")
	}
}

// Passing a synthetic destination explicitly proves the child oracle detects it.
func TestScrubDescriptorExecOracleDetectsExplicitDestination(t *testing.T) {
	sink, err := os.CreateTemp(t.TempDir(), "synthetic-explicit-destination")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	var identity syscall.Stat_t
	if err := syscall.Fstat(int(sink.Fd()), &identity); err != nil {
		t.Fatal(err)
	}
	runScrubDescriptorExecChild(t, identity, true, sink)
}

// Launch only the synthetic child test; ExtraFiles is solely the positive oracle.
func runScrubDescriptorExecChild(t *testing.T, identity syscall.Stat_t, expected bool, extra *os.File) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestScrubDescriptorExecChild$")
	command.Env = append(os.Environ(), "UR_SYNTHETIC_SCRUB_CHILD=1",
		fmt.Sprintf("UR_SYNTHETIC_SCRUB_DEVICE=%d", identity.Dev),
		fmt.Sprintf("UR_SYNTHETIC_SCRUB_INODE=%d", identity.Ino),
		fmt.Sprintf("UR_SYNTHETIC_SCRUB_EXPECT=%t", expected))
	if extra != nil {
		command.ExtraFiles = []*os.File{extra}
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("synthetic exec descriptor control failed: %v: %s", err, output)
	}
}

// Inspect only this synthetic exec child's descriptors, never a running service.
func TestScrubDescriptorExecChild(t *testing.T) {
	if os.Getenv("UR_SYNTHETIC_SCRUB_CHILD") != "1" {
		t.Skip("synthetic subprocess helper")
	}
	device, err := strconv.ParseUint(os.Getenv("UR_SYNTHETIC_SCRUB_DEVICE"), 10, 64)
	if err != nil {
		t.Fatal("invalid synthetic device")
	}
	inode, err := strconv.ParseUint(os.Getenv("UR_SYNTHETIC_SCRUB_INODE"), 10, 64)
	if err != nil {
		t.Fatal("invalid synthetic inode")
	}
	expected, err := strconv.ParseBool(os.Getenv("UR_SYNTHETIC_SCRUB_EXPECT"))
	if err != nil {
		t.Fatal("invalid synthetic expectation")
	}
	descriptors, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, descriptor := range descriptors {
		fd, err := strconv.Atoi(descriptor.Name())
		if err != nil || fd < 3 {
			continue
		}
		var identity syscall.Stat_t
		if err := syscall.Fstat(fd, &identity); err != nil {
			if err == syscall.EBADF {
				continue
			}
			t.Fatal(err)
		}
		if uint64(identity.Dev) == device && identity.Ino == inode && identity.Mode&syscall.S_IFMT == syscall.S_IFREG {
			found = true
		}
	}
	if found != expected {
		t.Fatal("synthetic original-destination inheritance differs from expectation")
	}
}
