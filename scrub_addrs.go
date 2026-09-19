package server

// The address scrubber the lb runs over nginx's error log (warp nginx.go),
// as its own copy. The root `warp` package is the lb's process runtime, which
// a service must not depend on, so the algorithm is carried here and
// scrub_addrs_test.go carries the lb's address corpus, so the two copies
// cannot drift apart without a test saying so.

import (
	"bytes"
	"net/netip"
	"strconv"
)

const scrubbedAddr = "[scrubbed]"

// an address is written with these and nothing else, so a maximal run of them
// is the only place one can be hiding. Scanning for the runs and parsing each
// one is what makes both families work: an ipv4 literal and every ipv6 form
// (full, compressed, ipv4 mapped) fall out of the same scan, and text that
// merely looks address shaped -- the `15:42:35` of a timestamp, the `0/0` of
// a byte count -- fails to parse and is left alone.
func isAddrByte(b byte) bool {
	switch {
	case '0' <= b && b <= '9':
		return true
	case 'a' <= b && b <= 'f', 'A' <= b && b <= 'F':
		return true
	case b == ':', b == '.', b == '%':
		return true
	}
	return false
}

// scrubAddrs replaces every IPv4 and IPv6 literal in b with `[scrubbed]`,
// keeping an ipv4 port. The input is arbitrary text: the scan finds maximal
// runs of address bytes and keeps only the runs that actually parse, so
// surrounding words, timestamps and byte counts survive.
func scrubAddrs(b []byte) []byte {
	scrubbed := make([]byte, 0, len(b))

	i := 0
	for i < len(b) {
		if !isAddrByte(b[i]) {
			scrubbed = append(scrubbed, b[i])
			i += 1
			continue
		}
		j := i
		for j < len(b) && isAddrByte(b[j]) {
			j += 1
		}

		replacement, ok := scrubAddr(b[i:j])
		if !ok {
			scrubbed = append(scrubbed, b[i:j]...)
			i = j
			continue
		}
		// ipv6 with a port is written `[addr]:port`, and the brackets belong to
		// the address that is going away
		if 0 < len(scrubbed) && scrubbed[len(scrubbed)-1] == '[' && j < len(b) && b[j] == ']' {
			scrubbed = scrubbed[:len(scrubbed)-1]
			j += 1
		}
		scrubbed = append(scrubbed, replacement...)
		i = j
	}

	return scrubbed
}

// `token` is a maximal run of address bytes, so more than the address can be in
// it
func scrubAddr(token []byte) ([]byte, bool) {
	// a zone is an interface name rather than part of the address, and only its
	// leading hex characters are in the token at all (`fe80::1%eth0` runs out at
	// the `t`), so the address ends where the zone starts
	addrEnd := len(token)
	if percent := bytes.IndexByte(token, '%'); 0 <= percent {
		addrEnd = percent
	}

	// the token can also carry a port (`10.0.0.4:80`) and trailing punctuation
	// from the surrounding text (`10.0.0.4.`), while a trailing separator can
	// equally be part of the address itself (`::`). So the longest prefix that
	// parses wins, and whatever follows it is text that stays.
	for end := addrEnd; 0 < end; end -= 1 {
		candidate := string(token[:end])

		if _, err := netip.ParseAddr(candidate); err == nil {
			return append([]byte(scrubbedAddr), token[end:]...), true
		}
		// ipv6 is only written with a port inside brackets, which are not
		// address bytes, so this is the ipv4 `addr:port` case. The port is not
		// an address and says which service block the entry is about, so it is
		// put back.
		if addrPort, err := netip.ParseAddrPort(candidate); err == nil {
			scrubbed := scrubbedAddr + ":" + strconv.Itoa(int(addrPort.Port()))
			return append([]byte(scrubbed), token[end:]...), true
		}
	}

	return nil, false
}
