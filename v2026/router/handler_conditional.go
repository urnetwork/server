package router

// Conditional requests for a JSON answer (RFC 9110 §8.8.3, §13.1.2): the
// answer carries an entity tag of its exact body, and a request whose
// If-None-Match names that tag is answered 304 Not Modified, without a body.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026/session"
)

// The Cache-Control of a conditional answer: a browser or app may keep it, but
// only for itself, and must revalidate it with its tag before every reuse.
const ConditionalCacheControl = "private, no-cache"

// ConditionalJsonFormatter returns a FormatFunction that writes result as JSON
// with an ETag (JsonEntityTag) and ConditionalCacheControl, and that answers
// 304 Not Modified, with the same headers and no body, when the request's
// If-None-Match names that tag. scope names what the answer is for besides
// the caller's network and the route, such as the query.
func ConditionalJsonFormatter[R any](w http.ResponseWriter, req *http.Request, scope string) FormatFunction[R] {
	return func(clientSession *session.ClientSession, result R) bool {
		body, err := json.Marshal(result)
		if err != nil {
			glog.Infof("[h]response encoding error (%T): %s\n", result, err)
			http.Error(w, UnexpectedErrorMessage, http.StatusInternalServerError)
			return true
		}
		etag := JsonEntityTag(clientSession, req, scope, body)
		header := w.Header()
		header.Set("ETag", etag)
		header.Set("Cache-Control", ConditionalCacheControl)
		if IfNoneMatch(req, etag) {
			if v := glog.V(2); v {
				v.Infof("[h]response (%T): not modified\n", result)
			}
			w.WriteHeader(http.StatusNotModified)
			return true
		}
		if v := glog.V(2); v {
			v.Infof("[h]response (%T): %d bytes\n", result, len(body))
		}
		header.Set("Content-Type", "application/json")
		w.Write(body)
		return true
	}
}

// The strong entity tag of a JSON answer: a digest of the caller's network,
// the route, scope and the exact body bytes. It changes exactly when the body
// does, and answers for different networks, routes or scopes never share a
// tag, even with equal bodies. It reads nothing but what the caller is
// answered, so it tells the caller nothing the body does not. A session
// without a network (no auth) is scoped to no network.
func JsonEntityTag(clientSession *session.ClientSession, req *http.Request, scope string, body []byte) string {
	networkId := ""
	if clientSession != nil && clientSession.ByJwt != nil {
		networkId = clientSession.ByJwt.NetworkId.String()
	}
	digest := sha256.New()
	// each part with its length first, so no two sets of parts write the same
	// bytes
	for _, part := range []string{networkId, req.Method, req.URL.Path, scope} {
		writeEntityTagPart(digest, []byte(part))
	}
	writeEntityTagPart(digest, body)
	sum := digest.Sum(nil)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

func writeEntityTagPart(digest hash.Hash, part []byte) {
	digest.Write(binary.AppendUvarint(nil, uint64(len(part))))
	digest.Write(part)
}

// Whether the request's If-None-Match names etag, compared weakly as a server
// compares them for a GET (RFC 9110 §13.1.2): W/"x" and "x" match, and "*"
// matches any current answer. Every If-None-Match header is read; a list stops
// at its first malformed tag.
func IfNoneMatch(req *http.Request, etag string) bool {
	for _, value := range req.Header.Values("If-None-Match") {
		rest := value
		for {
			rest = textproto.TrimString(rest)
			if rest == "" {
				break
			}
			if rest[0] == ',' {
				rest = rest[1:]
				continue
			}
			if rest[0] == '*' {
				return true
			}
			tag, remain := scanEntityTag(rest)
			if tag == "" {
				break
			}
			if weakEntityTagMatch(tag, etag) {
				return true
			}
			rest = remain
		}
	}
	return false
}

// The entity tag s starts with, W/"x" or "x", and what follows it; "" when s
// does not start with one (RFC 9110 §8.8.3).
func scanEntityTag(s string) (tag string, remain string) {
	start := 0
	if strings.HasPrefix(s, "W/") {
		start = 2
	}
	if len(s) < start+2 || s[start] != '"' {
		return "", ""
	}
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			return s[:i+1], s[i+1:]
		// etagc: %x21 / %x23-7E / obs-text
		case c == 0x21 || 0x23 <= c && c <= 0x7e || 0x80 <= c:
		default:
			return "", ""
		}
	}
	return "", ""
}

// Whether two entity tags match by the weak comparison: the same opaque tag,
// either of them weak or not.
func weakEntityTagMatch(a string, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}
