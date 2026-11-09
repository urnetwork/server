package server

import (
	"context"
	"runtime"
	"testing"
)

// A TB whose Fatal ends only the goroutine it runs on, so a test can see SetPgCommitFaultForTest refuse.
type pgCommitFaultRecorder struct {
	testing.TB
	fatal bool
}

func (self *pgCommitFaultRecorder) Helper() {}

func (self *pgCommitFaultRecorder) Fatal(args ...any) {
	self.fatal = true
	runtime.Goexit()
}

// One hook at a time: a second install while one is armed fails, and stopping the first lets the next
// arm. With none armed, a commit goes straight to the database, as production's always does.
func TestPgCommitFaultHookIsOneAtATime(t *testing.T) {
	if pgCommitFaultHook.Load() != nil {
		t.Fatal("a hook is armed outside any test")
	}
	passThrough := func(ctx context.Context, commit func() error, rollback func()) error {
		return commit()
	}
	stop := SetPgCommitFaultForTest(t, passThrough)
	recorder := &pgCommitFaultRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		SetPgCommitFaultForTest(recorder, passThrough)
	}()
	<-done
	if !recorder.fatal {
		t.Error("a second hook armed over the first")
	}
	stop()
	stop()
	if pgCommitFaultHook.Load() != nil {
		t.Fatal("stop left the hook armed")
	}
	SetPgCommitFaultForTest(t, passThrough)()
	if pgCommitFaultHook.Load() != nil {
		t.Error("the second hook outlived its stop")
	}
}
