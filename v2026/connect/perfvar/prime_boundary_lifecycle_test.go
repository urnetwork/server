package perfvar

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The prime fixture owns construction and the environment needed by final
// client identity removal. Teardown must join a late constructor result too.
type primeBoundaryLifetime struct {
	environmentCtx     context.Context
	constructionCtx    context.Context
	cancelEnvironment  context.CancelFunc
	cancelConstruction context.CancelFunc
	closeOnce          sync.Once
}

func newPrimeBoundaryLifetime(parent context.Context, timeout time.Duration) *primeBoundaryLifetime {
	environmentCtx, cancelEnvironment := context.WithTimeout(parent, timeout)
	constructionCtx, cancelConstruction := context.WithCancel(environmentCtx)
	return &primeBoundaryLifetime{
		environmentCtx: environmentCtx, constructionCtx: constructionCtx,
		cancelEnvironment: cancelEnvironment, cancelConstruction: cancelConstruction,
	}
}

func (l *primeBoundaryLifetime) close(joinAndClosePath, closeEnvironment func()) {
	l.closeOnce.Do(func() {
		defer l.cancelEnvironment()
		defer closeEnvironment()
		l.cancelConstruction()
		joinAndClosePath()
	})
}

// No DB, route fixture or wall-clock delay: channels pin all three ownership
// dispositions, including a successful path returned after cancellation.
func TestPrimeBoundaryLifetimeJoinsBeforeEnvironmentClose(t *testing.T) {
	for _, mode := range []string{"success", "pending-constructor", "constructor-error"} {
		t.Run(mode, func(t *testing.T) {
			lifetime := newPrimeBoundaryLifetime(context.Background(), 5*time.Second)
			type result struct {
				ownsPath bool
				err      error
			}
			primaryErr := errors.New("constructor failed before returning a path")
			results := make(chan result, 1)
			cancellationSeen := make(chan struct{})
			releaseConstructor := make(chan struct{})
			releaseRetirement := make(chan struct{})
			var constructorRelease, retirementRelease sync.Once
			joined := make(chan struct{})
			retirementEntered := make(chan struct{})
			environmentClosed := make(chan struct{})
			done := make(chan struct{})
			var joinCount, pathCloseCount, environmentCloseCount atomic.Int32
			var environmentCanceledBeforeRetirement, environmentCanceledBeforeClose atomic.Bool
			var observed result
			t.Cleanup(func() {
				defer lifetime.cancelEnvironment()
				lifetime.cancelConstruction()
				constructorRelease.Do(func() { close(releaseConstructor) })
				retirementRelease.Do(func() { close(releaseRetirement) })
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("fixture-free cleanup did not join constructor and retirement")
				}
			})
			switch mode {
			case "success":
				results <- result{ownsPath: true}
			case "constructor-error":
				results <- result{err: primaryErr}
			case "pending-constructor":
				go func() {
					<-lifetime.constructionCtx.Done()
					close(cancellationSeen)
					<-releaseConstructor
					results <- result{ownsPath: true}
				}()
			}
			go func() {
				defer close(done)
				lifetime.close(func() {
					observed = <-results
					joinCount.Add(1)
					close(joined)
					if observed.ownsPath {
						environmentCanceledBeforeRetirement.Store(lifetime.environmentCtx.Err() != nil)
						close(retirementEntered)
						<-releaseRetirement
						pathCloseCount.Add(1)
					}
				}, func() {
					environmentCanceledBeforeClose.Store(lifetime.environmentCtx.Err() != nil)
					environmentCloseCount.Add(1)
					close(environmentClosed)
				})
			}()
			await := func(event <-chan struct{}, name string) {
				t.Helper()
				select {
				case <-event:
				case <-time.After(5 * time.Second):
					t.Fatalf("did not reach %s", name)
				}
			}
			assertHeld := func(event <-chan struct{}, name string) {
				t.Helper()
				select {
				case <-event:
					t.Errorf("%s passed an unjoined owner", name)
				default:
				}
			}
			if mode == "pending-constructor" {
				await(cancellationSeen, "constructor cancellation")
				assertHeld(joined, "constructor join")
				assertHeld(environmentClosed, "environment close")
				assertHeld(done, "fixture close")
				if lifetime.environmentCtx.Err() != nil {
					t.Error("constructor cancellation canceled the network needed by rollback/retirement")
				}
				constructorRelease.Do(func() { close(releaseConstructor) })
			}
			await(joined, "constructor result join")
			if mode != "constructor-error" {
				await(retirementEntered, "path identity retirement")
				assertHeld(environmentClosed, "environment close")
				assertHeld(done, "fixture close")
				retirementRelease.Do(func() { close(releaseRetirement) })
			}
			await(done, "fixture close")
			if environmentCanceledBeforeRetirement.Load() || environmentCanceledBeforeClose.Load() {
				t.Error("environment canceled before path retirement and owned environment cleanup joined")
			}
			if lifetime.constructionCtx.Err() != context.Canceled || lifetime.environmentCtx.Err() != context.Canceled {
				t.Error("fixture close did not cancel both owned contexts")
			}
			wantPathCloses := int32(1)
			if mode == "constructor-error" {
				wantPathCloses = 0
				if !errors.Is(observed.err, primaryErr) {
					t.Error("constructor error was lost")
				}
			}
			if joinCount.Load() != 1 || pathCloseCount.Load() != wantPathCloses || environmentCloseCount.Load() != 1 {
				t.Fatalf("join/path/environment close counts=%d/%d/%d", joinCount.Load(), pathCloseCount.Load(), environmentCloseCount.Load())
			}
			lifetime.close(func() { t.Error("constructor/path was closed twice") }, func() { t.Error("environment was closed twice") })
		})
	}
}
