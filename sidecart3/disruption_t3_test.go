//go:build sidecar_disruption

// §22.8's disruption rows beyond the schedules (TEST-M2-15; D-TEST-7, R-TEST-2) on T3, each carried out for real by
// disruption.go: an lb drain through nginx from warp's directives, a resident killed mid-stream on server's exchange, a
// Redis master's restart under an evs world, and a Postgres restart mid-request in server's api. They use real time
// and real processes, so they build with sidecar_disruption and run in sidecar-t3.sh's nightly form only; the stream,
// Redis and Postgres rows need the private test stores (and the Redis row the local stack's MinIO), and nginx the lb
// rows. Each fails rather than skips without what it needs.
//
// What these show and what they do not:
//
//   - lb drain: nginx cannot retry a streamed segment PUT whose upstream is cut mid-body, and answers 502, while it
//     re-sends a buffered PUT to the sibling. The client half, evs's client retrying that 502 and the segment landing
//     once, is not here: the client does not retry a 502 (question test-m2-15-streamed-put-502-not-retried).
//   - resident: a stream whose peer's resident is killed, with the peer gone past the sender's ack timeout, resets
//     TRANSFER_FAILED on the sender, and what the reader got is a prefix of what was sent; a resident killed under a peer
//     that reconnects at once (a migration) keeps the stream's bytes whole or resets it, never a gap. The ack timeout is
//     the setting it is, shortened from connect's 60 s so a nightly run takes seconds.
//   - Redis: an evs world's soft state, leases and push sequences among it, goes with the restart; every committed
//     version still reads and verifies, the next push's sequence is above every earlier one, and a subscriber that lost
//     its lease catches up at its hourly resync and leases again, as FS-62 has it on fakes.
//   - Postgres: a request whose transaction's connection is terminated at its commit is refused and takes no effect,
//     so a retry applies it once; whether its status must be 503 is question test-m2-15-postgres-restart-answer.

package sidecart3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
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

// An evs world on T3 through a Redis master's restart, FS-62's two clauses (EVS1-M2-41) with the real thing in place of
// LoseSoftState: every key of the test's database gone and every connection to it cut, not four families deleted. What
// is durable is in MinIO and stays: the reader's reads after it are served from the cluster's head, and the next
// commit's push sequence is above every earlier one. The reader's lease is gone, so the commit after the restart is
// hinted to no one; the hourly resync reads main fresh and catches it, the next round leases again, and the commit
// after that is hinted and read.
func disruptionRedisSchedule(net *sidecartest.Net) *fault.Schedule {
	world := fault.NewEvsWorld(net, nil)
	restart := NewRedisRestart()
	sequenceBefore := uint64(0)
	steps := []*fault.Step{
		{Actor: "writerA", Action: world.Setup},
		// §20.4's evs_subscribers_per_repo, which the lease route reads before it writes (FS-12's step)
		{Actor: "admin", Action: world.SetQuotaLimits(map[string]int64{"evs_subscribers_per_repo": 16})},
		{Actor: "writerA", Action: world.Write("writerA", "a.txt", fault.EvsWorldContent("disruption a", 200))},
		{Actor: "reader", Action: world.Subscribe("reader")},
		{Actor: "check", Action: world.ExpectLease("reader", true)},
		// past the reader's 5 s reread, so its round reads for the hint alone
		{Advance: 6 * time.Second},
		{Actor: "writerB", Action: world.Write("writerB", "b.txt", fault.EvsWorldContent("disruption b", 200))},
		{Actor: "reader", Action: world.DeliverHints("reader")},
		{Actor: "check", Action: world.ExpectHinted("reader", "b.txt", true)},
		{Actor: "reader", Action: world.SubscriptionRound("reader")},
		{Actor: "check", Action: world.ExpectChange("reader", "b.txt")},
		{Actor: "reader", Action: world.VerifyCached("reader", "a.txt", "b.txt")},
		{Actor: "check", Action: world.Expect("the push sequence stands", func(ctx context.Context) error {
			sequence, err := world.PushSequence(ctx)
			if err != nil {
				return err
			}
			if sequence == 0 {
				return errors.New("no push sequence after two commits")
			}
			sequenceBefore = sequence
			return nil
		})},
		{Advance: time.Second},
		{Actor: "ops", Action: func(ctx context.Context, net *sidecartest.Net) error {
			if err := restart.Disrupt(ctx); err != nil {
				return err
			}
			if restart.KilledCount() == 0 {
				return errors.New("the restart cut no connection of the test's database")
			}
			return nil
		}},
		{Actor: "check", Action: world.ExpectLease("reader", false)},
		{Actor: "reader", Action: world.VerifyCached("reader", "a.txt", "b.txt")},
		{Actor: "writerA", Action: world.Write("writerA", "c.txt", fault.EvsWorldContent("disruption c", 200))},
		{Actor: "check", Action: world.Expect("the next sequence is above every earlier one", func(ctx context.Context) error {
			sequence, err := world.PushSequence(ctx)
			if err != nil {
				return err
			}
			if sequence <= sequenceBefore {
				return fmt.Errorf("the push sequence fell from %d to %d", sequenceBefore, sequence)
			}
			return nil
		})},
		{Actor: "reader", Action: world.DeliverHints("reader")},
		{Actor: "check", Action: world.ExpectHinted("reader", "c.txt", false)},
		// the hourly resync reads main fresh and catches c.txt, and the next round leases again
		{Advance: time.Hour},
		{Actor: "reader", Action: world.SubscriptionRound("reader")},
		{Actor: "check", Action: world.ExpectChange("reader", "c.txt")},
		{Actor: "reader", Action: world.SubscriptionRound("reader")},
		{Actor: "check", Action: world.ExpectLease("reader", true)},
		{Actor: "writerB", Action: world.Write("writerB", "d.txt", fault.EvsWorldContent("disruption d", 200))},
		{Actor: "reader", Action: world.DeliverHints("reader")},
		{Actor: "check", Action: world.ExpectHinted("reader", "d.txt", true)},
		{Actor: "reader", Action: world.SubscriptionRound("reader")},
		{Actor: "check", Action: world.ExpectChange("reader", "d.txt")},
		{Actor: "reader", Action: world.Verify("reader", "a.txt", "b.txt", "c.txt", "d.txt")},
	}
	return &fault.Schedule{Id: "disruption redis restart", Steps: steps, Check: world.Checks(), Tiers: []fault.Tier{fault.TierT3}}
}

