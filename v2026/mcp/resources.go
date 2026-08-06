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
// the provider at the egress location, not by this service, so dns rebinding
// against our own network is not the threat model; what is worth refusing is a
// non-http scheme or a literal address that names infrastructure rather than a
// site on the internet.

import (
	"fmt"
	"net"
	"net/netip"
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

// Rejects targets that are not fetchable web urls. Loopback and private ranges
// are refused by default because a caller should not be able to aim this tool
// at infrastructure; tests that run their web server on loopback turn the check
// off explicitly.
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

	if fetchAllowPrivateTargets {
		return nil
	}

	// only literal addresses are checked. A hostname is resolved at the egress
	// location, so resolving it here would describe a different network
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return nil
	}
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsUnspecified() {
		return fmt.Errorf("refusing to fetch a private or loopback address")
	}
	// the cloud metadata address is public-range but never a legitimate target
	if addr.Is4() && addr.String() == "169.254.169.254" {
		return fmt.Errorf("refusing to fetch a link local address")
	}

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
