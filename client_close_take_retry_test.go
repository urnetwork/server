package server_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// This RESP peer owns only synthetic state and net.Pipe sockets. Dropping an
// applied reply tests actual go-redis retransmission, without any Redis service.
type clientCloseTakePeer struct {
	stateLock      sync.Mutex
	workers        sync.WaitGroup
	connections    []net.Conn
	closing        bool
	members        []string
	selectedKey    string
	pops           int
	dropFirstReply bool
	dropped        bool
}

func newClientCloseTakePeer(t *testing.T, members []string, dropFirstReply bool) *clientCloseTakePeer {
	t.Helper()
	peer := &clientCloseTakePeer{members: append([]string(nil), members...), dropFirstReply: dropFirstReply}
	t.Cleanup(func() {
		peer.stateLock.Lock()
		peer.closing = true
		for _, connection := range peer.connections {
			_ = connection.Close()
		}
		peer.stateLock.Unlock()
		done := make(chan struct{})
		go func() { peer.workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("synthetic pipe workers did not retire")
		}
	})
	return peer
}

func (self *clientCloseTakePeer) state() (int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.pops, len(self.members)
}

func (self *clientCloseTakePeer) dial(context.Context, string, string) (net.Conn, error) {
	self.stateLock.Lock()
	if self.closing {
		self.stateLock.Unlock()
		return nil, net.ErrClosed
	}
	peer, client := net.Pipe()
	self.connections = append(self.connections, peer, client)
	self.workers.Add(1)
	self.stateLock.Unlock()
	go func() {
		defer self.workers.Done()
		defer peer.Close()
		reader := bufio.NewReader(peer)
		for {
			args, err := readClientCloseTakeCommand(reader)
			if err != nil {
				return
			}
			reply := "+OK\r\n"
			switch strings.ToLower(args[0]) {
			case "hello":
				reply = "*0\r\n" // same handshake form as existing deadlineRedisPeer
			case "ping":
				reply = "+PONG\r\n"
			case "spop":
				if len(args) != 3 {
					return
				}
				limit, err := strconv.Atoi(args[2])
				if err != nil || limit < 0 || limit > 16 {
					return
				}
				self.stateLock.Lock()
				if self.selectedKey == "" {
					self.selectedKey = args[1]
				}
				if args[1] != self.selectedKey {
					self.stateLock.Unlock()
					return
				}
				self.pops++
				count := min(limit, len(self.members))
				removed := append([]string(nil), self.members[:count]...)
				self.members = self.members[count:]
				drop := self.dropFirstReply && !self.dropped
				self.dropped = self.dropped || drop
				self.stateLock.Unlock()
				if drop {
					// Mutation has committed; only the response is lost.
					return
				}
				reply = fmt.Sprintf("*%d\r\n", len(removed))
				for _, member := range removed {
					reply += fmt.Sprintf("$%d\r\n%s\r\n", len(member), member)
				}
			}
			if _, err := io.WriteString(peer, reply); err != nil {
				return
			}
		}
	}()
	return client, nil
}

func readClientCloseTakeCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "*") {
		return nil, io.ErrUnexpectedEOF
	}
	count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil || count < 1 || count > 32 {
		return nil, io.ErrUnexpectedEOF
	}
	args := make([]string, count)
	for i := range args {
		header, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(header, "$") {
			return nil, io.ErrUnexpectedEOF
		}
		length, err := strconv.Atoi(strings.TrimSpace(header[1:]))
		if err != nil || length < 0 || length > 16384 {
			return nil, io.ErrUnexpectedEOF
		}
		body := make([]byte, length+2)
		if _, err := io.ReadFull(reader, body); err != nil {
			return nil, err
		}
		if string(body[length:]) != "\r\n" {
			return nil, io.ErrUnexpectedEOF
		}
		args[i] = string(body[:length])
	}
	return args, nil
}

// Injects a connection-class failure after a real successful SPOP response.
// go-redis's command processing is already done: only wrapper replay can pop again.
type clientCloseTakeCallbackLoss struct{ failed atomic.Bool }

func (*clientCloseTakeCallbackLoss) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*clientCloseTakeCallbackLoss) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (self *clientCloseTakeCallbackLoss) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		err := next(ctx, command)
		if err == nil && command.Name() == "spop" && self.failed.CompareAndSwap(false, true) {
			err = errors.New("synthetic i/o timeout after applied close-request pop")
			command.SetErr(err)
		}
		return err
	}
}

// A legitimate no-result sentinel must not be reclassified as reply loss.
type clientCloseTakeNilReply struct{}

