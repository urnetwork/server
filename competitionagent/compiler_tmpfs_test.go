//go:build linux

// Compiler execution is explicit and confined to its existing bounded work tmpfs.
package competitionagent

import "testing"

// Omitted exec reproduces Docker's default-noexec check-stage failure early.
func TestCompilerTmpfsRequiresExplicitExecution(t *testing.T) {
	fixture := newDockerProxyTest(t)
	options := "rw,nosuid,nodev,uid=65532,gid=65532,mode=0700,size=64m"
	if _, err := fixture.proxy.verifyDockerTmpfs("/work", options, true); err == nil {
		t.Fatal("compiler can silently inherit Docker's default noexec mount")
	}
	if _, err := fixture.proxy.verifyDockerTmpfs("/work", options+",exec", true); err != nil {
		t.Fatal("bounded executable compiler work rejected", err)
	}
}

// Runner, backing service, helper and conflicting mounts retain their boundary.
func TestCompilerTmpfsExecutionCannotEscapeItsRole(t *testing.T) {
	fixture := newDockerProxyTest(t)
	options := "rw,nosuid,nodev,uid=65532,gid=65532,mode=0700,size=64m,exec"
	for _, target := range []string{"/work", "/tmp", "/data", "/var/lib/postgresql"} {
		for _, build := range []bool{false, true} {
			if build && target == "/work" {
				continue
			}
			if _, err := fixture.proxy.verifyDockerTmpfs(target, options, build); err == nil {
				t.Fatalf("executable tmpfs escaped: %q build=%t", target, build)
			}
		}
	}
	if _, err := fixture.proxy.verifyDockerTmpfs("/work", options+",noexec", true); err == nil {
		t.Fatal("conflicting mount execution flags admitted")
	}
}
