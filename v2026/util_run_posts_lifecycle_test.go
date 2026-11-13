// RunPosts lifecycle tests pin cancellation to an exact admitted post so test
// environment teardown cannot overtake database work that already started.
package server

import (
	"context"
	"testing"
	"time"
)

// Cancellation joins a post held after admission before RunPosts returns.
func TestRunPostsCancellationJoinsAdmittedPost(t *testing.T) {
	testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer testCancel()
	ctx, cancel := context.WithCancel(context.Background())
	postStarted := make(chan struct{})
	releasePost := make(chan struct{})
	canceledWaitEntered := make(chan struct{})
	runDone := make(chan struct{})
	go func() {
		runPosts(
			ctx,
			func() {
				close(canceledWaitEntered)
			},
			func() any {
				close(postStarted)
				<-releasePost
				return nil
			},
		)
		close(runDone)
	}()
	select {
	case <-postStarted:
	case <-testCtx.Done():
		close(releasePost)
		t.Fatal("post did not reach admission barrier")
	}
	cancel()
	select {
	case <-canceledWaitEntered:
	case <-testCtx.Done():
		close(releasePost)
		t.Fatal("RunPosts did not reach canceled-worker join")
	}
	select {
	case <-runDone:
		close(releasePost)
		t.Fatal("RunPosts returned before its admitted post")
	default:
	}

	close(releasePost)
	select {
	case <-runDone:
	case <-testCtx.Done():
		t.Fatal("RunPosts did not join its admitted post")
	}
}
