package nginx_test

// The parity container's addresses (parity_test.go). The lb image has no ip
// tool, so the harness speaks rtnetlink itself, as
// `ip address add <addr>/<32 or 128> dev lo nodad` does.

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

// The kernel's rtnetlink layouts, in bytes: the message header, the address
// message, an attribute's header and its alignment (NLA_ALIGNTO), and the
// buffer one acknowledgement is read into.
const (
	parityNetlinkHeaderBytes = 16
	parityIfAddrMessageBytes = 8
	parityRtAttrHeaderBytes  = 4
	parityRtAttrAlignBytes   = 4
	parityNetlinkReplyBytes  = 4 * 1024
)

// Adds addr to the loopback device as a host address, with no duplicate
// address detection, so it can be bound at once. An address already there is
// not an error.
func addParityLoopbackAddr(addr netip.Addr) error {
	loopback, err := net.InterfaceByName("lo")
	if err != nil {
		return err
	}
	family := unix.AF_INET
	prefixBits := 32
	if addr.Is6() {
		family = unix.AF_INET6
		prefixBits = 128
	}

	attribute := func(attributeType uint16, value []byte) []byte {
		length := parityRtAttrHeaderBytes + len(value)
		encoded := make([]byte, (length+parityRtAttrAlignBytes-1)/parityRtAttrAlignBytes*parityRtAttrAlignBytes)
		binary.NativeEndian.PutUint16(encoded[0:2], uint16(length))
		binary.NativeEndian.PutUint16(encoded[2:4], attributeType)
		copy(encoded[parityRtAttrHeaderBytes:], value)
		return encoded
	}
	body := make([]byte, parityIfAddrMessageBytes)
	body[0] = byte(family)
	body[1] = byte(prefixBits)
	body[2] = unix.IFA_F_NODAD
	body[3] = unix.RT_SCOPE_UNIVERSE
	binary.NativeEndian.PutUint32(body[4:8], uint32(loopback.Index))
	body = append(body, attribute(unix.IFA_LOCAL, addr.AsSlice())...)
	body = append(body, attribute(unix.IFA_ADDRESS, addr.AsSlice())...)

	message := make([]byte, parityNetlinkHeaderBytes, parityNetlinkHeaderBytes+len(body))
	binary.NativeEndian.PutUint32(message[0:4], uint32(parityNetlinkHeaderBytes+len(body)))
	binary.NativeEndian.PutUint16(message[4:6], unix.RTM_NEWADDR)
	binary.NativeEndian.PutUint16(message[6:8], unix.NLM_F_REQUEST|unix.NLM_F_ACK|unix.NLM_F_CREATE|unix.NLM_F_EXCL)
	binary.NativeEndian.PutUint32(message[8:12], 1)
	message = append(message, body...)

	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	kernel := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	if err := unix.Sendto(fd, message, 0, kernel); err != nil {
		return err
	}
	reply := make([]byte, parityNetlinkReplyBytes)
	n, _, err := unix.Recvfrom(fd, reply, 0)
	if err != nil {
		return err
	}
	replies, err := syscall.ParseNetlinkMessage(reply[:n])
	if err != nil {
		return err
	}
	for _, replyMessage := range replies {
		if replyMessage.Header.Type != unix.NLMSG_ERROR || len(replyMessage.Data) < 4 {
			continue
		}
		errno := -int32(binary.NativeEndian.Uint32(replyMessage.Data[0:4]))
		if errno == 0 || syscall.Errno(errno) == syscall.EEXIST {
			return nil
		}
		return fmt.Errorf("adding %s to lo: %w", addr, syscall.Errno(errno))
	}
	return fmt.Errorf("adding %s to lo: the kernel sent no acknowledgement", addr)
}
