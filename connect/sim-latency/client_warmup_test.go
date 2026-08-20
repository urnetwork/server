// Deterministic coverage for the client-pool measurement boundary.
package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Adapts an inline deterministic transport without starting a listener.
type warmupRoundTripFunc func(*http.Request) (*http.Response, error)

func (self warmupRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

func TestBuildWarmClientPoolRetriesMissingAndPreservesOrder(t *testing.T) {
	pool := make([]ClientIdentity, 5)
	for index := range pool {
		pool[index].ClientId = server.NewId()
	}

	attemptCounts := make([]int, len(pool))
	var attemptLock sync.Mutex
	clients := buildWarmClientPool(
		context.Background(),
		pool,
		3,
		0,
		func(_ context.Context, identity ClientIdentity, poolIndex int) *pooledClient {
			attemptLock.Lock()
			attemptCounts[poolIndex]++
			attempt := attemptCounts[poolIndex]
			attemptLock.Unlock()

			// Make completion order differ from fixture order. Slot 1 also
			// simulates the transient first-attempt miss seen in the campaign.
			time.Sleep(time.Duration(len(pool)-poolIndex) * time.Millisecond)
			if poolIndex == 1 && attempt == 1 {
				return nil
			}
			return &pooledClient{label: identity.ClientId.String()}
		},
	)

	if len(clients) != len(pool) {
		t.Fatalf("warm clients = %d, want %d", len(clients), len(pool))
	}
	for index, client := range clients {
		if want := pool[index].ClientId.String(); client.label != want {
			t.Fatalf("client %d label = %q, want %q", index, client.label, want)
		}
		wantAttempts := 1
		if index == 1 {
			wantAttempts = 2
		}
		if attemptCounts[index] != wantAttempts {
			t.Fatalf(
				"client %d attempts = %d, want %d",
				index,
				attemptCounts[index],
				wantAttempts,
			)
		}
	}
}

func TestBuildWarmClientPoolHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	clients := buildWarmClientPool(
		ctx,
		[]ClientIdentity{{ClientId: server.NewId()}},
		3,
		0,
		func(context.Context, ClientIdentity, int) *pooledClient {
			called = true
			return &pooledClient{}
		},
	)
	if called || len(clients) != 0 {
		t.Fatalf("canceled warmup called builder=%t, clients=%d", called, len(clients))
	}
}

func TestWarmClientPoolDeadlineAcceptsCompletePool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := warmClientPoolError(ctx, 200, 200); err != nil {
		t.Fatalf("complete pool at deadline: %s", err)
	}
	if err := warmClientPoolError(ctx, 199, 200); err != context.Canceled {
		t.Fatalf("incomplete pool at deadline = %v, want %v", err, context.Canceled)
	}
}

func TestWarmMultiClientSettingsPinsExplicitQualityWindow(t *testing.T) {
	const qualityWindowSize = 2
	settings := newWarmMultiClientSettings(qualityWindowSize)
	windowType, windowSize, fixed := settings.DefaultPerformanceProfile.FixedWindow()
	if !fixed {
		t.Fatal("explicit calibration window left simulator routing in auto mode")
	}
	if windowType != connect.WindowTypeQuality {
		t.Fatalf("fixed window type = %v, want %v", windowType, connect.WindowTypeQuality)
	}
	if windowSize.WindowSizeMin != qualityWindowSize ||
		windowSize.WindowSizeMax != qualityWindowSize ||
		windowSize.WindowSizeHardMax != qualityWindowSize ||
		windowSize.FixedWindowSize != qualityWindowSize {
		t.Fatalf("fixed quality window = %+v, want every bound %d", windowSize, qualityWindowSize)
	}
	if got := settings.WindowSizes[connect.WindowTypeQuality]; got != windowSize {
		t.Fatalf("quality map window = %+v, want fixed profile window %+v", got, windowSize)
	}
}

func TestWarmMultiClientSettingsPreservesProductionAutoPolicy(t *testing.T) {
	settings := newWarmMultiClientSettings(0)
	if _, _, fixed := settings.DefaultPerformanceProfile.FixedWindow(); fixed {
		t.Fatal("zero calibration window unexpectedly fixed simulator routing")
	}
	want := connect.DefaultMultiClientSettings().WindowSizes[connect.WindowTypeQuality]
	if got := settings.WindowSizes[connect.WindowTypeQuality]; got != want {
		t.Fatalf("adaptive quality window = %+v, want production default %+v", got, want)
	}
}

