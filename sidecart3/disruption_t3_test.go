//go:build sidecar_disruption

// §22.8's disruption rows beyond the schedules (TEST-M2-15; D-TEST-7, R-TEST-2) on T3, each carried out for real by
// disruption.go: an lb drain through nginx from warp's directives, a resident killed mid-stream on server's exchange, a
// Redis master's restart under an evs world, and a Postgres restart mid-request in server's api. They use real time
// and real processes, so they build with sidecar_disruption and run in sidecar-t3.sh's nightly form only; the stream,
// Redis and Postgres rows need the private test stores (and the Redis row the local stack's MinIO), and nginx the lb
// rows (and the lb row with a client the local stack's MinIO too). Each fails rather than skips without what it needs.
// Two rows need a build tag more, so each has its file: the Redis row (disruption_redis_t3_test.go) builds with
// sidecar_testhooks, without which the evs world drives no subscription, and the Postgres row
// (disruption_postgres_t3_test.go) with sidecar_pending, whose MG1 creates the auth keys its request writes.
//
// What these show and what they do not:
//
//   - lb drain: nginx cannot retry a streamed segment PUT whose upstream is cut mid-body, and answers 502, while it
//     re-sends a buffered PUT to the sibling. The client half (QD-758): an evs world's writer sends its segment PUTs
//     through that nginx to two of the world's instances, a drain cuts the first one 1 KiB into its body and kills its
//     instance, and evs's client reads nginx's 502 as no answer, sends the same bytes again, and the segment lands once
//     on the sibling, the push committing once and reading back. Only segment PUTs go through nginx; the world's model
//     of the lb carries the rest, which nginx buffers and re-sends itself, as the buffered twin shows.
//   - resident: a stream whose peer's resident is killed, with the peer gone past the sender's ack timeout, resets
//     TRANSFER_FAILED on the sender, and what the reader got is a prefix of what was sent; a resident killed under a peer
//     that reconnects at once (a migration) keeps the stream's bytes whole or resets it, never a gap; and a mutating call
//     whose callee's resident dies mid-run is answered once, its retry under its request_id running nothing twice. The
//     ack timeout is the setting it is, shortened from connect's 60 s so a nightly run takes seconds.
//   - Redis: an evs world's soft state, leases and push sequences among it, goes with the restart; every committed
//     version still reads and verifies, the next push's sequence is above every earlier one, and a subscriber that lost
//     its lease catches up at its hourly resync and leases again, as FS-62 has it on fakes.
//   - Postgres: a request whose transaction's connection is terminated at its commit is answered 503 with
//     Retry-After: 1 (QD-759) and takes no effect, so the retry the answer asks for applies it once. The cut is a
//     backend's termination, which raises 57P01; a restart's other classes are held in server/router's own tests.

package sidecart3

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
)

// How long, in real time, a disruption row's step may take before it fails: only a broken run's bound.
const testDisruptionTimeout = 60 * time.Second

// The private stores, a database of the test's own, with server's migrations applied, as a row with clients or api
// calls needs.
func disruptionTestEnv() *server.TestEnv {
	return &server.TestEnv{ApplyDbMigrations: true}
}

// Two upstream blocks behind the lb: the first request to reach either reads 1 KiB of its body and holds there until
// its connection is cut; any later request is read whole and answered 200 naming its block.
type lbDrainUpstreams struct {
	arrivals chan string

	stateLock sync.Mutex
	served    []string
}

func newLbDrainUpstreams() *lbDrainUpstreams {
	return &lbDrainUpstreams{arrivals: make(chan string, 4)}
}

func (self *lbDrainUpstreams) handler(name string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		first := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.served = append(self.served, name)
			return len(self.served) == 1
		}()
		if first {
			prefix := make([]byte, 1024)
			io.ReadFull(request.Body, prefix)
			self.arrivals <- name
			// held mid-body until the drain cuts the connection
			<-request.Context().Done()
			return
		}
		byteCount, _ := io.Copy(io.Discard, request.Body)
		fmt.Fprintf(writer, "%s took %d\n", name, byteCount)
	})
}

