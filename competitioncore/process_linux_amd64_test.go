// Held-proc and no-follow regressions use only private synthetic files. Native
// pidfd syscalls, coredumps, process moves and kernel cgroups are never invoked.
package competitioncore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func testStat(pid, birth uint64) []byte {
	fields := make([]string, 20)
	for index := range fields {
		fields[index] = "0"
	}
	fields[0], fields[19] = "R", strconv.FormatUint(birth, 10)
	return []byte(fmt.Sprintf("%d (synthetic (comm) name) %s\n", pid, strings.Join(fields, " ")))
}

func newTestProcessLease(t *testing.T) (*nativeProcess, string, *directory) {
	t.Helper()
	path := t.TempDir()
	processPath := filepath.Join(path, "4321")
	if err := os.Mkdir(processPath, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"stat": testStat(4321, 777), "status": []byte("Pid:\t4321\nTgid:\t4321\nNSpid:\t4321 23\nUid:\t1000 1000 1000 1000\nGid:\t1001 1001 1001 1001\nCoreDumping:\t1\n"), "cgroup": []byte("0::/system.slice/synthetic-ordinary.service\n")} {
		if err := os.WriteFile(filepath.Join(processPath, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := openDirectory(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	invocation, err := ParseInvocation(testArguments(), DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := newProcessLease(root, invocation, nil, DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	lease.checkPidfd = func() error { return nil }
	t.Cleanup(func() { lease.Close() })
	return lease, processPath, root
}

func TestHeldProcLeaseBindsPidBirthCredentialsAndCgroup(t *testing.T) {
	lease, _, _ := newTestProcessLease(t)
	facts, err := lease.Facts(t.Context())
	if err != nil || facts.HostPid != 4321 || facts.NamespacePid != 23 || facts.Birth != 777 || facts.Uid != 1000 || facts.Gid != 1001 || facts.Cgroup != "/system.slice/synthetic-ordinary.service" {
		t.Fatal("held proc lease did not retain its exact kernel-shaped facts", facts, err)
	}
	lease.checkPidfd = func() error { return errors.New("synthetic dead pidfd") }
	if _, err := lease.Facts(t.Context()); err == nil {
		t.Fatal("dead pidfd became positive ordinary authority")
	}
}

func TestHeldProcLeaseRefusesDisappearanceAndPidDirectoryReuse(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		lease, path, _ := newTestProcessLease(t)
		if err := os.Rename(path, path+"-old"); err != nil {
			t.Fatal(err)
		}
		if reuse {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "stat"), testStat(4321, 888), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := lease.Facts(t.Context()); err == nil {
			t.Fatal("disappeared/reused process directory passed the held identity", reuse)
		}
	}
}

func TestHeldProcLeaseRefusesBirthOrNamespaceCredentialChanges(t *testing.T) {
	for _, fault := range []string{"birth", "namespace", "uid", "not-coredumping", "duplicate-status"} {
		lease, path, _ := newTestProcessLease(t)
		if fault == "birth" {
			if err := os.WriteFile(filepath.Join(path, "stat"), testStat(4321, 778), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			content, err := os.ReadFile(filepath.Join(path, "status"))
			if err != nil {
				t.Fatal(err)
			}
			value := string(content)
			switch fault {
			case "namespace":
				value = strings.Replace(value, "4321 23", "4321 24", 1)
			case "uid":
				value = strings.Replace(value, "Uid:\t1000", "Uid:\t1002", 1)
			case "not-coredumping":
				value = strings.Replace(value, "CoreDumping:\t1", "CoreDumping:\t0", 1)
			case "duplicate-status":
				value += "Pid:\t4321\n"
			}
			if err := os.WriteFile(filepath.Join(path, "status"), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := lease.Facts(t.Context()); err == nil {
			t.Fatal("changed proc generation/credentials qualified", fault)
		}
	}
}

func TestHeldProcLeaseReportsMigrationForCrossObservationRefusal(t *testing.T) {
	lease, path, _ := newTestProcessLease(t)
	before, err := lease.Facts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup"), []byte("0::/system.slice/synthetic-other.service\n"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err := lease.Facts(t.Context())
	if err != nil || before == after || after.Birth != before.Birth {
		t.Fatal("migration was hidden by a cached pathname/generation", before, after, err)
	}
}

// Both a leaf link and a relative ancestor link must fail, even when the link
// stays inside the same filesystem/root and the target contains valid facts.
func TestRawProcReadsRejectConfinedRelativeLeafAndAncestorSymlinks(t *testing.T) {
	lease, path, root := newTestProcessLease(t)
	if err := os.Rename(filepath.Join(path, "cgroup"), filepath.Join(path, "actual-cgroup")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("actual-cgroup", filepath.Join(path, "cgroup")); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Facts(t.Context()); err == nil {
		t.Fatal("relative proc leaf symlink bypassed no-follow")
	}
	if err := os.Symlink("4321", filepath.Join(filepath.Dir(path), "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := readRelative(root, "alias/stat", 4096, false); err == nil {
		t.Fatal("relative proc ancestor symlink bypassed no-follow")
	}
	if linked, err := root.OpenDirectory("alias"); err == nil {
		linked.Close()
		t.Fatal("relative proc directory symlink became a held generation")
	}
}

func TestRawRegistryReadRejectsRelativeControlSymlinkAndPathEscape(t *testing.T) {
	store, path := newTestStore(t)
	if err := os.Rename(filepath.Join(path, "registry.json"), filepath.Join(path, "actual-registry.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("actual-registry.json", filepath.Join(path, "registry.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(t.Context()); err == nil {
		t.Fatal("relative registry leaf symlink became protected authority")
	}
	for _, name := range []string{"../actual-registry.json", "/registry.json", "./actual-registry.json", "a/../actual-registry.json"} {
		if _, err := readRelative(store.root, name, 4096, false); err == nil {
			t.Fatal("descriptor-relative metadata escaped its exact path", name)
		}
	}
}

func TestProcFactsRejectNonfiniteOversizedAndAmbiguousKernelViews(t *testing.T) {
	settings := DefaultSettings()
	for _, value := range []string{"0::/a\n0::/b\n", "1:name=x:/a\n", "0::/a/../b\n", "0::/a//b\n", "0::relative\n"} {
		if _, err := parseProcessCgroup([]byte(value), settings); err == nil {
			t.Fatal("ambiguous proc cgroup view became ordinary authority", value)
		}
	}
	lease, path, _ := newTestProcessLease(t)
	if err := os.WriteFile(filepath.Join(path, "status"), []byte(strings.Repeat("x", int(settings.ProcBytes)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Facts(t.Context()); err == nil {
		t.Fatal("oversized proc metadata escaped its finite read bound")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := lease.Facts(ctx); err == nil {
		t.Fatal("canceled proc owner granted positive authority")
	}
}
