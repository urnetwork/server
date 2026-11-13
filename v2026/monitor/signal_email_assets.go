// Required image coverage remains unknown when exact-edge transport is excluded.
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
// signal_email_assets_test.go. It checks both the recipient-facing website URL
// and every exact edge pinned under the same Host, because either layer can break
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

// Keep this list equal to the distinct absolute https://ur.io/images/emails URLs
// in controller/email_templates (the shared _layout.html embeds them in every
// template). A new embedded asset is a new runtime dependency and belongs here
// and in §19.2, not only in the site's public tree.
var emailAssets = []emailAsset{
	{path: "/images/emails/ur-wordmark-black-bg-320.png", templates: "every template: the header wordmark on paper, and what Gmail's dark mode keeps"},
	{path: "/images/emails/ur-wordmark-white-320.png", templates: "every template: the header wordmark clients swap in under prefers-color-scheme dark"},
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
	// The images ship inside the product site's bundle, so the website domain is
	// both the recipient-facing URL and the exact Host every edge serves it under
	// (the same pin §19.3 uses for the association files). WebsiteDomain is the
	// services.yml-derived arming signal: alternate environments must not probe
	// production email URLs merely because they happen to have a PublicDomain.
	domain := strings.TrimSpace(env.cfg.websiteDomain)
	if domain == "" {
		return nil, nil
	}
	tasks := make([]emailAssetResult, 0, len(emailAssets)*(1+len(env.cfg.hosts)*2))
	for _, asset := range emailAssets {
		tasks = append(tasks, emailAssetResult{
			scope: "public", hostname: domain, asset: asset,
		})
	}
	for _, target := range env.cfg.hosts {
		for _, configured := range target.edgeIPv6 {
			for _, asset := range emailAssets {
				tasks = append(tasks, emailAssetResult{
					scope: "edge", hostname: domain,
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
			if task.scope == "public" {
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

	return emailAssetFindings(domain, ordered), nil
}

func emailAssetHostName(result emailAssetResult) string {
	if result.host == nil {
		return ""
	}
	return result.host.name
}

// Response classes establish the failed boundary; artifact and cache ownership need separate evidence.
func emailAssetFindings(domain string, results []emailAssetResult) []finding {
	var failures []string
	checked := 0
	publicFailures := 0
	edgeFailures := 0
	missing := 0
	edgeMissing := 0
	edgeChecks := 0
	edgeUnavailable := 0
	nonImage := 0
	empty := 0
	transportFailures := 0
	other := 0

	for _, result := range results {
		exitCode := result.public.values["monitor_exitcode"]
		// Exact-address transport is already localized by §18.1. The public path
		// remains independently user-facing and therefore is never suppressed.
		if result.scope == "edge" && (exitCode == "7" || exitCode == "28") {
			edgeUnavailable++
			continue
		}
		if result.scope == "edge" {
			edgeChecks++
		}
		checked++
		problem, category := emailAssetProblem(result.public)
		if problem == "" {
			continue
		}
		if result.scope == "public" {
			publicFailures++
		} else {
			edgeFailures++
		}
		switch category {
		case "missing":
			missing++
			if result.scope == "edge" {
				edgeMissing++
			}
		case "non-image":
			nonImage++
		case "empty":
			empty++
		case "transport":
			transportFailures++
		default:
			other++
		}
		failures = append(failures, emailAssetEvidence(result, problem))
	}
	findings := []finding{}
	if edgeUnavailable > 0 {
		unknown := cannotObserveFinding("email-assets/"+domain, fmt.Errorf("required exact-edge email images were not observed"))
		unknown.mechanism = "Required email images were not observed on every enabled exact edge/path. Transport ownership remains with the edge IPv6 signal; healthy public responses cannot establish exact-edge contract recovery."
		unknown.observed = fmt.Sprintf("expected=%d checked=%d unobserved=%d", len(results), checked, edgeUnavailable)
		unknown.action = "Retain prior email-image incidents and independent response/content findings. Use separately owned transport evidence before choosing a repair; do not infer missing images or invalidate a cache from unobserved responses."
		unknown.verify = "Every required public and enabled exact-edge image returns a complete response that can be classified and validated."
		unknown.playbook = "SIGNALS.md §19.2 and §18.1"
		findings = append(findings, unknown)
	}
	if len(failures) == 0 {
		return findings
	}

	class := "web-email-assets"
	sustain := 1
	mechanism := "A recipient-facing request or one exact edge did not return the byte-bearing image the shared email layout embeds. Ordinary page checks can remain healthy because they never exercise the /images/emails/ namespace, content type, and body together."
	context := "These absolute URLs are the header wordmark in every transactional email source template (authentication, password, welcome, payout, data code, and receipt). If current email-sending artifacts predate the template change, a 404 is a release-order blocker rather than proof that recipients have seen it; after those templates deploy, it is a functional product regression. Use executable provenance and build time to distinguish those states. The required asset list is maintained beside this probe and must track controller/email_templates."
	action := "Compare the failed public and exact-edge paths with successful controls. Identify the responding component, upstream result, active route, and running artifact from bounded existing evidence before selecting an admission, content, deployment, or cache repair. The images are part of the site bundle (mmm/ur.io/react/public/images/emails, mirrored into the astro build by sync-public), but a response failure alone does not prove that bundle is missing them."
	verify := "Require every listed public URL to return HTTP 200 image/* with nonzero bytes; require the same for every enabled edge when pinned with the exact site SNI/Host; then require zero exact-path 404 lines for ten minutes."
	allEdgesHealthy := edgeChecks > 0 && edgeFailures == 0 && edgeUnavailable == 0
	if edgeMissing > 0 && edgeMissing == edgeChecks && edgeUnavailable == 0 {
		mechanism = "Every exact edge returned HTTP 404 for an image path. Whether the deployed build predates the assets, sync-public omitted images, or a serving route or intermediary returned the 404 remains to be established from the current artifact and response."
		action = "Identify the responding route and running artifact first. If the serving artifact is confirmed to omit these images, build and deploy only the Web service from an intentional local Mmm checkout containing b4b229c5c and the intended local SDK checkout. Require the Makefile's sync-public target to place both images in staged output before the Warp build, and record any participating local diffs. If API and Taskworker artifacts containing current-main server commit ec6e3b92 or later are not active yet, keep them behind this Web gate. If any are already active, treat verified missing assets as a current broken-email dependency and prioritize the Web rollout; do not roll back or redeploy an otherwise-correct sender merely to clear this alert. Do not copy files into live containers."
	} else if edgeMissing > 0 {
		mechanism = fmt.Sprintf("Required images returned HTTP 404 on %d of %d configured exact-edge checks. Successful peers localize the failed response paths; they do not prove the failed artifact's contents or identify its responding route.", edgeMissing, edgeChecks+edgeUnavailable)
		action = "Compare the failed exact-edge responses with healthy peers. Identify each failed response's serving route and current artifact, then check staged bundle contents and sync-public output before choosing a Web repair. Keep successful image responses as controls."
	} else if allEdgesHealthy && publicFailures > 0 {
		mechanism = "Every checked exact edge is healthy, but the recipient-facing response failed. These controls isolate the observation to the public delivery path; they do not identify the responding component or establish a cached failure."
		action = "Identify the public path's selected responding component, exact site Host, upstream status, and effective route using bounded existing evidence. Compare the failed response with healthy exact-edge controls. Consider path-scoped cache repair only after proving the cache owns the failed response and that its current origin is healthy."
	}
	if other > 0 {
		mechanism += " Non-200 responses other than 404 do not establish missing image bytes or a stale bundle."
		action += " For HTTP 429, distinguish request admission, connection admission, and an upstream rejection against the active configuration before choosing a repair; response status alone is insufficient."
	}
	if transportFailures == len(failures) {
		class = "web-email-assets-transport"
		sustain = 2
		mechanism = "The failed checks ended before receiving any HTTP response. That proves a recipient-path transport failure but does not prove that the image bytes, edge bundle, or cached object are wrong. A single DNS route, public proxy address, TCP, or TLS failure cannot distinguish those causes."
		if allEdgesHealthy && publicFailures > 0 {
			mechanism += " Every exact edge completed its semantic image check, isolating the observation to the public transport path."
		}
		context = "Continuous mode requires this transport-only identity on two consecutive five-minute cadences; one-shot diagnostics still expose the first sample. Semantic HTTP/content failures remain immediate because they affirmatively observe the broken object response."
		action = "Repeat the failed public path separately over IPv4 and IPv6 while recording the selected remote address and TLS result, and compare the exact edges. If the next cadence is healthy, retain the first sample as a transient control. If the same boundary persists, diagnose the client route, resolver, TLS handshake, and selected public address. Do not invalidate cached objects without an HTTP error response."
		verify = "The failed URL returns HTTP 200 image/* with nonzero bytes over IPv4, IPv6, and ordinary DNS selection, every exact edge remains healthy, and two consecutive five-minute signal cadences contain no transport failure."
	}
	if edgeUnavailable > 0 {
		context += fmt.Sprintf(" %d configured exact-edge checks were unavailable before an HTTP response and remain owned by §18.1; their image contents are unknown.", edgeUnavailable)
	}

	return append(findings, finding{
		probeId: "synthetic/web-email-assets", tier: tierWarn,
		class: class, target: domain, sustain: sustain,
		symptom:   fmt.Sprintf("%s transactional-email images fail on %d public/edge checks", domain, len(failures)),
		mechanism: mechanism,
		baseline:  fmt.Sprintf("All %d distinct images embedded by controller/email_templates return HTTP 200, an image/* media type, and a non-empty body through https://%s and pinned to every enabled edge address under the same Host.", len(emailAssets), domain),
		observed:  fmt.Sprintf("checked=%d failed=%d public_failed=%d edge_failed=%d missing_404=%d non_image=%d empty_body=%d transport_failures=%d other_response_failures=%d", checked, len(failures), publicFailures, edgeFailures, missing, nonImage, empty, transportFailures, other),
		evidence:  strings.Join(failures, "\n"),
		context:   context,
		action:    action,
		verify:    verify,
		playbook:  "SIGNALS.md §19.2",
	})
}

func emailAssetProblem(result exactHTTPSResult) (problem, category string) {
	exitCode := result.values["monitor_exitcode"]
	code := result.values["monitor_http_code"]
	switch {
	case exitCode != "0":
		if exitCode == "" {
			if result.err != nil {
				return result.err.Error(), "transport"
			}
			exitCode = "<missing>"
		}
		return "curl exit " + exitCode, "transport"
	case result.err != nil:
		return result.err.Error(), "transport"
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
