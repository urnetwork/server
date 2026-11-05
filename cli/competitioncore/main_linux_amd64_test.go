// CLI regressions use only an injected dispatcher and private in-memory pipe.
package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/urnetwork/server/competitioncore"
)

type testPipe struct{ reads, closes int }

func (self *testPipe) Read([]byte) (int, error) { self.reads++; return 0, errors.New("no core read") }
func (self *testPipe) Close() error             { self.closes++; return nil }

type testDispatcher struct{ arguments []string }

func (self *testDispatcher) Handle(_ context.Context, args []string, core io.ReadCloser) competitioncore.Decision {
	self.arguments = append([]string(nil), args...)
	core.Close()
	return competitioncore.UnknownDropped
}

func TestKernelCliRetainsArgumentsAndNeverReadsOrPrintsCore(t *testing.T) {
	pipe := &testPipe{}
	handler := &testDispatcher{}
	arguments := []string{"-p23", "-s11", "-c0", "-d1", "-P4321", "-u1000", "-g1001", "-F", "--", "!synthetic!bin!program"}
	run(t.Context(), arguments, pipe, handler)
	if pipe.reads != 0 || pipe.closes != 1 || !reflect.DeepEqual(handler.arguments, arguments) {
		t.Fatal("silent kernel CLI changed core/argv custody")
	}
}

func TestKernelCliMissingDispatcherStillClosesUnreadCore(t *testing.T) {
	pipe := &testPipe{}
	run(t.Context(), nil, pipe, nil)
	if pipe.reads != 0 || pipe.closes != 1 {
		t.Fatal("missing dispatcher failed to drop unread core")
	}
}
