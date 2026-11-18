//go:build sidecar_pending

package main

// FS-52 on T3 (Q-OPS-5; §12.6, §19.11, D-OPS-11; DNS-M5-24): dns API blocks N and N+1 serve together at 25%, the
// edges applying both blocks' journals and snapshots, then a rollback from N+1 to N.
//
// Both blocks run over one dnshost.Store on the private stack's Postgres and Redis, as a staged deploy's do. N+1 is
// this release's block with the fields a next release adds in its answers: each answer and each row gains one, which
// N's edges must ignore (§12.6: "N ignores fields N+1 adds"). A row of a format N cannot read is what N+1 holds back
// until it serves 100%: the control shows an edge refusing such an answer and keeping its last good view, which is why
// the rollout gates it. One binary is built here, so N+1's own new code is the field this test adds and nothing else;
// a real N+1 build against a real N runs at T6 with the deploy's rollout (External).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The field N+1 adds to every answer and row: one N does not know.
const dnsT3NextReleaseField = "next_release_field"

// Routes each edge's feed requests between the harness's block, N, and a second block, N+1: every fourth to N+1 while
// it serves, so a quarter of the feed rides it, as a staged rollout's 25% does. Safe for concurrent use.
type dnsT3Rollout struct {
	stateLock sync.Mutex
	next      http.Handler
	// Answers whose rows N+1 holds back until 100%: a format N cannot read.
	unreadable bool
	nsCounts   map[string]int
	// Feed answers each block gave with rows or a snapshot in them.
	nextAnswered    int
	currentAnswered int
	changed         chan struct{}
}

func newDnsT3Rollout() *dnsT3Rollout {
	return &dnsT3Rollout{nsCounts: map[string]int{}, changed: make(chan struct{})}
}

// Whether a request is the feed's: the journal or a snapshot.
func dnsT3FeedPath(path string) bool {
	return strings.HasSuffix(path, "/journal") || strings.HasSuffix(path, "/snapshot")
}

// DnsHarnessSettings.EdgeTransport: the edge's feed requests between N and N+1, everything else to N.
func (self *dnsT3Rollout) wrap(ns string, current http.RoundTripper) http.RoundTripper {
	return dnsT3RoundTrip(func(request *http.Request) (*http.Response, error) {
		if !dnsT3FeedPath(request.URL.Path) {
			return current.RoundTrip(request)
		}
		next, unreadable := func() (http.Handler, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.nsCounts[ns] += 1
			if self.next == nil || self.nsCounts[ns]%4 != 0 {
				return nil, false
			}
			return self.next, self.unreadable
		}()
		if next == nil {
			response, err := current.RoundTrip(request)
			if err == nil && response.StatusCode == http.StatusOK {
				self.answered(false)
			}
			return response, err
		}
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, request)
		response := recorder.Result()
		response.Request = request
		if response.StatusCode != http.StatusOK {
			return response, nil
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			return nil, err
		}
		added, err := dnsT3NextReleaseAnswer(body, unreadable)
		if err != nil {
			return nil, err
		}
		response.Body = io.NopCloser(bytes.NewReader(added))
		response.ContentLength = int64(len(added))
		self.answered(true)
		return response, nil
	})
}

func (self *dnsT3Rollout) answered(next bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if next {
		self.nextAnswered += 1
	} else {
		self.currentAnswered += 1
	}
	close(self.changed)
	self.changed = make(chan struct{})
}

// N+1 serves from now on, or with nil no longer: the rollback.
func (self *dnsT3Rollout) setNext(next http.Handler, unreadable bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.next = next
	self.unreadable = unreadable
}

func (self *dnsT3Rollout) counts() (current int, next int, changed chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.currentAnswered, self.nextAnswered, self.changed
}

