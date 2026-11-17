package mcp

// Static discovery of the resources an html page references, and target
// validation for every url the fetch tool loads.
//
// Discovery is static only: the html is parsed and element references are
// resolved, with no script execution. Anything a page loads from javascript
// (single page app content, lazy loaded media) is invisible here, which is
// stated in the tool description so callers are not misled.
//
// Target validation is deliberately narrow. The target hostname is resolved by
// the provider at the egress location, and the connect packet path enforces its
// ip_security policy against the resolved address there.

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// One resource the page references.
type resourceRef struct {
	url  *url.URL
	kind string
}

const (
	resourceKindImage      = "image"
	resourceKindStylesheet = "stylesheet"
	resourceKindScript     = "script"
	resourceKindMedia      = "media"
)

// Rejects targets that are not fetchable web urls. Address policy belongs to
// connect/ip_security at the actual egress, where DNS has the correct view.
func validateFetchUrl(fetchUrl *url.URL) error {
	switch fetchUrl.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("unsupported url scheme %q, only http and https are fetchable", fetchUrl.Scheme)
	}

	host := fetchUrl.Hostname()
	if host == "" {
		return fmt.Errorf("the url has no host")
	}
	if fetchUrl.User != nil {
		return fmt.Errorf("url user information is not supported; use a request header")
	}

	// Non-public destinations are dropped by connect/ip_security after DNS
	// resolution, so callers observe the expected connection timeout.
	return nil
}

// Parses html and returns the resources it references, resolved against base,
// deduplicated, and capped at maxRefs. A parse failure yields no references
// rather than an error: the page body is still a useful result on its own.
func extractResourceRefs(base *url.URL, body []byte, maxRefs int) []*resourceRef {
	if maxRefs <= 0 {
		return nil
	}

	root, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil
	}

	refs := []*resourceRef{}
	seen := map[string]bool{}

	add := func(rawUrl string, kind string) {
		rawUrl = strings.TrimSpace(rawUrl)
		if rawUrl == "" || strings.HasPrefix(rawUrl, "data:") {
			return
		}
		refUrl, err := base.Parse(rawUrl)
		if err != nil {
			return
		}
		if err := validateFetchUrl(refUrl); err != nil {
			return
		}
		// the fragment never changes what is fetched
		refUrl.Fragment = ""
		if seen[refUrl.String()] {
			return
		}
		if maxRefs <= len(refs) {
			return
		}
		seen[refUrl.String()] = true
		refs = append(refs, &resourceRef{url: refUrl, kind: kind})
	}

	attr := func(node *html.Node, name string) string {
		for _, a := range node.Attr {
			if strings.EqualFold(a.Key, name) {
				return a.Val
			}
		}
		return ""
	}

	// the first candidate of a srcset is enough; the tool returns references,
	// not a rendering, so picking a specific density is not meaningful
	firstSrcSetCandidate := func(srcSet string) string {
		for _, candidate := range strings.Split(srcSet, ",") {
			fields := strings.Fields(candidate)
			if 0 < len(fields) {
				return fields[0]
			}
		}
		return ""
	}

	var walk func(node *html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch strings.ToLower(node.Data) {
			case "img":
				add(attr(node, "src"), resourceKindImage)
				add(firstSrcSetCandidate(attr(node, "srcset")), resourceKindImage)
			case "source":
				add(attr(node, "src"), resourceKindMedia)
				add(firstSrcSetCandidate(attr(node, "srcset")), resourceKindImage)
			case "video", "audio":
				add(attr(node, "src"), resourceKindMedia)
				add(attr(node, "poster"), resourceKindImage)
			case "script":
				add(attr(node, "src"), resourceKindScript)
			case "link":
				rel := strings.ToLower(attr(node, "rel"))
				if strings.Contains(rel, "stylesheet") {
					add(attr(node, "href"), resourceKindStylesheet)
				} else if strings.Contains(rel, "icon") {
					add(attr(node, "href"), resourceKindImage)
				}
			case "meta":
				property := strings.ToLower(attr(node, "property"))
				if property == "og:image" || property == "twitter:image" {
					add(attr(node, "content"), resourceKindImage)
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)

	return refs
}

// Reports whether a content type should be delivered to the model as a
// renderable image or audio block rather than as an opaque embedded resource.
func mediaContentKind(contentType string) string {
	mediaType := contentType
	if i := strings.Index(mediaType, ";"); 0 <= i {
		mediaType = mediaType[:i]
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))

	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return resourceKindImage
	case strings.HasPrefix(mediaType, "audio/"):
		return "audio"
	default:
		return ""
	}
}

// Reports whether a content type is textual, and so is returned to the model
// as text rather than as bytes.
func isTextContentType(contentType string) bool {
	mediaType := contentType
	if i := strings.Index(mediaType, ";"); 0 <= i {
		mediaType = mediaType[:i]
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))

	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/xml", "application/xhtml+xml",
		"application/javascript", "application/rss+xml", "application/atom+xml",
		"image/svg+xml", "":
		return true
	default:
		return false
	}
}

// Reports whether the response body should be parsed for resource references.
func isHtmlContentType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(mediaType, "text/html") ||
		strings.HasPrefix(mediaType, "application/xhtml+xml")
}

// Normalizes a host for display without leaking the port when it is the
// scheme default.
func displayHost(fetchUrl *url.URL) string {
	host, port, err := net.SplitHostPort(fetchUrl.Host)
	if err != nil {
		return fetchUrl.Host
	}
	if (fetchUrl.Scheme == "http" && port == "80") ||
		(fetchUrl.Scheme == "https" && port == "443") {
		return host
	}
	return fetchUrl.Host
}