func (self *lbDrainUpstreams) servedBy() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string{}, self.served...)
}

// A 1 MiB PUT of path through the lb, its first upstream drained while it holds the body: what the client got, the
// upstreams that took it, the one drained, and nginx's log.
func runLbDrain(t *testing.T, path string) (int, string, []string, string, string) {
	t.Helper()
	upstreams := newLbDrainUpstreams()
	lb, err := NewLbFixture(t, nil,
		&LbUpstream{Name: "evs-g1", Handler: upstreams.handler("evs-g1")},
		&LbUpstream{Name: "evs-g2", Handler: upstreams.handler("evs-g2")},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer lb.Close()
	ctx, cancel := context.WithTimeout(t.Context(), testDisruptionTimeout)
	defer cancel()
	type answer struct {
		status int
		body   string
		err    error
	}
	answers := make(chan answer, 1)
	go func() {
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, lb.Url()+path, bytes.NewReader(bytes.Repeat([]byte("s"), 1024*1024)))
		if err != nil {
			answers <- answer{err: err}
			return
		}
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		response, err := client.Do(request)
		if err != nil {
			answers <- answer{err: err}
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		answers <- answer{status: response.StatusCode, body: string(body)}
	}()
	var drained string
	select {
	case drained = <-upstreams.arrivals:
	case <-ctx.Done():
		t.Fatalf("no upstream took the PUT: %s", lb.ErrorLog())
	}
	if err := lb.Drain(drained).Disrupt(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-answers:
		if got.err != nil {
			t.Fatalf("the PUT ended %v: %s", got.err, lb.ErrorLog())
		}
		return got.status, got.body, upstreams.servedBy(), drained, lb.ErrorLog()
	case <-ctx.Done():
		t.Fatalf("the PUT never ended: %s", lb.ErrorLog())
	}
	return 0, "", nil, "", ""
}

// A segment PUT streams through the lb, so once nginx has sent part of its body it cannot pass it to the sibling: a
// drain cutting its upstream answers the client 502, and only the drained block saw it.
func TestT3LbDrainCutsAStreamedPutWith502(t *testing.T) {
	status, body, served, drained, errorLog := runLbDrain(t, "/evs/v1/repos/r1/segments/s1")
	if status != http.StatusBadGateway {
		t.Errorf("the streamed PUT answered %d %q, not 502", status, body)
	}
	if len(served) != 1 || served[0] != drained {
		t.Errorf("the streamed PUT reached %v; nginx re-sent a streamed body: %s", served, errorLog)
	}
}

// The twin: a buffered PUT cut the same way is re-sent to the sibling by nginx (proxy_next_upstream error) and lands
// there whole.
func TestT3LbDrainRetriesABufferedPutOnTheSibling(t *testing.T) {
	status, body, served, drained, errorLog := runLbDrain(t, "/evs/v1/repos/r1/push")
	if status != http.StatusOK || !strings.HasSuffix(strings.TrimSpace(body), fmt.Sprintf("took %d", 1024*1024)) {
		t.Errorf("the buffered PUT answered %d %q: %s", status, body, errorLog)
	}
	if len(served) != 2 || served[0] != drained || served[1] == drained {
		t.Errorf("the buffered PUT reached %v, drained %s", served, drained)
	}
}

// The client half of the lb drain row (QD-758; the file header). writerA's segment PUTs go through nginx to evs1 and
// evs2, each block serving them with the world's own instance of its name (fault.EvsWorld.InstanceHandler); the first
// to reach a block has that block drained, and its instance killed, once cutByteCount bytes of its body are through.
// Safe for concurrent use.
type lbDrainSegmentRow struct {
	ctx     context.Context
	content []byte
	// how much of the first segment PUT's body its block reads before the drain
	cutByteCount int
	// writerA's transport to nginx, which keeps no connection, so each attempt dials it afresh
	transport *http.Transport

	stateLock sync.Mutex
	lb        *LbFixture
	world     *fault.EvsWorld
	net       *sidecartest.Net
	cutTaken  bool
	drained   string
	drainErr  error
	// what each block took, in arrival order, and what writerA got for each segment PUT it sent nginx
	attempts []*lbDrainSegmentAttempt
	answers  []string
}

// One segment PUT as a block took it.
type lbDrainSegmentAttempt struct {
	upstream string
	// the path and query, which name the segment and the push
	target        string
	contentLength int64
	// what the block read of its body, and whether the drain cut it there
	read []byte
	cut  bool
}

// The read a block drained mid-body fails with.
var errLbDrainCut = errors.New("the block was drained mid-body")

func newLbDrainSegmentRow(ctx context.Context, content []byte, cutByteCount int) *lbDrainSegmentRow {
	return &lbDrainSegmentRow{
		ctx:          ctx,
		content:      content,
		cutByteCount: cutByteCount,
		transport:    &http.Transport{DisableKeepAlives: true},
	}
}

func (self *lbDrainSegmentRow) setLb(lb *LbFixture) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lb = lb
}

