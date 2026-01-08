package main

// http proxy and socks proxy connections.
//
// run the http proxy on the connect service and form the proxy connection via an ExchangeConnection to the resident
//

// run the socks proxy on the connect service and form the proxy conenction via an ExchangeConnection to the resident

type ProxyConnectHandlerSettings struct {
	WriteTimeout               time.Duration
	ReadTimeout                time.Duration
	ProxyConnectionIdleTimeout time.Duration
	ListenSocksPort            int
	FramerSettings             *connect.FramerSettings
}

// FIXME hook up framer to tun device packet write/read

type ProxyConnectHandler struct {

	//

}

func NewProxyConnectHandler() {

	// FIXME create tnet per client id, route output of tnet to

	proxy := goproxy.NewProxyHttpServer()

	proxy.Tr = &http.Transport{
		Dial: func(network, addr string) (net.Conn, error) {
			return tnet.DialContext(context.Background(), network, addr)
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ap, err := netip.ParseAddrPort(addr)
			if err != nil {
				return nil, err
			}

			return tnet.DialContextTCP(ctx, net.TCPAddrFromAddrPort(ap))
		},
	}

	proxy.ConnectDialWithReq = func(req *http.Request, network string, addr string) (net.Conn, error) {
		return tnet.DialContext(req.Context(), network, addr)
	}

}

func connectProxy() {
	// FIXME

	// if not exists, create a new one
	// each network action, record activity. Close connection if no activity in Timeout

	// the tnet packets are written/read to the exchange transport

}

// http proxy
func (self *ProxyConnectHandler) Connect(w http.ResponseWriter, r *http.Request) {

	// FIXME write the the local http proxy
	// FIXME packet output from the local http proxy should be written to a TUN transport to the resident

	proxyConnection := connectProxy(USER)

	proxyConnection.proxy.ServeHTTP(w, r)
}

func (self *ProxyConnectHandler) runSocks() {

	// FIXME write to the local SOCKS proxy
	// FIXME packet output from the local http proxy should be written to a TUN transport to the resident

	server := socks5.NewServer(
		socks5.WithLogger(self),
		socks5.WithAuthMethods([]Authenticator{
			UserPassAuthenticator(self),
		}),
		socks5.WithResolver(self),
		socks5.WithDialAndRequest(func(ctx context.Context, network string, addr string, request *socks5.Request) (net.Conn, error) {
			addrPort, err := netip.ParseAddrPort(addr)
			if err != nil {
				return nil, err
			}

			user := request.UserContext
			proxyConnection := self.connectProxy(user)

			switch network {
			case "tcp", "tcp4", "tcp6":
				return proxyConnection.tnet.DialContextTCP(ctx, net.TCPAddrFromAddrPort(addrPort))
			case "udp", "udp4", "udp6":
				return proxyConnection.tnet.DialContextUDP(ctx, net.UDPAddrFromAddrPort(addrPort))
			default:
				return nil, fmt.Errorf("Unsupported network: %s", network)
			}
		}),
	)

	server.ListenAndServe("tcp", self.settings.ListenSocksPort)

}

// socks.Logger
func (self *SocksLogger) Errorf(format string, args ...any) {
	glog.Errorf("[tp]"+format, args...)
}

// socks.CredentialStore
func (self *SocksCredentialStore) Valid(user, password, userAddr string) bool {
	// FIXME
	return true
}

// socks.NameResolver
func (self *ProxyConnectHandler) Resolve(ctx context.Context, name string) (context.Context, net.IP, error) {
	addrs := self.tnet.DohCache().Query(ctx, "A", name)
	if len(addrs) == 0 {
		return ctx, nil, fmt.Errorf("Not found.")
	}

	// choose one randomly
	addr := addrs[mathrand.Intn(len(addrs))]
	ip := net.Ip(addr.AsSlice())
	return ctx, ip, nil
}
