package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §19.2 maps to signal_email_assets.go and
// signal_email_assets_test.go. It checks both the recipient-facing CDN URL and
// every exact edge behind the CDN origin Host, because either layer can break
// images embedded in transactional email while ordinary web health stays green.
func NewEmailAssetsSignal() Signal {
	return &signalAdapter{
		number: "19.2",
		key:    "email-assets",
		name:   "Transactional email assets",
		probe:  emailAssetsProbe{},
	}
}

type emailAssetsProbe struct{}

func (emailAssetsProbe) id() string             { return "synthetic/web-email-assets" }
func (emailAssetsProbe) tier() string           { return tierWarn }
func (emailAssetsProbe) cadence() time.Duration { return 5 * time.Minute }

type emailAsset struct {
	path      string
	templates string
}

// Keep this list equal to the distinct absolute /res/emails URLs in
// controller/email_templates. A new embedded asset is a new runtime dependency
// and belongs here and in §19.2, not only in an image-build inventory.
var emailAssets = []emailAsset{
	{path: "/res/emails/bringyour-wordmark-bg-240.jpg", templates: "subscription transfer, interview, and x402 receipt"},
	{path: "/res/emails/ur-wordmark-bg-240.jpg", templates: "auth, welcome, subscription payment, and missing-wallet"},
	{path: "/res/emails/ur-welcome-header-1080.jpg", templates: "network welcome"},
	{path: "/res/emails/welcome-header-1080.jpg", templates: "network interview request"},
	{path: "/res/emails/urnetwork-goodbye-vpn.gif", templates: "network welcome"},
	{path: "/res/emails/urnetwork-spin.gif", templates: "subscription payment"},
}

type emailAssetResult struct {
	scope      string
	hostname   string
	host       *host
	configured EdgeIPv6InterfaceSettings
	asset      emailAsset
	public     exactHTTPSResult
}

func (emailAssetsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	domain := strings.TrimSpace(env.cfg.publicDomain)
	// WebsiteDomain is the services.yml-derived arming signal for the product
	// site. Alternate environments must not probe production email URLs merely
	// because they happen to have a PublicDomain value.
	if domain == "" || strings.TrimSpace(env.cfg.websiteDomain) == "" {
		return nil, nil
	}
	originHostname := "main-web." + domain

	tasks := make([]emailAssetResult, 0, len(emailAssets)*(1+len(env.cfg.hosts)*2))
	for _, asset := range emailAssets {
		tasks = append(tasks, emailAssetResult{
			scope: "cdn", hostname: domain, asset: asset,
		})
	}
	for _, target := range env.cfg.hosts {
		for _, configured := range target.edgeIPv6 {
			for _, asset := range emailAssets {
				tasks = append(tasks, emailAssetResult{
					scope: "origin", hostname: originHostname,
					host: target, configured: configured, asset: asset,
				})
			}
		}
	}

	results := make(chan emailAssetResult, len(tasks))
	semaphore := make(chan struct{}, 8)
	var wait sync.WaitGroup
	for _, queued := range tasks {
		task := queued
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				task.public.err = ctx.Err()
				results <- task
				return
			}
			if task.scope == "cdn" {
				task.public = runPublicHTTPS(ctx, env.runner, task.hostname, task.asset.path)
			} else {
				task.public = runExactHTTPS(
					ctx,
					env.runner,
					task.hostname,
					task.configured.Address,
					task.asset.path,
				)
			}
			results <- task
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]emailAssetResult, 0, len(tasks))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].scope != ordered[j].scope {
			return ordered[i].scope < ordered[j].scope
		}
		if emailAssetHostName(ordered[i]) != emailAssetHostName(ordered[j]) {
			return emailAssetHostName(ordered[i]) < emailAssetHostName(ordered[j])
		}
		if ordered[i].configured.Interface != ordered[j].configured.Interface {
			return ordered[i].configured.Interface < ordered[j].configured.Interface
		}
		return ordered[i].asset.path < ordered[j].asset.path
	})

	return emailAssetFindings(domain, originHostname, ordered), nil
}

func emailAssetHostName(result emailAssetResult) string {
	if result.host == nil {
		return ""
	}
	return result.host.name
}

