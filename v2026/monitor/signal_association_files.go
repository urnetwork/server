package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §19.1 maps to signal_association_files.go and
// signal_association_files_test.go. The probe bypasses DNS and CDN selection
// while retaining ur.io SNI, then validates the platform contracts rather
// than accepting an arbitrary HTTP 200 body.
func NewAssociationFilesSignal() Signal {
	return &signalAdapter{
		number: "19.1",
		key:    "association-files",
		name:   "Mobile association files",
		probe:  associationFilesProbe{},
	}
}

type associationFilesProbe struct{}

func (associationFilesProbe) id() string             { return "synthetic/web-association-files" }
func (associationFilesProbe) tier() string           { return tierWarn }
func (associationFilesProbe) cadence() time.Duration { return 5 * time.Minute }

const (
	associationOutputMarker   = "monitor_association_metadata_end"
	associationAndroidPackage = "com.bringyour.network"
	associationAppleAppID     = "6BGU69Q742.network.ur"
)

var associationFingerprintPattern = regexp.MustCompile(`(?i)^[0-9a-f]{2}(?::[0-9a-f]{2}){31}$`)

type associationDocument struct {
	kind string
	path string
}

var associationDocuments = []associationDocument{
	{kind: "android", path: "/.well-known/assetlinks.json"},
	{kind: "apple", path: "/.well-known/apple-app-site-association"},
}

type associationHTTPResult struct {
	body     string
	values   map[string]string
	output   string
	err      error
	parseErr error
}

type associationProbeResult struct {
	host       *host
	configured EdgeIPv6InterfaceSettings
	document   associationDocument
	public     associationHTTPResult
	validation error
}

func (associationFilesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hostname := strings.TrimSpace(env.cfg.websiteDomain)
	if hostname == "" {
		return nil, nil
	}

	tasks := []associationProbeResult{}
	for _, target := range env.cfg.hosts {
		for _, configured := range target.edgeIPv6 {
			for _, document := range associationDocuments {
				tasks = append(tasks, associationProbeResult{
					host: target, configured: configured, document: document,
				})
			}
		}
	}
	if len(tasks) == 0 {
		return nil, nil
	}

	results := make(chan associationProbeResult, len(tasks))
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
			task.public = runAssociationHTTPS(
				ctx,
				env.runner,
				hostname,
				task.configured.Address,
				task.document.path,
			)
			if associationHTTPHealthy(task.public) {
				task.validation = validateAssociationDocument(
					task.document.kind,
					task.public.body,
					task.public.values["monitor_content_type"],
				)
			}
			results <- task
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]associationProbeResult, 0, len(tasks))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].host.name != ordered[j].host.name {
			return ordered[i].host.name < ordered[j].host.name
		}
		if ordered[i].configured.Interface != ordered[j].configured.Interface {
			return ordered[i].configured.Interface < ordered[j].configured.Interface
		}
		return ordered[i].document.path < ordered[j].document.path
	})

	return associationFilesFindings(hostname, ordered), nil
}

func runAssociationHTTPS(
	ctx context.Context,
	runner probeRunner,
	hostname string,
	address string,
	path string,
) associationHTTPResult {
	resolve := fmt.Sprintf("%s:443:[%s]", hostname, address)
	writeOut := "\n" + associationOutputMarker + "\n" +
		"monitor_http_code=%{http_code}\n" +
		"monitor_exitcode=%{exitcode}\n" +
		"monitor_remote_ip=%{remote_ip}\n" +
		"monitor_content_type=%{content_type}\n" +
		"monitor_time_total=%{time_total}\n"
	output, err := runner.local(ctx, "curl",
		"--ipv6", "--http1.1", "--silent", "--show-error",
		"--connect-timeout", "3", "--max-time", "5", "--max-filesize", "1048576",
		"--noproxy", "*", "--resolve", resolve,
		"--header", "Accept: application/json",
		"--write-out", writeOut,
		"https://"+hostname+path,
	)
	result := associationHTTPResult{output: output, err: err}
	marker := "\n" + associationOutputMarker + "\n"
	markerAt := strings.LastIndex(output, marker)
	if markerAt < 0 {
		result.values = parseKeyValueLines(output)
		result.parseErr = fmt.Errorf("curl output omitted metadata marker")
		return result
	}
	result.body = strings.TrimSpace(output[:markerAt])
	result.values = parseKeyValueLines(output[markerAt+len(marker):])
	return result
}

func associationHTTPHealthy(result associationHTTPResult) bool {
	return result.err == nil && result.parseErr == nil &&
		result.values["monitor_exitcode"] == "0" &&
		result.values["monitor_http_code"] == "200"
}

func validateAssociationDocument(kind, body, contentType string) error {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
		return fmt.Errorf("content type %q is not JSON", contentType)
	}
	if body == "" {
		return fmt.Errorf("response body is empty")
	}
	switch kind {
	case "android":
		return validateAndroidAssociation(body)
	case "apple":
		return validateAppleAssociation(body)
	default:
		return fmt.Errorf("unknown association document kind %q", kind)
	}
}

func validateAndroidAssociation(body string) error {
	var statements []struct {
		Relation []string `json:"relation"`
		Target   struct {
			Namespace              string   `json:"namespace"`
			PackageName            string   `json:"package_name"`
			SHA256CertFingerprints []string `json:"sha256_cert_fingerprints"`
		} `json:"target"`
	}
	if err := json.Unmarshal([]byte(body), &statements); err != nil {
		return fmt.Errorf("invalid assetlinks JSON: %w", err)
	}
	for _, statement := range statements {
		if !containsString(statement.Relation, "delegate_permission/common.handle_all_urls") ||
			statement.Target.Namespace != "android_app" ||
			statement.Target.PackageName != associationAndroidPackage ||
			len(statement.Target.SHA256CertFingerprints) == 0 {
			continue
		}
		for _, fingerprint := range statement.Target.SHA256CertFingerprints {
			if !associationFingerprintPattern.MatchString(fingerprint) {
				return fmt.Errorf("assetlinks contains a malformed SHA-256 certificate fingerprint")
			}
		}
		return nil
	}
	return fmt.Errorf("assetlinks does not authorize %s to handle all URLs", associationAndroidPackage)
}

