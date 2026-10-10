package server

import (
	"testing"
)

// The lb's address corpus (warp nginx_test.go), less the nginx `client:` field
// that the lb removes whole: here the field stays and only its value is
// scrubbed. A case that diverges between the two copies fails here.
func TestScrubAddrsMatchesBothFamilies(t *testing.T) {
	entries := []struct {
		entry    string
		scrubbed string
	}{
		// the client field, whichever family was written in it
		{
			`[error] *1 upstream timed out, client: 203.0.113.9, server: 0.0.0.0:443`,
			`[error] *1 upstream timed out, client: [scrubbed], server: [scrubbed]:443`,
		},
		{
			`[error] *1 upstream timed out, client: 2001:db8::dead:beef, server: [::]:443`,
			`[error] *1 upstream timed out, client: [scrubbed], server: [scrubbed]:443`,
		},
		{
			`[error] *1 upstream timed out, client: 2001:0db8:0000:0000:0000:0000:dead:beef, server: [::]:443`,
			`[error] *1 upstream timed out, client: [scrubbed], server: [scrubbed]:443`,
		},
		{
			`[error] *1 upstream timed out, client: ::ffff:203.0.113.9, server: [::]:443`,
			`[error] *1 upstream timed out, client: [scrubbed], server: [scrubbed]:443`,
		},
		// a peer that is not ip at all
		{
			`[error] *1 upstream timed out, client: unix:, server: `,
			`[error] *1 upstream timed out, client: unix:, server: `,
		},
		// loose in a request line, which the client wrote
		{
			`[error] *1 open() failed, request: "GET /probe?peer=203.0.113.9 HTTP/1.1"`,
			`[error] *1 open() failed, request: "GET /probe?peer=[scrubbed] HTTP/1.1"`,
		},
		{
			`[error] *1 open() failed, request: "GET /probe?peer=2001:db8::1 HTTP/1.1"`,
			`[error] *1 open() failed, request: "GET /probe?peer=[scrubbed] HTTP/1.1"`,
		},
		// loose in a Host header, which the client also wrote
		{
			`[error] *1 no live upstreams, host: "203.0.113.9"`,
			`[error] *1 no live upstreams, host: "[scrubbed]"`,
		},
		// the port is not an address and says which service block the entry is
		// about, so it stays. The punctuation around it is text
		{
			`[error] *1 recv() failed, peer 203.0.113.9:51234.`,
			`[error] *1 recv() failed, peer [scrubbed]:51234.`,
		},
		// no range is exempt: the private addresses the service blocks live on,
		// loopback, the unspecified listen address and the resolvers all go
		{
			`[error] *1 connect() failed while connecting to upstream, upstream: "http://10.0.0.4:80/status", host: "lb.example.com"`,
			`[error] *1 connect() failed while connecting to upstream, upstream: "http://[scrubbed]:80/status", host: "lb.example.com"`,
		},
		{
			`[error] *1 connect() failed, upstream: "http://172.17.0.2:80/", server: 0.0.0.0:443`,
			`[error] *1 connect() failed, upstream: "http://[scrubbed]:80/", server: [scrubbed]:443`,
		},
		{
			`[error] *1 connect() failed, upstream: "http://192.168.1.9:80/", server: [::]:443`,
			`[error] *1 connect() failed, upstream: "http://[scrubbed]:80/", server: [scrubbed]:443`,
		},
		{
			`[error] *1 connect() failed, upstream: "http://127.0.0.1:19999/dead"`,
			`[error] *1 connect() failed, upstream: "http://[scrubbed]:19999/dead"`,
		},
		{
			`[error] *1 recv() failed while resolving, resolver: 1.1.1.1`,
			`[error] *1 recv() failed while resolving, resolver: [scrubbed]`,
		},
		// the brackets of `[addr]:port` belong to the address, and a zone is an
		// interface name rather than part of one
		{
			`[error] *1 connect() failed, upstream: "[fc00::4]:80", peer fe80::1%eth0, self ::1`,
			`[error] *1 connect() failed, upstream: "[scrubbed]:80", peer [scrubbed]%eth0, self [scrubbed]`,
		},
		{
			`[error] *1 upstream timed out, server: [2001:db8:beef::5]:443`,
			`[error] *1 upstream timed out, server: [scrubbed]:443`,
		},
		// text that is merely address shaped
		{
			`2026/08/12 15:42:35 [error] 73966#0: *2 recv() failed (104: Connection reset by peer)`,
			`2026/08/12 15:42:35 [error] 73966#0: *2 recv() failed (104: Connection reset by peer)`,
		},
		{
			`[error] *1 upstream timed out, bytes from/to client:0/0, bytes from/to upstream:0/0`,
			`[error] *1 upstream timed out, bytes from/to client:0/0, bytes from/to upstream:0/0`,
		},
		// percent encoding in a request line is not a zone
		{
			`[error] *1 open() failed, request: "GET /a%20b.c%2ed HTTP/1.1"`,
			`[error] *1 open() failed, request: "GET /a%20b.c%2ed HTTP/1.1"`,
		},
		// the lines a service itself writes
		{
			`http: TLS handshake error from 203.0.113.9:51423: EOF`,
			`http: TLS handshake error from [scrubbed]:51423: EOF`,
		},
		{
			`[h]request from 2606:4700:4700::1111 took 12ms`,
			`[h]request from [scrubbed] took 12ms`,
		},
	}

	for _, entry := range entries {
		if got := string(scrubAddrs([]byte(entry.entry))); got != entry.scrubbed {
			t.Errorf("scrubbing\n  %s\ngave\n  %s\nwant\n  %s", entry.entry, got, entry.scrubbed)
		}
	}
}
