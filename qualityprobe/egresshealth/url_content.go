// Bounded static HTML evidence distinguishes document gates from page widgets.
package egresshealth

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// Version two excludes inert structure and corroborates document-level prompts.
// This identifies detector semantics without changing timing or quota policy.
const UrlProbeContentMatcherVersion = 2

type urlProbeHtml struct {
	form, humanGate, portalTitle bool
}

// Live scripts may corroborate a gate, but their text is never visible prose.
// Article content defeats generic widget heuristics, not the existing stronger
// platform-script rule. This detector does not execute or render scripts.
func inspectUrlProbeHtml(body []byte) urlProbeHtml {
	page := urlProbeHtml{}
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return page
	}
	var captchaWidget, challengeScript, humanCaptchaMount, humanCaptchaScript bool
	var challengePrompt, javascriptPrompt, googleSorryForm, unusualTraffic, articleContent bool
	containsPrompt := func(value string) bool {
		for _, marker := range []string{"are you a human", "are you human", "verify you are human", "confirm you are human", "verify that you are human", "confirm that you are human", "security check", "just a moment", "captcha verification"} {
			if strings.Contains(value, marker) {
				return true
			}
		}
		return false
	}
	var visit func(*html.Node, bool, bool)
	visit = func(node *html.Node, inArticle, auxiliary bool) {
		if urlProbeHtmlIgnored(node) {
			return
		}
		if node.Type == html.ElementNode {
			inArticle = inArticle || node.Data == "article"
			auxiliary = auxiliary || node.Data == "footer" || node.Data == "aside" || node.Data == "nav"
			for _, attribute := range node.Attr {
				inArticle = inArticle || attribute.Key == "role" && strings.EqualFold(strings.TrimSpace(attribute.Val), "article")
			}
			if node.Data == "script" {
				source, scriptType := "", ""
				for _, attribute := range node.Attr {
					if attribute.Key == "src" {
						source = attribute.Val
					} else if attribute.Key == "type" {
						scriptType = strings.ToLower(strings.TrimSpace(attribute.Val))
					}
				}
				executable := scriptType == "" || scriptType == "module" || scriptType == "text/javascript" || scriptType == "application/javascript"
				if target := urlProbeHtmlMarkerUrl(source); target != nil && executable && !auxiliary {
					path := target.Path
					passive := strings.Contains(path, "/scripts/jsd/") || strings.HasSuffix(path, "/scripts/jsd")
					challengeScript = challengeScript || strings.HasPrefix(path, "/cdn-cgi/challenge-platform/") && !passive
					humanCaptchaScript = humanCaptchaScript || strings.HasSuffix(path, "/captcha.js")
				}
				return
			}
			if node.Data == "title" {
				value := urlProbeHtmlText(node)
				challengePrompt = challengePrompt || containsPrompt(value)
				for _, marker := range []string{"wi-fi sign in", "wifi sign in", "captive portal", "sign in to this network", "log in to this network"} {
					page.portalTitle = page.portalTitle || strings.Contains(value, marker)
				}
				return
			}
			if !inArticle && !auxiliary {
				if node.Data == "h1" {
					challengePrompt = challengePrompt || containsPrompt(urlProbeHtmlText(node))
				}
				if node.Data == "noscript" {
					javascriptPrompt = javascriptPrompt || strings.Contains(urlProbeHtmlText(node), "enable javascript and cookies")
				}
				page.form = page.form || node.Data == "form"
				for _, attribute := range node.Attr {
					value := strings.ToLower(strings.TrimSpace(attribute.Val))
					if attribute.Key == "class" || attribute.Key == "id" {
						for _, token := range strings.Fields(value) {
							captchaWidget = captchaWidget || token == "g-recaptcha" || token == "h-captcha" || token == "cf-turnstile"
						}
						humanCaptchaMount = humanCaptchaMount || attribute.Key == "id" && value == "px-captcha"
					}
					if node.Data == "form" && attribute.Key == "action" {
						if target := urlProbeHtmlMarkerUrl(attribute.Val); target != nil {
							host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
							googleHost := host == "google.com" || strings.HasSuffix(host, ".google.com")
							googleSorryForm = googleSorryForm || strings.HasPrefix(target.Path, "/sorry/") && (target.Host == "" || googleHost)
						}
					}
				}
			}
		}
		if node.Type == html.TextNode {
			value := strings.ToLower(strings.Join(strings.Fields(node.Data), " "))
			articleContent = articleContent || inArticle && value != ""
			unusualTraffic = unusualTraffic || !inArticle && !auxiliary && strings.Contains(value, "unusual traffic from your computer network")
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, inArticle, auxiliary)
		}
	}
	visit(root, false, false)
	widget := captchaWidget || humanCaptchaMount && humanCaptchaScript
	page.humanGate = challengeScript && (challengePrompt || javascriptPrompt) ||
		!articleContent && (challengePrompt && widget || googleSorryForm && unusualTraffic)
	return page
}

// Ignored ancestors suppress both attributes and text, including script sources.
func urlProbeHtmlIgnored(node *html.Node) bool {
	if node.Type != html.ElementNode {
		return false
	}
	switch node.Data {
	case "style", "template", "pre", "code":
		return true
	}
	for _, attribute := range node.Attr {
		if attribute.Key == "hidden" || attribute.Key == "inert" || attribute.Key == "aria-hidden" && strings.EqualFold(strings.TrimSpace(attribute.Val), "true") {
			return true
		}
		if attribute.Key == "style" {
			for _, declaration := range strings.Split(strings.ToLower(attribute.Val), ";") {
				name, value, ok := strings.Cut(declaration, ":")
				name, value = strings.TrimSpace(name), strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(value), "!important"))
				if ok && (name == "display" && value == "none" || name == "visibility" && (value == "hidden" || value == "collapse")) {
					return true
				}
			}
		}
	}
	return false
}

// Coalescing descendants preserves inline words and decoded entities, with a
// fixed text ceiling independent of the response body's larger sampling cap.
func urlProbeHtmlText(root *html.Node) string {
	var text strings.Builder
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if text.Len() >= 4096 || urlProbeHtmlIgnored(node) || node.Type == html.ElementNode && node.Data == "script" {
			return
		}
		if node.Type == html.TextNode {
			text.WriteString(node.Data[:min(len(node.Data), 4096-text.Len())])
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	return strings.ToLower(strings.Join(strings.Fields(text.String()), " "))
}

// Marker paths and hosts belong to parsed URLs, never query or fragment text.
func urlProbeHtmlMarkerUrl(value string) *url.URL {
	target, err := url.Parse(strings.TrimSpace(value))
	if err != nil || target.Opaque != "" || target.User != nil || target.Scheme != "" && (target.Hostname() == "" || target.Scheme != "http" && target.Scheme != "https") {
		return nil
	}
	return target
}
