// Pause the real native debit owner's granted row without executing a blocked
// SQL statement. Its production context and all SQL budgets remain unchanged.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"maps"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

type nativeDebitOwnerBarrier struct {
	balanceId server.Id
	ctx       context.Context
	cancel    context.CancelFunc
	listener  net.Listener
	target    string
	armed     atomic.Bool
	released  atomic.Bool
	commits   atomic.Int64
	rollbacks atomic.Int64
	lost      atomic.Int64
	held      chan int32
	release   chan struct{}
	once      sync.Once
	closeOnce sync.Once
	accepted  chan struct{}
	joined    sync.WaitGroup
	lock      sync.Mutex
	conns     map[net.Conn]bool
	ownerEnd  func()
}

// This observer covers the ordinary PostgreSQL route used by server.Tx in the
// native debit applier. It does not claim complete traffic/error observations.
func newNativeDebitOwnerBarrier(t testing.TB, ctx context.Context, balanceId server.Id) (*nativeDebitOwnerBarrier, func()) {
	return newNativeDebitOwnerBarrierOnResource(t, ctx, balanceId, server.DefaultPgVaultResourceName, nil)
}

// Route selection is explicit. The common owner uses the maintenance checkout;
// the preserved ordinary wrapper remains available for its qualified predecessor.
// ownerEnd observes the actual held backend's terminal reply or transport loss,
// not the later ordering of advisory-release observation callbacks.
func newNativeDebitOwnerBarrierOnResource(t testing.TB, ctx context.Context, balanceId server.Id,
	resourceName string, ownerEnd func()) (*nativeDebitOwnerBarrier, func()) {
	t.Helper()
	if resourceName != server.DefaultPgVaultResourceName && resourceName != server.MaintenancePgVaultResourceName {
		t.Fatal("native debit barrier requires an explicit known PostgreSQL route")
	}
	resource := server.Vault.RequireSimpleResource(resourceName)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("native debit owner barrier listen failed", err)
	}
	owned, cancel := context.WithCancel(ctx)
	barrier := &nativeDebitOwnerBarrier{balanceId: balanceId, ctx: owned, cancel: cancel,
		listener: listener, target: resource.RequireString("authority"), held: make(chan int32, 1), release: make(chan struct{}),
		accepted: make(chan struct{}), conns: map[net.Conn]bool{}, ownerEnd: ownerEnd}
	barrier.armed.Store(true)
	go func() {
		defer close(barrier.accepted)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			barrier.joined.Add(1)
			go func() { defer barrier.joined.Done(); barrier.forward(conn) }()
		}
	}()
	values := maps.Clone(resource.Parse())
	values["authority"] = listener.Addr().String()
	encoded, err := yaml.Marshal(values)
	server.Raise(err)
	pop := server.Vault.PushSimpleResource(resourceName, encoded)
	server.PgReset()
	return barrier, func() {
		barrier.closeOnce.Do(func() {
			barrier.Release()
			barrier.cancel()
			_ = listener.Close()
			<-barrier.accepted
			barrier.lock.Lock()
			conns := make([]net.Conn, 0, len(barrier.conns))
			for conn := range barrier.conns {
				conns = append(conns, conn)
			}
			barrier.lock.Unlock()
			for _, conn := range conns {
				_ = conn.Close()
			}
			barrier.joined.Wait()
			server.PgReset()
			pop()
		})
	}
}

func (self *nativeDebitOwnerBarrier) Release() {
	self.once.Do(func() { self.released.Store(true); close(self.release) })
}

func nativeDebitOwnerBalanceColumn(raw []byte) bool {
	if len(raw) < 2 || binary.BigEndian.Uint16(raw) != 1 {
		return false
	}
	end := bytes.IndexByte(raw[2:], 0)
	if end < 0 || string(raw[2:2+end]) != "balance_id" {
		return false
	}
	fields := raw[3+end:]
	return len(fields) == 18 && binary.BigEndian.Uint32(fields[6:10]) == 2950
}

func (self *nativeDebitOwnerBarrier) balanceRow(raw []byte) bool {
	if len(raw) < 6 || binary.BigEndian.Uint16(raw) != 1 {
		return false
	}
	length := int32(binary.BigEndian.Uint32(raw[2:6]))
	if length < 0 || int64(length) != int64(len(raw)-6) {
		return false
	}
	return bytes.Equal(raw[6:], self.balanceId[:]) || bytes.Equal(raw[6:], []byte(self.balanceId.String()))
}

