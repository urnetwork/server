package perfvar

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Body verification, local socket acceptance, and origin receipt are separate
// observations. None proves the identity or disposition of a Transfer ACK.
// Hooks are path-local, installed before a workload starts, and nil in normal
// measurements. Observers must not block. Count=-1 means a failed write's byte
// count is unavailable from writeFullTunAll.
type fullTunDownloadCompletionEvent struct {
	Phase string
	At    time.Time
	Count int64
	Hash  string
	Value byte
	Err   error
}

func (self *fullTunPath) observeDownloadCompletionForTest(event fullTunDownloadCompletionEvent) {
	if self.downloadCompletionPhaseForTest != nil {
		event.At = time.Now()
		self.downloadCompletionPhaseForTest(event)
	}
}

func TestFullTunDownloadBodyHashDoesNotCompleteBeforeServerByte(t *testing.T) {
	t.Run("release", func(t *testing.T) {
		testFullTunDownloadCompletion(t, false, false)
	})
	t.Run("cancel-before-write", func(t *testing.T) {
		testFullTunDownloadCompletion(t, true, false)
	})
}

func TestFullTunDownloadCompletionWriteDoesNotCompleteBeforeServerRead(t *testing.T) {
	testFullTunDownloadCompletion(t, false, true)
}

// Exact application barriers establish the negative assertions without sleeps.
// Holding the origin's application Read models delayed consumption, not a lost
// carrier frame. These controls do not reproduce the unlocalized ACK-head stall
// in the historical H1 PERF failures.
func testFullTunDownloadCompletion(t *testing.T, cancelBeforeWrite, holdServerRead bool) {
	t.Helper()
	if testing.Short() {
		t.Skip("requires local full-TUN fixture")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		fixtureCtx, fixtureCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer fixtureCancel()
		profile := initialNetworkProfiles(3320)["clean-lan"]
		environment := newRouteEnvironmentWithNetworkPeers(fixtureCtx, t, profile, false)
		defer environment.close()
		path := newFullTunPath(fixtureCtx, t, environment, fullTunRouteExchangeH1)
		defer path.close()
		workloadCtx, cancelWorkload := context.WithCancel(fixtureCtx)
		defer cancelWorkload()

		const byteCount = int64(64 * 1024)
		expectedHash := deterministicPayloadHash(byteCount)
		events := make(chan fullTunDownloadCompletionEvent, 3)
		var eventOverflow atomic.Bool
		path.downloadCompletionPhaseForTest = func(event fullTunDownloadCompletionEvent) {
			select {
			case events <- event:
			default:
				eventOverflow.Store(true)
			}
		}
		atWrite, releaseWrite := make(chan struct{}), make(chan struct{})
		atRead, releaseRead := make(chan struct{}), make(chan struct{})
		var releaseWriteOnce, releaseReadOnce sync.Once
		resumeWrite := func() { releaseWriteOnce.Do(func() { close(releaseWrite) }) }
		resumeRead := func() { releaseReadOnce.Do(func() { close(releaseRead) }) }
		path.beforeDownloadCompletionForTest = func(ctx context.Context) error {
			close(atWrite)
			select {
			case <-releaseWrite:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if holdServerRead {
			path.beforeDownloadCompletionReadForTest = func(ctx context.Context) error {
				close(atRead)
				select {
				case <-releaseRead:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		var originalClient *clientconnect.Client
		var originalID clientconnect.Id
		pinOriginal := func() error {
			client, id, flows, source := activeH3FullTunDeviceClient(path)
			if client == nil || source != "flow" || flows <= 0 {
				return fmt.Errorf("completion fixture has no original active flow: source=%s flows=%d", source, flows)
			}
			originalClient, originalID = client, id
			return nil
		}
		type completion struct {
			result workloadResult
			err    error
		}
		done := make(chan completion, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			result, err := measureFullTunDownloadWithWarmupAndStartHook(
				workloadCtx, path, 0, byteCount, byteCount, pinOriginal,
			)
			if err == nil {
				err = path.waitForPostWorkloadBoundary(workloadCtx)
			}
			done <- completion{result: result, err: err}
		}()
		defer func() {
			cancelWorkload()
			resumeWrite()
			resumeRead()
			select {
			case <-joined:
			case <-fixtureCtx.Done():
				t.Error("completion workload did not join cleanup")
			}
		}()
		select {
		case <-atWrite:
		case result := <-done:
			t.Fatalf("workload returned before completion-write gate: %v", result.err)
		case <-fixtureCtx.Done():
			t.Fatal(fixtureCtx.Err())
		}
		body := <-events // published before the completion-write gate
		if body.Phase != "body-verified" || body.Count != byteCount ||
			body.Hash != expectedHash || body.Err != nil || body.At.IsZero() ||
			path.workloadProgressBytes.Load() != byteCount {
			t.Fatalf("body verification was not witnessed: %+v", body)
		}
		assertPending := func() {
			t.Helper()
			select {
			case event := <-events:
				t.Fatalf("completion phase escaped a held application gate: %+v", event)
			default:
			}
			select {
			case result := <-done:
				t.Fatalf("workload returned at a held application gate: %v", result.err)
			default:
			}
		}
		assertPending()
		client, id, flows, source := activeH3FullTunDeviceClient(path)
		if client != originalClient || id != originalID || source != "flow" || flows <= 0 ||
			originalClient.Ctx().Err() != nil {
			t.Fatal("completion fixture substituted its original flow owner")
		}
		observed := map[string]fullTunDownloadCompletionEvent{body.Phase: body}
		if cancelBeforeWrite {
			cancelWorkload()
		} else {
			resumeWrite()
			if holdServerRead {
				select {
				case <-atRead:
				case <-fixtureCtx.Done():
					t.Fatal(fixtureCtx.Err())
				}
				select {
				case event := <-events:
					if event.Phase != "client-write" || event.Count != 1 || event.Value != 1 || event.Err != nil {
						t.Fatalf("local completion write failed: %+v", event)
					}
					observed[event.Phase] = event
				case result := <-done:
					t.Fatalf("workload returned with origin read held: %v", result.err)
				case <-fixtureCtx.Done():
					t.Fatal(fixtureCtx.Err())
				}
				assertPending()
				resumeRead()
			}
		}
		var result completion
		select {
		case result = <-done:
		case <-fixtureCtx.Done():
			t.Fatal(fixtureCtx.Err())
		}
		<-joined
		// All workload and server callbacks have joined. Reading the finite
		// channel now checks exact multiplicity without callback-order guesses.
		for len(events) > 0 {
			event := <-events
			if _, exists := observed[event.Phase]; exists {
				t.Fatalf("duplicate completion phase: %s", event.Phase)
			}
			observed[event.Phase] = event
		}
		if eventOverflow.Load() {
			t.Fatal("completion phase recorder overflowed")
		}
		if cancelBeforeWrite {
			if !errors.Is(result.err, context.Canceled) || result.result.UsefulByteCount != 0 {
				t.Fatalf("canceled completion reported success: %+v", result)
			}
			if _, exists := observed["client-write"]; exists {
				t.Fatal("cancellation at the body gate still attempted completion write")
			}
			if event, exists := observed["server-read"]; exists && (event.Err == nil || event.Count != 0) {
				t.Fatalf("origin received a completion byte after pre-write cancellation: %+v", event)
			}
			return
		}
		if result.err != nil || result.result.UsefulByteCount != byteCount || result.result.ContentHash != expectedHash {
			t.Fatalf("released completion failed: %+v", result)
		}
		for _, phase := range []string{"client-write", "server-read"} {
			event, exists := observed[phase]
			if !exists || event.Count != 1 || event.Value != 1 || event.Err != nil || event.At.Before(body.At) {
				t.Fatalf("missing successful %s after body verification: %+v", phase, event)
			}
		}
		if len(observed) != 3 || originalClient.Ctx().Err() != nil {
			t.Fatalf("completion did not retain exactly three phases and its original owner: phases=%d", len(observed))
		}
	})
}
