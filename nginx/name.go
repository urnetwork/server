package nginx

// Host normalization and name matching with nginx's semantics (owner: s-tls).
// The front matches the TLS SNI and each request's authority the same way:
// the SNI picks the certificate, and the authority picks the route (§4.2,
// D9). Both follow the pinned nginx (1.31.4): normalizeHost is
// ngx_http_validate_host, which nginx applies to the SNI and to every Host,
// and nameMatcher is ngx_http_find_virtual_server over the route table's
// server names, which come in two forms only: exact, and a leading wildcard.
//
// nginx gives an invalid or unknown SNI the default server's certificate,
// and an unknown Host the default server's routes; the callers here do the
// same with a false result.
//
// A name comes from the client before any authentication: an SNI can run to
// 64 KiB over TCP (Go caps only the whole ClientHello) and 16 KiB over QUIC,
// and a Host to the front's header limit. Matching one costs time linear in
// its length.

import (
	"iter"
	"strings"
)

// The host nginx's $host holds for an authority, by ngx_http_validate_host at
// the pinned commit:
//   - its :port is removed, and an IPv6 literal keeps its brackets;
//   - one trailing dot is removed;
//   - it is lowercased.
//
// False, which nginx answers with a 400 (and, for an SNI, with the default
// server), for:
//   - a host that is empty once its port and trailing dot are removed;
//   - two dots in a row;
//   - a byte nginx doesn't allow. A name allows letters, digits, '-', '.',
//     '_', '~', '%' and the sub-delims !$&'()*+,;=, and an IPv6 literal the
//     same less '%', plus ':';
//   - an unterminated '[', or anything but a port after its ']';
//   - a port that isn't digits up to 65535. An empty port is allowed.
//
// A leading dot is allowed, as nginx allows it.
func normalizeHost(host string) (string, bool) {
	// nginx's sw_host_start, sw_host, sw_host_ip_literal, sw_host_end and
	// sw_port
	type hostState int
	const (
		hostStateStart hostState = iota
		hostStateName
		hostStateIpLiteral
		hostStateEnd
		hostStatePort
	)
	// nginx's sub-delims, allowed in a name and in an IPv6 literal
	isSubDelim := func(c byte) bool {
		switch c {
		case '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=':
			return true
		default:
			return false
		}
	}

	state := hostStateStart
	// the last dot's index, as nginx's dot_pos, which starts past the end
	dotIndex := len(host)
	// the host's length without its port, as nginx's host_len
	hostLength := len(host)
	port := 0
	for i := 0; i < len(host); i += 1 {
		c := host[i]
		switch state {
		case hostStateStart:
			if c == '[' {
				state = hostStateIpLiteral
				continue
			}
			state = hostStateName
			fallthrough
		case hostStateName:
			switch {
			case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
			case c == ':':
				hostLength = i
				state = hostStatePort
			case c == '.':
				if dotIndex == i-1 {
					return "", false
				}
				dotIndex = i
			case c == '-', c == '_', c == '~', c == '%', isSubDelim(c):
			default:
				return "", false
			}
		case hostStateIpLiteral:
			switch {
			case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
			case c == ':':
			case c == ']':
				hostLength = i + 1
				state = hostStateEnd
			case c == '.':
				if dotIndex == i-1 {
					return "", false
				}
				dotIndex = i
			case c == '-', c == '_', c == '~', isSubDelim(c):
			default:
				return "", false
			}
		case hostStateEnd:
			if c != ':' {
				return "", false
			}
			state = hostStatePort
		case hostStatePort:
			if c < '0' || '9' < c {
				return "", false
			}
			digit := int(c - '0')
			// over 65535, checked as nginx does before the multiply
			if 6553 <= port && (6553 < port || 5 < digit) {
				return "", false
			}
			port = port*10 + digit
		}
	}
	if state == hostStateIpLiteral {
		return "", false
	}
	if dotIndex == hostLength-1 {
		hostLength -= 1
	}
	if hostLength == 0 {
		return "", false
	}
	// every byte left is ascii, so this is nginx's ngx_strlow
	return strings.ToLower(host[:hostLength]), true
}

// What a served name routes to.
type nameMatch struct {
	// the served name that matched, as the route table writes it: the name
	// itself, or the wildcard *.b.c that covers it
	name string
	// the owning service; nil for an lb name
	service *RouteService
	class   NameClass
	// the redirect's host, for NameClassRedirect
	redirectTarget string
}

