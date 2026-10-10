// Bounded static HTML evidence distinguishes document gates from page widgets.
package egresshealth

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Version three recognizes human-prompt grammar and direct visible instructions.
// This identifies detector semantics without changing timing or quota policy.
const UrlProbeContentMatcherVersion = 3

// Optional articles and conjunctions do not change the human-verification request.
var urlProbeHumanPrompt = regexp.MustCompile(`\b(?:are you (?:a )?human|(?:verify|confirm) (?:that )?you are (?:a )?human)\b`)

type urlProbeHtml struct {
	form, humanGate, portalTitle bool
}

// Ancestor context is copied into children, never shared between sibling nodes.
type urlProbeHtmlScope struct {
	article, auxiliary, form, prompt, widget bool
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
	var formCaptchaWidget, formHumanCaptchaMount, documentPrompt, humanInstruction, ordinaryContent bool
	var challengePrompt, javascriptPrompt, googleSorryForm, unusualTraffic, articleContent bool
	containsPrompt := func(value string) bool {
		if urlProbeHumanPrompt.MatchString(value) {
			return true
		}
		for _, marker := range []string{"security check", "just a moment", "captcha verification"} {
			if strings.Contains(value, marker) {
				return true
			}
		}
		return false
	}
	// Body prose must be a direct instruction, not a description of one.
	containsInstruction := func(value string) bool {
		match := urlProbeHumanPrompt.FindStringIndex(value)
		if match == nil {
			return false
		}
		switch strings.TrimPrefix(value[:match[0]], "please ") {
		case "", "press & hold to ", "press and hold to ":
		default:
			return false
		}
		suffix := strings.TrimSuffix(strings.TrimRight(value[match[1]:], ".!?"), " to continue")
		return suffix == "" || suffix == " (and not a bot)"
	}
	var visit func(*html.Node, urlProbeHtmlScope)
	visit = func(node *html.Node, scope urlProbeHtmlScope) {
		if urlProbeHtmlIgnored(node) {
			return
		}
		if node.Type == html.ElementNode {
			scope.article = scope.article || node.Data == "article"
			scope.auxiliary = scope.auxiliary || node.Data == "footer" || node.Data == "aside" || node.Data == "nav"
			scope.form = scope.form || node.Data == "form"
			for _, attribute := range node.Attr {
				scope.article = scope.article || attribute.Key == "role" && strings.EqualFold(strings.TrimSpace(attribute.Val), "article")
			}
			if node.Data == "blockquote" || node.Data == "q" {
				// Quoted prose stays visible content, but none of its controls or
				// prompts can corroborate a challenge on the containing page.
				visible := false
				var visitText func(*html.Node, bool)
				visitText = func(quoted *html.Node, inArticle bool) {
					if visible && articleContent || urlProbeHtmlIgnored(quoted) || quoted.Type == html.ElementNode && quoted.Data == "script" {
						return
					}
					if quoted.Type == html.ElementNode {
						inArticle = inArticle || quoted.Data == "article"
						for _, attribute := range quoted.Attr {
							inArticle = inArticle || attribute.Key == "role" && strings.EqualFold(strings.TrimSpace(attribute.Val), "article")
						}
					}
					if quoted.Type == html.TextNode && strings.TrimSpace(quoted.Data) != "" {
						visible = true
						articleContent = articleContent || inArticle
					}
					for child := quoted.FirstChild; child != nil; child = child.NextSibling {
						visitText(child, inArticle)
					}
				}
				visitText(node, scope.article)
				ordinaryContent = ordinaryContent || !scope.auxiliary && visible
				return
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
				if target := urlProbeHtmlMarkerUrl(source); target != nil && executable && !scope.auxiliary {
					path := target.Path
					passive := strings.Contains(path, "/scripts/jsd/") || strings.HasSuffix(path, "/scripts/jsd")
					challengeScript = challengeScript || strings.HasPrefix(path, "/cdn-cgi/challenge-platform/") && !passive
					humanCaptchaScript = humanCaptchaScript || strings.HasSuffix(path, "/captcha.js")
				}
				return
			}
			if node.Data == "title" {
				value := urlProbeHtmlText(node)
				prompt := containsPrompt(value)
				challengePrompt = challengePrompt || prompt
				documentPrompt = documentPrompt || prompt
				for _, marker := range []string{"wi-fi sign in", "wifi sign in", "captive portal", "sign in to this network", "log in to this network"} {
					page.portalTitle = page.portalTitle || strings.Contains(value, marker)
				}
				return
			}
			if !scope.article && !scope.auxiliary {
				if node.Data == "h1" {
					prompt := containsPrompt(urlProbeHtmlText(node))
					challengePrompt = challengePrompt || prompt
					documentPrompt = documentPrompt || !scope.form && prompt
					scope.prompt = scope.prompt || prompt
				}
				if node.Data == "p" || node.Data == "button" {
					instruction := containsInstruction(urlProbeHtmlText(node))
					humanInstruction = humanInstruction || !scope.form && instruction
					scope.prompt = scope.prompt || instruction
				}
				if node.Data == "noscript" {
					javascriptPrompt = javascriptPrompt || strings.Contains(urlProbeHtmlText(node), "enable javascript and cookies")
				}
				page.form = page.form || node.Data == "form"
				for _, attribute := range node.Attr {
					value := strings.ToLower(strings.TrimSpace(attribute.Val))
					if attribute.Key == "class" || attribute.Key == "id" {
						for _, token := range strings.Fields(value) {
							if token == "g-recaptcha" || token == "h-captcha" || token == "cf-turnstile" {
								captchaWidget = captchaWidget || !scope.form
								formCaptchaWidget = formCaptchaWidget || scope.form
								scope.prompt, scope.widget = true, true
							}
						}
						if attribute.Key == "id" && value == "px-captcha" {
							humanCaptchaMount = humanCaptchaMount || !scope.form
							formHumanCaptchaMount = formHumanCaptchaMount || scope.form
							scope.prompt, scope.widget = true, true
						}
					}
					if node.Data == "form" && attribute.Key == "action" {
						if target := urlProbeHtmlMarkerUrl(attribute.Val); target != nil {
							host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
							googleHost := host == "google.com" || strings.HasSuffix(host, ".google.com")
							googleSorryForm = googleSorryForm || strings.HasPrefix(target.Path, "/sorry/") && (target.Host == "" || googleHost)
						}
					}
				}
				if scope.form && !scope.widget {
					switch node.Data {
					case "textarea", "select":
						ordinaryContent = true
					case "input":
						inputType := "text"
						for _, attribute := range node.Attr {
							if attribute.Key == "type" {
								inputType = strings.ToLower(strings.TrimSpace(attribute.Val))
							}
						}
						switch inputType {
						case "hidden", "submit", "button", "reset", "image":
						default:
							ordinaryContent = true
						}
					}
				}
			}
		}
		if node.Type == html.TextNode {
			value := strings.ToLower(strings.Join(strings.Fields(node.Data), " "))
			articleContent = articleContent || scope.article && value != ""
			ordinaryContent = ordinaryContent || !scope.auxiliary && !scope.prompt && value != ""
			unusualTraffic = unusualTraffic || !scope.article && !scope.auxiliary && strings.Contains(value, "unusual traffic from your computer network")
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, scope)
		}
	}
	visit(root, urlProbeHtmlScope{})
	// Form-local prompts and widgets cannot corroborate unrelated page content.
	widget := captchaWidget || humanCaptchaMount && humanCaptchaScript
	// A prompt and otherwise empty challenge form retain the standalone-gate rule.
	formGate := documentPrompt && !ordinaryContent && (formCaptchaWidget || formHumanCaptchaMount && humanCaptchaScript)
	page.humanGate = challengeScript && (challengePrompt || javascriptPrompt) ||
		!articleContent && (documentPrompt && widget || humanInstruction && (widget || challengeScript) || formGate || googleSorryForm && unusualTraffic)
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

// Inline words stay joined; line breaks and blocks separate adjacent words.
// The text ceiling is independent of the response body's larger sampling cap.
func urlProbeHtmlText(root *html.Node) string {
	var text strings.Builder
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if text.Len() >= 4096 || urlProbeHtmlIgnored(node) || node.Type == html.ElementNode && (node.Data == "script" || node.Data == "blockquote" || node.Data == "q") {
			return
		}
		separator := false
		if node.Type == html.ElementNode {
			switch node.Data {
			case "br", "div", "p", "section", "h1", "h2", "h3", "h4", "h5", "h6", "li":
				separator = true
				text.WriteByte(' ')
			}
		}
		if node.Type == html.TextNode {
			text.WriteString(node.Data[:min(len(node.Data), 4096-text.Len())])
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
		if separator && text.Len() < 4096 {
			text.WriteByte(' ')
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