// The block named name: the world's instance of that name, reading each request's body through lbDrainSegmentBody.
func (self *lbDrainSegmentRow) upstream(name string) *LbUpstream {
	return &LbUpstream{Name: name, Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempt := &lbDrainSegmentAttempt{upstream: name, target: request.URL.RequestURI(), contentLength: request.ContentLength}
		var world *fault.EvsWorld
		cutIn := -1
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			world = self.world
			if !self.cutTaken {
				self.cutTaken = true
				cutIn = self.cutByteCount
			}
			self.attempts = append(self.attempts, attempt)
		}()
		if world == nil {
			// no world yet: the row's schedule has not started, and a block answers nothing
			panic(http.ErrAbortHandler)
		}
		request.Body = &lbDrainSegmentBody{ReadCloser: request.Body, row: self, attempt: attempt, cutIn: cutIn}
		world.InstanceHandler(name).ServeHTTP(writer, request)
	})}
}

// What a block reads of a segment PUT, kept on its attempt. On the attempt the row cuts, the read after cutIn bytes
// drains the block and fails, as a read in a block killed there does, and so does every read after it.
type lbDrainSegmentBody struct {
	io.ReadCloser
	row     *lbDrainSegmentRow
	attempt *lbDrainSegmentAttempt
	// bytes left before the cut, negative on an attempt not cut
	cutIn   int
	drained bool
}

func (self *lbDrainSegmentBody) Read(buffer []byte) (int, error) {
	if 0 <= self.cutIn {
		if self.cutIn == 0 {
			if !self.drained {
				self.drained = true
				self.row.drain(self.attempt)
			}
			return 0, errLbDrainCut
		}
		buffer = buffer[:min(len(buffer), self.cutIn)]
	}
	n, err := self.ReadCloser.Read(buffer)
	self.row.noteRead(self.attempt, buffer[:n])
	if 0 <= self.cutIn {
		self.cutIn -= n
	}
	return n, err
}

func (self *lbDrainSegmentRow) noteRead(attempt *lbDrainSegmentAttempt, read []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	attempt.read = append(attempt.read, read...)
}

