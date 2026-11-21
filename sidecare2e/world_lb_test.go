//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's lb interface (world_test.go), as warp's lb hands a connection to a block's stream port: one interface,
// WorldSettings.LbAddress, whose tlsedge ports reach tlsedge's direct listeners and whose TCP 25 reaches mxedge's
// router, each connection opening with the PROXY v1 header the lb writes, naming its source. The dns API lists it as
// its lb inventory, so the dns edge's probes (§12.6, §13.4) reach the world's own tlsedge and mxedge through it, and a
// visitor to a published name reaches tlsedge the same way (world_sockets_test.go).

package sidecare2e

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server/tlsedgehost"
)

// The block and port destination's connection reaches behind the lb: tlsedge's direct listener for one of its service
// ports, or mxedge's router's for 25, at the world's one lb interface alone.
func (self *World) lbTarget(destination netip.AddrPort) (string, error) {
	if destination.Addr() != self.settings.LbAddress {
		return "", fmt.Errorf("no lb interface answers at %s", destination.Addr())
	}
	block, port := "", int(destination.Port())
	switch {
	case port == worldSmtpPort:
		block = WorldBlockMxedge
	case slices.Contains(tlsedgehost.HostServicePorts, port):
		block = WorldBlockTlsedge
	default:
		return "", fmt.Errorf("the lb forwards no port %d", port)
	}
	running := self.Block(block)
	if running == nil {
		return "", fmt.Errorf("the world runs no %s behind the lb", block)
	}
	address := running.DirectAddress(port)
	if address == "" {
		return "", fmt.Errorf("%s is stopped behind the lb", block)
	}
	return address, nil
}

// A connection from source to destination through the world's lb: TCP to the block behind destination, then the lb's
// PROXY v1 header naming source and destination, as warp's lb opens each stream port's connection.
func (self *World) DialLb(ctx context.Context, source netip.AddrPort, destination netip.AddrPort) (net.Conn, error) {
	target, err := self.lbTarget(destination)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	family := "TCP4"
	if source.Addr().Is6() {
		family = "TCP6"
	}
	header := fmt.Appendf(nil, "PROXY %s %s %s %d %d\r\n", family, source.Addr().Unmap(), destination.Addr().Unmap(), source.Port(), destination.Port())
	if _, err := conn.Write(header); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// The dns API's lb inventory: the world's one interface, on §19.5's router list, so its tlsedge ports take the user
// names' probes and its 25 the mx hosts' banner probe.
func (self *World) lbInterfaces() []dnsservice.DnsLbInterface {
	return []dnsservice.DnsLbInterface{{Name: self.settings.LbName, Ipv4: self.settings.LbAddress, Router: true}}
}

// The dns edge's probe dialer: each probe through the world's lb from the edge's own address
// (WorldSettings.LbProbeSource), so tlsedge answers a hello for probe.edge.<root> with its alert and mxedge's router
// greets 220, as they answer production's probes. Any address but the lb's is refused, as no other lb answers.
type worldLbProbeDialer struct {
	world *World
}

var _ dnsservice.DnsProbeDialer = (*worldLbProbeDialer)(nil)

func (self *worldLbProbeDialer) DialProbe(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
	return self.world.DialLb(ctx, self.world.settings.LbProbeSource, address)
}
