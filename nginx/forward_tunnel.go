package nginx

// Websocket tunnels (owner: s-forward): nginx's upgraded mode
// (ngx_http_upstream_upgrade, ngx_http_upstream_process_upgraded). When a
// block answers 101 to an HTTP/1.1 request that asked for an upgrade, the
// front hijacks the client connection, relays the 101, and copies bytes both
// ways until either side ends, as nginx finalizes a tunnel when either side
// reaches its end with nothing left to send.
//
// Bounds, as nginx's timers:
//   - idle: ForwardSettings.StreamTimeout with no bytes either way. nginx
//     re-arms its upstream read timer on every tunnel event, either way, and
//     keeps none on the client's reads, so only silence both ways ends it;
//     here every read, either way, extends both read deadlines;
//   - a write to the block that stalls past StreamTimeout (proxy_send_timeout
//     on websocket routes);
//   - a write to the client that stalls past HttpSettings.SendTimeout
//     (send_timeout).
//
// The tunnel holds its block as in flight, as nginx holds the peer until
// the request ends, and serveHttp returns only when it closes, so the
// caller's concurrency slot spans it (D8). The group tracks every tunnel, so
// close cuts them (D12).

import (
	"bufio"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"
)

// One tunnel's two connections.
type forwardTunnel struct {
	clientConn   net.Conn
	upstreamConn *upstreamConnection
	closeOnce    sync.Once
}

// Closes both sides, which ends both copies. Safe to call more than once and
// concurrently.
func (self *forwardTunnel) cut() {
	self.closeOnce.Do(func() {
		self.clientConn.Close()
		self.upstreamConn.close()
	})
}

// Runs a tunnel for a 101 response, until it closes.
func (self *upstreamGroup) tunnel(w http.ResponseWriter, responseController *http.ResponseController, block *upstreamBlock, exchange *upstreamExchange, options *forwardOptions) {
	defer self.releaseBlock(block, options)
	defer exchange.finish(false)

	if err := exchange.joinWrite(); err != nil {
		// the block switched before it had the whole request
		self.respondError(w, http.StatusBadGateway)
		return
	}
	clientConn, clientReadWriter, err := responseController.Hijack()
	if err != nil {
		// not an HTTP/1.1 connection: nothing to tunnel through
		self.respondError(w, http.StatusBadGateway)
		return
	}
	tunnel := &forwardTunnel{
		clientConn:   clientConn,
		upstreamConn: exchange.conn,
	}
	defer tunnel.cut()
	open := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.tunnels[tunnel] = true
		return true
	}()
	if !open {
		return
	}
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.tunnels, tunnel)
	}()
	forwardTunnelsGauge.WithLabelValues(self.service.Name).Inc()
	defer forwardTunnelsGauge.WithLabelValues(self.service.Name).Dec()

	// the server's deadlines belong to the request it served; the tunnel
	// keeps its own, and the upstream connection's per-operation ones are
	// replaced by the tunnel's
	clientConn.SetDeadline(time.Time{})
	upstreamConn := exchange.conn
	upstreamConn.readTimeout = 0
	upstreamConn.writeTimeout = 0
	upstreamConn.conn.SetDeadline(time.Time{})

	streamTimeout := self.settings.Forward.StreamTimeout
	sendTimeout := self.settings.Http.SendTimeout
	extendIdle := func() {
		if 0 < streamTimeout {
			idleDeadline := time.Now().Add(streamTimeout)
			clientConn.SetReadDeadline(idleDeadline)
			upstreamConn.conn.SetReadDeadline(idleDeadline)
		}
	}
	extendIdle()

	if sendTimeout > 0 {
		clientConn.SetWriteDeadline(time.Now().Add(sendTimeout))
	}
	clientReadWriter.Writer.Write(forwardSwitchingProtocolsHead(w.Header(), exchange.response))
	if err := clientReadWriter.Writer.Flush(); err != nil {
		return
	}
	if options.tunnelOpened != nil {
		options.tunnelOpened()
	}

	// bytes either side sent ahead of the switch are in its reader
	pipe := func(dst net.Conn, src *bufio.Reader, writeTimeout time.Duration) {
		// the first side to end ends the tunnel
		defer tunnel.cut()
		copyBuffer := self.copyBuffers.Get().(*[]byte)
		defer self.copyBuffers.Put(copyBuffer)
		for {
			n, readErr := src.Read(*copyBuffer)
			if 0 < n {
				extendIdle()
				if 0 < writeTimeout {
					dst.SetWriteDeadline(time.Now().Add(writeTimeout))
				}
				if _, err := dst.Write((*copyBuffer)[:n]); err != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}
	var pipes sync.WaitGroup
	pipes.Go(func() {
		pipe(upstreamConn.conn, clientReadWriter.Reader, streamTimeout)
	})
	pipe(clientConn, upstreamConn.reader, sendTimeout)
	pipes.Wait()
}

// The 101 head for the client: the headers the front set, the block's
// except the hidden ones, its Upgrade, and Connection: upgrade, as nginx's
// header filter writes for a 101.
func forwardSwitchingProtocolsHead(frontHeader http.Header, response *http.Response) []byte {
	head := []byte("HTTP/1.1 101 Switching Protocols\r\n")
	header := frontHeader.Clone()
	if header == nil {
		header = http.Header{}
	}
	forwardResponseHeaders(header, response)
	header.Del(contentLengthHeader)
	header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	header.Set(connectionHeader, "upgrade")
	if upgrades := response.Header.Values(upgradeHeader); 0 < len(upgrades) {
		header[upgradeHeader] = slices.Clone(upgrades)
	}
	headerNames := make([]string, 0, len(header))
	for name := range header {
		headerNames = append(headerNames, name)
	}
	slices.Sort(headerNames)
	for _, name := range headerNames {
		for _, value := range header[name] {
			if !forwardHeaderValueValid(value) {
				continue
			}
			head = append(head, name...)
			head = append(head, ": "...)
			head = append(head, value...)
			head = append(head, "\r\n"...)
		}
	}
	return append(head, "\r\n"...)
}