// The route table's served names, indexed for matching. Read-only once built,
// so safe for concurrent use.
type nameMatcher struct {
	// the exact names
	exactNameMatches map[string]nameMatch
	// the leading wildcards, each *.b.c keyed by its base b.c
	wildcardBaseMatches map[string]nameMatch
	// the longest base in wildcardBaseMatches, in bytes: no longer part of a
	// name can match, so match looks none up
	maxWildcardBaseLength int
}

// Indexes every served name of a parsed table, the set RouteTable.ServedNames
// lists: the lb names (NameClassLb), every exposed service's names
// (NameClassConnect for front.connect_service, NameClassApi for
// front.api_service, NameClassForwarded for the rest) and its redirect
// aliases (NameClassRedirect, with its target). A service that isn't exposed
// has no server in nginx, so its names and aliases are left out: they match
// as unknown names, the default server's on TCP and refused on QUIC, and a
// shorter wildcard of an exposed service still covers them. A parsed table
// serves each name once.
func newNameMatcher(routeTable *RouteTable) *nameMatcher {
	matcher := &nameMatcher{
		exactNameMatches:    map[string]nameMatch{},
		wildcardBaseMatches: map[string]nameMatch{},
	}
	addName := func(name string, match nameMatch) {
		match.name = name
		if base, ok := strings.CutPrefix(name, "*."); ok {
			matcher.wildcardBaseMatches[base] = match
			matcher.maxWildcardBaseLength = max(matcher.maxWildcardBaseLength, len(base))
		} else {
			matcher.exactNameMatches[name] = match
		}
	}

	for _, name := range routeTable.Lb.Names {
		addName(name, nameMatch{
			class: NameClassLb,
		})
	}
	for _, service := range routeTable.Services {
		if !service.Exposed {
			continue
		}
		class := NameClassForwarded
		switch service.Name {
		case routeTable.Front.ConnectService:
			class = NameClassConnect
		case routeTable.Front.ApiService:
			class = NameClassApi
		}
		for _, name := range service.Names {
			addName(name, nameMatch{
				service: service,
				class:   class,
			})
		}
		for _, redirectAlias := range service.RedirectAliases {
			addName(redirectAlias.Name, nameMatch{
				service:        service,
				class:          NameClassRedirect,
				redirectTarget: redirectAlias.Target,
			})
		}
	}
	return matcher
}

// Matches a normalized name as nginx's server_name does: an exact name first,
// then the longest leading wildcard that matches. A wildcard *.b.c matches
// one or more leading labels (a.b.c, x.a.b.c), never b.c itself. False for a
// name the table doesn't serve.
func (self *nameMatcher) match(name string) (nameMatch, bool) {
	if match, ok := self.exactNameMatches[name]; ok {
		return match, true
	}
	// longest first, so the first base found is the longest wildcard's, as
	// ngx_hash_find_wc_head finds the deepest
	for base := range wildcardBases(name, self.maxWildcardBaseLength) {
		if match, ok := self.wildcardBaseMatches[base]; ok {
			return match, true
		}
	}
	return nameMatch{}, false
}

// The bases a leading wildcard covering name could have, longest first: the
// part of name after each of its dots. Only bases of at most maxBaseLength
// bytes are yielded, and the walk starts where the first of them can begin,
// so a long name costs no more than a short one. Looking up the part after
// every dot would hash bytes in the square of the name's length, since a Go
// map of more than 8 entries hashes the whole key on every lookup. None for
// a negative maxBaseLength.
func wildcardBases(name string, maxBaseLength int) iter.Seq[string] {
	return func(yield func(string) bool) {
		if maxBaseLength < 0 {
			return
		}
		rest := name
		// a base of at most maxBaseLength bytes follows a dot in the last
		// maxBaseLength+1 bytes
		if maxBaseLength < len(rest)-1 {
			rest = rest[len(rest)-1-maxBaseLength:]
		}
		for {
			_, base, ok := strings.Cut(rest, ".")
			if !ok || !yield(base) {
				return
			}
			rest = base
		}
	}
}

// Every served name's match, the exact names and the wildcards, in no set
// order. A new slice on every call.
func (self *nameMatcher) servedMatches() []nameMatch {
	matches := make([]nameMatch, 0, len(self.exactNameMatches)+len(self.wildcardBaseMatches))
	for _, match := range self.exactNameMatches {
		matches = append(matches, match)
	}
	for _, match := range self.wildcardBaseMatches {
		matches = append(matches, match)
	}
	return matches
}
