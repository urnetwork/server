// Probe request deadlines own DNS/socket/TLS work even though net/http normally
// detaches a reusable connection's dial from the request that first needed it.
package providertunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/operator-proxy/egresshealth"
)

var errProbeOwnerFixture = errors.New("synthetic probe dial released")

// The real TUN has its own 30-second dial bound. This synthetic dial models
// that independent bound, while exposing the actual HTTP owner's context.
type probeRequestOwnerDial struct {
	entered   chan context.Context
	release   chan struct{}
	active    atomic.Int64
	maximum   atomic.Int64
	closeOnce sync.Once
}

func newProbeRequestOwnerDial() *probeRequestOwnerDial {
	return &probeRequestOwnerDial{entered: make(chan context.Context, 32), release: make(chan struct{})}
}

func (self *probeRequestOwnerDial) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	active := self.active.Add(1)
	defer self.active.Add(-1)
	for previous := self.maximum.Load(); previous < active; previous = self.maximum.Load() {
		if self.maximum.CompareAndSwap(previous, active) {
			break
		}
	}
	localCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	self.entered <- ctx
	select {
	case <-localCtx.Done():
		return nil, context.Cause(localCtx)
	case <-self.release:
		return nil, errProbeOwnerFixture
	}
}

// Release the synthetic fallback even on RED, then join all owner work in the
// bubble. No abandoned dial, real sleep, network or production identity.
func (self *probeRequestOwnerDial) close() {
	self.closeOnce.Do(func() { close(self.release) })
	synctest.Wait()
}

func startProbeRequestOwner(t *testing.T, client *http.Client, ctx context.Context) <-chan error {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://echo.example/my-ip-info", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, requestErr := client.Do(req)
		if response != nil {
			// Cleanup only; the request result remains the owner assertion.
			_ = response.Body.Close()
		}
		done <- requestErr
	}()
	return done
}

func requireProbeRequestOwnerResult(t *testing.T, done <-chan error, want error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Errorf("request error = %v, want %v", err, want)
		}
	default:
		t.Error("request did not terminate at its owner boundary")
	}
}

// Fake time verifies the real ten-second full-load and fifteen-second
// blackhole deadlines without turning either into a wall-clock wait.
func checkProbeRequestOwnerDeadline(t *testing.T, duration time.Duration) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		dial := newProbeRequestOwnerDial()
		defer dial.close()
		client := httpClientOverDialerWithHosts(dial.dial, nil, []string{"echo.example"}, time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), duration)
		defer cancel()
		expected, _ := ctx.Deadline()
		done := startProbeRequestOwner(t, client, ctx)
		dialCtx := <-dial.entered
		if actual, ok := dialCtx.Deadline(); !ok || !actual.Equal(expected) {
			t.Error("raw tunnel dial lost the request's exact deadline")
		}
		time.Sleep(duration)
		synctest.Wait()
		requireProbeRequestOwnerResult(t, done, context.DeadlineExceeded)
		if dial.active.Load() != 0 {
			t.Error("expired request left its 30-second tunnel dial active")
		}
	})
}

func TestProbeHttpRequestOwnerFullDeadline(t *testing.T) {
	checkProbeRequestOwnerDeadline(t, 10*time.Second)
}

func TestProbeHttpRequestOwnerBlackholeDeadline(t *testing.T) {
	checkProbeRequestOwnerDeadline(t, 15*time.Second)
}

// Client.Timeout installs a request context inside net/http before RoundTrip;
// this owner also has to reach the custom dial, not just caller WithTimeout.
func TestProbeHttpRequestOwnerClientTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := newProbeRequestOwnerDial()
		defer dial.close()
		client := httpClientOverDialerWithHosts(dial.dial, nil, []string{"echo.example"}, 15*time.Second)
		done := startProbeRequestOwner(t, client, context.Background())
		<-dial.entered
		time.Sleep(15 * time.Second)
		synctest.Wait()
		requireProbeRequestOwnerResult(t, done, context.DeadlineExceeded)
		if dial.active.Load() != 0 {
			t.Error("Client.Timeout left its tunnel dial active")
		}
	})
}

func TestProbeHttpRequestOwnerCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := newProbeRequestOwnerDial()
		defer dial.close()
		client := httpClientOverDialerWithHosts(dial.dial, nil, []string{"echo.example"}, time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := startProbeRequestOwner(t, client, ctx)
		<-dial.entered
		cancel()
		synctest.Wait()
		requireProbeRequestOwnerResult(t, done, context.Canceled)
		if dial.active.Load() != 0 {
			t.Error("canceled request retained its detached dial")
		}
	})
}

// A retired attempt must not overlap its successor merely because net/http
// kept dialing a connection this non-reusing probe can never borrow.
func TestProbeHttpRequestOwnerRetryDoesNotOverlapRetiredDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := newProbeRequestOwnerDial()
		defer dial.close()
		client := httpClientOverDialerWithHosts(dial.dial, nil, []string{"echo.example"}, time.Minute)
		firstCtx, cancelFirst := context.WithCancel(context.Background())
		defer cancelFirst()
		first := startProbeRequestOwner(t, client, firstCtx)
		<-dial.entered
		cancelFirst()
		synctest.Wait()
		requireProbeRequestOwnerResult(t, first, context.Canceled)
		secondCtx, cancelSecond := context.WithCancel(context.Background())
		defer cancelSecond()
		second := startProbeRequestOwner(t, client, secondCtx)
		<-dial.entered
		synctest.Wait()
		if dial.maximum.Load() != 1 {
			t.Error("retired request amplified successor dial concurrency")
		}
		cancelSecond()
		synctest.Wait()
		requireProbeRequestOwnerResult(t, second, context.Canceled)
	})
}

// The real health run releases a six-way fetch slot after each ten-second
// request. A detached thirty-second dial must not survive that release and
// turn three successive waves into eighteen simultaneous tunnel dials.
func TestProbeHttpRequestOwnerHealthConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := newProbeRequestOwnerDial()
		defer dial.close()
		client := httpClientOverDialerWithHosts(dial.dial, nil, []string{"echo.example"}, time.Minute)
		destinations := make([]egresshealth.Destination, 18)
		for i := range destinations {
			destinations[i] = egresshealth.Destination{
				Name: fmt.Sprintf("synthetic-load-%d", i), Class: egresshealth.ClassConnectivity,
				Url: "https://echo.example/load", Expect: egresshealth.ExpectBody,
			}
		}
		type outcome struct {
			result *egresshealth.Result
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := egresshealth.Check(context.Background(), client, egresshealth.Options{
				Destinations: destinations, AllDestinations: true,
				PerRequestTimeout: 10 * time.Second, Concurrency: 6,
				LoadAttempts: 1, Budget: time.Minute, Rand: rand.New(rand.NewSource(1)),
			})
			done <- outcome{result: result, err: err}
		}()
		synctest.Wait()
		if dial.active.Load() != 6 {
			t.Fatal("health run did not establish its six-slot first wave")
		}
		time.Sleep(30 * time.Second)
		synctest.Wait()
		select {
		case got := <-done:
			if got.err != nil || got.result == nil || len(got.result.Checks) != 18 {
				t.Fatal("health run did not complete its selected synthetic loads")
			}
			if got.result.Total != 18 || got.result.OkCount != 0 || got.result.NotMeasured != 0 {
				t.Fatal("request deadlines were relabeled as unmeasured or passing loads")
			}
		default:
			t.Fatal("health run outlived its three bounded request waves")
		}
		if got := dial.maximum.Load(); got > 6 {
			t.Errorf("six fetch slots admitted %d overlapping tunnel dials", got)
		}
	})
}

