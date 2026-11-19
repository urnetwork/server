package nginx

// The request uri as nginx reads it (owner: s-http). Routing matches nginx's
// locations against $uri, the normalized path, so a request reaches the same
// location through the front as through the lb: //status and /%73tatus are
// both /status, which a service name denies, while /status/ and /STATUS are
// other paths. nginx parses the uri with ngx_http_parse_complex_uri, with
// merge_slashes on (its default), and re-escapes it with ngx_escape_uri's
// NGX_ESCAPE_URI set when it sends it upstream; both are ported here.

import (
	"strings"
)

// One request target as nginx holds it.
type routeUri struct {
	// $uri: percent-decoded, repeated slashes merged and dot segments
	// resolved. It always starts with /.
	path string
	// $args: the raw query after the first ?, without a fragment. Empty
	// when there is none.
	args string
}

// Parses an origin-form request target (`/path?query`) as nginx does. The
// path is decoded as it is scanned, so an escaped / separates segments and
// an escaped dot takes part in a dot segment, as in nginx; an escaped %, #
// or ? is kept as that character. False, which nginx answers with a 400, for
// a target that doesn't start with /, a malformed escape, an escaped NUL, or
// a .. that climbs above the root.
func parseRouteUri(requestTarget string) (routeUri, bool) {
	const (
		stateUsual = iota
		stateSlash
		stateDot
		stateDotDot
		stateQuoted
		stateQuotedSecond
	)

	if !strings.HasPrefix(requestTarget, "/") {
		return routeUri{}, false
	}

	path := make([]byte, 0, len(requestTarget))
	args := ""
	state := stateUsual
	quotedState := stateUsual
	var decoded byte
	// a decoded character waiting to be read in the state before its %
	var pendingCharacter byte
	pending := false

	// set when a ? or # ended the path, which skips the end-of-input dot
	// handling, as nginx's goto args and goto done do
	pathEnded := false
	// the args end at a fragment
	setArgs := func(rest string) {
		args, _, _ = strings.Cut(rest, "#")
	}
	// for a .. that ends a segment: path ends "/<segment>/..", and the
	// segment and the dots go, leaving the slash before the segment
	climb := func() bool {
		for i := len(path) - 4; 0 <= i; i -= 1 {
			if path[i] == '/' {
				path = path[:i+1]
				return true
			}
		}
		return false
	}
	hexValue := func(c byte) (byte, bool) {
		switch {
		case '0' <= c && c <= '9':
			return c - '0', true
		case 'a' <= c|0x20 && c|0x20 <= 'f':
			return c | 0x20 - 'a' + 10, true
		default:
			return 0, false
		}
	}

	i := 0
scan:
	for {
		var c byte
		if pending {
			c = pendingCharacter
			pending = false
		} else {
			if len(requestTarget) <= i {
				break
			}
			c = requestTarget[i]
			i += 1
		}

		switch state {
		case stateUsual:
			switch c {
			case '/':
				state = stateSlash
				path = append(path, c)
			case '%':
				quotedState = state
				state = stateQuoted
			case '?':
				setArgs(requestTarget[i:])
				pathEnded = true
				break scan
			case '#':
				pathEnded = true
				break scan
			default:
				path = append(path, c)
			}

		case stateSlash:
			switch c {
			case '/':
				// merge_slashes
			case '.':
				state = stateDot
				path = append(path, c)
			case '%':
				quotedState = state
				state = stateQuoted
			case '?':
				setArgs(requestTarget[i:])
				pathEnded = true
				break scan
			case '#':
				pathEnded = true
				break scan
			default:
				state = stateUsual
				path = append(path, c)
			}

		case stateDot:
			switch c {
			case '/':
				// a "." segment
				state = stateSlash
				path = path[:len(path)-1]
			case '.':
				state = stateDotDot
				path = append(path, c)
			case '%':
				quotedState = state
				state = stateQuoted
			case '?':
				path = path[:len(path)-1]
				setArgs(requestTarget[i:])
				pathEnded = true
				break scan
			case '#':
				path = path[:len(path)-1]
				pathEnded = true
				break scan
			default:
				state = stateUsual
				path = append(path, c)
			}

		case stateDotDot:
			switch c {
			case '/', '?', '#':
				if !climb() {
					return routeUri{}, false
				}
				if c == '?' {
					setArgs(requestTarget[i:])
					pathEnded = true
					break scan
				}
				if c == '#' {
					pathEnded = true
					break scan
				}
				state = stateSlash
			case '%':
				quotedState = state
				state = stateQuoted
			default:
				state = stateUsual
				path = append(path, c)
			}

		case stateQuoted:
			value, ok := hexValue(c)
			if !ok {
				return routeUri{}, false
			}
			decoded = value
			state = stateQuotedSecond

		case stateQuotedSecond:
			value, ok := hexValue(c)
			if !ok {
				return routeUri{}, false
			}
			character := decoded<<4 + value
			switch character {
			case 0:
				return routeUri{}, false
			case '%', '#', '?':
				// kept as the character, never read as syntax
				state = stateUsual
				path = append(path, character)
			default:
				state = quotedState
				pendingCharacter = character
				pending = true
			}
		}
	}

	if !pathEnded {
		switch state {
		case stateQuoted, stateQuotedSecond:
			return routeUri{}, false
		case stateDot:
			path = path[:len(path)-1]
		case stateDotDot:
			if !climb() {
				return routeUri{}, false
			}
		}
	}
	return routeUri{
		path: string(path),
		args: args,
	}, true
}

// The path and query nginx sends upstream for a uri: the path re-escaped,
// then ? and the args when there are any.
func (self routeUri) upstreamTarget(path string) string {
	escapedPath := escapeRouteUriPath(path)
	if self.args == "" {
		return escapedPath
	}
	return escapedPath + "?" + self.args
}

// Percent-escapes what nginx's NGX_ESCAPE_URI escapes: control bytes, space,
// #, %, ?, DEL and every byte above 0x7f, with uppercase hex.
func escapeRouteUriPath(path string) string {
	const hexDigits = "0123456789ABCDEF"
	escape := func(c byte) bool {
		return c <= ' ' || c == '#' || c == '%' || c == '?' || 0x7f <= c
	}
	escapeCount := 0
	for i := 0; i < len(path); i += 1 {
		if escape(path[i]) {
			escapeCount += 1
		}
	}
	if escapeCount == 0 {
		return path
	}
	escapedPath := make([]byte, 0, len(path)+2*escapeCount)
	for i := 0; i < len(path); i += 1 {
		c := path[i]
		if escape(c) {
			escapedPath = append(escapedPath, '%', hexDigits[c>>4], hexDigits[c&0x0f])
		} else {
			escapedPath = append(escapedPath, c)
		}
	}
	return string(escapedPath)
}

// The origin form of a request's raw target, nginx's $request_uri: the
// target as sent when it starts with /, or the path and query of an
// absolute target (`http://host/path`), "/" when it has no path. False for
// any other form, such as `*` or an authority, which nginx refuses.
func routeRequestUri(requestTarget string) (string, bool) {
	if strings.HasPrefix(requestTarget, "/") {
		return requestTarget, true
	}
	_, afterScheme, ok := strings.Cut(requestTarget, "://")
	if !ok {
		return "", false
	}
	pathStart := strings.IndexAny(afterScheme, "/?")
	if pathStart < 0 {
		return "/", true
	}
	if afterScheme[pathStart] == '?' {
		return "/" + afterScheme[pathStart:], true
	}
	return afterScheme[pathStart:], true
}
