package router

import (
	"strings"
)

// route_trie.go implements the path-segment trie that Router uses to match a
// request path to a route without scanning every route.
//
// Each trie node is one literal path segment. A route is inserted along the run
// of literal segments that prefixes its pattern (its "static prefix"); the first
// segment carrying a regex metacharacter ends the prefix. Fully static routes
// (no metacharacter anywhere) terminate at their final segment and need no regex
// at match time. A route with a dynamic tail is parked at the node for its
// static prefix as a tail matcher whose full `^pattern$` regex is run against the
// whole path — so the trie only narrows the candidate set; the regex semantics
// are identical to the original full scan.
//
// Matching walks the request path segment by segment. At every node along the
// walk it runs that node's tail matchers (their static prefix is, by
// construction, a prefix of the request path); when the walk consumes the whole
// path at a node it also considers that node's static terminals. Among all
// routes whose pattern matches, the lowest declaration index wins, reproducing
// the first-match-wins precedence of the original linear scan. The match path
// performs no heap allocation for a static route that hits its method.

// trieNode is one literal path segment. childSegmentNodes branches deeper;
// staticRoutes terminate exactly here; dynamicRoutes have their static prefix
// ending here and a regex tail matched against the full path.
type trieNode struct {
	childSegmentNodes map[string]*trieNode
	staticRoutes      []*Route
	dynamicRoutes     []*Route
}

func newTrieNode() *trieNode {
	return &trieNode{
		childSegmentNodes: map[string]*trieNode{},
	}
}

// routeTrie indexes a route table by static path-segment prefix.
type routeTrie struct {
	root *trieNode
}

// newRouteTrie builds the trie and assigns each route its declaration index,
// which match uses for first-match-wins precedence.
func newRouteTrie(routes []*Route) *routeTrie {
	self := &routeTrie{root: newTrieNode()}
	for index, route := range routes {
		route.index = index
		prefixSegments, dynamic := staticPrefixSegments(route.pattern)
		node := self.root
		for _, segment := range prefixSegments {
			childNode := node.childSegmentNodes[segment]
			if childNode == nil {
				childNode = newTrieNode()
				node.childSegmentNodes[segment] = childNode
			}
			node = childNode
		}
		if dynamic {
			node.dynamicRoutes = append(node.dynamicRoutes, route)
		} else {
			node.staticRoutes = append(node.staticRoutes, route)
		}
	}
	return self
}

// staticPrefixSegments splits a route pattern into its leading run of literal
// path segments and reports whether a regex tail follows. A segment is literal
// iff it carries no regex metacharacter. The walk stops at the first non-literal
// segment; if every segment is literal the route is fully static (dynamic ==
// false) and the returned segments are its complete path.
//
// A '/' inside a regex tail (e.g. the char class in `([^/]+)`) is handled: the
// fragment before that slash already carries a metacharacter, so the prefix has
// already ended.
func staticPrefixSegments(pattern string) (segments []string, dynamic bool) {
	rest := pattern
	if 0 < len(rest) && rest[0] == '/' {
		rest = rest[1:]
	}
	for {
		slashIndex := strings.IndexByte(rest, '/')
		last := slashIndex < 0
		var segment string
		if last {
			segment = rest
		} else {
			segment = rest[:slashIndex]
		}
		if !isLiteralSegment(segment) {
			// first segment with a regex metacharacter ends the static prefix
			return segments, true
		}
		segments = append(segments, segment)
		if last {
			return segments, false
		}
		rest = rest[slashIndex+1:]
	}
}

// isLiteralSegment reports whether s contains no regex metacharacter, i.e.
// matching s as a regex is equivalent to matching it literally. This is the same
// set that regexp.QuoteMeta escapes; checking inline avoids QuoteMeta's per-call
// allocation when building the trie.
func isLiteralSegment(s string) bool {
	for i := 0; i < len(s); i += 1 {
		switch s[i] {
		case '\\', '.', '+', '*', '?', '(', ')', '|', '[', ']', '{', '}', '^', '$':
			return false
		}
	}
	return true
}

// matchState accumulates the best (lowest-index) matching route and, when no
// candidate matches the method, the distinct allowed methods for a 405.
// allowedMethods is allocated only on a method mismatch, so the common path
// stays allocation-free.
type matchState struct {
	method         string
	bestRoute      *Route
	allowedMethods []string
}

// consider folds one route whose pattern matched into the result: a method hit
// updates bestRoute (keeping the lowest index); a method miss records the method
// for a possible 405.
func (self *matchState) consider(route *Route) {
	if route.method == self.method || route.method == "*" {
		if self.bestRoute == nil || route.index < self.bestRoute.index {
			self.bestRoute = route
		}
	} else {
		for _, allowedMethod := range self.allowedMethods {
			if allowedMethod == route.method {
				return
			}
		}
		self.allowedMethods = append(self.allowedMethods, route.method)
	}
}

func (self *matchState) considerDynamic(node *trieNode, path string) {
	for _, route := range node.dynamicRoutes {
		if route.regex.MatchString(path) {
			self.consider(route)
		}
	}
}

func (self *matchState) considerStatic(node *trieNode) {
	for _, route := range node.staticRoutes {
		self.consider(route)
	}
}

// match walks the request path through the trie and returns the winning route
// (or nil) and, when nothing matched the method, the allowed methods for a 405.
func (self *routeTrie) match(method string, path string) (*Route, []string) {
	state := matchState{method: method}

	node := self.root
	rest := path
	if 0 < len(rest) && rest[0] == '/' {
		rest = rest[1:]
	}
	for {
		// tail matchers parked here: their static prefix equals the segments
		// consumed to reach `node`, so run their full-path regex.
		state.considerDynamic(node, path)

		slashIndex := strings.IndexByte(rest, '/')
		if slashIndex < 0 {
			// `rest` is the final path segment (the last element of a split).
			// Descend by it to reach a terminal node, if any.
			childNode := node.childSegmentNodes[rest]
			if childNode != nil {
				state.considerDynamic(childNode, path)
				state.considerStatic(childNode)
			}
			break
		}

		segment := rest[:slashIndex]
		childNode := node.childSegmentNodes[segment]
		if childNode == nil {
			// this segment continues no route's literal prefix, and it is not
			// the final segment, so no static terminal can match. stop.
			break
		}
		node = childNode
		rest = rest[slashIndex+1:]
	}

	return state.bestRoute, state.allowedMethods
}