// A blackhole check uses the same real HTTP owner for all three loads. A
// fifteen-second deadline remains a measured failure after retries; it must
// not leak the retired dial or manufacture a NotMeasured verdict.
func TestProbeHttpRequestOwnerBlackholeRetriesMeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := newProbeRequestOwnerDial()
		defer dial.close()
		client := httpClientOverDialerWithHosts(dial.dial, nil, []string{"echo.example"}, time.Minute)
		done := make(chan *egresshealth.BlackholeResult, 1)
		go func() {
			done <- egresshealth.Blackhole(context.Background(), client, egresshealth.Options{
				Destinations: probeRequestOwnerDestinations(), PerRequestTimeout: 15 * time.Second,
				LoadAttempts: 2, LoadRetryMeanInterval: time.Nanosecond, Budget: time.Minute,
				Rand: rand.New(rand.NewSource(1)),
			})
		}()
		synctest.Wait()
		if dial.active.Load() != 3 {
			t.Fatal("blackhole did not start its three loads")
		}
		time.Sleep(30*time.Second + 3*time.Nanosecond)
		synctest.Wait()
		select {
		case got := <-done:
			if got.Ok || got.Failure != egresshealth.FailureAllDestinationsFailed || got.NotMeasured != 0 || len(got.Results) != 3 {
				t.Fatal("blackhole request deadlines changed measured-failure semantics")
			}
			for _, load := range got.Results {
				if load.Attempts != 2 || load.NotMeasured || load.TlsAuthenticationFailure {
					t.Fatal("blackhole changed retries or terminal failure class")
				}
			}
		default:
			t.Fatal("blackhole did not finish its bounded retry chain")
		}
		if dial.active.Load() != 0 || dial.maximum.Load() > 3 {
			t.Errorf("blackhole retries retained tunnel dials: active=%d maximum=%d", dial.active.Load(), dial.maximum.Load())
		}
	})
}

func probeRequestOwnerDestinations() []egresshealth.Destination {
	destinations := make([]egresshealth.Destination, 3)
	for i := range destinations {
		destinations[i] = egresshealth.Destination{
			Name: fmt.Sprintf("synthetic-connectivity-%d", i), Class: egresshealth.ClassConnectivity,
			Url: "https://echo.example/load", Expect: egresshealth.ExpectBody,
		}
	}
	return destinations
}

type probeRequestOwnerLostPath struct {
	client *http.Client
	lost   context.Context
}

func (self *probeRequestOwnerLostPath) Current() (*http.Client, context.Context) {
	return self.client, self.lost
}

func (self *probeRequestOwnerLostPath) Reopen(context.Context) error {
	return errProbeOwnerFixture
}

// Losing the actual path during a load is different from a live path whose
// request timed out: the check stays NotMeasured, and owns dial cleanup too.
func TestProbeHttpRequestOwnerBlackholeLostPathUnmeasured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := newProbeRequestOwnerDial()
		defer dial.close()
		client := httpClientOverDialerWithHosts(dial.dial, nil, []string{"echo.example"}, time.Minute)
		lost, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan *egresshealth.BlackholeResult, 1)
		go func() {
			done <- egresshealth.Blackhole(context.Background(), nil, egresshealth.Options{
				Path:         &probeRequestOwnerLostPath{client: client, lost: lost},
				Destinations: probeRequestOwnerDestinations(), PerRequestTimeout: 15 * time.Second,
				LoadAttempts: 1, Budget: time.Minute, Rand: rand.New(rand.NewSource(1)),
			})
		}()
		for range 3 {
			<-dial.entered
		}
		cancel()
		synctest.Wait()
		select {
		case got := <-done:
			if got.Ok || got.Failure != egresshealth.FailureNotMeasured || got.NotMeasured != 3 {
				t.Fatal("lost path was promoted to a measured failure or pass")
			}
		default:
			t.Fatal("lost path did not finish its admitted attempts")
		}
		if dial.active.Load() != 0 {
			t.Error("NotMeasured result left detached raw dials active")
		}
	})
}