func (*clientCloseTakeNilReply) DialHook(next redis.DialHook) redis.DialHook { return next }
func (*clientCloseTakeNilReply) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (*clientCloseTakeNilReply) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		err := next(ctx, command)
		if err == nil && command.Name() == "spop" {
			command.SetErr(redis.Nil)
			return redis.Nil
		}
		return err
	}
}

func clientCloseTakePanic(ctx context.Context, clientId server.Id, limit int) (value *model.ClientCloseRequests, failure any) {
	defer func() { failure = recover() }()
	value = model.TakeClientCloseRequests(ctx, clientId, limit)
	return
}

func TestClientCloseTakeCommandReplyLossDoesNotReplay(t *testing.T) {
	contractIds := []server.Id{server.NewId(), server.NewId()}
	peer := newClientCloseTakePeer(t, []string{"c:" + contractIds[0].String(), "c:" + contractIds[1].String()}, true)
	server.TestingClientCloseTakeRedisPools(t, peer.dial, nil, func() {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		requests, failure := clientCloseTakePanic(ctx, server.NewId(), 1)
		pops, remaining := peer.state()
		if failure == nil || requests != nil || pops != 1 || remaining != 1 {
			t.Fatalf("ambiguous command take: failed=%t result=%t physical_pops=%d remaining=%d; want true,false,1,1", failure != nil, requests != nil, pops, remaining)
		}
	})
}

func TestClientCloseTakeCallbackReplyLossDoesNotReplay(t *testing.T) {
	peer := newClientCloseTakePeer(t, []string{"c:" + server.NewId().String(), "c:" + server.NewId().String()}, false)
	hook := &clientCloseTakeCallbackLoss{}
	server.TestingClientCloseTakeRedisPools(t, peer.dial, hook, func() {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		requests, failure := clientCloseTakePanic(ctx, server.NewId(), 1)
		pops, remaining := peer.state()
		if !hook.failed.Load() || failure == nil || requests != nil || pops != 1 || remaining != 1 {
			t.Fatalf("ambiguous callback take: injected=%t failed=%t result=%t physical_pops=%d remaining=%d; want true,true,false,1,1", hook.failed.Load(), failure != nil, requests != nil, pops, remaining)
		}
	})
}

func TestClientCloseTakeHealthyAndEmptyOwnedPeer(t *testing.T) {
	contractId, peerId := server.NewId(), server.NewId()
	clientId := server.NewId()
	peer := newClientCloseTakePeer(t, []string{"c:" + contractId.String(), "p:" + peerId.String()}, false)
	server.TestingClientCloseTakeRedisPools(t, peer.dial, nil, func() {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		requests := model.TakeClientCloseRequests(ctx, clientId, 2)
		if len(requests.ContractIds) != 1 || requests.ContractIds[0] != contractId || len(requests.PeerIds) != 1 || requests.PeerIds[0] != peerId {
			t.Fatal("valid mixed requests were not classified")
		}
		if empty := model.TakeClientCloseRequests(ctx, clientId, 2); empty == nil || !empty.Empty() {
			t.Fatal("complete empty request set was not a legitimate empty result")
		}
		if pops, remaining := peer.state(); pops != 2 || remaining != 0 {
			t.Fatalf("healthy physical_pops=%d remaining=%d; want2,0", pops, remaining)
		}
	})
	nilPeer := newClientCloseTakePeer(t, nil, false)
	server.TestingClientCloseTakeRedisPools(t, nilPeer.dial, &clientCloseTakeNilReply{}, func() {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if empty := model.TakeClientCloseRequests(ctx, server.NewId(), 1); empty == nil || !empty.Empty() {
			t.Fatal("Redis nil result was not a legitimate empty take")
		}
		if pops, remaining := nilPeer.state(); pops != 1 || remaining != 0 {
			t.Fatalf("nil-result physical_pops=%d remaining=%d; want1,0", pops, remaining)
		}
	})
}

func TestClientCloseTakeCanceledOwnerDoesNotConsume(t *testing.T) {
	peer := newClientCloseTakePeer(t, []string{"c:" + server.NewId().String()}, false)
	server.TestingClientCloseTakeRedisPools(t, peer.dial, nil, func() {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		requests, failure := clientCloseTakePanic(ctx, server.NewId(), 1)
		if failure == nil || requests != nil {
			t.Fatal("canceled take did not preserve unavailable result")
		}
		if pops, remaining := peer.state(); pops != 0 || remaining != 1 {
			t.Fatalf("canceled physical_pops=%d remaining=%d; want0,1", pops, remaining)
		}
	})
}
