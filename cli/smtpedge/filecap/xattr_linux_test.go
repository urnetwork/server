//go:build linux

// On linux, through the kernel: a fresh file holds no attribute; one written reads back as the helper wrote it and
// as getcap prints it; a chown drops it, which is why nothing may touch smtpedge's binary after the image's check;
// and SO_MARK is refused without the capability. Writing the attribute needs CAP_SETFCAP, which root holds in a
// build stage or a container; a process without it gets the kernel's refusal, and the rest is skipped.

package filecap

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestFileCapabilityOnALinuxFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bringyour-smtpedge")
	if err := os.WriteFile(path, []byte("#!/bin/false\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileCapability(path); !errors.Is(err, ErrNoFileCapability) {
		t.Fatalf("a fresh file answered %v", err)
	}
	err := SetFileCapability(path, NetAdminFileCapability())
	if errors.Is(err, syscall.EPERM) {
		t.Skipf("this process may not write %s: %v", AttributeName, err)
	}
	if err != nil {
		t.Fatal(err)
	}
	value, err := ReadFileCapability(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckNetAdmin(value); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeFileCapability(value)
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.String(); got != "cap_net_admin=ep" {
		t.Fatalf("getcap prints %q", got)
	}
	// the kernel drops a file's capability when its owner is set, even to the same owner
	if err := os.Chown(path, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFileCapability(path); !errors.Is(err, ErrNoFileCapability) {
		t.Fatalf("after a chown the attribute answered %v", err)
	}
}

// SO_MARK needs CAP_NET_ADMIN, or CAP_NET_RAW from Linux 5.17: a process with neither is refused with EPERM, the
// error smtpedge's first marked dial would meet as uid 65532 without the file capability.
func TestProbeSoMarkRefusedWithoutTheCapability(t *testing.T) {
	err := ProbeSoMark()
	if err == nil {
		t.Skip("this process holds CAP_NET_ADMIN or CAP_NET_RAW")
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("SO_MARK without CAP_NET_ADMIN answered %v", err)
	}
}