// attempt's block drained as warp drains one: nginx's connections to it cut and its listener closed
// (LbFixture.Drain), and its instance killed, so neither nginx nor the world's model of the lb reaches it again.
func (self *lbDrainSegmentRow) drain(attempt *lbDrainSegmentAttempt) {
	self.stateLock.Lock()
	lb, net := self.lb, self.net
	self.stateLock.Unlock()
	err := lb.Drain(attempt.upstream).Disrupt(self.ctx)
	if err == nil {
		err = net.Kill(attempt.upstream)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.drained = attempt.upstream
	self.drainErr = err
	attempt.cut = true
}

// A RoundTripper of one function.
type lbDrainRoundTrip func(request *http.Request) (*http.Response, error)

func (self lbDrainRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// A device's transport (fault.EvsWorldSettings.DeviceTransport): writerA's segment PUTs to nginx, which streams them,
// and every other request to the world's model of the lb, which stands in for what nginx buffers.
func (self *lbDrainSegmentRow) deviceTransport(device string, worldLb http.RoundTripper) http.RoundTripper {
	if device != "writerA" {
		return worldLb
	}
	return lbDrainRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPut || !strings.Contains(request.URL.Path, "/segments/") {
			return worldLb.RoundTrip(request)
		}
		self.stateLock.Lock()
		lb := self.lb
		self.stateLock.Unlock()
		lbUrl, err := url.Parse(lb.Url())
		if err != nil {
			return nil, err
		}
		throughLb := request.Clone(request.Context())
		throughLb.URL.Scheme = lbUrl.Scheme
		throughLb.URL.Host = lbUrl.Host
		response, err := self.transport.RoundTrip(throughLb)
		answer := "no answer"
		if err == nil {
			answer = fmt.Sprint(response.StatusCode)
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.answers = append(self.answers, answer)
		return response, err
	})
}

// The row on net: writerA writes the content, whose segment PUT the drain cuts, the reader reads it back, and the
// check holds the segment to landing once.
func (self *lbDrainSegmentRow) schedule(net *sidecartest.Net) *fault.Schedule {
	settings := fault.DefaultEvsWorldSettings()
	settings.Instances = []string{"evs1", "evs2", "evsr"}
	settings.Devices = []*fault.EvsWorldDeviceSettings{
		{Name: "writerA", Instances: []string{"evs1", "evs2"}, Writer: true},
		{Name: "reader", Instances: []string{"evsr"}},
	}
	settings.DeviceTransport = self.deviceTransport
	world := fault.NewEvsWorld(net, settings)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.world = world
		self.net = net
	}()
	steps := []*fault.Step{
		{Actor: "writerA", Action: world.Setup},
		{Actor: "writerA", Action: world.Write("writerA", "drained.bin", self.content)},
		{Actor: "reader", Action: world.Verify("reader", "drained.bin")},
		{Actor: "check", Action: world.Expect("the segment PUT the drain cut retried from the client and landed once", func(ctx context.Context) error {
			return self.landedOnce(ctx, world, net)
		})},
	}
	return &fault.Schedule{Id: "disruption lb drain segment put", Steps: steps, Check: world.Checks(), Tiers: []fault.Tier{fault.TierT3}}
}

// What the row's check holds: the drained block took the first segment PUT and read cutByteCount bytes of it; the
// sibling took one more, the retry, whole, naming the same segment and push with the same bytes; writerA got
// nginx's 502 and then a 200, under its one push id; and the segment cluster holds the segment once.
func (self *lbDrainSegmentRow) landedOnce(ctx context.Context, world *fault.EvsWorld, net *sidecartest.Net) error {
	self.stateLock.Lock()
	drained, drainErr := self.drained, self.drainErr
	attempts := []lbDrainSegmentAttempt{}
	for _, attempt := range self.attempts {
		attempts = append(attempts, *attempt)
	}
	answers := append([]string{}, self.answers...)
	self.stateLock.Unlock()
	if drainErr != nil {
		return fmt.Errorf("the drain of %s: %w", drained, drainErr)
	}
	if len(attempts) != 2 {
		return fmt.Errorf("the blocks took %d segment PUTs, not the one the drain cut and its retry", len(attempts))
	}
	cut, retry := attempts[0], attempts[1]
	if !cut.cut || cut.upstream != drained || len(cut.read) != self.cutByteCount {
		return fmt.Errorf("the first segment PUT reached %s, %q drained, and was read for %d bytes", cut.upstream, drained, len(cut.read))
	}
	if retry.cut || retry.upstream == drained || int64(len(retry.read)) != retry.contentLength {
		return fmt.Errorf("the retry reached %s, %q drained, and was read for %d of %d bytes", retry.upstream, drained, len(retry.read), retry.contentLength)
	}
	if retry.target != cut.target || retry.contentLength != cut.contentLength || !bytes.HasPrefix(retry.read, cut.read) {
		return errors.New("the retry named another segment or push, or sent other bytes")
	}
	if len(answers) != 2 || answers[0] != fmt.Sprint(http.StatusBadGateway) || answers[1] != fmt.Sprint(http.StatusOK) {
		return fmt.Errorf("writerA got %v from nginx, not its 502 and then a 200", answers)
	}
	segmentPath, query, _ := strings.Cut(retry.target, "?")
	segmentIdText := segmentPath[strings.LastIndex(segmentPath, "/")+1:]
	segmentSum := sha256.Sum256(retry.read)
	if hex.EncodeToString(segmentSum[:]) != segmentIdText {
		return errors.New("the retry's bytes are not the segment its id names")
	}
	pushIds := world.DevicePushIds("writerA")
	if len(pushIds) != 1 || query != "push="+hex.EncodeToString(pushIds[0][:]) {
		return fmt.Errorf("writerA posted push ids %v, and its segment PUTs named %q", pushIds, query)
	}
	cluster, err := net.Host().Blobs.SegmentCluster()
	if err != nil {
		return err
	}
	infos, _, err := cluster.List(sidecartest.InspectContext(ctx), "seg/"+sidecar.EvsKeyId(world.RepoId)+"/", "", 16)
	if err != nil {
		return err
	}
	if len(infos) != 1 || !strings.HasSuffix(infos[0].Key, "/"+segmentIdText) || infos[0].ByteCount != retry.contentLength {
		return fmt.Errorf("the segment cluster holds %d objects under the repo's seg/, not the one segment", len(infos))
	}
	return nil
}

