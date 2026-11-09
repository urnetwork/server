package sidecart3

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// A TB that records what a helper reports instead of failing the test, so a test can assert that the
// helper fails: Errorf and Fatalf are kept, and Fatalf ends only the goroutine record runs it on.
type recordingTB struct {
	testing.TB

	stateLock sync.Mutex
	failures  []string
	fatal     bool
}

func (self *recordingTB) Helper() {}

func (self *recordingTB) add(text string, fatal bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failures = append(self.failures, text)
	self.fatal = self.fatal || fatal
}

func (self *recordingTB) Error(args ...any) {
	self.add(fmt.Sprint(args...), false)
}

func (self *recordingTB) Errorf(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...), false)
}

func (self *recordingTB) Fatal(args ...any) {
	self.add(fmt.Sprint(args...), true)
	runtime.Goexit()
}

func (self *recordingTB) Fatalf(format string, args ...any) {
	self.add(fmt.Sprintf(format, args...), true)
	runtime.Goexit()
}

func (self *recordingTB) Failed() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return 0 < len(self.failures)
}

// Every failure, joined, for an assertion on its text.
func (self *recordingTB) text() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return strings.Join(self.failures, "\n")
}

// Runs run with a recordingTB on t, on a goroutine of its own, and returns what it recorded.
func record(t testing.TB, run func(tb testing.TB)) *recordingTB {
	recorder := &recordingTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(recorder)
	}()
	<-done
	return recorder
}