func emailAssetFindings(domain, originHostname string, results []emailAssetResult) []finding {
	var failures []string
	checked := 0
	cdnFailures := 0
	originFailures := 0
	missing := 0
	originMissing := 0
	nonImage := 0
	empty := 0
	other := 0

	for _, result := range results {
		exitCode := result.public.values["monitor_exitcode"]
		// Exact-address transport is already localized by §18.1. The CDN path
		// remains independently user-facing and therefore is never suppressed.
		if result.scope == "origin" && (exitCode == "7" || exitCode == "28") {
			continue
		}
		checked++
		problem, category := emailAssetProblem(result.public)
		if problem == "" {
			continue
		}
		if result.scope == "cdn" {
			cdnFailures++
		} else {
			originFailures++
		}
		switch category {
		case "missing":
			missing++
			if result.scope == "origin" {
				originMissing++
			}
		case "non-image":
			nonImage++
		case "empty":
			empty++
		default:
			other++
		}
		failures = append(failures, emailAssetEvidence(result, problem))
	}
	if len(failures) == 0 {
		return nil
	}

	mechanism := "A recipient-facing CDN request or one exact web origin did not return the byte-bearing image embedded by a server email template. Ordinary page redirects and /status can remain healthy because those routes do not exercise the asset namespace, content type, body, CDN cache, and origin Host together."
	action := "Compare the CDN response with the same path pinned to every exact origin address. If exact origins fail, deploy one web generation that contains the asset directory and explicitly serves the CDN origin Host. If only the CDN fails, verify its configured origin Host and clear the stale error object after the origins are healthy."
	if originMissing > 0 {
		mechanism = "CloudFront deliberately replaces the public Host with `" + originHostname + "`. Web commit `dc8fd20c` narrowed the former wildcard legacy server to the apex redirect and stopped matching that origin Host. The request consequently fell into nginx's empty default `/etc/nginx/html` root and returned 404 even though the image contained every email asset."
		action = "Build and deploy the web service from web commit `2b410faa` or later. It explicitly serves `/res/emails/` from the BringYour asset tree for both the public and CDN-origin Hosts while preserving the ur.io redirect for legacy pages. Do not copy files into live containers or turn the default server into a broad static host."
	} else if originFailures == 0 && cdnFailures > 0 {
		mechanism = "Every checked exact origin is healthy, but the recipient-facing CDN response is not. The remaining boundary is CDN configuration or a cached negative/error response produced before the origin Host repair; the web asset bytes themselves are present and reachable."
		action = "Confirm the distribution still sends `Host: " + originHostname + "`, then invalidate only the failed `/res/emails/` objects if its negative-cache TTL has not expired. Recheck the public URLs; do not redeploy unrelated API, Connect, taskworker, or database services."
	}

	return []finding{{
		probeId: "synthetic/web-email-assets", tier: tierWarn,
		class: "web-email-assets", target: domain, sustain: 1,
		symptom:   fmt.Sprintf("%s transactional-email images fail on %d CDN/origin checks", domain, len(failures)),
		mechanism: mechanism,
		baseline:  fmt.Sprintf("All %d distinct images embedded by controller/email_templates return HTTP 200, an image/* media type, and a non-empty body through %s and through %s pinned to every enabled edge address.", len(emailAssets), domain, originHostname),
		observed:  fmt.Sprintf("checked=%d failed=%d cdn_failed=%d origin_failed=%d missing_404=%d non_image=%d empty_body=%d other_response_failures=%d", checked, len(failures), cdnFailures, originFailures, missing, nonImage, empty, other),
		evidence:  strings.Join(failures, "\n"),
		context:   "These absolute URLs are embedded in authentication, password, welcome, subscription, interview, and receipt emails. A 404 produces broken recipient content and is a functional product regression, not generic scanner traffic. The required asset list is maintained beside this probe and must track controller/email_templates.",
		action:    action,
		verify:    "Require every listed public URL to return HTTP 200 image/* with nonzero bytes; require the same for every enabled edge when pinned with the exact main-web SNI/Host; preserve 301 redirects for legacy page paths; then require zero exact-path nginx ENOENT lines for ten minutes.",
		playbook:  "SIGNALS.md §19.2",
	}}
}

func emailAssetProblem(result exactHTTPSResult) (problem, category string) {
	exitCode := result.values["monitor_exitcode"]
	code := result.values["monitor_http_code"]
	switch {
	case result.err != nil:
		return result.err.Error(), "response"
	case exitCode != "0":
		if exitCode == "" {
			exitCode = "<missing>"
		}
		return "curl exit " + exitCode, "response"
	case code == "404":
		return "required image returned HTTP 404", "missing"
	case code != "200":
		return "required image returned HTTP " + code, "response"
	}

	contentType := strings.ToLower(strings.TrimSpace(strings.Split(result.values["monitor_content_type"], ";")[0]))
	if !strings.HasPrefix(contentType, "image/") {
		return fmt.Sprintf("content type %q is not an image", result.values["monitor_content_type"]), "non-image"
	}
	size, err := strconv.ParseFloat(strings.TrimSpace(result.values["monitor_size_download"]), 64)
	if err != nil || size <= 0 {
		return fmt.Sprintf("response body size %q is not positive", result.values["monitor_size_download"]), "empty"
	}
	return "", ""
}

func emailAssetEvidence(result emailAssetResult, problem string) string {
	fields := []string{
		"scope=" + result.scope,
		"host=" + result.hostname,
		"path=" + result.asset.path,
		"templates=" + strconv.Quote(result.asset.templates),
	}
	if result.host != nil {
		fields = append(fields,
			"edge="+result.host.name,
			"interface="+result.configured.Interface,
			"address="+result.configured.Address,
		)
	}
	fields = append(fields,
		"http="+result.public.values["monitor_http_code"],
		"exit="+result.public.values["monitor_exitcode"],
		"content_type="+strconv.Quote(result.public.values["monitor_content_type"]),
		"bytes="+result.public.values["monitor_size_download"],
		"remote_ip="+result.public.values["monitor_remote_ip"],
		"problem="+problem,
	)
	return strings.Join(fields, " ")
}