func TestT3RedisMasterRestartLosesOnlyRebuildableState(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		Run(tb, disruptionRedisSchedule, nil)
	})
}

// What a request is marked with, so the commit hook acts on its commit alone.
type disruptionRequestKey struct{}

// POST /network/auth-keys's transaction has its connection terminated at its commit, as a Postgres or PgBouncer restart
// mid-request does: the api's answer is a refusal, nothing of the request took effect, and the same request sent again
// makes one key, so a retry has no double effect. The status is logged and not held: server's router answers the
// raised 57P01 with its fixed 500, where §22.8's "answers retryable" may mean the 503 and Retry-After a lock timeout
// already gets (question test-m2-15-postgres-restart-answer), so this row proves the no-double-effect half alone.
func TestT3PostgresRestartMidRequest(t *testing.T) {
	disruptionTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), testDisruptionTimeout)
		defer cancel()
		net := sidecartest.NewNet(ctx, nil)
		defer net.Close()
		api := NewT3Api(ctx, net, nil)
		networkId := server.NewId()
		userId := server.NewId()
		networkName := fmt.Sprintf("disrupt%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		root := "Bearer " + session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign()

		restart := NewPostgresRestart(nil)
		restarted := make(chan error, 1)
		server.SetPgCommitFaultForTest(t, func(ctx context.Context, commit func() error, rollback func()) error {
			if ctx.Value(disruptionRequestKey{}) == nil {
				return commit()
			}
			select {
			case restarted <- restart.Disrupt(context.WithoutCancel(ctx)):
			default:
			}
			return commit()
		})
		keyCount := func() int {
			count := 0
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_auth_key WHERE network_id = $1`, networkId).Scan(&count))
			})
			return count
		}
		before := keyCount()
		args := &model.CreateAuthKeyArgs{DurationSeconds: 600, MaxUses: 1}
		status, answer, err := api.Call(context.WithValue(ctx, disruptionRequestKey{}, true), http.MethodPost, "/network/auth-keys", root, args)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-restarted:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("the request committed nothing for the restart to cut: %d %s", status, answer)
		}
		if status == http.StatusOK {
			t.Fatalf("a commit on a terminated connection answered 200 %s", answer)
		}
		t.Logf("a commit cut by a Postgres restart answers %d %s", status, strings.TrimSpace(string(answer)))
		if status < 500 {
			t.Errorf("the api answered %d, a refusal of the request's own rather than a server's failure", status)
		}
		if after := keyCount(); after != before {
			t.Errorf("the cut request left %d keys, from %d", after, before)
		}
		status, answer, err = api.Call(ctx, http.MethodPost, "/network/auth-keys", root, args)
		if err != nil || status != http.StatusOK {
			t.Fatalf("the request after the restart: %d %s %v", status, answer, err)
		}
		result := &model.CreateAuthKeyResult{}
		if err := json.Unmarshal(answer, result); err != nil || result.AuthKey == "" {
			t.Fatalf("the request after the restart answered %s", answer)
		}
		if after := keyCount(); after != before+1 {
			t.Errorf("the request after the restart left %d keys, from %d", after, before)
		}
	})
}
