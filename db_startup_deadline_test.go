package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A real local TCP peer accepts the PostgreSQL startup and never answers it.
// Caller cancellation must join independently; the detached native pgx
// constructor must then close its socket and retire without Pool.Close.
func TestPgStartupCanceledAcquireRetiresNativeConstructor(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	started := make(chan struct{})
	peerDone := make(chan error, 1)
	var peer net.Conn
	var peerLock sync.Mutex
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			peerDone <- err
			return
		}
		peerLock.Lock()
		peer = conn
		peerLock.Unlock()
		defer conn.Close()
		backend := pgproto3.NewBackend(conn, conn)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			peerDone <- err
			return
		}
		close(started)
		var one [1]byte
		_, err = conn.Read(one[:])
		peerDone <- err
	}()
	defer func() {
		peerLock.Lock()
		if peer != nil {
			_ = peer.Close()
		}
		peerLock.Unlock()
	}()
	config, err := pgxpool.ParseConfig(fmt.Sprintf("postgres://synthetic@%s/synthetic?sslmode=disable&connect_timeout=30", listener.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	config.MinConns, config.MaxConns = 0, 1
	configurePgPoolLiveness(config)
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	begin := time.Now()
	go func() {
		conn, err := pool.Acquire(caller)
		if conn != nil {
			conn.Release()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("startup did not reach the real TCP barrier")
	}
	if pool.Stat().ConstructingConns() != 1 {
		t.Fatal("startup did not own exactly one native constructor")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire returned %v; want caller cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled Acquire did not join")
	}
	if pool.Stat().ConstructingConns() != 1 {
		t.Fatal("test did not retain a detached constructor after caller exit")
	}
	deadline := time.NewTimer(7*time.Second - time.Since(begin))
	defer deadline.Stop()
	select {
	case err := <-peerDone:
		if err == nil {
			t.Fatal("unexpected startup reply")
		}
	case <-deadline.C:
		t.Fatal("detached startup socket outlived the native establishment bound")
	}
	for pool.Stat().TotalConns() != 0 {
		select {
		case <-deadline.C:
			t.Fatal("constructor did not retire after its startup socket closed")
		case <-time.After(time.Millisecond):
		}
	}
	if pool.Stat().AcquiredConns() != 0 || pool.Stat().IdleConns() != 0 {
		t.Fatal("failed startup manufactured a usable connection")
	}
}

// Name resolution runs before pgconn starts its native connection timeout.
// A real pool and its detached constructor exercise the configured wrapper.
func TestPgStartupCanceledAcquireRetiresBlockedLookup(t *testing.T) {
	config, err := pgxpool.ParseConfig("host=synthetic.invalid user=synthetic dbname=synthetic sslmode=disable connect_timeout=30")
	if err != nil {
		t.Fatal(err)
	}
	config.MinConns, config.MaxConns = 0, 1
	started, lookupDone := make(chan struct{}), make(chan struct{})
	config.ConnConfig.LookupFunc = func(ctx context.Context, _ string) ([]string, error) {
		close(started)
		<-ctx.Done()
		close(lookupDone)
		return nil, ctx.Err()
	}
	configurePgPoolLiveness(config)
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := pool.Acquire(ctx)
		if conn != nil {
			conn.Release()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("lookup did not enter native constructor")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Acquire returned %v; want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire did not join while constructor was resolving")
	}
	select {
	case <-lookupDone:
	case <-time.After(6 * time.Second):
		t.Fatal("detached lookup outlived its establishment bound")
	}
	deadline := time.Now().Add(time.Second)
	for pool.Stat().TotalConns() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("failed lookup constructor did not retire")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPgStartupKeepsShorterConfiguredDeadline(t *testing.T) {
	for _, configured := range []time.Duration{0, time.Second, 30 * time.Second} {
		config, err := pgxpool.ParseConfig("host=synthetic.invalid user=synthetic dbname=synthetic sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		config.ConnConfig.ConnectTimeout = configured
		configurePgPoolLiveness(config)
		want := 5 * time.Second
		if 0 < configured && configured < want {
			want = configured
		}
		if config.ConnConfig.ConnectTimeout != want {
			t.Fatalf("startup deadline=%v, want %v", config.ConnConfig.ConnectTimeout, want)
		}
	}
}
