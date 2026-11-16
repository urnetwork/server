// Pins the actual configuration reader and Redis pool selection beneath HTTP
// admission. The wire peer holds a real GET reply; it never inspects context.
package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Uses native go-redis sockets through net.Pipe, as the server's Redis deadline
// fixtures do. Only dial transport is replaced; command deadlines are real.
type httpBudgetRedisPeer struct {
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
	closeOnce sync.Once
	workers   sync.WaitGroup
}

// Supplies the synthetic peer before either production pool opens a socket.
func (self *httpBudgetRedisPeer) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		client, peer := net.Pipe()
		self.workers.Add(1)
		go func() {
			defer self.workers.Done()
			defer peer.Close()
			reader := bufio.NewReader(peer)
			for {
				line, err := reader.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "*") {
					return
				}
				count, err := strconv.Atoi(strings.TrimSpace(line[1:]))
				if err != nil || count < 1 || count > 32 {
					return
				}
				args := make([]string, count)
				for index := range args {
					header, err := reader.ReadString('\n')
					if err != nil || !strings.HasPrefix(header, "$") {
						return
					}
					length, err := strconv.Atoi(strings.TrimSpace(header[1:]))
					if err != nil || length < 0 || length > 16384 {
						return
					}
					value := make([]byte, length+2)
					if _, err := io.ReadFull(reader, value); err != nil {
						return
					}
					args[index] = string(value[:length])
				}
				reply := "+OK\r\n"
				switch strings.ToLower(args[0]) {
				case "hello":
					reply = "%0\r\n"
				case "ping":
					reply = "+PONG\r\n"
				case "get":
					self.enterOnce.Do(func() { close(self.entered) })
					<-self.release
					return
				}
				if _, err := io.WriteString(peer, reply); err != nil {
					return
				}
			}
		}()
		return client, nil
	}
}

// Leaves command execution and its socket policy entirely in go-redis.
func (self *httpBudgetRedisPeer) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

// Leaves handshake and command pipelines unchanged too.
func (self *httpBudgetRedisPeer) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// Releases the held reply only during test cleanup, then joins its peer.
func (self *httpBudgetRedisPeer) close() {
	self.closeOnce.Do(func() { close(self.release) })
	self.workers.Wait()
}

// Passing a deadline to ordinary Redis was insufficient: its sockets ignored
// that deadline. The real reader must choose the dedicated deadline pool.
func TestHttpConnectBudgetIncludesRedisSocketRead(t *testing.T) {
	configureHttpBudgetTest(t)
	t.Cleanup(server.Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 192.0.2.1:6379\npassword: synthetic\ndb: 0\n")))
	t.Cleanup(server.Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 8\n")))
	synctest.Test(t, func(t *testing.T) {
		server.RedisReset()
		peer := &httpBudgetRedisPeer{entered: make(chan struct{}), release: make(chan struct{})}
		defer peer.close()
		defer server.RedisReset()
		server.Redis(t.Context(), func(client server.RedisClient) { client.AddHook(peer) })
		installCtx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if err := server.RedisWithDeadline(installCtx, func(client server.RedisClient) error {
			client.AddHook(peer)
			return nil
		}, server.OptRedisNoPreflight()); err != nil {
			t.Fatal(err)
		}
		manager := newHttpConnectTestManager(t)
		core := newHttpConnectTestProxy(manager)
		core.Settings().ProxyConnectBudget = time.Second
		responseDone, handlerDone := startHttpBudgetTestConnect(t, core, server.NewId())
		<-peer.entered
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case code := <-responseDone:
			if code != http.StatusGatewayTimeout {
				t.Errorf("held Redis response returned %d, want 504", code)
			}
		default:
			t.Error("production Redis reader outlived the CONNECT budget")
		}
		peer.close()
		<-handlerDone
	})
}