func TestBuildWarmClientPoolDeadlineStopsQueuedBuilders(t *testing.T) {
	pool := make([]ClientIdentity, warmupConcurrency+1)
	for index := range pool {
		pool[index].ClientId = server.NewId()
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan int, warmupConcurrency)
	done := make(chan []*pooledClient, 1)
	go func() {
		done <- buildWarmClientPool(
			ctx,
			pool,
			1,
			0,
			func(buildCtx context.Context, _ ClientIdentity, poolIndex int) *pooledClient {
				started <- poolIndex
				<-buildCtx.Done()
				return nil
			},
		)
	}()

	startedIndexes := map[int]bool{}
	for index := 0; index < warmupConcurrency; index++ {
		startedIndexes[<-started] = true
	}
	cancel()
	clients := <-done
	if len(clients) != 0 {
		t.Fatalf("deadline warm clients = %d, want 0", len(clients))
	}
	if len(startedIndexes) != warmupConcurrency {
		t.Fatalf("builders started = %d, want %d", len(startedIndexes), warmupConcurrency)
	}
	select {
	case poolIndex := <-started:
		t.Fatalf("builder %d started after every warm-up slot was occupied", poolIndex)
	default:
	}
}

func TestQualityWindowReadyRequiresDistinctUsableQualityExits(t *testing.T) {
	healthyClientId := connect.NewId()
	tests := []struct {
		name string
		exit *connect.ExitInfo
		want bool
	}{
		{
			name: "healthy",
			exit: &connect.ExitInfo{
				ClientId:   healthyClientId,
				WindowType: connect.WindowTypeQuality,
			},
			want: true,
		},
		{
			name: "wrong window",
			exit: &connect.ExitInfo{
				ClientId:   healthyClientId,
				WindowType: connect.WindowTypeSpeed,
			},
			want: false,
		},
		{
			name: "warning",
			exit: &connect.ExitInfo{
				ClientId:   healthyClientId,
				WindowType: connect.WindowTypeQuality,
				Warning:    true,
			},
			want: false,
		},
		{
			name: "quarantined",
			exit: &connect.ExitInfo{
				ClientId:    healthyClientId,
				WindowType:  connect.WindowTypeQuality,
				Quarantined: true,
			},
			want: false,
		},
		{
			name: "done",
			exit: &connect.ExitInfo{
				ClientId:   healthyClientId,
				WindowType: connect.WindowTypeQuality,
				Done:       true,
			},
			want: false,
		},
		{
			name: "peer only",
			exit: &connect.ExitInfo{
				ClientId:   healthyClientId,
				WindowType: connect.WindowTypeQuality,
				P2pOnly:    true,
			},
			want: false,
		},
		{name: "missing", exit: nil, want: false},
	}
	for _, test := range tests {
		if got := qualityWindowReady([]*connect.ExitInfo{test.exit}, 1); got != test.want {
			t.Errorf("%s ready = %t, want %t", test.name, got, test.want)
		}
	}

	duplicateExits := []*connect.ExitInfo{
		{
			ClientId:   healthyClientId,
			WindowType: connect.WindowTypeQuality,
		},
		{
			ClientId:   healthyClientId,
			WindowType: connect.WindowTypeQuality,
		},
	}
	if qualityWindowReady(duplicateExits, 2) {
		t.Fatal("duplicate quality exit satisfied a two-provider window")
	}
	distinctExits := append(duplicateExits, &connect.ExitInfo{
		ClientId:   connect.NewId(),
		WindowType: connect.WindowTypeQuality,
	})
	if !qualityWindowReady(distinctExits, 2) {
		t.Fatal("two distinct usable quality exits did not satisfy the window")
	}
}