// An answer as N+1 writes it: every JSON object in it, the answer, each row and each snapshot line, gains the field
// N does not know, and with unreadable a format N cannot read.
func dnsT3NextReleaseAnswer(body []byte, unreadable bool) ([]byte, error) {
	change := func(object map[string]any) {
		object[dnsT3NextReleaseField] = "ignored by N"
		if _, ok := object["format"]; ok && unreadable {
			object["format"] = dnsservice.DnsFormat + 1
		}
		if rows, ok := object["rows"].([]any); ok {
			for _, row := range rows {
				if rowObject, ok := row.(map[string]any); ok {
					rowObject[dnsT3NextReleaseField] = "ignored by N"
				}
			}
		}
	}
	out := &bytes.Buffer{}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	for {
		object := map[string]any{}
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		change(object)
		line, err := json.Marshal(object)
		if err != nil {
			return nil, err
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// N+1: a block of its own over the harness's store and deps, on the Net's clock and the harness's gates.
func (self *dnsT3) nextBlock(t testing.TB, ctx context.Context) *dnsservice.DnsApi {
	t.Helper()
	host := *self.net.Host()
	host.Metrics = sidecartest.NewFakeMetrics(nil)
	host.Gates = self.harness.Gates()
	host.Clock = self.net.Clock()
	block, err := dnsservice.NewDnsApi(ctx, &host, self.settings, self.store, self.harness.ApiDeps())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(block.Close)
	waitCtx, cancel := context.WithTimeout(ctx, dnsT3WaitTimeout)
	defer cancel()
	for {
		count, changed := block.Probes()
		if 0 < count && block.Ready() {
			return block
		}
		select {
		case <-changed:
		case <-waitCtx.Done():
			t.Fatalf("N+1 never read its store: %v", waitCtx.Err())
		}
	}
}

func TestFs52BlocksNAndNextServeTogetherAndRollBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rollout := newDnsT3Rollout()
		stack := newDnsT3(t, ctx, func(settings *sidecartest.DnsHarnessSettings) {
			settings.EdgeTransport = rollout.wrap
		})
		networkId, apex := stack.network(t, ctx, "")
		stack.awaitLatest(t, ctx)
		write := func(owner string, text string) uint64 {
			t.Helper()
			if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: stack.root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{dnsT3Txt(owner, text)}}); err != nil {
				t.Fatal(err)
			}
			return stack.awaitLatest(t, ctx)
		}
		requireBoth := func(owner string, text string) {
			t.Helper()
			for _, ns := range stack.settings.Ns {
				if _, texts := stack.answer(t, ctx, ns, owner, dns.TypeTXT); len(texts) != 1 || texts[0] != `"`+text+`"` {
					t.Errorf("%s answers %s with %v, want %q", ns, owner, texts, text)
				}
			}
		}

		// N and N+1 together, a quarter of the feed on N+1: both NS apply every write, whichever block answered it
		next := stack.nextBlock(t, ctx)
		rollout.setNext(next.Handler(), false)
		for index := range 12 {
			owner := "rollout." + apex
			text := "write " + string(rune('a'+index))
			write(owner, text)
			requireBoth(owner, text)
		}
		current, nextCount, _ := rollout.counts()
		if nextCount == 0 || current == 0 {
			t.Fatalf("the feed rode one block: N %d, N+1 %d", current, nextCount)
		}
		t.Logf("FS-52 at 25%%: N answered %d feed requests, N+1 %d", current, nextCount)

		// the control: a row N cannot read is refused, and the edge keeps its last good view until N answers again
		rollout.setNext(next.Handler(), true)
		if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: stack.root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{dnsT3Txt("gated."+apex, "gated")}}); err != nil {
			t.Fatal(err)
		}
		gated := stack.serial(t, ctx)
		waitCtx, cancel := context.WithTimeout(ctx, dnsT3WaitTimeout)
		defer cancel()
		refused := func() bool {
			for _, ns := range stack.settings.Ns {
				if 0 < stack.harness.EdgeMetrics(ns).Value("urnetwork_dnsedge_feed_refused_total", "format") {
					return true
				}
			}
			return false
		}
		// each edge asks until one of its polls lands on N+1, a failed apply waiting its backoff on the Net's clock
		for !refused() {
			_, timerAdded := stack.net.Clock().WatchTimers()
			_, _, answered := rollout.counts()
			if refused() {
				break
			}
			select {
			case <-timerAdded:
				stack.net.Clock().Advance(time.Duration(stack.settings.Feed.RetryMaxSeconds) * time.Second)
			case <-answered:
			case <-waitCtx.Done():
				t.Fatalf("no edge took an unreadable answer: %v", waitCtx.Err())
			}
		}
		for _, ns := range stack.settings.Ns {
			if served, _ := stack.harness.Edge(ns).ServedSerial(stack.root); gated < served {
				t.Errorf("%s serves %d past the gated write %d", ns, served, gated)
			}
		}

		// the rollback: N+1 stops and N answers alone; both NS go on from their last good views
		rollout.setNext(nil, false)
		for range 2 {
			stack.net.Clock().Advance(time.Duration(stack.settings.Feed.RetryMaxSeconds) * time.Second)
		}
		if err := stack.harness.AwaitServed(waitCtx, stack.root, gated); err != nil {
			t.Fatal(err)
		}
		requireBoth("gated."+apex, "gated")
		after := write("rolled-back."+apex, "after the rollback")
		requireBoth("rolled-back."+apex, "after the rollback")
		for _, ns := range stack.settings.Ns {
			if soa := stack.query(t, ctx, ns, stack.root, dns.TypeSOA, false); soa.Answer[0].(*dns.SOA).Serial != uint32(after) {
				t.Errorf("%s after the rollback: %v", ns, soa.Answer)
			}
		}
	})
}
