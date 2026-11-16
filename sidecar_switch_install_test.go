package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A switch's install as a test drives it: the switch, what a build answers,
// and what it built, removed and said.
type testSwitchInstall struct {
	on        bool
	buildErr  error
	builds    int
	removes   int
	contexts  []context.Context
	lines     []string
	installer *SidecarSwitchInstall
}

func newTestSwitchInstall() *testSwitchInstall {
	self := &testSwitchInstall{}
	self.installer = NewSidecarSwitchInstall("part", func() bool {
		return self.on
	}, func(ctx context.Context) (func(), error) {
		self.builds += 1
		if self.buildErr != nil {
			return nil, self.buildErr
		}
		self.contexts = append(self.contexts, ctx)
		return func() {
			self.removes += 1
		}, nil
	}, nil)
	self.installer.log = func(format string, arguments ...any) {
		self.lines = append(self.lines, fmt.Sprintf(format, arguments...))
	}
	return self
}

// Off, nothing is built; on, the part is built once; off again, it is removed
// and its context ended; on again, it is built anew.
func TestSidecarSwitchInstallFollowsTheSwitch(t *testing.T) {
	ctx := context.Background()
	world := newTestSwitchInstall()
	for range 3 {
		if world.installer.Sync(ctx) || world.installer.Installed() {
			t.Fatal("installed with the switch off")
		}
	}
	if world.builds != 0 || len(world.lines) != 0 {
		t.Fatalf("the switch off built %d times and said %q", world.builds, world.lines)
	}

	world.on = true
	for range 3 {
		if !world.installer.Sync(ctx) || !world.installer.Installed() {
			t.Fatal("not installed with the switch on")
		}
	}
	if world.builds != 1 || world.removes != 0 {
		t.Fatalf("%d builds and %d removals with the switch on", world.builds, world.removes)
	}
	if world.contexts[0].Err() != nil {
		t.Fatal("the installed part's context ended while it is installed")
	}

	world.on = false
	if world.installer.Sync(ctx) || world.installer.Installed() {
		t.Fatal("still installed with the switch off again")
	}
	if world.removes != 1 || world.contexts[0].Err() == nil {
		t.Fatalf("%d removals, context error %v", world.removes, world.contexts[0].Err())
	}
	world.installer.Sync(ctx)
	if world.removes != 1 {
		t.Fatal("removed twice")
	}

	world.on = true
	if !world.installer.Sync(ctx) || world.builds != 2 || world.contexts[1].Err() != nil {
		t.Fatalf("the second on: %d builds", world.builds)
	}
	world.installer.Close()
	if world.installer.Installed() || world.removes != 2 || world.contexts[1].Err() == nil {
		t.Fatal("Close left the part installed")
	}
	if !slices.Equal(world.lines, []string{
		"[part]sidecar.yml's switch is on: installed\n",
		"[part]sidecar.yml's switch is off: removed\n",
		"[part]sidecar.yml's switch is on: installed\n",
	}) {
		t.Fatalf("said %q", world.lines)
	}
}

// A build that fails leaves the part uninstalled, is tried again at each step,
// says one failure once, and installs once the build passes.
func TestSidecarSwitchInstallRetriesAFailedBuild(t *testing.T) {
	ctx := context.Background()
	world := newTestSwitchInstall()
	world.on = true
	world.buildErr = errors.New("no account-a.yml")
	for range 3 {
		if world.installer.Sync(ctx) {
			t.Fatal("installed by a build that failed")
		}
	}
	if world.builds != 3 || len(world.lines) != 1 || !strings.Contains(world.lines[0], "no account-a.yml") {
		t.Fatalf("%d builds, said %q", world.builds, world.lines)
	}
	world.buildErr = errors.New("no account-b.yml")
	world.installer.Sync(ctx)
	if len(world.lines) != 2 || !strings.Contains(world.lines[1], "no account-b.yml") {
		t.Fatalf("a new failure: said %q", world.lines)
	}
	// off and on again says a standing failure again
	world.on = false
	world.installer.Sync(ctx)
	world.on = true
	world.installer.Sync(ctx)
	if len(world.lines) != 3 || !strings.Contains(world.lines[2], "no account-b.yml") {
		t.Fatalf("after off and on: said %q", world.lines)
	}
	world.buildErr = nil
	if !world.installer.Sync(ctx) || world.removes != 0 {
		t.Fatal("not installed once the build passed")
	}
	if len(world.lines) != 4 || !strings.Contains(world.lines[3], "installed") {
		t.Fatalf("said %q", world.lines)
	}
}

// Run steps at its start and removes the part when its context ends.
func TestSidecarSwitchInstallRunRemovesAtTheEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	world := newTestSwitchInstall()
	world.on = true
	world.installer.settings = &SidecarSwitchInstallSettings{PollInterval: 0}
	done := make(chan struct{})
	go func() {
		defer close(done)
		world.installer.Run(ctx)
	}()
	cancel()
	<-done
	if world.builds != 1 || world.removes != 1 || world.installer.Installed() {
		t.Fatalf("%d builds, %d removals", world.builds, world.removes)
	}
}