// T3's settings for the row: writerA waits on nginx and the stores in real time, where the barrier driver reads its
// goroutines as blocked with nothing at work, so no stall takes the baton, and a step may take the row's real-time
// bound. One actor works at a time throughout, so no order is lost.
func lbDrainSegmentSettings() *Settings {
	settings := DefaultSettings()
	netSettings := *settings.Net
	barriers := *netSettings.Barriers
	barriers.AwaitTimeout = testDisruptionTimeout
	barriers.StallTimeout = 0
	netSettings.Barriers = &barriers
	settings.Net = &netSettings
	return settings
}

// §22.8's lb drain row, its client half (QD-758; the file header): a streamed segment PUT cut by a drain retries from
// the client and lands once.
func TestT3LbDrainCutSegmentPutRetriesFromTheClientAndLandsOnce(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), testDisruptionTimeout)
		defer cancel()
		row := newLbDrainSegmentRow(ctx, disruptionBytes(t, 1024*1024), 1024)
		defer row.transport.CloseIdleConnections()
		lb, err := NewLbFixture(tb, nil, row.upstream("evs1"), row.upstream("evs2"))
		if err != nil {
			tb.Fatal(err)
		}
		defer lb.Close()
		row.setLb(lb)
		if result := Run(tb, row.schedule, lbDrainSegmentSettings()); result.Err != nil {
			tb.Logf("nginx's error log: %s", lb.ErrorLog())
		}
	})
}

