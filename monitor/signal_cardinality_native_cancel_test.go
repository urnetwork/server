// A completed native reply cannot publish findings after its live owner ends.
package monitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Join the actual generated-command/public-Run seam at the exact reply handoff.
func TestCardinalityNativeCompletedReplyHonorsParentCancellation(t *testing.T) {
	for _, value := range []string{"no", "yes"} {
		func() {
			reply := cardinalityConfigReply{body: "latency-tracking\n" + value + "\n"}
			fixture := newCardinalityConfigNativeFixture(t, []cardinalityConfigReply{reply, reply}, cardinalityConfigMetrics(true))
			source := fixture.settings.Source.(*cardinalityConfigHostSource)
			original := source.observeHost
			ctx, cancel := context.WithCancel(context.Background())
			ready := make(chan struct{})
			resume := make(chan struct{})
			done := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(resume) }) }
			type result struct {
				alerts Alerts
				err    error
			}
			returned := make(chan result, 1)
			source.observeHost = func(sourceCtx context.Context, host HostSettings, command string) (string, error) {
				output, err := original(sourceCtx, host, command)
				if strings.Contains(command, metricCardinalityRedisSourceMarker) && err == nil {
					close(ready)
					<-resume
				}
				return output, err
			}
			t.Cleanup(func() {
				cancel()
				release()
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					t.Error("canceled native reply control did not join its Run owner")
				}
			})
			go func() {
				defer close(done)
				alerts, err := NewCardinalitySignal().Run(ctx, fixture.settings)
				returned <- result{alerts: alerts, err: err}
			}()
			select {
			case <-ready:
			case <-done:
				t.Fatal("Run ended before the completed native reply handoff")
			case <-time.After(30 * time.Second):
				t.Fatal("native reply never reached its acknowledged handoff")
			}
			cancel()
			release()
			select {
			case actual := <-returned:
				if len(actual.alerts) != 0 || !errors.Is(actual.err, context.Canceled) {
					t.Fatal("completed native reply published after parent cancellation")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("Run did not return after canceled reply handoff")
			}
			<-done
			fixture.requireCalls(t, 1)
		}()
	}
}
