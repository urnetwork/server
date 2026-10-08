//go:build linux

package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shadowRPCTestIdentity() ArinShadowRPCIdentity {
	return ArinShadowRPCIdentity{ProcessNonce: NewId(), StartedAt: NowUtc(), Revision: strings.Repeat("a", 40), Role: "connect"}
}

func shadowRPCTestSocket(t *testing.T, service *ArinShadowRPCService) ArinShadowRPCRoundTrip {
	t.Helper()
	dir, err := os.MkdirTemp("", "arin-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			serveArinShadowConnection(ctx, service, conn)
		}
	}()
	t.Cleanup(func() { cancel(); listener.Close(); <-done; os.Remove(path); os.Remove(dir) })
	return ArinShadowUnixRoundTrip(path)
}

func TestArinRPCUnixCaptureBindsSourcePinsAndNeverTransfersAddress(t *testing.T) {
	r, facts := shadowFixture(t, false)
	id, clientId, handlerId := NewId(), NewId(), NewId()
	owner := &shadowCaptureTestOwner{available: true, snapshot: ArinShadowOwnerSnapshot{ConnectionId: id, ClientId: clientId, HandlerId: handlerId, Address: netip.MustParseAddr("192.0.2.1"), At: NowUtc()}}
	key := [32]byte{1, 2, 3}
	run := NewId()
	identity := shadowRPCTestIdentity()
	service, err := NewArinShadowRPCService(context.Background(), run, key, identity, func(ctx context.Context, method string, input json.RawMessage) (any, error) {
		return r.CaptureRPC(ctx, input, func(context.Context, []Id) ([]ArinShadowCaptureTarget, error) {
			return []ArinShadowCaptureTarget{{id, owner}}, nil
		}, func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
			return []ArinShadowCaptureFacts{{ConnectionId: id, ClientId: clientId, HandlerId: handlerId, ObservedAt: NowUtc(), Connected: true, Present: true, Actual: facts}}, nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	transport := shadowRPCTestSocket(t, service)
	privateFree := func(ctx context.Context, request []byte) ([]byte, error) {
		reply, err := transport(ctx, request)
		if bytes.Contains(request, []byte("192.0.2.")) || bytes.Contains(reply, []byte("192.0.2.")) {
			t.Error("exact address crossed IPC")
		}
		return reply, err
	}
	client, err := NewArinShadowRPCClient(run, key, identity, privateFree)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := r.CallCapture(context.Background(), client, []Id{id})
	if err != nil || len(batch.rows) != 1 || batch.rows[0].reason != "qualified" || !batch.rows[0].facts.verified {
		t.Fatal("healthy IPC capture did not qualify", err)
	}
	wrong := identity
	wrong.ProcessNonce = NewId()
	other, _ := NewArinShadowRPCClient(run, key, wrong, transport)
	if _, err = r.CallCapture(context.Background(), other, []Id{id}); err == nil {
		t.Fatal("replacement process accepted")
	}
	owner.mu.Lock()
	owner.available = false
	owner.mu.Unlock()
	batch, err = r.CallCapture(context.Background(), client, []Id{id})
	if err != nil || batch.rows[0].reason != "owner_unavailable" {
		t.Fatal("closed transport did not remain unknown")
	}
}

func TestArinRPCAuthenticationReplayBoundsPrecedeOwningWork(t *testing.T) {
	var calls atomic.Int64
	key := [32]byte{9}
	run := NewId()
	identity := shadowRPCTestIdentity()
	service, _ := NewArinShadowRPCService(context.Background(), run, key, identity, func(context.Context, string, json.RawMessage) (any, error) { calls.Add(1); return struct{}{}, nil })
	request := ArinShadowRPCRequest{RunId: run, Nonce: NewId(), ProcessNonce: identity.ProcessNonce, Deadline: NowUtc().Add(time.Second), Method: "capture", Input: json.RawMessage(`{}`)}
	data, _ := json.Marshal(request)
	packet := arinShadowSign(key, data)
	for _, bad := range [][]byte{arinShadowSign([32]byte{8}, data), packet[:31], make([]byte, ArinShadowRPCRequestLimit+1)} {
		if _, err := service.Handle(context.Background(), bad); err == nil {
			t.Fatal("unauthenticated/oversized request accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("bad request reached owning work")
	}
	if _, err := service.Handle(context.Background(), packet); err != nil {
		t.Fatal("healthy request refused")
	}
	if _, err := service.Handle(context.Background(), packet); err == nil || calls.Load() != 1 {
		t.Fatal("nonce replay executed again")
	}
	request.Nonce = NewId()
	request.Deadline = NowUtc().Add(-time.Second)
	data, _ = json.Marshal(request)
	if _, err := service.Handle(context.Background(), arinShadowSign(key, data)); err == nil || calls.Load() != 1 {
		t.Fatal("expired request executed")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], ArinShadowRPCResponseLimit+1)
	if _, err := ReadArinShadowRPCFrame(bytes.NewReader(header[:]), ArinShadowRPCResponseLimit); err == nil {
		t.Fatal("unbounded frame allocation")
	}
}

func TestArinRPCUnixCancellationJoinsStalledOwner(t *testing.T) {
	key := [32]byte{9}
	run := NewId()
	identity := shadowRPCTestIdentity()
	joined := make(chan struct{})
	service, _ := NewArinShadowRPCService(context.Background(), run, key, identity, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		defer close(joined)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client, _ := NewArinShadowRPCClient(run, key, identity, shadowRPCTestSocket(t, service))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	var reply struct{}
	if client.Call(ctx, "capture", struct{}{}, &reply) == nil {
		t.Fatal("stalled capture succeeded")
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("canceled owning callback escaped")
	}
}

func TestArinRPCSealedReplyRejectsTamperAndWrongResource(t *testing.T) {
	r, _ := shadowFixture(t, false)
	key := [32]byte{9}
	run := NewId()
	identity := shadowRPCTestIdentity()
	service, _ := NewArinShadowRPCService(context.Background(), run, key, identity, func(context.Context, string, json.RawMessage) (any, error) { return struct{}{}, nil })
	client, _ := NewArinShadowRPCClient(run, key, identity, func(ctx context.Context, request []byte) ([]byte, error) {
		reply, err := service.Handle(ctx, request)
		if len(reply) > 32 {
			reply[len(reply)-1] ^= 1
		}
		return reply, err
	})
	var reply struct{}
	if client.Call(context.Background(), "capture", struct{}{}, &reply) == nil {
		t.Fatal("tampered reply accepted")
	}
	pins, _ := r.ResourcePins()
	pins.CandidateSHA256 = strings.Repeat("0", 64)
	encoded, _ := json.Marshal(arinShadowCaptureRPCRequest{pins, []Id{NewId()}})
	if _, err := r.CaptureRPC(context.Background(), encoded, nil, nil); err == nil {
		t.Fatal("wrong pinned artifact admitted")
	}
}