// Two admitted sidecars' dispatchers on Required clients of platform (version_pair.go's sides), the sender's ack timeout
// ackTimeout, the receiver listening on target and handing each stream to accepted. The test's cleanup closes both.
func newDisruptionStreamPair(t *testing.T, ctx context.Context, platform *exchangePlatform, ackTimeout time.Duration, target string, accepted chan *sidecar.SubprotocolStream) (*T3SubprotocolSide, *T3SubprotocolSide) {
	t.Helper()
	sides, err := newT3SubprotocolSides(ctx, t, platform, func(name string, settings *connect.ClientSettings) {
		if name == "sender" {
			sendBuffer := *settings.SendBufferSettings
			sendBuffer.AckTimeout = ackTimeout
			settings.SendBufferSettings = &sendBuffer
		}
	}, "sender", "receiver")
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range sides {
		t.Cleanup(side.Close)
	}
	sender, receiver := sides[0], sides[1]
	if _, err := receiver.Subprotocol.Listen(target, func(ctx context.Context, incoming *sidecar.StreamIncoming) error {
		accepted <- incoming.Stream
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return sender, receiver
}

// A stream from sender to receiver, its first bytes read across, and both ends.
func openDisruptionStream(t *testing.T, ctx context.Context, sender *T3SubprotocolSide, receiver *T3SubprotocolSide, target string, accepted chan *sidecar.SubprotocolStream, warm []byte) (*sidecar.SubprotocolStream, *sidecar.SubprotocolStream) {
	t.Helper()
	opened, err := sender.Subprotocol.OpenStream(ctx, sidecar.Id(receiver.Client.ClientId), &sidecar.StreamOpenRequest{Target: target})
	if err != nil {
		t.Fatalf("open through the exchange: %v", err)
	}
	t.Cleanup(func() { opened.Close() })
	var stream *sidecar.SubprotocolStream
	select {
	case stream = <-accepted:
	case <-ctx.Done():
		t.Fatal("no stream accepted")
	}
	t.Cleanup(func() { stream.Close() })
	if _, err := opened.Write(warm); err != nil {
		t.Fatal(err)
	}
	read := make([]byte, len(warm))
	if err := stream.SetReadDeadline(time.Now().Add(testDisruptionTimeout)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(stream, read); err != nil || !bytes.Equal(read, warm) {
		t.Fatalf("the stream's first bytes: %v", err)
	}
	return opened, stream
}

// Server's exchange for a stream row: the defaults, with a close that waits out a client whose contract work is still
// on the peer the row took away, which connect joins before a close returns.
func newDisruptionExchange(ctx context.Context, t *testing.T) *exchangePlatform {
	t.Helper()
	settings := DefaultExchangeSettings()
	settings.CloseTimeout = 3 * testDisruptionTimeout
	platform, err := newExchangePlatform(ctx, t, settings)
	if err != nil {
		t.Fatal(err)
	}
	return platform
}

// size random bytes.
func disruptionBytes(t *testing.T, size int) []byte {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

// The receiver's resident is killed mid-stream and the receiver does not come back: what the sender writes after is
// never acked, so at its ack timeout the stream resets TRANSFER_FAILED, and the reader holds a prefix of what was sent.
func TestT3ResidentKilledMidStreamResetsTransferFailed(t *testing.T) {
	disruptionTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*testDisruptionTimeout)
		defer cancel()
		platform := newDisruptionExchange(ctx, t)
		defer platform.Close()
		accepted := make(chan *sidecar.SubprotocolStream, 1)
		sender, receiver := newDisruptionStreamPair(t, ctx, platform, 3*time.Second, "disruption", accepted)
		warm := disruptionBytes(t, 4*1024)
		opened, stream := openDisruptionStream(t, ctx, sender, receiver, "disruption", accepted, warm)

		if err := NewResidentKill(platform.exchange, receiver.Client.ClientId).Disrupt(ctx); err != nil {
			t.Fatal(err)
		}
		// the receiver's device goes with its resident, and nothing it was sent is acked again
		receiver.Close()
		data := disruptionBytes(t, 64*1024)
		written := make(chan error, 1)
		go func() {
			_, err := opened.Write(data)
			if err == nil {
				// the write may have fit the window; the stream's end is what the ack timeout decides
				_, err = opened.Read(make([]byte, 1))
			}
			written <- err
		}()
		select {
		case err := <-written:
			resetError := &sidecar.StreamResetError{}
			if !errors.As(err, &resetError) || resetError.Code != sidecarprotocol.StreamResetCode_RST_TRANSFER_FAILED {
				t.Errorf("the sender's stream ended %v, not reset TRANSFER_FAILED", err)
			}
		case <-ctx.Done():
			t.Fatal("the sender's stream never ended")
		}
		// what the dead receiver's end held is a prefix of what was sent, never something else
		stream.SetReadDeadline(time.Now())
		held, _ := io.ReadAll(stream)
		if !bytes.HasPrefix(data, held) {
			t.Errorf("the receiver holds %d bytes that are not what was sent", len(held))
		}
	})
}

// The twin, a migration: the receiver's resident is killed and the receiver reconnects at once to a new one. The
// stream's bytes arrive whole, connect resending what the dead resident dropped, or the stream resets; never a gap.
func TestT3ResidentMigrationKeepsAStreamWholeOrResetsIt(t *testing.T) {
	disruptionTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*testDisruptionTimeout)
		defer cancel()
		platform := newDisruptionExchange(ctx, t)
		defer platform.Close()
		accepted := make(chan *sidecar.SubprotocolStream, 1)
		sender, receiver := newDisruptionStreamPair(t, ctx, platform, 30*time.Second, "disruption", accepted)
		warm := disruptionBytes(t, 4*1024)
		opened, stream := openDisruptionStream(t, ctx, sender, receiver, "disruption", accepted, warm)

		if err := NewResidentKill(platform.exchange, receiver.Client.ClientId).Disrupt(ctx); err != nil {
			t.Fatal(err)
		}
		data := disruptionBytes(t, 64*1024)
		written := make(chan error, 1)
		go func() {
			if _, err := opened.Write(data); err != nil {
				written <- err
				return
			}
			written <- opened.CloseWrite()
		}()
		if err := stream.SetReadDeadline(time.Now().Add(2 * testDisruptionTimeout)); err != nil {
			t.Fatal(err)
		}
		read, err := io.ReadAll(stream)
		resetError := &sidecar.StreamResetError{}
		switch {
		case err == nil:
			if !bytes.Equal(read, data) {
				t.Errorf("%d of %d bytes, changed or with a gap and no reset", len(read), len(data))
			}
		case errors.As(err, &resetError):
			if !bytes.HasPrefix(data, read) {
				t.Errorf("a reset after %d bytes that are not what was sent", len(read))
			}
		default:
			t.Errorf("the read ended %v after %d bytes", err, len(read))
		}
		select {
		case err := <-written:
			if err != nil && !errors.As(err, &resetError) {
				t.Errorf("the write ended %v", err)
			}
		case <-ctx.Done():
			t.Error("the write never ended")
		}
	})
}