func (self *nativeDebitOwnerBarrier) forward(client net.Conn) {
	defer client.Close()
	upstream, err := (&net.Dialer{}).DialContext(self.ctx, "tcp", self.target)
	if err != nil {
		return
	}
	defer upstream.Close()
	self.lock.Lock()
	if self.ctx.Err() != nil {
		self.lock.Unlock()
		return
	}
	self.conns[client], self.conns[upstream] = true, true
	self.lock.Unlock()
	defer func() { self.lock.Lock(); delete(self.conns, client); delete(self.conns, upstream); self.lock.Unlock() }()
	frontendDone := make(chan struct{})
	go func() { defer close(frontendDone); _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
	defer func() { _ = client.Close(); _ = upstream.Close(); <-frontendDone }()
	var pid int32
	column, row, command, owner, ended := false, false, false, false, false
	defer func() {
		if owner && !ended {
			self.lost.Add(1)
			if self.ownerEnd != nil {
				self.ownerEnd()
			}
		}
	}()
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
		switch header[0] {
		case 'K':
			if len(body) == 8 {
				pid = int32(binary.BigEndian.Uint32(body))
			}
		case 'T':
			column = nativeDebitOwnerBalanceColumn(body)
		case 'D':
			row = row || (column && self.balanceRow(body))
		case 'C':
			command = command || (row && bytes.Equal(body, []byte("SELECT 1\x00")))
			if owner && !ended {
				if bytes.Equal(body, []byte("COMMIT\x00")) || bytes.Equal(body, []byte("ROLLBACK\x00")) {
					ended = true
					if self.ownerEnd != nil {
						self.ownerEnd()
					}
					if !self.released.Load() {
						self.lost.Add(1)
					}
					if bytes.Equal(body, []byte("COMMIT\x00")) {
						self.commits.Add(1)
					} else {
						self.rollbacks.Add(1)
					}
				}
			}
		case 'E':
			column, row, command = false, false, false
		case 'Z':
			if column && row && command && pid > 0 && len(body) == 1 && body[0] == 'T' && self.armed.CompareAndSwap(true, false) {
				owner = true
				self.held <- pid
				select {
				case <-self.release:
				case <-frontendDone:
					return
				case <-self.ctx.Done():
					return
				}
			}
			// pgx5.10 ExecStatement reuses the prepared field description
			// after its prepare Sync; it need not send Describe Portal again.
			// The required pg_stat_activity query witness rejects any
			// unrelated cached statement before the caller accepts ownership.
			row, command = false, false
		}
		if _, err := client.Write(header[:]); err != nil {
			return
		}
		if _, err := client.Write(body); err != nil {
			return
		}
	}
}

// Call after receiving held and again immediately before releasing it. Exact
// backend state/query, the returned hot row, and Ready(T) jointly prove the
// native applier's real granted lock. A selector/unknown SQL is not accepted.
func requireNativeDebitOwnerHeld(t testing.TB, ctx context.Context, pid int32) {
	t.Helper()
	var state, query string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT state,query FROM pg_stat_activity WHERE pid=$1 AND datname=current_database()`, pid).Scan(&state, &query))
	})
	expected := "SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR NO KEY UPDATE SKIP LOCKED"
	if state != "idle in transaction" || strings.Join(strings.Fields(query), " ") != expected {
		t.Fatal("native debit barrier lost exact granted owner", state)
	}
}

func TestNativeDebitOwnerBarrierFrames(t *testing.T) {
	column := []byte{0, 1}
	column = append(column, []byte("balance_id\x00")...)
	fields := make([]byte, 18)
	binary.BigEndian.PutUint32(fields[6:10], 2950)
	column = append(column, fields...)
	if !nativeDebitOwnerBalanceColumn(column) {
		t.Fatal("real one-column UUID description was refused")
	}
	wrongName := bytes.ReplaceAll(column, []byte("balance_id"), []byte("contract_id"))
	wrongType := bytes.Clone(column)
	binary.BigEndian.PutUint32(wrongType[len(wrongType)-12:], 25)
	for _, malformed := range [][]byte{nil, {0, 0}, column[:len(column)-1], wrongName, wrongType} {
		if nativeDebitOwnerBalanceColumn(malformed) {
			t.Fatal("unrelated or malformed description armed owner barrier")
		}
	}
	barrier := &nativeDebitOwnerBarrier{balanceId: server.NewId()}
	for _, value := range [][]byte{barrier.balanceId[:], []byte(barrier.balanceId.String())} {
		row := []byte{0, 1}
		row = binary.BigEndian.AppendUint32(row, uint32(len(value)))
		row = append(row, value...)
		if !barrier.balanceRow(row) {
			t.Fatal("actual matching UUID row was refused")
		}
		row[len(row)-1] ^= 1
		if barrier.balanceRow(row) {
			t.Fatal("different UUID row armed owner barrier")
		}
	}
	for _, raw := range [][]byte{nil, {0, 0}, {0, 1, 255, 255, 255, 255}, {0, 1, 0, 0, 0, 16, 1}} {
		if barrier.balanceRow(raw) {
			t.Fatal("empty/null/incomplete lock result armed owner barrier")
		}
	}
}
