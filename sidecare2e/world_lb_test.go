//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's lb interfaces (world_test.go), as warp's lb hands a connection to a block's stream port: the router
// interface, WorldSettings.LbAddress, whose tlsedge ports reach tlsedge's direct listeners and whose TCP 25 reaches
// mxedge's router, and a second interface of IPv6 alone, WorldSettings.LbIpv6Address, off §19.5's router list, whose
// tlsedge ports reach tlsedge too; each connection opens with the PROXY v1 header the lb writes, naming its source.
// The dns API lists both as its lb inventory, so the dns edge's probes (§12.6, §13.4) reach the world's own tlsedge
// through each family and mxedge through the router, the edge answers a published name's A and AAAA (§12.7's * edge
// records) from the interfaces whose probes pass, and a visitor to a published name reaches tlsedge the same way
// (world_sockets_test.go). mxedge-platform's own interface, WorldSettings.PlatformLbAddress, carries its TCP 25 alone
// (W4), which reaches the platform listener's direct listener, and is in no inventory, as the report host's address is
// dns.yml's.

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
// ports at either interface, mxedge's router's for 25 at the router interface alone, or the platform listener's for 25
// at the platform's interface, which forwards nothing else.
func (self *World) lbTarget(destination netip.AddrPort) (string, error) {
	router := destination.Addr() == self.settings.LbAddress
	ipv6 := self.settings.LbIpv6Address.IsValid() && destination.Addr() == self.settings.LbIpv6Address
	platform := self.settings.PlatformLbAddress.IsValid() && destination.Addr() == self.settings.PlatformLbAddress
	if !router && !ipv6 && !platform {
		return "", fmt.Errorf("no lb interface answers at %s", destination.Addr())
	}
	block, port := "", int(destination.Port())
	switch {
	case platform && port == worldSmtpPort:
		block = WorldBlockMxedgePlatform
	case platform:
		return "", fmt.Errorf("the lb forwards no port %d at %s", port, destination.Addr())
	case port == worldSmtpPort && router:
		block = WorldBlockMxedge
	case slices.Contains(tlsedgehost.HostServicePorts, port):
		block = WorldBlockTlsedge
	default:
		return "", fmt.Errorf("the lb forwards no port %d at %s", port, destination.Addr())
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
// PROXY v1 header naming source and destination, as warp's lb opens each stream port's connection. The two must be of
// one family, as a PROXY v1 line's are.
func (self *World) DialLb(ctx context.Context, source netip.AddrPort, destination netip.AddrPort) (net.Conn, error) {
	if source.Addr().Unmap().Is4() != destination.Addr().Unmap().Is4() {
		return nil, fmt.Errorf("a PROXY line names one family, not %s and %s", source.Addr(), destination.Addr())
	}
	target, err := self.lbTarget(destination)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	family := "TCP4"
	if !source.Addr().Unmap().Is4() {
		family = "TCP6"
	}
	header := fmt.Appendf(nil, "PROXY %s %s %s %d %d\r\n", family, source.Addr().Unmap(), destination.Addr().Unmap(), source.Port(), destination.Port())
	if _, err := conn.Write(header); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// The dns API's lb inventory: the router interface, on §19.5's router list, so its tlsedge ports take the user names'
// probes and its 25 the mx hosts' banner probe, and the IPv6 interface, whose tlsedge ports take the user names'.
func (self *World) lbInterfaces() []dnsservice.DnsLbInterface {
	interfaces := []dnsservice.DnsLbInterface{{Name: self.settings.LbName, Ipv4: self.settings.LbAddress, Router: true}}
	if self.settings.LbIpv6Address.IsValid() {
		interfaces = append(interfaces, dnsservice.DnsLbInterface{Name: self.settings.LbIpv6Name, Ipv6: self.settings.LbIpv6Address})
	}
	return interfaces
}

// The addresses of each family the edge answers a user name's edge rows with while every probe passes: every lb
// interface's, sorted, as dnsservice.HealthySet orders them.
func (self *World) LbEdgeAddresses() (ipv4s []netip.Addr, ipv6s []netip.Addr) {
	for _, lbInterface := range self.lbInterfaces() {
		if lbInterface.Ipv4.IsValid() {
			ipv4s = append(ipv4s, lbInterface.Ipv4.Unmap())
		}
		if lbInterface.Ipv6.IsValid() {
			ipv6s = append(ipv6s, lbInterface.Ipv6)
		}
	}
	slices.SortFunc(ipv4s, netip.Addr.Compare)
	slices.SortFunc(ipv6s, netip.Addr.Compare)
	return ipv4s, ipv6s
}

// The dns edge's probe dialer: each probe through the world's lb from the edge's own address of the probed address's
// family (WorldSettings.LbProbeSource, LbIpv6ProbeSource), so tlsedge answers a hello for probe.edge.<root> with its
// alert and mxedge's router greets 220, as they answer production's probes. Any address but the lb's is refused, as
// no other lb answers.
type worldLbProbeDialer struct {
	world *World
}

var _ dnsservice.DnsProbeDialer = (*worldLbProbeDialer)(nil)

func (self *worldLbProbeDialer) DialProbe(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
	source := self.world.settings.LbProbeSource
	if !address.Addr().Unmap().Is4() {
		source = self.world.settings.LbIpv6ProbeSource
	}
	return self.world.DialLb(ctx, source, address)
}