func TestWarmupRequestCohortKeepsEveryMeasuredLaneReusable(t *testing.T) {
	const connectionsPerCrawl = 6
	barriers := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var stateLock sync.Mutex
	requestCount := 0
	activeCount := 0
	maxActiveCount := 0
	newConnectionCount := 0

	handler := http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		barrierIndex := 0
		position := 0
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			barrierIndex = requestCount / connectionsPerCrawl
			position = requestCount % connectionsPerCrawl
			requestCount++
			activeCount++
			if maxActiveCount < activeCount {
				maxActiveCount = activeCount
			}
		}()
		defer func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			activeCount--
		}()

		if len(barriers) <= barrierIndex {
			http.Error(responseWriter, "unexpected request", http.StatusInternalServerError)
			return
		}
		if position == connectionsPerCrawl-1 {
			close(barriers[barrierIndex])
		}
		select {
		case <-barriers[barrierIndex]:
		case <-request.Context().Done():
			return
		}
		_, _ = io.WriteString(responseWriter, "{\"urls\":[],\"size\":4}\nabcd")
	})

	testServer := httptest.NewUnstartedServer(handler)
	testServer.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		// StateNew is emitted exactly once for each accepted connection.
		// Counting later states would count reuse as new work.
		if state != http.StateNew {
			return
		}
		stateLock.Lock()
		defer stateLock.Unlock()
		newConnectionCount++
	}
	testServer.Start()
	defer testServer.Close()

	dialer := &net.Dialer{}
	httpClient := newWarmHTTPClient(dialer.DialContext, connectionsPerCrawl)
	defer httpClient.CloseIdleConnections()
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("warm transport type = %T, want *http.Transport", httpClient.Transport)
	}
	if transport.MaxIdleConnsPerHost < connectionsPerCrawl {
		t.Fatalf(
			"idle connections per host = %d, want at least %d",
			transport.MaxIdleConnsPerHost,
			connectionsPerCrawl,
		)
	}
	if transport.IdleConnTimeout != 0 {
		t.Fatalf("idle connection timeout = %s, want evaluation-owned lifetime", transport.IdleConnTimeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !warmupRequestCohort(ctx, httpClient, testServer.URL, connectionsPerCrawl) {
		t.Fatal("first complete lane cohort failed")
	}
	if !warmupRequestCohort(ctx, httpClient, testServer.URL, connectionsPerCrawl) {
		t.Fatal("second complete lane cohort failed")
	}

	stateLock.Lock()
	gotRequests := requestCount
	gotMaxActive := maxActiveCount
	gotConnections := newConnectionCount
	stateLock.Unlock()
	if gotRequests != 2*connectionsPerCrawl {
		t.Fatalf("requests = %d, want %d", gotRequests, 2*connectionsPerCrawl)
	}
	if gotMaxActive != connectionsPerCrawl {
		t.Fatalf("maximum concurrent requests = %d, want %d", gotMaxActive, connectionsPerCrawl)
	}
	if gotConnections != connectionsPerCrawl {
		t.Fatalf(
			"connections after two cohorts = %d, want %d reused lanes",
			gotConnections,
			connectionsPerCrawl,
		)
	}
}

func TestWarmupRequestCohortRejectsOneIncompleteLane(t *testing.T) {
	const connectionsPerCrawl = 6
	var stateLock sync.Mutex
	requestCount := 0
	httpClient := &http.Client{
		Transport: warmupRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			stateLock.Lock()
			requestIndex := requestCount
			requestCount++
			stateLock.Unlock()
			body := "{\"urls\":[],\"size\":4}\nabcd"
			if requestIndex == connectionsPerCrawl-1 {
				body = "{\"urls\":[],\"size\":4}\nab"
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Body:          io.NopCloser(strings.NewReader(body)),
				ContentLength: int64(len(body)),
				Request:       request,
			}, nil
		}),
	}
	if warmupRequestCohort(context.Background(), httpClient, "http://warm.test/", connectionsPerCrawl) {
		t.Fatal("cohort accepted one incomplete lane")
	}
	stateLock.Lock()
	gotRequests := requestCount
	stateLock.Unlock()
	if gotRequests != connectionsPerCrawl {
		t.Fatalf("requests = %d, want %d", gotRequests, connectionsPerCrawl)
	}
}

func TestWarmupRequestAttemptInitiatesLazyWindowBeforeReadinessCheck(t *testing.T) {
	ready := false
	events := []string{}
	got := warmupRequestAttempt(
		context.Background(),
		func(context.Context) bool {
			events = append(events, "cohort")
			if ready {
				t.Fatal("quality window was ready before its initiating cohort")
			}
			ready = true
			return true
		},
		func() bool {
			events = append(events, "readiness")
			return ready
		},
	)
	if !got {
		t.Fatal("complete cohort did not establish its lazy quality window")
	}
	if len(events) != 2 || events[0] != "cohort" || events[1] != "readiness" {
		t.Fatalf("warm-up ordering = %v, want [cohort readiness]", events)
	}
}

func TestValidateWarmClientPoolRetriesOnlyUnreadyClients(t *testing.T) {
	clients := []*pooledClient{{label: "0"}, {label: "1"}, {label: "2"}}
	attemptCounts := make([]int, len(clients))
	var stateLock sync.Mutex
	validated := validateWarmClientPool(
		context.Background(),
		clients,
		3,
		0,
		func(_ context.Context, _ *pooledClient, index int) bool {
			stateLock.Lock()
			defer stateLock.Unlock()
			attemptCounts[index]++
			return index != 1 || 1 < attemptCounts[index]
		},
		func(*pooledClient) bool { return true },
	)
	if validated != len(clients) {
		t.Fatalf("validated clients = %d, want %d", validated, len(clients))
	}
	for index, got := range attemptCounts {
		want := 1
		if index == 1 {
			want = 2
		}
		if got != want {
			t.Errorf("client %d validation attempts = %d, want %d", index, got, want)
		}
	}
}

func TestValidateWarmClientPoolRechecksReadinessAfterEachBatch(t *testing.T) {
	clients := []*pooledClient{{label: "0"}, {label: "1"}}
	attemptCounts := make([]int, len(clients))
	var stateLock sync.Mutex
	validated := validateWarmClientPool(
		context.Background(),
		clients,
		3,
		0,
		func(_ context.Context, _ *pooledClient, index int) bool {
			stateLock.Lock()
			defer stateLock.Unlock()
			attemptCounts[index]++
			return true
		},
		func(client *pooledClient) bool {
			stateLock.Lock()
			defer stateLock.Unlock()
			if client.label == "1" {
				return 1 < attemptCounts[1]
			}
			return true
		},
	)
	if validated != len(clients) {
		t.Fatalf("validated clients = %d, want %d", validated, len(clients))
	}
	if attemptCounts[0] != 1 || attemptCounts[1] != 2 {
		t.Fatalf("validation attempts = %v, want [1 2]", attemptCounts)
	}
}