// A TLS handshake blocked on a synthetic write owns the same request lifetime.
// Close unblocks that write; this is an owned connection, never a borrowed one.
type probeRequestOwnerConn struct {
	writeStarted chan struct{}
	closed       chan struct{}
	writeOnce    sync.Once
	closeOnce    sync.Once
	closeCount   atomic.Int64
}

func newProbeRequestOwnerConn() *probeRequestOwnerConn {
	return &probeRequestOwnerConn{writeStarted: make(chan struct{}), closed: make(chan struct{})}
}

func (self *probeRequestOwnerConn) Read([]byte) (int, error) {
	<-self.closed
	return 0, net.ErrClosed
}
func (self *probeRequestOwnerConn) Write([]byte) (int, error) {
	self.writeOnce.Do(func() { close(self.writeStarted) })
	<-self.closed
	return 0, net.ErrClosed
}
func (self *probeRequestOwnerConn) Close() error {
	self.closeCount.Add(1)
	self.closeOnce.Do(func() { close(self.closed) })
	return nil
}
func (self *probeRequestOwnerConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (self *probeRequestOwnerConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (self *probeRequestOwnerConn) SetDeadline(time.Time) error      { return nil }
func (self *probeRequestOwnerConn) SetReadDeadline(time.Time) error  { return nil }
func (self *probeRequestOwnerConn) SetWriteDeadline(time.Time) error { return nil }

func TestProbeHttpRequestOwnerCancellationClosesTlsHandshake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn := newProbeRequestOwnerConn()
		defer func() { _ = conn.Close(); synctest.Wait() }()
		client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
			return conn, nil
		}, nil, []string{"echo.example"}, time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := startProbeRequestOwner(t, client, ctx)
		<-conn.writeStarted
		cancel()
		synctest.Wait()
		requireProbeRequestOwnerResult(t, done, context.Canceled)
		if conn.closeCount.Load() == 0 {
			t.Error("request cancellation left its TLS connection open")
		}
	})
}

// Unrelated requests on one client keep independent owners: canceling one must
// not terminate the other or turn request ownership into a shared budget.
func TestProbeHttpRequestOwnerIndependentRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := newProbeRequestOwnerDial()
		defer dial.close()
		client := httpClientOverDialerWithHosts(dial.dial, nil, []string{"echo.example"}, time.Minute)
		firstCtx, cancelFirst := context.WithCancel(context.Background())
		defer cancelFirst()
		first := startProbeRequestOwner(t, client, firstCtx)
		<-dial.entered
		secondCtx, cancelSecond := context.WithCancel(context.Background())
		defer cancelSecond()
		second := startProbeRequestOwner(t, client, secondCtx)
		secondDial := <-dial.entered
		cancelFirst()
		synctest.Wait()
		requireProbeRequestOwnerResult(t, first, context.Canceled)
		if secondDial.Err() != nil {
			t.Error("one request canceled an independent dial")
		}
		select {
		case <-second:
			t.Error("independent request terminated early")
		default:
		}
		cancelSecond()
		synctest.Wait()
		requireProbeRequestOwnerResult(t, second, context.Canceled)
	})
}

func TestProbeHttpRequestOwnerRetainsEarlierTunnelBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := newProbeRequestOwnerDial()
		defer dial.close()
		client := httpClientOverDialerWithHosts(dial.dial, nil, []string{"echo.example"}, time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		done := startProbeRequestOwner(t, client, ctx)
		<-dial.entered
		time.Sleep(30 * time.Second)
		synctest.Wait()
		requireProbeRequestOwnerResult(t, done, context.DeadlineExceeded)
		if ctx.Err() != nil || dial.active.Load() != 0 {
			t.Error("independent tunnel bound changed")
		}
	})
}

type probeRequestOwnerContextKey struct{}