func validateAppleAssociation(body string) error {
	var document struct {
		AppLinks struct {
			Details []struct {
				AppID  string   `json:"appID"`
				AppIDs []string `json:"appIDs"`
			} `json:"details"`
		} `json:"applinks"`
		WebCredentials struct {
			Apps []string `json:"apps"`
		} `json:"webcredentials"`
	}
	if err := json.Unmarshal([]byte(body), &document); err != nil {
		return fmt.Errorf("invalid apple-app-site-association JSON: %w", err)
	}
	appLinksAuthorized := false
	for _, detail := range document.AppLinks.Details {
		if detail.AppID == associationAppleAppID || containsString(detail.AppIDs, associationAppleAppID) {
			appLinksAuthorized = true
			break
		}
	}
	if !appLinksAuthorized {
		return fmt.Errorf("apple association does not authorize app links for %s", associationAppleAppID)
	}
	if !containsString(document.WebCredentials.Apps, associationAppleAppID) {
		return fmt.Errorf("apple association does not authorize web credentials for %s", associationAppleAppID)
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func associationFilesFindings(hostname string, results []associationProbeResult) []finding {
	failures := []string{}
	missing := 0
	invalid := 0
	responseFailures := 0
	checked := 0
	for _, result := range results {
		exitCode := result.public.values["monitor_exitcode"]
		// A dead exact-address transport is already localized by §18.1. It
		// must not create a second static-file incident with no new evidence.
		if exitCode == "7" || exitCode == "28" {
			continue
		}
		checked++
		code := result.public.values["monitor_http_code"]
		problem := ""
		switch {
		case result.public.parseErr != nil:
			problem = result.public.parseErr.Error()
			responseFailures++
		case result.public.err != nil:
			problem = result.public.err.Error()
			responseFailures++
		case exitCode != "0":
			problem = "curl exit " + exitCode
			responseFailures++
		case code == "404":
			problem = "required file returned HTTP 404"
			missing++
		case code != "200":
			problem = "required file returned HTTP " + code
			responseFailures++
		case result.validation != nil:
			problem = result.validation.Error()
			invalid++
		}
		if problem == "" {
			continue
		}
		failures = append(failures, fmt.Sprintf(
			"edge=%s interface=%s address=%s path=%s http=%s exit=%s content_type=%q problem=%s body_prefix=%q",
			result.host.name,
			result.configured.Interface,
			result.configured.Address,
			result.document.path,
			code,
			exitCode,
			result.public.values["monitor_content_type"],
			problem,
			associationBodyPrefix(result.public.body),
		))
	}
	if len(failures) == 0 {
		return nil
	}

	mechanism := "At least one serving web generation does not expose a valid Android/Apple ownership document. Platform verifiers therefore cannot prove that the site owns the declared mobile app, even while ordinary pages remain healthy."
	action := "Compare the tracked files, Astro dist output, staged build/main tree, and the exact web image on each failed edge. Repair the first boundary that loses or mutates the documents, deploy one web generation to every edge, and do not mask the failure with an nginx-generated 200 response."
	if missing > 0 {
		mechanism = "The required documents exist in the tracked Astro public tree and in dist, but the deployable web tree omitted the root .well-known directory. The production build used `mv dist/*`; the shell glob excludes dot-prefixed entries after the SEO gate has already passed, so the web image legitimately returned 404 from every generation."
		action = "Build and deploy the web service from mmm/ur.io commit 72190198 or later, whose dotfile-safe staging script moves every dist entry and whose regression test requires both association files. Confirm every web block carries that build; do not edit the platform files in-place on containers or add a fallback 200 in nginx."
	}

	return []finding{{
		probeId: "synthetic/web-association-files", tier: tierWarn,
		class: "web-association-files", target: hostname, sustain: 1,
		symptom:   fmt.Sprintf("%s mobile association metadata fails on %d exact edge/path checks", hostname, len(failures)),
		mechanism: mechanism,
		baseline:  fmt.Sprintf("Every enabled edge returns HTTP 200 application/json for both association paths; assetlinks authorizes %s and the Apple document authorizes %s for app links and web credentials.", associationAndroidPackage, associationAppleAppID),
		observed:  fmt.Sprintf("checked=%d failed=%d missing_404=%d invalid_contract=%d other_response_failures=%d", checked, len(failures), missing, invalid, responseFailures),
		evidence:  strings.Join(failures, "\n"),
		context:   "Android's release manifest declares an auto-verified ur.io HTTPS intent filter. These are platform ownership contracts, not optional crawler assets; ordinary homepage and process health cannot substitute for them.",
		action:    action,
		verify:    "Require both paths to return HTTP 200 application/json with the expected semantic authorization on every enabled exact edge address and through the canonical CDN hostname. Then require zero exact-path nginx ENOENT lines for ten minutes.",
		playbook:  "SIGNALS.md §19.1",
	}}
}

func associationBodyPrefix(body string) string {
	prefix := strings.Join(strings.Fields(body), " ")
	const maxBytes = 240
	if len(prefix) > maxBytes {
		return prefix[:maxBytes] + "..."
	}
	return prefix
}
