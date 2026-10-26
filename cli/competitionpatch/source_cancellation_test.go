// Explicit process/socket barriers prove that source-owner cancellation joins
// Git and a descendant holding its output pipe. No timing race drives success.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Runs only as the synthetic Git executable or its inherited-pipe descendant.
func TestRepositorySourceGitCancellationHelper(t *testing.T) {
	phase := os.Getenv("COMPETITION_TEST_GIT_PHASE")
	if phase == "" {
		return
	}
	if phase == "parent" && !strings.Contains(" "+strings.Join(os.Args, " ")+" ", " -c gc.auto=0 -c maintenance.auto=false ") {
		t.Fatal("trusted Git source operation permits background maintenance")
	}
	connection, err := net.Dial("tcp", os.Getenv("COMPETITION_TEST_GIT_BARRIER"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := fmt.Fprintln(connection, phase); err != nil {
		t.Fatal(err)
	}
	if phase == "parent" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(executable, "-test.run=^TestRepositorySourceGitCancellationHelper$")
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "COMPETITION_TEST_GIT_PHASE=") {
				child.Env = append(child.Env, value)
			}
		}
		child.Env = append(child.Env, "COMPETITION_TEST_GIT_PHASE=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		defer child.Wait()
	}
	_, _ = io.Copy(io.Discard, connection)
}

// Cancellation occurs only after both independent sockets prove that the
// parent and inherited-pipe child are alive. Deadlines are failure backstops.
func TestRepositorySourceCancellationRetiresGitProcessGroup(t *testing.T) {
	root, content, digest, original := repositoryApplyFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	shim := "#!/bin/sh\nexec \"$COMPETITION_TEST_GIT_EXECUTABLE\" -test.run=^TestRepositorySourceGitCancellationHelper$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMPETITION_TEST_GIT_EXECUTABLE", executable)
	t.Setenv("COMPETITION_TEST_GIT_PHASE", "parent")
	t.Setenv("COMPETITION_TEST_GIT_BARRIER", listener.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		patch := []byte("diff --git a/source.txt b/source.txt\n--- a/source.txt\n+++ b/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
		_, err := applyRepositoryPatchesContext(ctx, root, content, digest, original["server"], strings.Repeat("c", 64), "mono_root_v1", map[string][]byte{"connect": patch}, defaultRepositoryApplySettings())
		finished <- err
	}()
	connections := map[string]net.Conn{}
	defer func() {
		for _, connection := range connections {
			connection.Close()
		}
	}()
	for range 2 {
		connection, err := listener.Accept()
		if err != nil {
			t.Fatal("Git process did not reach its explicit barrier:", err)
		}
		if err := connection.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
			t.Fatal(err)
		}
		phase, err := bufio.NewReader(connection).ReadString('\n')
		phase = strings.TrimSpace(phase)
		if err != nil || (phase != "parent" && phase != "child") || connections[phase] != nil {
			connection.Close()
			t.Fatalf("invalid process barrier %q: %v", phase, err)
		}
		connections[phase] = connection
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("source operation lost owner cancellation:", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("canceled Git operation did not join")
	}
	for phase, connection := range connections {
		if _, err := io.ReadAll(connection); err != nil {
			t.Errorf("cancellation left %s process alive: %v", phase, err)
		}
	}
}