// A mutating call the callee's resident dies under, mid-run: the call ends in its answer or in an error naming its
// request_id, and sent again under that request_id it is answered from the callee's idempotency cache or joins the run
// still live (§6.6), so the handler runs once whichever way the first attempt ended: §22.8's "RPC retries" across a
// resident's death.
func TestT3ResidentKilledMidCallRetriesWithOneEffect(t *testing.T) {
	disruptionTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*testDisruptionTimeout)
		defer cancel()
		platform := newDisruptionExchange(ctx, t)
		defer platform.Close()
		sides, err := newT3SubprotocolSides(ctx, t, platform, nil, "caller", "callee")
		if err != nil {
			t.Fatal(err)
		}
		for _, side := range sides {
			t.Cleanup(side.Close)
		}
		caller, callee := sides[0], sides[1]
		const messageType = sidecarprotocol.MessageType(0x8f10)
		runs := make(chan struct{}, 8)
		release := make(chan struct{})
		if _, err := callee.Subprotocol.Handle(messageType, func(ctx context.Context, request *sidecar.SubprotocolRequest) ([]byte, error) {
			runs <- struct{}{}
			<-release
			return []byte("done"), nil
		}, &sidecar.HandleOptions{Mutating: true}); err != nil {
			t.Fatal(err)
		}
		type answer struct {
			body []byte
			err  error
		}
		answers := make(chan answer, 1)
		go func() {
			body, err := caller.Subprotocol.Call(ctx, sidecar.Id(callee.Client.ClientId), messageType, []byte("once"), &sidecar.CallOptions{Timeout: testDisruptionTimeout / 2})
			answers <- answer{body: body, err: err}
		}()
		select {
		case <-runs:
		case <-ctx.Done():
			t.Fatal("the call never reached its handler")
		}
		if err := NewResidentKill(platform.exchange, callee.Client.ClientId).Disrupt(ctx); err != nil {
			t.Fatal(err)
		}
		close(release)
		var first answer
		select {
		case first = <-answers:
		case <-ctx.Done():
			t.Fatal("the call never ended")
		}
		body := first.body
		t.Logf("the attempt the resident died under ended %v", first.err)
		if first.err != nil {
			subprotocolError := &sidecar.SubprotocolError{}
			if !errors.As(first.err, &subprotocolError) || subprotocolError.RequestId == 0 {
				t.Fatalf("the call ended %v, naming no request_id to retry", first.err)
			}
			body, err = caller.Subprotocol.Call(ctx, sidecar.Id(callee.Client.ClientId), messageType, []byte("once"), &sidecar.CallOptions{Timeout: testDisruptionTimeout / 2, RequestId: subprotocolError.RequestId})
			if err != nil {
				t.Fatalf("the retry under request_id %d: %v", subprotocolError.RequestId, err)
			}
		}
		if string(body) != "done" {
			t.Errorf("the call answered %q", body)
		}
		select {
		case <-runs:
			t.Error("the handler ran twice")
		default:
		}
	})
}

