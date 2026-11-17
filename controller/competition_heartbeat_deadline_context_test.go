// The private real-deadline fixture uses only WithoutCancel's exact value-key
// masking; its deadline, Done and Err still belong to the actual parent.
package controller

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// An application *int key must not disappear merely because context's private
// cancellation key happens to share its type. The actual deadline and fallback
// child-cancel witness remain live without claiming arbitrary cause forwarding.
func TestCompetitionHeartbeatPgDeadlineContextPreservesApplicationValuesAndDeadlineWitness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type applicationKey struct{}
		intKey := new(int)
		structKey := applicationKey{}
		parent := context.WithValue(t.Context(), intKey, "synthetic-int-value")
		parent = context.WithValue(parent, structKey, "synthetic-struct-value")
		deadline := time.Now().Add(time.Second)
		actual, cancel := context.WithDeadline(parent, deadline)
		defer cancel()
		wrapped := &competitionHeartbeatPgDeadlineContext{Context: actual, stopped: make(chan struct{})}
		projected, hasDeadline := wrapped.Deadline()
		if !hasDeadline || !projected.Equal(deadline) || wrapped.Done() != actual.Done() || wrapped.Err() != nil ||
			wrapped.Value(intKey) != "synthetic-int-value" || wrapped.Value(structKey) != "synthetic-struct-value" {
			t.Fatal("deadline wrapper changed the parent's live timing or application values")
		}
		programEntered, releaseProgram, programJoined := make(chan struct{}), make(chan struct{}), make(chan struct{})
		stop := context.AfterFunc(wrapped, func() {
			close(programEntered)
			<-releaseProgram
			close(programJoined)
		})
		<-wrapped.Done()
		<-wrapped.stopped
		<-programEntered
		if wrapped.Err() != context.DeadlineExceeded || actual.Err() != context.DeadlineExceeded || context.Cause(wrapped) != context.DeadlineExceeded ||
			wrapped.Value(intKey) != "synthetic-int-value" || wrapped.Value(structKey) != "synthetic-struct-value" || stop() {
			close(releaseProgram)
			<-programJoined
			t.Fatal("real deadline or acknowledged child cancellation was replaced")
		}
		select {
		case <-programJoined:
			t.Fatal("child-cancel witness was mislabeled a completed callback join")
		default:
		}
		close(releaseProgram)
		<-programJoined
	})
}