func TestProbeHttpRequestOwnerPreservesValuesAndError(t *testing.T) {
	ctx := context.WithValue(context.Background(), probeRequestOwnerContextKey{}, "synthetic-owner")
	var sawValue atomic.Bool
	client := httpClientOverDialerWithHosts(func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		sawValue.Store(dialCtx.Value(probeRequestOwnerContextKey{}) == "synthetic-owner")
		return nil, errProbeOwnerFixture
	}, nil, []string{"echo.example"}, time.Minute)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://echo.example/my-ip-info", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if !errors.Is(err, errProbeOwnerFixture) || !sawValue.Load() {
		t.Fatal("dial error identity or context values changed")
	}
}

func TestProbeHttpRequestOwnerPolicyStillPrecedesDial(t *testing.T) {
	var dialed atomic.Bool
	client := httpClientOverDialerWithHosts(func(context.Context, string, string) (net.Conn, error) {
		dialed.Store(true)
		return nil, errProbeOwnerFixture
	}, nil, []string{"echo.example"}, time.Minute)
	_, err := client.Get("https://unlisted.example/my-ip-info")
	if !errors.Is(err, ErrPinHostUnknown) || dialed.Load() {
		t.Fatal("request ownership bypassed the host policy")
	}
}

// Exercise the wrapper's body boundary with an in-memory HTTP/1 peer. TLS
// policy is tested separately at the actual constructor; this control proves
// that joining completed dials does not cancel a successful streaming body.
func TestProbeHttpRequestOwnerBodySurvivesHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer clientConn.Close()
		defer serverConn.Close()
		sendBody := make(chan struct{})
		serverDone := make(chan error, 1)
		go func() {
			defer serverConn.Close()
			req, err := http.ReadRequest(bufio.NewReader(serverConn))
			if err == nil {
				err = req.Body.Close()
			}
			if err == nil {
				_, err = io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nConnection: close\r\n\r\n")
			}
			if err == nil {
				<-sendBody
				_, err = io.WriteString(serverConn, "payload")
			}
			serverDone <- err
		}()
		transport := &providerHttpTransport{Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				_, done, err := beginProviderHttpDial(ctx)
				if err != nil {
					return nil, err
				}
				defer done()
				return clientConn, nil
			},
		}}
		client := &http.Client{Transport: transport, Timeout: time.Minute}
		response, err := client.Get("http://echo.example/body")
		if err != nil {
			close(sendBody)
			t.Fatal(err)
		}
		client.CloseIdleConnections()
		close(sendBody)
		body, err := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err != nil || closeErr != nil || string(body) != "payload" {
			t.Fatalf("completed dial canceled its live response body: read=%v close=%v", err, closeErr)
		}
		if err := <-serverDone; err != nil {
			t.Fatal(err)
		}
	})
}

// Admission and Wait are serialized: close cancels and joins admitted work,
// while a net/http dial scheduled after retirement cannot start fresh work.
func TestProbeHttpRequestOwnerJoinsAndRejectsLateAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		owner := &providerHttpRequestOwner{ctx: ctx, cancel: cancel}
		detached := context.WithValue(context.Background(), providerHttpRequestKey{}, owner)
		dialCtx, done, err := beginProviderHttpDial(detached)
		if err != nil {
			t.Fatal(err)
		}
		closed := make(chan struct{})
		go func() { owner.close(); close(closed) }()
		synctest.Wait()
		if !errors.Is(dialCtx.Err(), context.Canceled) {
			t.Error("retirement did not cancel its admitted dial")
		}
		select {
		case <-closed:
			t.Error("owner returned before its dial joined")
		default:
		}
		done()
		<-closed
		lateCtx, lateDone, err := beginProviderHttpDial(detached)
		if !errors.Is(err, context.Canceled) || lateCtx != nil || lateDone != nil {
			t.Fatal("retired owner admitted late detached work")
		}
	})
}
