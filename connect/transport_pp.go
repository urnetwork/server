


import (
	"heap"

	"github.com/mailgun/proxyproto"
)



type ProxyProtocolSettings struct {
	ProxyTimeout time.Duration
}


// implements PacketConn
type ProxyProtocolPacketConn struct {
	

	conn net.PacketConn

	settings *ProxyProtocolSettings


	readBuffer []byte

	// state lock
	// proxy address to state
	proxyStates map[net.Addr]*proxyState
	// real addr to proxy addr
	proxyAddrs[net.Addr]net.Addr
	q *proxyStateQueue
}


func ReadFrom(p []byte) (n int, addr Addr, err error) {
	buffer := self.readBuffer

	n, addr, err = conn.ReadFrom(buffer)

	// the packet may contain a proxy protocol header at any time
	// if the last packet from addr was > proxy timeout,
	// we can trust that the header is from our proxy and the protocol header is used
	// otherwise the header is discarded because we can't tell if header is from our proxy or the user
	h, header, err := parsePpHeader(buffer[0:n])

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		now := time.Now()
		expireTime := now.Add(-self.ProxyTimeout)
		for 0 < q.Len() && q.Peek().lastUpdatedTime.Before(expireTime) {
			s := q.Pop()
			delete(self.proxyState, s.addr)
		}

		if s, ok := self.proxyState[addr]; !ok {
			if h == 0 {
				err = fmt.Errorf("proxy protocol header required but not found")
				return
			}
			s = &proxyState{
				addr: addr,
				realAddr: header.Source,
				lastUpdateTime: now,
			}
			self.proxyState[addr] = s
			heap.Add(q, s)

			buffer = buffer[h:n]
			n -= h
		} else {
			s.lastUpdateTime = now
			heap.Fix(q, s.index)

			// *important* the header can be either from our proxy or the user
			//             do not use or store the header value. Just discard it.
			if 0 < h {
				buffer = buffer[h:n]
				n -= h
			}
		}

		addr = s.readAddr
	}()

	if err != nil {
		return
	}

	copy(p[0:n], buffer[0:n])
	return
}



func WriteTo(p []byte, addr Addr) (n int, err error) {

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		now := time.Now()
		expireTime := now.Add(-self.ProxyTimeout)
		for 0 < q.Len() && q.Peek().lastUpdatedTime.Before(expireTime) {
			s := q.Pop()
			delete(self.proxyState, s.addr)
		}

		if proxyAddr, ok := proxyAddrs[addr]; ok {
			s := self.proxyStates[proxyAddr]
			s.lastUpdateTime = now
			heap.Fix(q, s.index)

			addr = proxyAddr
		} else {
			err = fmt.Errorf("proxy protocol state not found")
		}
	}()
	if err != nil {
		return 
	}

	n, err = conn.WriteTo(p, addr)
	return
}

func LocalAddr() Addr {
	return self.conn.LocalAddr()
}

func SetDeadline(t time.Time) error {
	return self.conn.SetDeadline(t)
}

func SetReadDeadline(t time.Time) error {
	return self.conn.SetReadDeadline(t)
}

func SetWriteDeadline(t time.Time) error {
	return self.conn.SetWriteDeadline(t)
}

func Close() error {
	
}




struct proxyState {
	proxyAddr net.Addr
	realAddr net.Addr
	lastUpdateTime time.Time
}



// https://www.haproxy.org/download/1.8/doc/proxy-protocol.txt
const ppSignature = [12]byte{
	0x0D,
	0x0A,
	0x0D,
	0x0A,
	0x00,
	0x0D,
	0x0A,
	0x51,
	0x55,
	0x49,
	0x54,
	0x0A,
}

func parsePpHeader(b []byte) (h int, header *proxyproto.Header, err error) {
	if len(b) < 12 {
		return 0, nil, nil
	}
	if ppSignature != (*[12]byte)(b) {
		return 0, nil, nil
	}
	r := bytes.NewReader(b)
	header, err = proxyproto.ReadHeader(conn)
	h = len(b) - r.Len()
	return
}