// The twin, with the retry certain: the first attempt gives up at its own 2 s deadline while the handler runs, the
// callee's resident dies, and the call sent again under its request_id joins the run still live, which answers once.
func TestT3ResidentKilledUnderACallItsRetryJoinsTheRun(t *testing.T) {
	disruptionTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*testDisruptionTimeout)
		defer cancel()
		platform := newDisruptionExchange(ctx, t)
		defer platform.Close()
		sides, err := newT3SubprotocolSides(ctx, t, platform, nil, "caller", "callee")
		if err != nil {
			t.Fatal(err)
		}
		for _, side := range sides {
			t.Cleanup(side.Close)
		}
		caller, callee := sides[0], sides[1]
		const messageType = sidecarprotocol.MessageType(0x8f11)
		runs := make(chan struct{}, 8)
		release := make(chan struct{})
		if _, err := callee.Subprotocol.Handle(messageType, func(ctx context.Context, request *sidecar.SubprotocolRequest) ([]byte, error) {
			runs <- struct{}{}
			<-release
			return []byte("done"), nil
		}, &sidecar.HandleOptions{Mutating: true}); err != nil {
			t.Fatal(err)
		}
		calleeId := sidecar.Id(callee.Client.ClientId)
		_, err = caller.Subprotocol.Call(ctx, calleeId, messageType, []byte("once"), &sidecar.CallOptions{Timeout: 2 * time.Second})
		subprotocolError := &sidecar.SubprotocolError{}
		if !errors.As(err, &subprotocolError) || subprotocolError.Code != sidecarprotocol.ErrorCode_ERR_DEADLINE_EXCEEDED || subprotocolError.RequestId == 0 {
			close(release)
			t.Fatalf("the first attempt ended %v, not its own deadline naming its request_id", err)
		}
		select {
		case <-runs:
		default:
			close(release)
			t.Fatal("the first attempt never reached its handler")
		}
		if err := NewResidentKill(platform.exchange, callee.Client.ClientId).Disrupt(ctx); err != nil {
			close(release)
			t.Fatal(err)
		}
		type answer struct {
			body []byte
			err  error
		}
		answers := make(chan answer, 1)
		go func() {
			body, err := caller.Subprotocol.Call(ctx, calleeId, messageType, []byte("once"), &sidecar.CallOptions{Timeout: testDisruptionTimeout / 2, RequestId: subprotocolError.RequestId})
			answers <- answer{body: body, err: err}
		}()
		close(release)
		select {
		case retried := <-answers:
			if retried.err != nil || string(retried.body) != "done" {
				t.Errorf("the retry under request_id %d answered %q %v", subprotocolError.RequestId, retried.body, retried.err)
			}
		case <-ctx.Done():
			t.Fatal("the retry never ended")
		}
		select {
		case <-runs:
			t.Error("the handler ran twice")
		default:
		}
	})
}
