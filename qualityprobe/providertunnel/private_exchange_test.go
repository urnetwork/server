package providertunnel

import (
	"context"
	"net"

	connectserver "github.com/urnetwork/server/connect"
)

// A local platform's connect exchange, on an OS-assigned loopback port that it
// owns. connect binds a fixed service port with SO_REUSEPORT on every address,
// so every other process that binds the same port, such as the proxy package's
// harness or a second run of these tests, takes a share of the exchange's
// resident connections.
func newPrivateTestExchange(ctx context.Context, host string) (*connectserver.Exchange, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	return connectserver.NewExchangeWithListeners(
		ctx,
		host,
		"connect",
		"test",
		map[int]int{port: port},
		map[string]string{host: "127.0.0.1"},
		connectserver.DefaultExchangeSettings(),
		map[int]net.Listener{port: listener},
	), nil
}
