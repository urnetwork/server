// A private PostgreSQL proxy drops an actually completed commit acknowledgement.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"maps"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"gopkg.in/yaml.v3"
)

// Only the disposable child database is routed through this loopback listener.
// No frame contents are retained. The armed one-operation window discards the
// real COMMIT CommandComplete and its idle ReadyForQuery, after PostgreSQL has
// finished the transaction and before the normal server.Tx owner sees its reply.
type legacyCohortCommitReplyProxy struct {
	listener      net.Listener
	target        string
	ctx           context.Context
	cancel        context.CancelFunc
	armed         atomic.Bool
	committed     atomic.Int64
	idle          atomic.Int64
	stateLock     sync.Mutex
	connectionKVs map[net.Conn]bool
	accepted      chan struct{}
	joined        sync.WaitGroup
	closeOnce     sync.Once
}

func newLegacyCohortCommitReplyProxy(t testing.TB, ctx context.Context, target string) *legacyCohortCommitReplyProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("private commit proxy could not listen", err)
	}
	owned, cancel := context.WithCancel(ctx)
	proxy := &legacyCohortCommitReplyProxy{listener: listener, target: target, ctx: owned, cancel: cancel,
		connectionKVs: map[net.Conn]bool{}, accepted: make(chan struct{})}
	go func() {
		defer close(proxy.accepted)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			proxy.joined.Add(1)
			go func() { defer proxy.joined.Done(); proxy.forward(client) }()
		}
	}()
	return proxy
}

func (self *legacyCohortCommitReplyProxy) close() {
	self.closeOnce.Do(func() {
		self.cancel()
		_ = self.listener.Close()
		<-self.accepted
		self.stateLock.Lock()
		for conn := range self.connectionKVs {
			_ = conn.Close()
		}
		self.stateLock.Unlock()
		self.joined.Wait()
	})
}

func (self *legacyCohortCommitReplyProxy) forward(client net.Conn) {
	defer client.Close()
	upstream, err := (&net.Dialer{}).DialContext(self.ctx, "tcp", self.target)
	if err != nil {
		return
	}
	defer upstream.Close()
	self.stateLock.Lock()
	if self.ctx.Err() != nil {
		self.stateLock.Unlock()
		return
	}
	self.connectionKVs[client], self.connectionKVs[upstream] = true, true
	self.stateLock.Unlock()
	defer func() {
		self.stateLock.Lock()
		delete(self.connectionKVs, client)
		delete(self.connectionKVs, upstream)
		self.stateLock.Unlock()
	}()
	frontendDone := make(chan struct{})
	go func() { defer close(frontendDone); _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
	defer func() { _ = client.Close(); _ = upstream.Close(); <-frontendDone }()
	discarding := false
	for {
		var header [5]byte
		if _, err := io.ReadFull(upstream, header[:]); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length < 4 || length > 16*1024*1024 {
			return
		}
		body := make([]byte, int(length)-4)
		if _, err := io.ReadFull(upstream, body); err != nil {
			return
		}
		if header[0] == 'C' && bytes.Equal(body, []byte("COMMIT\x00")) && self.armed.CompareAndSwap(true, false) {
			self.committed.Add(1)
			discarding = true
			continue
		}
		if discarding {
			if header[0] == 'Z' {
				if bytes.Equal(body, []byte{'I'}) {
					self.idle.Add(1)
				}
				return
			}
			continue
		}
		if _, err := client.Write(header[:]); err != nil {
			return
		}
		if _, err := client.Write(body); err != nil {
			return
		}
	}
}

// This calls the unchanged outer cohort owner and real server.Tx. A committed
// cohort with a missing reply must return unknown/error, never an individual
// fallback or a claimed committed result. All eight finances survive and replay
// cannot reclaim them; durable owners repair the optional lost publications.
func TestLegacyFinancialCohortUnknownCommitNeverFallsBack(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 8)
		resource := server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName)
		proxy := newLegacyCohortCommitReplyProxy(t, ctx, resource.RequireString("authority"))
		defer proxy.close()
		values := maps.Clone(resource.Parse())
		values["authority"] = proxy.listener.Addr().String()
		encoded, err := yaml.Marshal(values)
		server.Raise(err)
		popPg := server.Vault.PushSimpleResource(server.DefaultPgVaultResourceName, encoded)
		server.PgReset()
		defer func() { server.PgReset(); popPg() }()
		before := contractClosedCounter.Snapshot()
		proxy.armed.Store(true)
		attempts, err := flushLegacySettlementCohort(ctx, f.ids)
		after := contractClosedCounter.Snapshot()
		if err == nil || len(attempts) != 0 || proxy.committed.Load() != 1 || proxy.idle.Load() != 1 {
			t.Fatal("missing actual commit reply did not retain unknown ownership", attempts, err, proxy.committed.Load(), proxy.idle.Load())
		}
		if !before.Stable || !after.Stable || after.Confirmed != before.Confirmed || after.Uncertain-before.Uncertain != 8 || after.Untracked != before.Untracked {
			t.Fatal("unknown cohort reply changed acknowledged outcome counting", before, after)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		if replay, err := FlushLegacySettlements(ctx, 1, nil, 8); err != nil || replay.Visited != 0 || replay.Completed != 0 {
			t.Fatal("unknown commit replay repeated financial ownership", replay, err)
		}
		drain, drainErr := legacyFinancialDrainOwners(t, ctx, 9, nil)
		if drainErr != nil || drain.Finished != 9 {
			t.Fatal("unknown commit recovery owner failed", drain, drainErr)
		}
		requireLegacyOwnedMetadataRedis(t, ctx, f.balances[0], 0)
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		final := contractClosedCounter.Snapshot()
		if final.Confirmed != after.Confirmed || final.Uncertain != after.Uncertain || final.Untracked != after.Untracked {
			t.Fatal("optional recovery reclassified original uncertain close replies", after, final)
		}
	})
}
